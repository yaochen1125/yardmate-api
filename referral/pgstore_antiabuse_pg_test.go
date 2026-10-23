package referral

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 016 (anti-abuse) against a real Postgres — same harness as
// pgstore_pg_test.go (REFERRAL_PG_TEST_URL, throwaway schema; skipped when
// unset; never point it at staging or prod).

const (
	userD    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	pgDevA   = "AAAAAAAA-1B6C-4F3E-9C2E-1D3A5F6B7C8D"
	pgDevB   = "BBBBBBBB-1B6C-4F3E-9C2E-1D3A5F6B7C8D"
	pgSubKey = "app_store:2026-10-20T00:00:00Z"
)

func pgReferralID(t *testing.T, pool *pgxpool.Pool, ctx context.Context, referee string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `select id from referrals where referee = $1`, referee).Scan(&id); err != nil {
		t.Fatalf("referral id of %s: %v", referee, err)
	}
	return id
}

func pgRefRow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id int64) (state, reason, key, install string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`select state, coalesce(void_reason, ''), coalesce(subscription_key, ''), coalesce(claim_install_id, '')
		   from referrals where id = $1`, id).Scan(&state, &reason, &key, &install); err != nil {
		t.Fatalf("referral %d: %v", id, err)
	}
	return
}

// pgCredit writes one reward_ledger row for a referral.
func pgCredit(t *testing.T, pool *pgxpool.Pool, ctx context.Context, refID int64, referrer, referee, txn, state string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`insert into reward_ledger (referral_id, referrer, referee, rc_event_id, event_type, transaction_id,
		                            period_type, price_usd_cents, takehome_percentage, credit_cents,
		                            purchased_at, available_at, state)
		 values ($1, $2, $3, 'ev-' || $4, 'RENEWAL', $4, 'NORMAL', 699, 0.85, 178, now(), now() + interval '90 days', $5)`,
		refID, referrer, referee, txn, state); err != nil {
		t.Fatalf("credit %s: %v", txn, err)
	}
}

func pgLedgerRow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, txn string) (state, reason string) {
	t.Helper()
	if err := pool.QueryRow(ctx, `select state, coalesce(void_reason, '') from reward_ledger where transaction_id = $1`, txn).
		Scan(&state, &reason); err != nil {
		t.Fatalf("ledger %s: %v", txn, err)
	}
	return
}

