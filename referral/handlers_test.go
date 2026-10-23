package referral

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	device = "4E7B1B6C-7B9F-4F3E-9C2E-1D3A5F6B7C8D"
	userA  = "11111111-2222-3333-4444-555555555555"
	userB  = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	entID  = "YardMate: AI Plant ID & Care Pro"
)

func okAuth(*http.Request) (string, error)  { return userA, nil }
func badAuth(*http.Request) (string, error) { return "", errors.New("bad token") }

func newSvc(store Store, rc Subscribers, ent string, auth Authenticator) *Service {
	return New(store, rc, ent, auth, Options{})
}

func configured() (*Service, *memStore, *fakeRC) {
	svc, store, rc, _ := configuredDC()
	return svc, store, rc
}

// configuredDC is configured() plus the fake DeviceCheck it runs with
// (enforce mode, the prod setting).
func configuredDC() (*Service, *memStore, *fakeRC, *fakeDC) {
	store, rc, dc := newMemStore(), newFakeRC(), newFakeDC()
	store.meStanding = Standing{Code: "YM-7KQ2", Invited: 3, MonthsEarned: 2, MonthsPending: 1, MonthsSaved: 1, CanClaim: true}
	store.claimOffer = "XK7P2M9Q"
	return New(store, rc, entID, okAuth, Options{DeviceCheck: dc}), store, rc, dc
}

// claimBody is a /claim body from a real device (token "tok") in production.
func claimBody(code string) string {
	return `{"code":"` + code + `","subscribed_before":false,"device_token":"tok","device_check_env":"production"}`
}

func call(t *testing.T, h http.HandlerFunc, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/v1/referral", strings.NewReader(body))
	r.Header.Set("X-Device-Install-Id", device)
	r.Header.Set("X-App-Version", "2.2.0 (50)")
	r.Header.Set("Authorization", "Bearer x")
	for k, v := range headers {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func body(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not json: %q", w.Body.String())
	}
	return out
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	c, _ := body(t, w)["error"].(string)
	return c
}

type endpoint struct {
	name    string
	method  string
	body    string
	handler func(*Service) http.HandlerFunc
}

func endpoints() []endpoint {
	return []endpoint{
		{"me", "GET", "", (*Service).HandleMe},
		{"claim", "POST", claimBody("YM-7KQ2"), (*Service).HandleClaim},
		{"redeemed", "POST", `{"offer_code":"XK7P2M9Q"}`, (*Service).HandleRedeemed},
	}
}

// MARK: who is asking

func TestEveryEndpointNeedsTheTwoHeaders(t *testing.T) {
	svc, _, _ := configured()
	cases := []struct{ name, header, value, want string }{
		{"no install id", "X-Device-Install-Id", "", "missing_device_id"},
		{"install id not a uuid", "X-Device-Install-Id", "nope", "missing_device_id"},
		{"no app version", "X-App-Version", "", "missing_app_version"},
	}
	for _, c := range cases {
		for _, e := range endpoints() {
			w := call(t, e.handler(svc), e.method, e.body, map[string]string{c.header: c.value})
			if w.Code != 400 || errCode(t, w) != c.want {
				t.Errorf("%s / %s: %d %q, want 400 %s", c.name, e.name, w.Code, errCode(t, w), c.want)
			}
		}
	}
}

func TestEveryEndpointAnswersNotConfigured(t *testing.T) {
	store, rc := newMemStore(), newFakeRC()
	cases := []struct {
		name string
		svc  *Service
	}{
		{"no store", newSvc(nil, rc, entID, okAuth)},
		{"no revenuecat", newSvc(store, nil, entID, okAuth)},
		{"no entitlement", newSvc(store, rc, "   ", okAuth)},
		{"no auth", newSvc(store, rc, entID, nil)},
	}
	for _, c := range cases {
		if c.svc.Configured() {
			t.Errorf("%s: Configured() = true", c.name)
		}
		for _, e := range endpoints() {
			w := call(t, e.handler(c.svc), e.method, e.body, nil)
			if w.Code != 503 || errCode(t, w) != "not_configured" {
				t.Errorf("%s / %s: %d %q, want 503 not_configured", c.name, e.name, w.Code, errCode(t, w))
			}
		}
	}
	if m := newSvc(nil, nil, "", nil).Missing(); len(m) != 4 {
		t.Errorf("Missing() = %v, want all four", m)
	}
}

func TestEveryEndpointRejectsABadToken(t *testing.T) {
	store, rc := newMemStore(), newFakeRC()
	svc := newSvc(store, rc, entID, badAuth)
	for _, e := range endpoints() {
		w := call(t, e.handler(svc), e.method, e.body, nil)
		if w.Code != 401 || errCode(t, w) != "unauthorized" {
			t.Errorf("%s: %d %q, want 401 unauthorized", e.name, w.Code, errCode(t, w))
		}
	}
	if store.gotUser != "" || rc.gets != 0 {
		t.Error("a rejected token must touch neither the store nor RevenueCat")
	}
}

// MARK: me

func TestMeReturnsTheStandingAndAsksRevenueCatAboutCanClaim(t *testing.T) {
	svc, store, rc := configured()

	w := call(t, svc.HandleMe(), "GET", "", nil)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var got Standing
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	want := store.meStanding
	if got != want {
		t.Errorf("standing = %+v, want %+v", got, want)
	}
	if store.gotUser != userA {
		t.Errorf("store asked about %q, want the verified user", store.gotUser)
	}
	// Gate 2: every /me hands the store the device it came from.
	if len(store.meInstalls) != 1 || store.meInstalls[0] != device {
		t.Errorf("/me install ids = %v, want [%s]", store.meInstalls, device)
	}
	for _, k := range []string{"code", "invited", "months_earned", "months_pending", "months_saved", "can_claim"} {
		if _, ok := body(t, w)[k]; !ok {
			t.Errorf("response missing %q", k)
		}
	}

	// A paid history on RevenueCat turns can_claim off.
	rc.set(userA, withSubs(map[string]Subscription{"premium.month": storeSub(time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))}))
	w = call(t, svc.HandleMe(), "GET", "", nil)
	if cc := body(t, w)["can_claim"]; cc != false {
		t.Errorf("can_claim = %v with paid history, want false", cc)
	}

	// A RevenueCat outage fails OPEN (the claim re-checks with a hard answer).
	rc.err = errors.New("down")
	w = call(t, svc.HandleMe(), "GET", "", nil)
	if w.Code != 200 || body(t, w)["can_claim"] != true {
		t.Errorf("rc outage: %d can_claim=%v, want 200 true", w.Code, body(t, w)["can_claim"])
	}

	// Already claimed per the database: RevenueCat is not consulted at all.
	rc.err, rc.gets = nil, 0
	store.meStanding.CanClaim = false
	w = call(t, svc.HandleMe(), "GET", "", nil)
	if body(t, w)["can_claim"] != false || rc.gets != 0 {
		t.Errorf("claimed: can_claim=%v rc.gets=%d, want false 0", body(t, w)["can_claim"], rc.gets)
	}
}

