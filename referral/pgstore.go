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
	pgRaiseException   = "P0001" // plpgsql `raise exception` — our named refusals
	pgUndefinedTable   = "42P01" // migration not applied yet
	pgUndefinedColumn  = "42703" // migration not applied yet (a column of it)
	pgLockNotAvailable = "55P03" // lock_timeout ran out (the gated claim's device lock)
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

// PendingOffer calls referral_pending_offer (022). It may swap the code (the
// re-claim path of referral_claim), so it runs as its own statement — one
// implicit transaction.
func (p *PGStore) PendingOffer(ctx context.Context, userID, installID string) (*PendingOffer, error) {
	var raw []byte
	if err := p.pool.QueryRow(ctx, `select referral_pending_offer($1::text, $2::text)::text`, userID, installID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("referral_pending_offer: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	var out PendingOffer
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("referral_pending_offer decode: %w", err)
	}
	if out.OfferCode == "" || out.FriendCode == "" {
		return nil, nil
	}
	return &out, nil
}

// OfferVariant reads referrals.offer_variant (023) for this friend.
func (p *PGStore) OfferVariant(ctx context.Context, userID string) (string, error) {
	var v *string
	err := p.pool.QueryRow(ctx, `select offer_variant from referrals where referee = $1`, userID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) || undefinedObject(err) {
		return VariantMonth, nil
	}
	if err != nil {
		return VariantMonth, err
	}
	return NormaliseVariant(v), nil
}

// ClaimCheck is referral_claim_check (016 / 022): judgement steps 2–4, read-only.
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

// deviceLockWait bounds how long a gated claim waits for the device lock
// (lock_timeout, which also covers the claim's row locks): the holder is at
// most a few Apple calls away from commit. Past it → device_check_unavailable.
const deviceLockWait = 3 * time.Second

// deviceLockSQL is the one transaction-scoped advisory lock every gated claim
// takes (ClaimGate). Transaction-scoped, so it is safe behind Supabase's
// transaction-mode pooler and can never outlive the claim.
const deviceLockSQL = `select pg_advisory_xact_lock(hashtextextended('referral_devicecheck', 0))`

// Claim runs the three-argument referral_claim (016 / 017) in a transaction.
// With a gate: lock_timeout, the global device lock, referral_claim_check
// again under it (a claim of this account that committed while we waited
// makes this a re-claim — Apple is not asked), gate.Check for a first
// claim, then referral_claim, then gate.Set — all before commit: bit0 is set
// while the pool code is still only ours, and a failed check or set rolls
// the claim back with the code untouched (docs: 置位失败 → 回滚).
func (p *PGStore) Claim(ctx context.Context, userID, code, installID string, gate *ClaimGate) (string, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// 023: an app build that does not know the experiment groups gets the
	// monthly group for a new referral (transaction-local, so it never leaks
	// to the next user of the pooled connection).
	if ForceMonth(ctx) {
		if _, err := tx.Exec(ctx, `select set_config('referral.force_month', 'on', true)`); err != nil {
			return "", err
		}
	}
	if gate != nil {
		if _, err := tx.Exec(ctx, fmt.Sprintf("set local lock_timeout = '%dms'", deviceLockWait.Milliseconds())); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, deviceLockSQL); err != nil {
			return "", claimFailure(err)
		}
		var kind string
		if err := tx.QueryRow(ctx, `select referral_claim_check($1::text, $2::text, $3::text)`, userID, code, installID).Scan(&kind); err != nil {
			return "", claimFailure(err)
		}
		if kind == "new" && gate.Check != nil {
			if err := gate.Check(); err != nil {
				return "", err
			}
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `select referral_claim($1::text, $2::text, $3::text)::text`, userID, code, installID).Scan(&raw); err != nil {
		return "", claimFailure(err)
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
	if gate != nil && gate.Set != nil {
		if err := gate.Set(fresh); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if gate != nil && fresh {
			log.Printf("referral claim: commit failed AFTER the DeviceCheck set (bit0 may be set without a claim): %v", err)
		}
		return "", err
	}
	return out.OfferCode, nil
}

// claimFailure is asFailure plus the device lock (or a row lock behind it)
// not coming free within deviceLockWait: busy, not broken — 503
// device_check_unavailable, "try again".
func claimFailure(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgLockNotAvailable {
		log.Printf("referral claim: device lock busy for %s; answering device_check_unavailable", deviceLockWait)
		return Failure{Code: ErrDeviceUnavailable}
	}
	return asFailure(err)
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
// count again (gate 3). Copy and delete are one transaction with the user's
// referrals locked first, so settlement cannot stamp a subscription_key
// (referral_mark_redeemed) between the copy and the cascade and have it
// vanish with neither a live referral nor a tombstone left to stop it.
// Missing 016 objects are skipped, like a missing table.
func (p *PGStore) DeleteUser(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("referral: delete user: empty user id")
	}
	err := p.deleteUserKeepingKeys(ctx, userID)
	if !undefinedObject(err) {
		return err
	}
	// 013 without 016 (no subscription_key yet: nothing to keep), or no
	// referral tables at all (then this is undefined too → nothing to delete).
	_, err = p.pool.Exec(ctx, `delete from referral_users where user_id = $1`, userID)
	if undefinedObject(err) {
		return nil
	}
	return err
}

func (p *PGStore) deleteUserKeepingKeys(ctx context.Context, userID string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// The same order the cascade takes them: the user row, then the referrals.
	if _, err := tx.Exec(ctx, `select 1 from referral_users where user_id = $1 for update`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`select 1 from referrals where referrer = $1 or referee = $1 for update`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`insert into referral_used_subscriptions (subscription_key)
		 select distinct subscription_key from referrals
		  where (referrer = $1 or referee = $1) and state <> 'void' and subscription_key is not null
		 on conflict (subscription_key) do nothing`, userID); err != nil {
		if undefinedObject(err) {
			return err
		}
		return fmt.Errorf("keep used subscription keys: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from referral_users where user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	// 023: the experiment's share and the yearly group's code name. Missing
	// columns (023 not run yet) leave both zero — one monthly pool, as before.
	err = p.pool.QueryRow(ctx, `select offer_year_pct, offer_ref_month, offer_ref_year from referral_settings where id = 1`).
		Scan(&st.Settings.OfferYearPct, &st.Settings.OfferRefMonth, &st.Settings.OfferRefYear)
	if err != nil && !undefinedObject(err) {
		return st, fmt.Errorf("pool settings (023): %w", err)
	}
	if err == nil {
		if err := p.pool.QueryRow(ctx,
			`select count(*) from referrals r join code_pool c on c.code = r.offer_code
			  where r.state = 'claimed' and c.state = 'issued' and r.offer_variant = 'year'`).
			Scan(&st.UnredeemedYear); err != nil {
			return st, fmt.Errorf("pool unredeemed yearly: %w", err)
		}
	}
	return st, nil
}

func (p *PGStore) HoldDays(ctx context.Context) (int, error) {
	var days int
	err := p.pool.QueryRow(ctx, `select hold_days from referral_settings where id = 1`).Scan(&days)
	return days, err
}

const referralColumns = `id, referrer, referee, state, claimed_at, coalesce(offer_code, ''), redeemed_at, reviewed_at is not null`

func scanReferrals(rows pgx.Rows) ([]Referral, error) {
	defer rows.Close()
	var out []Referral
	for rows.Next() {
		var r Referral
		var redeemed *time.Time
		if err := rows.Scan(&r.ID, &r.Referrer, &r.Referee, &r.State, &r.ClaimedAt, &r.OfferCode, &redeemed, &r.Reviewed); err != nil {
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

// MarkQualified is referral_qualify (017): redeemed → qualified, or held by
// the velocity gate (counted on theoretical success time).
func (p *PGStore) MarkQualified(ctx context.Context, id int64) (bool, error) {
	var outcome string
	if err := p.pool.QueryRow(ctx, `select referral_qualify($1::bigint)`, id).Scan(&outcome); err != nil {
		return false, err
	}
	return outcome == "held", nil
}

// HeldDigests: referrers with held referrals not yet mailed, none of whose
// referrals was mailed in the last 24 hours; per referrer every held referral
// plus the successes that counted against one of them — theoretical success
// time (redeemed_at + hold_days; with one hold_days for all, redeemed_at
// itself) inside the window before that held one's, as referral_qualify (017)
// counts.
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
	var lim, window, hold int
	if err := p.pool.QueryRow(ctx,
		`select velocity_limit, velocity_window_hours, hold_days from referral_settings where id = 1`).Scan(&lim, &window, &hold); err != nil {
		return nil, fmt.Errorf("velocity settings: %w", err)
	}
	out := make([]HeldDigest, 0, len(referrers))
	for _, u := range referrers {
		d := HeldDigest{Referrer: u, Limit: lim, WindowHours: window}
		rows, err := p.pool.Query(ctx,
			`select r.id, r.referee, r.state, r.claimed_at, coalesce(r.qualified_at, r.claimed_at),
			        coalesce(r.redeemed_at, r.qualified_at, r.claimed_at) + make_interval(days => $3::int) as due_at,
			        coalesce(r.claim_install_id, ''), r.held_notified_at is not null
			   from referrals r
			  where r.referrer = $1
			    and (r.state = 'held'
			         or (r.state in ('qualified','saved','rewarded')
			             and exists (select 1 from referrals h
			                          where h.referrer = r.referrer and h.state = 'held'
			                            and coalesce(r.redeemed_at, r.qualified_at)
			                                  > coalesce(h.redeemed_at, h.qualified_at) - make_interval(hours => $2::int)
			                            and coalesce(r.redeemed_at, r.qualified_at)
			                                  <= coalesce(h.redeemed_at, h.qualified_at))))
			  order by due_at, r.id`, u, window, hold)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h HeldReferral
			if err := rows.Scan(&h.ID, &h.Referee, &h.State, &h.ClaimedAt, &h.QualifiedAt, &h.DueAt, &h.InstallID, &h.Notified); err != nil {
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