func TestPGMigration016IsRerunnable(t *testing.T) {
	_, pool, ctx := pgTest(t)
	sql, err := os.ReadFile(filepath.Join("..", "proxy", "enrichment", "migrations", "016_referral_antiabuse.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("second run of 016: %v", err)
	}
	var lim, win int
	if err := pool.QueryRow(ctx, `select velocity_limit, velocity_window_hours from referral_settings where id = 1`).Scan(&lim, &win); err != nil || lim != 5 || win != 24 {
		t.Errorf("velocity settings = %d / %d (%v), want 5 / 24", lim, win, err)
	}
	// held is a legal state now; nonsense still is not.
	if _, err := pool.Exec(ctx, `insert into referral_users (user_id, code) values ($1, 'YM-ZZZ2'), ($2, 'YM-ZZZ3')`, userA, userB); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into referrals (referrer, referee, state) values ($1, $2, 'held')`, userA, userB); err != nil {
		t.Errorf("held rejected by the state check: %v", err)
	}
	if _, err := pool.Exec(ctx, `update referrals set state = 'bogus'`); err == nil {
		t.Error("the state check accepts anything")
	}
}

func TestPGClaimJudgementOrderAndDevices(t *testing.T) {
	st, pool, ctx := pgTest(t)
	me, err := st.Me(ctx, userA, pgDevA)
	if err != nil {
		t.Fatal(err)
	}
	codeA := me.Code
	codeC := pgInviteCode(t, st, ctx, userC)
	pgSeed(t, pool, ctx, "b90", 90*day, "P1", "P2", "P3")

	check := func(user, code, install string) (ClaimKind, string) {
		k, err := st.ClaimCheck(ctx, user, code, install)
		if f, ok := named(err); ok {
			return k, f.Code
		}
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		return k, ""
	}
	if _, c := check(userB, "YM-2345", ""); c != ErrUnknownCode {
		t.Errorf("unknown code: %q", c)
	}
	if _, c := check(userA, codeA, ""); c != ErrOwnCode {
		t.Errorf("own code: %q", c)
	}
	// Gate 2: B on A's device (any case), A's code.
	for _, dev := range []string{pgDevA, "aaaaaaaa-1b6c-4f3e-9c2e-1d3a5f6b7c8d"} {
		if _, c := check(userB, codeA, dev); c != ErrOwnCode {
			t.Errorf("A's device %s: %q, want own_code", dev, c)
		}
		if _, err := st.Claim(ctx, userB, codeA, dev, nil); func() string { f, _ := named(err); return f.Code }() != ErrOwnCode {
			t.Errorf("claim on A's device: %v, want own_code from the atomic re-check", err)
		}
	}
	if k, c := check(userB, codeA, pgDevB); c != "" || k != ClaimNew {
		t.Errorf("B's own device: %v %q, want new", k, c)
	}
	var gotFresh []bool
	var checks int
	hook := &ClaimGate{
		Check: func() error { checks++; return nil },
		Set:   func(fresh bool) error { gotFresh = append(gotFresh, fresh); return nil },
	}
	offer, err := st.Claim(ctx, userB, codeA, pgDevB, hook)
	if err != nil || offer == "" {
		t.Fatalf("claim = %q, %v", offer, err)
	}
	id := pgReferralID(t, pool, ctx, userB)
	if _, _, _, inst := pgRefRow(t, pool, ctx, id); inst != "bbbbbbbb-1b6c-4f3e-9c2e-1d3a5f6b7c8d" {
		t.Errorf("claim_install_id = %q, want the lowercased claiming device", inst)
	}
	// Step 3 before step 4: B re-claiming A's code on A's device gets its code.
	if k, c := check(userB, codeA, pgDevA); c != "" || k != ClaimAgain {
		t.Errorf("re-claim on A's device: %v %q, want again", k, c)
	}
	if again, err := st.Claim(ctx, userB, codeA, pgDevA, hook); err != nil || again != offer {
		t.Errorf("re-claim = %q, %v; want %s", again, err, offer)
	}
	if len(gotFresh) != 2 || !gotFresh[0] || gotFresh[1] {
		t.Errorf("hook fresh = %v, want [true false]", gotFresh)
	}
	// Under the device lock the re-check says "again": Apple is not asked.
	if checks != 1 {
		t.Errorf("gate checks = %d, want 1 (the first claim only)", checks)
	}
	if _, c := check(userB, codeC, ""); c != ErrAlreadyClaimed {
		t.Errorf("another code: %q, want already_claimed", c)
	}
	// /me again from the same device: one row, last_seen moved.
	if _, err := st.Me(ctx, userA, pgDevA); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from referral_devices where user_id = $1`, userA).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("referral_devices rows = %d (%v), want 1", rows, err)
	}
}

