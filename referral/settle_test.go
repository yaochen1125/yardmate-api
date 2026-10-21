package referral

import (
	"context"
	"errors"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func settleAt(svc *Service, store *memStore, at time.Time) Report {
	svc.now = func() time.Time { return at }
	store.dueNow = at
	return svc.Settle(context.Background())
}

func newSettle() (*Service, *memStore, *fakeRC) {
	store, rc := newMemStore(), newFakeRC()
	return New(store, rc, entID, okAuth, Options{}), store, rc
}

// bSubscribed makes B look like they redeemed: a store subscription of their
// own starting at `start` (after the claim) and running a month.
func bSubscribed(rc *fakeRC, start time.Time) {
	rc.set(userB, withSubs(map[string]Subscription{
		"com.chenyao.plantapp.premium.month": storeSub(start, start.Add(RewardMonth)),
	}))
}

func TestSettleGrantsStraightAwayWhenReferrerIsNotPaying(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	bSubscribed(rc, base) // redeemed one hour after claiming

	// ① RevenueCat shows the subscription → redeemed at its real start.
	rep := settleAt(svc, store, base.Add(time.Hour))
	if rep.Redeemed != 1 || store.get(id).State != StateRedeemed || !store.get(id).RedeemedAt.Equal(base) {
		t.Fatalf("after ①: %+v state=%s redeemedAt=%v", rep, store.get(id).State, store.get(id).RedeemedAt)
	}
	// ② not yet: six days is not seven.
	rep = settleAt(svc, store, base.Add(6*day))
	if rep.Qualified != 0 || rep.Granted != 0 || len(rc.grants) != 0 {
		t.Fatalf("day 6: %+v grants=%d, want nothing", rep, len(rc.grants))
	}
	// ②+③ day seven: qualified and, A not paying, granted at once.
	now := base.Add(7*day + time.Minute)
	rep = settleAt(svc, store, now)
	if rep.Qualified != 1 || rep.Granted != 1 || rep.Saved != 0 {
		t.Fatalf("day 7: %+v", rep)
	}
	if len(rc.grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(rc.grants))
	}
	g := rc.grants[0]
	if g.user != AppUserID(userA) || g.ent != entID || !g.until.Equal(now.Add(RewardMonth)) {
		t.Errorf("grant = %+v, want A/%s until now+30d (%v)", g, entID, now.Add(RewardMonth))
	}
	led, _ := store.Ledger(context.Background(), userA)
	if led.MonthsGranted != 1 || !led.PromoUntil.Equal(g.until) {
		t.Errorf("ledger = %+v", led)
	}
	if store.get(id).State != StateRewarded {
		t.Errorf("state = %s, want rewarded", store.get(id).State)
	}
	// Idempotent: another pass does nothing more.
	if rep = settleAt(svc, store, now.Add(time.Hour)); rep.Granted+rep.Saved+rep.Flushed != 0 || len(rc.grants) != 1 {
		t.Errorf("extra pass: %+v grants=%d", rep, len(rc.grants))
	}
}

func TestSettleSavesWhileReferrerIsPayingThenPaysOutWhenItEnds(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateRedeemed, base.Add(-8*day))
	store.refs[id].RedeemedAt = base.Add(-7*day - time.Hour)
	// A is on a paid plan until day 20 (a 3-day trial or their own friend's
	// free month look exactly the same to this check).
	paidUntil := base.Add(20 * day)
	rc.set(userA, withSubs(map[string]Subscription{
		"com.chenyao.plantapp.premium.month": {Store: "app_store", PeriodType: "trial", OwnershipType: "PURCHASED",
			PurchaseDate: tp(base.Add(-day)), OriginalPurchaseDate: tp(base.Add(-day)), ExpiresDate: tp(paidUntil)},
	}))
	bSubscribed(rc, base.Add(-7*day-time.Hour))

	rep := settleAt(svc, store, base)
	if rep.Qualified != 1 || rep.Saved != 1 || rep.Granted != 0 || len(rc.grants) != 0 {
		t.Fatalf("while paying: %+v grants=%d", rep, len(rc.grants))
	}
	if store.get(id).State != StateSaved {
		t.Fatalf("state = %s, want saved", store.get(id).State)
	}
	// Still paying on day 15: nothing moves.
	if rep = settleAt(svc, store, base.Add(15*day)); rep.Flushed != 0 || len(rc.grants) != 0 {
		t.Fatalf("day 15: %+v", rep)
	}
	// Paid plan over: the saved month goes out on the next pass.
	now := base.Add(21 * day)
	rep = settleAt(svc, store, now)
	if rep.Flushed != 1 || len(rc.grants) != 1 || !rc.grants[0].until.Equal(now.Add(RewardMonth)) {
		t.Fatalf("after plan ended: %+v grants=%+v", rep, rc.grants)
	}
	if store.get(id).State != StateRewarded {
		t.Errorf("state = %s, want rewarded", store.get(id).State)
	}
}

