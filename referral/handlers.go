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
	// claimBodyCap: /claim also carries the DeviceCheck token (a few KB of
	// base64).
	claimBodyCap = 16 << 10
	// maxDeviceToken is past any DCDevice token; longer is not one.
	maxDeviceToken = 12 << 10
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
	// Alerter receives the code-pool low-stock / expiry email (pool.go) and
	// the gate-4 review email (review.go); nil = WARN log only.
	Alerter Alerter
	// DeviceCheck is Apple's DeviceCheck (gate 1); nil = not configured,
	// which in enforce mode makes every new claim device_check_unavailable.
	DeviceCheck DeviceChecker
	// DeviceCheckMode is DEVICECHECK_MODE; "" = enforce.
	DeviceCheckMode DeviceCheckMode
	// DeviceCheckAllowDevelopment is DEVICECHECK_ALLOW_DEVELOPMENT (staging
	// only, never prod): enforce also accepts device_check_env =
	// "development" and asks Apple's development host, so gate 1 can be
	// tested in enforce with a DEBUG build. Log mode ignores it.
	DeviceCheckAllowDevelopment bool
	// DeviceCheckTimeout bounds each Apple call (default 3 s).
	DeviceCheckTimeout time.Duration
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

	// gate 1 (devicecheck.go): Apple's client (nil = not configured), the
	// mode, and the per-call bound.
	dc         DeviceChecker
	dcMode     DeviceCheckMode
	dcTimeout  time.Duration
	dcAllowDev bool // DEVICECHECK_ALLOW_DEVELOPMENT

	interval     time.Duration
	startupDelay time.Duration
	nudge        chan struct{}
	now          func() time.Time

	// pool watch (pool.go): where the alert goes, and which condition sets
	// have already been mailed today (alertDay, UTC) so a day sends at most
	// one email per set even when the conditions flap. Only the settlement
	// goroutine touches these.
	alerter    Alerter
	alertDay   string
	alertsSent map[string]bool
	// poolConfigWarned: the settings-problem line (pool.go) has been logged
	// for the current misconfiguration; cleared once it is fixed.
	poolConfigWarned bool
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
	if opt.DeviceCheckMode == "" {
		opt.DeviceCheckMode = DeviceCheckEnforce
	}
	if opt.DeviceCheckTimeout <= 0 {
		opt.DeviceCheckTimeout = DefaultDeviceCheckTimeout
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
		alerter:      opt.Alerter,
		dc:           opt.DeviceCheck,
		dcMode:       opt.DeviceCheckMode,
		dcTimeout:    opt.DeviceCheckTimeout,
		dcAllowDev:   opt.DeviceCheckAllowDevelopment,
	}
}

// DeviceCheck reports gate 1's mode and whether Apple's client is wired, for
// the startup log.
func (s *Service) DeviceCheck() (mode DeviceCheckMode, configured bool) {
	return s.dcMode, s.dc != nil
}

// DeviceCheckAllowsDevelopment reports DEVICECHECK_ALLOW_DEVELOPMENT.
func (s *Service) DeviceCheckAllowsDevelopment() bool { return s.dcAllowDev }

// enforceAccepts is enforce's test of a request's device_check_env:
// "production", or "development" when DEVICECHECK_ALLOW_DEVELOPMENT is on.
func (s *Service) enforceAccepts(env string) bool {
	return isProductionEnv(env) || (s.dcAllowDev && isDevelopmentEnv(env))
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
		// The install id goes in with every /me: gate 2 needs the devices a
		// referrer has used, so a friend claiming on one of them is refused.
		standing, err := s.store.Me(ctx, userID, r.Header.Get("X-Device-Install-Id"))
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
	// DeviceToken is DCDevice.generateToken() base64 (gate 1). Absent on the
	// simulator or when the app could not get one.
	DeviceToken string `json:"device_token"`
	// DeviceCheckEnv is "development" (DEBUG build) or "production"
	// (TestFlight / App Store): which Apple host, whose bits are kept apart.
	// enforce only accepts "production" (see enforceAccepts).
	DeviceCheckEnv string `json:"device_check_env"`
}

