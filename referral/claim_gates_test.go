package referral

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The /claim judgement order and anti-abuse gates 1 and 2
// (invite-friends.md ### 防刷, /claim 判定顺序), against a memStore that
// replays 016's SQL rules and a fake DeviceCheck.

// userC is declared with the PG tests.
const (
	codeA   = "YM-AAAA"
	codeC   = "YM-CCCC"
	deviceA = "AAAAAAAA-1B6C-4F3E-9C2E-1D3A5F6B7C8D" // a device A has opened the invite page on
)

// gated: B is the caller (okAuth answers userA, so these use asB), A and C
// own codes, the shelf has P1 and P2.
func gated(mode DeviceCheckMode) (*Service, *memStore, *fakeRC, *fakeDC) {
	store, rc, dc := newMemStore(), newFakeRC(), newFakeDC()
	store.emulateClaims("P1", "P2")
	store.own(userA, codeA)
	store.own(userC, codeC)
	store.sawDevice(userA, deviceA)
	svc := New(store, rc, entID, asB, Options{DeviceCheck: dc, DeviceCheckMode: mode})
	return svc, store, rc, dc
}

func asB(*http.Request) (string, error) { return userB, nil }

func claimWith(code, token string, subscribedBefore bool) string {
	b := `{"code":"` + code + `"`
	if subscribedBefore {
		b += `,"subscribed_before":true`
	}
	if token != "" {
		b += `,"device_token":"` + token + `","device_check_env":"production"`
	}
	return b + "}"
}

func claimAs(t *testing.T, svc *Service, body string, headers map[string]string) (int, string, string) {
	t.Helper()
	w := call(t, svc.HandleClaim(), "POST", body, headers)
	m := body2(w.Body.String())
	return w.Code, m["error"], m["offer_code"]
}

// body2 decodes a flat string map ({"offer_code":…} / {"error":…}).
func body2(s string) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// MARK: judgement order

func TestClaimOrderUnknownCodeComesFirst(t *testing.T) {
	svc, store, rc, dc := gated(DeviceCheckEnforce)
	dc.bits[DeviceEnvProduction+"|tok"] = true
	rc.set(userB, withSubs(map[string]Subscription{"premium.month": storeSub(time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour))}))
	status, code, _ := claimAs(t, svc, claimWith("YM-2345", "", true), nil)
	if status != 400 || code != ErrUnknownCode {
		t.Errorf("%d %q, want 400 unknown_code before not_new_user / device checks", status, code)
	}
	if rc.gets != 0 || dc.queries != 0 || store.claimCalls != 0 {
		t.Errorf("rc=%d dc=%d claim=%d, want nothing asked", rc.gets, dc.queries, store.claimCalls)
	}
}

func TestClaimOrderAlreadyClaimedBeforeOwnCode(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	store.claims[userB] = &memClaim{referrer: userC, offer: "OLD"}
	// A's code, on A's own device, saying subscribed: already_claimed wins.
	status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", true), map[string]string{"X-Device-Install-Id": deviceA})
	if status != 400 || code != ErrAlreadyClaimed {
		t.Errorf("%d %q, want 400 already_claimed", status, code)
	}
	if dc.queries != 0 {
		t.Error("DeviceCheck asked for a refusal decided earlier")
	}
}