func TestMePendingOfferOnlyForClaimedAndFailsSoft(t *testing.T) {
	svc, store, _ := configured()
	store.pending = &PendingOffer{OfferCode: "XK7P2M9Q", FriendCode: "YM-7KQ2"}

	// Never claimed (database can_claim true): not even asked.
	w := call(t, svc.HandleMe(), "GET", "", nil)
	if _, ok := body(t, w)["pending_offer"]; ok || store.pendingCalls != 0 {
		t.Errorf("unclaimed: pending_offer present=%v calls=%d, want absent 0", ok, store.pendingCalls)
	}

	// Claimed: the pending code and friend's code come back.
	store.meStanding.CanClaim = false
	w = call(t, svc.HandleMe(), "GET", "", nil)
	var got Standing
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PendingOffer == nil || *got.PendingOffer != *store.pending {
		t.Errorf("claimed: pending_offer = %+v, want %+v", got.PendingOffer, store.pending)
	}

	// None to hand out: the field is left out.
	store.pending = nil
	w = call(t, svc.HandleMe(), "GET", "", nil)
	if _, ok := body(t, w)["pending_offer"]; ok {
		t.Error("no pending offer: field must be absent")
	}

	// Lookup failing (022 not applied, hiccup): still 200, field left out.
	store.pendingErr = errors.New("function referral_pending_offer does not exist")
	w = call(t, svc.HandleMe(), "GET", "", nil)
	if _, ok := body(t, w)["pending_offer"]; w.Code != 200 || ok {
		t.Errorf("lookup error: %d pending present=%v, want 200 absent", w.Code, ok)
	}
}

func TestMeStoreDownIsStoreFailed(t *testing.T) {
	svc, store, _ := configured()
	store.meErr = errors.New("connection refused")
	w := call(t, svc.HandleMe(), "GET", "", nil)
	if w.Code != 502 || errCode(t, w) != "store_failed" {
		t.Errorf("%d %q, want 502 store_failed", w.Code, errCode(t, w))
	}
}

// MARK: claim

