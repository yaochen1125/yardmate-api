// Package referral is the three endpoints behind "invite a friend, you both
// get a month of Premium" (yardmate-swiftui docs: main-navigation/more/
// invite-friends) plus the settlement loop that pays referrers.
//
// The shape is set by Apple's rules and one product decision:
//
//  1. A one-time offer code never shows up in the transaction, so the only way
//     to know who redeemed is to have handed that exact code to that exact
//     person and written it down — that is code_pool.
//  2. One Apple ID may redeem a given offer once, ever, so the referee's free
//     month is an App Store offer code but the referrer's reward is NOT: it is
//     a RevenueCat promotional entitlement, stacked 30 days at a time, with no
//     cap. A referrer who is currently paying (trial / intro / their own free
//     month included) has the month saved and granted after the paid plan ends,
//     because a promotional grant running alongside a store subscription buys
//     nothing.
//  3. Identity is PROVEN, not asserted: every endpoint verifies the Supabase
//     access token (same JWKS path as /v1/account/delete) and keys everything
//     on the `sub`. The RevenueCat app user id is that same id (uppercased —
//     see AppUserID).
//
// Nothing about who is ever logged: a referral row is two user ids, and
// neither belongs in a log line.
package referral

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/yaochen1125/yardmate-api/ratelimit"
)

const (
	// bodyCap: a code and nothing else. The rest is somebody trying something.
	bodyCap = 4 << 10
	// maxOfferCode is past any App Store Connect code; they run to 12.
	maxOfferCode = 64

	// Per-device policy (docs ## 数据契约): claim 5 / 24 h, me + redeemed 30 / h.
	DefaultClaimLimit  = 5
	DefaultClaimWindow = 24 * time.Hour
	DefaultReadLimit   = 30
	DefaultReadWindow  = time.Hour

	// RevenueCat lookups inside a request are bounded tighter than the client's
	// own 10 s so a slow upstream never pushes a request past nginx's patience.
	claimRCTimeout = 8 * time.Second
	meRCTimeout    = 5 * time.Second
	storeTimeout   = 10 * time.Second
)

// Authenticator verifies the request's bearer token and returns the Supabase
// auth user id (a lowercase UUID string). Injected by main from the same JWKS
// verifier /v1/account/delete uses, so this package never sees SUPABASE_URL.
type Authenticator func(r *http.Request) (userID string, err error)

// Options tunes the service; zero values take the defaults above.
type Options struct {
	ClaimLimit   int
	ClaimWindow  time.Duration
	ReadLimit    int
	ReadWindow   time.Duration
	Interval     time.Duration // settlement cadence (default 10 min)
	StartupDelay time.Duration // before the first settlement pass (default 1 min)
}

// Service owns the three handlers, their per-device buckets, and the
// settlement loop. Any nil dependency makes every endpoint answer
// 503 not_configured instead of half-working.
type Service struct {
	store       Store
	rc          Subscribers
	entitlement string
	auth        Authenticator
	// rewards fills /me's "rewards" block (rewards/ package, SetRewards);
	// nil leaves the field out.
	rewards RewardsReader

	claimBucket *ratelimit.Bucket
	readBucket  *ratelimit.Bucket

	interval     time.Duration
	startupDelay time.Duration
	nudge        chan struct{}
	now          func() time.Time
}

// New wires a Service. Pass nil for whatever is not configured.
func New(store Store, rc Subscribers, entitlement string, auth Authenticator, opt Options) *Service {
	if opt.ClaimLimit <= 0 {
		opt.ClaimLimit = DefaultClaimLimit
	}
	if opt.ClaimWindow <= 0 {
		opt.ClaimWindow = DefaultClaimWindow
	}
	if opt.ReadLimit <= 0 {
		opt.ReadLimit = DefaultReadLimit
	}
	if opt.ReadWindow <= 0 {
		opt.ReadWindow = DefaultReadWindow
	}
	if opt.Interval <= 0 {
		opt.Interval = defaultSettleInterval
	}
	if opt.StartupDelay <= 0 {
		opt.StartupDelay = defaultSettleStartupDelay
	}
	return &Service{
		store:        store,
		rc:           rc,
		entitlement:  strings.TrimSpace(entitlement),
		auth:         auth,
		claimBucket:  ratelimit.NewBucket(opt.ClaimLimit, opt.ClaimWindow),
		readBucket:   ratelimit.NewBucket(opt.ReadLimit, opt.ReadWindow),
		interval:     opt.Interval,
		startupDelay: opt.StartupDelay,
		nudge:        make(chan struct{}, 1),
		now:          time.Now,
	}
}

// Configured reports whether every dependency is present.
func (s *Service) Configured() bool {
	return s != nil && s.store != nil && s.rc != nil && s.entitlement != "" && s.auth != nil
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
	if s.rc == nil {
		m = append(m, "REVENUECAT_SECRET_KEY")
	}
	if s.entitlement == "" {
		m = append(m, "REVENUECAT_ENTITLEMENT_ID")
	}
	return m
}

// ReadLimiter is the per-device middleware for /me and /redeemed (30 / h).
// /claim's tighter bucket is consumed INSIDE its handler, after the cheap
// checks, so a malformed body does not spend one of the day's five tries.
func (s *Service) ReadLimiter() func(http.Handler) http.Handler {
	return ratelimit.PerDeviceMiddleware(s.readBucket, "rate_limit_device")
}

// DeleteUser removes everything keyed on userID (account deletion). A nil
// store (feature never configured) has nothing to delete.
func (s *Service) DeleteUser(ctx context.Context, userID string) error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.DeleteUser(ctx, userID)
}

