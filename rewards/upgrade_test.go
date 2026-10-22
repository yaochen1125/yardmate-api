package rewards

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// The friend's products (invite-friends.md ### 朋友在免费月里换套餐).
const (
	prodMonth    = "com.chenyao.plantapp.premium.month"
	prodYearly   = "com.chenyao.plantapp.premium.yearly"
	prodYearly3d = "com.chenyao.plantapp.premium.yearly.3d"
)

// fakeRedeemer stands in for referral settlement ① run on the spot: it
// answers verdict (or err) and moves the referral the way referral would.
type fakeRedeemer struct {
	store   *memStore
	verdict ChargeVerdict
	err     error
	// after, when set, replaces the state the verdict would leave — the
	// settlement loop acting between the redeem and the webhook's re-read.
	after   string
	calls   int
	product string
}

func (f *fakeRedeemer) RedeemForCharge(_ context.Context, _ int64, referee string, _ time.Time, productID string) (ChargeVerdict, error) {
	f.calls++
	f.product = productID
	if f.err != nil {
		return 0, f.err
	}
	switch {
	case f.after != "":
		f.store.setState(referee, f.after)
	case f.verdict == ChargeRedeemed:
		f.store.setState(referee, refRedeemed)
	case f.verdict == ChargeVoided:
		f.store.setState(referee, "void")
	}
	return f.verdict, nil
}

func (m *memStore) setState(referee, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[referee]
	r.State = state
	m.refs[referee] = r
}

func (m *memStore) setIssued(referee string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.refs[referee]
	r.IssuedAt = at
	m.refs[referee] = r
}

func (m *memStore) stateOf(referee string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refs[referee].State
}

// claimedSvc: a referral claimed at t0 that settlement has not redeemed yet,
// with a fake Redeemer wired.
func claimedSvc(t *testing.T, v ChargeVerdict) (*Service, *memStore, *fakeRedeemer) {
	t.Helper()
	svc, store, _ := newTestSvc(t0)
	store.addReferral(referrer, referee, refClaimed, t0)
	r := &fakeRedeemer{store: store, verdict: v}
	svc.SetRedeemer(r)
	return svc, store, r
}

// The free month's upgrade to premium.yearly, in RevenueCat's order: the
// PRODUCT_CHANGE notice (price 0), then the yearly's RENEWAL (NORMAL, 39.99)
// — both before settlement ① redeemed the referral.
func upgradeEvents() (change, renewal map[string]any) {
	at := t0.Add(10 * time.Minute)
	change = with(purchase("evt-change"), "type", "PRODUCT_CHANGE", "product_id", prodMonth,
		"new_product_id", prodYearly, "price", 0, "period_type", "TRIAL", "purchased_at_ms", at.UnixMilli())
	renewal = with(purchase("evt-renewal"), "type", "RENEWAL", "product_id", prodYearly,
		"transaction_id", "2000000222222222", "purchased_at_ms", at.UnixMilli())
	return change, renewal
}

func TestWebhookUpgradeBeforeSettlement(t *testing.T) {
	svc, store, red := claimedSvc(t, ChargeRedeemed)
	change, renewal := upgradeEvents()

	w := post(t, svc, secret, change)
	if got := status(t, w); w.Code != http.StatusOK || got != outProductChange {
		t.Fatalf("PRODUCT_CHANGE: %d %q", w.Code, got)
	}
	if n := svc.productChanges.Load(); n != 1 || red.calls != 0 || len(store.rows()) != 0 {
		t.Fatalf("PRODUCT_CHANGE: count %d, redeemer calls %d, rows %d", n, red.calls, len(store.rows()))
	}

	w = post(t, svc, secret, renewal)
	if got := status(t, w); w.Code != http.StatusOK || got != outRecorded {
		t.Fatalf("RENEWAL: %d %q", w.Code, got)
	}
	if red.calls != 1 || red.product != prodYearly {
		t.Fatalf("redeemer: %d call(s) for %q, want 1 for the yearly", red.calls, red.product)
	}
	if store.stateOf(referee) != refRedeemed {
		t.Fatalf("state %s, want redeemed", store.stateOf(referee))
	}
	rows := store.rows()
	if len(rows) != 1 || rows[0].CreditCents != 1020 || rows[0].EventType != "RENEWAL" || rows[0].ProductID != prodYearly {
		t.Fatalf("rows = %+v, want the yearly's 1020", rows)
	}

	// RevenueCat redelivers: nothing more, and no second redeem.
	w = post(t, svc, secret, renewal)
	if got := status(t, w); w.Code != http.StatusOK || got != outDuplicate || red.calls != 1 || len(store.rows()) != 1 {
		t.Fatalf("redelivery: %d %q calls=%d rows=%d", w.Code, got, red.calls, len(store.rows()))
	}
}

