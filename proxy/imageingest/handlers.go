package imageingest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// adminTokenHeader is the header carrying the internal admin token (SPEC §2.1).
const adminTokenHeader = "X-Ingest-Admin-Token"

// slugPattern is the Slug() value domain: lowercase-alphanumeric groups joined by
// single hyphens (no leading / trailing / doubled hyphen). The internal ?slug=
// param is interpolated straight into R2 keys AND the ledger primary key, so an
// unvalidated value could smuggle '/', '..', or ':' (path traversal / key
// clobbering) — and a "catalog:..." value would collide with the
// IngestCatalogSpecies single-flight key. Reject anything off-domain (finding #10).
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// internalRequestTimeout bounds a synchronous internal run (search + downloads
// + uploads for one gallery, paced; generous headroom over the per-call 30s).
const internalRequestTimeout = 5 * time.Minute

// publicWorkTimeout bounds the detached goroutine doing the actual ingest after
// the public endpoint returns 202 (SPEC §2.1 fire-and-forget).
const publicWorkTimeout = 5 * time.Minute

// Service is the imageingest HTTP-facing wrapper: an Ingestor plus the admin
// token. Built by main's buildImageIngestService (nil → routes not registered,
// graceful-disable mirroring buildEnrichmentService). SPEC §10 wiring.
type Service struct {
	ingestor   *Ingestor
	adminToken string
}

// NewService wraps an Ingestor with the admin token. adminToken MUST be
// non-empty in production (the internal route is only registered when it is
// set); an empty token rejects every internal request (defense in depth).
func NewService(ingestor *Ingestor, adminToken string) *Service {
	return &Service{ingestor: ingestor, adminToken: adminToken}
}

// publicRequest is the JSON body of POST /v1/plants/imageingest (SPEC §1.3).
// hero_image.photo_id drives slot-1 pass-through (§2.1.1); url/license_code/
// attribution (if sent) are advisory for iOS display and ignored here.
type publicRequest struct {
	ScientificName string `json:"scientific_name"`
	ImageCount     int    `json:"image_count"`
	HeroImage      *struct {
		PhotoID json.RawMessage `json:"photo_id"` // string OR number; canonicalized to int64
	} `json:"hero_image"`
}

// canonicalizeHeroPhotoID normalizes a forwarded iNat photo id (JSON string or
// number) into the base-10 int64 string used for the cascade DedupKey match
// (SPEC §2.1.1 / §9 #17: key = "inat-photo-"+id). Any non-numeric / non-positive
// / unparseable value → "" (no pass-through; slot 1 cascades). Leading zeros are
// normalized away so the key still byte-matches the iNat-built one.
func canonicalizeHeroPhotoID(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	s = strings.TrimSpace(strings.Trim(s, `"`)) // accept "123" (string) or 123 (number)
	if s == "" {
		return ""
	}
	// ParseUint rejects any sign ("+9" / "-5") and non-digits; leading zeros are
	// normalized away by FormatUint so the key still matches the int64-built one.
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	return strconv.FormatUint(n, 10)
}

