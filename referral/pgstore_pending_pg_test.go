package referral

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 022: referral_pending_offer (GET /me hands a claimed-but-unredeemed code
// back so a reinstall / new phone can redeem it) and re-claims of a referral
// that has left claimed answering already_claimed.

// isFailure reports whether err is the named refusal code.
func isFailure(err error, code string) bool {
	f, ok := named(err)
	return ok && f.Code == code
}

func TestPGPendingOfferHandsBackTheClaimedCode(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "D90")
	pgSeed(t, pool, ctx, "b120", 120*day, "D120")

	// Never claimed: nothing.
	if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p != nil {
		t.Fatalf("unclaimed: %+v, %v; want nil", p, err)
	}
	if offer, err := st.Claim(ctx, userB, codeA, "", nil); err != nil || offer != "D90" {
		t.Fatalf("claim = %q, %v", offer, err)
	}
	// Claimed, code good for months: the same code and the friend's code.
	p, err := st.PendingOffer(ctx, userB, "")
	if err != nil || p == nil || p.OfferCode != "D90" || p.FriendCode != codeA {
		t.Fatalf("claimed: %+v, %v; want D90 / %s", p, err, codeA)
	}
	if s := pgState(t, pool, ctx, "D120"); s != "free" {
		t.Errorf("D120 = %s, want free (nothing swapped)", s)
	}
	// Only ever the caller's own: the referrer asking gets nothing.
	if p, err := st.PendingOffer(ctx, userA, ""); err != nil || p != nil {
		t.Errorf("referrer: %+v, %v; want nil", p, err)
	}

	// Inside the swap grace: swapped like a re-claim, old one retired.
	pgExpiresIn(t, pool, ctx, "D90", 2*day)
	p, err = st.PendingOffer(ctx, userB, "")
	if err != nil || p == nil || p.OfferCode != "D120" {
		t.Fatalf("dying code: %+v, %v; want swapped to D120", p, err)
	}
	if s := pgState(t, pool, ctx, "D90"); s != "expired" {
		t.Errorf("D90 = %s, want expired", s)
	}
	if offer, _ := pgReferral(t, pool, ctx, userB); offer != "D120" {
		t.Errorf("referral offer = %s, want D120", offer)
	}

	// Redeemed at Apple (pool says so) before settlement moved the referral:
	// nothing to hand back.
	if _, err := pool.Exec(ctx, `update code_pool set state = 'redeemed' where code = 'D120'`); err != nil {
		t.Fatal(err)
	}
	if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p != nil {
		t.Errorf("pool redeemed: %+v, %v; want nil", p, err)
	}
	if _, err := pool.Exec(ctx, `update code_pool set state = 'issued' where code = 'D120'`); err != nil {
		t.Fatal(err)
	}

	// Referral left claimed: nothing, and a re-claim is already_claimed.
	for _, state := range []string{StateRedeemed, StateVoid} {
		if _, err := pool.Exec(ctx, `update referrals set state = $2 where referee = $1`, userB, state); err != nil {
			t.Fatal(err)
		}
		if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p != nil {
			t.Errorf("%s: %+v, %v; want nil", state, p, err)
		}
	}
}

func TestPGReportedRedeemKeepsTheCodeUntilItExpires(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "D90")
	pgSeed(t, pool, ctx, "b120", 120*day, "D120")
	if offer, err := st.Claim(ctx, userB, codeA, "", nil); err != nil || offer != "D90" {
		t.Fatalf("claim = %q, %v", offer, err)
	}
	if err := st.ReportRedeemed(ctx, userB, "D90"); err != nil {
		t.Fatal(err)
	}
	// The app said it redeemed: a dying (not dead) code is kept by /me and by
	// a re-claim alike — it may be redeemed, settlement just has not seen it.
	pgExpiresIn(t, pool, ctx, "D90", 2*day)
	if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p == nil || p.OfferCode != "D90" {
		t.Fatalf("reported + dying: %+v, %v; want D90 kept", p, err)
	}
	if offer, err := st.Claim(ctx, userB, codeA, "", nil); err != nil || offer != "D90" {
		t.Fatalf("reported + dying re-claim = %q, %v; want D90 kept", offer, err)
	}
	if s := pgState(t, pool, ctx, "D120"); s != "free" {
		t.Errorf("D120 = %s, want free (not burnt)", s)
	}
	// Dead: swapped after all (a reinstall has no friend's code to retry with).
	pgExpiresIn(t, pool, ctx, "D90", -time.Hour)
	if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p == nil || p.OfferCode != "D120" {
		t.Fatalf("reported + dead: %+v, %v; want swapped to D120", p, err)
	}
}

func TestPGPendingOfferPoolEmptyAndDeadIsNothing(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "D90")
	if _, err := st.Claim(ctx, userB, codeA, "", nil); err != nil {
		t.Fatal(err)
	}
	// Dying, nothing to swap to: the old one while it still works...
	pgExpiresIn(t, pool, ctx, "D90", 2*day)
	if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p == nil || p.OfferCode != "D90" {
		t.Fatalf("dying, empty pool: %+v, %v; want D90", p, err)
	}
	// ...and nothing once it is dead (pool_empty swallowed, not an error).
	pgExpiresIn(t, pool, ctx, "D90", -time.Hour)
	if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p != nil {
		t.Errorf("dead, empty pool: %+v, %v; want nil", p, err)
	}
}

func TestPGMigration022IsRerunnable(t *testing.T) {
	st, pool, ctx := pgTest(t) // 013 → 017 + 022 applied once
	pgApply(t, pool, ctx, "022_referral_pending_offer.sql")
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b90", 90*day, "D90")
	if _, err := st.Claim(ctx, userB, codeA, "", nil); err != nil {
		t.Fatal(err)
	}
	if p, err := st.PendingOffer(ctx, userB, ""); err != nil || p == nil {
		t.Errorf("after rerun: %+v, %v", p, err)
	}
}

func pgApply(t *testing.T, pool *pgxpool.Pool, ctx context.Context, file string) {
	t.Helper()
	sql, err := os.ReadFile(filepath.Join("..", "proxy", "enrichment", "migrations", file))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}