func TestClaimAgainSkipsRevenueCatAndDeviceCheck(t *testing.T) {
	svc, store, rc, dc := gated(DeviceCheckEnforce)
	store.claims[userB] = &memClaim{referrer: userA, offer: "MINE"}
	// The friend has redeemed by now (paid history on RevenueCat, the app
	// says subscribed), this device's bit0 is set (by that very claim), and
	// they are on another phone with no token: still the same code back.
	rc.set(userB, withSubs(map[string]Subscription{"premium.month": storeSub(time.Now().Add(-48*time.Hour), time.Now().Add(time.Hour))}))
	dc.bits[DeviceEnvProduction+"|tok"] = true
	for _, h := range []map[string]string{nil, {"X-Device-Install-Id": "0E7B1B6C-7B9F-4F3E-9C2E-1D3A5F6B7C8D"}} {
		for _, body := range []string{claimWith(codeA, "", true), claimWith("ym aaaa", "tok", false)} {
			status, code, offer := claimAs(t, svc, body, h)
			if status != 200 || offer != "MINE" {
				t.Errorf("re-claim %s: %d %q %q, want 200 MINE", body, status, code, offer)
			}
		}
	}
	if rc.gets != 0 || dc.queries != 0 || dc.sets != 0 {
		t.Errorf("rc=%d dc queries=%d sets=%d, want none for a re-claim", rc.gets, dc.queries, dc.sets)
	}
}

func TestClaimOrderOwnCodeBeforeNotNewUser(t *testing.T) {
	store, rc, dc := newMemStore(), newFakeRC(), newFakeDC()
	store.emulateClaims("P1")
	store.own(userA, codeA)
	svc := New(store, rc, entID, okAuth, Options{DeviceCheck: dc}) // caller is A
	status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", true), nil)
	if status != 400 || code != ErrOwnCode {
		t.Errorf("%d %q, want 400 own_code", status, code)
	}
	if rc.gets != 0 || dc.queries != 0 {
		t.Error("own_code must be answered before RevenueCat / DeviceCheck")
	}
}

func TestClaimOnTheReferrersDeviceIsOwnCode(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	// Gate 2: B on a device A has used, A's code → own_code (install id case
	// does not matter) …
	for _, dev := range []string{deviceA, strings.ToLower(deviceA)} {
		status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", false), map[string]string{"X-Device-Install-Id": dev})
		if status != 400 || code != ErrOwnCode {
			t.Errorf("%s: %d %q, want 400 own_code", dev, status, code)
		}
	}
	if dc.queries != 0 || store.claimCalls != 0 {
		t.Error("own_code must stop before DeviceCheck and the pool")
	}
	// … but C's code on that same device is not A's business (gate 1 is).
	status, code, offer := claimAs(t, svc, claimWith(codeC, "tok", false), map[string]string{"X-Device-Install-Id": deviceA})
	if status != 200 || offer != "P1" {
		t.Errorf("C's code on A's device: %d %q %q, want 200 P1", status, code, offer)
	}
	if c := store.claims[userB]; c == nil || c.install != strings.ToLower(deviceA) {
		t.Errorf("claim = %+v, want the claiming device recorded", c)
	}
}

func TestClaimOrderNotNewUserBeforeDeviceCheck(t *testing.T) {
	svc, _, rc, dc := gated(DeviceCheckEnforce)
	status, code, _ := claimAs(t, svc, claimWith(codeA, "", true), nil)
	if status != 400 || code != ErrNotNewUser {
		t.Errorf("app says subscribed: %d %q, want 400 not_new_user (not device_unverified)", status, code)
	}
	rc.set(userB, withSubs(map[string]Subscription{"premium.month": storeSub(time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))}))
	status, code, _ = claimAs(t, svc, claimWith(codeA, "", false), nil)
	if status != 400 || code != ErrNotNewUser {
		t.Errorf("revenuecat history: %d %q, want 400 not_new_user", status, code)
	}
	if dc.queries != 0 {
		t.Error("DeviceCheck asked for someone who is not new")
	}
}

// MARK: gate 1, enforce

