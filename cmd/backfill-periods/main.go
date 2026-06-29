// Command backfill-periods is a one-shot that repairs existing Supabase
// plants_pending rows so their stored *_period_short labels agree with
// *_months_north — the same invariant the resident service now enforces on
// every new Generate/Translate (see proxy/enrichment/bloom.go). Rows written
// before that change can still carry the drift (per-month chart says one thing,
// the range header another); this brings them in line.
//
// It is independent of the resident service — run it once after deploy, then
// let it exit. Dry-run by default; pass -apply to write.
//
// Usage:
//
//	backfill-periods                 # dry-run against /etc/yardmate-api/secrets.env
//	backfill-periods -apply          # write changes
//	backfill-periods -secrets PATH   # use a different secrets file
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/secrets"
)

func main() {
	secretsPath := flag.String("secrets", envOr("YARDMATE_API_SECRETS_PATH", "/etc/yardmate-api/secrets.env"), "path to secrets.env")
	apply := flag.Bool("apply", false, "write changes to the database (default: dry-run, no writes)")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout for the run")
	flag.Parse()

	vault, err := secrets.Load(*secretsPath)
	if err != nil {
		log.Fatalf("load secrets (%s): %v", *secretsPath, err)
	}
	dsn := vault.Get("SUPABASE_DB_URL")
	if dsn == "" {
		log.Fatal("SUPABASE_DB_URL missing in secrets — cannot reach plants_pending")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	db, err := enrichment.NewDB(ctx, dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	if *apply {
		log.Print("mode: APPLY — changed rows will be written")
	} else {
		log.Print("mode: DRY-RUN — no writes; pass -apply to persist")
	}

	stats, err := enrichment.BackfillPeriods(ctx, db, *apply, log.Printf)
	if err != nil {
		log.Fatalf("backfill: %v", err)
	}
	log.Printf("done: scanned=%d changed=%d failed=%d apply=%v",
		stats.Scanned, stats.Changed, stats.Failed, *apply)
	if stats.Failed > 0 {
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
