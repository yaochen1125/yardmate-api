package proxy

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	// dexFlushInterval is the cadence of the single background flush worker.
	// The crash-loss window is at most one interval of counts — acceptable for
	// a daily-aggregated rarity metric.
	dexFlushInterval = 30 * time.Second
	// dexFlushTimeout bounds one batched upsert.
	dexFlushTimeout = 30 * time.Second
)

// IdentifyScanSink receives batched Plantdex counter increments
// (dex_identify_daily, enrichment migration 012). *enrichment.DB implements it
// (AddIdentifyScans); the interface lives here because enrichment already
// imports proxy — same reverse-dependency pattern as DiseaseEnricher.
type IdentifyScanSink interface {
	AddIdentifyScans(ctx context.Context, counts map[string]int64) error
}

// IdentifyScanCounter accumulates successful in-catalog identifications in
// memory and flushes them to the sink on a fixed cadence from ONE background
// worker. The identify hot path only takes a mutex and bumps a map entry —
// no goroutine, no DB round-trip per request (enrichment SPEC §9 #18: bound
// concurrency; never spawn per-request goroutines against the shared pool).
// Pending size is bounded by the catalog (~1.6k ids), so a long DB outage
// costs memory-negligible re-merged counts, never growth.
//
// A nil counter is inert: Add and Start are nil-safe no-ops, so a disabled
// feature (RARITY_COUNT_ENABLED unset/false, or no DB pool) costs nothing.
type IdentifyScanCounter struct {
	sink    IdentifyScanSink
	mu      sync.Mutex
	pending map[string]int64
}

// NewIdentifyScanCounter builds a counter for the given sink.
func NewIdentifyScanCounter(sink IdentifyScanSink) *IdentifyScanCounter {
	return &IdentifyScanCounter{sink: sink, pending: make(map[string]int64)}
}

// Add records one successful identification of plantID. Empty id and nil
// receiver are no-ops, so callers can pass dexCountablePlantID's result
// unconditionally. Never blocks on IO; the response path stays unaffected.
func (c *IdentifyScanCounter) Add(plantID string) {
	if c == nil || plantID == "" {
		return
	}
	c.mu.Lock()
	c.pending[plantID]++
	c.mu.Unlock()
}

// Start launches the flush loop; returns immediately. Safe on nil. Loop shape
// mirrors enrichment/sweep.go: top-level recover (last resort — loop exits,
// process lives) + per-tick recover (one bad flush never stops the schedule).
func (c *IdentifyScanCounter) Start(ctx context.Context) {
	if c == nil {
		return
	}
	go c.loop(ctx)
	log.Printf("dex identify counter: flushing every %s", dexFlushInterval)
}

func (c *IdentifyScanCounter) loop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("dex identify counter: loop panic recovered (flushing disabled): %v", r)
		}
	}()
	ticker := time.NewTicker(dexFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("dex identify counter: flush panic recovered: %v", r)
					}
				}()
				c.flush(ctx)
			}()
		}
	}
}

// flush swaps the pending map out under the mutex, then upserts the batch
// outside it. On sink failure the counts merge back into pending (bounded by
// catalog size) and the next tick retries — best-effort, log-only.
func (c *IdentifyScanCounter) flush(ctx context.Context) {
	c.mu.Lock()
	batch := c.pending
	if len(batch) == 0 {
		c.mu.Unlock()
		return
	}
	c.pending = make(map[string]int64)
	c.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, dexFlushTimeout)
	defer cancel()
	if err := c.sink.AddIdentifyScans(fctx, batch); err != nil {
		c.mu.Lock()
		for id, n := range batch {
			c.pending[id] += n
		}
		c.mu.Unlock()
		log.Printf("dex identify counter: flush of %d species failed (re-queued): %v", len(batch), err)
		return
	}
}

// dexCountablePlantID returns the curated catalog id a successful identify
// should count for Plantdex rarity, or "" when the result must not count: no
// suggestions, top suggestion out-of-catalog (nil/blank plant_id), or the
// AAA0000 "unknown" sentinel — which reaches the success path with a non-nil
// PlantID but is a miss, not an identification. This is the single write-path
// filter (the manifest builder scrubs legacy rows on the read path).
func dexCountablePlantID(result *IdentifyResult) string {
	if result == nil || len(result.Suggestions) == 0 {
		return ""
	}
	pid := result.Suggestions[0].PlantID
	if pid == nil {
		return ""
	}
	id := strings.TrimSpace(*pid)
	if id == "" || id == unknownSentinelPlantID {
		return ""
	}
	return id
}
