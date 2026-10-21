package referral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	pgRaiseException  = "P0001" // plpgsql `raise exception` — our named refusals
	pgUndefinedTable  = "42P01" // migration not applied yet
	pgUndefinedColumn = "42703" // migration not applied yet (a column of it)
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

// Me calls the two-argument referral_me (016): the standing, plus installID
// upserted into referral_devices (gate 2).
func (p *PGStore) Me(ctx context.Context, userID, installID string) (Standing, error) {
	var raw []byte
	if err := p.pool.QueryRow(ctx, `select referral_me($1::text, $2::text)::text`, userID, installID).Scan(&raw); err != nil {
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

// ClaimCheck is referral_claim_check (016): judgement steps 2–4, read-only.
func (p *PGStore) ClaimCheck(ctx context.Context, userID, code, installID string) (ClaimKind, error) {
	var kind string
	if err := p.pool.QueryRow(ctx, `select referral_claim_check($1::text, $2::text, $3::text)`, userID, code, installID).Scan(&kind); err != nil {
		return ClaimNew, asFailure(err)
	}
	switch kind {
	case "again":
		return ClaimAgain, nil
	case "new":
		return ClaimNew, nil
	}
	return ClaimNew, fmt.Errorf("referral_claim_check: unexpected %q", kind)
}

// Claim runs the three-argument referral_claim (016) in a transaction and,
// when a hook is given, calls it before commit: the DeviceCheck bit0 set
// happens while the pool code is still only ours, and a failed set rolls the
// claim back with the code untouched (docs: 置位失败 → 回滚).
func (p *PGStore) Claim(ctx context.Context, userID, code, installID string, hook ClaimHook) (string, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var raw []byte
	if err := tx.QueryRow(ctx, `select referral_claim($1::text, $2::text, $3::text)::text`, userID, code, installID).Scan(&raw); err != nil {
		return "", asFailure(err)
	}
	var out struct {
		OfferCode string `json:"offer_code"`
		Fresh     *bool  `json:"fresh"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("referral_claim decode: %w", err)
	}
	if out.OfferCode == "" {
		return "", Failure{Code: ErrPoolEmpty}
	}
	// The three-argument function always says; if it ever does not, treat it
	// as new (setting bit0 on a re-claim only re-marks a friend's device).
	fresh := out.Fresh == nil || *out.Fresh
	if hook != nil {
		if err := hook(fresh); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if hook != nil && fresh {
			log.Printf("referral claim: commit failed AFTER the DeviceCheck hook ran (bit0 may be set without a claim): %v", err)
		}
		return "", err
	}
	return out.OfferCode, nil
}

// ReportRedeemed restamps redeem_reported_at on every report while the
// referral is still claimed: ListClaimed puts a row reported since its last
// look first, so a report after a look that found nothing yet is not lost.
func (p *PGStore) ReportRedeemed(ctx context.Context, userID, offerCode string) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set redeem_reported_at = now()
		  where referee = $1 and offer_code = $2 and state = 'claimed'`,
		userID, offerCode)
	return err
}

// DeleteUser removes the referral_users row; referrals (both sides) and the
// reward ledger cascade, pool codes keep their state with issued_to nulled.
// A missing table (migration not yet applied) is nothing to delete, not an
// error — account deletion must never 500 over this feature's rollout order.
//
// Before that, the subscription keys of this user's live referrals (either
// side) go into referral_used_subscriptions — the key and a time, no user id —
// so deleting the account and starting over cannot make the same Apple ID
// count again (gate 3). Missing 016 objects are skipped, like a missing table.
func (p *PGStore) DeleteUser(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("referral: delete user: empty user id")
	}
	if _, err := p.pool.Exec(ctx,
		`insert into referral_used_subscriptions (subscription_key)
		 select distinct subscription_key from referrals
		  where (referrer = $1 or referee = $1) and state <> 'void' and subscription_key is not null
		 on conflict (subscription_key) do nothing`, userID); err != nil && !undefinedObject(err) {
		return fmt.Errorf("keep used subscription keys: %w", err)
	}
	_, err := p.pool.Exec(ctx, `delete from referral_users where user_id = $1`, userID)
	if undefinedObject(err) {
		return nil
	}
	return err
}

// undefinedObject: a table or column a not-yet-applied migration adds.
func undefinedObject(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == pgUndefinedTable || pgErr.Code == pgUndefinedColumn)
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
		`select issue_margin_days, swap_grace_days, low_stock_threshold, expiry_warn_days
		   from referral_settings where id = 1`).
		Scan(&st.Settings.IssueMarginDays, &st.Settings.SwapGraceDays, &st.Settings.LowStockThreshold, &st.Settings.ExpiryWarnDays); err != nil {
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
	// long expired (6 months) — they stay 'claimed' and cost nothing. A
	// re-claim that swapped the code restarts the window (reissued_at, 014);
	// claimed_at itself stays the first claim, which is what StartedAfter
	// compares against.
	//
	// Rotation (017): a fixed order used to hand back the same `limit` rows
	// every pass once more than a batch of never-redeemed claims had piled
	// up, and nothing behind them was ever looked at. Now: reported /redeemed
	// since the last look first, then never looked at, then looked at longest
	// ago (MarkClaimedChecked stamps last_checked_at after each answer).
	rows, err := p.pool.Query(ctx,
		`select `+referralColumns+` from referrals
		  where state = 'claimed' and coalesce(reissued_at, claimed_at) > now() - interval '200 days'
		  order by (redeem_reported_at is not null
		            and redeem_reported_at > coalesce(last_checked_at, '-infinity'::timestamptz)) desc,
		           last_checked_at asc nulls first,
		           claimed_at, id
		  limit $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanReferrals(rows)
}

func (p *PGStore) MarkClaimedChecked(ctx context.Context, id int64, at time.Time) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set last_checked_at = $2 where id = $1 and state = 'claimed'`, id, at)
	return err
}