// HandleClaim is POST /v1/referral/claim: a friend's code goes in, an App
// Store one-time code comes back.
//
// Judgement order (invite-friends.md /claim 判定顺序, first hit answers):
//
//  1. token 401, request shape 400, the day's per-device try 429
//  2. unknown_code
//  3. this account claimed before: same code → the code it has (no
//     RevenueCat, no DeviceCheck); another code → already_claimed
//  4. own_code: their own code, or this device is one the owner has used
//  5. not_new_user: the app says so, or RevenueCat has a store subscription
//  6. DeviceCheck: no / rejected token, or (enforce) device_check_env not
//     "production" → device_unverified (enforce only); bit0 set →
//     device_used; Apple unusable → device_check_unavailable
//  7. a pool code: none → pool_empty (bit0 untouched); taken → bit0 set in
//     the same transaction, a failed set rolls the claim back →
//     device_check_unavailable
//
// 2–4 are a read-only pre-check (ClaimCheck) and are checked again,
// atomically, by the claim itself. The Apple half of 6 and all of 7 run
// inside the claim's transaction under one global lock (ClaimGate), so two
// accounts claiming on one device at once cannot both see bit0 clear.
func (s *Service) HandleClaim() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.caller(w, r)
		if !ok {
			return
		}
		var req claimRequest
		if !decodeCapped(w, r, &req, claimBodyCap) {
			return
		}
		code, okCode := Normalise(req.Code)
		if !okCode {
			writeError(w, http.StatusBadRequest, "bad_code")
			return
		}
		token := strings.TrimSpace(req.DeviceToken)
		if len(token) > maxDeviceToken || !printable(token) {
			token = "" // not a DeviceCheck token; treated as none
		}
		env := deviceEnv(req.DeviceCheckEnv)
		if s.dcMode != DeviceCheckLog && !s.enforceAccepts(req.DeviceCheckEnv) {
			// enforce: only a production token counts (plus development ones
			// on a staging server with DEVICECHECK_ALLOW_DEVELOPMENT). Anything
			// else is "no token" → device_unverified, and Apple's development
			// host (whose bits are separate) is never asked.
			token = ""
		}
		installID := r.Header.Get("X-Device-Install-Id")
		if allowed, retry := s.claimBucket.Allow(installID, s.now()); !allowed {
			ratelimit.Write429(w, retry, "rate_limit_device")
			return
		}

		// 2–4
		ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
		kind, err := s.store.ClaimCheck(ctx, userID, code, installID)
		cancel()
		if err != nil {
			fail(w, err, "claim check")
			return
		}
		if kind == ClaimAgain {
			s.finishClaim(w, r.Context(), userID, code, installID, nil)
			return
		}

		// 5
		if req.SubscribedBefore {
			writeError(w, http.StatusBadRequest, ErrNotNewUser)
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

		// 6 + 7: what needs no Apple (no client, no token) is decided here;
		// asking Apple, taking the code and setting bit0 happen inside the
		// claim's transaction under the device lock (ClaimGate).
		//
		// From here on Apple may be told to set bit0, which nothing undoes.
		// The client hanging up must not cancel the read-back or the commit
		// that go with it: the claim would roll back with the device already
		// marked used, and every retry would read device_used. So the gated
		// claim runs detached from the request, bounded by finishClaim's own
		// timeout; a claim the client never hears about is re-handed on the
		// next try (ClaimAgain).
		claimCtx := context.WithoutCancel(r.Context())
		gate, refuse, status := s.deviceGate(claimCtx, env, token)
		if refuse != "" {
			writeError(w, status, refuse)
			return
		}
		s.finishClaim(w, claimCtx, userID, code, installID, gate)
	}
}

// finishClaim takes (or re-hands) the pool code and answers. A gated claim
// gets room for the lock wait and up to three Apple calls (query, set, the
// re-read after a failed set) on top of the database's own budget, so the
// transaction never runs out under an Apple call that has already been
// sent.
func (s *Service) finishClaim(w http.ResponseWriter, parent context.Context, userID, code, installID string, gate *ClaimGate) {
	budget := storeTimeout
	if gate != nil {
		budget += deviceLockWait + 3*s.dcTimeout
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	offer, err := s.store.Claim(ctx, userID, code, installID, gate)
	if err != nil {
		fail(w, err, "claim")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"offer_code": offer})
}

