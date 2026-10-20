package referral

import (
	"context"
	"errors"
	"time"
)

// Standing is what GET /v1/referral/me returns — the invite page draws itself
// from it. Field names are the wire contract with the iOS app (docs:
// invite-friends.md ## 数据契约).
type Standing struct {
	// Code is this user's own invite code (YM-7KQ2). Ours, not Apple's.
	Code string `json:"code"`
	// Invited is how many friends have put that code in.
	Invited int `json:"invited"`
	// MonthsEarned is how many referrals have succeeded (friend stayed the hold
	// out), INCLUDING the ones still saved for later.
	MonthsEarned int `json:"months_earned"`
	// MonthsPending is how many friends have redeemed but not yet served the
	// hold.
	MonthsPending int `json:"months_pending"`
	// MonthsSaved is how many earned months are waiting for this user's own
	// paid plan to end before they are granted.
	MonthsSaved int `json:"months_saved"`
	// CanClaim is whether this account may still put in a friend's code: never
	// claimed before, and no subscription history on RevenueCat.
	CanClaim bool `json:"can_claim"`
}

// Referral is one A→B record as the settlement loop sees it.
type Referral struct {
	ID         int64
	Referrer   string // Supabase user id (lowercase uuid string)
	Referee    string
	State      string
	ClaimedAt  time.Time
	OfferCode  string
	RedeemedAt time.Time // zero until state >= redeemed
}

// Ledger is a referrer's reward account.
type Ledger struct {
	UserID        string
	MonthsGranted int
	// PromoUntil is the RevenueCat promotional expiry we last wrote, or zero.
	// It is the base the next grant stacks on.
	PromoUntil time.Time
}

// Referral states (referrals.state).
const (
	StateClaimed   = "claimed"
	StateRedeemed  = "redeemed"
	StateQualified = "qualified"
	StateSaved     = "saved"
	StateRewarded  = "rewarded"
	StateVoid      = "void"
)

// Failure is a refusal the store named. The handler turns it into an HTTP
// status; the app turns that into a sentence.
type Failure struct{ Code string }

func (f Failure) Error() string { return f.Code }

// The refusals the SQL side can name. Everything else is a 502.
const (
	ErrUnknownCode    = "unknown_code"    // nobody has that code
	ErrOwnCode        = "own_code"        // it is their own
	ErrAlreadyClaimed = "already_claimed" // this account has already used someone's code
	ErrNotNewUser     = "not_new_user"    // this account has had Premium before
	ErrPoolEmpty      = "pool_empty"      // no unexpired free code left
)

// named reports whether err is one of the refusals above, and which.
func named(err error) (Failure, bool) {
	var f Failure
	ok := errors.As(err, &f)
	return f, ok
}

// Store is everything the endpoints and the settlement loop need from the
// database. An interface so the handler + settle tests run without one; the
// only implementation is PGStore (pgstore.go) over the shared Supabase pool.
type Store interface {
	// Me is the user's own standing, making their code on the spot the first
	// time they ask. CanClaim here reflects the database only (never claimed);
	// the RevenueCat half is applied by the handler.
	Me(ctx context.Context, userID string) (Standing, error)
	// Claim records referrer→referee and hands back an Apple one-time code
	// from the pool, atomically. code is already normalised.
	Claim(ctx context.Context, userID, code string) (offerCode string, err error)
	// ReportRedeemed notes that the app says offerCode was redeemed by userID.
	// A hint for the settlement loop, never a proof. Idempotent.
	ReportRedeemed(ctx context.Context, userID, offerCode string) error
	// DeleteUser removes the user's invite code, every referral they are on
	// either side of, and their reward ledger (account deletion).
	DeleteUser(ctx context.Context, userID string) error

	// --- settlement (settle.go) ---

	// ExpireCodes marks free/issued pool codes past their expiry.
	ExpireCodes(ctx context.Context) (int64, error)
	// HoldDays reads referral_settings.hold_days.
	HoldDays(ctx context.Context) (int, error)
	// ListClaimed returns referrals still waiting for the referee to redeem,
	// most recently reported first.
	ListClaimed(ctx context.Context, limit int) ([]Referral, error)
	// MarkRedeemed moves a claimed referral to redeemed at the subscription's
	// real start time (from RevenueCat).
	MarkRedeemed(ctx context.Context, id int64, at time.Time) error
	// ListDue returns redeemed referrals whose hold has elapsed.
	ListDue(ctx context.Context, hold time.Duration, limit int) ([]Referral, error)
	// MarkVoid retires a referral (refund).
	MarkVoid(ctx context.Context, id int64, reason string) error
	// MarkQualified moves a redeemed referral to qualified.
	MarkQualified(ctx context.Context, id int64) error
	// ListQualified returns qualified referrals not yet rewarded or saved.
	ListQualified(ctx context.Context, limit int) ([]Referral, error)
	// MarkSaved parks a qualified referral until the referrer's paid plan ends.
	MarkSaved(ctx context.Context, id int64) error
	// Ledger reads a referrer's reward account (zero value when none).
	Ledger(ctx context.Context, userID string) (Ledger, error)
	// ListSavers returns referrers who have at least one saved referral.
	ListSavers(ctx context.Context, limit int) ([]string, error)
	// SavedIDs returns the saved referral ids of one referrer.
	SavedIDs(ctx context.Context, userID string) ([]int64, error)
	// RecordGranted marks ids rewarded and advances the ledger to until,
	// adding len(ids) months, in one transaction.
	RecordGranted(ctx context.Context, userID string, ids []int64, until time.Time) error
}
