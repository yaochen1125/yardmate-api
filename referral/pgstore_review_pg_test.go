package referral

import (
	"os"
	"path/filepath"
	"testing"
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
