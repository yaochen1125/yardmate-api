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
	lim := ratelimit.New(1000, time.Hour, 1000, time.Hour, 1000, time.Hour)
	referralSvc := buildReferralService(vault, nil)
	rewardsSvc := buildRewardsService(vault, nil, referralSvc)
	return newServer(verifier, vault, lim, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, referralSvc, rewardsSvc)
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
