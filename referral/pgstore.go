package referral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the only Store: Supabase Postgres over the shared pgx pool
// (proxy/enrichment migration 013_referral.sql). The two operations that must
// be atomic — making a code, handing out a pool code — are plpgsql functions;
// the settlement steps are plain statements, each small enough to be its own
// unit of work.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps the pool; nil in → nil out so the caller can leave the
// feature unconfigured.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	if pool == nil {
		return nil
	}
	return &PGStore{pool: pool}
}

const (
	pgRaiseException = "P0001" // plpgsql `raise exception` — our named refusals
	pgUndefinedTable = "42P01" // migration not applied yet
)

// asFailure pulls a named refusal out of a Postgres error; anything else
// stays a plain error (502 at the handler).
func asFailure(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgRaiseException {
		switch pgErr.Message {
		case ErrUnknownCode, ErrOwnCode, ErrAlreadyClaimed, ErrNotNewUser, ErrPoolEmpty:
			return Failure{Code: pgErr.Message}
		}
	}
	return err
}

func (p *PGStore) Me(ctx context.Context, userID string) (Standing, error) {
	var raw []byte
	if err := p.pool.QueryRow(ctx, `select referral_me($1)::text`, userID).Scan(&raw); err != nil {
		return Standing{}, fmt.Errorf("referral_me: %w", asFailure(err))
	}
	var out Standing
	if err := json.Unmarshal(raw, &out); err != nil {
		return Standing{}, fmt.Errorf("referral_me decode: %w", err)
	}
	if out.Code == "" {
		return Standing{}, errors.New("referral_me: empty code")
	}
	return out, nil
}

func (p *PGStore) Claim(ctx context.Context, userID, code string) (string, error) {
	var raw []byte
	if err := p.pool.QueryRow(ctx, `select referral_claim($1, $2)::text`, userID, code).Scan(&raw); err != nil {
		return "", asFailure(err)
	}
	var out struct {
		OfferCode string `json:"offer_code"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("referral_claim decode: %w", err)
	}
	if out.OfferCode == "" {
		return "", Failure{Code: ErrPoolEmpty}
	}
	return out.OfferCode, nil
}

func (p *PGStore) ReportRedeemed(ctx context.Context, userID, offerCode string) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set redeem_reported_at = now()
		  where referee = $1 and offer_code = $2 and redeem_reported_at is null`,
		userID, offerCode)
	return err
}

// DeleteUser removes the referral_users row; referrals (both sides) and the
// reward ledger cascade, pool codes keep their state with issued_to nulled.
// A missing table (migration not yet applied) is nothing to delete, not an
// error — account deletion must never 500 over this feature's rollout order.
func (p *PGStore) DeleteUser(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("referral: delete user: empty user id")
	}
	_, err := p.pool.Exec(ctx, `delete from referral_users where user_id = $1`, userID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return nil
	}
	return err
}

