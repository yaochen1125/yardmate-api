package referral

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type fakeRewards struct {
	v       any
	err     error
	gotUser string
}

func (f *fakeRewards) Rewards(_ context.Context, userID string) (any, error) {
	f.gotUser = userID
	return f.v, f.err
}

func TestMeWithoutRewardsReaderOmitsField(t *testing.T) {
	svc, _, _ := configured()
	w := call(t, svc.HandleMe(), "GET", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if _, ok := body(t, w)["rewards"]; ok {
		t.Fatalf("rewards present without a reader: %s", w.Body.String())
	}
}

func TestMeCarriesRewardsBlock(t *testing.T) {
	svc, _, _ := configured()
	fr := &fakeRewards{v: map[string]any{"available_cents": 2360, "pending_cents": 1200, "lifetime_cents": 12360, "payout": nil}}
	svc.SetRewards(fr)
	w := call(t, svc.HandleMe(), "GET", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	got, ok := body(t, w)["rewards"].(map[string]any)
	if !ok {
		t.Fatalf("rewards missing: %s", w.Body.String())
	}
	if got["available_cents"] != float64(2360) || got["pending_cents"] != float64(1200) {
		t.Fatalf("rewards = %v", got)
	}
	if fr.gotUser != userA {
		t.Fatalf("reader asked for %q", fr.gotUser)
	}
	// The rest of /me is untouched.
	if body(t, w)["code"] != "YM-7KQ2" {
		t.Fatalf("code lost: %s", w.Body.String())
	}
}

func TestMeRewardsFailureOmitsFieldKeeps200(t *testing.T) {
	svc, _, _ := configured()
	svc.SetRewards(&fakeRewards{err: errors.New("relation \"reward_ledger\" does not exist")})
	w := call(t, svc.HandleMe(), "GET", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if _, ok := body(t, w)["rewards"]; ok {
		t.Fatalf("rewards present after a failed read: %s", w.Body.String())
	}
}

func TestClaimBucketIsTheClaimBucket(t *testing.T) {
	svc, _, _ := configured()
	b := svc.ClaimBucket()
	for i := 0; i < DefaultClaimLimit; i++ {
		if ok, _ := b.Allow(device, svc.now()); !ok {
			t.Fatalf("try %d refused", i+1)
		}
	}
	// The shared bucket is spent: /claim now refuses this device.
	w := call(t, svc.HandleClaim(), "POST", `{"code":"YM-7KQ2","subscribed_before":false}`, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("claim after spending the shared bucket: %d %s", w.Code, w.Body.String())
	}
}