// HandleMe is GET /v1/referral/me: the caller's own code (made on first ask)
// and where their invitations have got to.
func (s *Service) HandleMe() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.caller(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
		defer cancel()
		standing, err := s.store.Me(ctx, userID)
		if err != nil {
			fail(w, err, "me")
			return
		}
		// The database only knows whether they have claimed; "never subscribed"
		// is RevenueCat's to answer. Fail OPEN on an upstream hiccup: the entry
		// shows and the claim itself re-checks with a hard answer.
		if standing.CanClaim {
			rcCtx, rcCancel := context.WithTimeout(r.Context(), meRCTimeout)
			sub, err := s.rc.Subscriber(rcCtx, AppUserID(userID))
			rcCancel()
			switch {
			case err != nil:
				log.Printf("referral me: revenuecat lookup failed (can_claim left true): %v", err)
			case sub.HasPaidHistory():
				standing.CanClaim = false
			}
		}
		s.attachRewards(ctx, userID, &standing)
		writeJSON(w, http.StatusOK, standing)
	}
}

type claimRequest struct {
	Code string `json:"code"`
	// SubscribedBefore is the app having found a Premium subscription of this
	// Apple ID's own in its purchase history. Its word only — the server
	// re-checks RevenueCat, and the App Store offer (new subscribers only) is
	// the real gate. What this saves is a code off the shelf for nothing.
	SubscribedBefore bool `json:"subscribed_before"`
}

// HandleClaim is POST /v1/referral/claim: a friend's code goes in, an App
// Store one-time code comes back.
//
// Order: cheap shape checks → the day's per-device try → RevenueCat history
// → the one atomic database statement (code exists, not their own, not
// claimed before, stock left).
func (s *Service) HandleClaim() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.caller(w, r)
		if !ok {
			return
		}
		var req claimRequest
		if !decode(w, r, &req) {
			return
		}
		code, okCode := Normalise(req.Code)
		if !okCode {
			writeError(w, http.StatusBadRequest, "bad_code")
			return
		}
		if req.SubscribedBefore {
			writeError(w, http.StatusBadRequest, ErrNotNewUser)
			return
		}
		if allowed, retry := s.claimBucket.Allow(r.Header.Get("X-Device-Install-Id"), s.now()); !allowed {
			ratelimit.Write429(w, retry, "rate_limit_device")
			return
		}

		rcCtx, rcCancel := context.WithTimeout(r.Context(), claimRCTimeout)
		sub, err := s.rc.Subscriber(rcCtx, AppUserID(userID))
		rcCancel()
		if err != nil {
			log.Printf("referral claim: revenuecat lookup failed: %v", err)
			writeError(w, http.StatusBadGateway, "store_failed")
			return
		}
		if sub.HasPaidHistory() {
			writeError(w, http.StatusBadRequest, ErrNotNewUser)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
		defer cancel()
		offer, err := s.store.Claim(ctx, userID, code)
		if err != nil {
			fail(w, err, "claim")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"offer_code": offer})
	}
}

type redeemedRequest struct {
	OfferCode string `json:"offer_code"`
}

// HandleRedeemed is POST /v1/referral/redeemed: the app saying the code it
// was given has been redeemed. A hint that makes the settlement loop look at
// RevenueCat sooner — never a proof; RevenueCat is. Idempotent.
func (s *Service) HandleRedeemed() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.caller(w, r)
		if !ok {
			return
		}
		var req redeemedRequest
		if !decode(w, r, &req) {
			return
		}
		offer := strings.TrimSpace(req.OfferCode)
		if offer == "" || len(offer) > maxOfferCode || !printable(offer) {
			writeError(w, http.StatusBadRequest, "bad_code")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
		defer cancel()
		if err := s.store.ReportRedeemed(ctx, userID, offer); err != nil {
			fail(w, err, "redeemed")
			return
		}
		s.Nudge()
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// caller checks the two headers every endpoint needs, the configuration, and
// the bearer token, in that order. The install id is what the rate limiters
// key on; the verified user id is who the rows belong to. They are not the
// same thing: one phone can sign into two accounts, one account onto many
// phones.
func (s *Service) caller(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !isUUID(r.Header.Get("X-Device-Install-Id")) {
		writeError(w, http.StatusBadRequest, "missing_device_id")
		return "", false
	}
	if strings.TrimSpace(r.Header.Get("X-App-Version")) == "" {
		writeError(w, http.StatusBadRequest, "missing_app_version")
		return "", false
	}
	if !s.Configured() {
		writeError(w, http.StatusServiceUnavailable, "not_configured")
		return "", false
	}
	userID, err := s.auth(r)
	if err != nil {
		// Never echo the token or which check failed.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return userID, true
}

// decode reads a small JSON body. Unknown fields are tolerated so a newer app
// can send more than this build reads.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, bodyCap)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json")
		return false
	}
	return true
}

// fail turns whatever came back from the store into one answer. A refusal the
// schema named is the user's business and travels on with its own code; a
// database that is down is ours, and the app is told so in one word it can
// tell apart from a no.
func fail(w http.ResponseWriter, err error, where string) {
	if f, ok := named(err); ok {
		switch f.Code {
		case ErrPoolEmpty:
			// Not the user's fault and not permanent: the pool wants restocking.
			writeError(w, http.StatusServiceUnavailable, f.Code)
		default:
			writeError(w, http.StatusBadRequest, f.Code)
		}
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		log.Printf("referral %s: store timeout: %v", where, err)
	} else {
		log.Printf("referral %s: store error: %v", where, err)
	}
	writeError(w, http.StatusBadGateway, "store_failed")
}

func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Error: code})
}

// isUUID accepts RFC 4122 canonical form (36 chars, dashes at 8/13/18/23),
// either hex case. Duplicated from ratelimit/proxy (12 lines; not worth a
// package).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
