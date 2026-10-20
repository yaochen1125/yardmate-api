package rewards

import (
	"context"
	"errors"
	"time"
)

// Ledger states (reward_ledger.state, migration 015).
const (
	LedgerPending   = "pending"   // inside Apple's 90-day refund window
	LedgerAvailable = "available" // counts towards the redeemable balance
	LedgerVoided    = "voided"    // refunded while still pending
)

// Payout states (reward_payouts.state, migration 015).
const (
	PayoutRequested = "requested" // waiting for the operator to buy + send the card
	PayoutSent      = "sent"
	PayoutCancelled = "cancelled" // operator cancelled; the amount is back in the balance
)

// The referral states (013 referrals.state) whose referee's payments count:
// the friend has redeemed. claimed is handled separately (see eligible).
const (
	refClaimed   = "claimed"
	refRedeemed  = "redeemed"
	refQualified = "qualified"
	refSaved     = "saved"
	refRewarded  = "rewarded"
)

// Referral is the referee's referral row as the webhook needs it.
type Referral struct {
	ID        int64
	Referrer  string // Supabase user id (lowercase uuid string)
	Referee   string
	State     string
	ClaimedAt time.Time
}

// Credit is one reward_ledger row to write: one real charge to the friend.
type Credit struct {
	ReferralID            int64
	Referrer              string
	Referee               string
	EventID               string // RevenueCat event.id — the dedupe key
	EventType             string // INITIAL_PURCHASE / RENEWAL
	TransactionID         string
	OriginalTransactionID string
	ProductID             string
	PeriodType            string // NORMAL / INTRO
	PriceUSDCents         int64
	Takehome              string // decimal text, e.g. "0.8500"
	CreditCents           int64
	PurchasedAt           time.Time
	AvailableAt           time.Time
}

// Payout is one reward_payouts row.
type Payout struct {
	ID          int64
	UserID      string
	AmountCents int64
	Email       string
	State       string
	RequestedAt time.Time
	SentAt      time.Time // zero unless sent
}

// Balance is the raw sums the /me block is computed from (see Summarize).
type Balance struct {
	AvailableLedgerCents int64   // sum of available ledger rows (= lifetime)
	PendingCents         int64   // sum of pending ledger rows
	SpentCents           int64   // sum of requested + sent payouts
	Latest               *Payout // most recent payout of any state; nil when none
}

// YearTotals is what one user has had in gift cards this calendar year (UTC),
// for the operator's 1099-NEC / W-9 check. Includes the request just made.
type YearTotals struct {
	SentCents      int64 // sent, sent_at in this year
	RequestedCents int64 // still requested (at most one: the new one)
}

// Total is sent + requested.
func (y YearTotals) Total() int64 { return y.SentCents + y.RequestedCents }

// The two refusals a payout request can get from the store. Anything else is a
// 502 store_failed.
var (
	ErrBelowThreshold   = errors.New("below_threshold")
	ErrPayoutInProgress = errors.New("payout_in_progress")
)

// Store is everything the webhook, the maturing loop, /me and /payout need
// from the database. An interface so the tests run without one; the only
// implementation is PGStore (pgstore.go) over the shared Supabase pool.
type Store interface {
	// ReferralByReferee finds the referral whose friend is referee (a lowercase
	// Supabase user id). ok=false when there is none.
	ReferralByReferee(ctx context.Context, referee string) (ref Referral, ok bool, err error)
	// RecordCredit writes one pending credit. inserted=false when the event id
	// is already in the ledger (a RevenueCat redelivery) — not an error.
	RecordCredit(ctx context.Context, c Credit) (inserted bool, err error)
	// VoidPending voids the PENDING credits of one store transaction (refund).
	// Already-available credits are left alone.
	VoidPending(ctx context.Context, transactionID string) (int64, error)
	// MatureDue turns pending credits whose available_at has passed into
	// available.
	MatureDue(ctx context.Context) (int64, error)
	// Balance reads the sums behind the /me rewards block.
	Balance(ctx context.Context, userID string) (Balance, error)
	// RequestPayout atomically: refuses with ErrPayoutInProgress when the user
	// already has a requested payout, with ErrBelowThreshold when the
	// redeemable balance is under threshold, else inserts a requested payout
	// of amount and returns it with the user's totals for the calendar year
	// starting at yearStart.
	RequestPayout(ctx context.Context, userID, email string, amount, threshold int64, yearStart time.Time) (Payout, YearTotals, error)
}
