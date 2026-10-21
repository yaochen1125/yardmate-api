package referral

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// /claim fixes from the api#126 review: gate 1's check-then-set is atomic
// (one device, many accounts at once → one code), and a set whose answer was
// lost is read back before the claim is rolled back.

// MARK: one device, many accounts at once

func TestClaimSameDeviceManyAccountsAtOnceOnlyOneGetsACode(t *testing.T) {
	const n = 8
	store, rc, dc := newMemStore(), newFakeRC(), newFakeDC()
	shelf := make([]string, n)
	for i := range shelf {
		shelf[i] = fmt.Sprintf("P%d", i)
	}
	store.emulateClaims(shelf...)
	store.own(userA, codeA)
	// Apple takes a moment, so every claim is between its check and its set
	// at once unless something keeps them apart.
	dc.delay = 5 * time.Millisecond
	byHeader := func(r *http.Request) (string, error) { return r.Header.Get("X-Test-User"), nil }
	svc := New(store, rc, entID, byHeader, Options{DeviceCheck: dc})

	statuses, codes := make([]int, n), make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest("POST", "/v1/referral/claim", strings.NewReader(claimWith(codeA, "one-phone", false)))
			// Same device token; a different account and a forged install
			// id each (so neither the per-device bucket nor gate 2 helps).
			r.Header.Set("X-Device-Install-Id", fmt.Sprintf("%08X-1B6C-4F3E-9C2E-1D3A5F6B7C8D", i))
			r.Header.Set("X-App-Version", "2.2.0 (50)")
			r.Header.Set("X-Test-User", friend(100+i))
			w := httptest.NewRecorder()
			svc.HandleClaim()(w, r)
			statuses[i], codes[i] = w.Code, body2(w.Body.String())["error"]
		}(i)
	}
	wg.Wait()

	ok := 0
	for i := range statuses {
		switch {
		case statuses[i] == http.StatusOK:
			ok++
		case statuses[i] != http.StatusBadRequest || codes[i] != ErrDeviceUsed:
			t.Errorf("claim %d: %d %q, want 200 or 400 device_used", i, statuses[i], codes[i])
		}
	}
	if ok != 1 {
		t.Errorf("%d claims got a code, want exactly 1 for one device", ok)
	}
	if len(store.shelf) != n-1 || len(store.claims) != 1 || dc.sets != 1 {
		t.Errorf("shelf %d claims %d sets %d, want one code out, one claim, one set", len(store.shelf), len(store.claims), dc.sets)
	}
}

func TestClaimDoubleTapSameAccountAsksAppleOnce(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	dc.delay = 5 * time.Millisecond
	offers := make([]string, 2)
	var wg sync.WaitGroup
	for i := range offers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, code, offer := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
			if status != http.StatusOK {
				t.Errorf("tap %d: %d %q", i, status, code)
			}
			offers[i] = offer
		}(i)
	}
	wg.Wait()
	// Whichever way the two interleave, the second is a re-claim — under the
	// device lock it is re-judged before Apple is asked — and gets the same
	// code.
	if offers[0] != "P1" || offers[1] != "P1" || len(store.shelf) != 1 {
		t.Errorf("offers %v shelf %v, want P1 twice and one code out", offers, store.shelf)
	}
	if dc.queries != 1 || dc.sets != 1 {
		t.Errorf("queries %d sets %d, want Apple asked and told once", dc.queries, dc.sets)
	}
}

// MARK: a set whose answer got lost

func TestClaimSetAnswerLostButBitSetKeepsTheClaim(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	dc.setErr, dc.setApplies = errors.New("devicecheck: update_two_bits: context deadline exceeded"), true
	status, code, offer := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
	if status != http.StatusOK || offer != "P1" {
		t.Fatalf("%d %q %q, want 200 P1: Apple did set bit0, only its answer was lost", status, code, offer)
	}
	if store.claims[userB] == nil || len(store.shelf) != 1 || !dc.bit(DeviceEnvProduction, "tok") {
		t.Errorf("claim %+v shelf %v — the claim must stand with the bit", store.claims[userB], store.shelf)
	}
	if dc.queries != 2 {
		t.Errorf("queries = %d, want the check plus one read-back", dc.queries)
	}
}

func TestClaimSetFailsAndReadsBackClearRollsBack(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	dc.setErr = errors.New("devicecheck: update_two_bits status 503")
	status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
	if status != http.StatusServiceUnavailable || code != ErrDeviceUnavailable {
		t.Fatalf("%d %q, want 503 device_check_unavailable", status, code)
	}
	if store.claims[userB] != nil || len(store.shelf) != 2 || dc.queries != 2 {
		t.Errorf("claim %+v shelf %v queries %d — rolled back after one read-back", store.claims[userB], store.shelf, dc.queries)
	}
	// Not marked: the retry goes through.
	dc.setErr = nil
	if status, code, offer := claimAs(t, svc, claimWith(codeA, "tok", false), nil); status != http.StatusOK || offer != "P1" {
		t.Errorf("retry: %d %q %q, want 200 P1", status, code, offer)
	}
}

func TestClaimSetAndReadBackBothFailRollsBack(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckEnforce)
	// Apple may well have set it — but nobody can tell, so no code goes out.
	dc.setErr, dc.setApplies = errors.New("devicecheck: update_two_bits status 500"), true
	dc.queryFails = func(n int) error {
		if n >= 2 {
			return errors.New("devicecheck: query_two_bits: timeout")
		}
		return nil
	}
	status, code, _ := claimAs(t, svc, claimWith(codeA, "tok", false), nil)
	if status != http.StatusServiceUnavailable || code != ErrDeviceUnavailable {
		t.Fatalf("%d %q, want 503 device_check_unavailable", status, code)
	}
	if store.claims[userB] != nil || len(store.shelf) != 2 {
		t.Errorf("claim %+v shelf %v, want rolled back", store.claims[userB], store.shelf)
	}
}

func TestClaimLogModeSetFailureIsNotReadBack(t *testing.T) {
	svc, store, _, dc := gated(DeviceCheckLog)
	dc.setErr = errors.New("500")
	if status, _, offer := claimAs(t, svc, claimWith(codeA, "tok", false), nil); status != http.StatusOK || offer != "P1" || store.claims[userB] == nil {
		t.Fatalf("log mode: %d %q, want 200 P1 kept", status, offer)
	}
	if dc.queries != 1 {
		t.Errorf("queries = %d, want no read-back in log mode (nothing is refused anyway)", dc.queries)
	}
}