// RevenueCat does not show the yearly yet: 503 so it redelivers; once the
// settlement loop has redeemed the referral the redelivery records it once.
func TestWebhookRenewalBeforeSettlementRetries(t *testing.T) {
	svc, store, red := claimedSvc(t, ChargeRedeemed)
	red.err = errors.New("referral: subscription not visible on revenuecat yet")
	_, renewal := upgradeEvents()

	w := post(t, svc, secret, renewal)
	if got := status(t, w); w.Code != http.StatusServiceUnavailable || got != "error:not_settled" {
		t.Fatalf("not ready: %d %q", w.Code, got)
	}
	if len(store.rows()) != 0 || store.stateOf(referee) != refClaimed {
		t.Fatalf("not ready: rows %d state %s", len(store.rows()), store.stateOf(referee))
	}

	store.setState(referee, refRedeemed) // the 10-minute loop redeemed it
	w = post(t, svc, secret, renewal)
	if got := status(t, w); w.Code != http.StatusOK || got != outRecorded || len(store.rows()) != 1 || red.calls != 1 {
		t.Fatalf("redelivery: %d %q rows=%d calls=%d", w.Code, got, len(store.rows()), red.calls)
	}
}

func TestWebhookRenewalBeforeSettlementVerdicts(t *testing.T) {
	cases := []struct {
		name      string
		verdict   ChargeVerdict
		after     string // state the loop leaves meanwhile ("" = the verdict's)
		wantState string
	}{
		// A restored subscription older than the claim: never counts.
		{"older than the claim", ChargeNotCounted, "", refClaimed},
		// Gate 3 voided it on the spot.
		{"duplicate Apple ID", ChargeVoided, "", "void"},
		// Redeemed, but voided by someone else before the re-read.
		{"voided meanwhile", ChargeRedeemed, "void", "void"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store, red := claimedSvc(t, c.verdict)
			red.after = c.after
			_, renewal := upgradeEvents()
			w := post(t, svc, secret, renewal)
			if got := status(t, w); w.Code != http.StatusOK || got != outNotRedeemed {
				t.Fatalf("%d %q, want not_redeemed", w.Code, got)
			}
			if len(store.rows()) != 0 || store.stateOf(referee) != c.wantState {
				t.Fatalf("rows %d state %s", len(store.rows()), store.stateOf(referee))
			}
		})
	}
}

func TestWebhookRenewalBeforeSettlementEdges(t *testing.T) {
	_, renewal := upgradeEvents()

	// Nothing wired: 503, never a silent 200 that would lose the charge.
	svc, store, _ := newTestSvc(t0)
	store.addReferral(referrer, referee, refClaimed, t0)
	if w := post(t, svc, secret, renewal); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no redeemer: %d %s", w.Code, w.Body.String())
	}

	// A malformed event is 400 before anything is redeemed.
	svc, _, red := claimedSvc(t, ChargeRedeemed)
	if w := post(t, svc, secret, with(renewal, "transaction_id", nil)); w.Code != http.StatusBadRequest || red.calls != 0 {
		t.Fatalf("bad event: %d calls=%d", w.Code, red.calls)
	}

	// Charged before the claim: not counted, RevenueCat not asked.
	svc, _, red = claimedSvc(t, ChargeRedeemed)
	w := post(t, svc, secret, with(renewal, "purchased_at_ms", t0.Add(-time.Hour).UnixMilli()))
	if got := status(t, w); got != outNotRedeemed || red.calls != 0 {
		t.Fatalf("before the claim: %q calls=%d", got, red.calls)
	}

	// INITIAL_PURCHASE after the claim still counts without asking.
	svc, store, red = claimedSvc(t, ChargeRedeemed)
	w = post(t, svc, secret, purchase("evt-first"))
	if got := status(t, w); got != outRecorded || red.calls != 0 || store.stateOf(referee) != refClaimed {
		t.Fatalf("first purchase: %q calls=%d state=%s", got, red.calls, store.stateOf(referee))
	}

	// Past settlement ①'s 200-day window (from the last code issue): ① gave
	// up on it, a charge does not revive it.
	svc, store, red = claimedSvc(t, ChargeRedeemed)
	store.setIssued(referee, t0.Add(-201*24*time.Hour))
	w = post(t, svc, secret, renewal)
	if got := status(t, w); got != outNotRedeemed || red.calls != 0 {
		t.Fatalf("past the window: %q calls=%d", got, red.calls)
	}
	// A code reissued inside the window keeps it open.
	svc, store, red = claimedSvc(t, ChargeRedeemed)
	store.setIssued(referee, t0.Add(-199*24*time.Hour))
	if got := status(t, post(t, svc, secret, renewal)); got != outRecorded || red.calls != 1 {
		t.Fatalf("inside the window: %q calls=%d", got, red.calls)
	}

	// A PRODUCT_CHANGE of someone who is nobody's friend is not counted.
	svc, _, _ = claimedSvc(t, ChargeRedeemed)
	change, _ := upgradeEvents()
	w = post(t, svc, secret, with(change, "app_user_id", "AAAAAAAA-0000-0000-0000-000000000000"))
	if got := status(t, w); got != outNotReferee || svc.productChanges.Load() != 0 {
		t.Fatalf("stranger's PRODUCT_CHANGE: %q count %d", got, svc.productChanges.Load())
	}
}

