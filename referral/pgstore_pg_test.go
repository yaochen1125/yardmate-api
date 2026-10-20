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
// margin, the expires_at order, re-claim keeping / replacing the issued code,
// pool_empty rolling the referral back, and PoolStatus grouping. Needs a
// scratch database:
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

// pgCode seeds one pool code expiring in `in` from now.
func pgSeed(t *testing.T, pool *pgxpool.Pool, ctx context.Context, batch string, in time.Duration, codes ...string) {
	t.Helper()
	for _, c := range codes {
		if _, err := pool.Exec(ctx,
			`insert into code_pool (code, offer_ref_name, expires_at) values ($1, $2, now() + make_interval(secs => $3))`,
			c, batch, in.Seconds()); err != nil {
			t.Fatalf("seed %s: %v", c, err)
		}
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

func pgInviteCode(t *testing.T, st *PGStore, ctx context.Context, user string) string {
	t.Helper()
	me, err := st.Me(ctx, user)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	return me.Code
}

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
	userC := "cccccccc-cccc-cccc-cccc-cccccccccccc"
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

	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D30" {
		t.Fatalf("first claim = %q, %v; want D30", offer, err)
	}
	userC := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	if offer, err := st.Claim(ctx, userC, codeA); err != nil || offer != "D45" {
		t.Fatalf("second claim = %q, %v; want D45", offer, err)
	}
}

func TestPGReclaimKeepsTheIssuedCodeUntilItIsInsideTheMargin(t *testing.T) {
	st, pool, ctx := pgTest(t)
	codeA := pgInviteCode(t, st, ctx, userA)
	pgSeed(t, pool, ctx, "b20", 20*day, "D20")
	pgSeed(t, pool, ctx, "b90", 90*day, "D90")

	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D20" {
		t.Fatalf("claim = %q, %v; want D20", offer, err)
	}
	// Same person, same code, the app crashed: the same card again.
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D20" {
		t.Fatalf("re-claim = %q, %v; want D20 again", offer, err)
	}
	// Time passes: D20 now has 10 days left — inside the margin, not expired.
	if _, err := pool.Exec(ctx, `update code_pool set expires_at = now() + interval '10 days' where code = 'D20'`); err != nil {
		t.Fatal(err)
	}
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D90" {
		t.Fatalf("re-claim near expiry = %q, %v; want a fresh D90", offer, err)
	}
	if s := pgState(t, pool, ctx, "D20"); s != "expired" {
		t.Errorf("D20 state = %s, want expired", s)
	}
	var current string
	if err := pool.QueryRow(ctx, `select offer_code from referrals where referee = $1`, userB).Scan(&current); err != nil || current != "D90" {
		t.Errorf("referrals.offer_code = %q, %v; want D90", current, err)
	}
	// A redeemed code is always the answer, however close its date.
	if _, err := pool.Exec(ctx, `update code_pool set state = 'redeemed', expires_at = now() + interval '1 day' where code = 'D90'`); err != nil {
		t.Fatal(err)
	}
	if offer, err := st.Claim(ctx, userB, codeA); err != nil || offer != "D90" {
		t.Errorf("re-claim after redeem = %q, %v; want D90", offer, err)
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
	if ps.Settings != (PoolSettings{IssueMarginDays: 14, LowStockThreshold: 100, ExpiryWarnDays: 30}) {
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
