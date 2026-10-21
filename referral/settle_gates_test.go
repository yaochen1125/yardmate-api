package referral

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Anti-abuse gates 3 (one Apple ID counts once) and 4 (velocity → held,
// review mail, approve / reject) in the settlement loop.

const premiumMonth = "com.chenyao.plantapp.premium.month"

// MARK: gate 3

func TestSubscriptionKeyFollowsTheChainNotTheAccount(t *testing.T) {
	start := base
	first := withSubs(map[string]Subscription{premiumMonth: storeSub(start, start.Add(RewardMonth))})
	// The same chain seen later on another account: renewed (new purchase
	// date), maybe upgraded (other product key) — same original purchase.
	renewed := storeSub(start, start.Add(2*RewardMonth))
	renewed.PurchaseDate = tp(start.Add(RewardMonth))
	elsewhere := withSubs(map[string]Subscription{"com.chenyao.plantapp.premium.yearly.3d": renewed})
	k1, k2 := first.SubscriptionKey(premiumMonth), elsewhere.SubscriptionKey("com.chenyao.plantapp.premium.yearly.3d")
	if k1 == "" || k1 != k2 {
		t.Errorf("keys %q / %q, want the same non-empty key", k1, k2)
	}
	if !strings.HasPrefix(k1, "app_store:") {
		t.Errorf("key %q, want the store in it", k1)
	}
	other := withSubs(map[string]Subscription{premiumMonth: storeSub(start.Add(time.Second), start.Add(RewardMonth))})
	if other.SubscriptionKey(premiumMonth) == k1 {
		t.Error("a different original purchase must be a different key")
	}
	if first.SubscriptionKey("nope") != "" || (*Subscriber)(nil).SubscriptionKey(premiumMonth) != "" {
		t.Error("no entry → empty key")
	}
	if withSubs(map[string]Subscription{premiumMonth: {Store: "app_store"}}).SubscriptionKey(premiumMonth) != "" {
		t.Error("no start time → empty key")
	}
}

func TestSettleSameAppleIDCountsOnce(t *testing.T) {
	svc, store, rc := newSettle()
	idB := store.add(userA, userB, StateClaimed, base.Add(-2*time.Hour))
	idC := store.add(userA, userC, StateClaimed, base.Add(-time.Hour))
	// One Apple ID redeemed on B's account and was restored onto C's: both
	// customers show the same subscription chain.
	rc.set(userB, withSubs(map[string]Subscription{premiumMonth: storeSub(base, base.Add(RewardMonth))}))
	rc.set(userC, withSubs(map[string]Subscription{premiumMonth: storeSub(base, base.Add(RewardMonth))}))

	rep := settleAt(svc, store, base.Add(time.Hour))
	if rep.Redeemed != 1 || rep.Duplicate != 1 || rep.Voided != 1 {
		t.Fatalf("①: %+v, want 1 redeemed + 1 duplicate", rep)
	}
	if b := store.refs[idB]; b.State != StateRedeemed || b.subKey == "" {
		t.Errorf("B: %s key %q, want redeemed with the key", b.State, b.subKey)
	}
	if c := store.refs[idC]; c.State != StateVoid || c.voidReason != VoidDuplicate || c.subKey != store.refs[idB].subKey {
		t.Errorf("C: %s %q key %q, want void duplicate_apple_id with the same key", c.State, c.voidReason, c.subKey)
	}
	// A gets one month, not two.
	settleAt(svc, store, base.Add(8*day))
	if len(rc.grants) != 1 {
		t.Errorf("grants = %d, want 1", len(rc.grants))
	}
}

func TestSettleKeyOfADeletedAccountStillCounts(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	rc.set(userB, withSubs(map[string]Subscription{premiumMonth: storeSub(base, base.Add(RewardMonth))}))
	store.usedKeys[rc.subs[AppUserID(userB)].SubscriptionKey(premiumMonth)] = true // left by an account deletion
	if rep := settleAt(svc, store, base.Add(time.Hour)); rep.Duplicate != 1 || store.get(id).State != StateVoid {
		t.Errorf("%+v state %s, want void as duplicate", rep, store.get(id).State)
	}
}

func TestSettleAVoidReferralDoesNotBlockTheKey(t *testing.T) {
	svc, store, rc := newSettle()
	old := store.add(userA, userB, StateVoid, base.Add(-3*time.Hour))
	store.refs[old].voidReason = VoidRefunded
	id := store.add(userA, userC, StateClaimed, base.Add(-time.Hour))
	rc.set(userC, withSubs(map[string]Subscription{premiumMonth: storeSub(base, base.Add(RewardMonth))}))
	store.refs[old].subKey = rc.subs[AppUserID(userC)].SubscriptionKey(premiumMonth)
	if rep := settleAt(svc, store, base.Add(time.Hour)); rep.Redeemed != 1 || store.get(id).State != StateRedeemed {
		t.Errorf("%+v state %s, want redeemed (only LIVE referrals hold a key)", rep, store.get(id).State)
	}
}

// MARK: gate 4

// friend returns a distinct referee id.
func friend(i int) string { return fmt.Sprintf("%08d-0000-4000-8000-000000000000", i) }

// dueFriends adds n referrals of A, redeemed 8 days before at, each with its
// own friend, subscription and claim device (devs[i] when given).
func dueFriends(store *memStore, rc *fakeRC, at time.Time, from, n int, devs ...string) []int64 {
	var ids []int64
	for i := from; i < from+n; i++ {
		f := friend(i)
		id := store.add(userA, f, StateRedeemed, at.Add(-9*day))
		store.refs[id].RedeemedAt = at.Add(-8 * day)
		store.refs[id].installID = fmt.Sprintf("DEV-%d", i)
		if k := i - from; k < len(devs) {
			store.refs[id].installID = devs[k]
		}
		rc.set(f, withSubs(map[string]Subscription{premiumMonth: storeSub(at.Add(-8*day), at.Add(30*day))}))
		ids = append(ids, id)
	}
	return ids
}

func gateSvc() (*Service, *memStore, *fakeRC, *fakeAlerter) {
	svc, store, rc := newSettle()
	alert := &fakeAlerter{}
	svc.alerter = alert
	return svc, store, rc, alert
}

func TestSettleVelocityHoldsTheSixthAndMailsOnce(t *testing.T) {
	svc, store, rc, alert := gateSvc()
	ids := dueFriends(store, rc, base, 0, 6)

	rep := settleAt(svc, store, base)
	if rep.Qualified != 5 || rep.Held != 1 || rep.Granted != 5 || len(rc.grants) != 5 {
		t.Fatalf("pass: %+v grants=%d, want 5 qualified+granted, 1 held", rep, len(rc.grants))
	}
	held := store.byState(StateHeld)
	if len(held) != 1 || held[0].ID != ids[5] {
		t.Fatalf("held = %+v, want the sixth", held)
	}
	if rep.Reviewed != 1 || len(alert.sent) != 1 {
		t.Fatalf("review mails: reviewed=%d sent=%d, want 1", rep.Reviewed, len(alert.sent))
	}
	m := alert.sent[0]
	for _, want := range []string{userA, "待审核：1 条（这封新增 1 条）", "6 台不同设备 → 都不同",
		fmt.Sprintf("#%d", ids[5]), "held (新)", "deploy/referral-review.sh approve", "deploy/referral-review.sh reject",
		"24 小时内成功超过 5 个，第 6 个起待审核"} {
		if !strings.Contains(m.body, want) {
			t.Errorf("mail body missing %q:\n%s", want, m.body)
		}
	}
	if !strings.Contains(m.subject, "1 held") || strings.ContainsFunc(m.subject, func(r rune) bool { return r > 127 }) {
		t.Errorf("subject %q, want ASCII with the count", m.subject)
	}

	// The next pass: nothing new — no mail, no grant, still held.
	rep = settleAt(svc, store, base.Add(10*time.Minute))
	if rep.Reviewed != 0 || len(alert.sent) != 1 || len(rc.grants) != 5 || store.get(ids[5]).State != StateHeld {
		t.Errorf("second pass: %+v sent=%d grants=%d state=%s", rep, len(alert.sent), len(rc.grants), store.get(ids[5]).State)
	}

	// approve → the next pass pays it like any other; it is not held again.
	if !store.approve(ids[5]) {
		t.Fatal("approve")
	}
	rep = settleAt(svc, store, base.Add(20*time.Minute))
	if rep.Granted != 1 || len(rc.grants) != 6 || store.get(ids[5]).State != StateRewarded {
		t.Errorf("after approve: %+v grants=%d state=%s", rep, len(rc.grants), store.get(ids[5]).State)
	}
}

func TestSettleVelocityFiveIsFine(t *testing.T) {
	svc, store, rc, alert := gateSvc()
	dueFriends(store, rc, base, 0, 5)
	rep := settleAt(svc, store, base)
	if rep.Qualified != 5 || rep.Held != 0 || len(rc.grants) != 5 || len(alert.sent) != 0 {
		t.Errorf("%+v grants=%d mails=%d, want all five paid, no mail", rep, len(rc.grants), len(alert.sent))
	}
}

func TestSettleVelocityWindowAndSwitch(t *testing.T) {
	// Five successes 25 hours ago are outside the 24-hour window.
	svc, store, rc, _ := gateSvc()
	for _, id := range dueFriends(store, rc, base, 0, 5) {
		store.refs[id].State, store.refs[id].qualifiedAt = StateRewarded, base.Add(-25*time.Hour)
	}
	dueFriends(store, rc, base, 10, 1)
	if rep := settleAt(svc, store, base); rep.Held != 0 || rep.Qualified != 1 {
		t.Errorf("outside the window: %+v, want qualified", rep)
	}

	// The window and the limit come from referral_settings; 0 turns it off.
	svc, store, rc, _ = gateSvc()
	store.velocityLimit = 0
	dueFriends(store, rc, base, 0, 8)
	if rep := settleAt(svc, store, base); rep.Held != 0 || rep.Qualified != 8 {
		t.Errorf("limit 0: %+v, want all qualified", rep)
	}
	svc, store, rc, _ = gateSvc()
	store.velocityLimit = 2
	dueFriends(store, rc, base, 0, 4)
	if rep := settleAt(svc, store, base); rep.Held != 2 || rep.Qualified != 2 {
		t.Errorf("limit 2: %+v, want 2 + 2 held", rep)
	}
}

func TestSettleRejectedHeldIsNeverPaid(t *testing.T) {
	svc, store, rc, _ := gateSvc()
	ids := dueFriends(store, rc, base, 0, 6)
	settleAt(svc, store, base)
	if !store.reject(ids[5]) {
		t.Fatal("reject")
	}
	settleAt(svc, store, base.Add(time.Hour))
	settleAt(svc, store, base.Add(48*time.Hour))
	if r := store.refs[ids[5]]; r.State != StateVoid || r.voidReason != VoidRejected || len(rc.grants) != 5 {
		t.Errorf("rejected: %s %q grants=%d, want void rejected, five grants", r.State, r.voidReason, len(rc.grants))
	}
}

func TestSettleReviewMailAtMostOncePerReferrerPerDay(t *testing.T) {
	svc, store, rc, alert := gateSvc()
	dueFriends(store, rc, base, 0, 6)
	settleAt(svc, store, base)
	if len(alert.sent) != 1 {
		t.Fatalf("first mail: %d", len(alert.sent))
	}
	// Two hours later another one goes held: no second mail inside 24 h.
	later := base.Add(2 * time.Hour)
	dueFriends(store, rc, later, 20, 1)
	if rep := settleAt(svc, store, later); rep.Held != 1 || len(alert.sent) != 1 {
		t.Fatalf("inside 24 h: %+v mails=%d, want held and no mail", rep, len(alert.sent))
	}
	// A day after the first mail, the unannounced one is mailed.
	rep := settleAt(svc, store, base.Add(25*time.Hour))
	if rep.Reviewed != 1 || len(alert.sent) != 2 {
		t.Fatalf("after 24 h: %+v mails=%d, want the second mail", rep, len(alert.sent))
	}
	if body := alert.sent[1].body; !strings.Contains(body, "待审核：2 条（这封新增 1 条）") {
		t.Errorf("second mail:\n%s", body)
	}
}

func TestSettleReviewMailFailureIsRetried(t *testing.T) {
	svc, store, rc, alert := gateSvc()
	dueFriends(store, rc, base, 0, 6)
	alert.err = errors.New("smtp: 535")
	if rep := settleAt(svc, store, base); rep.Reviewed != 0 || rep.Errors == 0 {
		t.Fatalf("failed send: %+v", rep)
	}
	alert.err = nil
	if rep := settleAt(svc, store, base.Add(10*time.Minute)); rep.Reviewed != 1 || len(alert.sent) != 1 {
		t.Errorf("retry: %+v sent=%d", rep, len(alert.sent))
	}

	// Without a mailer: logged once, marked, not repeated.
	svc, store, rc, _ = gateSvc()
	svc.alerter = nil
	dueFriends(store, rc, base, 0, 6)
	if rep := settleAt(svc, store, base); rep.Reviewed != 1 {
		t.Errorf("no mailer: %+v, want logged + marked", rep)
	}
	if rep := settleAt(svc, store, base.Add(10*time.Minute)); rep.Reviewed != 0 {
		t.Errorf("no mailer, second pass: %+v", rep)
	}

	// The digest read failing is counted, nothing else stops.
	svc, store, rc, _ = gateSvc()
	store.digestErr = errors.New("db down")
	dueFriends(store, rc, base, 0, 6)
	if rep := settleAt(svc, store, base); rep.Errors == 0 || rep.Granted != 5 {
		t.Errorf("digest error: %+v", rep)
	}
}

func TestReviewMailSpotsSharedDevices(t *testing.T) {
	svc, store, rc, alert := gateSvc()
	dueFriends(store, rc, base, 0, 6, "SAME", "SAME", "same", "B", "", "C")
	settleAt(svc, store, base)
	if len(alert.sent) != 1 {
		t.Fatal("no mail")
	}
	body := alert.sent[0].body
	for _, want := range []string{"记了设备的 5 条，3 台不同设备 → 有重复", "另有 1 条是 016 之前填的码"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}
