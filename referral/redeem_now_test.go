package referral

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The friend's products (invite-friends.md ### 朋友在免费月里换套餐).
const (
	prodMonth    = "com.chenyao.plantapp.premium.month"     // level 3, the free month's product
	prodYearly   = "com.chenyao.plantapp.premium.yearly"    // level 1: buying it in the free month is an upgrade
	prodYearly3d = "com.chenyao.plantapp.premium.yearly.3d" // level 4: buying it in the free month is a downgrade
)

// freeMonth is the Referral Friend Month record: a free trial of
// premium.month starting at start.
func freeMonth(start, expires time.Time) Subscription {
	s := storeSub(start, expires)
	s.PeriodType = "trial"
	return s
}

// upgradedAt is the chain after an upgrade to premium.yearly at `at` inside a
// free month that began at start: the free month record (cut short) and the
// yearly record, both with the chain's start as original_purchase_date.
func upgradedAt(start, at time.Time) *Subscriber {
	yearly := storeSub(start, at.Add(365*day))
	yearly.PurchaseDate = tp(at)
	return withSubs(map[string]Subscription{
		prodMonth:  freeMonth(start, at),
		prodYearly: yearly,
	})
}

// Immediate upgrade (RevenueCat: PRODUCT_CHANGE, then the yearly RENEWAL)
// before settlement ① ran: the webhook's RENEWAL redeems the referral on the
// spot, settlement does not redeem it again, and seven days after the
// redemption the referrer gets exactly one month.
func TestRedeemForChargeUpgradeBeforeSettlement(t *testing.T) {
	svc, store, rc := newSettle()
	claimedAt := base.Add(-time.Hour)
	id := store.add(userA, userB, StateClaimed, claimedAt)
	rc.set(userB, upgradedAt(base, base.Add(5*time.Minute)))

	v, err := svc.RedeemForCharge(context.Background(), id, userB, claimedAt, prodYearly)
	if err != nil || v != ChargeRedeemed {
		t.Fatalf("RedeemForCharge = %v, %v; want redeemed", v, err)
	}
	r := store.get(id)
	if r.State != StateRedeemed || !r.RedeemedAt.Equal(base) {
		t.Fatalf("state %s redeemedAt %v; want redeemed at the free month's start %v", r.State, r.RedeemedAt, base)
	}
	if want := "app_store:" + base.Format(time.RFC3339Nano); store.refs[id].subKey != want {
		t.Fatalf("subscription key %q, want %q", store.refs[id].subKey, want)
	}
	// A redelivery after that finds it past claimed: still "redeemed", no error.
	if v, err := svc.RedeemForCharge(context.Background(), id, userB, claimedAt, prodYearly); err != nil || v != ChargeRedeemed {
		t.Fatalf("again: %v, %v", v, err)
	}

	// The loop's ① has nothing left to do.
	if rep := settleAt(svc, store, base.Add(10*time.Minute)); rep.Redeemed != 0 || rep.Voided != 0 {
		t.Fatalf("① after the webhook: %+v", rep)
	}
	// Day 6 (counted from the redemption, not the upgrade): nothing.
	if rep := settleAt(svc, store, base.Add(6*day)); rep.Qualified+rep.Granted != 0 {
		t.Fatalf("day 6: %+v", rep)
	}
	// Day 7: one month for A, once.
	now := base.Add(7*day + time.Minute)
	if rep := settleAt(svc, store, now); rep.Qualified != 1 || rep.Granted != 1 {
		t.Fatalf("day 7: %+v", rep)
	}
	if rep := settleAt(svc, store, now.Add(time.Hour)); rep.Granted+rep.Saved+rep.Flushed != 0 || len(rc.grants) != 1 {
		t.Fatalf("extra pass: %+v grants=%d", rep, len(rc.grants))
	}
}