// MarkRedeemed is referral_mark_redeemed (016): claimed → redeemed with the
// subscription key, or void (duplicate_apple_id) when the key is taken.
func (p *PGStore) MarkRedeemed(ctx context.Context, id int64, at time.Time, subscriptionKey string) (bool, error) {
	var outcome string
	if err := p.pool.QueryRow(ctx, `select referral_mark_redeemed($1::bigint, $2::timestamptz, $3::text)`,
		id, at, subscriptionKey).Scan(&outcome); err != nil {
		return false, err
	}
	return outcome == "void", nil
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

// MarkVoid is referral_void (016); for refunded / no_subscription it touches
// only the referral (the refund's own webhook voids that charge).
func (p *PGStore) MarkVoid(ctx context.Context, id int64, reason string) error {
	_, err := p.pool.Exec(ctx, `select referral_void($1::bigint, $2::text)`, id, reason)
	return err
}

// MarkQualified is referral_qualify (016): redeemed → qualified, or held by
// the velocity gate.
func (p *PGStore) MarkQualified(ctx context.Context, id int64) (bool, error) {
	var outcome string
	if err := p.pool.QueryRow(ctx, `select referral_qualify($1::bigint)`, id).Scan(&outcome); err != nil {
		return false, err
	}
	return outcome == "held", nil
}

// HeldDigests: referrers with held referrals not yet mailed, none of whose
// referrals was mailed in the last 24 hours; per referrer every held referral
// plus the successes inside the velocity window.
func (p *PGStore) HeldDigests(ctx context.Context, limit int) ([]HeldDigest, error) {
	rows, err := p.pool.Query(ctx,
		`select r.referrer from referrals r
		  where r.state = 'held' and r.held_notified_at is null
		    and not exists (select 1 from referrals x
		                     where x.referrer = r.referrer and x.held_notified_at > now() - interval '24 hours')
		  group by r.referrer
		  order by min(r.qualified_at)
		  limit $1`, limit)
	if err != nil {
		return nil, err
	}
	var referrers []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return nil, err
		}
		referrers = append(referrers, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(referrers) == 0 {
		return nil, nil
	}
	var lim, window int
	if err := p.pool.QueryRow(ctx,
		`select velocity_limit, velocity_window_hours from referral_settings where id = 1`).Scan(&lim, &window); err != nil {
		return nil, fmt.Errorf("velocity settings: %w", err)
	}
	out := make([]HeldDigest, 0, len(referrers))
	for _, u := range referrers {
		d := HeldDigest{Referrer: u, Limit: lim, WindowHours: window}
		rows, err := p.pool.Query(ctx,
			`select id, referee, state, claimed_at, coalesce(qualified_at, claimed_at),
			        coalesce(claim_install_id, ''), held_notified_at is not null
			   from referrals
			  where referrer = $1
			    and (state = 'held'
			         or (state in ('qualified','saved','rewarded')
			             and qualified_at > now() - make_interval(hours => $2::int)))
			  order by qualified_at, id`, u, window)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h HeldReferral
			if err := rows.Scan(&h.ID, &h.Referee, &h.State, &h.ClaimedAt, &h.QualifiedAt, &h.InstallID, &h.Notified); err != nil {
				rows.Close()
				return nil, err
			}
			d.Referrals = append(d.Referrals, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (p *PGStore) MarkHeldNotified(ctx context.Context, ids []int64) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set held_notified_at = now()
		  where id = any($1::bigint[]) and state = 'held' and held_notified_at is null`, ids)
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

// MarkSaved clears last_checked_at: whatever ① stamped there while it was
// claimed is not a look at the referrer, and a fresh saver should be taken
// early by ListSavers.
func (p *PGStore) MarkSaved(ctx context.Context, id int64) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set state = 'saved', last_checked_at = null where id = $1 and state = 'qualified'`, id)
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

// ListSavers rotates like ListClaimed (017): a referrer with any saved
// referral not looked at yet first, then the least recently looked at — a
// fixed order kept handing back the same `limit` still-paying referrers.
func (p *PGStore) ListSavers(ctx context.Context, limit int) ([]string, error) {
	rows, err := p.pool.Query(ctx,
		`select referrer from referrals where state = 'saved'
		  group by referrer
		  order by min(coalesce(last_checked_at, '-infinity'::timestamptz)), min(qualified_at), referrer
		  limit $1`, limit)
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

func (p *PGStore) MarkSaverChecked(ctx context.Context, userID string, at time.Time) error {
	_, err := p.pool.Exec(ctx,
		`update referrals set last_checked_at = $2 where referrer = $1 and state = 'saved'`, userID, at)
	return err
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
