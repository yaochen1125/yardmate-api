package rarity

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// artifactRelPath is the artifact's path under the content prefix — the
	// plantdex contract's fixed address (client resolves
	// {content-base}/dex/rarity.json).
	artifactRelPath = "dex/rarity.json"
	// cacheControl matches the flat-content publish default (publish.sh:
	// public,max-age=60): a short edge TTL is the whole freshness story since
	// this artifact deliberately does NOT bump version.txt (SPEC.md §Gate) and
	// the publisher does no CF purge.
	cacheControl = "public, max-age=60"
	// bootDelay lets the process finish cold start before the first publish
	// (mirrors enrichment sweepStartupDelay).
	bootDelay = 2 * time.Minute
	// minInterval floors RARITY_PUBLISH_INTERVAL (mirrors the catalog
	// hot-load clamp stance; sub-10m republish of a daily aggregate is noise).
	minInterval = 10 * time.Minute
	// publishTimeout bounds one publish pass (one aggregate query + one small
	// R2 read + one small R2 write).
	publishTimeout = 2 * time.Minute
	// rebuildTimeout bounds the synchronous admin rebuild request.
	rebuildTimeout = 2 * time.Minute
	// AdminTokenHeader carries the rebuild gate token
	// (imageingest X-Ingest-Admin-Token pattern).
	AdminTokenHeader = "X-Rarity-Admin-Token"
)

// CountSource supplies the per-species all-time scan totals.
// *enrichment.DB implements it (IdentifyCountTotals).
type CountSource interface {
	IdentifyCountTotals(ctx context.Context) (map[string]int64, error)
}

// ObjectStore is the narrow R2 surface the publisher needs.
// *imageingest.R2Client satisfies it in production; tests inject fakes.
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte, contentType, cacheControl string) error
	// Get returns (body, found, err); found=false (no error) on a 404.
	Get(ctx context.Context, key string) ([]byte, bool, error)
}

// Publisher owns the periodic aggregate→publish pass and the manual rebuild
// endpoint. A single mutex serializes passes (ticker vs admin) so concurrent
// rebuilds can't interleave the version read-modify-write (credits.go
// creditsMu stance).
type Publisher struct {
	counts CountSource
	store  ObjectStore
	cfg    Config
	key    string
	mu     sync.Mutex
}

// NewPublisher normalizes cfg (defaults + clamps, logged when they bite) and
// builds the publisher. counts/store must be non-nil — main only constructs
// the publisher once both dependencies exist.
func NewPublisher(counts CountSource, store ObjectStore, cfg Config) *Publisher {
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	if prefix == "" {
		prefix = "content"
	}
	cfg.Prefix = prefix
	if cfg.MinSample < 1 {
		if cfg.MinSample != 0 {
			log.Printf("rarity: MinSample %d below 1; clamping to 1", cfg.MinSample)
		}
		cfg.MinSample = 1
	}
	if cfg.MinSpecies < 0 {
		cfg.MinSpecies = 0
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	} else if cfg.Interval < minInterval {
		log.Printf("rarity: interval %v below %v floor; clamping to floor", cfg.Interval, minInterval)
		cfg.Interval = minInterval
	}
	var zero [4]float64
	if cfg.TierCuts == zero {
		cfg.TierCuts = DefaultTierCuts
	}
	return &Publisher{
		counts: counts,
		store:  store,
		cfg:    cfg,
		key:    prefix + "/" + artifactRelPath,
	}
}

// AdminToken exposes the (possibly empty) rebuild gate so server.go can skip
// registering the internal route when unset.
func (p *Publisher) AdminToken() string { return p.cfg.AdminToken }

// Key exposes the full R2 object key (logs / tests).
func (p *Publisher) Key() string { return p.key }

// Start launches the periodic publish loop; returns immediately. Safe on nil.
// Loop shape mirrors enrichment/sweep.go: top-level recover (last resort —
// loop exits, process lives) + per-tick recover (one bad pass never stops the
// schedule).
func (p *Publisher) Start(ctx context.Context) {
	if p == nil {
		return
	}
	go p.loop(ctx)
	log.Printf("rarity publish: scheduled every %s → %s (first run in %s)", p.cfg.Interval, p.key, bootDelay)
}

func (p *Publisher) loop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("rarity publish: loop panic recovered (publishing disabled): %v", r)
		}
	}()
	timer := time.NewTimer(bootDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("rarity publish: pass panic recovered: %v", r)
					}
				}()
				pctx, cancel := context.WithTimeout(ctx, publishTimeout)
				defer cancel()
				if _, err := p.Publish(pctx); err != nil {
					log.Printf("rarity publish: pass failed (next tick retries): %v", err)
				}
			}()
			timer.Reset(p.cfg.Interval)
		}
	}
}

// Publish runs one full aggregate→build→upload pass and returns the published
// manifest. Full rebuild every time (credits.go stance) — never a diff/append,
// so corrections and deletions in the counters flow through cleanly.
func (p *Publisher) Publish(ctx context.Context) (Manifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	start := time.Now()

	counts, err := p.counts.IdentifyCountTotals(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("rarity: count totals: %w", err)
	}

	version, err := p.nextVersion(ctx)
	if err != nil {
		// Transient R2 read failure: abort rather than regress the version
		// counter; the next tick retries.
		return Manifest{}, fmt.Errorf("rarity: read previous version: %w", err)
	}

	m := BuildManifest(counts, version, time.Now().UTC(), p.cfg)
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, fmt.Errorf("rarity: marshal: %w", err)
	}
	if err := p.store.Put(ctx, p.key, body, "application/json", cacheControl); err != nil {
		return Manifest{}, fmt.Errorf("rarity: upload: %w", err)
	}
	log.Printf("rarity publish ok: key=%s version=%d totalScans=%d speciesPublished=%d speciesCounted=%d bytes=%d elapsed=%s",
		p.key, m.Version, m.TotalScans, len(m.Species), len(counts), len(body), time.Since(start).Round(time.Millisecond))
	return m, nil
}

// nextVersion reads the currently published artifact and returns its version+1.
// Absent object → 1 (first publish). Corrupt JSON → WARN + 1 (self-heal wins
// over monotonicity when the published state is already garbage). R2 errors
// propagate — see Publish.
func (p *Publisher) nextVersion(ctx context.Context) (int, error) {
	body, found, err := p.store.Get(ctx, p.key)
	if err != nil {
		return 0, err
	}
	if !found {
		return 1, nil
	}
	var prev struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(body, &prev); err != nil || prev.Version < 0 {
		log.Printf("WARN: rarity: published %s unparseable (%v); restarting version at 1", p.key, err)
		return 1, nil
	}
	return prev.Version + 1, nil
}

// HandleRebuild is the POST /internal/rarity/rebuild handler: admin-token
// gated (constant-time compare; empty configured token denies everything —
// imageingest defense-in-depth stance), synchronous publish, JSON summary
// back. Reachable only from the server host: nginx whitelists /v1/* +
// /healthz publicly, everything else 404s (deploy/OPS.md).
func (p *Publisher) HandleRebuild() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(AdminTokenHeader)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing_admin_token"})
			return
		}
		if p.cfg.AdminToken == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(p.cfg.AdminToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad_admin_token"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), rebuildTimeout)
		defer cancel()
		m, err := p.Publish(ctx)
		if err != nil {
			log.Printf("rarity rebuild: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "publish_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":           "ok",
			"key":              p.key,
			"version":          m.Version,
			"generatedAt":      m.GeneratedAt,
			"totalScans":       m.TotalScans,
			"speciesPublished": len(m.Species),
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
