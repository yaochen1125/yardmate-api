package referral

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The plpgsql side (migrations 013 + 014) against a real Postgres: the issue
// margin, the expires_at order, re-claim keeping / replacing the issued code
// (swap grace, old code kept when the pool has no replacement, claimed_at
// refreshed on a swap), pool_empty rolling the referral back, and PoolStatus
// grouping. Needs a scratch database:
//
//	REFERRAL_PG_TEST_URL=postgres://postgres:postgres@localhost:5432/scratch go test ./referral/ -run PG -v
//
// Skipped when the variable is unset. Everything lives in a throwaway schema
// that is dropped afterwards, so any database works — never point it at
// staging or prod. The roles the migrations grant to (anon / authenticated /
// service_role) are created when missing, which needs CREATEROLE.

func pgTest(t *testing.T) (*PGStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("REFERRAL_PG_TEST_URL")
	if url == "" {
		t.Skip("REFERRAL_PG_TEST_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("referral_test_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `do $$ begin
		if not exists (select 1 from pg_roles where rolname = 'anon')          then create role anon nologin;          end if;
		if not exists (select 1 from pg_roles where rolname = 'authenticated') then create role authenticated nologin; end if;
		if not exists (select 1 from pg_roles where rolname = 'service_role')  then create role service_role nologin;  end if;
	end $$`); err != nil {
		t.Fatalf("roles: %v", err)
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	admin.Close(ctx)

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(ctx, url); err == nil {
			_, _ = c.Exec(ctx, "drop schema "+schema+" cascade")
			c.Close(ctx)
		}
	})
	for _, f := range []string{"013_referral.sql", "014_referral_pool_margin.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "proxy", "enrichment", "migrations", f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// No arguments → simple protocol → the whole file runs as one script.
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return NewPGStore(pool), pool, ctx
}

// pgSeed seeds one batch: every code gets the SAME expiry (one timestamp
// computed here, like import-codes.sh gives a CSV one date), so they group as
// one batch in PoolStatus.
func pgSeed(t *testing.T, pool *pgxpool.Pool, ctx context.Context, batch string, in time.Duration, codes ...string) {
	t.Helper()
	exp := time.Now().UTC().Add(in).Truncate(time.Second)
	if _, err := pool.Exec(ctx,
		`insert into code_pool (code, offer_ref_name, expires_at)
		 select unnest($1::text[]), $2, $3`, codes, batch, exp); err != nil {
		t.Fatalf("seed %s: %v", batch, err)
	}
}

func pgExpiresIn(t *testing.T, pool *pgxpool.Pool, ctx context.Context, code string, in time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `update code_pool set expires_at = now() + make_interval(secs => $2) where code = $1`,
		code, in.Seconds()); err != nil {
		t.Fatalf("move %s: %v", code, err)
	}
}

func pgState(t *testing.T, pool *pgxpool.Pool, ctx context.Context, code string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `select state from code_pool where code = $1`, code).Scan(&s); err != nil {
		t.Fatalf("state of %s: %v", code, err)
	}
	return s
}

func pgReferral(t *testing.T, pool *pgxpool.Pool, ctx context.Context, referee string) (offer string, claimedAt time.Time) {
	t.Helper()
	if err := pool.QueryRow(ctx, `select coalesce(offer_code, ''), claimed_at from referrals where referee = $1`, referee).
		Scan(&offer, &claimedAt); err != nil {
		t.Fatalf("referral of %s: %v", referee, err)
	}
	return offer, claimedAt
}

func pgInviteCode(t *testing.T, st *PGStore, ctx context.Context, user string) string {
	t.Helper()
	me, err := st.Me(ctx, user)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	return me.Code
}

const userC = "cccccccc-cccc-cccc-cccc-cccccccccccc"

