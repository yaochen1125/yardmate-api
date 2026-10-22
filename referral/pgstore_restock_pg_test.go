package referral

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 021 + the restock half of PGStore against a real Postgres. Same
// gate as pgstore_pg_test.go (REFERRAL_PG_TEST_URL, throwaway schema; never
// staging or prod):
//
//	REFERRAL_PG_TEST_URL=postgres://postgres:postgres@localhost:5432/scratch go test ./referral/ -run PGRestock -v

func pgRestockTest(t *testing.T) (*PGStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	store, pool, ctx := pgTest(t)
	sql, err := os.ReadFile(filepath.Join("..", "proxy", "enrichment", "migrations", "021_referral_auto_restock.sql"))
	if err != nil {
		t.Fatal(err)
	}
	// Twice: 021 must be re-runnable.
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply 021 (run %d): %v", i+1, err)
		}
	}
	return store, pool, ctx
}

func TestPGRestockMigrationDefaultsAndChecks(t *testing.T) {
	store, pool, ctx := pgRestockTest(t)
	day := utcDayStart(time.Now())
	st, err := store.RestockStatus(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	want := RestockSettings{Enabled: false, Below: 300, BatchSize: 1000, DailyMax: 2, LeadDays: 45, Environment: ""}
	if st.Settings != want || st.Open != nil || st.StartedToday != 0 || !st.LastStarted.IsZero() {
		t.Fatalf("fresh status = %+v", st)
	}
	for _, bad := range []string{
		`update referral_settings set restock_environment = 'prod' where id = 1`,
		`update referral_settings set restock_batch_size = 0 where id = 1`,
		`update referral_settings set restock_batch_size = 25001 where id = 1`,
		`update referral_settings set restock_below = -1 where id = 1`,
		`insert into code_restocks (environment, number_of_codes, expires_at) values ('STAGING', 10, now())`,
		`insert into code_restocks (environment, number_of_codes, expires_at, state) values ('SANDBOX', 10, now(), 'done')`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	if _, err := pool.Exec(ctx, `update referral_settings set restock_environment = 'SANDBOX', auto_restock_enabled = true where id = 1`); err != nil {
		t.Fatal(err)
	}
	if st, _ := store.RestockStatus(ctx, day); !st.Settings.Enabled || st.Settings.Environment != EnvSandbox {
		t.Errorf("after update: %+v", st.Settings)
	}
}

func TestPGRestockLifecycle(t *testing.T) {
	store, pool, ctx := pgRestockTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	day := utcDayStart(now)
	exp := restockExpiry(now)
	start := RestockStart{Environment: EnvSandbox, Codes: 3, ExpiresAt: exp, StartedAt: now, DayStart: day, DailyMax: 2, MinGap: time.Hour}

	id, err := store.BeginRestock(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	// One unfinished at a time.
	later := start
	later.StartedAt = now.Add(2 * time.Hour)
	if _, err := store.BeginRestock(ctx, later); !errors.Is(err, errRestockOpen) {
		t.Fatalf("second while open: %v", err)
	}
	st, err := store.RestockStatus(ctx, day)
	if err != nil || st.Open == nil || st.Open.ID != id || st.Open.State != RestockCreating || st.StartedToday != 1 || !st.LastStarted.Equal(now) {
		t.Fatalf("status = %+v err %v", st, err)
	}
	if ok, err := store.MarkRestockCreated(ctx, id, "622701"); err != nil || !ok {
		t.Fatalf("created: %v %v", ok, err)
	}
	if ok, _ := store.MarkRestockCreated(ctx, id, "622702"); ok {
		t.Error("created twice")
	}
	if taken, err := store.RestockBatchesTaken(ctx, []string{"622701", "622999"}); err != nil || !taken["622701"] || taken["622999"] {
		t.Errorf("taken = %v %v", taken, err)
	}
	// One code already in the pool (a hand import): ignored, not an error.
	pgSeed(t, pool, ctx, "Referral Friend Month", 100*24*time.Hour, "RSTK00000002")
	n, err := store.ImportRestock(ctx, id, []string{"RSTK00000001", "RSTK00000002", "RSTK00000003"}, RestockOfferRefName, exp, now)
	if err != nil || n != 2 {
		t.Fatalf("import: %d %v", n, err)
	}
	var state string
	var count int
	if err := pool.QueryRow(ctx, `select state, imported_count from code_restocks where id = $1`, id).Scan(&state, &count); err != nil || state != RestockImported || count != 2 {
		t.Fatalf("row: %s %d %v", state, count, err)
	}
	if got := pgState(t, pool, ctx, "RSTK00000001"); got != "free" {
		t.Errorf("imported code state %s", got)
	}
	var ref string
	var expires time.Time
	if err := pool.QueryRow(ctx, `select offer_ref_name, expires_at from code_pool where code = 'RSTK00000003'`).Scan(&ref, &expires); err != nil || ref != RestockOfferRefName || !expires.Equal(exp) {
		t.Errorf("pool row: %q %v %v", ref, expires, err)
	}
	if _, err := store.ImportRestock(ctx, id, []string{"RSTK00000004"}, RestockOfferRefName, exp, now); !errors.Is(err, errRestockNotCreated) {
		t.Errorf("import into an imported restock: %v", err)
	}
	if got := pgState(t, pool, ctx, "RSTK00000003"); got != "free" {
		t.Errorf("state %s", got)
	}

	// Gap and daily max, re-checked at insert time.
	soon := start
	soon.StartedAt = now.Add(30 * time.Minute)
	if _, err := store.BeginRestock(ctx, soon); !errors.Is(err, errRestockLimited) {
		t.Errorf("inside the gap: %v", err)
	}
	second := start
	second.StartedAt = now.Add(61 * time.Minute)
	id2, err := store.BeginRestock(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	// The same batch id cannot be claimed by a second row.
	if _, err := store.MarkRestockCreated(ctx, id2, "622701"); err == nil {
		t.Error("asc_batch_id claimed twice")
	}
	if err := store.FailRestock(ctx, id2, "test", now); err != nil {
		t.Fatal(err)
	}
	third := start
	third.StartedAt = now.Add(3 * time.Hour)
	if _, err := store.BeginRestock(ctx, third); !errors.Is(err, errRestockLimited) {
		t.Errorf("over the daily max: %v", err)
	}
	if st, _ := store.RestockStatus(ctx, day); st.Open != nil {
		t.Errorf("open after fail: %+v", st.Open)
	}
}

func TestPGRestockConcurrentBeginOnlyOne(t *testing.T) {
	store, _, ctx := pgRestockTest(t)
	now := time.Now().UTC()
	start := RestockStart{Environment: EnvProduction, Codes: 500, ExpiresAt: restockExpiry(now), StartedAt: now, DayStart: utcDayStart(now), DailyMax: 10, MinGap: 0}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, open, other int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.BeginRestock(ctx, start)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, errRestockOpen), errors.Is(err, errRestockLimited):
				open++
			default:
				other++
				t.Errorf("begin: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || open != 7 || other != 0 {
		t.Fatalf("ok=%d refused=%d other=%d, want exactly one", ok, open, other)
	}
}