func TestPGClaimHookFailureRollsBack(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "P1")
	boom := errors.New("set bit0 failed")
	if _, err := st.Claim(ctx, userB, codeA, pgDevB, &ClaimGate{Set: func(bool) error { return boom }}); !errors.Is(err, boom) {
		t.Fatalf("claim = %v, want the gate's error", err)
	}
	// A refusing check rolls back the same way, before any code is taken.
	used := Failure{Code: ErrDeviceUsed}
	if _, err := st.Claim(ctx, userB, codeA, pgDevB, &ClaimGate{Check: func() error { return used }}); !errors.Is(err, used) {
		t.Fatalf("claim = %v, want the check's refusal", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from referrals where referee = $1`, userB).Scan(&n); err != nil || n != 0 {
		t.Errorf("referrals after rollback = %d (%v), want 0", n, err)
	}
	if s := pgState(t, pool, ctx, "P1"); s != "free" {
		t.Errorf("P1 = %s after rollback, want free", s)
	}
	if offer, err := st.Claim(ctx, userB, codeA, pgDevB, nil); err != nil || offer != "P1" {
		t.Errorf("retry = %q, %v; want P1", offer, err)
	}
}

func TestPGVoidReferralNeverSwaps(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b20", 20*day, "OLD")
	pgSeed(t, pool, ctx, "b90", 90*day, "NEW")
	if _, err := st.Claim(ctx, userB, codeA, "", nil); err != nil {
		t.Fatal(err)
	}
	id := pgReferralID(t, pool, ctx, userB)
	if err := st.MarkVoid(ctx, id, VoidRejected); err != nil {
		t.Fatal(err)
	}
	pgExpiresIn(t, pool, ctx, "OLD", 2*day) // inside the swap grace
	// 022: already_claimed (016 / 017 handed OLD back); either way no swap.
	if offer, err := st.Claim(ctx, userB, codeA, "", nil); !isFailure(err, ErrAlreadyClaimed) {
		t.Errorf("re-claim of a void referral = %q, %v; want already_claimed (no swap)", offer, err)
	}
	if s := pgState(t, pool, ctx, "NEW"); s != "free" {
		t.Errorf("NEW = %s, want left on the shelf", s)
	}
}

func TestPGMarkRedeemedOneAppleIDOnce(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "P1", "P2", "P3")
	for _, u := range []string{userB, userC, userD} {
		if _, err := st.Claim(ctx, u, codeA, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	idB, idC, idD := pgReferralID(t, pool, ctx, userB), pgReferralID(t, pool, ctx, userC), pgReferralID(t, pool, ctx, userD)
	pgCredit(t, pool, ctx, idB, userA, userB, "tB", "pending")
	pgCredit(t, pool, ctx, idC, userA, userC, "tC1", "pending")
	pgCredit(t, pool, ctx, idC, userA, userC, "tC2", "available")
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

	if voided, err := st.MarkRedeemed(ctx, idB, at, pgSubKey); err != nil || voided {
		t.Fatalf("B: %v %v, want redeemed", voided, err)
	}
	if voided, err := st.MarkRedeemed(ctx, idC, at, pgSubKey); err != nil || !voided {
		t.Fatalf("C (same Apple ID): %v %v, want voided", voided, err)
	}
	if s, r, k, _ := pgRefRow(t, pool, ctx, idC); s != StateVoid || r != VoidDuplicate || k != pgSubKey {
		t.Errorf("C = %s %q %q", s, r, k)
	}
	if s, _, k, _ := pgRefRow(t, pool, ctx, idB); s != StateRedeemed || k != pgSubKey {
		t.Errorf("B = %s %q", s, k)
	}
	// C's money goes with it — pending AND available; B's is untouched.
	for txn, want := range map[string]string{"tC1": "voided", "tC2": "voided", "tB": "pending"} {
		if s, r := pgLedgerRow(t, pool, ctx, txn); s != want || (want == "voided" && r != VoidDuplicate) {
			t.Errorf("ledger %s = %s %q, want %s", txn, s, r, want)
		}
	}
	// C's pool code was never redeemed; B's was.
	var stB, stC string
	_ = pool.QueryRow(ctx, `select state from code_pool where referral_id = $1`, idB).Scan(&stB)
	_ = pool.QueryRow(ctx, `select state from code_pool where referral_id = $1`, idC).Scan(&stC)
	if stB != "redeemed" || stC != "issued" {
		t.Errorf("pool: B %s C %s, want redeemed / issued", stB, stC)
	}
	// Already decided → skipped, nothing changes.
	if voided, err := st.MarkRedeemed(ctx, idB, at, "app_store:other"); err != nil || voided {
		t.Errorf("re-mark: %v %v", voided, err)
	}

	// B deletes the account: the key is kept without a user id, and D (the
	// same Apple ID again) is still a duplicate.
	if err := st.DeleteUser(ctx, userB); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := pool.QueryRow(ctx, `select count(*) from referral_used_subscriptions where subscription_key = $1`, pgSubKey).Scan(&kept); err != nil || kept != 1 {
		t.Errorf("kept keys = %d (%v), want 1", kept, err)
	}
	if voided, err := st.MarkRedeemed(ctx, idD, at, pgSubKey); err != nil || !voided {
		t.Errorf("D after B's deletion: %v %v, want voided", voided, err)
	}
}

func TestPGQualifyVelocityHeldReviewAndDigest(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	if _, err := pool.Exec(ctx, `update referral_settings set velocity_limit = 2 where id = 1`); err != nil {
		t.Fatal(err)
	}
	pgSeed(t, pool, ctx, "b90", 90*day, "P1", "P2", "P3", "P4")
	friends := []string{userB, userC, userD}
	var ids []int64
	for i, u := range friends {
		if _, err := st.Claim(ctx, u, codeA, []string{pgDevA, pgDevB, pgDevB}[i], nil); err != nil {
			t.Fatal(err)
		}
		id := pgReferralID(t, pool, ctx, u)
		if _, err := st.MarkRedeemed(ctx, id, time.Now().Add(-8*day), "app_store:"+u); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var held []bool
	for _, id := range ids {
		h, err := st.MarkQualified(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, h)
	}
	if held[0] || held[1] || !held[2] {
		t.Fatalf("held = %v, want the third held (limit 2)", held)
	}
	me, err := st.Me(ctx, userA, "")
	if err != nil {
		t.Fatal(err)
	}
	if me.MonthsPending != 3 || me.Invited != 3 || me.MonthsEarned != 0 {
		t.Errorf("/me = %+v, want held counted in months_pending (3), earned 0", me)
	}

	ds, err := st.HeldDigests(ctx, 10)
	if err != nil || len(ds) != 1 || ds[0].Referrer != userA || len(ds[0].Referrals) != 3 || ds[0].Limit != 2 || ds[0].WindowHours != 24 {
		t.Fatalf("digests = %+v, %v", ds, err)
	}
	if got := ds[0].NewHeld(); len(got) != 1 || got[0] != ids[2] {
		t.Errorf("new held = %v, want [%d]", got, ids[2])
	}
	if d, r, m := deviceSpread(ds[0].Referrals); d != 2 || r != 3 || m != 0 {
		t.Errorf("devices = %d distinct of %d (missing %d), want 2 of 3", d, r, m)
	}
	if err := st.MarkHeldNotified(ctx, ds[0].NewHeld()); err != nil {
		t.Fatal(err)
	}
	if ds, err := st.HeldDigests(ctx, 10); err != nil || len(ds) != 0 {
		t.Errorf("after the mail: %+v %v, want none (24 h)", ds, err)
	}

	// approve → qualified + reviewed_at; a second approve does nothing.
	var out string
	if err := pool.QueryRow(ctx, `select referral_approve($1)::text`, ids[2]).Scan(&out); err != nil {
		t.Fatal(err)
	}
	if s, _, _, _ := pgRefRow(t, pool, ctx, ids[2]); s != StateQualified {
		t.Errorf("approved = %s (%s)", s, out)
	}
	var reviewed *time.Time
	_ = pool.QueryRow(ctx, `select reviewed_at from referrals where id = $1`, ids[2]).Scan(&reviewed)
	if reviewed == nil {
		t.Error("reviewed_at not set")
	}
	_ = pool.QueryRow(ctx, `select (referral_approve($1)->>'approved')`, ids[2]).Scan(&out)
	if out != "false" {
		t.Errorf("second approve = %s, want false", out)
	}

	// reject works on anything live (here a rewarded one) and takes the money.
	if _, err := pool.Exec(ctx, `update referrals set state = 'rewarded' where id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	pgCredit(t, pool, ctx, ids[0], userA, userB, "tR", "available")
	if err := pool.QueryRow(ctx, `select referral_reject($1)->>'previous_state'`, ids[0]).Scan(&out); err != nil || out != StateRewarded {
		t.Errorf("reject previous_state = %q, %v", out, err)
	}
	if s, r, _, _ := pgRefRow(t, pool, ctx, ids[0]); s != StateVoid || r != VoidRejected {
		t.Errorf("rejected = %s %q", s, r)
	}
	if s, r := pgLedgerRow(t, pool, ctx, "tR"); s != "voided" || r != VoidRejected {
		t.Errorf("rejected's money = %s %q, want voided rejected", s, r)
	}
	// A refund-voided referral keeps its money (the refund webhook voids the
	// refunded charge itself).
	pgCredit(t, pool, ctx, ids[1], userA, userC, "tK", "pending")
	if err := st.MarkVoid(ctx, ids[1], VoidRefunded); err != nil {
		t.Fatal(err)
	}
	if s, _ := pgLedgerRow(t, pool, ctx, "tK"); s != "pending" {
		t.Errorf("refunded referral's other money = %s, want left alone", s)
	}
}

func TestPGVelocityWindowAndOff(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "P1", "P2", "P3")
	if _, err := pool.Exec(ctx, `update referral_settings set velocity_limit = 1 where id = 1`); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, u := range []string{userB, userC, userD} {
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
		t.Fatalf("first: %v %v", h, err)
	}
	// The first success's theoretical time (redeemed_at + hold_days, 017) is
	// 25 hours before the second's: outside the window — however close
	// together the two are actually judged.
	if _, err := pool.Exec(ctx, `update referrals set redeemed_at = redeemed_at - interval '25 hours' where id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if h, err := st.MarkQualified(ctx, ids[1]); err != nil || h {
		t.Errorf("second (first outside the window): held=%v %v, want qualified", h, err)
	}
	if _, err := pool.Exec(ctx, `update referral_settings set velocity_limit = 0 where id = 1`); err != nil {
		t.Fatal(err)
	}
	if h, err := st.MarkQualified(ctx, ids[2]); err != nil || h {
		t.Errorf("limit 0: held=%v %v, want the gate off", h, err)
	}
}
