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
	// refHeld is the referral package's anti-abuse gate 4 (migration 016):
	// waiting for an operator. The friend has redeemed, so money is still
	// recorded and matures as usual (rewards.md ### 防刷); a reject voids it.
	refHeld     = "held"
	refSaved    = "saved"
	refRewarded = "rewarded"
)

// VoidRefunded is reward_ledger.void_reason (migration 016) for a charge
// Apple refunded (webhook). The other reasons there are the referral's own
// (duplicate_apple_id / rejected), written by referral_void in SQL when the
// whole referral is voided.
const VoidRefunded = "refunded"

// Review is what the gift-card request mail adds for the operator's review
// (anti-abuse gate 5, rewards.md ### 入账与兑换): the referrer's friends and
// how believable they look.
type Review struct {
	// Friends is the referrer's referrals that are not void; FriendsPaid is
	// how many of them have brought in money (a ledger row not voided).
	Friends     int
	FriendsPaid int
	// ClaimDevices is the distinct claim_install_id among those friends;
	// NoDevice is how many of them have none recorded (claimed before
	// migration 016). All different ⇔ ClaimDevices == Friends - NoDevice.
	ClaimDevices int
	NoDevice     int
	// Refunds is the referrer's ledger rows voided by an Apple refund.
	Refunds int
	// VoidByReason counts void referrals by void_reason
	// (duplicate_apple_id / rejected / refunded / no_subscription …).
	VoidByReason map[string]int
	// Held is the referrals waiting for gate-4 review right now.
	Held int
}

// Referral is the referee's referral row as the webhook needs it.
type Referral struct {
	ID        int64
	Referrer  string // Supabase user id (lowercase uuid string)
	Referee   string
	State     string
	ClaimedAt time.Time
	// IssuedAt is coalesce(reissued_at, claimed_at): when the friend last got
	// an offer code — the start of settlement ①'s 200-day window, which the
	// webhook's on-the-spot redeem keeps to as well (chargeBeforeSettlement).
	IssuedAt time.Time
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

// CreditResult is what RecordCredit did with one credit.
type CreditResult int

const (
	CreditInserted     CreditResult = iota // a new pending row
	CreditDuplicate                        // its event id or transaction id is already in the ledger (a redelivery)
	CreditVoided                           // its transaction was refunded before this charge arrived; nothing written
	CreditReferralVoid                     // its referral was voided (or deleted) after the webhook read it; nothing written
)

// Payout is one reward_payouts row.
type Payout struct {
	ID          int64
	UserID      string
	AmountCents int64
	Email       string
	State       string
	RequestedAt time.Time
	SentAt      time.Time // zero unless sent
	// EmailEditable: the row really is requested — not marked sent, not even
	// inside the undo hour — so its owner may still change the email
	// (POST /v1/referral/payout/email, rewards.md ### 邮箱：确认、拼写提示、修改).
	EmailEditable bool
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
	// ErrPayoutNotEditable: ChangePayoutEmail found no requested payout of
	// the caller's (none, marked sent — undo hour included —, or cancelled).
	ErrPayoutNotEditable = errors.New("payout_not_editable")
)

// Store is everything the webhook, the maturing loop, /me and /payout need
// from the database. An interface so the tests run without one; the only
// implementation is PGStore (pgstore.go) over the shared Supabase pool.
type Store interface {
	// ReferralByReferee finds the referral whose friend is referee (a lowercase
	// Supabase user id). ok=false when there is none.
	ReferralByReferee(ctx context.Context, referee string) (ref Referral, ok bool, err error)
	// RecordCredit writes one pending credit. Not written, and not an error:
	// CreditDuplicate when its event id or its transaction id is already in
	// the ledger (a RevenueCat redelivery), CreditVoided when a refund of the
	// transaction got here first (reward_voided_txns, see VoidPending),
	// CreditReferralVoid when the referral is void or gone by the time the
	// credit is written (re-read under a row lock: referral_void cannot slip
	// in between and leave a fresh pending credit on a void referral).
	RecordCredit(ctx context.Context, c Credit) (CreditResult, error)
	// VoidPending voids the PENDING credits of one store transaction (refund).
	// Already-available credits are left alone. When it voids nothing, it
	// remembers the transaction id (reward_voided_txns) so a charge for it
	// that arrives later is not credited.
	VoidPending(ctx context.Context, transactionID string) (int64, error)
	// MatureDue turns pending credits whose available_at has passed into
	// available.
	MatureDue(ctx context.Context) (int64, error)
	// Balance reads the sums behind the /me rewards block.
	Balance(ctx context.Context, userID string) (Balance, error)
	// RequestPayout atomically: refuses with ErrPayoutInProgress when the user
	// already has a requested payout (or one in its undo hour), with
	// ErrBelowThreshold when PayoutAmountFor(redeemable balance) is 0 (under
	// $20), else inserts a requested payout of that amount — sized from the
	// balance read in the same transaction — and returns it with the user's
	// totals for the calendar year starting at yearStart.
	RequestPayout(ctx context.Context, userID, email string, yearStart time.Time) (Payout, YearTotals, error)
	// ReviewStats reads the referrer's review numbers for the payout mail.
	ReviewStats(ctx context.Context, userID string) (Review, error)
	// ChangePayoutEmail sets the email of the user's requested payout (the
	// one row the partial unique index allows) and stamps email_changed_at.
	// The same address again (case-insensitively) changes nothing:
	// changed=false and the payout as it is. No requested payout →
	// ErrPayoutNotEditable. old is the address it replaced.
	ChangePayoutEmail(ctx context.Context, userID, email string) (po Payout, old string, changed bool, err error)
	// W9ThresholdCents reads referral_settings.w9_threshold_cents (migration
	// 019): the 1099 filing threshold the payout mail's W-9 line is based on.
	W9ThresholdCents(ctx context.Context) (int64, error)
	// Progress reads what the card's counts and list are computed from
	// (Progress in progress.go): the referrer's referrals that are not void,
	// and their ledger rows that still count (pending / available, the
	// deleted friends' included with ReferralID 0).
	Progress(ctx context.Context, userID string) ([]TrackReferral, []TrackCredit, error)
}