func TestPGClaimSkipsCodesInsideTheIssueMargin(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "short", 10*day, "SHORT10")
	pgSeed(t, pool, ctx, "long", 90*day, "LONG90")

	offer, err := st.Claim(ctx, userB, codeA)
	if err != nil || offer != "LONG90" {
		t.Fatalf("claim = %q, %v; want LONG90 (SHORT10 has only 10 of the 14 margin days)", offer, err)
	}
	// The short one is left as it is: still free, just not issuable.
	if s := pgState(t, pool, ctx, "SHORT10"); s != "free" {
		t.Errorf("SHORT10 state = %s, want free", s)
	}
	// Margin is a setting: at 0 days the short code is fair game.
	if _, err := pool.Exec(ctx, `update referral_settings set issue_margin_days = 0 where id = 1`); err != nil {
		t.Fatal(err)
	}
	if offer, err := st.Claim(ctx, userC, codeA); err != nil || offer != "SHORT10" {
		t.Errorf("margin 0: claim = %q, %v; want SHORT10", offer, err)
	}
}

func TestPGClaimHandsOutTheSoonestUsableCodeFirst(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b60", 60*day, "D60")
	pgSeed(t, pool, ctx, "b30", 30*day, "D30")
	pgSeed(t, pool, ctx, "b45", 45*day, "D45")
	pgSeed(t, pool, ctx, "b07", 7*day, "D07") // inside the margin: never
	pgSeed(t, pool, ctx, "dead", -time.Hour, "DEAD")

	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D30" {
		t.Fatalf("first claim = %q, %v; want D30", offer, err)
	}
	if offer, err := st.Claim(ctx, userC, codeA); err != nil || offer != "D45" {
		t.Fatalf("second claim = %q, %v; want D45", offer, err)
	}
	// Claiming no longer retires dead stock; that is ExpireCodes' job.
	if s := pgState(t, pool, ctx, "DEAD"); s != "free" {
		t.Errorf("DEAD state after claims = %s, want free (ExpireCodes retires it)", s)
	}
	if n, err := st.ExpireCodes(ctx); err != nil || n != 1 {
		t.Errorf("ExpireCodes = %d, %v; want 1", n, err)
	}
}

func TestPGReclaimSwapsOnlyInsideTheSwapGrace(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b20", 20*day, "D20")
	pgSeed(t, pool, ctx, "b90", 90*day, "D90")

	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D20" {
		t.Fatalf("claim = %q, %v; want D20", offer, err)
	}
	_, claimed0 := pgReferral(t, pool, ctx, userB)
	// Same person, same code, the app crashed: the same card again.
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D20" {
		t.Fatalf("re-claim = %q, %v; want D20 again", offer, err)
	}
	// 10 days left: inside the 14-day ISSUE margin but outside the 3-day SWAP
	// grace — still the same card (no burning a second good code).
	pgExpiresIn(t, pool, ctx, "D20", 10*day)
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D20" {
		t.Fatalf("re-claim at 10 days = %q, %v; want D20 kept", offer, err)
	}
	if s := pgState(t, pool, ctx, "D90"); s != "free" {
		t.Errorf("D90 state = %s, want free (not burnt)", s)
	}
	// 2 days left: inside the swap grace → fresh code, old one retired,
	// referral re-pointed and claimed_at refreshed.
	pgExpiresIn(t, pool, ctx, "D20", 2*day)
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D90" {
		t.Fatalf("re-claim at 2 days = %q, %v; want a fresh D90", offer, err)
	}
	if s := pgState(t, pool, ctx, "D20"); s != "expired" {
		t.Errorf("D20 state = %s, want expired", s)
	}
	if offer, claimed1 := pgReferral(t, pool, ctx, userB); offer != "D90" || !claimed1.After(claimed0) {
		t.Errorf("referral = %s claimed_at %v (was %v); want D90 and a later claimed_at", offer, claimed1, claimed0)
	}
	// A redeemed code is always the answer, however close its date.
	if _, err := pool.Exec(ctx, `update code_pool set state = 'redeemed' where code = 'D90'`); err != nil {
		t.Fatal(err)
	}
	pgExpiresIn(t, pool, ctx, "D90", time.Hour)
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D90" {
		t.Errorf("re-claim after redeem = %q, %v; want D90", offer, err)
	}
}