func TestSettlePaysAllSavedMonthsInOneGrant(t *testing.T) {
	svc, store, rc := newSettle()
	for _, b := range []string{userB, "77777777-7777-7777-7777-777777777777", "88888888-8888-8888-8888-888888888888"} {
		id := store.add(userA, b, StateQualified, base.Add(-10*day))
		store.refs[id].qualifiedAt = base
	}
	rc.set(userA, withSubs(map[string]Subscription{"p": storeSub(base.Add(-day), base.Add(10*day))}))

	if rep := settleAt(svc, store, base); rep.Saved != 3 || len(rc.grants) != 0 {
		t.Fatalf("saving: %+v", rep)
	}
	now := base.Add(11 * day)
	rep := settleAt(svc, store, now)
	if rep.Flushed != 3 || len(rc.grants) != 1 {
		t.Fatalf("flush: %+v grants=%d", rep, len(rc.grants))
	}
	if want := now.Add(3 * RewardMonth); !rc.grants[0].until.Equal(want) {
		t.Errorf("until = %v, want %v (3 × 30 d)", rc.grants[0].until, want)
	}
	led, _ := store.Ledger(context.Background(), userA)
	if led.MonthsGranted != 3 || len(store.byState(StateSaved)) != 0 {
		t.Errorf("ledger = %+v saved left = %d", led, len(store.byState(StateSaved)))
	}
}

func TestSettleHasNoCap(t *testing.T) {
	svc, store, rc := newSettle()
	// Ten already paid out, promotional access running to day 300.
	store.ledgers[userA] = Ledger{MonthsGranted: 10, PromoUntil: base.Add(300 * day)}
	rc.set(userA, withSubs(map[string]Subscription{"rc_promo": promoSub(base.Add(300 * day))}))
	id := store.add(userA, userB, StateQualified, base.Add(-10*day))

	rep := settleAt(svc, store, base)
	if rep.Granted != 1 || len(rc.grants) != 1 {
		t.Fatalf("11th: %+v grants=%d", rep, len(rc.grants))
	}
	if want := base.Add(330 * day); !rc.grants[0].until.Equal(want) {
		t.Errorf("until = %v, want existing expiry + 30 d (%v)", rc.grants[0].until, want)
	}
	led, _ := store.Ledger(context.Background(), userA)
	if led.MonthsGranted != 11 || store.get(id).State != StateRewarded {
		t.Errorf("ledger = %+v state = %s", led, store.get(id).State)
	}
}

func TestSettleStacksOnWhateverRevenueCatAlreadyShows(t *testing.T) {
	// A grant made by hand in the dashboard (ledger knows nothing) is extended,
	// never cut short.
	svc, store, rc := newSettle()
	rc.set(userA, withSubs(map[string]Subscription{"rc_promo": promoSub(base.Add(100 * day))}))
	store.add(userA, userB, StateQualified, base.Add(-10*day))
	settleAt(svc, store, base)
	if len(rc.grants) != 1 || !rc.grants[0].until.Equal(base.Add(130*day)) {
		t.Errorf("grants = %+v, want until day 130", rc.grants)
	}
}

func TestSettleRefundDoesNotCount(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateRedeemed, base.Add(-9*day))
	store.refs[id].RedeemedAt = base.Add(-8 * day)
	sub := storeSub(base.Add(-8*day), base.Add(22*day))
	sub.RefundedAt = tp(base.Add(-2 * day))
	rc.set(userB, withSubs(map[string]Subscription{"premium.month": sub}))

	rep := settleAt(svc, store, base)
	if rep.Voided != 1 || rep.Qualified != 0 || len(rc.grants) != 0 {
		t.Fatalf("%+v grants=%d", rep, len(rc.grants))
	}
	if r := store.refs[id]; r.State != StateVoid || r.voidReason != "refunded" {
		t.Errorf("state=%s reason=%q", r.State, r.voidReason)
	}
}