func TestClaimDeviceCheckEnforce(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		setup  func(*fakeDC)
		status int
		code   string
	}{
		{"no token", "", nil, 400, ErrDeviceUnverified},
		{"token apple rejects", "bad", func(d *fakeDC) { d.invalid["bad"] = true }, 400, ErrDeviceUnverified},
		{"device already a friend", "tok", func(d *fakeDC) { d.bits[DeviceEnvProduction+"|tok"] = true }, 400, ErrDeviceUsed},
		{"apple down", "tok", func(d *fakeDC) { d.queryErr = errors.New("503") }, 503, ErrDeviceUnavailable},
	}
	for _, c := range cases {
		svc, store, _, dc := gated(DeviceCheckEnforce)
		if c.setup != nil {
			c.setup(dc)
		}
		status, code, _ := claimAs(t, svc, claimWith(codeA, c.token, false), nil)
		if status != c.status || code != c.code {
			t.Errorf("%s: %d %q, want %d %s", c.name, status, code, c.status, c.code)
		}
		if store.claimCalls != 0 || len(store.shelf) != 2 || dc.sets != 0 {
			t.Errorf("%s: claim calls %d shelf %v sets %d — nothing may be taken or set", c.name, store.claimCalls, store.shelf, dc.sets)
		}
	}
}

func TestClaimSetsBit0WithTheCodeAndRollsBackWhenItCannot(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	dc.setErr = errors.New("apple 500")
	status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
	if status != 503 || code != ErrDeviceUnavailable {
		t.Fatalf("set fails: %d %q, want 503 device_check_unavailable", status, code)
	}
	if len(store.shelf) != 2 || store.shelf[0] != "P1" || store.claims[userB] != nil {
		t.Fatalf("after a failed set: shelf %v claim %+v — the code must still be free, no referral", store.shelf, store.claims[userB])
	}

	dc.setErr = nil
	status, code, offer := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
	if status != 200 || offer != "P1" {
		t.Fatalf("retry: %d %q %q, want 200 P1 (the code the failed try left)", status, code, offer)
	}
	if !dc.bit(DeviceEnvProduction, "tok") {
		t.Error("bit0 not set after a successful claim")
	}
}

func TestClaimPoolEmptyLeavesBit0Alone(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	store.shelf = nil
	status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
	if status != 503 || code != ErrPoolEmpty {
		t.Errorf("%d %q, want 503 pool_empty", status, code)
	}
	if dc.queries != 1 || dc.sets != 0 || dc.bit(DeviceEnvProduction, "tok") {
		t.Errorf("queries %d sets %d — pool_empty must not set bit0", dc.queries, dc.sets)
	}
}

func TestClaimDeviceUsedOnAnotherAccount(t *testing.T) {
	// Acceptance: B claimed on this phone; C (same phone, reinstalled) then
	// tries anyone's code → device_used, no code.
	store, rc, dc := newMemStore(), newFakeRC(), newFakeDC()
	store.emulateClaims("P1", "P2")
	store.own(userA, codeA)
	store.own(userC, codeC)
	caller := userB
	svc := New(store, rc, entID, func(*http.Request) (string, error) { return caller, nil }, Options{DeviceCheck: dc})
	if status, code, _ := claimAs(t, svc, claimWith(codeA, "phone", false), nil); status != 200 {
		t.Fatalf("B: %d %q", status, code)
	}
	caller = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	other := map[string]string{"X-Device-Install-Id": "0E7B1B6C-7B9F-4F3E-9C2E-1D3A5F6B7C8D"}
	if status, code, _ := claimAs(t, svc, claimWith(codeC, "phone", false), other); status != 400 || code != ErrDeviceUsed {
		t.Errorf("D on B's phone: %d %q, want 400 device_used", status, code)
	}
	if len(store.shelf) != 1 {
		t.Errorf("shelf = %v, want only B's code gone", store.shelf)
	}
}

// MARK: gate 1, log mode and not configured

