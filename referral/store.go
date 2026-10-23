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
	// Rewards is the gift-card block (rewards.md ## 数据契约), filled by the
	// injected RewardsReader; absent when none is injected or it failed.
	Rewards any `json:"rewards,omitempty"`
	// PendingOffer is the Apple code this user claimed as a friend and has not
	// redeemed yet, so a reinstall / new phone can get back to "redeem it"
	// (invite-friends.md ### 领了码没兑，换机 / 重装后找回). Absent when there is
	// none, or the lookup failed.
	PendingOffer *PendingOffer `json:"pending_offer,omitempty"`
}

// PendingOffer is a friend's claimed-but-unredeemed Apple code (already
// swapped when it was expired or close to it) plus the friend's code they put
// in (YM-XXXX).
type PendingOffer struct {
	OfferCode  string `json:"offer_code"`
	FriendCode string `json:"friend_code"`
	// OfferVariant is the group the friend is in (023: "month" / "year"),
	// which decides the renewal terms the app shows. Absent from an older
	// database; the app then shows the monthly terms.
	OfferVariant string `json:"offer_variant,omitempty"`
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
	// Reviewed: an operator approved / rejected it (reviewed_at, 016). A
	// qualified referral that is Reviewed came out of held, so ③ checks the
	// friend's refund again before paying (the ② check ran before the
	// review, however long that took).
	Reviewed bool
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
	// StateHeld is anti-abuse gate 4 (migration 016): the referrer already
	// had velocity_limit successes inside velocity_window_hours, so this one
	// waits for an operator (deploy/referral-review.sh approve / reject).
	// Counted in /me's months_pending; never granted while held.
	StateHeld     = "held"
	StateSaved    = "saved"
	StateRewarded = "rewarded"
	StateVoid     = "void"
)

// void_reason values (referrals.void_reason).
const (
	VoidRefunded       = "refunded"           // ② the friend's subscription was refunded
	VoidNoSubscription = "no_subscription"    // ② the subscription is gone by the end of the hold
	VoidDuplicate      = "duplicate_apple_id" // ① gate 3: another live referral has the same subscription
	VoidRejected       = "rejected"           // gate 4 / gift-card review: an operator said no
)

// ClaimKind is the /claim pre-check's verdict on judgement steps 2–4
// (docs: /claim 判定顺序), before RevenueCat and DeviceCheck are asked.
type ClaimKind int

const (
	// ClaimNew: a first claim — steps 5 (new user) and 6 (DeviceCheck) apply.
	ClaimNew ClaimKind = iota
	// ClaimAgain: this account already put this very code in. The answer is
	// the code it was given (swapped if it is about to expire), with no
	// RevenueCat or DeviceCheck look — "换台设备也能拿回".
	ClaimAgain
)

// ClaimGate is gate 1 (DeviceCheck) as a new claim's database transaction
// runs it. Apple's bits know nothing of our transactions, so "query bit0,
// hand out a code, set bit0" is only atomic if no two claims do it at once:
// Store.Claim first takes ONE global lock (held until commit / rollback; the
// claim volume is tiny), re-checks steps 2–4 under it, calls Check (only for
// a first claim), takes the pool code, then calls Set with fresh = this call
// created the referral. Of several accounts claiming on one device at the
// same moment — each with its own install id — only the first sees bit0
// clear. An error from either rolls the whole claim back (the code goes back
// to free) and comes out of Claim unchanged. Either func may be nil.
type ClaimGate struct {
	Check func() error
	Set   func(fresh bool) error
}

// HeldReferral is one line of a gate-4 review digest.
type HeldReferral struct {
	ID          int64
	Referee     string
	State       string // held, or qualified / saved / rewarded inside a held one's window
	ClaimedAt   time.Time
	QualifiedAt time.Time // when settlement judged it
	// DueAt is the theoretical success time the gate counts by (017):
	// redeemed_at + hold_days.
	DueAt     time.Time
	InstallID string // the claim's X-Device-Install-Id; "" for claims before migration 016
	Notified  bool   // a held one already mailed before
}

// HeldDigest is what the operator mail about one referrer's new held
// referrals is drawn from: every held referral of theirs plus every success
// that counted against one of them (theoretical success time inside the
// velocity window before that held one's), by theoretical success time.
type HeldDigest struct {
	Referrer    string
	Limit       int // velocity_limit
	WindowHours int // velocity_window_hours
	Referrals   []HeldReferral
}

