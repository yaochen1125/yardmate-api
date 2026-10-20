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

func (p *PGStore) RecordCredit(ctx context.Context, c Credit) (bool, error) {
	// Takehome goes in as text and is cast, so no float ever touches the
	// stored value.
	tag, err := p.pool.Exec(ctx,
		`insert into reward_ledger
		   (referral_id, referrer, referee, rc_event_id, event_type, transaction_id,
		    original_transaction_id, product_id, period_type, price_usd_cents,
		    takehome_percentage, credit_cents, purchased_at, available_at)
		 values ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $9, $10, $11::text::numeric, $12, $13, $14)
		 on conflict (rc_event_id) do nothing`,
		c.ReferralID, c.Referrer, c.Referee, c.EventID, c.EventType, c.TransactionID,
		c.OriginalTransactionID, c.ProductID, c.PeriodType, c.PriceUSDCents,
		c.Takehome, c.CreditCents, c.PurchasedAt, c.AvailableAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (p *PGStore) VoidPending(ctx context.Context, transactionID string) (int64, error) {
	tag, err := p.pool.Exec(ctx,
		`update reward_ledger set state = 'voided', voided_at = now()
		  where transaction_id = $1 and state = 'pending'`, transactionID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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

const payoutColumns = `id, user_id, amount_cents, email, state, requested_at, sent_at`

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
		`select exists (select 1 from reward_payouts where user_id = $1 and state = 'requested'),
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
