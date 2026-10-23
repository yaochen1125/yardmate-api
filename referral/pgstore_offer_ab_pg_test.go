package referral

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 023: the friend's free month split into a monthly and a yearly group
// (invite-friends.md ### 兑换码分组实验（月付组 / 年付组））.

const (
	abMonth = "Referral Friend Month"
	abYear  = "Referral Friend Year"
	day90   = 90 * 24 * time.Hour
)

func pgSetYearPct(t *testing.T, pool *pgxpool.Pool, ctx context.Context, pct int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `select referral_set_offer_year_pct($1)`, pct); err != nil {
		t.Fatalf("set pct %d: %v", pct, err)
	}
}

func pgVariantOf(t *testing.T, pool *pgxpool.Pool, ctx context.Context, referee string) *string {
	t.Helper()
	var v *string
	if err := pool.QueryRow(ctx, `select offer_variant from referrals where referee = $1`, referee).Scan(&v); err != nil {
		t.Fatalf("variant of %s: %v", referee, err)
	}
	return v
}

func pgPoolName(t *testing.T, pool *pgxpool.Pool, ctx context.Context, code string) string {
	t.Helper()
	var n string
	if err := pool.QueryRow(ctx, `select offer_ref_name from code_pool where code = $1`, code).Scan(&n); err != nil {
		t.Fatalf("pool name of %s: %v", code, err)
	}
	return n
}

func friendID(i int) string { return fmt.Sprintf("ab%06d-0000-0000-0000-000000000000", i) }

func TestPGOfferVariantFollowsTheShareAndSticks(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abMonth, day90, "M1", "M2", "M3")
	pgSeed(t, pool, ctx, abYear, day90, "Y1", "Y2")

	// Default 0: monthly, a monthly code, and the store says month.
	offer, err := st.Claim(ctx, friendID(1), codeA, "", nil)
	if err != nil || pgPoolName(t, pool, ctx, offer) != abMonth {
		t.Fatalf("claim at 0%%: %q %v (pool %q)", offer, err, pgPoolName(t, pool, ctx, offer))
	}
	if v, err := st.OfferVariant(ctx, friendID(1)); err != nil || v != VariantMonth {
		t.Fatalf("variant at 0%% = %q %v", v, err)
	}

	// 100: yearly, a yearly code.
	pgSetYearPct(t, pool, ctx, 100)
	offer, err = st.Claim(ctx, friendID(2), codeA, "", nil)
	if err != nil || pgPoolName(t, pool, ctx, offer) != abYear {
		t.Fatalf("claim at 100%%: %q %v", offer, err)
	}
	if v, _ := st.OfferVariant(ctx, friendID(2)); v != VariantYear {
		t.Fatalf("variant at 100%% = %q", v)
	}

	// Back to 0: the yearly friend's re-claim (and a forced swap) stays yearly.
	pgSetYearPct(t, pool, ctx, 0)
	if again, err := st.Claim(ctx, friendID(2), codeA, "", nil); err != nil || again != offer {
		t.Fatalf("re-claim = %q %v, want the same %q", again, err, offer)
	}
	pgExpiresIn(t, pool, ctx, offer, time.Hour) // inside the swap grace → swap
	swapped, err := st.Claim(ctx, friendID(2), codeA, "", nil)
	if err != nil || swapped == offer || pgPoolName(t, pool, ctx, swapped) != abYear {
		t.Fatalf("swap = %q %v (pool %q), want another yearly code", swapped, err, pgPoolName(t, pool, ctx, swapped))
	}
	if v := pgVariantOf(t, pool, ctx, friendID(2)); v == nil || *v != VariantYear {
		t.Fatalf("variant after swap = %v", v)
	}
}

