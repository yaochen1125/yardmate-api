package imageingest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

// adminTokenHeader is the header carrying the internal admin token (SPEC §2.1).
const adminTokenHeader = "X-Ingest-Admin-Token"

// batchRequestTimeout bounds a manual batch run started via the HTTP trigger.
const batchRequestTimeout = 10 * time.Minute

// singleRequestTimeout bounds a single-slug run (one search + one download +
// one upload, paced; generous headroom over the per-call 30s).
const singleRequestTimeout = 2 * time.Minute

// Service is the imageingest HTTP-facing wrapper: an Ingestor plus the admin
// token. Built by main's buildImageIngestService (nil → route not registered,
// graceful-disable mirroring buildEnrichmentService). SPEC §10 wiring.
type Service struct {
	ingestor   *Ingestor
	adminToken string
}

// NewService wraps an Ingestor with the admin token. adminToken MUST be
// non-empty in production (the route is only registered when it is set); an
// empty token here rejects every request (defense in depth).
func NewService(ingestor *Ingestor, adminToken string) *Service {
	return &Service{ingestor: ingestor, adminToken: adminToken}
}

// Start launches the background ingest ticker (SPEC §2.1) — a passthrough to
// the underlying Ingestor so main can drive it from the *Service it holds.
// interval<=0 is a no-op. Returns a stop func. Safe on a nil service.
func (s *Service) Start(interval time.Duration) (stop func()) {
	if s == nil || s.ingestor == nil {
		return func() {}
	}
	return s.ingestor.Start(interval)
}

// HandleRun returns the http.HandlerFunc for POST /internal/imageingest/run
// (SPEC §2.1 / §3). svc may be nil → 503 ingest_disabled (the route should not
// be registered in that case, but guard anyway).
//
//	?slug=<>&name=<>  → IngestOne, returns the IngestOutcome JSON (200)
//	(no slug)         → RunBatch(?limit), returns the BatchSummary JSON (202)
//	?slug without ?name → 400 bad_request
func HandleRun(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || svc.ingestor == nil {
			writeError(w, http.StatusServiceUnavailable, "ingest_disabled")
			return
		}

		// Admin-token gate (constant-time compare, SPEC §9 #7).
		token := r.Header.Get(adminTokenHeader)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing_admin_token")
			return
		}
		if svc.adminToken == "" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(svc.adminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "bad_admin_token")
			return
		}

		q := r.URL.Query()
		slug := q.Get("slug")
		name := q.Get("name")

		// Single-slug path (smoke-testing the slug↔R2 round-trip).
		if slug != "" {
			if name == "" {
				writeError(w, http.StatusBadRequest, "bad_request")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), singleRequestTimeout)
			defer cancel()
			out, err := svc.ingestor.IngestOne(ctx, slug, name)
			if err != nil {
				log.Printf("imageingest run single err: slug=%s err=%v", slug, err)
				writeError(w, http.StatusInternalServerError, "internal")
				return
			}
			// Record the single-slug outcome too (parity with batch). Read prior
			// for attempt accounting.
			prior, _ := svc.ingestor.ledger.Lookup(ctx, slug)
			svc.ingestor.recordOutcome(ctx, slug, name, out, prior)
			log.Printf("imageingest run single ok: slug=%s status=%s license=%s bytes=%d",
				slug, out.Status, out.License, out.Bytes)
			writeJSON(w, http.StatusOK, out)
			return
		}

		// Batch path. Optional ?limit; malformed → 400 bad_request.
		limit := 0
		if ls := q.Get("limit"); ls != "" {
			n, err := strconv.Atoi(ls)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, "bad_request")
				return
			}
			limit = n
		}

		// Run the batch in a detached goroutine bounded by limit (SPEC §2.1 —
		// long batches return 202 and continue). The single-flight guard in
		// RunBatch prevents overlap with the ticker.
		go func(limit int) {
			ctx, cancel := context.WithTimeout(context.Background(), batchRequestTimeout)
			defer cancel()
			summary, err := svc.ingestor.RunBatch(ctx, limit)
			if err != nil {
				log.Printf("imageingest run batch err: limit=%d err=%v", limit, err)
				return
			}
			log.Printf("imageingest run batch ok: seen=%d attempted=%d ingested=%d noImage=%d deferred=%d skipped=%d errors=%d",
				summary.Seen, summary.Attempted, summary.Ingested, summary.NoImage,
				summary.Deferred, summary.Skipped, summary.Errors)
		}(limit)

		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

// --- local HTTP helpers (mirrors proxy/enrichment/handlers.go; small + stable) ---

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Error: code})
}