// NewHeld lists the ids of held referrals not mailed yet.
func (d HeldDigest) NewHeld() []int64 {
	var ids []int64
	for _, r := range d.Referrals {
		if r.State == StateHeld && !r.Notified {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

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
	ErrPoolEmpty      = "pool_empty"      // no free code with the issue margin left
)

// The DeviceCheck refusals (gate 1). Decided in Go, not SQL.
const (
	ErrDeviceUsed        = "device_used"              // 400: bit0 already set — this device has been a friend
	ErrDeviceUnverified  = "device_unverified"        // 400 (enforce): no token, or Apple rejected it
	ErrDeviceUnavailable = "device_check_unavailable" // 503: Apple timed out / failed, or setting bit0 failed
)

// PoolSettings is the tunable part of referral_settings the code pool reads
// (migration 014). Changing the row takes effect on the next claim / pass —
// no deploy.
type PoolSettings struct {
	// OfferYearPct / OfferRefYear (023): the yearly group's share of new
	// friends and the code_pool.offer_ref_name its codes carry. Codes under
	// any other name are the monthly group's. 0 / "" on a database without
	// 023 — everything is the monthly group, as before.
	OfferYearPct  int
	OfferRefMonth string
	OfferRefYear  string
	// IssueMarginDays: /claim only hands out NEW codes that expire later than
	// now + this many days, so the friend has time to redeem (default 14).
	IssueMarginDays int
	// SwapGraceDays: on a re-claim the code already in hand is swapped only
	// when it has fewer than this many days left (default 3). Deliberately
	// smaller than the issue margin, or every code nearing the margin would
	// be replaced on a re-claim and burn a second good one.
	SwapGraceDays int
	// LowStockThreshold: fewer usable free codes than this → alert (default
	// 100). 0 turns both alerts off (expiring is a projected low stock).
	LowStockThreshold int
	// ExpiryWarnDays: the earliest usable batch expiring within this many
	// days, with fewer than LowStockThreshold usable codes left once it is
	// gone → alert (default 30).
	ExpiryWarnDays int
}

// PoolBatch is one import batch of code_pool: the codes that share an App
// Store Connect reference name and expiry (import-codes.sh gives a CSV one
// name and one date).
type PoolBatch struct {
	Name      string
	ExpiresAt time.Time
	Free      int // on the shelf (may be inside the issue margin — see Stock)
	Issued    int
	Redeemed  int
	Expired   int
}

// PoolStatus is code_pool as the settlement loop's watch sees it: the
// settings row plus every batch, earliest expiry first.
type PoolStatus struct {
	Settings PoolSettings
	Batches  []PoolBatch
	// UnredeemedYear (023): yearly-group referrals still holding an
	// unredeemed code. The yearly stock is watched while this is > 0 even
	// when the share is back at 0, so their expiring codes can still be
	// swapped.
	UnredeemedYear int
}

// RestockSettings is the auto-restock half of referral_settings (migration
// 021). Changing the row takes effect on the next pass — no deploy.
type RestockSettings struct {
	// Enabled is auto_restock_enabled (default false): off = App Store
	// Connect is never called and the pool behaves exactly as before.
	Enabled bool
	// Below is restock_below (300): restock when usable stock is under it, or
	// when what stays usable once the earliest batch dies would be.
	Below int
	// BatchSize is restock_batch_size (1,000; 1..25,000 in the table). For
	// PRODUCTION codes it is clamped to Apple's 500..25,000.
	BatchSize int
	// DailyMax is restock_daily_max (2): restocks started per UTC day.
	DailyMax int
	// LeadDays is restock_lead_days (45): the earliest usable batch expiring
	// within this many days counts as running out (restock early).
	LeadDays int
	// Environment is restock_environment: PRODUCTION / SANDBOX, "" when null
	// (the migration leaves it null; each environment fills its own in).
	Environment string
}

// Restock states (code_restocks.state, migration 021).
const (
	// RestockCreating: asking Apple for a batch — or the answer was lost
	// and the batch list is being reconciled.
	RestockCreating = "creating"
	// RestockCreated: Apple named the batch (asc_batch_id); its codes are
	// not in code_pool yet.
	RestockCreated = "created"
	// RestockImported: the batch is in code_pool.
	RestockImported = "imported"
	// RestockFailed: given up (error says why).
	RestockFailed = "failed"
)

// Restock is one code_restocks row.
type Restock struct {
	ID          int64
	Environment string
	Codes       int       // number_of_codes asked for
	ExpiresAt   time.Time // the expiration date, 00:00 UTC
	BatchID     string    // asc_batch_id; "" until created
	State       string
	StartedAt   time.Time
}

// RestockStatus is what one pass decides from: the settings, the unfinished
// restock (creating / created; at most one, the partial unique index says
// so), how many restocks started since the UTC day began, and the last start.
type RestockStatus struct {
	Settings     RestockSettings
	Open         *Restock
	StartedToday int
	LastStarted  time.Time // zero when none ever
}

// RestockStart is a new restock as BeginRestock records it, with the limits
// it re-checks atomically.
type RestockStart struct {
	Environment string
	Codes       int
	ExpiresAt   time.Time
	StartedAt   time.Time
	DayStart    time.Time     // 00:00 UTC of StartedAt's day
	DailyMax    int           // at most this many started since DayStart
	MinGap      time.Duration // and none started within this of StartedAt
}

// Restock store refusals.
var (
	// ErrRestockNotMigrated: the auto-restock columns / table are not there
	// yet (021 not applied). The pass treats auto-restock as off.
	ErrRestockNotMigrated = errors.New("referral restock: migration 021 not applied")
	// errRestockOpen: another restock is unfinished (creating / created).
	errRestockOpen = errors.New("referral restock: another restock is unfinished")
	// errRestockLimited: the daily max or the minimum gap, re-checked at
	// insert time, said no (another process started one meanwhile).
	errRestockLimited = errors.New("referral restock: daily max or minimum gap reached")
	// errRestockNotCreated: the row is no longer created (another process
	// finished or failed it).
	errRestockNotCreated = errors.New("referral restock: restock is no longer created")
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
	// time they ask, and notes installID among the devices this user has
	// used (gate 2: a friend claiming this user's code on one of them gets
	// own_code). CanClaim here reflects the database only (never claimed);
	// the RevenueCat half is applied by the handler.
	Me(ctx context.Context, userID, installID string) (Standing, error)
	// ClaimCheck runs judgement steps 2–4 without writing: unknown_code /
	// already_claimed / own_code (their own code, or installID is a device
	// the code's owner has used) as a Failure, else ClaimNew or ClaimAgain.
	ClaimCheck(ctx context.Context, userID, code, installID string) (ClaimKind, error)
	// PendingOffer is the code this user claimed as a friend and has not
	// redeemed (referral still claimed), swapped first when expired or within
	// swap_grace_days the way a re-claim would be — except a code whose
	// redemption the app has reported is kept until it actually expires. nil
	// when there is none or none can be handed out. Only ever the caller's own.
	PendingOffer(ctx context.Context, userID, installID string) (*PendingOffer, error)
	// OfferVariant is the experiment group of this user's referral as a
	// friend (023, invite-friends.md ### 兑换码分组实验): "month" or "year".
	// "month" when the referral predates the experiment, there is none, or
	// the column does not exist yet.
	OfferVariant(ctx context.Context, userID string) (string, error)
	// Claim re-checks 2–4 atomically, records referrer→referee (with the
	// claiming device) and hands back an Apple one-time code from the pool;
	// a re-claim gets the code it already has (or a swap; never once the
	// referral has left claimed). gate, when not nil, is run inside the
	// transaction under the global device lock (see ClaimGate). code is
	// already normalised.
	Claim(ctx context.Context, userID, code, installID string, gate *ClaimGate) (offerCode string, err error)
	// ReportRedeemed notes that the app says offerCode was redeemed by userID.
	// A hint for the settlement loop, never a proof. Each report restamps the
	// time (while the referral is claimed), which puts the row at the front
	// of the next ListClaimed if it has been looked at since.
	ReportRedeemed(ctx context.Context, userID, offerCode string) error
	// DeleteUser removes the user's invite code, every referral they are on
	// either side of, and their reward ledger (account deletion).
	DeleteUser(ctx context.Context, userID string) error

	// --- settlement (settle.go) ---

	// ExpireCodes marks free/issued pool codes past their expiry.
	ExpireCodes(ctx context.Context) (int64, error)
	// PoolStatus takes stock of code_pool (settings + per-batch counts) for
	// the low-stock / expiry watch.
	PoolStatus(ctx context.Context) (PoolStatus, error)
	// HoldDays reads referral_settings.hold_days.
	HoldDays(ctx context.Context) (int, error)
	// ListClaimed returns referrals still waiting for the referee to redeem,
	// in rotation order: first those reported /redeemed since their last
	// look, then never looked at, then looked at longest ago — so a backlog
	// bigger than one batch rotates instead of the same rows coming back
	// every pass.
	ListClaimed(ctx context.Context, limit int) ([]Referral, error)
	// MarkClaimedChecked records that RevenueCat answered for this claimed
	// referral at `at` (last_checked_at, migration 017) — ListClaimed's
	// rotation key. A referral no longer claimed is left alone.
	MarkClaimedChecked(ctx context.Context, id int64, at time.Time) error
	// MarkRedeemed moves a claimed referral to redeemed at the subscription's
	// real start time (from RevenueCat) and records subscriptionKey (gate 3,
	// Subscriber.SubscriptionKey). When another live referral — or a deleted
	// account's — already has that key, the referral is voided instead
	// (duplicate_apple_id, its rewards money with it) and voided is true.
	MarkRedeemed(ctx context.Context, id int64, at time.Time, subscriptionKey string) (voided bool, err error)
	// ListDue returns redeemed referrals whose hold has elapsed.
	ListDue(ctx context.Context, hold time.Duration, limit int) ([]Referral, error)
	// MarkVoid retires a referral (refund / no subscription).
	MarkVoid(ctx context.Context, id int64, reason string) error
	// MarkQualified moves a redeemed referral to qualified — or to held when
	// the referrer already has velocity_limit successes (held included)
	// inside velocity_window_hours (gate 4); held is true then. The window is
	// on theoretical success time (redeemed_at + hold_days, 017), not on when
	// settlement gets round to it, so a backlog judged in one pass after an
	// outage is not held wholesale.
	MarkQualified(ctx context.Context, id int64) (held bool, err error)
	// HeldDigests returns, per referrer with held referrals not yet mailed
	// and no review mail in the last 24 hours, what the operator mail needs.
	HeldDigests(ctx context.Context, limit int) ([]HeldDigest, error)
	// MarkHeldNotified records that the review mail for these held
	// referrals went out.
	MarkHeldNotified(ctx context.Context, ids []int64) error
	// ListQualified returns qualified referrals not yet rewarded or saved.
	ListQualified(ctx context.Context, limit int) ([]Referral, error)
	// MarkSaved parks a qualified referral until the referrer's paid plan
	// ends, with no last look yet (so ListSavers takes its referrer early).
	MarkSaved(ctx context.Context, id int64) error
	// Ledger reads a referrer's reward account (zero value when none).
	Ledger(ctx context.Context, userID string) (Ledger, error)
	// ListSavers returns referrers who have at least one saved referral, in
	// rotation order: a referrer with a saved referral never looked at
	// first, then the one looked at longest ago.
	ListSavers(ctx context.Context, limit int) ([]string, error)
	// MarkSaverChecked records that RevenueCat answered for this referrer at
	// `at` and they are still covered: every saved referral of theirs gets
	// last_checked_at = at (ListSavers' rotation key).
	MarkSaverChecked(ctx context.Context, userID string, at time.Time) error
	// SavedIDs returns the saved referral ids of one referrer.
	SavedIDs(ctx context.Context, userID string) ([]int64, error)
	// RecordGranted marks ids rewarded and advances the ledger to until,
	// adding len(ids) months, in one transaction.
	RecordGranted(ctx context.Context, userID string, ids []int64, until time.Time) error

	// --- code-pool auto-restock (restock.go, migration 021) ---

	// RestockStatus reads the auto-restock settings, the unfinished restock
	// and the start counts (dayStart = 00:00 UTC today).
	// ErrRestockNotMigrated when 021 is missing.
	RestockStatus(ctx context.Context, dayStart time.Time) (RestockStatus, error)
	// BeginRestock records a new restock as creating and returns its id —
	// only if no other is unfinished (errRestockOpen; the partial unique
	// index decides between two processes) and the daily max / minimum gap
	// still allow it (errRestockLimited).
	BeginRestock(ctx context.Context, r RestockStart) (int64, error)
	// RestockBatchesTaken reports which of these App Store Connect batch ids
	// some code_restocks row already holds.
	RestockBatchesTaken(ctx context.Context, batchIDs []string) (map[string]bool, error)
	// MarkRestockCreated moves a creating restock to created with Apple's
	// batch id; false when it was no longer creating.
	MarkRestockCreated(ctx context.Context, id int64, batchID string) (bool, error)
	// ImportRestock puts the batch's codes into code_pool (codes already
	// there are ignored) and moves the restock to imported with the number
	// actually inserted, in one transaction. errRestockNotCreated when the
	// row is no longer created (nothing is inserted then).
	ImportRestock(ctx context.Context, id int64, codes []string, offerRefName string, expiresAt, at time.Time) (inserted int, err error)
	// FailRestock moves an unfinished restock to failed with the reason.
	FailRestock(ctx context.Context, id int64, reason string, at time.Time) error
}