func TestPGReclaimKeepsTheOldCodeWhenThereIsNoReplacement(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "only", 20*day, "ONLY")

	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "ONLY" {
		t.Fatalf("claim = %q, %v", offer, err)
	}
	_, claimed0 := pgReferral(t, pool, ctx, userB)
	// 2 days left, nothing to swap to: the old code still works at Apple, so
	// it is handed back rather than a 503 — and nothing is touched.
	pgExpiresIn(t, pool, ctx, "ONLY", 2*day)
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "ONLY" {
		t.Fatalf("re-claim with empty pool = %q, %v; want ONLY kept", offer, err)
	}
	if s := pgState(t, pool, ctx, "ONLY"); s != "issued" {
		t.Errorf("ONLY state = %s, want issued (not retired without a replacement)", s)
	}
	if _, claimed1 := pgReferral(t, pool, ctx, userB); !claimed1.Equal(claimed0) {
		t.Errorf("claimed_at moved (%v → %v) without a swap", claimed0, claimed1)
	}
	// Old code dead and still nothing to swap to: now it is pool_empty, and
	// the existing referral stays (only the first claim's insert rolls back).
	pgExpiresIn(t, pool, ctx, "ONLY", -time.Hour)
	_, err := st.Claim(ctx, userB, codeA)
	if f, ok := named(err); !ok || f.Code != ErrPoolEmpty {
		t.Fatalf("re-claim with dead code and empty pool: %v, want pool_empty", err)
	}
	if offer, _ := pgReferral(t, pool, ctx, userB); offer != "ONLY" {
		t.Errorf("referral offer_code = %s, want ONLY untouched", offer)
	}
	// A restock fixes it: swap happens now.
	pgSeed(t, pool, ctx, "fresh", 120*day, "FRESH")
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "FRESH" {
		t.Errorf("after restock = %q, %v; want FRESH", offer, err)
	}
	if s := pgState(t, pool, ctx, "ONLY"); s != "expired" {
		t.Errorf("ONLY state = %s, want expired after the swap", s)
	}
}

func TestPGClaimPoolEmptyWhenOnlyShortCodesAreLeft(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "short", 5*day, "SHORT5")

	_, err := st.Claim(ctx, userB, codeA)
	if f, ok := named(err); !ok || f.Code != ErrPoolEmpty {
		t.Fatalf("claim err = %v, want pool_empty", err)
	}
	// The raise rolled the referral back: B can try again after a restock.
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from referrals where referee = $1`, userB).Scan(&n); err != nil || n != 0 {
		t.Errorf("referrals for B = %d, %v; want 0", n, err)
	}
	if s := pgState(t, pool, ctx, "SHORT5"); s != "free" {
		t.Errorf("SHORT5 state = %s, want free (not expired: it has 5 days)", s)
	}
	pgSeed(t, pool, ctx, "fresh", 120*day, "FRESH")
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "FRESH" {
		t.Errorf("after restock: %q, %v", offer, err)
	}
}

func TestPGPoolStatusGroupsByBatch(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "Referral Friend Month", 10*day, "S1", "S2")
	pgSeed(t, pool, ctx, "Referral Friend Month", 120*day, "L1", "L2", "L3")
	if _, err := st.Claim(ctx, userB, codeA); err != nil { // takes L1
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update code_pool set state = 'expired' where code = 'S2'`); err != nil {
		t.Fatal(err)
	}

	ps, err := st.PoolStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Settings != (PoolSettings{IssueMarginDays: 14, SwapGraceDays: 3, LowStockThreshold: 100, ExpiryWarnDays: 30}) {
		t.Errorf("settings = %+v, want the 014 defaults", ps.Settings)
	}
	if len(ps.Batches) != 2 {
		t.Fatalf("batches = %+v, want 2 (same name, two dates)", ps.Batches)
	}
	short, long := ps.Batches[0], ps.Batches[1]
	if !short.ExpiresAt.Before(long.ExpiresAt) {
		t.Errorf("batches not in expiry order: %+v", ps.Batches)
	}
	if short.Free != 1 || short.Expired != 1 || short.Issued != 0 {
		t.Errorf("short batch = %+v", short)
	}
	if long.Free != 2 || long.Issued != 1 {
		t.Errorf("long batch = %+v", long)
	}
	stock := TakeStock(ps, time.Now())
	if stock.Usable != 2 || stock.Short != 1 || !stock.LowStock {
		t.Errorf("stock = %+v", stock)
	}
}
