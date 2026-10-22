// Package rewards is "earn 30% of what your friends pay, as Amazon.com gift
// cards" (yardmate-swiftui docs: main-navigation/more/invite-friends/rewards/
// rewards.md), on top of the referral package's month-for-a-month.
//
//   - POST /v1/revenuecat/webhook: RevenueCat tells us about every charge. A
//     real charge (INITIAL_PURCHASE / RENEWAL, NORMAL or INTRO, price > 0,
//     PRODUCTION) to someone who is a redeemed referee becomes a pending
//     credit of round(price × takehome × 0.30 × 100) cents for their referrer;
//     an Apple refund inside the window voids it (webhook.go).
//   - A 10-minute loop turns credits past purchased_at + 90 days into
//     available (this file).
//   - GET /v1/referral/me carries a "rewards" block (Rewards, injected into the
//     referral package as its RewardsReader).
//   - POST /v1/referral/payout: from $20 of available balance → one gift-card
//     request for the largest whole $10 up to $500 (PayoutAmountFor), mailed
//     to the operator, sent by hand (payout.go, deploy/referral-payout.sh).
//
// Money is integer cents end to end; the one multiplication is done on exact
// decimals (math/big), never float64. Nothing here logs a user id, an email
// address or a transaction id.
package rewards

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yaochen1125/yardmate-api/ratelimit"
)

const (
	// MinPayoutCents is the redeemable balance a request needs: $20
	// (rewards.md ### 兑换金额). The App keeps the same three numbers.
	MinPayoutCents = 2000
	// PayoutStepCents: every card is a whole multiple of $10; the change
	// stays in the balance.
	PayoutStepCents = 1000
	// MaxPayoutCents caps one card at $500; the rest waits for the next one.
	MaxPayoutCents = 50000
	// DefaultW9ThresholdCents is the 1099 filing threshold used when
	// referral_settings.w9_threshold_cents (migration 019) cannot be read:
	// $2,000 for payments from 2026 (One Big Beautiful Bill Act; indexed for
	// inflation from 2027 — the operator updates the row each January).
	DefaultW9ThresholdCents = 200000
	// W9MarginCents: the operator mail says W-9 FIRST from threshold − $100
	// in one calendar year (sent + requested, this request included). A fixed
	// margin, not "one card": cards are $20–$500 now, and the card that
	// crosses the threshold is always at or past this line
	// (rewards.md ### 税务：W-9).
	W9MarginCents = 10000
	// RefundWindow is how long a credit stays pending: Apple's refund window.
	RefundWindow = 90 * 24 * time.Hour

	defaultInterval     = 10 * time.Minute
	defaultStartupDelay = time.Minute
	matureTimeout       = 2 * time.Minute
	storeTimeout        = 10 * time.Second

	// The webhook's own per-IP ceiling (WebhookLimiter). It authenticates
	// with a shared secret, so it stays out of the /v1 600 / h per-IP group —
	// a burst of renewals (RevenueCat posts from a few IPs) or a backlog it
	// redelivers must not be 429'd into hours of retry backoff — but it is
	// still a public URL, so an unauthenticated flood from one IP is capped.
	webhookIPLimit  = 600
	webhookIPWindow = time.Minute
)

// Authenticator verifies the request's bearer token and returns the Supabase
// auth user id. Same verifier the referral endpoints use (main wires it).
type Authenticator func(r *http.Request) (userID string, err error)

// ChargeVerdict is a Redeemer's answer for a charge that beat settlement.
type ChargeVerdict int

const (
	ChargeRedeemed   ChargeVerdict = iota // the referral is redeemed now: credit the charge
	ChargeNotCounted                      // the charged subscription began before the claim: never credited
	ChargeVoided                          // settlement ① voided the referral (gate 3)
)

// Redeemer runs referral settlement step ① for one claimed referral on the
// spot: a RENEWAL charged while the referral is still claimed — the friend
// upgraded out of the free month before the 10-minute loop redeemed them
// (rewards.md ### 记账来源). Satisfied, through main, by
// referral.Service.RedeemForCharge; an error means "not now" and the webhook
// answers 503 so RevenueCat redelivers. This package does not import
// referral (SPEC §1).
type Redeemer interface {
	RedeemForCharge(ctx context.Context, referralID int64, referee string, claimedAt time.Time, productID string) (ChargeVerdict, error)
}

// Mailer sends the operator notice for a new payout request. Satisfied by
// *enrichment.FeedbackMailer (same account as /v1/feedback).
type Mailer interface {
	Alert(subject, body string) error
}

