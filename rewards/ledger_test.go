package rewards

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func summaryOf(t *testing.T, svc *Service, user string) Summary {
	t.Helper()
	v, err := svc.Rewards(context.Background(), user)
	if err != nil {
		t.Fatalf("Rewards: %v", err)
	}
	s, ok := v.(Summary)
	if !ok {
		t.Fatalf("Rewards returned %T", v)
	}
	return s
}

func TestMatureMovesOnlyDuePending(t *testing.T) {
	svc, store, _ := newTestSvc(t0)
	store.credit(referrer, "e1", "t1", 1020, LedgerPending, t0.Add(-time.Second)) // due
	store.credit(referrer, "e2", "t2", 178, LedgerPending, t0)                    // due exactly now
	store.credit(referrer, "e3", "t3", 500, LedgerPending, t0.Add(time.Hour))     // not yet
	store.credit(referrer, "e4", "t4", 700, LedgerVoided, t0.Add(-time.Hour))     // voided stays voided

	if n := svc.Mature(context.Background()); n != 2 {
		t.Fatalf("matured %d, want 2", n)
	}
	want := map[string]string{"e1": LedgerAvailable, "e2": LedgerAvailable, "e3": LedgerPending, "e4": LedgerVoided}
	for _, r := range store.rows() {
		if r.State != want[r.EventID] {
			t.Errorf("%s state=%s, want %s", r.EventID, r.State, want[r.EventID])
		}
	}
	// A second pass has nothing left to do.
	if n := svc.Mature(context.Background()); n != 0 {
		t.Fatalf("second pass matured %d", n)
	}
}

func TestMatureStoreErrorIsCountedAsZero(t *testing.T) {
	svc, store, _ := newTestSvc(t0)
	store.matureErr = errBoom
	if n := svc.Mature(context.Background()); n != 0 {
		t.Fatalf("matured %d on error", n)
	}
}

func TestSummaryZeroForNewUser(t *testing.T) {
	svc, _, _ := newTestSvc(t0)
	s := summaryOf(t, svc, referrer)
	if s != (Summary{}) {
		t.Fatalf("summary = %+v, want zeros", s)
	}
	raw, _ := json.Marshal(s)
	if string(raw) != `{"available_cents":0,"pending_cents":0,"lifetime_cents":0,"payout":null}` {
		t.Fatalf("wire = %s", raw)
	}
}

func TestSummaryBalanceRules(t *testing.T) {
	svc, store, _ := newTestSvc(t0)
	// 2 × $102.00 available, $12.00 pending, $7.00 voided (never counts).
	store.credit(referrer, "a1", "t1", 10200, LedgerAvailable, t0.Add(-time.Hour))
	store.credit(referrer, "a2", "t2", 10200, LedgerAvailable, t0.Add(-time.Hour))
	store.credit(referrer, "p1", "t3", 1200, LedgerPending, t0.Add(time.Hour))
	store.credit(referrer, "v1", "t4", 700, LedgerVoided, t0.Add(-time.Hour))
	// Someone else's money is not ours.
	store.credit(stranger, "s1", "t5", 99999, LedgerAvailable, t0.Add(-time.Hour))

	s := summaryOf(t, svc, referrer)
	if s.AvailableCents != 20400 || s.PendingCents != 1200 || s.LifetimeCents != 20400 || s.Payout != nil {
		t.Fatalf("before payouts: %+v", s)
	}

	// One sent + one requested: both come off the balance; lifetime unchanged.
	sent := store.payout(referrer, PayoutSent, t0.Add(-48*time.Hour), t0.Add(-24*time.Hour))
	_ = sent
	req := store.payout(referrer, PayoutRequested, t0.Add(-time.Hour), time.Time{})
	s = summaryOf(t, svc, referrer)
	if s.AvailableCents != 400 || s.LifetimeCents != 20400 {
		t.Fatalf("after two payouts: %+v", s)
	}
	if s.Payout == nil || s.Payout.ID != req.ID || s.Payout.State != PayoutRequested || s.Payout.SentAt != nil {
		t.Fatalf("latest payout = %+v", s.Payout)
	}

	// The operator cancels the requested one: its $100 is back, no write to
	// the ledger needed.
	store.mu.Lock()
	req.State = PayoutCancelled
	store.mu.Unlock()
	s = summaryOf(t, svc, referrer)
	if s.AvailableCents != 10400 {
		t.Fatalf("after cancel: available=%d, want 10400", s.AvailableCents)
	}
	if s.Payout == nil || s.Payout.State != PayoutCancelled {
		t.Fatalf("latest after cancel = %+v", s.Payout)
	}
}

func TestSummaryNeverNegative(t *testing.T) {
	// A sent card whose credits were later voided would otherwise read < 0.
	svc, store, _ := newTestSvc(t0)
	store.credit(referrer, "a1", "t1", 5000, LedgerAvailable, t0)
	store.payout(referrer, PayoutSent, t0.Add(-time.Hour), t0)
	if s := summaryOf(t, svc, referrer); s.AvailableCents != 0 || s.LifetimeCents != 5000 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestPayoutViewWireFormat(t *testing.T) {
	loc := time.FixedZone("PDT", -7*3600)
	v := viewOf(&Payout{ID: 17, AmountCents: 10000, State: PayoutSent,
		RequestedAt: time.Date(2026, 10, 20, 5, 6, 7, 123456789, loc),
		SentAt:      time.Date(2026, 10, 22, 9, 0, 0, 999, time.UTC)})
	raw, _ := json.Marshal(v)
	want := `{"id":17,"amount_cents":10000,"state":"sent","requested_at":"2026-10-20T12:06:07Z","sent_at":"2026-10-22T09:00:00Z"}`
	if string(raw) != want {
		t.Fatalf("wire = %s\nwant  %s", raw, want)
	}
	raw, _ = json.Marshal(viewOf(&Payout{ID: 1, AmountCents: 10000, State: PayoutRequested, RequestedAt: t0}))
	if string(raw) != `{"id":1,"amount_cents":10000,"state":"requested","requested_at":"2026-10-20T12:00:00Z","sent_at":null}` {
		t.Fatalf("requested wire = %s", raw)
	}
}

func TestRewardsStoreError(t *testing.T) {
	svc, store, _ := newTestSvc(t0)
	store.balanceErr = errBoom
	if _, err := svc.Rewards(context.Background(), referrer); err == nil {
		t.Fatal("want error")
	}
}
