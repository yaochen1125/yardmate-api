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
	"github.com/yaochen1125/yardmate-api/ratelimit"
	"github.com/yaochen1125/yardmate-api/secrets"
)

const (
	defaultAddr        = "127.0.0.1:8080"
	defaultDBPath      = "/var/lib/yardmate-api/credentials.db"
	defaultSecretsPath = "/etc/yardmate-api/secrets.env"
	defaultAppID       = "PMX32RG52M.com.chenyao.plantapp"

	// Rate-limit defaults (see ratelimit/SPEC §3).
	defaultIPLimit       = 100
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

	srv := newServer(verifier, vault, lim, plantNet, plantID, vision, content, enrichSvc)
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
