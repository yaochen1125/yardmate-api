// Package main is the entry point of yardmate-api, the App-Attest-gated
// secret-vending HTTP service for the YardMate iOS app. See attest/SPEC.md
// and secrets/SPEC.md for the design contracts.
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/proxy"
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

	// Plant.id proxy client (V1 — server-side image identification).
	// Key never leaves server. Disabled if PLANT_ID_API_KEY is missing
	// (deploy.sh enforces presence in production secrets.env).
	var plantID *proxy.PlantIDClient
	if v := vault.Get("PLANT_ID_API_KEY"); v != "" {
		plantID = proxy.NewPlantIDClient(v)
	} else {
		log.Printf("WARN: PLANT_ID_API_KEY missing; /v1/identify will not be registered")
	}

	srv := newServer(verifier, vault, lim, plantID)
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