// Downgrade: premium.yearly.3d bought during the free month takes effect at
// its end. By then settlement has long redeemed the referral; a free trial
// of the yearly (if any) is not counted, its first charge is — once.
func TestWebhookDowngradeAtRenewal(t *testing.T) {
	svc, store := redeemedSvc(t)
	red := &fakeRedeemer{store: store}
	svc.SetRedeemer(red)
	end := t0.Add(30 * 24 * time.Hour)

	change := with(purchase("evt-change"), "type", "PRODUCT_CHANGE", "product_id", prodMonth,
		"new_product_id", prodYearly3d, "price", 0, "period_type", "TRIAL")
	trial := with(purchase("evt-trial"), "type", "RENEWAL", "product_id", prodYearly3d, "period_type", "TRIAL",
		"price", 0, "transaction_id", "2000000333333333", "purchased_at_ms", end.UnixMilli())
	charge := with(purchase("evt-charge"), "type", "RENEWAL", "product_id", prodYearly3d,
		"transaction_id", "2000000444444444", "purchased_at_ms", end.Add(3*24*time.Hour).UnixMilli())

	for _, step := range []struct {
		ev   map[string]any
		want string
	}{
		{change, outProductChange},
		{trial, outNotCounted},
		{charge, outRecorded},
		{charge, outDuplicate},
	} {
		w := post(t, svc, secret, step.ev)
		if got := status(t, w); w.Code != http.StatusOK || got != step.want {
			t.Fatalf("%v: %d %q, want %q", step.ev["id"], w.Code, got, step.want)
		}
	}
	rows := store.rows()
	if len(rows) != 1 || rows[0].CreditCents != 1020 || red.calls != 0 {
		t.Fatalf("rows = %+v calls=%d, want one 1020 and no redeem", rows, red.calls)
	}
}

// REVENUECAT_WEBHOOK_ALLOW_SANDBOX (staging only): SANDBOX events are booked
// like PRODUCTION ones; anything else (e.g. a missing environment) still is
// not. Off by default.
func TestWebhookAllowSandbox(t *testing.T) {
	svc, store := redeemedSvc(t)
	if got := status(t, post(t, svc, secret, with(purchase("e1"), "environment", "SANDBOX"))); got != outIgnoredEnvironment {
		t.Fatalf("default: %q, want ignored", got)
	}
	svc.allowSandbox = true
	if got := status(t, post(t, svc, secret, with(purchase("e2"), "environment", "SANDBOX"))); got != outRecorded || len(store.rows()) != 1 {
		t.Fatalf("allowed: %q rows=%d", got, len(store.rows()))
	}
	if got := status(t, post(t, svc, secret, with(purchase("e3"), "environment", nil, "transaction_id", "2000000555555555"))); got != outIgnoredEnvironment {
		t.Fatalf("no environment: %q, want ignored", got)
	}
}
