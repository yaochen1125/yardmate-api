package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

// requestBodyCap is the JSON body size limit for POST /v1/plants/enrichment.
// Tiny endpoint: scientificName + commonName + plantId rarely exceed a few
// hundred bytes. 64 KB is a generous cap to defend against malformed clients.
// SPEC §4.2 body cap.
const requestBodyCap = 64 << 10

// requestTimeout bounds the full handler execution: cache + catalog (microseconds)
// + Supabase round-trip (~150 ms) + LLM call (12 s inner) + Supabase write
// (~150 ms). 30 s headroom is the same as identify/diagnose to keep server-wide
// timeout constants consistent.
const requestTimeout = 30 * time.Second

// requestPayload is the wire shape of the POST body.
type requestPayload struct {
	ScientificName string `json:"scientificName"`
	CommonName     string `json:"commonName"`
	PlantID        string `json:"plantId"`
}

// HandleEnrichment returns the http.HandlerFunc for POST /v1/plants/enrichment.
// See SPEC §2.1 + §3.
func HandleEnrichment(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Body cap (drops the connection on overflow; surfaces as a
		// MaxBytesError on the next Read so we can map cleanly).
		r.Body = http.MaxBytesReader(w, r.Body, requestBodyCap)

		// 2. Required headers (parity with /v1/identify + /v1/diagnose).
		deviceID := r.Header.Get("X-Device-Install-Id")
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		appVer := r.Header.Get("X-App-Version")
		if appVer == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}

		// 3. Optional App Attest signals (logged only V1, parent SPEC §5).
		attKeyID := r.Header.Get("X-AppAttest-KeyID")
		attAssertPresent := r.Header.Get("X-AppAttest-Assertion") != ""

		// 4. JSON body parse.
		var req requestPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// MaxBytesError or any decode failure collapses to bad_json. The
			// body is tiny enough that a "too large" code wouldn't help clients.
			writeError(w, http.StatusBadRequest, "bad_json")
			return
		}

		// 5. Delegate to Service.
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		result, err := svc.GetOrGenerate(ctx, Request{
			ScientificName: req.ScientificName,
			CommonName:     req.CommonName,
			PlantIDHint:    req.PlantID,
		})
		if err != nil {
			handleServiceError(w, r, err, deviceID, appVer, req.ScientificName, attKeyID, attAssertPresent)
			return
		}

		// 6. Success — single-line structured log (parent SPEC §5.2 forensics).
		// Log only the scientific name + outcome, NOT the response body
		// (SPEC §9 #10).
		log.Printf("enrichment ok: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v sciName=%q hadCommonName=%v cacheLen=%d",
			deviceID, appVer, attKeyID, attAssertPresent,
			req.ScientificName, req.CommonName != "",
			svc.CacheLen(),
		)
		writeJSON(w, http.StatusOK, result)
	}
}

// handleServiceError maps Service errors to HTTP error codes per SPEC §3.
func handleServiceError(w http.ResponseWriter, _ *http.Request, err error, deviceID, appVer, sciName, attKeyID string, attAssertPresent bool) {
	switch {
	case errors.Is(err, ErrInvalidScientificName):
		writeError(w, http.StatusBadRequest, "missing_scientific_name")
	case errors.Is(err, ErrScientificNameTooLong):
		writeError(w, http.StatusBadRequest, "scientific_name_too_long")
	case errors.Is(err, ErrDBUnavailable):
		writeError(w, http.StatusBadGateway, "db_unavailable")
	case errors.Is(err, ErrEnrichmentUnavailable):
		writeError(w, http.StatusBadGateway, "enrichment_unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "internal")
	}
	log.Printf("enrichment err: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v sciName=%q err=%v",
		deviceID, appVer, attKeyID, attAssertPresent, sciName, err)
}

// CacheLen exposes the underlying cache size for the success log; trivial helper.
func (s *Service) CacheLen() int {
	if s == nil {
		return 0
	}
	return s.cache.Len()
}

// --- helpers (duplicated from proxy/handlers.go to avoid exposing internal
// utilities through proxy's public surface; the duplication is small + stable).

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

// isUUID accepts RFC 4122 canonical form (36 chars with dashes at positions
// 8/13/18/23). Case-insensitive for hex digits — iOS NSUUID().uuidString
// produces uppercase-canonical, but we accept either case for robustness.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
