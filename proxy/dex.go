package proxy

import (
	"context"
	"log"
	"strings"
	"time"
)

// identifyScanRecordTimeout bounds the detached best-effort Plantdex counter
// write so a slow/hung DB can never pile up goroutines from the identify hot
// path.
const identifyScanRecordTimeout = 5 * time.Second

// IdentifyScanRecorder records one successful in-catalog identification for
// the Plantdex rarity counters (dex_identify_daily, enrichment migration 012;
// aggregation in proxy/rarity). *enrichment.DB implements it; the interface
// lives here because enrichment already imports proxy — same reverse-dependency
// pattern as DiseaseEnricher. nil disables counting (RARITY_COUNT_ENABLED
// unset/false, or no DB pool).
type IdentifyScanRecorder interface {
	RecordIdentifyScan(ctx context.Context, plantID string) error
}

// dexCountablePlantID returns the curated catalog id a successful identify
// should count for Plantdex rarity, or "" when the result must not count: no
// suggestions, top suggestion out-of-catalog (nil/blank plant_id), or the
// AAA0000 "unknown" sentinel — which reaches the success path with a non-nil
// PlantID but is a miss, not an identification.
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

// recordIdentifyScan fires the best-effort Plantdex counter write on a
// detached goroutine: context.Background() because the request context dies
// when the response is written (enrichment/backfill.go rule), own timeout,
// errors logged only. The identify response never waits on or fails because
// of this write.
func recordIdentifyScan(recorder IdentifyScanRecorder, plantID string) {
	if recorder == nil || plantID == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("identify dex-count: panic recovered: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), identifyScanRecordTimeout)
		defer cancel()
		if err := recorder.RecordIdentifyScan(ctx, plantID); err != nil {
			log.Printf("identify dex-count: record %s failed: %v", plantID, err)
		}
	}()
}