// HandlePublic returns the http.HandlerFunc for POST /v1/plants/imageingest
// (SPEC §1.1 / §2.1). The endpoint is App Attest gated in the SAME sense as
// /v1/identify: the attest envelope is read + logged for forensics but V1 does
// NOT call VerifyAssertion (iOS 26 issue — memory option_d_progress.md / SPEC
// §5). Abuse is bounded by the per-IP + per-device rate-limit middleware
// (server.go) and single-flight per slug. Returns 202 fire-and-forget.
func HandlePublic(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || svc.ingestor == nil {
			writeError(w, http.StatusServiceUnavailable, "ingest_disabled")
			return
		}

		// Required headers (mirror /v1/identify; per-device middleware also keys
		// on the device id).
		if !isUUID(r.Header.Get("X-Device-Install-Id")) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		if r.Header.Get("X-App-Version") == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}
		// App Attest signals: logged only in V1 (SPEC §5 / option_d_progress).
		attKeyID := r.Header.Get("X-AppAttest-KeyID")
		attAssertPresent := r.Header.Get("X-AppAttest-Assertion") != ""

		var body publicRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		if err := dec.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request")
			return
		}
		name := strings.TrimSpace(body.ScientificName)
		slug := Slug(name)
		if name == "" || slug == "" {
			writeError(w, http.StatusBadRequest, "bad_request")
			return
		}
		if body.ImageCount != 0 && (body.ImageCount < minImageCount || body.ImageCount > maxImageCount) {
			writeError(w, http.StatusBadRequest, "bad_request")
			return
		}

		n := body.ImageCount
		if n <= 0 {
			n = svc.ingestor.cfg.DefaultImageCount
		}

		heroPhotoID := ""
		if body.HeroImage != nil {
			heroPhotoID = canonicalizeHeroPhotoID(body.HeroImage.PhotoID)
		}

		// Fire-and-forget: the real work runs in a detached goroutine bounded by
		// single-flight per slug (concurrent duplicates coalesce, SPEC §4.2).
		go func(req IngestRequest) {
			ctx, cancel := context.WithTimeout(context.Background(), publicWorkTimeout)
			defer cancel()
			out, err := svc.ingestor.IngestSpecies(ctx, req)
			if err != nil {
				log.Printf("imageingest public err: slug=%s err=%v", req.Slug, err)
				return
			}
			if out.Coalesced {
				log.Printf("imageingest public coalesced: slug=%s", out.Slug)
				return
			}
			log.Printf("imageingest public ok: slug=%s images=%d attestKey=%q attestAssert=%v",
				out.Slug, len(out.PerImage), attKeyID, attAssertPresent)
		}(IngestRequest{ScientificName: name, ImageCount: n, HeroPhotoID: heroPhotoID})

		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":              true,
			"slug":                  slug,
			"image_count_requested": n,
		})
	}
}

// HandleRun returns the http.HandlerFunc for POST /internal/imageingest/run
// (SPEC §2.1 / §3). Admin-token gated, internal-only (mounted outside /v1), ops:
//
//	?slug=<>&name=<>&image_count=<>  → single, synchronous IngestOutcome (200)
//	?names=n1,n2&image_count=<>      → batch reseed, synchronous []IngestOutcome
//
// svc may be nil → 503 ingest_disabled (the route should not be registered then,
// but guard anyway).
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
		imageCount := 0
		if cs := q.Get("image_count"); cs != "" {
			n, err := strconv.Atoi(cs)
			if err != nil || n < 0 || n > maxImageCount {
				writeError(w, http.StatusBadRequest, "bad_request")
				return
			}
			imageCount = n
		}

		ctx, cancel := context.WithTimeout(r.Context(), internalRequestTimeout)
		defer cancel()

		// Single-slug path (smoke-testing the slug↔R2 round-trip; forces a slug).
		if slug := q.Get("slug"); slug != "" {
			// Shape-validate before the value reaches R2 keys / the ledger PK
			// (finding #10): only the Slug() value domain is accepted.
			if !slugPattern.MatchString(slug) {
				writeError(w, http.StatusBadRequest, "bad_request")
				return
			}
			name := q.Get("name")
			if name == "" {
				writeError(w, http.StatusBadRequest, "bad_request")
				return
			}
			out, err := svc.ingestor.IngestSpecies(ctx, IngestRequest{
				Slug: slug, ScientificName: name, ImageCount: imageCount,
			})
			if err != nil {
				log.Printf("imageingest run single err: slug=%s err=%v", slug, err)
				writeError(w, http.StatusInternalServerError, "internal")
				return
			}
			writeJSON(w, http.StatusOK, out)
			return
		}

		// Batch reseed path (ops, e.g. R2 bucket restore). Comma-separated
		// scientific names; each is cascaded + slugged synchronously.
		if namesParam := q.Get("names"); namesParam != "" {
			var outs []IngestOutcome
			for _, raw := range strings.Split(namesParam, ",") {
				name := strings.TrimSpace(raw)
				if name == "" {
					continue
				}
				out, err := svc.ingestor.IngestSpecies(ctx, IngestRequest{
					ScientificName: name, ImageCount: imageCount,
				})
				if err != nil {
					log.Printf("imageingest run batch err: name=%q err=%v", name, err)
					continue
				}
				outs = append(outs, out)
			}
			writeJSON(w, http.StatusOK, outs)
			return
		}

		writeError(w, http.StatusBadRequest, "bad_request")
	}
}

// isUUID accepts RFC 4122 canonical form (36 chars, dashes at 8/13/18/23).
// Case-insensitive for hex. Duplicated from proxy.isUUID / ratelimit.isUUID —
// the same 12-line check, not worth a shared package for three callers.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
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