// Options tunes the service; zero values take the defaults.
type Options struct {
	// WebhookSecret is REVENUECAT_WEBHOOK_SECRET: what RevenueCat puts in the
	// Authorization header. Empty → the webhook answers 503.
	WebhookSecret string
	// Mailer receives payout requests; nil → logged only (the request is
	// still stored and listed by deploy/referral-payout.sh).
	Mailer Mailer
	// DeviceBucket is /claim's per-device bucket (referral.Service.ClaimBucket),
	// shared so /payout spends from the same 5 / 24 h. nil → a private bucket
	// with the same policy.
	DeviceBucket *ratelimit.Bucket
	// AllowSandbox (REVENUECAT_WEBHOOK_ALLOW_SANDBOX, staging only) makes the
	// webhook book SANDBOX events like PRODUCTION ones, so a sandbox purchase
	// on staging can be followed end to end. Never on prod.
	AllowSandbox bool
	Interval     time.Duration // maturing cadence (default 10 min)
	StartupDelay time.Duration // before the first maturing pass (default 1 min)
}

// Service owns the webhook, the payout endpoint, the /me block and the
// maturing loop.
type Service struct {
	store  Store
	auth   Authenticator
	secret string
	// allowSandbox: Options.AllowSandbox.
	allowSandbox bool
	mailer       Mailer
	bucket       *ratelimit.Bucket
	// webhookIP is the webhook's per-IP bucket (WebhookLimiter), swept by
	// the maturing pass.
	webhookIP *ratelimit.Bucket
	// redeemer settles a claimed referral whose RENEWAL beat settlement
	// (SetRedeemer); nil → such a RENEWAL answers 503 until the loop has
	// redeemed it.
	redeemer Redeemer
	// productChanges counts referees' PRODUCT_CHANGE events since start, for
	// the reconciliation log line.
	productChanges atomic.Int64

	interval     time.Duration
	startupDelay time.Duration
	now          func() time.Time
	// async runs the operator mail off the request path; tests make it
	// synchronous.
	async func(func())
}

// New wires a Service. Pass nil for whatever is not configured.
func New(store Store, auth Authenticator, opt Options) *Service {
	if opt.Interval <= 0 {
		opt.Interval = defaultInterval
	}
	if opt.StartupDelay <= 0 {
		opt.StartupDelay = defaultStartupDelay
	}
	if opt.DeviceBucket == nil {
		opt.DeviceBucket = ratelimit.NewBucket(5, 24*time.Hour)
	}
	return &Service{
		store:        store,
		auth:         auth,
		secret:       strings.TrimSpace(opt.WebhookSecret),
		allowSandbox: opt.AllowSandbox,
		mailer:       opt.Mailer,
		bucket:       opt.DeviceBucket,
		webhookIP:    ratelimit.NewBucket(webhookIPLimit, webhookIPWindow),
		interval:     opt.Interval,
		startupDelay: opt.StartupDelay,
		now:          time.Now,
		async:        func(f func()) { go f() },
	}
}

// WebhookLimiter is the per-IP middleware for POST /v1/revenuecat/webhook:
// 600 / min, its own bucket instead of the /v1 group's 600 / h (see
// webhookIPLimit). 429 rate_limit_ip, which RevenueCat retries.
func (s *Service) WebhookLimiter() func(http.Handler) http.Handler {
	return ratelimit.PerIPMiddleware(s.webhookIP, "rate_limit_ip")
}

// Missing names the absent dependencies, for the startup log.
func (s *Service) Missing() []string {
	var m []string
	if s.store == nil {
		m = append(m, "SUPABASE_DB_URL")
	}
	if s.auth == nil {
		m = append(m, "SUPABASE_URL")
	}
	if s.secret == "" {
		m = append(m, "REVENUECAT_WEBHOOK_SECRET")
	}
	return m
}

// Summary is the "rewards" block of GET /v1/referral/me. Field names are the
// wire contract with the iOS app (rewards.md ## 数据契约).
type Summary struct {
	AvailableCents int64       `json:"available_cents"`
	PendingCents   int64       `json:"pending_cents"`
	LifetimeCents  int64       `json:"lifetime_cents"`
	Payout         *PayoutView `json:"payout"` // null when the user has never asked
	// Counts and Referrals are the card's progress (progress.go, added
	// 2026-10-21). Left out when reading them failed — the balance above is
	// still served — so old apps and a broken progress query see the block
	// exactly as before.
	Counts    *Counts        `json:"counts,omitempty"`
	Referrals []ReferralView `json:"referrals,omitempty"`
}