func TestSettleCancelledButNotRefundedStillCounts(t *testing.T) {
	// Friend turned auto-renew off inside the free month: no refund, so it
	// counts (docs ## 特殊情况).
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateRedeemed, base.Add(-9*day))
	store.refs[id].RedeemedAt = base.Add(-8 * day)
	sub := storeSub(base.Add(-8*day), base.Add(22*day))
	sub.UnsubscribeDetectedAt = tp(base.Add(-3 * day))
	rc.set(userB, withSubs(map[string]Subscription{"premium.month": sub}))
	if rep := settleAt(svc, store, base); rep.Qualified != 1 || rep.Granted != 1 {
		t.Errorf("%+v", rep)
	}
}

func TestSettleIgnoresSubscriptionsThatPredateTheClaim(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base)
	// Began before the code was put in (a renewal after it does not matter).
	rc.set(userB, withSubs(map[string]Subscription{"premium.month": {Store: "app_store", OwnershipType: "PURCHASED",
		OriginalPurchaseDate: tp(base.Add(-40 * day)), PurchaseDate: tp(base.Add(day)), ExpiresDate: tp(base.Add(31 * day))}}))
	if rep := settleAt(svc, store, base.Add(2*day)); rep.Redeemed != 0 || store.get(id).State != StateClaimed {
		t.Errorf("%+v state=%s", rep, store.get(id).State)
	}
	// A promotional grant or a family-shared plan is not a redemption either.
	rc.set(userB, withSubs(map[string]Subscription{
		"rc_promo":      promoSub(base.Add(31 * day)),
		"premium.month": {Store: "app_store", OwnershipType: "FAMILY_SHARED", OriginalPurchaseDate: tp(base.Add(day)), ExpiresDate: tp(base.Add(31 * day))},
	}))
	if rep := settleAt(svc, store, base.Add(2*day)); rep.Redeemed != 0 {
		t.Errorf("%+v", rep)
	}
}

func TestSettleHoldDaysComeFromSettings(t *testing.T) {
	svc, store, rc := newSettle()
	store.holdDays = 3
	id := store.add(userA, userB, StateRedeemed, base.Add(-4*day))
	store.refs[id].RedeemedAt = base.Add(-3*day - time.Minute)
	bSubscribed(rc, base.Add(-3*day-time.Minute))
	if rep := settleAt(svc, store, base); rep.Qualified != 1 {
		t.Errorf("hold_days=3: %+v", rep)
	}
	// Settings unreadable → default 7, counted as an error, nothing qualifies early.
	svc2, store2, rc2 := newSettle()
	store2.holdErr = errors.New("no row")
	id2 := store2.add(userA, userB, StateRedeemed, base.Add(-4*day))
	store2.refs[id2].RedeemedAt = base.Add(-3 * day)
	bSubscribed(rc2, base.Add(-3*day))
	if rep := settleAt(svc2, store2, base); rep.Qualified != 0 || rep.Errors != 1 {
		t.Errorf("settings down: %+v", rep)
	}
}

func TestSettleLedgerWriteFailureIsLoudAndRetried(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateQualified, base.Add(-10*day))
	store.recordErr = errors.New("db down")
	rep := settleAt(svc, store, base)
	if rep.Errors != 1 || rep.Granted != 0 || len(rc.grants) != 1 || store.get(id).State != StateQualified {
		t.Fatalf("%+v grants=%d state=%s", rep, len(rc.grants), store.get(id).State)
	}
	// Next pass (db back): granted again and recorded — the user gets an extra
	// month rather than losing one.
	store.recordErr = nil
	if rep = settleAt(svc, store, base.Add(10*time.Minute)); rep.Granted != 1 || store.get(id).State != StateRewarded {
		t.Errorf("retry: %+v state=%s", rep, store.get(id).State)
	}
}

func TestSettleRevenueCatOutageSkipsRows(t *testing.T) {
	svc, store, rc := newSettle()
	store.add(userA, userB, StateClaimed, base)
	store.add(userA, "77777777-7777-7777-7777-777777777777", StateQualified, base)
	rc.err = errors.New("503")
	rep := settleAt(svc, store, base.Add(day))
	if rep.Errors == 0 || rep.Redeemed+rep.Granted+rep.Saved != 0 || len(rc.grants) != 0 {
		t.Errorf("%+v grants=%d", rep, len(rc.grants))
	}
	rc.gerr = errors.New("404 entitlement")
	rc.err = nil
	rep = settleAt(svc, store, base.Add(day))
	if rep.Granted != 0 || rep.Errors == 0 || len(store.byState(StateQualified)) != 1 {
		t.Errorf("grant failing: %+v", rep)
	}
}

func TestSettlerOnlyStartsWhenConfigured(t *testing.T) {
	svc := newSvc(nil, nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartSettler(ctx) // must not panic or spin
	svc.Nudge()
	var none *Service
	none.Nudge()
}
