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

	// Embedded content index (plants_index, plants_detail, diseases catalog).
	// Built once at startup; ~10 MB binary footprint.
	content, err := proxy.LoadContent()
	if err != nil {
		log.Fatalf("load content: %v", err)
	}
	log.Printf("content loaded: catalog ready")

	// Enrichment service — V1 plant-detail enrichment endpoint
	// (proxy/enrichment/SPEC.md). Requires SUPABASE_DB_URL (Session Pooler
	// DSN per SPEC §9 #15) + OPENAI_API_KEY. Gracefully disabled with a
	// WARN log if either is missing or the DB ping fails.
	enrichSvc := buildEnrichmentService(vault, content)

	// Image-ingest service — fills out-of-catalog plant hero images on R2 from
	// Wikimedia Commons (proxy/imageingest/SPEC.md). Requires R2 creds +
	// SUPABASE_DB_URL + IMAGEINGEST_ADMIN_TOKEN; gracefully disabled (nil +
	// WARN) if any is missing, in which case the /internal route is not
	// registered and the ticker does not start.
	ingestSvc := buildImageIngestService(vault)

	srv := newServer(verifier, vault, lim, plantNet, plantID, vision, content, enrichSvc, ingestSvc)

	// Optional background ingest ticker (proxy/imageingest/SPEC.md §2.1).
	// Disabled by default (IMAGEINGEST_TICK_INTERVAL=0/unset → manual-only).
	// Started only when the service is configured AND the interval > 0; the
	// single-flight guard in RunBatch prevents overlapping passes.
	if ingestSvc != nil {
		if tick := envDurationOr("IMAGEINGEST_TICK_INTERVAL", 0); tick > 0 {
			stop := ingestSvc.Start(tick)
			defer stop()
			log.Printf("imageingest ticker started: interval=%v", tick)
		} else {
			log.Printf("imageingest ticker disabled (IMAGEINGEST_TICK_INTERVAL unset/0); manual trigger only")
		}
	}
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

// buildEnrichmentService wires the /v1/plants/enrichment dependencies:
// pgx pool against Supabase, OpenAI LLM client, and the in-process LRU cache.
// Returns nil (with a WARN log) if required secrets are missing or the
// initial DB ping fails — in that case the route stays unregistered.
//
// The DB pool's lifetime is the process lifetime; no graceful Close() on
// shutdown in V1 (systemd SIGTERM kills the process; Postgres reclaims
// connections via idle timeout).
func buildEnrichmentService(vault *secrets.Vault, content *proxy.ContentIndex) *enrichment.Service {
	dsn := vault.Get("SUPABASE_DB_URL")
	openaiKey := vault.Get("OPENAI_API_KEY")
	if dsn == "" || openaiKey == "" {
		log.Printf("WARN: SUPABASE_DB_URL or OPENAI_API_KEY missing; /v1/plants/enrichment disabled")
		return nil
	}
	initCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := enrichment.NewDB(initCtx, dsn)
	if err != nil {
		log.Printf("WARN: enrichment DB init failed: %v; /v1/plants/enrichment disabled", err)
		return nil
	}
	if err := db.Ping(initCtx); err != nil {
		log.Printf("WARN: enrichment DB ping failed: %v; /v1/plants/enrichment disabled", err)
		db.Close()
		return nil
	}
	llm := enrichment.NewLLMClient(openaiKey)
	cache := enrichment.NewCache(0, 0) // defaults: 10k entries, 30 min TTL
	log.Printf("enrichment service ready: db pool + LRU cache + LLM %s", enrichment.SourceTag)
	return enrichment.NewService(content, db, llm, cache)
}

// buildImageIngestService wires the proxy/imageingest dependencies: R2 client
// (S3 SDK), the plant_image_ingest ledger + plants_pending seed reader (both
// own small pgx pools, MaxConns=2), and the Wikimedia Commons client. Returns
// nil (with a WARN log) if any required secret is missing or a DB ping fails —
// in that case the /internal route stays unregistered and the ticker never
// starts (mirrors buildEnrichmentService graceful-disable).
//
// Required secrets: R2_ACCESS_KEY_ID + R2_SECRET_ACCESS_KEY + R2_BUCKET +
// (R2_ENDPOINT or R2_ACCOUNT_ID), SUPABASE_DB_URL, IMAGEINGEST_ADMIN_TOKEN.
// The R2 creds + admin token are SERVER-ONLY and MUST NOT be in vendedKeys.
//
// Pool lifetime is the process lifetime (no graceful Close on shutdown in V1;
// systemd SIGTERM kills the process, Postgres reclaims via idle timeout —
// same stance as buildEnrichmentService).
func buildImageIngestService(vault *secrets.Vault) *imageingest.Service {
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
	seeds, err := imageingest.NewSeedReader(initCtx, dsn)
	if err != nil {
		log.Printf("WARN: image ingest seed reader init failed: %v; disabled", err)
		ledger.Close()
		return nil
	}

	commons := imageingest.NewCommonsClient(imageingest.CommonsOptions{
		UserAgent: vault.Get("IMAGEINGEST_USER_AGENT"), // empty → built-in default UA
		MaxBytes:  int64(envIntOr("IMAGEINGEST_MAX_BYTES", 0)),
	})

	cfg := imageingest.Config{
		AllowAttributionLicenses: vault.GetBool("IMAGEINGEST_ALLOW_ATTRIBUTION_LICENSES", false),
		MinInterval:              envDurationOr("IMAGEINGEST_MIN_INTERVAL", time.Second),
		BatchLimit:               envIntOr("IMAGEINGEST_BATCH_LIMIT", 25),
	}
	ingestor := imageingest.NewIngestor(commons, r2Client, ledger, seeds, cfg)
	log.Printf("image ingest service ready: R2 bucket=%s ledger+seed pools + commons (allowAttribution=%v batchLimit=%d)",
		r2Cfg.Bucket, cfg.AllowAttributionLicenses, cfg.BatchLimit)
	return imageingest.NewService(ingestor, adminToken)
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