// PayoutView is a payout on the wire. Times are RFC 3339, UTC, whole seconds.
type PayoutView struct {
	ID          int64   `json:"id"`
	AmountCents int64   `json:"amount_cents"`
	State       string  `json:"state"`
	RequestedAt string  `json:"requested_at"`
	SentAt      *string `json:"sent_at"`
	// Email and EmailEditable (2026-10-22): the card's "To {email}" line
	// and whether its Change shows. The address is the caller's own.
	Email         string `json:"email"`
	EmailEditable bool   `json:"email_editable"`
}

func viewOf(p *Payout) *PayoutView {
	if p == nil {
		return nil
	}
	v := &PayoutView{
		ID:            p.ID,
		AmountCents:   p.AmountCents,
		State:         p.State,
		RequestedAt:   rfc3339(p.RequestedAt),
		Email:         p.Email,
		EmailEditable: p.EmailEditable,
	}
	if !p.SentAt.IsZero() {
		sent := rfc3339(p.SentAt)
		v.SentAt = &sent
	}
	return v
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// PayoutAmountFor is the card a redeemable balance buys right now
// (rewards.md ### 兑换金额): the largest whole $10 up to $500, or 0 under $20.
// $51.30 → $50, $480 → $480, $3,000 → $500; the change and anything over
// $500 stay in the balance. No choice for the user.
func PayoutAmountFor(redeemableCents int64) int64 {
	if redeemableCents < MinPayoutCents {
		return 0
	}
	amount := redeemableCents / PayoutStepCents * PayoutStepCents
	if amount > MaxPayoutCents {
		amount = MaxPayoutCents
	}
	return amount
}

// Summarize turns the store's sums into the wire block: redeemable =
// available ledger − (requested + sent payouts), never below zero; lifetime =
// everything that ever became available (redeemed or not). A cancelled payout
// is in neither sum, so its amount is back in the balance without a write.
func Summarize(b Balance) Summary {
	avail := b.AvailableLedgerCents - b.SpentCents
	if avail < 0 {
		avail = 0
	}
	return Summary{
		AvailableCents: avail,
		PendingCents:   b.PendingCents,
		LifetimeCents:  b.AvailableLedgerCents,
		Payout:         viewOf(b.Latest),
	}
}

// Rewards is the /me hook (referral.RewardsReader): the caller's block, or an
// error the referral handler logs before leaving the field out. A failure
// reading the progress only drops counts / referrals (logged, no user id).
func (s *Service) Rewards(ctx context.Context, userID string) (any, error) {
	b, err := s.store.Balance(ctx, userID)
	if err != nil {
		return nil, err
	}
	sum := Summarize(b)
	refs, credits, err := s.store.Progress(ctx, userID)
	if err != nil {
		log.Printf("referral rewards progress: %v", err)
		return sum, nil
	}
	counts, list := Progress(refs, credits, s.now())
	sum.Counts = &counts
	sum.Referrals = list
	return sum, nil
}

// CanRead reports whether the /me hook can be injected (a store exists).
func (s *Service) CanRead() bool { return s != nil && s.store != nil }

// SetRedeemer wires referral settlement into the webhook (main does it once
// at startup, before serving).
func (s *Service) SetRedeemer(r Redeemer) { s.redeemer = r }

// Start launches the maturing loop until ctx ends. A no-op (logged) without a
// store.
func (s *Service) Start(ctx context.Context) {
	if !s.CanRead() {
		log.Printf("referral rewards mature: disabled (no SUPABASE_DB_URL)")
		return
	}
	log.Printf("referral rewards mature: enabled interval=%s", s.interval)
	go s.loop(ctx)
}

func (s *Service) loop(ctx context.Context) {
	first := time.NewTimer(s.startupDelay)
	defer first.Stop()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			s.Mature(ctx)
		case <-ticker.C:
			s.Mature(ctx)
		}
	}
}

// Mature runs one pass: pending credits past available_at become available.
// Exported for tests; the loop calls it on its timer. The per-device bucket is
// the referral package's (swept by its settlement pass) or our own private one
// (swept here); the webhook's per-IP bucket is always ours.
func (s *Service) Mature(ctx context.Context) int64 {
	// Sweep first, so a database outage does not stop the buckets shrinking.
	s.bucket.Sweep(s.now())
	s.webhookIP.Sweep(s.now())
	mctx, cancel := context.WithTimeout(ctx, matureTimeout)
	defer cancel()
	n, err := s.store.MatureDue(mctx)
	if err != nil {
		log.Printf("referral rewards mature: %v", err)
		return 0
	}
	if n > 0 {
		log.Printf("referral rewards mature: available=%d", n)
	}
	return n
}