func TestClaimDeviceCheckLogModeOnlyLogs(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		setup    func(*fakeDC)
		wantSets int
	}{
		{"no token", "", nil, 0},
		{"token apple rejects", "bad", func(d *fakeDC) { d.invalid["bad"] = true }, 0},
		{"device already a friend", "tok", func(d *fakeDC) { d.bits[DeviceEnvProduction+"|tok"] = true }, 1},
		{"apple down on query", "tok", func(d *fakeDC) { d.queryErr = errors.New("503") }, 1},
		{"set fails", "tok", func(d *fakeDC) { d.setErr = errors.New("500") }, 1},
		{"clean", "tok", nil, 1},
	}
	for _, c := range cases {
		svc, store, _, dc := gated(DeviceCheckLog)
		if c.setup != nil {
			c.setup(dc)
		}
		status, code, offer := claimAs(t, svc, claimWith(codeA, c.token, false), nil)
		if status != 200 || offer != "P1" {
			t.Errorf("%s: %d %q %q, want 200 P1 (log mode never refuses)", c.name, status, code, offer)
		}
		if store.claims[userB] == nil {
			t.Errorf("%s: claim not kept", c.name)
		}
		if dc.sets != c.wantSets {
			t.Errorf("%s: sets = %d, want %d (照查照置)", c.name, dc.sets, c.wantSets)
		}
	}
}

func TestClaimDeviceCheckNotConfigured(t *testing.T) {
	store, rc := newMemStore(), newFakeRC()
	store.emulateClaims("P1")
	store.own(userA, codeA)
	svc := New(store, rc, entID, asB, Options{}) // enforce, no client
	if mode, ok := svc.DeviceCheck(); mode != DeviceCheckEnforce || ok {
		t.Errorf("DeviceCheck() = %s %v", mode, ok)
	}
	status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
	if status != 503 || code != ErrDeviceUnavailable {
		t.Errorf("enforce without a client: %d %q, want 503 device_check_unavailable", status, code)
	}
	if store.claimCalls != 0 {
		t.Error("no code may go out unchecked in enforce mode")
	}
	// A re-claim never needs Apple.
	store.claims[userB] = &memClaim{referrer: userA, offer: "MINE"}
	if status, _, offer := claimAs(t, svc, claimWith(codeA, "", false), nil); status != 200 || offer != "MINE" {
		t.Errorf("re-claim without a client: %d %q, want 200 MINE", status, offer)
	}

	store2 := newMemStore()
	store2.emulateClaims("P1")
	store2.own(userA, codeA)
	logSvc := New(store2, rc, entID, asB, Options{DeviceCheckMode: DeviceCheckLog})
	if status, _, offer := claimAs(t, logSvc, claimWith(codeA, "", false), nil); status != 200 || offer != "P1" {
		t.Errorf("log mode without a client: %d %q, want 200 P1", status, offer)
	}
}

func TestClaimAppleHangingDoesNotHangTheClaim(t *testing.T) {
	for _, mode := range []DeviceCheckMode{DeviceCheckEnforce, DeviceCheckLog} {
		store, rc, dc := newMemStore(), newFakeRC(), newFakeDC()
		store.emulateClaims("P1")
		store.own(userA, codeA)
		dc.block = true
		svc := New(store, rc, entID, asB, Options{DeviceCheck: dc, DeviceCheckMode: mode, DeviceCheckTimeout: 50 * time.Millisecond})
		start := time.Now()
		status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s: took %v", mode, took)
		}
		want, wantCode := 503, ErrDeviceUnavailable
		if mode == DeviceCheckLog {
			want, wantCode = 200, ""
		}
		if status != want || code != wantCode {
			t.Errorf("%s: %d %q, want %d %q", mode, status, code, want, wantCode)
		}
	}
	if DefaultDeviceCheckTimeout != 3*time.Second {
		t.Errorf("default Apple timeout = %v, want 3s", DefaultDeviceCheckTimeout)
	}
}

