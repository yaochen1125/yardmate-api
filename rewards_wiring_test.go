package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/ratelimit"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// rewardsTestServer is the real router with the referral + rewards services
// built from vaultText and no database — what a server looks like before
// SUPABASE_DB_URL / migration 015 are in place.
func rewardsTestServer(t *testing.T, vaultText string) *Server {
	t.Helper()
	return rewardsTestServerIPLimit(t, vaultText, 1000)
}

// rewardsTestServerIPLimit is rewardsTestServer with the /v1 per-IP limit
// (per hour) set to ipLimit.
func rewardsTestServerIPLimit(t *testing.T, vaultText string, ipLimit int) *Server {
	t.Helper()
	store, err := attest.OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	verifier, err := attest.New(attest.Options{AppID: "TEAM.bundle", Store: store})
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.Parse(strings.NewReader(vaultText))
	if err != nil {
		t.Fatal(err)
	}
	lim := ratelimit.New(ipLimit, time.Hour, 1000, time.Hour, 1000, time.Hour)
	referralSvc := buildReferralService(vault, nil, nil)
	rewardsSvc := buildRewardsService(vault, nil, referralSvc, nil)
	return newServer(verifier, vault, lim, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, referralSvc, nil, rewardsSvc)
}

func TestRevenueCatWebhookRouteRegistered(t *testing.T) {
	body := `{"event":{"id":"e","type":"TEST","environment":"SANDBOX"},"api_version":"1.0"}`
	do := func(s *Server, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/revenuecat/webhook", strings.NewReader(body))
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	// No secret in the vault: 503, never 404 and never open.
	s := rewardsTestServer(t, "")
	if w := do(s, "anything"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret: %d %s", w.Code, w.Body.String())
	}

	// Secret set, no database: wrong secret 401, right one 503.
	s = rewardsTestServer(t, "REVENUECAT_WEBHOOK_SECRET=rc-hook-secret\n")
	if w := do(s, "Bearer wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", w.Code)
	}
	if w := do(s, "Bearer rc-hook-secret"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no db: %d %s", w.Code, w.Body.String())
	}
}

func TestRevenueCatWebhookHasItsOwnIPLimit(t *testing.T) {
	// /v1 allows one request per IP per hour here. The webhook must not spend
	// from that bucket (RevenueCat posts everyone's events from a few IPs) but
	// keeps its own 600 / min per IP. httptest requests all come from 192.0.2.1.
	s := rewardsTestServerIPLimit(t, "REVENUECAT_WEBHOOK_SECRET=rc-hook-secret\n", 1)
	do := func(path string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"event":{"type":"TEST","environment":"SANDBOX"}}`))
		r.Header.Set("Authorization", "Bearer rc-hook-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 600; i++ {
		if code := do("/v1/revenuecat/webhook"); code == http.StatusTooManyRequests {
			t.Fatalf("webhook 429 at request %d", i+1)
		}
	}
	if code := do("/v1/revenuecat/webhook"); code != http.StatusTooManyRequests {
		t.Fatalf("webhook request 601 in a minute: %d, want 429", code)
	}
	// The /v1 bucket is untouched by all of that: its one request still passes.
	if code := do("/v1/attest/challenge"); code == http.StatusTooManyRequests {
		t.Fatal("/v1 bucket spent by webhook traffic")
	}
	if code := do("/v1/attest/challenge"); code != http.StatusTooManyRequests {
		t.Fatalf("second /v1 request: %d, want 429 (limit 1)", code)
	}
}

func TestReferralPayoutRouteRegistered(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/referral/payout", strings.NewReader(`{"email":"a@example.com"}`))
	r.Header.Set("X-Device-Install-Id", "4E7B1B6C-7B9F-4F3E-9C2E-1D3A5F6B7C8D")
	r.Header.Set("X-App-Version", "2.2.0 (50)")
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	rewardsTestServer(t, "").ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "not_configured") {
		t.Fatalf("unconfigured payout: %d %s", w.Code, w.Body.String())
	}
}