// A subscription that began before the claim (a second account restoring an
// older purchase) never counts, however recent the renewal.
func TestRedeemForChargeOldSubscriptionNotCounted(t *testing.T) {
	svc, store, rc := newSettle()
	claimedAt := base
	id := store.add(userA, userB, StateClaimed, claimedAt)
	old := storeSub(base.Add(-40*day), base.Add(325*day))
	old.PurchaseDate = tp(base.Add(time.Hour)) // renewed after the claim
	rc.set(userB, withSubs(map[string]Subscription{prodYearly: old}))

	v, err := svc.RedeemForCharge(context.Background(), id, userB, claimedAt, prodYearly)
	if err != nil || v != ChargeNotCounted {
		t.Fatalf("= %v, %v; want not counted", v, err)
	}
	if store.get(id).State != StateClaimed {
		t.Fatalf("state %s, want still claimed", store.get(id).State)
	}
}

// RevenueCat not showing the charged product yet, or failing: not ready, so
// the webhook answers 5xx and RevenueCat redelivers.
func TestRedeemForChargeNotReady(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	rc.set(userB, withSubs(map[string]Subscription{prodMonth: freeMonth(base, base.Add(30*day))}))

	if _, err := svc.RedeemForCharge(context.Background(), id, userB, base.Add(-time.Hour), prodYearly); !errors.Is(err, ErrChargeNotReady) {
		t.Fatalf("yearly not on RevenueCat yet: err %v, want ErrChargeNotReady", err)
	}
	rc.err = errors.New("revenuecat 502")
	if _, err := svc.RedeemForCharge(context.Background(), id, userB, base.Add(-time.Hour), prodYearly); !errors.Is(err, ErrChargeNotReady) {
		t.Fatalf("revenuecat down: err %v, want ErrChargeNotReady", err)
	}
	if store.get(id).State != StateClaimed {
		t.Fatalf("state %s, want still claimed", store.get(id).State)
	}
	// Not configured at all: not ready either (never a silent 200).
	bare := New(store, nil, entID, okAuth, Options{})
	if _, err := bare.RedeemForCharge(context.Background(), id, userB, base, prodYearly); !errors.Is(err, ErrChargeNotReady) {
		t.Fatalf("no revenuecat: err %v", err)
	}
}

// Gate 3 runs on the spot too: the chain already belongs to another referral.
func TestRedeemForChargeDuplicateAppleID(t *testing.T) {
	svc, store, rc := newSettle()
	other := store.add(userA, "99999999-7777-8888-9999-aaaaaaaaaaaa", StateRedeemed, base.Add(-2*day))
	store.refs[other].subKey = "app_store:" + base.Format(time.RFC3339Nano)
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	rc.set(userB, upgradedAt(base, base.Add(time.Minute)))

	v, err := svc.RedeemForCharge(context.Background(), id, userB, base.Add(-time.Hour), prodYearly)
	if err != nil || v != ChargeVoided {
		t.Fatalf("= %v, %v; want voided", v, err)
	}
	if r := store.get(id); r.State != StateVoid {
		t.Fatalf("state %s, want void", r.State)
	}
}

// A family-shared record is not the friend's own purchase.
func TestRedeemForChargeFamilyShared(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	fam := storeSub(base, base.Add(365*day))
	fam.OwnershipType = ownershipFamily
	rc.set(userB, withSubs(map[string]Subscription{prodYearly: fam}))
	if v, err := svc.RedeemForCharge(context.Background(), id, userB, base.Add(-time.Hour), prodYearly); err != nil || v != ChargeNotCounted {
		t.Fatalf("= %v, %v; want not counted", v, err)
	}
}