func TestClaimEnforceOnlyAcceptsProductionTokens(t *testing.T) {
	// enforce: anything but an explicit "production" is "no token" —
	// device_unverified, Apple (and its development host) never asked.
	for _, env := range []string{`"development"`, `"DEVELOPMENT"`, `"staging"`, `""`, ``} {
		body := `{"code":"YM-AAAA","device_token":"t"`
		if env != "" {
			body += `,"device_check_env":` + env
		}
		body += "}"
		svc, store, _, dc := gated(DeviceCheckEnforce)
		status, code, _ := claimAs(t, svc, body, nil)
		if status != 400 || code != ErrDeviceUnverified {
			t.Errorf("enforce, env %s: %d %q, want 400 device_unverified", env, status, code)
		}
		if dc.queries != 0 || dc.sets != 0 || store.claimCalls != 0 {
			t.Errorf("enforce, env %s: queries %d sets %d claims %d, want nothing asked or taken", env, dc.queries, dc.sets, store.claimCalls)
		}
	}
	for _, env := range []string{"production", " Production "} {
		svc, _, _, dc := gated(DeviceCheckEnforce)
		body := `{"code":"YM-AAAA","device_token":"t","device_check_env":"` + env + `"}`
		if status, code, _ := claimAs(t, svc, body, nil); status != 200 {
			t.Fatalf("enforce, env %q: %d %q, want 200", env, status, code)
		}
		if len(dc.envs) != 1 || dc.envs[0] != DeviceEnvProduction || !dc.bit(DeviceEnvProduction, "t") {
			t.Errorf("enforce, env %q: envs %v, want production for query and set", env, dc.envs)
		}
	}
}

func TestClaimLogModeAsksTheEnvironmentTheRequestNames(t *testing.T) {
	for body, want := range map[string]string{
		`{"code":"YM-AAAA","device_token":"t","device_check_env":"development"}`: DeviceEnvDevelopment,
		`{"code":"YM-AAAA","device_token":"t","device_check_env":"production"}`:  DeviceEnvProduction,
		`{"code":"YM-AAAA","device_token":"t"}`:                                  DeviceEnvProduction,
	} {
		svc, _, _, dc := gated(DeviceCheckLog)
		if status, code, _ := claimAs(t, svc, body, nil); status != 200 {
			t.Fatalf("log, %s: %d %q", body, status, code)
		}
		if len(dc.envs) != 1 || dc.envs[0] != want || !dc.bit(want, "t") {
			t.Errorf("log, %s: envs %v, want %s for both query and set", body, dc.envs, want)
		}
	}
	// log + development with Apple refusing: still only logged.
	svc, store, _, dc := gated(DeviceCheckLog)
	dc.bits[DeviceEnvDevelopment+"|t"] = true
	if status, _, offer := claimAs(t, svc, `{"code":"YM-AAAA","device_token":"t","device_check_env":"development"}`, nil); status != 200 || offer != "P1" || store.claims[userB] == nil {
		t.Errorf("log, development, bit0 set: %d %q, want 200 P1", status, offer)
	}
}

func TestClaimBodyRoomForATokenButNoMore(t *testing.T) {
	svc, _, _, _ := gated(DeviceCheckEnforce)
	// A real token is a few KB of base64: it fits.
	if status, code, _ := claimAs(t, svc, claimWith(codeA, strings.Repeat("A", 4000), false), nil); status != 200 {
		t.Errorf("4 KB token: %d %q, want 200", status, code)
	}
	// Past any token: treated as none (device_unverified), not a 500.
	svc, _, _, _ = gated(DeviceCheckEnforce)
	if status, code, _ := claimAs(t, svc, claimWith(codeA, strings.Repeat("A", 13<<10), false), nil); status != 400 || code != ErrDeviceUnverified {
		t.Errorf("13 KB token: %d %q, want 400 device_unverified", status, code)
	}
	// Past the body cap: bad_json.
	if status, code, _ := claimAs(t, svc, claimWith(codeA, strings.Repeat("A", 20<<10), false), nil); status != 400 || code != "bad_json" {
		t.Errorf("20 KB body: %d %q, want 400 bad_json", status, code)
	}
}