// deviceGate is step 6 up to the point where Apple has to be asked. No
// client (enforce: 503) or no token (enforce: device_unverified) is decided
// here; otherwise it returns the gate Store.Claim runs under the device
// lock. A nil gate and no refusal = log mode letting the claim through
// unchecked. Log lines carry the verdict only — never the token.
func (s *Service) deviceGate(parent context.Context, env, token string) (gate *ClaimGate, refuse string, status int) {
	enforce := s.dcMode != DeviceCheckLog
	if s.dc == nil {
		if enforce {
			log.Printf("referral claim: devicecheck not configured (DEVICECHECK_KEY_ID / DEVICECHECK_PRIVATE_KEY / APPLE_TEAM_ID); refusing in enforce mode")
			return nil, ErrDeviceUnavailable, http.StatusServiceUnavailable
		}
		logPass(env, "devicecheck not configured")
		return nil, "", 0
	}
	if token == "" {
		if enforce {
			return nil, ErrDeviceUnverified, http.StatusBadRequest
		}
		logPass(env, "no device_token (would be device_unverified)")
		return nil, "", 0
	}
	run := &deviceRun{s: s, parent: parent, env: env, token: token}
	return &ClaimGate{Check: run.check, Set: run.set}, "", 0
}

func logPass(env, why string) {
	log.Printf("referral claim: devicecheck log mode, not refusing: %s (env=%s)", why, env)
}

// deviceRun is gate 1's Apple half for one new claim. Both calls run inside
// the claim transaction, under the device lock. In log mode nothing is
// refused, but Apple is asked and told exactly as in enforce, so staging
// shows what prod would do.
type deviceRun struct {
	s          *Service
	parent     context.Context
	env, token string
	setBit     bool // check decided bit0 is to be set once a code is taken
}

// check queries bit0: device_used / device_unverified /
// device_check_unavailable refusals in enforce (the claim rolls back, the
// code stays free); log mode only logs.
func (d *deviceRun) check() error {
	enforce := d.s.dcMode != DeviceCheckLog
	ctx, cancel := context.WithTimeout(d.parent, d.s.dcTimeout)
	used, err := d.s.dc.QueryBit0(ctx, d.env, d.token)
	cancel()
	switch {
	case errors.Is(err, ErrDeviceTokenInvalid):
		if enforce {
			return Failure{Code: ErrDeviceUnverified}
		}
		logPass(d.env, "apple rejected the device token (would be device_unverified)")
		return nil
	case err != nil:
		log.Printf("referral claim: devicecheck query failed: %v", err)
		if enforce {
			return Failure{Code: ErrDeviceUnavailable}
		}
		logPass(d.env, "apple query failed (would be device_check_unavailable)")
	case used:
		if enforce {
			return Failure{Code: ErrDeviceUsed}
		}
		logPass(d.env, "bit0 already set (would be device_used)")
	}
	d.setBit = true
	return nil
}

// set sets bit0 for a FRESH claim — "与从码池取码同进退". When Apple's answer
// to the set is an error (a timeout, a 5xx) the bit may still have been set
// with only the answer lost; rolling back then would leave a real friend
// with a code-less device that reads device_used forever. So bit0 is read
// back once: set → the claim stands; clear, or the read failing too → in
// enforce the claim rolls back (device_check_unavailable); log mode only
// logs.
func (d *deviceRun) set(fresh bool) error {
	if !fresh || !d.setBit {
		return nil // a re-claim (or lost a race to this account's other device: that claim set it)
	}
	ctx, cancel := context.WithTimeout(d.parent, d.s.dcTimeout)
	err := d.s.dc.SetBit0(ctx, d.env, d.token)
	cancel()
	if err == nil {
		return nil
	}
	if d.s.dcMode == DeviceCheckLog {
		log.Printf("referral claim: devicecheck log mode, set bit0 failed (claim kept): %v", err)
		return nil
	}
	ctx, cancel = context.WithTimeout(d.parent, d.s.dcTimeout)
	set, qerr := d.s.dc.QueryBit0(ctx, d.env, d.token)
	cancel()
	if qerr == nil && set {
		log.Printf("referral claim: devicecheck set bit0 answered an error but bit0 reads back set; claim kept: %v", err)
		return nil
	}
	log.Printf("referral claim: devicecheck set bit0 failed, claim rolled back: %v (read back: set=%v err=%v)", err, set, qerr)
	return Failure{Code: ErrDeviceUnavailable}
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
	return decodeCapped(w, r, dst, bodyCap)
}

func decodeCapped(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
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
		case ErrPoolEmpty, ErrDeviceUnavailable:
			// Not the user's fault and not permanent: the pool wants
			// restocking / Apple wants a moment.
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