// Downgrade at the end of the free month (premium.yearly.3d bought during it):
// nothing changes before the free month ends, so settlement redeems and pays
// as usual; the yearly record that appears later shares the chain's start.
func TestSettleDowngradeAtRenewal(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	rc.set(userB, withSubs(map[string]Subscription{prodMonth: freeMonth(base, base.Add(30*day))}))

	if rep := settleAt(svc, store, base.Add(10*time.Minute)); rep.Redeemed != 1 {
		t.Fatalf("①: %+v", rep)
	}
	if rep := settleAt(svc, store, base.Add(7*day+time.Minute)); rep.Qualified != 1 || rep.Granted != 1 {
		t.Fatalf("day 7: %+v", rep)
	}
	// The switch at the end of the month changes nothing for the referral.
	y3d := storeSub(base, base.Add(30*day+3*day))
	y3d.PeriodType, y3d.PurchaseDate = "trial", tp(base.Add(30*day))
	rc.set(userB, withSubs(map[string]Subscription{prodMonth: freeMonth(base, base.Add(30*day)), prodYearly3d: y3d}))
	if rep := settleAt(svc, store, base.Add(31*day)); rep.Granted+rep.Voided != 0 || len(rc.grants) != 1 || store.get(id).State != StateRewarded {
		t.Fatalf("after the switch: %+v grants=%d state=%s", rep, len(rc.grants), store.get(id).State)
	}
}

// After an upgrade the chain has two records with the same start. ②'s refund
// test looks at both, whichever StartedAfter picks: a refund on the yearly
// voids, a refund mark on the free month alone does not. Repeated so that Go's
// random map order would have caught a single-record check.
func TestSettleRefundLooksAtTheWholeChain(t *testing.T) {
	cases := []struct {
		name                        string
		monthRefunded, yearRefunded bool
		wantVoid                    bool
	}{
		{"yearly refunded", false, true, true},
		{"both refunded", true, true, true},
		{"only the free month marked", true, false, false},
		{"nothing refunded", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < 40; i++ {
				svc, store, rc := newSettle()
				id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
				sub := upgradedAt(base, base.Add(time.Hour))
				if c.monthRefunded {
					m := sub.Subscriptions[prodMonth]
					m.RefundedAt = tp(base.Add(2 * time.Hour))
					sub.Subscriptions[prodMonth] = m
				}
				if c.yearRefunded {
					y := sub.Subscriptions[prodYearly]
					y.RefundedAt = tp(base.Add(2 * day))
					sub.Subscriptions[prodYearly] = y
				}
				rc.set(userB, sub)
				settleAt(svc, store, base.Add(2*time.Hour))
				settleAt(svc, store, base.Add(7*day+time.Minute))
				r := store.get(id)
				if gotVoid := r.State == StateVoid; gotVoid != c.wantVoid {
					t.Fatalf("run %d: state %s (void reason %q), want void=%v", i, r.State, store.refs[id].voidReason, c.wantVoid)
				}
				if c.wantVoid && store.refs[id].voidReason != VoidRefunded {
					t.Fatalf("void reason %q, want %q", store.refs[id].voidReason, VoidRefunded)
				}
			}
		})
	}
}

func TestStartedAfterTieIsDeterministic(t *testing.T) {
	sub := upgradedAt(base, base.Add(time.Hour))
	for i := 0; i < 50; i++ {
		key, start, ok := sub.StartedAfter(base.Add(-time.Hour))
		if !ok || key != prodMonth || !start.Equal(base) {
			t.Fatalf("run %d: %q %v %v", i, key, start, ok)
		}
	}
}

func TestSubscriptionFree(t *testing.T) {
	var sub Subscriber
	raw := `{"subscriptions": {
		"trial":      {"store": "app_store", "period_type": "trial"},
		"zero":       {"store": "app_store", "period_type": "intro", "price": {"amount": 0, "currency": "USD"}},
		"paid":       {"store": "app_store", "period_type": "normal", "price": {"amount": 39.99, "currency": "USD"}},
		"no price":   {"store": "app_store", "period_type": "normal"},
		"null price": {"store": "app_store", "period_type": "normal", "price": null},
		"odd price":  {"store": "app_store", "period_type": "normal", "price": "free"}
	}}`
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		t.Fatalf("an unexpected price shape must not fail the decode: %v", err)
	}
	want := map[string]bool{"trial": true, "zero": true, "paid": false, "no price": false, "null price": false, "odd price": false}
	for k, w := range want {
		if got := sub.Subscriptions[k].free(); got != w {
			t.Errorf("%s: free = %v, want %v", k, got, w)
		}
	}
}
