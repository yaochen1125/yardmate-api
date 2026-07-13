package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// signalRequestPayload is the wire shape of POST /v1/plants/signal.
type signalRequestPayload struct {
	ScientificName string `json:"scientificName"`
	// PlantId, when set, is the AAA catalog id of an IN-catalog plant. It routes
	// the signal to catalog_signals (in-catalog usage) instead of plant_signals
	// (out-of-catalog promotion interest). Empty = out-of-catalog, keyed by name.
	PlantId string `json:"plantId"`
	Kind    string `json:"kind"` // "search" | "garden" | "identify"
}

// signalKinds is the allowed set of interest-signal kinds. Kept in sync with the
// plant_signals.kind CHECK constraint (migrations 006 + 007).
var signalKinds = map[string]bool{"search": true, "garden": true, "identify": true}

// catalogSignalKinds is the subset valid for in-catalog signals (catalog_signals,
// migration 010). 'garden' is excluded — in-catalog garden adds are tracked by
// garden_records, not here.
var catalogSignalKinds = map[string]bool{"search": true, "identify": true}

// catalogPlantID matches the AAA-prefixed catalog id (e.g. "AAA0505").
var catalogPlantID = regexp.MustCompile(`^AAA[0-9]{4,}$`)

// HandleSignal returns the handler for POST /v1/plants/signal — a lightweight,
// fire-and-forget interest counter. It records that ONE device searched (opened
// a library-outside plant's detail from search) or garden-added a library-outside
// plant. Dedup by (normalized name, kind, device id) makes count(*) per
// (name, kind) a distinct-device (≈ distinct-person) tally, which feeds the
// catalog review tool's "how many people searched / added" badges — a promotion
// priority signal. Reuses the shared Supabase pool (enrichDB); no attestation
// (log-only parity with /v1/plants/enrichment). iOS only calls this for
// non-catalog plants, already gated by the user's analytics consent.
func HandleSignal(db *DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, requestBodyCap)

		// Required headers (parity with /v1/plants/enrichment). The install id
		// doubles as the dedup key — no separate identifier is sent.
		deviceID := r.Header.Get("X-Device-Install-Id")
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		if r.Header.Get("X-App-Version") == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}

		var req signalRequestPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		// In-catalog signal: plantId set → catalog_signals, keyed by AAA id
		// directly (no name mapping). 'garden' rejected here (garden_records owns it).
		if req.PlantId != "" {
			if !catalogPlantID.MatchString(req.PlantId) {
				writeError(w, http.StatusBadRequest, "invalid_plant_id")
				return
			}
			if !catalogSignalKinds[req.Kind] {
				writeError(w, http.StatusBadRequest, "invalid_kind")
				return
			}
			if err := db.RecordCatalogSignal(ctx, req.PlantId, req.Kind, deviceID); err != nil {
				writeError(w, http.StatusBadGateway, "db_unavailable")
				log.Printf("catalog signal err: deviceID=%s plantId=%q kind=%q err=%v",
					deviceID, req.PlantId, req.Kind, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
			return
		}

		// Out-of-catalog: same normalization as enrichment/catalog so a signal keys
		// to the same row the promotion pipeline will assign (× hybrids, ranks).
		normalized := proxy.NormalizeScientificName(req.ScientificName)
		if normalized == "" {
			writeError(w, http.StatusBadRequest, "missing_scientific_name")
			return
		}
		// Same 200-char ceiling the main enrichment path enforces (maxScientificNameLen):
		// bound the value before it hits the DB, matching that validation's posture.
		if len(normalized) > maxScientificNameLen {
			writeError(w, http.StatusBadRequest, "scientific_name_too_long")
			return
		}
		if !signalKinds[req.Kind] {
			writeError(w, http.StatusBadRequest, "invalid_kind")
			return
		}
		if err := db.RecordSignal(ctx, normalized, req.Kind, deviceID); err != nil {
			writeError(w, http.StatusBadGateway, "db_unavailable")
			log.Printf("signal err: deviceID=%s sciName=%q kind=%q err=%v",
				deviceID, req.ScientificName, req.Kind, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
	}
}

// RecordSignal upserts one (normalized, kind, device) signal row. Idempotent per
// device via the composite primary key — re-reporting is a no-op, so count(*)
// per (name, kind) counts distinct devices. Nil-safe (returns ErrDBUnavailable
// when the shared pool is absent, matching the enrichment DB contract).
func (d *DB) RecordSignal(ctx context.Context, normalized, kind, deviceID string) error {
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}
	const stmt = `
		INSERT INTO plant_signals (scientific_name_normalized, kind, device_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (scientific_name_normalized, kind, device_id) DO NOTHING`
	if _, err := d.pool.Exec(ctx, stmt, normalized, kind, deviceID); err != nil {
		return fmt.Errorf("%w: signal upsert: %v", ErrDBUnavailable, err)
	}
	return nil
}

// RecordCatalogSignal upserts one (plant_id, kind, device) row into
// catalog_signals — in-catalog usage, keyed by AAA id. Idempotent per device via
// the composite primary key, so count(*) per (plant_id, kind) counts distinct
// devices. Nil-safe (ErrDBUnavailable when the shared pool is absent).
func (d *DB) RecordCatalogSignal(ctx context.Context, plantID, kind, deviceID string) error {
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}
	const stmt = `
		INSERT INTO catalog_signals (plant_id, kind, device_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (plant_id, kind, device_id) DO NOTHING`
	if _, err := d.pool.Exec(ctx, stmt, plantID, kind, deviceID); err != nil {
		return fmt.Errorf("%w: catalog signal upsert: %v", ErrDBUnavailable, err)
	}
	return nil
}
