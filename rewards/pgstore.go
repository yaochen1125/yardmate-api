package rewards

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the only Store: Supabase Postgres over the shared pgx pool
// (proxy/enrichment migrations 013_referral.sql + 015_referral_rewards.sql).
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

const pgUniqueViolation = "23505"

func (p *PGStore) ReferralByReferee(ctx context.Context, referee string) (Referral, bool, error) {
	var r Referral
	err := p.pool.QueryRow(ctx,
		`select id, referrer, referee, state, claimed_at from referrals where referee = $1`, referee).
		Scan(&r.ID, &r.Referrer, &r.Referee, &r.State, &r.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Referral{}, false, nil
	}
	if err != nil {
		return Referral{}, false, err
	}
	return r, true, nil
}

// txnLockSQL serialises RecordCredit and VoidPending of one transaction id
// (a transaction-scoped advisory lock), so a charge and its refund handled at
// the same moment cannot miss each other — the refund finding nothing pending
// while the charge, not yet seeing the refund's tombstone, inserts.
const txnLockSQL = `select pg_advisory_xact_lock(hashtextextended('reward_txn:' || $1::text, 0))`

func (p *PGStore) RecordCredit(ctx context.Context, c Credit) (CreditResult, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, txnLockSQL, c.TransactionID); err != nil {
		return 0, err
	}
	// The webhook judged the referral from an earlier read. Re-read it under
	// a share lock: referral_void (settlement's duplicate_apple_id, an
	// operator's reject) updates this row before voiding its ledger, so either
	// it committed first and the row reads void here, or it waits for this
	// insert and then voids it along with the rest.
	var refState string
	err = tx.QueryRow(ctx, `select state from referrals where id = $1 for share`, c.ReferralID).Scan(&refState)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditReferralVoid, nil // deleted with an account since
	}
	if err != nil {
		return 0, err
	}
	if refState == "void" {
		return CreditReferralVoid, nil
	}
	// Takehome goes in as text and is cast, so no float ever touches the
	// stored value. "on conflict do nothing" without a target: both unique
	// indexes (rc_event_id, transaction_id) make a redelivery a no-op. The
	// not-exists skips a transaction already refunded (VoidPending's tombstone).
	tag, err := tx.Exec(ctx,
		`insert into reward_ledger
		   (referral_id, referrer, referee, rc_event_id, event_type, transaction_id,
		    original_transaction_id, product_id, period_type, price_usd_cents,
		    takehome_percentage, credit_cents, purchased_at, available_at)
		 select $1::bigint, $2::text, $3::text, $4::text, $5::text, $6::text,
		        nullif($7::text, ''), $8::text, $9::text, $10::int,
		        $11::text::numeric, $12::int, $13::timestamptz, $14::timestamptz
		  where not exists (select 1 from reward_voided_txns where transaction_id = $6::text)
		 on conflict do nothing`,
		c.ReferralID, c.Referrer, c.Referee, c.EventID, c.EventType, c.TransactionID,
		c.OriginalTransactionID, c.ProductID, c.PeriodType, c.PriceUSDCents,
		c.Takehome, c.CreditCents, c.PurchasedAt, c.AvailableAt)
	if err != nil {
		return 0, err
	}
	res := CreditInserted
	if tag.RowsAffected() == 0 {
		// Nothing written: a ledger row for this event / transaction (a
		// redelivery — even one refunded since), else the tombstone.
		var dup bool
		if err := tx.QueryRow(ctx,
			`select exists (select 1 from reward_ledger where rc_event_id = $1 or transaction_id = $2)`,
			c.EventID, c.TransactionID).Scan(&dup); err != nil {
			return 0, err
		}
		res = CreditVoided
		if dup {
			res = CreditDuplicate
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return res, nil
}

func (p *PGStore) VoidPending(ctx context.Context, transactionID string) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, txnLockSQL, transactionID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx,
		`update reward_ledger set state = 'voided', voided_at = now(), void_reason = 'refunded'
		  where transaction_id = $1 and state = 'pending'`, transactionID)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	if n == 0 {
		// Refund before (or without) its charge: leave a tombstone that
		// RecordCredit checks. Also written for a refund of an available or
		// unknown transaction — harmless, the charge is in the ledger already
		// or will never come.
		if _, err := tx.Exec(ctx,
			`insert into reward_voided_txns (transaction_id) values ($1) on conflict do nothing`, transactionID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

func (p *PGStore) MatureDue(ctx context.Context) (int64, error) {
	tag, err := p.pool.Exec(ctx,
		`update reward_ledger set state = 'available', matured_at = now()
		  where state = 'pending' and available_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// payoutColumns reads a payout the way everyone outside the operator script
// sees it. A payout marked sent less than an hour ago (undo_until > now(),
// migration 018, rewards.md ### 标记已发) still reads as requested with no
// sent_at: the App keeps showing "on its way", /payout keeps answering
// payout_in_progress, and an undo inside the hour leaves no trace. now() is
// the database clock — the same one reward_payout_mark_sent / _undo use — so
// the API server's clock can never disagree with the undo deadline.
const payoutColumns = `id, user_id, amount_cents, email,
	case when state = 'sent' and undo_until > now() then 'requested' else state end,
	requested_at,
	case when state = 'sent' and undo_until > now() then null else sent_at end`

func scanPayout(row pgx.Row) (Payout, error) {
	var po Payout
	var sent *time.Time
	if err := row.Scan(&po.ID, &po.UserID, &po.AmountCents, &po.Email, &po.State, &po.RequestedAt, &sent); err != nil {
		return Payout{}, err
	}
	if sent != nil {
		po.SentAt = *sent
	}
	return po, nil
}

func (p *PGStore) Balance(ctx context.Context, userID string) (Balance, error) {
	var b Balance
	if err := p.pool.QueryRow(ctx,
		`select coalesce((select sum(credit_cents) from reward_ledger
		                   where referrer = $1 and state = 'available'), 0),
		        coalesce((select sum(credit_cents) from reward_ledger
		                   where referrer = $1 and state = 'pending'), 0),
		        coalesce((select sum(amount_cents) from reward_payouts
		                   where user_id = $1 and state in ('requested','sent')), 0)`, userID).
		Scan(&b.AvailableLedgerCents, &b.PendingCents, &b.SpentCents); err != nil {
		return Balance{}, err
	}
	po, err := scanPayout(p.pool.QueryRow(ctx,
		`select `+payoutColumns+` from reward_payouts
		  where user_id = $1 order by requested_at desc, id desc limit 1`, userID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Balance{}, err
	default:
		b.Latest = &po
	}
	return b, nil
}

func (p *PGStore) RequestPayout(ctx context.Context, userID, email string, amount, threshold int64, yearStart time.Time) (Payout, YearTotals, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Payout{}, YearTotals{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Serialise this user's requests: two taps at once queue here, and the
	// second sees the first one's row. NO KEY UPDATE so the lock does not
	// block FK checks of inserts that reference the row (referrals, ledger).
	// No referral_users row → no ledger either → nothing to redeem.
	var one int
	err = tx.QueryRow(ctx, `select 1 from referral_users where user_id = $1 for no key update`, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payout{}, YearTotals{}, ErrBelowThreshold
	}
	if err != nil {
		return Payout{}, YearTotals{}, err
	}

	var inProgress bool
	var available, spent int64
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from reward_payouts
		                 where user_id = $1
		                   and (state = 'requested' or (state = 'sent' and undo_until > now()))),
		        coalesce((select sum(credit_cents) from reward_ledger
		                   where referrer = $1 and state = 'available'), 0),
		        coalesce((select sum(amount_cents) from reward_payouts
		                   where user_id = $1 and state in ('requested','sent')), 0)`, userID).
		Scan(&inProgress, &available, &spent); err != nil {
		return Payout{}, YearTotals{}, err
	}
	if inProgress {
		return Payout{}, YearTotals{}, ErrPayoutInProgress
	}
	if available-spent < threshold {
		return Payout{}, YearTotals{}, ErrBelowThreshold
	}

	po, err := scanPayout(tx.QueryRow(ctx,
		`insert into reward_payouts (user_id, amount_cents, email) values ($1, $2, $3)
		 returning `+payoutColumns, userID, amount, email))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		// The partial unique index caught a race the lock above should have
		// prevented; same answer either way.
		return Payout{}, YearTotals{}, ErrPayoutInProgress
	}
	if err != nil {
		return Payout{}, YearTotals{}, err
	}

	var year YearTotals
	if err := tx.QueryRow(ctx,
		`select coalesce(sum(amount_cents) filter (where state = 'sent' and sent_at >= $2), 0),
		        coalesce(sum(amount_cents) filter (where state = 'requested'), 0)
		   from reward_payouts where user_id = $1`, userID, yearStart).
		Scan(&year.SentCents, &year.RequestedCents); err != nil {
		return Payout{}, YearTotals{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Payout{}, YearTotals{}, err
	}
	return po, year, nil
}

// ReviewStats: the gate-5 numbers for one referrer (rewards.md ### 入账与兑换).
// Reads referrals' 016 columns (claim_install_id) and reward_ledger.void_reason.
func (p *PGStore) ReviewStats(ctx context.Context, userID string) (Review, error) {
	rv := Review{VoidByReason: map[string]int{}}
	if err := p.pool.QueryRow(ctx,
		`select count(*) filter (where state <> 'void'),
		        count(distinct claim_install_id) filter (where state <> 'void'),
		        count(*) filter (where state <> 'void' and claim_install_id is null),
		        count(*) filter (where state = 'held'),
		        (select count(distinct l.referral_id)
		           from reward_ledger l join referrals f on f.id = l.referral_id
		          where l.referrer = $1 and f.state <> 'void' and l.state <> 'voided'),
		        (select count(*) from reward_ledger
		          where referrer = $1 and state = 'voided' and coalesce(void_reason, 'refunded') = 'refunded')
		   from referrals where referrer = $1`, userID).
		Scan(&rv.Friends, &rv.ClaimDevices, &rv.NoDevice, &rv.Held, &rv.FriendsPaid, &rv.Refunds); err != nil {
		return rv, err
	}
	rows, err := p.pool.Query(ctx,
		`select coalesce(void_reason, 'unknown'), count(*) from referrals
		  where referrer = $1 and state = 'void' group by 1 order by 1`, userID)
	if err != nil {
		return rv, err
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return rv, err
		}
		rv.VoidByReason[reason] = n
	}
	return rv, rows.Err()
}

func (p *PGStore) Progress(ctx context.Context, userID string) ([]TrackReferral, []TrackCredit, error) {
	// One read-only snapshot for both reads: a friend deleting their account
	// (or a referral_void) between them would otherwise show the same friend
	// twice, or a voided referral once.
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	refs, err := progressReferrals(ctx, tx, userID)
	if err != nil {
		return nil, nil, err
	}
	credits, err := progressCredits(ctx, tx, userID)
	if err != nil {
		return nil, nil, err
	}
	return refs, credits, nil
}

func progressReferrals(ctx context.Context, tx pgx.Tx, userID string) ([]TrackReferral, error) {
	// referrals_referrer_idx (013) covers this.
	rows, err := tx.Query(ctx,
		`select id, state, claimed_at, redeemed_at, coalesce(reissued_at, claimed_at) from referrals
		  where referrer = $1 and state <> 'void'`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []TrackReferral
	for rows.Next() {
		var r TrackReferral
		var redeemed *time.Time
		if err := rows.Scan(&r.ID, &r.State, &r.ClaimedAt, &redeemed, &r.IssuedAt); err != nil {
			return nil, err
		}
		if redeemed != nil {
			r.RedeemedAt = *redeemed
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

func progressCredits(ctx context.Context, tx pgx.Tx, userID string) ([]TrackCredit, error) {
	// reward_ledger_referrer_state_idx (015) covers both halves. Every pending
	// row (bounded: 90 days of charges), but only the latest available row per
	// referral — available rows only grow, and Progress needs no more of them
	// than "has paid" and the latest product. referral_id is null once the
	// friend deleted their account: the money is still the referrer's.
	rows, err := tx.Query(ctx,
		`(select coalesce(referral_id, 0), state, credit_cents, product_id, purchased_at, available_at
		    from reward_ledger
		   where referrer = $1 and state = 'pending')
		 union all
		 (select distinct on (coalesce(referral_id, 0))
		         coalesce(referral_id, 0), state, credit_cents, product_id, purchased_at, available_at
		    from reward_ledger
		   where referrer = $1 and state = 'available'
		   order by coalesce(referral_id, 0), purchased_at desc, id desc)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var credits []TrackCredit
	for rows.Next() {
		var c TrackCredit
		if err := rows.Scan(&c.ReferralID, &c.State, &c.CreditCents, &c.ProductID, &c.PurchasedAt, &c.AvailableAt); err != nil {
			return nil, err
		}
		credits = append(credits, c)
	}
	return credits, rows.Err()
}
