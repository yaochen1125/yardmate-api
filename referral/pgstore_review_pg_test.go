package referral

import (
	"os"
	"path/filepath"
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