func (p *PGStore) ExpireCodes(ctx context.Context) (int64, error) {
	tag, err := p.pool.Exec(ctx,
		`update code_pool set state = 'expired'
		  where state in ('free','issued') and expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PoolStatus reads the three pool settings (migration 014) and groups
// code_pool by batch. A batch is (offer_ref_name, expires_at): import-codes.sh
// gives one CSV one name and one date, and the same name re-imported with a
// later date is a new batch with its own expiry.
func (p *PGStore) PoolStatus(ctx context.Context) (PoolStatus, error) {
	var st PoolStatus
	if err := p.pool.QueryRow(ctx,
		`select issue_margin_days, low_stock_threshold, expiry_warn_days
		   from referral_settings where id = 1`).
		Scan(&st.Settings.IssueMarginDays, &st.Settings.LowStockThreshold, &st.Settings.ExpiryWarnDays); err != nil {
		return st, fmt.Errorf("pool settings: %w", err)
	}
	rows, err := p.pool.Query(ctx,
		`select offer_ref_name, expires_at,
		        count(*) filter (where state = 'free'),
		        count(*) filter (where state = 'issued'),
		        count(*) filter (where state = 'redeemed'),
		        count(*) filter (where state = 'expired')
		   from code_pool
		  group by offer_ref_name, expires_at
		  order by expires_at, offer_ref_name`)
	if err != nil {
		return st, fmt.Errorf("pool batches: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b PoolBatch
		if err := rows.Scan(&b.Name, &b.ExpiresAt, &b.Free, &b.Issued, &b.Redeemed, &b.Expired); err != nil {
			return st, fmt.Errorf("pool batches: %w", err)
		}
		st.Batches = append(st.Batches, b)
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("pool batches: %w", err)
	}
	return st, nil
}

func (p *PGStore) HoldDays(ctx context.Context) (int, error) {
	var days int
	err := p.pool.QueryRow(ctx, `select hold_days from referral_settings where id = 1`).Scan(&days)
	return days, err
}

const referralColumns = `id, referrer, referee, state, claimed_at, coalesce(offer_code, ''), redeemed_at`

func scanReferrals(rows pgx.Rows) ([]Referral, error) {
	defer rows.Close()
	var out []Referral
	for rows.Next() {
		var r Referral
		var redeemed *time.Time
		if err := rows.Scan(&r.ID, &r.Referrer, &r.Referee, &r.State, &r.ClaimedAt, &r.OfferCode, &redeemed); err != nil {
			return nil, err
		}
		if redeemed != nil {
			r.RedeemedAt = *redeemed
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PGStore) ListClaimed(ctx context.Context, limit int) ([]Referral, error) {
	// Stop polling RevenueCat for claims old enough that the issued code has
	// long expired (6 months) — they stay 'claimed' and cost nothing.
	rows, err := p.pool.Query(ctx,
		`select `+referralColumns+` from referrals
		  where state = 'claimed' and claimed_at > now() - interval '200 days'
		  order by redeem_reported_at desc nulls last, claimed_at
		  limit $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanReferrals(rows)
}

func (p *PGStore) MarkRedeemed(ctx context.Context, id int64, at time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx,
		`update referrals set state = 'redeemed', redeemed_at = $2
		  where id = $1 and state = 'claimed'`, id, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`update code_pool set state = 'redeemed', redeemed_at = $2
		  where referral_id = $1 and state = 'issued'`, id, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PGStore) ListDue(ctx context.Context, hold time.Duration, limit int) ([]Referral, error) {
	rows, err := p.pool.Query(ctx,
		`select `+referralColumns+` from referrals
		  where state = 'redeemed' and redeemed_at <= now() - make_interval(secs => $1)
		  order by redeemed_at
		  limit $2`, hold.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return scanReferrals(rows)
}

func (p *PGStore) MarkVoid(ctx context.Context, id int64, reason string) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set state = 'void', void_reason = $2
		  where id = $1 and state in ('claimed','redeemed','qualified')`, id, reason)
	return err
}

func (p *PGStore) MarkQualified(ctx context.Context, id int64) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set state = 'qualified', qualified_at = now()
		  where id = $1 and state = 'redeemed'`, id)
	return err
}

func (p *PGStore) ListQualified(ctx context.Context, limit int) ([]Referral, error) {
	rows, err := p.pool.Query(ctx,
		`select `+referralColumns+` from referrals
		  where state = 'qualified' order by qualified_at limit $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanReferrals(rows)
}

func (p *PGStore) MarkSaved(ctx context.Context, id int64) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set state = 'saved' where id = $1 and state = 'qualified'`, id)
	return err
}

func (p *PGStore) Ledger(ctx context.Context, userID string) (Ledger, error) {
	led := Ledger{UserID: userID}
	var until *time.Time
	err := p.pool.QueryRow(ctx,
		`select months_granted, promo_until from referral_rewards where user_id = $1`, userID).
		Scan(&led.MonthsGranted, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return led, nil
	}
	if err != nil {
		return led, err
	}
	if until != nil {
		led.PromoUntil = *until
	}
	return led, nil
}

func (p *PGStore) ListSavers(ctx context.Context, limit int) ([]string, error) {
	rows, err := p.pool.Query(ctx,
		`select referrer from referrals where state = 'saved'
		  group by referrer order by min(qualified_at) limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (p *PGStore) SavedIDs(ctx context.Context, userID string) ([]int64, error) {
	rows, err := p.pool.Query(ctx,
		`select id from referrals where referrer = $1 and state = 'saved' order by qualified_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (p *PGStore) RecordGranted(ctx context.Context, userID string, ids []int64, until time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx,
		`insert into referral_rewards (user_id, months_granted, promo_until)
		 values ($1, $2, $3)
		 on conflict (user_id) do update
		    set months_granted = referral_rewards.months_granted + excluded.months_granted,
		        promo_until    = excluded.promo_until,
		        updated_at     = now()`, userID, len(ids), until); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`update referrals set state = 'rewarded', rewarded_at = now()
		  where id = any($1) and state in ('qualified','saved')`, ids); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