func TestClaimHappyPath(t *testing.T) {
	svc, store, rc, dc := configuredDC()
	w := call(t, svc.HandleClaim(), "POST", `{"code":"ym 7kq2","subscribed_before":false,"device_token":"tok","device_check_env":"production"}`, nil)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if got := body(t, w)["offer_code"]; got != "XK7P2M9Q" {
		t.Errorf("offer_code = %v", got)
	}
	if store.gotCode != "YM-7KQ2" || store.gotUser != userA {
		t.Errorf("store got (%q, %q), want normalised code for the verified user", store.gotUser, store.gotCode)
	}
	if rc.gets != 1 {
		t.Errorf("RevenueCat consulted %d times, want 1", rc.gets)
	}
	if store.gotInstall != device {
		t.Errorf("claim recorded install %q, want the request's", store.gotInstall)
	}
	if dc.queries != 1 || dc.sets != 1 || !dc.bit(DeviceEnvProduction, "tok") {
		t.Errorf("devicecheck queries=%d sets=%d bit0=%v, want 1 1 true", dc.queries, dc.sets, dc.bit(DeviceEnvProduction, "tok"))
	}
}

func TestClaimRefusalsBeforeAnythingIsSpent(t *testing.T) {
	svc, store, rc := configured()
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"bad json", `{`, 400, "bad_json"},
		{"bad code", `{"code":"YM-7KQ0"}`, 400, "bad_code"},
		{"empty code", `{"code":""}`, 400, "bad_code"},
	}
	for _, c := range cases {
		w := call(t, svc.HandleClaim(), "POST", c.body, nil)
		if w.Code != c.status || errCode(t, w) != c.code {
			t.Errorf("%s: %d %q, want %d %s", c.name, w.Code, errCode(t, w), c.status, c.code)
		}
	}
	if store.gotCode != "" || rc.gets != 0 {
		t.Error("cheap refusals must touch neither RevenueCat nor the store")
	}
}

func TestClaimRevenueCatDecides(t *testing.T) {
	svc, store, rc, dc := configuredDC()
	rc.set(userA, withSubs(map[string]Subscription{"premium.yearly": storeSub(time.Now().Add(-400*24*time.Hour), time.Now().Add(-35*24*time.Hour))}))
	w := call(t, svc.HandleClaim(), "POST", claimBody("YM-7KQ2"), nil)
	if w.Code != 400 || errCode(t, w) != "not_new_user" {
		t.Errorf("paid history: %d %q, want 400 not_new_user", w.Code, errCode(t, w))
	}
	if store.claimCalls != 0 || dc.queries != 0 {
		t.Error("a code must not be taken from the pool, nor Apple asked, for someone Apple will refuse")
	}

	// Promotional-only and family-shared histories are not "subscribed before".
	rc.set(userA, withSubs(map[string]Subscription{
		"rc_promo_pro_monthly": promoSub(time.Now().Add(10 * 24 * time.Hour)),
		"premium.month":        {Store: "app_store", OwnershipType: "FAMILY_SHARED", PurchaseDate: tp(time.Now()), ExpiresDate: tp(time.Now().Add(time.Hour))},
	}))
	w = call(t, svc.HandleClaim(), "POST", claimBody("YM-7KQ2"), nil)
	if w.Code != 200 {
		t.Errorf("promo/family only: %d %s, want 200", w.Code, w.Body)
	}

	rc.err = errors.New("timeout")
	w = call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2","device_token":"tok2"}`, nil)
	if w.Code != 502 || errCode(t, w) != "store_failed" {
		t.Errorf("rc down: %d %q, want 502 store_failed", w.Code, errCode(t, w))
	}
}

func TestClaimStoreRefusalsKeepTheirCode(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{Failure{ErrUnknownCode}, 400, "unknown_code"},
		{Failure{ErrOwnCode}, 400, "own_code"},
		{Failure{ErrAlreadyClaimed}, 400, "already_claimed"},
		{Failure{ErrNotNewUser}, 400, "not_new_user"},
		{Failure{ErrPoolEmpty}, 503, "pool_empty"},
		{errors.New("connection reset"), 502, "store_failed"},
		{context.DeadlineExceeded, 502, "store_failed"},
	}
	for _, c := range cases {
		// From the claim itself (the atomic re-check) …
		svc, store, _ := configured()
		store.claimErr = c.err
		w := call(t, svc.HandleClaim(), "POST", claimBody("YM-7KQ2"), nil)
		if w.Code != c.status || errCode(t, w) != c.code {
			t.Errorf("claim %v: %d %q, want %d %s", c.err, w.Code, errCode(t, w), c.status, c.code)
		}
		// … and from the read-only pre-check.
		svc, store, _ = configured()
		store.checkErr = c.err
		w = call(t, svc.HandleClaim(), "POST", claimBody("YM-7KQ2"), nil)
		if w.Code != c.status || errCode(t, w) != c.code {
			t.Errorf("check %v: %d %q, want %d %s", c.err, w.Code, errCode(t, w), c.status, c.code)
		}
	}
}

