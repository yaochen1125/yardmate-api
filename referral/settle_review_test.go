package referral

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Settlement fixes from the api#126 review (migration 017): the ① / ④
// batches rotate.

// MARK: batch rotation

func TestSettleClaimedBacklogRotates(t *testing.T) {
	svc, store, rc := newSettle()
	// More never-redeemed claims than one batch holds.
	n := settleBatch + 50
	var stale []int64
	for i := 0; i < n; i++ {
		stale = append(stale, store.add(userA, friend(1000+i), StateClaimed, base.Add(-time.Duration(n-i)*time.Minute)))
	}
	settleAt(svc, store, base)
	if rc.gets != settleBatch {
		t.Fatalf("pass 1 looked at %d, want one full batch (%d)", rc.gets, settleBatch)
	}

	// A friend claims after that pass and redeems straight away.
	late := store.add(userA, userB, StateClaimed, base.Add(time.Minute))
	bSubscribed(rc, base.Add(2*time.Minute))
	rep := settleAt(svc, store, base.Add(10*time.Minute))
	if rep.Redeemed != 1 || store.get(late).State != StateRedeemed {
		t.Fatalf("pass 2: %+v, newcomer %s — it must not wait behind the same stale batch forever", rep, store.get(late).State)
	}
	// The second batch starts with the rows the first one did not reach.
	for _, id := range stale[settleBatch:] {
		if got := rc.asked[AppUserID(store.get(id).Referee)]; got != 1 {
			t.Fatalf("claim #%d (not in pass 1) looked at %d time(s), want once in pass 2", id, got)
		}
	}
	// … and the rest of it is the longest-unchecked of the first.
	if got := rc.asked[AppUserID(store.get(stale[0]).Referee)]; got != 2 {
		t.Errorf("oldest-checked claim looked at %d time(s), want 2", got)
	}
	if got := rc.asked[AppUserID(store.get(stale[settleBatch-1]).Referee)]; got != 1 {
		t.Errorf("last of pass 1 looked at %d time(s), want 1 (pass 2 is full before it)", got)
	}
}

func TestSettleClaimedReportedSinceTheLastLookComesFirst(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	settleAt(svc, store, base) // looked at once: nothing yet
	// A full batch of claims nobody has looked at piles in behind it …
	for i := 0; i < settleBatch; i++ {
		store.add(userA, friend(2000+i), StateClaimed, base.Add(time.Duration(i)*time.Second))
	}
	// … then B redeems and the app says so.
	bSubscribed(rc, base.Add(4*time.Minute))
	store.dueNow = base.Add(5 * time.Minute)
	if err := store.ReportRedeemed(context.Background(), userB, "OFFER"); err != nil {
		t.Fatal(err)
	}
	if rep := settleAt(svc, store, base.Add(10*time.Minute)); rep.Redeemed != 1 || store.get(id).State != StateRedeemed {
		t.Errorf("%+v state %s, want B (reported after its last look) in the batch ahead of never-looked-at rows", rep, store.get(id).State)
	}
}

func TestSettleFailedLookupIsNotStamped(t *testing.T) {
	svc, store, rc := newSettle()
	id := store.add(userA, userB, StateClaimed, base.Add(-time.Hour))
	rc.err = errors.New("503")
	settleAt(svc, store, base)
	if !store.refs[id].lastChecked.IsZero() {
		t.Error("a claim RevenueCat did not answer for was stamped as looked at")
	}
	rc.err = nil
	settleAt(svc, store, base.Add(10*time.Minute))
	if !store.refs[id].lastChecked.Equal(base.Add(10 * time.Minute)) {
		t.Errorf("last look = %v, want the pass time", store.refs[id].lastChecked)
	}
}

func TestSettleSaversBacklogRotates(t *testing.T) {
	svc, store, rc := newSettle()
	paying := withSubs(map[string]Subscription{premiumMonth: storeSub(base.Add(-day), base.Add(100*day))})
	// More still-paying savers than one batch holds.
	for i := 0; i < settleBatch+10; i++ {
		u := friend(3000 + i)
		ref := store.add(u, friend(5000+i), StateSaved, base.Add(-30*day))
		store.refs[ref].qualifiedAt = base.Add(-20*day + time.Duration(i)*time.Minute)
		rc.set(u, paying)
	}
	settleAt(svc, store, base)

	// A referrer saved later, whose paid plan has ended by now (A has no
	// store subscription at all on the fake).
	id := store.add(userA, userB, StateQualified, base.Add(-10*day))
	store.refs[id].qualifiedAt = base.Add(-day)
	store.refs[id].lastChecked = base.Add(-9 * day) // stamped back when it was claimed
	if err := store.MarkSaved(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !store.refs[id].lastChecked.IsZero() {
		t.Fatal("MarkSaved must clear the last look (it was the claim's, not the referrer's)")
	}
	rep := settleAt(svc, store, base.Add(10*time.Minute))
	if rep.Flushed != 1 || store.get(id).State != StateRewarded {
		t.Errorf("%+v state %s, want the new saver reached and paid", rep, store.get(id).State)
	}
}
