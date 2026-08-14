// Package main is the entry point of yardmate-api, the App-Attest-gated
// secret-vending HTTP service for the YardMate iOS app. See attest/SPEC.md
// and secrets/SPEC.md for the design contracts.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/doctor"
	"github.com/yaochen1125/yardmate-api/inflight"
	"github.com/yaochen1125/yardmate-api/proxy"
	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/proxy/imageingest"
	"github.com/yaochen1125/yardmate-api/proxy/imageingest/sources"
	"github.com/yaochen1125/yardmate-api/ratelimit"
	"github.com/yaochen1125/yardmate-api/secrets"
)

const (
	defaultAddr        = "127.0.0.1:8080"
	defaultDBPath      = "/var/lib/yardmate-api/credentials.db"
	defaultSecretsPath = "/etc/yardmate-api/secrets.env"
	defaultAppID       = "PMX32RG52M.com.chenyao.plantapp"

	// Rate-limit defaults (see ratelimit/SPEC §3).
	// per-IP gates ALL /v1 endpoints incl. the cheap App-Attest/secrets
	// handshake (attest/challenge, attest/register, secrets/challenge,
	// app-secrets). One client doing the handshake fires several of these per
	// key fetch, so the per-IP ceiling must be generous or the handshake
	// starves the expensive proxy calls — observed in prod: ~90 handshake
	// reqs/hr from a single device exhausted the old 100 ceiling and 429'd
	// identify after ~3 photos. The expensive upstream calls
	// (identify/diagnose/enrichment) stay independently bounded by the per-Device
	// bucket (300/hr) regardless of this ceiling, so raising per-IP does NOT
	// widen the upstream-cost / abuse surface.
	defaultIPLimit       = 600
	defaultIPWindow      = time.Hour
	defaultKeyIDLimit    = 50
	defaultKeyIDWindow   = 24 * time.Hour
	defaultDeviceLimit   = 300
	defaultDeviceWindow  = time.Hour
	defaultSweepInterval = time.Minute

	// defaultChallengeSweepInterval is how often the attest challenge store is
	// pruned of expired rows. Challenges are ~5 min-lived (attest.DefaultChallengeTTL)
	// and the consume path deletes on success, so a 10-min sweep is ample to keep
	// issued-but-never-consumed challenges from accumulating in BoltDB.
	defaultChallengeSweepInterval = 10 * time.Minute

	// In-flight concurrency bound for /v1/identify + /v1/diagnose (inflight/SPEC).
	// maxInflight is the memory guard (each request buffers a ≤8 MB image);
	// 30 × ~16 MB ≈ 480 MB, well under the 4 GB cgroup cap. maxWait bounds
	// queued waiters (each ~KB). waitBudget is kept under the client request
	// timeout (identify 30 s / diagnose 20 s) so overflow usually resolves into
	// a served 200 rather than a 503.
	defaultMaxInflight     = 30
	defaultInflightMaxWait = 200
	defaultInflightWait    = 5 * time.Second

	// Doctor's own concurrency bound (doctor/SPEC.md §4): each SSE stream
	// holds its slot for the full 5–15 s generation, so the cap is far lower
	// than identify/diagnose's — 4 concurrent generations is plenty at launch
	// and keeps peak memory (cap × ≤9 MB upload) negligible.
	defaultDoctorInflight     = 4
	defaultDoctorInflightWait = 8

	// Doctor's hourly call ceiling (vault DOCTOR_HOURLY_BUDGET overrides).
	// Separate from the identify/diagnose budget: neither traffic class may
	// exhaust the other's.
	defaultDoctorHourlyBudget = 200

	// Below this many remaining Pl@ntNet daily-quota requests, each identify
	// WARN-logs so the server-side watcher can email before exhaustion
	// (proxy.PlantNetClient.logQuota). Override via env in the systemd unit,
	// like the YARDMATE_API_RL_* / _INFLIGHT_* knobs (see deploy/README.md).
	defaultPlantNetQuotaWarn = 50
)

