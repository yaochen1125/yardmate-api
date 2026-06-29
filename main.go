// Package main is the entry point of yardmate-api, the App-Attest-gated
// secret-vending HTTP service for the YardMate iOS app. See attest/SPEC.md
// and secrets/SPEC.md for the design contracts.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/yaochen1125/yardmate-api/attest"
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
	// bucket (100/hr) regardless of this ceiling, so raising per-IP does NOT
	// widen the upstream-cost / abuse surface.
	defaultIPLimit       = 600
	defaultIPWindow      = time.Hour
	defaultKeyIDLimit    = 50
	defaultKeyIDWindow   = 24 * time.Hour
	defaultDeviceLimit   = 100
	defaultDeviceWindow  = time.Hour
	defaultSweepInterval = time.Minute
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

	// Pl@ntNet proxy client — PRIMARY /v1/identify engine (SPEC §7).
	// Key never leaves server. Disabled if PLANTNET_API_KEY is missing;
	// then /v1/identify degrades to Plant.id-only (graceful, warn-logged).
	var plantNet *proxy.PlantNetClient
	if v := vault.Get("PLANTNET_API_KEY"); v != "" {
		plantNet = proxy.NewPlantNetClient(v)
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

	// One-time native_region localization backfill (SPEC §7 v5). Operator sets
	// ENRICH_BACKFILL_NATIVE_REGION=1 for a single run; the process localizes the
	// legacy non-English rows and exits WITHOUT starting the HTTP server. Removing
	// the env var restores normal serving. Gated here so it has the shared pool +
	// OPENAI_API_KEY already resolved.
	maybeRunNativeRegionBackfill(vault, enrichDB)

	// Enrichment service — V1 plant-detail enrichment endpoint
	// (proxy/enrichment/SPEC.md). Requires the shared Supabase pool (above) +
	// OPENAI_API_KEY. Gracefully disabled with a WARN log if either is missing.
	enrichSvc := buildEnrichmentService(vault, content, inat, enrichDB)

	// Disease enrichment — inline in /v1/diagnose for out-of-catalog diseases
	// (proxy/enrichment/SPEC_disease.md). REUSES the shared Supabase pgx pool;
	// nil (out-of-catalog issues stay slim) if that pool / OPENAI_API_KEY absent.
	diseaseSvc := buildDiseaseEnrichmentService(vault, content, enrichDB)

	// Image-ingest service — fills out-of-catalog plant galleries on R2 via the
	// on-demand iNat→Wikimedia cascade (proxy/imageingest/SPEC.md). Requires R2
	// creds + SUPABASE_DB_URL + IMAGEINGEST_ADMIN_TOKEN; gracefully disabled
	// (nil + WARN) if any is missing, in which case neither the public
	// POST /v1/plants/imageingest nor the internal route is registered.
	// `content` supplies the authoritative catalog id→scientific_name map used by
	// the /v1/plants/catalog-images server-side name resolution (SPEC §2.8).
	ingestSvc := buildImageIngestService(vault, content)

	srv := newServer(verifier, vault, lim, plantNet, plantID, vision, inat, content, enrichSvc, diseaseSvc, ingestSvc, enrichDB)

	// ReadTimeout / WriteTimeout cover the slowest endpoint (/v1/identify
	// streams to Plant.id, up to ~30 s upstream). Headroom 5 s.
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       35 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("yardmate-api listening on %s", addr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatal(err)
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
// process — it is a maintenance task, not part of normal serving. A no-op when
// the env var is unset. Requires the shared Supabase pool + OPENAI_API_KEY; if
// either is missing it logs and exits non-zero so the operator notices.
//
// Exit code reflects the OUTCOME: 0 only when no row failed; 1 when the list
// query errored OR any per-row translate/update failed (rep.Failed > 0). Per-row
// failures are non-fatal to the run (best-effort, logged + skipped) but MUST NOT
// surface as a clean exit, or an operator would believe a half-failed run
// (e.g. OpenAI rate-limited) finished and remove the env flag prematurely.
func maybeRunNativeRegionBackfill(vault *secrets.Vault, db *enrichment.DB) {
	if vault.Get("ENRICH_BACKFILL_NATIVE_REGION") != "1" {
		return
	}
	openaiKey := vault.Get("OPENAI_API_KEY")
	if db == nil || openaiKey == "" {
		log.Printf("ERROR: native_region backfill requested but shared db (%v) or OPENAI_API_KEY (%v) missing", db != nil, openaiKey != "")
		os.Exit(1)
	}
	llm := enrichment.NewLLMClient(openaiKey)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := enrichment.RunNativeRegionBackfill(ctx, db, llm)
	// Release the shared pool explicitly: os.Exit below skips main's deferred
	// cleanup, and this maintenance path never starts the HTTP server.
	db.Close()
	if err != nil {
		log.Printf("ERROR: native_region backfill failed: %v", err)
		os.Exit(1)
	}
	log.Printf("native_region backfill report: total=%d updated=%d skipped=%d vanished=%d failed=%d",
		rep.Total, rep.Updated, rep.Skipped, rep.Vanished, rep.Failed)
	if rep.Failed > 0 {
		log.Printf("ERROR: native_region backfill had %d per-row failure(s); re-run to retry them", rep.Failed)
		os.Exit(1)
	}
	os.Exit(0)
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
	log.Printf("disease enrichment ready: shared db pool + LRU cache + LLM %s", enrichment.DiseaseSourceTag)
	return enrichment.NewDiseaseService(content, db, llm, cache)
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
