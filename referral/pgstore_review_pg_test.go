package referral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Migration 017 (review fixes) against a real Postgres — same harness as
// pgstore_pg_test.go (REFERRAL_PG_TEST_URL, throwaway schema; skipped when
// unset; never point it at staging or prod).

func TestPGMigration017IsRerunnable(t *testing.T) {
	_, pool, ctx := pgTest(t) // 013 → 017 applied once
	sql, err := os.ReadFile(filepath.Join("..", "proxy", "enrichment", "migrations", "017_referral_review_fixes.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("second run of 017: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`select count(*) from information_schema.columns
		  where table_schema = current_schema() and table_name = 'referrals' and column_name = 'last_checked_at'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("referrals.last_checked_at = %d (%v), want the column", n, err)
	}
	// Every signature an older binary calls is still there (013 / 014 / 016).
	for _, sig := range []string{
		"referral_me(text)", "referral_me(text,text)",
		"referral_claim(text,text)", "referral_claim(text,text,text)", "referral_claim_check(text,text,text)",
		"referral_mark_redeemed(bigint,timestamp with time zone,text)", "referral_qualify(bigint)",
		"referral_void(bigint,text)", "referral_approve(bigint)", "referral_reject(bigint)",
	} {
		var ok bool
		if err := pool.QueryRow(ctx, `select to_regprocedure($1) is not null`, sig).Scan(&ok); err != nil || !ok {
			t.Errorf("function %s missing after 017 (%v)", sig, err)
		}
	}
}

// referees lists the referee of each referral, in order.
func referees(refs []Referral) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Referee)
	}
	return out
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPGListClaimedRotatesPastABatch(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "P1", "P2", "P3")
	for _, u := range []string{userB, userC, userD} { // claimed_at B < C < D
		if _, err := st.Claim(ctx, u, codeA, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListClaimed(ctx, 2)
	if err != nil || !sameList(referees(first), []string{userB, userC}) {
		t.Fatalf("batch 1 = %v, %v; want B, C (never looked at, oldest claim first)", referees(first), err)
	}
	// A little in the past so the database clock's /redeemed stamp below is
	// surely later.
	looked := time.Now().Add(-time.Second)
	for _, r := range first {
		if err := st.MarkClaimedChecked(ctx, r.ID, looked); err != nil {
			t.Fatal(err)
		}
	}
	// The second batch starts with D, which the first did not reach.
	second, err := st.ListClaimed(ctx, 2)
	if err != nil || !sameList(referees(second), []string{userD, userB}) {
		t.Fatalf("batch 2 = %v, %v; want D (never looked at) then B (looked at longest ago)", referees(second), err)
	}
	// C says /redeemed after its look: first in line again.
	offerC, _ := pgReferral(t, pool, ctx, userC)
	if err := st.ReportRedeemed(ctx, userC, offerC); err != nil {
		t.Fatal(err)
	}
	if third, err := st.ListClaimed(ctx, 1); err != nil || !sameList(referees(third), []string{userC}) {
		t.Errorf("after C's report = %v, %v; want C", referees(third), err)
	}
	// A report on a referral that is no longer claimed changes nothing.
	idB := pgReferralID(t, pool, ctx, userB)
	if _, err := st.MarkRedeemed(ctx, idB, time.Now().Add(-time.Hour), "app_store:b"); err != nil {
		t.Fatal(err)
	}
	offerB, _ := pgReferral(t, pool, ctx, userB)
	if err := st.ReportRedeemed(ctx, userB, offerB); err != nil {
		t.Fatal(err)
	}
	var reported *time.Time
	if err := pool.QueryRow(ctx, `select redeem_reported_at from referrals where id = $1`, idB).Scan(&reported); err != nil || reported != nil {
		t.Errorf("redeemed B's report stamp = %v (%v), want none", reported, err)
	}
	// MarkClaimedChecked leaves a non-claimed referral alone.
	if err := st.MarkClaimedChecked(ctx, idB, time.Now()); err != nil {
		t.Fatal(err)
	}
	var checked *time.Time
	if err := pool.QueryRow(ctx, `select last_checked_at from referrals where id = $1`, idB).Scan(&checked); err != nil || checked == nil || !checked.Equal(looked.Truncate(time.Microsecond)) {
		t.Errorf("redeemed B's last look = %v (%v), want still the claimed-time stamp %v", checked, err, looked)
	}
}

func TestPGListSaversRotatesAndMarkSavedClears(t *testing.T) {
	st, pool, ctx := pgTest(t)
	// Three referrers with one saved referral each, qualified A, C, D (oldest
	// first); B is everyone's friend row.
	for _, u := range []string{userA, userB, userC, userD} {
		pgInviteCode(t, st, ctx, u)
	}
	friends := map[string]string{userA: "f-a", userC: "f-c", userD: "f-d"}
	for i, u := range []string{userA, userC, userD} {
		pgInviteCode(t, st, ctx, friends[u])
		if _, err := pool.Exec(ctx,
			`insert into referrals (referrer, referee, state, redeemed_at, qualified_at)
			 values ($1, $2, 'saved', now() - interval '20 days', now() - make_interval(hours => $3))`,
			u, friends[u], 10-i); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListSavers(ctx, 2)
	if err != nil || !sameList(first, []string{userA, userC}) {
		t.Fatalf("batch 1 = %v, %v; want A, C", first, err)
	}
	for _, u := range first {
		if err := st.MarkSaverChecked(ctx, u, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if second, err := st.ListSavers(ctx, 2); err != nil || !sameList(second, []string{userD, userA}) {
		t.Fatalf("batch 2 = %v, %v; want D (never looked at) then A", second, err)
	}
	// C gets a newly saved referral: MarkSaved clears the stamp ① left on
	// it, so C is early again.
	if _, err := pool.Exec(ctx,
		`insert into referrals (referrer, referee, state, redeemed_at, qualified_at, last_checked_at)
		 values ($1, $2, 'qualified', now() - interval '8 days', now(), now() - interval '9 days')`, userC, userB); err != nil {
		t.Fatal(err)
	}
	id := pgReferralID(t, pool, ctx, userB)
	if err := st.MarkSaved(ctx, id); err != nil {
		t.Fatal(err)
	}
	var checked *time.Time
	if err := pool.QueryRow(ctx, `select last_checked_at from referrals where id = $1`, id).Scan(&checked); err != nil || checked != nil {
		t.Errorf("last look after MarkSaved = %v (%v), want cleared", checked, err)
	}
	if third, err := st.ListSavers(ctx, 1); err != nil || !sameList(third, []string{userC}) {
		t.Errorf("after C's new saved referral = %v, %v; want C first", third, err)
	}
}

// MARK: gate 1 under the device lock

func TestPGGatedClaimsOnOneDeviceAreSerialised(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	const n = 6
	codes := make([]string, n)
	for i := range codes {
		codes[i] = fmt.Sprintf("Q%d", i)
	}
	pgSeed(t, pool, ctx, "b90", 90*day, codes...)
	// One device's bit0 at "Apple", shared by every claim; each Apple call
	// takes a moment, so without the lock all of them would check before any
	// of them sets.
	var mu sync.Mutex
	bit := false
	gate := func() *ClaimGate {
		return &ClaimGate{
			Check: func() error {
				mu.Lock()
				used := bit
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				if used {
					return Failure{Code: ErrDeviceUsed}
				}
				return nil
			},
			Set: func(fresh bool) error {
				time.Sleep(20 * time.Millisecond)
				if fresh {
					mu.Lock()
					bit = true
					mu.Unlock()
				}
				return nil
			},
		}
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A different account and a forged install id each.
			_, errs[i] = st.Claim(ctx, fmt.Sprintf("friend-%d", i), codeA, fmt.Sprintf("dev-%d", i), gate())
		}(i)
	}
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, Failure{Code: ErrDeviceUsed}):
			t.Errorf("claim %d: %v, want nil or device_used", i, err)
		}
	}
	if ok != 1 {
		t.Errorf("%d claims got a code, want exactly 1", ok)
	}
	var issued, refs int
	_ = pool.QueryRow(ctx, `select count(*) from code_pool where state = 'issued'`).Scan(&issued)
	_ = pool.QueryRow(ctx, `select count(*) from referrals`).Scan(&refs)
	if issued != 1 || refs != 1 {
		t.Errorf("issued %d referrals %d, want 1 / 1 (the refused ones rolled back)", issued, refs)
	}
}

func TestPGGatedClaimGivesUpOnABusyDeviceLock(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "P1")
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, deviceLockSQL); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	// A deadline of our own, so a lock_timeout that did not apply fails the
	// test instead of hanging it.
	cctx, cancel := context.WithTimeout(ctx, deviceLockWait+10*time.Second)
	defer cancel()
	_, err = st.Claim(cctx, userB, codeA, "", &ClaimGate{})
	if !errors.Is(err, Failure{Code: ErrDeviceUnavailable}) {
		t.Fatalf("claim behind a held lock = %v, want device_check_unavailable", err)
	}
	if took := time.Since(start); took < deviceLockWait || took > deviceLockWait+5*time.Second {
		t.Errorf("gave up after %v, want about %v (lock_timeout)", took, deviceLockWait)
	}
	_ = holder.Rollback(ctx)
	// Nothing was taken.
	if s := pgState(t, pool, ctx, "P1"); s != "free" {
		t.Errorf("P1 = %s, want free", s)
	}
}

// MARK: approved out of held → ③ re-checks the refund

func TestPGListQualifiedSaysWhichWereReviewed(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "P1", "P2")
	if _, err := pool.Exec(ctx, `update referral_settings set velocity_limit = 1 where id = 1`); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, u := range []string{userB, userC} {
		if _, err := st.Claim(ctx, u, codeA, "", nil); err != nil {
			t.Fatal(err)
		}
		id := pgReferralID(t, pool, ctx, u)
		if _, err := st.MarkRedeemed(ctx, id, time.Now().Add(-8*day), "app_store:"+u); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if h, err := st.MarkQualified(ctx, ids[0]); err != nil || h {
		t.Fatalf("first: held=%v %v", h, err)
	}
	if h, err := st.MarkQualified(ctx, ids[1]); err != nil || !h {
		t.Fatalf("second: held=%v %v, want held (limit 1)", h, err)
	}
	if _, err := pool.Exec(ctx, `select referral_approve($1)`, ids[1]); err != nil {
		t.Fatal(err)
	}
	q, err := st.ListQualified(ctx, 10)
	if err != nil || len(q) != 2 {
		t.Fatalf("qualified = %+v, %v", q, err)
	}
	for _, r := range q {
		if want := r.ID == ids[1]; r.Reviewed != want {
			t.Errorf("referral %d Reviewed = %v, want %v (only the approved one)", r.ID, r.Reviewed, want)
		}
	}
	// The re-check voids it like ② would: refunded touches only the referral.
	if err := st.MarkVoid(ctx, ids[1], VoidRefunded); err != nil {
		t.Fatal(err)
	}
	if s, r, _, _ := pgRefRow(t, pool, ctx, ids[1]); s != StateVoid || r != VoidRefunded {
		t.Errorf("approved then refunded = %s %q, want void refunded", s, r)
	}
}

// MARK: re-claim once the referral has left claimed

func TestPGReclaimAfterRedeemingNeverBurnsAnotherCode(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b20", 20*day, "OLD")
	pgSeed(t, pool, ctx, "b90", 90*day, "NEW")
	if offer, err := st.Claim(ctx, userB, codeA, "", nil); err != nil || offer != "OLD" {
		t.Fatalf("claim = %q, %v; want OLD (earliest beyond the margin)", offer, err)
	}
	id := pgReferralID(t, pool, ctx, userB)
	// B redeemed OLD at Apple just before it expired; ExpireCodes got to it
	// before settlement ① did, so the pool row says expired.
	pgExpiresIn(t, pool, ctx, "OLD", -time.Hour)
	if _, err := st.ExpireCodes(ctx); err != nil {
		t.Fatal(err)
	}
	if voided, err := st.MarkRedeemed(ctx, id, time.Now().Add(-2*time.Hour), "app_store:b"); err != nil || voided {
		t.Fatalf("mark redeemed: %v %v", voided, err)
	}
	if s := pgState(t, pool, ctx, "OLD"); s != "expired" {
		t.Fatalf("OLD = %s, want expired (the case under test)", s)
	}
	// The app re-opens the code sheet (an automatic re-claim) at every later
	// state: already_claimed (022; 017 handed OLD back), NEW stays on the shelf.
	for _, state := range []string{StateRedeemed, StateQualified, StateHeld, StateSaved, StateRewarded, StateVoid} {
		if _, err := pool.Exec(ctx, `update referrals set state = $2 where id = $1`, id, state); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimCheck(ctx, userB, codeA, ""); !isFailure(err, ErrAlreadyClaimed) {
			t.Fatalf("%s: check err = %v, want already_claimed", state, err)
		}
		if offer, err := st.Claim(ctx, userB, codeA, "", nil); !isFailure(err, ErrAlreadyClaimed) {
			t.Errorf("%s: re-claim = %q, %v; want already_claimed", state, offer, err)
		}
	}
	if s := pgState(t, pool, ctx, "NEW"); s != "free" {
		t.Errorf("NEW = %s, want still free (no code burnt)", s)
	}
	if offer, _ := pgReferral(t, pool, ctx, userB); offer != "OLD" {
		t.Errorf("referral offer = %s, want OLD", offer)
	}
	if r := pgReissued(t, pool, ctx, userB); r != nil {
		t.Errorf("reissued_at = %v, want never", r)
	}

	// Still claimed with a dying code is the one case that swaps (014).
	if _, err := st.Claim(ctx, userC, codeA, "", nil); err != nil {
		t.Fatal(err)
	}
	offerC, _ := pgReferral(t, pool, ctx, userC)
	pgExpiresIn(t, pool, ctx, offerC, 2*day) // inside the 3-day swap grace
	pgSeed(t, pool, ctx, "b60", 60*day, "SPARE")
	if again, err := st.Claim(ctx, userC, codeA, "", nil); err != nil || again == offerC {
		t.Errorf("claimed + dying code: re-claim = %q, %v; want a swap", again, err)
	}
}

// MARK: velocity gate on theoretical success time

func TestPGVelocityCountsTheoreticalSuccessTime(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	if _, err := pool.Exec(ctx, `update referral_settings set velocity_limit = 2 where id = 1`); err != nil {
		t.Fatal(err)
	}
	pgSeed(t, pool, ctx, "b90", 90*day, "P1", "P2", "P3", "P4", "P5", "P6", "P7")
	claimRedeemed := func(user string, redeemedAgo time.Duration) int64 {
		t.Helper()
		if _, err := st.Claim(ctx, user, codeA, "", nil); err != nil {
			t.Fatal(err)
		}
		id := pgReferralID(t, pool, ctx, user)
		if _, err := st.MarkRedeemed(ctx, id, time.Now().Add(-redeemedAgo), "app_store:"+user); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// A backlog after an outage: four friends who redeemed 26 hours apart
	// (oldest first, as ListDue hands them over), all judged right now.
	var backlog []int64
	for i, u := range []string{"bk-0", "bk-1", "bk-2", "bk-3"} {
		backlog = append(backlog, claimRedeemed(u, 8*day+time.Duration(3-i)*26*time.Hour))
	}
	for _, id := range backlog {
		if h, err := st.MarkQualified(ctx, id); err != nil || h {
			t.Errorf("backlog %d: held=%v %v, want qualified (26 h apart in theory)", id, h, err)
		}
	}
	// A burst judged late: three friends within the same hour, fifteen days
	// ago (a week after their hold ended; clear of the backlog's 8–11¼ days).
	// The third is held.
	var burst []int64
	for i, u := range []string{"bu-0", "bu-1", "bu-2"} {
		burst = append(burst, claimRedeemed(u, 15*day-time.Duration(i)*10*time.Minute))
	}
	var held []bool
	for _, id := range burst {
		h, err := st.MarkQualified(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, h)
	}
	if held[0] || held[1] || !held[2] {
		t.Errorf("burst held = %v, want the third held", held)
	}
	// The digest lists the held one and the two that counted against it —
	// not the backlog, judged at the same moment but days apart in theory.
	ds, err := st.HeldDigests(ctx, 10)
	if err != nil || len(ds) != 1 {
		t.Fatalf("digests = %+v, %v", ds, err)
	}
	var got []int64
	for _, r := range ds[0].Referrals {
		got = append(got, r.ID)
		if r.DueAt.IsZero() {
			t.Errorf("referral %d: no theoretical success time", r.ID)
		}
	}
	if len(got) != 3 || got[0] != burst[0] || got[1] != burst[1] || got[2] != burst[2] {
		t.Errorf("digest = %v, want the burst %v in theoretical order", got, burst)
	}
	// hold_days moves the theoretical time, not who counts.
	var due time.Time
	_ = pool.QueryRow(ctx, `select redeemed_at + interval '7 days' from referrals where id = $1`, burst[0]).Scan(&due)
	if !ds[0].Referrals[0].DueAt.Equal(due) {
		t.Errorf("DueAt = %v, want redeemed_at + hold_days = %v", ds[0].Referrals[0].DueAt, due)
	}
}