func main() {
	addr := envOr("YARDMATE_API_ADDR", defaultAddr)
	dbPath := envOr("YARDMATE_API_DB_PATH", defaultDBPath)
	secretsPath := envOr("YARDMATE_API_SECRETS_PATH", defaultSecretsPath)
	appID := envOr("YARDMATE_API_APP_ID", defaultAppID)

	vault, err := secrets.Load(secretsPath)
	if err != nil {
		log.Fatalf("load secrets: %v", err)
	}
	allowDev := vault.GetBool("ATTEST_ALLOW_DEV", false)
	log.Printf("config: addr=%s db=%s secrets=%s appID=%s allowDev=%v",
		addr, dbPath, secretsPath, appID, allowDev)

	// One-time native_region localization backfill (SPEC §7 v5). Handled FIRST —
	// before the attest store, rate limiter, or HTTP wiring — so a one-shot
	// invocation (ENRICH_BACKFILL_NATIVE_REGION=1) running alongside the live
	// service stays minimal and never opens the attest credentials.db (no SQLite
	// contention with the serving process). It builds its own short-lived Supabase
	// pool, runs, and exits without starting the HTTP server. A no-op otherwise.
	maybeRunNativeRegionBackfill(vault)

	// One-time disease-severity backfill (DiseasePromptVersion v3). Same one-shot
	// posture as above (own short-lived pool, exits before the HTTP server). A
	// no-op unless ENRICH_BACKFILL_DISEASE_SEVERITY=1. Deterministic — needs only
	// SUPABASE_DB_URL (severity is derived from the English row's labels, no OpenAI).
	maybeRunDiseaseSeverityBackfill(vault)

	store, err := attest.OpenStore(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	verifier, err := attest.New(attest.Options{
		AppID:    appID,
		AllowDev: allowDev,
		Store:    store,
	})
	if err != nil {
		log.Fatalf("attest.New: %v", err)
	}

	lim := ratelimit.New(
		envIntOr("YARDMATE_API_RL_IP_LIMIT", defaultIPLimit),
		envDurationOr("YARDMATE_API_RL_IP_WINDOW", defaultIPWindow),
		envIntOr("YARDMATE_API_RL_KEYID_LIMIT", defaultKeyIDLimit),
		envDurationOr("YARDMATE_API_RL_KEYID_WINDOW", defaultKeyIDWindow),
		envIntOr("YARDMATE_API_RL_DEVICE_LIMIT", defaultDeviceLimit),
		envDurationOr("YARDMATE_API_RL_DEVICE_WINDOW", defaultDeviceWindow),
	)
	sweepStop := lim.StartSweeper(defaultSweepInterval)
	defer close(sweepStop)

	// Challenge-store sweeper: attest challenges persist in BoltDB until they
	// expire. Consume marks a challenge Consumed=true (kept, NOT deleted, so a
	// replay still trips ErrChallengeReplay), so THIS sweeper is the only cleanup
	// for both consumed and issued-but-never-consumed rows once past TTL. Mirrors
	// lim.StartSweeper's lifecycle: a ticker goroutine stopped on shutdown.
	challengeSweepStop := make(chan struct{})
	go func() {
		t := time.NewTicker(defaultChallengeSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-challengeSweepStop:
				return
			case now := <-t.C:
				if _, err := store.SweepExpired(now, verifier.ChallengeTTL()); err != nil {
					log.Printf("attest challenge sweep: %v", err)
				}
			}
		}
	}()
	defer close(challengeSweepStop)

	// Pl@ntNet proxy client — PRIMARY /v1/identify engine (SPEC §7).
	// Key never leaves server. Disabled if PLANTNET_API_KEY is missing;
	// then /v1/identify degrades to Plant.id-only (graceful, warn-logged).
	var plantNet *proxy.PlantNetClient
	if v := vault.Get("PLANTNET_API_KEY"); v != "" {
		// Reject a negative quota-warn threshold (a typo like "-5"): logQuota
		// WARNs on `remaining < threshold`, so a negative value never fires for
		// any real remaining count (incl. 0) and silently loses the exhaustion
		// signal. Fall back to the default, matching the pre-config-path parser's
		// non-negative validation (PR #78 review).
		quotaWarn := envIntOr("YARDMATE_API_PLANTNET_QUOTA_WARN", defaultPlantNetQuotaWarn)
		if quotaWarn < 0 {
			log.Printf("WARN: YARDMATE_API_PLANTNET_QUOTA_WARN=%d is negative; using default %d", quotaWarn, defaultPlantNetQuotaWarn)
			quotaWarn = defaultPlantNetQuotaWarn
		}
		plantNet = proxy.NewPlantNetClient(v, quotaWarn)
	} else {
		log.Printf("WARN: PLANTNET_API_KEY missing; /v1/identify primary engine disabled (Plant.id-only fallback)")
	}

	// Plant.id proxy client — /v1/identify FALLBACK + sole /v1/diagnose
	// engine (SPEC §7). Key never leaves server. Disabled if
	// PLANT_ID_API_KEY is missing; then /v1/diagnose is not registered and
	// /v1/identify runs Pl@ntNet-only.
	var plantID *proxy.PlantIDClient
	if v := vault.Get("PLANT_ID_API_KEY"); v != "" {
		plantID = proxy.NewPlantIDClient(v)
	} else {
		log.Printf("WARN: PLANT_ID_API_KEY missing; /v1/diagnose will not be registered (identify falls back to Pl@ntNet-only)")
	}

	// OpenAI vision client — drives the ai_enhance rerank on /v1/identify and
	// catalog-id disambiguation on /v1/diagnose. Optional: a missing key
	// leaves both as no-ops (Plant.id-only behavior).
	var vision *proxy.VisionClient
	if v := vault.Get("OPENAI_API_KEY"); v != "" {
		vision = proxy.NewVisionClient(v)
	} else {
		log.Printf("WARN: OPENAI_API_KEY missing; ai_enhance + catalog disambiguation disabled")
	}

	// iNaturalist taxa client — upgrades out-of-catalog common names on
	// /v1/identify (SPEC §2.1). No API key required; best-effort, never blocks.
	inat := proxy.NewINatClient()

	// Embedded content index (plants_index, plants_detail, diseases catalog).
	// Built once at startup; ~10 MB binary footprint.
	content, err := proxy.LoadContent()
	if err != nil {
		log.Fatalf("load content: %v", err)
	}
	log.Printf("content loaded: catalog ready")

	// Shared Supabase pgx pool — opened ONCE from SUPABASE_DB_URL, independent
	// of OpenAI/enrichment gating. Consumed by the enrichment service (only when
	// OPENAI_API_KEY is also present), the inline disease enrichment, AND the
	// account-delete route. Opening it here (not inside buildEnrichmentService)
	// means POST /v1/account/delete registers whenever SUPABASE_DB_URL is
	// configured even if OPENAI_API_KEY is absent (Codex #55: account deletion is
	// compliance-critical and must not 404 for an unrelated OpenAI config reason).
	// nil (with a WARN) when SUPABASE_DB_URL is missing or the ping fails.
	enrichDB := buildSupabaseDB(vault)

	// Enrichment service — V1 plant-detail enrichment endpoint
	// (proxy/enrichment/SPEC.md). Requires the shared Supabase pool (above) +
	// OPENAI_API_KEY. Gracefully disabled with a WARN log if either is missing.
	enrichSvc := buildEnrichmentService(vault, content, inat, enrichDB)

	// Disease enrichment — inline in /v1/diagnose for out-of-catalog diseases
	// (proxy/enrichment/SPEC_disease.md). REUSES the shared Supabase pgx pool;
	// nil (out-of-catalog issues stay slim) if that pool / OPENAI_API_KEY absent.
	diseaseSvc := buildDiseaseEnrichmentService(vault, content, enrichDB)

	// Translation self-heal sweep — the in-request backfill is fire-and-forget
	// (no retry; drops on queue saturation / restart / transient LLM error), so
	// legacy masters and lost jobs never re-translate on their own. This periodic
	// sweep re-enqueues just the missing languages for both plants and diseases.
	// Disabled by ENRICHMENT_SWEEP_INTERVAL="off"; cadence overridable with any
	// Go duration (e.g. "12h"); default 6h.
	startEnrichmentSweep(vault, enrichDB, enrichSvc, diseaseSvc)

	// Image-ingest service — fills out-of-catalog plant galleries on R2 via the
	// on-demand iNat→Wikimedia cascade (proxy/imageingest/SPEC.md). Requires R2
	// creds + SUPABASE_DB_URL + IMAGEINGEST_ADMIN_TOKEN; gracefully disabled
	// (nil + WARN) if any is missing, in which case neither the public
	// POST /v1/plants/imageingest nor the internal route is registered.
	// `content` supplies the authoritative catalog id→scientific_name map used by
	// the /v1/plants/catalog-images server-side name resolution (SPEC §2.8).
	ingestSvc := buildImageIngestService(vault, content)

	// Catalog name-index hot-load (proxy/SPEC.md §9). OFF by default: with
	// CATALOG_HOTLOAD_ENABLED unset/false the embedded plants_index baseline
	// (loaded above) is the sole source and no goroutine runs. When enabled it
	// polls the CDN plants_index.json and atomically swaps the name→id maps on
	// `content` so a freshly catalog-promoted plant becomes identify-resolvable
	// without a redeploy, zero downtime. Fail-safe: any poll error keeps the
	// embed/last-good table. Started AFTER buildImageIngestService so imageingest's
	// one-time CatalogScientificNames() snapshot deterministically captures the
	// embed baseline (SPEC §9.2 sciByID note), not a racing first-poll swap.
	startCatalogReloader(vault, content)

	// In-flight concurrency bound for the two image-buffering endpoints
	// (inflight/SPEC). Caps peak memory so a burst sheds cleanly (503) instead
	// of OOM-killing the process; overflow first waits up to waitBudget for a
	// slot, so most of it is served (a slightly slower 200) rather than shed.
	maxInflight := envIntOr("YARDMATE_API_INFLIGHT_MAX", defaultMaxInflight)
	inflightMaxWait := envIntOr("YARDMATE_API_INFLIGHT_MAX_WAIT", defaultInflightMaxWait)
	inflightWait := envDurationOr("YARDMATE_API_INFLIGHT_WAIT_BUDGET", defaultInflightWait)
	inflightLim := inflight.New(maxInflight, inflightMaxWait, inflightWait)
	log.Printf("inflight: maxConcurrent=%d maxWait=%d waitBudget=%s",
		maxInflight, inflightMaxWait, inflightWait)

	// /v1/doctor — conversational diagnosis (doctor/SPEC.md). Own inflight
	// bound (SSE streams hold slots for the whole 5–15 s generation) and own
	// hourly spend bucket; nil service (no OPENAI_API_KEY) leaves the route
	// unregistered.
	doctorSvc, doctorBudget := buildDoctorService(vault)
	doctorInflight := inflight.New(
		envIntOr("YARDMATE_API_DOCTOR_INFLIGHT_MAX", defaultDoctorInflight),
		envIntOr("YARDMATE_API_DOCTOR_INFLIGHT_MAX_WAIT", defaultDoctorInflightWait),
		inflightWait,
	)

	srv := newServer(verifier, vault, lim, plantNet, plantID, vision, inat, content, enrichSvc, diseaseSvc, ingestSvc, enrichDB, inflightLim, doctorSvc, doctorInflight, doctorBudget)

	// ReadTimeout / WriteTimeout cover the slowest endpoint (/v1/identify
	// streams to Plant.id, up to ~30 s upstream) with 5 s headroom = 35 s base.
	//
	// PLUS the in-flight limiter's queue wait (up to inflightWait): Go anchors
	// both deadlines at header-read, but the limiter makes an overflow request
	// wait AFTER header-read and BEFORE the handler sets reqStart. The handlers'
	// wall-clock budgets (identify rose budget / diagnose upstream + AI-fallback,
	// anchored at reqStart to fit under this WriteTimeout — proxy/handlers.go,
	// Codex #48 P2) assume a full 35 s window FROM reqStart. Adding inflightWait
	// here means that after the queue wait elapses, exactly 35 s remains from
	// reqStart, so a request that queued the full budget still gets its whole
	// downstream window instead of being cut off mid-response (PR #76 review).
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       35*time.Second + inflightWait,
		WriteTimeout:      35*time.Second + inflightWait,
		IdleTimeout:       60 * time.Second,
	}
	// Graceful shutdown: on SIGTERM/SIGINT stop accepting new connections and
	// drain in-flight requests via httpSrv.Shutdown, THEN let main return so the
	// deferred cleanup (store.Close, sweeper stops) actually runs — previously
	// log.Fatal(ListenAndServe) called os.Exit and skipped every defer. systemd's
	// default TimeoutStopSec (90 s) comfortably covers the 30 s drain deadline.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	log.Printf("yardmate-api listening on %s", addr)
	serveErr := make(chan error, 1)
	go func() {
		// ListenAndServe returns http.ErrServerClosed on graceful shutdown; only a
		// real listen/serve failure is surfaced here.
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		// Listener never came up (e.g. addr in use) — nothing to drain; exit.
		log.Fatal(err)
	case <-ctx.Done():
		stop() // restore default handling so a second signal force-kills
		log.Printf("shutdown signal received; draining in-flight requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}
}

// buildSupabaseDB opens the shared Supabase pgx pool from SUPABASE_DB_URL
// (Session Pooler DSN per SPEC §9 #15), INDEPENDENT of OpenAI/enrichment
// gating. This single pool is shared by every Supabase consumer — the
// enrichment service, inline disease enrichment, and the account-delete route —
// so it is opened exactly once. Returns nil (with a WARN log) when
// SUPABASE_DB_URL is missing or the initial ping fails; consumers degrade
// gracefully on nil (their routes stay unregistered).
//
// The pool's lifetime is the process lifetime; no graceful Close() on shutdown
// in V1 (systemd SIGTERM kills the process; Postgres reclaims connections via
// idle timeout).
func buildSupabaseDB(vault *secrets.Vault) *enrichment.DB {
	dsn := vault.Get("SUPABASE_DB_URL")
	if dsn == "" {
		log.Printf("WARN: SUPABASE_DB_URL missing; enrichment + disease enrichment + /v1/account/delete disabled")
		return nil
	}
	initCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := enrichment.NewDB(initCtx, dsn)
	if err != nil {
		log.Printf("WARN: Supabase DB init failed: %v; enrichment + disease enrichment + /v1/account/delete disabled", err)
		return nil
	}
	if err := db.Ping(initCtx); err != nil {
		log.Printf("WARN: Supabase DB ping failed: %v; enrichment + disease enrichment + /v1/account/delete disabled", err)
		db.Close()
		return nil
	}
	log.Printf("Supabase DB pool ready (shared by enrichment / disease enrichment / account-delete)")
	return db
}

// maybeRunNativeRegionBackfill runs the one-time native_region localization
// backfill (SPEC §7 v5) when ENRICH_BACKFILL_NATIVE_REGION=1, then exits the
// process — it is a maintenance task, not part of normal serving, and never
// starts the HTTP server. A no-op when the env var is unset. It is intentionally
// SELF-CONTAINED: it builds its own short-lived Supabase pool (closed before
// exit) and requires only SUPABASE_DB_URL + OPENAI_API_KEY, so a one-shot run can
// sit beside the live service without sharing or contending for its resources.
// Run it via the dedicated `yardmate-api-backfill.service` (Type=oneshot) or a
// manual `ENRICH_BACKFILL_NATIVE_REGION=1 /usr/local/bin/yardmate-api` — NEVER by
// baking the env var into the long-running service (it would exit after the
// backfill and take the HTTP server down, or loop under Restart=on-failure).
//
// Exit code reflects the OUTCOME: 0 only when the run is COMPLETE; 1 when deps
// are missing, the list query errored, OR any row is left unlocalized — both
// rep.Failed (transient translate/update error) AND rep.Skipped (model returned a
// wrong element count, row deliberately left English for a later retry). Either
// MUST NOT surface as a clean exit, or an operator/automation would believe a
// half-finished run (OpenAI rate-limited, or a region the model keeps mangling)
// completed and clear the env flag while non-English rows still hold English
// regions. rep.Vanished (row deleted between list and update) is benign.
func maybeRunNativeRegionBackfill(vault *secrets.Vault) {
	// Trigger is an OS ENV var (like the other YARDMATE_API_* config read via
	// envOr), NOT a secrets-file key: vault.Get reads only the secrets file, and
	// putting the flag there is unsafe — the long-running service reads the SAME
	// file and would run the backfill + exit on its next restart. Reading os.Getenv
	// lets a dedicated one-shot (yardmate-api-backfill.service Environment=, or a
	// manual `ENRICH_BACKFILL_NATIVE_REGION=1 /usr/local/bin/yardmate-api`) trigger
	// it in isolation while the serving unit, which never sets it, stays unaffected.
	if os.Getenv("ENRICH_BACKFILL_NATIVE_REGION") != "1" {
		return
	}
	openaiKey := vault.Get("OPENAI_API_KEY")
	db := buildSupabaseDB(vault) // own pool; independent of the serving path's enrichDB
	if db == nil || openaiKey == "" {
		log.Printf("ERROR: native_region backfill requested but Supabase pool (%v) or OPENAI_API_KEY (%v) missing", db != nil, openaiKey != "")
		os.Exit(1)
	}
	llm := enrichment.NewLLMClient(openaiKey)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := enrichment.RunNativeRegionBackfill(ctx, db, llm)
	db.Close() // os.Exit below skips deferred cleanup; release the pool explicitly
	if err != nil {
		log.Printf("ERROR: native_region backfill failed: %v", err)
		os.Exit(1)
	}
	log.Printf("native_region backfill report: total=%d updated=%d skipped=%d vanished=%d failed=%d",
		rep.Total, rep.Updated, rep.Skipped, rep.Vanished, rep.Failed)
	if rep.Failed > 0 || rep.Skipped > 0 {
		log.Printf("ERROR: native_region backfill INCOMPLETE: %d failed, %d skipped (arity mismatch); re-run to retry before clearing ENRICH_BACKFILL_NATIVE_REGION", rep.Failed, rep.Skipped)
		os.Exit(1)
	}
	os.Exit(0)
}

// maybeRunDiseaseSeverityBackfill runs the one-time disease-severity backfill
// (DiseasePromptVersion v3) when ENRICH_BACKFILL_DISEASE_SEVERITY=1, then exits
// the process. Like maybeRunNativeRegionBackfill it is a self-contained one-shot
// (own short-lived Supabase pool, never starts the HTTP server), but it is
// DETERMINISTIC and needs ONLY SUPABASE_DB_URL — severity is derived from each
// disease's English row labels, with no OpenAI call. A no-op when the env var is
// unset. Trigger it the same way (the dedicated yardmate-api-backfill.service
// one-shot, or a manual `ENRICH_BACKFILL_DISEASE_SEVERITY=1 /usr/local/bin/yardmate-api`),
// NEVER by baking the flag into the long-running serving unit.
//
// Exit code reflects the OUTCOME and distinguishes re-run-fixable from not:
//   - exit 1 (re-run after resolving): pool missing, list query errored, any row
//     update Failed (transient), any disease skipped for no English row (run the
//     translation sweep first), or any Mismatch (a row's group count diverged from
//     English — a data anomaly to investigate). The strict code stops automation
//     from clearing the flag while these remain.
//   - exit 0 with a loud WARN: Undetermined diseases (severity-graded but their
//     English labels lack the mild/severe keyword, so the deterministic sniff
//     can't classify them). These are NOT re-run-fixable — forcing exit 1 would
//     mean the unit can never go green — so they are surfaced as a warning with a
//     count and per-disease log lines (from the runner) for manual/LLM follow-up.
//   - Vanished (row deleted between list and update) is benign.
//
// Note: if BOTH ENRICH_BACKFILL_NATIVE_REGION=1 and ENRICH_BACKFILL_DISEASE_SEVERITY=1
// are set in one invocation, only native_region runs (it os.Exit()s first, above).
// Run the two one-shots separately (that is the dedicated-unit pattern anyway).
func maybeRunDiseaseSeverityBackfill(vault *secrets.Vault) {
	if os.Getenv("ENRICH_BACKFILL_DISEASE_SEVERITY") != "1" {
		return
	}
	db := buildSupabaseDB(vault) // own pool; independent of the serving path's enrichDB
	if db == nil {
		log.Printf("ERROR: disease severity backfill requested but Supabase pool missing (SUPABASE_DB_URL)")
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := enrichment.RunDiseaseSeverityBackfill(ctx, db)
	db.Close() // os.Exit below skips deferred cleanup; release the pool explicitly
	if err != nil {
		log.Printf("ERROR: disease severity backfill failed: %v", err)
		os.Exit(1)
	}
	log.Printf("disease severity backfill report: diseases=%d noEnglish=%d updated=%d alreadyOK=%d undetermined=%d mismatch=%d vanished=%d failed=%d",
		rep.Diseases, rep.NoEnglish, rep.Updated, rep.AlreadyOK, rep.Undetermined, rep.Mismatch, rep.Vanished, rep.Failed)
	if rep.Undetermined > 0 {
		log.Printf("WARN: disease severity backfill: %d disease(s) are severity-graded but their English labels lack a mild/severe keyword (logged above); they remain unbadged and need manual/LLM classification — re-running will NOT fix them", rep.Undetermined)
	}
	if rep.Failed > 0 || rep.NoEnglish > 0 || rep.Mismatch > 0 {
		log.Printf("ERROR: disease severity backfill INCOMPLETE: %d failed, %d skipped (no English row), %d mismatch (data anomaly); resolve and re-run before clearing ENRICH_BACKFILL_DISEASE_SEVERITY", rep.Failed, rep.NoEnglish, rep.Mismatch)
		os.Exit(1)
	}
	os.Exit(0)
}

// startEnrichmentSweep wires and launches the periodic translation self-heal
// sweep (proxy/enrichment/sweep.go) for both plants and diseases. Unlike
// maybeRunNativeRegionBackfill (a one-shot that exits the process), this runs
// for the process lifetime as a background goroutine sharing the serving pool +
// the already-built backfiller worker pools, so it never contends for its own
// resources. No-op when the shared pool is absent or neither enrichment side is
// enabled. Cadence comes from the ENRICHMENT_SWEEP_INTERVAL env var: "off"
// disables it, any Go duration overrides the default; an unset/garbage value
// falls back to the package default (6h).
func startEnrichmentSweep(vault *secrets.Vault, db *enrichment.DB, enrichSvc *enrichment.Service, diseaseSvc proxy.DiseaseEnricher) {
	if db == nil {
		return
	}
	raw := os.Getenv("ENRICHMENT_SWEEP_INTERVAL")
	if raw == "off" {
		log.Printf("enrichment sweep: disabled by ENRICHMENT_SWEEP_INTERVAL=off")
		return
	}
	interval, err := time.ParseDuration(raw) // raw=="" → err, interval==0 → NewSweeper default
	if raw != "" && err != nil {
		log.Printf("WARN: ENRICHMENT_SWEEP_INTERVAL=%q not a valid duration; using default", raw)
		interval = 0
	}
	plantBF := enrichSvc.Backfiller() // nil-safe accessor (nil service → nil)
	var diseaseBF *enrichment.DiseaseBackfiller
	if ds, ok := diseaseSvc.(*enrichment.DiseaseService); ok {
		diseaseBF = ds.Backfiller()
	}
	enrichment.NewSweeper(db, plantBF, diseaseBF, interval).Start(context.Background())
}

// Catalog name-index hot-load defaults (proxy/SPEC.md §9). The URL default is
// the prod CDN object published by yardmate-content/publish.sh; staging
// overrides it to the content-staging/ prefix via CATALOG_HOTLOAD_URL.
const (
	defaultCatalogHotloadURL      = "https://images.yardmate.ai/content/plants_index.json"
	defaultCatalogHotloadInterval = 10 * time.Minute
	minCatalogHotloadInterval     = 1 * time.Minute
)

// startCatalogReloader wires runtime catalog name-index hot-load (proxy/SPEC.md
// §9). OFF by default (kill-switch): with CATALOG_HOTLOAD_ENABLED unset/false it
// logs and returns — the embedded plants_index baseline is the sole source and
// no goroutine starts, byte-identical to pre-hot-load behaviour. When enabled it
// starts a background poller that conditional-GETs the CDN plants_index.json
// (ETag version gate) and atomically swaps the name→id maps on `content`, so a
// catalog-promoted plant is recognised by /v1/identify within one poll interval
// with zero downtime and no redeploy. All three knobs read the secrets vault
// (not os.Getenv) so they live in secrets.env with the other feature flags
// (Codex #23: vault-only config read via os.Getenv silently no-ops).
func startCatalogReloader(vault *secrets.Vault, content *proxy.ContentIndex) {
	if !vault.GetBool("CATALOG_HOTLOAD_ENABLED", false) {
		log.Printf("catalog hot-load: disabled (CATALOG_HOTLOAD_ENABLED unset/false); identify uses embedded plants_index")
		return
	}
	url := vault.Get("CATALOG_HOTLOAD_URL")
	if url == "" {
		url = defaultCatalogHotloadURL
	}
	interval := vaultDurationOr(vault, "CATALOG_HOTLOAD_INTERVAL", defaultCatalogHotloadInterval)
	if interval < minCatalogHotloadInterval {
		log.Printf("catalog hot-load: interval %v below %v floor; clamping to floor", interval, minCatalogHotloadInterval)
		interval = minCatalogHotloadInterval
	}
	proxy.NewCatalogReloader(content, url, interval).Start(context.Background())
	log.Printf("catalog hot-load: enabled url=%s interval=%s", url, interval)
}

// buildEnrichmentService wires the /v1/plants/enrichment dependencies: the
// shared Supabase pool (db, opened by buildSupabaseDB), OpenAI LLM client, and
// the in-process LRU cache. Returns nil (with a WARN log) if db is nil (no
// SUPABASE_DB_URL / ping failed) or OPENAI_API_KEY is missing — in that case
// the route stays unregistered. Does NOT own the pool's lifetime (main does),
// so it never opens or closes it.
func buildEnrichmentService(vault *secrets.Vault, content *proxy.ContentIndex, inat *proxy.INatClient, db *enrichment.DB) *enrichment.Service {
	openaiKey := vault.Get("OPENAI_API_KEY")
	if db == nil || openaiKey == "" {
		log.Printf("WARN: shared Supabase pool or OPENAI_API_KEY missing (sharedDB=%v, openaiKey=%v); /v1/plants/enrichment disabled", db != nil, openaiKey != "")
		return nil
	}
	llm := enrichment.NewLLMClient(openaiKey)
	cache := enrichment.NewCache(0, 0) // defaults: 10k entries, 30 min TTL
	svc := enrichment.NewService(content, db, llm, cache, inat)
	svc.SetBackfiller(enrichment.NewBackfiller(db, llm)) // async multi-language translation backfill (SPEC §7)
	log.Printf("enrichment service ready: shared db pool + LRU cache + LLM %s + %d-language backfill",
		enrichment.SourceTag, len(enrichment.SupportedLangs))
	return svc
}

// buildDiseaseEnrichmentService wires the inline disease enrichment used by
// /v1/diagnose for out-of-catalog diseases (proxy/enrichment/SPEC_disease.md).
// REUSES the shared Supabase pgx pool (db, from buildSupabaseDB) — no second
// pool. Returns a nil interface (diagnose leaves out-of-catalog issues slim,
// never errors) when that pool is absent (no SUPABASE_DB_URL / ping failed) or
// OPENAI_API_KEY is missing.
func buildDiseaseEnrichmentService(vault *secrets.Vault, content *proxy.ContentIndex, db *enrichment.DB) proxy.DiseaseEnricher {
	openaiKey := vault.Get("OPENAI_API_KEY")
	if db == nil || openaiKey == "" {
		log.Printf("WARN: disease enrichment disabled (sharedDB=%v, openaiKey=%v); out-of-catalog diagnoses stay slim", db != nil, openaiKey != "")
		return nil
	}
	llm := enrichment.NewDiseaseLLMClient(openaiKey)
	cache := enrichment.NewDiseaseCache(0, 0) // defaults: 10k entries, 30 min TTL
	svc := enrichment.NewDiseaseService(content, db, llm, cache)
	svc.SetBackfiller(enrichment.NewDiseaseBackfiller(db, llm)) // async multi-language translation backfill (SPEC §7)
	log.Printf("disease enrichment ready: shared db pool + LRU cache + LLM %s + %d-language backfill",
		enrichment.DiseaseSourceTag, len(enrichment.SupportedLangs))
	return svc
}

// buildImageIngestService wires the proxy/imageingest dependencies: R2 client
// (S3 SDK), the two-table plant_image_species/files ledger (own small pgx pool,
// MaxConns=2), and the iNaturalist + Wikimedia cascade source clients. Returns
// nil (with a WARN log) if any required secret is missing or a DB ping fails —
// in that case neither the public nor the internal route is registered
// (mirrors buildEnrichmentService graceful-disable).
//
// Required secrets: R2_ACCESS_KEY_ID + R2_SECRET_ACCESS_KEY + R2_BUCKET +
// (R2_ENDPOINT or R2_ACCOUNT_ID), SUPABASE_DB_URL, IMAGEINGEST_ADMIN_TOKEN.
// The R2 creds + admin token are SERVER-ONLY and MUST NOT be in vendedKeys.
//
// Pool lifetime is the process lifetime (no graceful Close on shutdown in V1;
// systemd SIGTERM kills the process, Postgres reclaims via idle timeout —
// same stance as buildEnrichmentService).
//
// content supplies the authoritative catalog id→scientific_name map
// (CatalogScientificNames) injected as Config.CatalogNames — the server-side
// search name for /v1/plants/catalog-images (SPEC §2.8, Codex P1). It is always
// non-nil here: main fatals if LoadContent fails before this is called.
func buildImageIngestService(vault *secrets.Vault, content *proxy.ContentIndex) *imageingest.Service {
	dsn := vault.Get("SUPABASE_DB_URL")
	adminToken := vault.Get("IMAGEINGEST_ADMIN_TOKEN")
	r2Cfg := imageingest.R2Config{
		AccountID:       vault.Get("R2_ACCOUNT_ID"),
		AccessKeyID:     vault.Get("R2_ACCESS_KEY_ID"),
		SecretAccessKey: vault.Get("R2_SECRET_ACCESS_KEY"),
		Bucket:          vault.Get("R2_BUCKET"),
		Endpoint:        vault.Get("R2_ENDPOINT"),
	}
	if dsn == "" || adminToken == "" || r2Cfg.AccessKeyID == "" ||
		r2Cfg.SecretAccessKey == "" || r2Cfg.Bucket == "" ||
		(r2Cfg.Endpoint == "" && r2Cfg.AccountID == "") {
		log.Printf("WARN: R2 creds / SUPABASE_DB_URL / IMAGEINGEST_ADMIN_TOKEN missing; image ingest disabled")
		return nil
	}

	r2Client, err := imageingest.NewR2Client(r2Cfg)
	if err != nil {
		log.Printf("WARN: image ingest R2 init failed: %v; disabled", err)
		return nil
	}

	initCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ledger, err := imageingest.NewLedger(initCtx, dsn)
	if err != nil {
		log.Printf("WARN: image ingest ledger init failed: %v; disabled", err)
		return nil
	}
	if err := ledger.Ping(initCtx); err != nil {
		log.Printf("WARN: image ingest ledger ping failed: %v; disabled", err)
		ledger.Close()
		return nil
	}

	maxBytes := int64(vaultIntOr(vault, "IMAGEINGEST_MAX_BYTES", 0))
	inat := sources.NewINatClient(sources.INatOptions{
		UserAgent: vault.Get("INATURALIST_USER_AGENT"), // empty → built-in default UA
		MaxBytes:  maxBytes,
	})
	wikimedia := sources.NewWikimediaClient(sources.WikimediaOptions{
		UserAgent: vault.Get("IMAGEINGEST_USER_AGENT"), // empty → built-in default UA
		MaxBytes:  maxBytes,
	})

	cfg := imageingest.Config{
		AllowAttributionLicenses: vault.GetBool("IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES", false),
		MinInterval:              vaultDurationOr(vault, "IMAGEINGEST_MIN_INTERVAL", time.Second),
		// Authoritative catalog id→scientific_name map from the embedded
		// plants_index.json (SPEC §2.8 catalog-images P1 fix): the catalog-images
		// search name is derived server-side from catalog_id, never the client's
		// scientific_name. Reuses the single existing embed (no second copy).
		CatalogNames: content.CatalogScientificNames(),
	}
	ingestor := imageingest.NewIngestor(inat, wikimedia, r2Client, ledger, cfg)
	log.Printf("image ingest service ready: R2 bucket=%s ledger pool + iNat/Wikimedia cascade (allowAttribution=%v)",
		r2Cfg.Bucket, cfg.AllowAttributionLicenses)
	return imageingest.NewService(ingestor, adminToken)
}

// buildDoctorService wires POST /v1/doctor (doctor/SPEC.md). Needs only the
// OpenAI key; absent → nil + WARN and the route stays unregistered (same
// degrade pattern as the other optional services). All knobs read the vault,
// not os.Getenv (Codex #23): DOCTOR_MODEL (whitelist-checked in NewService,
// default gpt-4o-mini), DOCTOR_ALLOW_MODEL_OVERRIDE (dev/staging only — an
// override request in prod is silently ignored, never an error), and
// DOCTOR_HOURLY_BUDGET for the endpoint's own spend bucket.
func buildDoctorService(vault *secrets.Vault) (*doctor.Service, *ratelimit.Bucket) {
	// 默认关（同 OOB_ESCAPE / VISION_KNN 的新功能惯例）：把「部署二进制」和
	// 「开放付费端点」拆成两个动作。prod 的 vault 里本就有 OPENAI_API_KEY，
	// 没有这道闸，任何一次常规部署都会顺手把新付费面推上公网 —— 客户端还没
	// 发版、没人该调它的时候。staging 显式 DOCTOR_ENABLED=true 验证，
	// app 版本准备上架时 prod 再翻开。
	if !vault.GetBool("DOCTOR_ENABLED", false) {
		log.Printf("doctor: disabled (DOCTOR_ENABLED unset/false); /v1/doctor not registered")
		return nil, nil
	}
	openaiKey := vault.Get("OPENAI_API_KEY")
	if openaiKey == "" {
		log.Printf("WARN: OPENAI_API_KEY missing; /v1/doctor disabled")
		return nil, nil
	}
	svc := doctor.NewService(
		openaiKey,
		"", // default OpenAI endpoint
		vault.Get("DOCTOR_MODEL"),
		vault.GetBool("DOCTOR_ALLOW_MODEL_OVERRIDE", false),
	)
	// 分轮模型：前 LeadTurns 轮 DOCTOR_MODEL，之后 DOCTOR_FOLLOWUP_MODEL（未设 = 不分轮）
	if fm := vault.Get("DOCTOR_FOLLOWUP_MODEL"); fm != "" {
		if doctor.ModelAllowed(fm) {
			svc.FollowupModel = fm
		} else {
			log.Printf("WARN: DOCTOR_FOLLOWUP_MODEL %q not in whitelist; ignoring", fm)
		}
	}
	svc.LeadTurns = vaultIntOr(vault, "DOCTOR_LEAD_TURNS", 2)
	// grok 系上游：配了 XAI_API_KEY 才建（缺省零 xAI 面；选 grok 自动回落并告警）
	if xaiKey := vault.Get("XAI_API_KEY"); xaiKey != "" {
		svc.XAIClient = doctor.NewClient(xaiKey, doctor.XAIEndpoint)
		svc.XAIClient.SetResponseHeaderTimeout(120 * time.Second)
		log.Printf("doctor: xAI upstream ready (grok models selectable)")
	}
	budget := ratelimit.NewBucket(vaultIntOr(vault, "DOCTOR_HOURLY_BUDGET", defaultDoctorHourlyBudget), time.Hour)
	log.Printf("doctor service ready: model=%s followup=%s leadTurns=%d override=%v budget=%d/h",
		svc.Model, svc.FollowupModel, svc.LeadTurns, svc.AllowOverride, vaultIntOr(vault, "DOCTOR_HOURLY_BUDGET", defaultDoctorHourlyBudget))
	return svc, budget
}

// vaultDurationOr / vaultIntOr read tuning knobs from the secrets Vault (the
// operator-edited /etc/yardmate-api/secrets.env), NOT os.Getenv. The systemd
// unit does not export secrets.env into the process environment (no
// EnvironmentFile=), so IMAGEINGEST_* knobs documented in secrets.env.example
// are only reachable via the Vault — reading them with os.Getenv left the
// ticker permanently off when an operator enabled it in secrets.env (Codex #23).
func vaultDurationOr(vault *secrets.Vault, key string, def time.Duration) time.Duration {
	v := vault.Get(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	log.Printf("vault %s=%q not a duration, using default %v", key, v, def)
	return def
}

func vaultIntOr(vault *secrets.Vault, key string, def int) int {
	v := vault.Get(key)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	log.Printf("vault %s=%q not an int, using default %d", key, v, def)
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("env %s=%q not an int, using default %d", key, v, def)
	}
	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("env %s=%q not a duration, using default %v", key, v, def)
	}
	return def
}