func TestClaimIsFivePerDevicePerDay(t *testing.T) {
	store, rc := newMemStore(), newFakeRC()
	store.claimOffer = "X"
	// Log mode: this test is about the bucket, not about bit0.
	svc := New(store, rc, entID, okAuth, Options{ClaimLimit: 2, ClaimWindow: 24 * time.Hour,
		DeviceCheck: newFakeDC(), DeviceCheckMode: DeviceCheckLog})

	// A malformed code does not spend a try.
	for i := 0; i < 5; i++ {
		call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ0"}`, nil)
	}
	for i := 1; i <= 2; i++ {
		if w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2"}`, nil); w.Code != 200 {
			t.Fatalf("try %d: %d %s", i, w.Code, w.Body)
		}
	}
	w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2"}`, nil)
	if w.Code != 429 || errCode(t, w) != "rate_limit_device" || w.Header().Get("Retry-After") == "" {
		t.Errorf("3rd try: %d %q retry-after=%q, want 429 rate_limit_device", w.Code, errCode(t, w), w.Header().Get("Retry-After"))
	}
	// Another device is its own bucket.
	other := map[string]string{"X-Device-Install-Id": "0E7B1B6C-7B9F-4F3E-9C2E-1D3A5F6B7C8D"}
	if w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2"}`, other); w.Code != 200 {
		t.Errorf("other device: %d, want 200", w.Code)
	}
	// The window rolls over.
	svc.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2"}`, nil); w.Code != 200 {
		t.Errorf("next day: %d, want 200", w.Code)
	}
}

func TestReadLimiterIsThirtyPerDevicePerHour(t *testing.T) {
	store, rc := newMemStore(), newFakeRC()
	store.meStanding = Standing{Code: "YM-7KQ2"}
	svc := New(store, rc, entID, okAuth, Options{ReadLimit: 3, ReadWindow: time.Hour})
	h := svc.ReadLimiter()(svc.HandleMe())
	for i := 1; i <= 3; i++ {
		if w := call(t, h.ServeHTTP, "GET", "", nil); w.Code != 200 {
			t.Fatalf("call %d: %d", i, w.Code)
		}
	}
	if w := call(t, h.ServeHTTP, "GET", "", nil); w.Code != 429 || errCode(t, w) != "rate_limit_device" {
		t.Errorf("4th: %d %q, want 429 rate_limit_device", w.Code, errCode(t, w))
	}
}

// MARK: redeemed

func TestRedeemedIsAHintAndIdempotent(t *testing.T) {
	svc, store, _ := configured()
	for i := 0; i < 2; i++ {
		w := call(t, svc.HandleRedeemed(), "POST", `{"offer_code":" XK7P2M9Q "}`, nil)
		if w.Code != 200 || body(t, w)["status"] != "ok" {
			t.Fatalf("status %d %s", w.Code, w.Body)
		}
	}
	if store.gotOffer != "XK7P2M9Q" || store.gotUser != userA {
		t.Errorf("store got (%q, %q)", store.gotUser, store.gotOffer)
	}
	select {
	case <-svc.nudge:
	default:
		t.Error("redeemed must nudge the settlement loop")
	}

	for _, bad := range []string{`{"offer_code":""}`, `{"offer_code":"` + strings.Repeat("A", 65) + `"}`, "{\"offer_code\":\"AB\\nCD\"}", `{`} {
		w := call(t, svc.HandleRedeemed(), "POST", bad, nil)
		if w.Code != 400 {
			t.Errorf("%q: %d, want 400", bad, w.Code)
		}
	}

	store.redeemErr = errors.New("down")
	w := call(t, svc.HandleRedeemed(), "POST", `{"offer_code":"XK7P2M9Q"}`, nil)
	if w.Code != 502 || errCode(t, w) != "store_failed" {
		t.Errorf("store down: %d %q, want 502 store_failed", w.Code, errCode(t, w))
	}
}

// MARK: account deletion

func TestDeleteUserIsNilSafe(t *testing.T) {
	var none *Service
	if err := none.DeleteUser(context.Background(), userA); err != nil {
		t.Errorf("nil service: %v", err)
	}
	if err := newSvc(nil, nil, "", nil).DeleteUser(context.Background(), userA); err != nil {
		t.Errorf("no store: %v", err)
	}
	svc, store, _ := configured()
	if err := svc.DeleteUser(context.Background(), userA); err != nil || len(store.deleted) != 1 || store.deleted[0] != userA {
		t.Errorf("configured: err=%v deleted=%v", err, store.deleted)
	}
}
