// Command backfill-kingdom is a one-shot that stamps the biological `kingdom`
// ("Fungi" / "Plantae") on existing Supabase plants_pending rows written before
// the field existed, so cached out-of-catalog mushrooms get the iOS safety notice
// (see proxy/enrichment/backfill_kingdom.go + SPEC §7 kingdom). The kingdom comes
// from iNaturalist only (free, no LLM call); fungal rows also lose any
// edible/culinary uses_list entries and the "edible" attribute.
//
// It is independent of the resident service — run it once after deploy, then let
// it exit. Dry-run by default (iNat is queried, nothing is written); pass -apply
// to write.
//
// Usage:
//
//	backfill-kingdom                 # dry-run against /etc/yardmate-api/secrets.env
//	backfill-kingdom -apply          # write changes
//	backfill-kingdom -secrets PATH   # use a different secrets file
//	backfill-kingdom -interval 2s    # min gap between iNat requests (default 1s)
//
// Exit code 1 when any plant's iNat lookup failed (429 / 5xx / timeout, after
// backoff retries) or any row write failed — re-run to retry. Plants iNat simply
// has no exact match for are reported as UNDETERMINED and do not fail the run.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/secrets"
)

func main() {
	secretsPath := flag.String("secrets", envOr("YARDMATE_API_SECRETS_PATH", "/etc/yardmate-api/secrets.env"), "path to secrets.env")
	apply := flag.Bool("apply", false, "write changes to the database (default: dry-run, no writes)")
	interval := flag.Duration("interval", time.Second, "minimum gap between iNaturalist requests (also the retry backoff base)")
	timeout := flag.Duration("timeout", 60*time.Minute, "overall timeout for the run")
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
		log.Print("mode: APPLY — rows lacking kingdom will be patched")
	} else {
		log.Print("mode: DRY-RUN — no writes; pass -apply to persist")
	}

	rep, err := enrichment.RunKingdomBackfill(ctx, db, proxy.NewINatClient(), *apply, *interval, log.Printf)
	if err != nil {
		log.Fatalf("backfill: %v", err)
	}
	if rep.Undetermined > 0 {
		log.Printf("NOTE: %d plant(s) have no exact iNat match and keep kingdom=null (logged above)", rep.Undetermined)
	}
	if rep.LookupFailed > 0 || rep.Failed > 0 {
		log.Printf("ERROR: kingdom backfill INCOMPLETE: %d plant lookup(s) failed (iNat 429/5xx/timeout), %d row write(s) failed — re-run to retry", rep.LookupFailed, rep.Failed)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