func TestPGOfferVariantGroupsNeverLendCodes(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abMonth, day90, "M1", "M2")
	pgSetYearPct(t, pool, ctx, 100)
	if _, err := st.Claim(ctx, friendID(1), codeA, "", nil); err == nil || !strings.Contains(err.Error(), ErrPoolEmpty) {
		t.Fatalf("yearly friend with only monthly codes: %v, want pool_empty", err)
	}
	// Nothing written: the referral rolled back with the error.
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from referrals where referee = $1`, friendID(1)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("referrals for the refused friend = %d %v", n, err)
	}
	// An alias batch (not the yearly name) counts as monthly.
	pgSetYearPct(t, pool, ctx, 0)
	pgSeed(t, pool, ctx, abYear, day90, "Y1")
	if _, err := pool.Exec(ctx, `update code_pool set state = 'expired' where offer_ref_name = $1`, abMonth); err != nil {
		t.Fatal(err)
	}
	pgSeed(t, pool, ctx, "Referral Friend Month (staging test)", day90, "S1", "S2")
	offer, err := st.Claim(ctx, friendID(2), codeA, "", nil)
	if err != nil || (offer != "S1" && offer != "S2") {
		t.Fatalf("monthly friend = %q %v, want an alias batch code (never the yearly Y1)", offer, err)
	}
	ps, err := st.PoolStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ps.UnknownOfferNames(); len(got) != 1 || got[0] != "Referral Friend Month (staging test)" {
		t.Fatalf("unknown names = %v", got)
	}
}

func TestPGOfferVariantHashSplitsAboutEvenly(t *testing.T) {
	_, pool, ctx := pgTest(t)
	pgSetYearPct(t, pool, ctx, 50)
	var year int
	if err := pool.QueryRow(ctx,
		`select count(*) from generate_series(1, 2000) g
		  where referral_offer_variant('user-' || g) = 'year'`).Scan(&year); err != nil {
		t.Fatal(err)
	}
	if year < 880 || year > 1120 {
		t.Fatalf("yearly = %d of 2000 at 50%%, want about half", year)
	}
	var a, b string
	if err := pool.QueryRow(ctx, `select referral_offer_variant('same'), referral_offer_variant('same')`).Scan(&a, &b); err != nil || a != b {
		t.Fatalf("same user twice = %q / %q %v", a, b, err)
	}
}

func TestPGOfferVariantLegacyIsMonthly(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abMonth, day90, "M1", "M2")
	pgSeed(t, pool, ctx, abYear, day90, "Y1")
	if _, err := st.Claim(ctx, friendID(1), codeA, "", nil); err != nil {
		t.Fatal(err)
	}
	// A referral from before 023.
	if _, err := pool.Exec(ctx, `update referrals set offer_variant = null where referee = $1`, friendID(1)); err != nil {
		t.Fatal(err)
	}
	pgSetYearPct(t, pool, ctx, 100)
	if v, _ := st.OfferVariant(ctx, friendID(1)); v != VariantMonth {
		t.Fatalf("legacy variant = %q", v)
	}
	offer, _ := pgReferral(t, pool, ctx, friendID(1))
	pgExpiresIn(t, pool, ctx, offer, time.Hour)
	swapped, err := st.Claim(ctx, friendID(1), codeA, "", nil)
	if err != nil || pgPoolName(t, pool, ctx, swapped) != abMonth {
		t.Fatalf("legacy swap = %q %v, want a monthly code", swapped, err)
	}
	if v := pgVariantOf(t, pool, ctx, friendID(1)); v == nil || *v != VariantMonth {
		t.Fatalf("legacy swap left variant %v, want month written", v)
	}
}

func TestPGPendingOfferCarriesTheVariant(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abYear, day90, "Y1")
	pgSetYearPct(t, pool, ctx, 100)
	if _, err := st.Claim(ctx, friendID(1), codeA, "", nil); err != nil {
		t.Fatal(err)
	}
	p, err := st.PendingOffer(ctx, friendID(1), "")
	if err != nil || p == nil || p.OfferCode != "Y1" || p.OfferVariant != VariantYear {
		t.Fatalf("pending = %+v %v", p, err)
	}
}

func TestPGSetOfferYearPctRecordsTheStartOnce(t *testing.T) {
	_, pool, ctx := pgTest(t)
	started := func() *time.Time {
		var s *time.Time
		if err := pool.QueryRow(ctx, `select offer_ab_started_at from referral_settings where id = 1`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	pgSetYearPct(t, pool, ctx, 0)
	if started() != nil {
		t.Fatal("0 started the experiment")
	}
	pgSetYearPct(t, pool, ctx, 30)
	first := started()
	if first == nil {
		t.Fatal("30 did not start it")
	}
	pgSetYearPct(t, pool, ctx, 0)
	pgSetYearPct(t, pool, ctx, 70)
	if s := started(); s == nil || !s.Equal(*first) {
		t.Fatalf("start moved: %v → %v", first, s)
	}
	if _, err := pool.Exec(ctx, `select referral_set_offer_year_pct(101)`); err == nil {
		t.Fatal("101 accepted")
	}
}

func TestPGSwitchUnredeemedAllOrNothing(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abMonth, day90, "M1", "M2", "M3", "M4")
	for i := 1; i <= 4; i++ { // share 0: everyone monthly
		if _, err := st.Claim(ctx, friendID(i), codeA, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// 1 redeemed (never moved); 2 legacy / old app build (offer_year_pct_at_claim null: an old build shows
	// "a month" for any code, so never moved); 3 plain monthly from a new build (moved); 4 reported /redeemed
	// (may already be redeemed: skipped).
	if _, err := pool.Exec(ctx, `update code_pool set state = 'redeemed' where code = (select offer_code from referrals where referee = $1)`, friendID(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update referrals set offer_variant = null, offer_year_pct_at_claim = null where referee = $1`, friendID(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update referrals set redeem_reported_at = now() where referee = $1`, friendID(4)); err != nil {
		t.Fatal(err)
	}
	switchTo := func(execute bool) (map[string]int, error) {
		var raw []byte
		if err := pool.QueryRow(ctx, `select referral_switch_unredeemed('year', $1)::text`, execute).Scan(&raw); err != nil {
			return nil, err
		}
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		m := map[string]int{}
		for k, v := range out {
			if f, ok := v.(float64); ok {
				m[k] = int(f)
			}
		}
		return m, nil
	}
	dry, err := switchTo(false)
	if err != nil || dry["candidates"] != 1 || dry["available"] != 0 {
		t.Fatalf("dry run = %v %v, want 1 candidate (friend 3) and 0 yearly codes", dry, err)
	}
	if _, err := switchTo(true); err == nil || !strings.Contains(err.Error(), "not_enough_codes") {
		t.Fatalf("execute with no yearly codes: %v", err)
	}
	if v := pgVariantOf(t, pool, ctx, friendID(3)); v == nil || *v != VariantMonth {
		t.Fatalf("friend 3 moved by a refused switch: %v", v)
	}
	old3, _ := pgReferral(t, pool, ctx, friendID(3))
	pgSeed(t, pool, ctx, abYear, day90*2, "Y2")
	got, err := switchTo(true)
	if err != nil || got["moved"] != 1 {
		t.Fatalf("execute = %v %v", got, err)
	}
	for _, f := range []int{3} {
		if v := pgVariantOf(t, pool, ctx, friendID(f)); v == nil || *v != VariantYear {
			t.Fatalf("friend %d variant = %v", f, v)
		}
		var switched *time.Time
		if err := pool.QueryRow(ctx, `select offer_variant_switched_at from referrals where referee = $1`, friendID(f)).Scan(&switched); err != nil || switched == nil {
			t.Fatalf("friend %d switched_at = %v %v", f, switched, err)
		}
	}
	if s := pgState(t, pool, ctx, old3); s != "expired" {
		t.Fatalf("old code %s = %s, want expired (never re-issued)", old3, s)
	}
	for _, f := range []int{1, 4} {
		if v := pgVariantOf(t, pool, ctx, friendID(f)); v == nil || *v != VariantMonth {
			t.Fatalf("friend %d should not move: %v", f, v)
		}
	}
	if v := pgVariantOf(t, pool, ctx, friendID(2)); v != nil {
		t.Fatalf("legacy friend 2 moved to %v", *v)
	}
}

func TestPGForceMonthForUnawareApps(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abMonth, day90, "M1")
	pgSeed(t, pool, ctx, abYear, day90, "Y1")
	pgSetYearPct(t, pool, ctx, 100)
	offer, err := st.Claim(WithForceMonth(ctx), friendID(1), codeA, "", nil)
	if err != nil || offer != "M1" {
		t.Fatalf("unaware claim at 100%% = %q %v, want the monthly M1", offer, err)
	}
	var pct *int
	if err := pool.QueryRow(ctx, `select offer_year_pct_at_claim from referrals where referee = $1`, friendID(1)).Scan(&pct); err != nil || pct != nil {
		t.Fatalf("pct at claim = %v %v, want null (not a random assignment)", pct, err)
	}
	// The setting does not leak to the next claim on the pool.
	if offer, err := st.Claim(ctx, friendID(2), codeA, "", nil); err != nil || offer != "Y1" {
		t.Fatalf("aware claim after = %q %v, want Y1", offer, err)
	}
}

func TestPGABStatsCountsTheExperimentOnly(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abMonth, day90, "M1", "M2")
	pgSeed(t, pool, ctx, abYear, day90, "Y1", "Y2")

	// Claimed at share 0: not a random assignment, not counted.
	if _, err := st.Claim(ctx, friendID(9), codeA, "", nil); err != nil {
		t.Fatal(err)
	}
	pgSetYearPct(t, pool, ctx, 100)
	// Started now; backdate the start so the cohorts can be old enough.
	if _, err := pool.Exec(ctx, `update referral_settings set offer_ab_started_at = now() - interval '400 days' where id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update referrals set claimed_at = now() - interval '500 days' where referee = $1`, friendID(9)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx, friendID(1), codeA, "", nil); err != nil { // yearly, paid, renewed
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx, friendID(2), codeA, "", nil); err != nil { // yearly, never redeemed
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`update referrals set claimed_at = now() - interval '370 days', redeemed_at = now() - interval '369 days', state = 'rewarded'
		  where referee = $1`, friendID(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update referrals set claimed_at = now() - interval '370 days' where referee = $1`, friendID(2)); err != nil {
		t.Fatal(err)
	}
	// Random assignment happened at a share inside 1–99 (set directly; at 100 these would not count).
	if _, err := pool.Exec(ctx, `update referrals set offer_year_pct_at_claim = 50 where referee in ($1, $2)`, friendID(1), friendID(2)); err != nil {
		t.Fatal(err)
	}
	id1 := pgReferralID(t, pool, ctx, friendID(1))
	for i, ago := range []string{"339 days", "3 days"} { // first charge after the free month; the renewal a year later
		if _, err := pool.Exec(ctx,
			`insert into reward_ledger (referral_id, referrer, referee, rc_event_id, event_type, transaction_id,
			                            period_type, price_usd_cents, takehome_percentage, credit_cents,
			                            purchased_at, available_at, state)
			 values ($1, $2, $3, $4, 'RENEWAL', $4, 'NORMAL', 3999, 0.85, 1020, now() - $5::interval, now(), 'available')`,
			id1, userA, friendID(1), fmt.Sprintf("tx-%d", i), ago); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(ctx, `select variant, claimed, redeemed, paid, renewed, refunded, switched, window_days, claim_cohort, claim_proceeds_cents from referral_ab_stats()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		claimed, redeemed, paid, renewed, refunded, switched, cohort int
		proceeds                                                     int64
	}
	got := map[string]row{}
	for rows.Next() {
		var v string
		var d int
		var r row
		if err := rows.Scan(&v, &r.claimed, &r.redeemed, &r.paid, &r.renewed, &r.refunded, &r.switched, &d, &r.cohort, &r.proceeds); err != nil {
			t.Fatal(err)
		}
		got[fmt.Sprintf("%s/%d", v, d)] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	y365 := got["year/365"]
	if y365.claimed != 2 || y365.redeemed != 1 || y365.paid != 1 || y365.renewed != 1 || y365.refunded != 0 || y365.cohort != 2 {
		t.Fatalf("year/365 = %+v", y365)
	}
	// 365 days after claiming: only the first charge (claimed 370 days ago, first charge 339 days ago = day 31).
	if want := int64(3399); y365.proceeds != want { // round(3999 × 0.85)
		t.Fatalf("year/365 proceeds = %d, want %d", y365.proceeds, want)
	}
	if y425 := got["year/425"]; y425.cohort != 0 {
		t.Fatalf("year/425 cohort = %d, want 0 (no one claimed 425+ days ago)", y425.cohort)
	}
	if m := got["month/30"]; m.claimed != 0 {
		t.Fatalf("month = %+v, want the pre-start friend left out", m)
	}
}

func TestPGClaimWithVariantAnswersFromTheSameTransaction(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abYear, day90, "Y1")
	pgSetYearPct(t, pool, ctx, 100)
	offer, variant, err := st.ClaimWithVariant(ctx, friendID(1), codeA, "", nil)
	if err != nil || offer != "Y1" || variant != VariantYear {
		t.Fatalf("claim = %q %q %v", offer, variant, err)
	}
	offer, variant, err = st.ClaimWithVariant(ctx, friendID(1), codeA, "", nil) // re-claim
	if err != nil || offer != "Y1" || variant != VariantYear {
		t.Fatalf("re-claim = %q %q %v", offer, variant, err)
	}
}

func TestPGABStatsExclusions(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, abYear, day90, "Y1", "Y2", "Y3", "Y4")
	pgSetYearPct(t, pool, ctx, 100)
	for i := 1; i <= 4; i++ {
		if _, err := st.Claim(ctx, friendID(i), codeA, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `update referrals set offer_year_pct_at_claim = 50`); err != nil {
		t.Fatal(err)
	}
	// 1 counted; 2 void as duplicate_apple_id (dropped); 3 void as no_subscription (kept); 4 switched (own column).
	if _, err := pool.Exec(ctx, `update referrals set state = 'void', void_reason = 'duplicate_apple_id' where referee = $1`, friendID(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update referrals set state = 'void', void_reason = 'no_subscription', redeemed_at = now() - interval '40 days' where referee = $1`, friendID(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update referrals set offer_variant_switched_at = now() where referee = $1`, friendID(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update referrals set claimed_at = now() - interval '40 days'`); err != nil {
		t.Fatal(err)
	}
	// Friend 3 paid once, then that charge was refunded and voided: counted as paid + refunded, not as proceeds.
	id3 := pgReferralID(t, pool, ctx, friendID(3))
	if _, err := pool.Exec(ctx,
		`insert into reward_ledger (referral_id, referrer, referee, rc_event_id, event_type, transaction_id,
		                            period_type, price_usd_cents, takehome_percentage, credit_cents,
		                            purchased_at, available_at, state, void_reason)
		 values ($1, $2, $3, 'ev-r', 'RENEWAL', 'tx-r', 'NORMAL', 3999, 0.85, 1020, now() - interval '35 days', now(), 'voided', 'refunded')`,
		id3, userA, friendID(3)); err != nil {
		t.Fatal(err)
	}
	var claimed, redeemed, paid, refunded, switched, cohort int
	var proceeds int64
	if err := pool.QueryRow(ctx,
		`select claimed, redeemed, paid, refunded, switched, claim_cohort, claim_proceeds_cents
		   from referral_ab_stats() where variant = 'year' and window_days = 30`).
		Scan(&claimed, &redeemed, &paid, &refunded, &switched, &cohort, &proceeds); err != nil {
		t.Fatal(err)
	}
	if claimed != 2 || redeemed != 1 || paid != 1 || refunded != 1 || switched != 1 || cohort != 2 || proceeds != 0 {
		t.Fatalf("year/30 = claimed %d redeemed %d paid %d refunded %d switched %d cohort %d proceeds %d",
			claimed, redeemed, paid, refunded, switched, cohort, proceeds)
	}
}
