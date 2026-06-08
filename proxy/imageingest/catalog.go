package imageingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// catalog.go — in-catalog (AAA-id) supplementary image ingest.
//
// The out-of-catalog path (IngestSpecies) owns the slug-keyed gallery under
// plant_images/ingested/{slug}/ with a Supabase ledger + global credits.json.
// This file adds a PARALLEL, ZERO-DB path for the curated 1522 plants: it fills
// an `external/` subfolder under each plant's own AAA-id folder with third-party
// images, reusing the exact same iNat → Wikimedia cascade + license gate
// (gatherCandidates), but writing per-species attribution to a small index.json
// instead of the shared ledger/credits. See SPEC §"catalog external".
//
// Why a separate path instead of reusing IngestSpecies:
//   - Different R2 namespace (catalogKey vs galleryKey) so the offline scripts/
//     pipeline that writes the 4 generated semantic images never collides.
//   - No DB: the curated set is large and these supplementary images are a
//     best-effort enrichment; a per-species index.json built from the in-memory
//     ingest result is enough, and keeps the in-catalog path off the ledger.
//   - No global credits.json mutation: rebuilding it from an empty ledger would
//     wipe the out-of-catalog credits — this path must never touch it.

// catalogKey is the R2 object key for an in-catalog plant's supplementary
// (third-party) gallery image. These live UNDER the curated plant's own AAA-id
// folder but in an `external/` subfolder, so the offline scripts/ pipeline that
// writes the 4 generated semantic images (plant_images/{AAA}/{1_whole..4_scene})
// is never overwritten by — nor collides with — these ingested images.
func catalogKey(catalogID string, index int) string {
	return fmt.Sprintf("plant_images/%s/external/%d.png", catalogID, index)
}

// catalogIndexKey is the per-species manifest (count + per-image attribution).
// Its EXISTENCE doubles as the "this species is done" idempotency marker: it is
// written LAST, only after every image uploaded, so a HEAD on it gates re-work.
func catalogIndexKey(catalogID string) string {
	return fmt.Sprintf("plant_images/%s/external/index.json", catalogID)
}

// externalIndexCacheControl is short (unlike the immutable images) so a re-ingest
// of a species can refresh the manifest within the hour. The images themselves
// stay immutable (imageCacheControl) — a slot's bytes never change once chosen.
const externalIndexCacheControl = "public, max-age=3600"

// ExternalIndex is the per-species manifest iOS reads from
// plant_images/{AAA}/external/index.json to learn how many supplementary images
// exist and their attribution — the in-catalog analogue of the out-of-catalog
// global credits.json, but per-species and fully decoupled from it.
type ExternalIndex struct {
	Count  int                  `json:"count"`
	Images []ExternalIndexImage `json:"images"`
}

// ExternalIndexImage is one supplementary image's positional index + attribution.
// Index is 1-based and CONTIGUOUS (1..count, no gaps), matching the R2 key
// external/{index}.png — so iOS can iterate either images[].index or 1..count.
type ExternalIndexImage struct {
	Index        int    `json:"index"`
	Source       string `json:"source"`
	LicenseShort string `json:"license_short"`
	LicenseURL   string `json:"license_url"`
	Author       string `json:"author"`
	SourceURL    string `json:"source_url"`
}

// BuildExternalIndex assembles the per-species manifest from the ingested image
// outcomes (callers pass only successfully-stored, contiguous rows). Built purely
// from the in-memory ingest result — NO database — so the path stays zero-DB.
func BuildExternalIndex(images []ImageOutcome) ([]byte, error) {
	idx := ExternalIndex{Count: len(images), Images: make([]ExternalIndexImage, 0, len(images))}
	for _, oc := range images {
		idx.Images = append(idx.Images, ExternalIndexImage{
			Index:        oc.Index,
			Source:       oc.Source,
			LicenseShort: oc.LicenseShort,
			LicenseURL:   oc.LicenseURL,
			Author:       oc.Author,
			SourceURL:    oc.SourceURL,
		})
	}
	return json.MarshalIndent(idx, "", "  ")
}

// CatalogIngestOutcome is the result of an in-catalog supplementary ingest. Like
// IngestOutcome but keyed by catalog id (AAA); PerImage holds the successfully
// stored images with CONTIGUOUS 1-based indices (the manifest source), and
// AlreadyDone surfaces the index.json idempotency signal.
type CatalogIngestOutcome struct {
	CatalogID   string         `json:"catalog_id"`
	Coalesced   bool           `json:"coalesced,omitempty"`
	AlreadyDone bool           `json:"already_done,omitempty"`
	PerImage    []ImageOutcome `json:"per_image,omitempty"`
}

// IngestCatalogSpecies fills the `external/` supplementary gallery for an
// in-catalog (AAA-id) plant with third-party images, reusing the same iNat →
// Wikimedia cascade + license gate as the out-of-catalog path (gatherCandidates).
//
// It diverges from IngestSpecies in three deliberate ways, all so the in-catalog
// path is ZERO-DB and never disturbs the out-of-catalog domain:
//  1. R2 keys are plant_images/{AAA}/external/{i}.png (catalogKey), isolated from
//     both the curated semantic images and the ingested/{slug}/ space.
//  2. NO ledger writes and NO global credits.json rebuild. Per-image attribution
//     is written to a per-species external/index.json built from the in-memory
//     outcome (BuildExternalIndex).
//  3. Idempotency is the existence of index.json (written LAST). A species whose
//     index.json already exists is skipped wholesale (AlreadyDone) — there is no
//     per-slot ledger to consult, and re-deriving partial-fill attribution
//     without a ledger would drop captions, so the manifest is the single commit
//     marker and the whole gallery is re-filled if it is absent.
func (in *Ingestor) IngestCatalogSpecies(ctx context.Context, catalogID, scientificName string, count int) (CatalogIngestOutcome, error) {
	out := CatalogIngestOutcome{CatalogID: catalogID}
	// isCatalogID is also a security boundary (catalogID is interpolated into the
	// R2 key — see HandleCatalog); validate here too as defense in depth for any
	// non-HTTP caller (batch tooling, tests). Log so the no-op isn't silent.
	if !isCatalogID(catalogID) || strings.TrimSpace(scientificName) == "" {
		log.Printf("imageingest catalog: rejected invalid request id=%q name=%q", catalogID, scientificName)
		return out, nil
	}

	// Single-flight keyed by catalog id (prefixed so it can never collide with a
	// slug key in the shared inflight map — Slug() output is [a-z0-9-], never ':').
	// Concurrent detail-page opens of the same species coalesce.
	flightKey := "catalog:" + catalogID
	if !in.acquire(flightKey) {
		out.Coalesced = true
		return out, nil
	}
	defer in.release(flightKey)

	n := count
	if n <= 0 {
		n = in.cfg.DefaultImageCount
	}
	if n < minImageCount {
		n = minImageCount
	}
	if n > maxImageCount {
		n = maxImageCount
	}

	// Idempotency: index.json exists ⇒ this species is already done (it is written
	// LAST, after every image, so its presence implies a complete gallery).
	idxKey := catalogIndexKey(catalogID)
	exists, err := in.store.Exists(ctx, idxKey)
	if err != nil {
		// HEAD failed transiently (or a read-permission blip) — skip this trigger
		// as RETRYABLE rather than re-ingesting a possibly-already-done species
		// (which would burn a full iNat→Wikimedia cascade on every flapping-HEAD
		// mount). The next mount retries; a genuinely-absent index.json is filled
		// then (Codex P2).
		log.Printf("imageingest catalog index head err (retryable skip): id=%s err=%v", catalogID, err)
		return out, nil
	}
	if exists {
		out.AlreadyDone = true
		return out, nil
	}

	// Gather third-party candidates from the SAME cascade as out-of-catalog.
	// usedSourceURLs is nil: the whole gallery is built in one shot, so there is
	// no prior-slot set to dedup against (within-gather DedupKey dedup still
	// applies). The BY/SA gate is ON in prod, so `eligible` already includes
	// CC-BY/-SA; `gated` is only non-empty when the flag is OFF, and the in-catalog
	// path intentionally ignores it (a curated plant shows fewer supplementary
	// images rather than parking deferred rows it has no ledger for).
	eligible, _ := in.gatherCandidates(ctx, scientificName, n, nil)

	// Fill up to n CONTIGUOUS slots. Consume eligible candidates in rank order
	// with download AND upload fall-through; each SUCCESS takes the next
	// contiguous index (1,2,3…). Keying by the success count (len(PerImage)+1),
	// NOT a fixed slot position, means a 404 / upload blip on one candidate leaves
	// NO gap — the manifest's indices are always 1..count, so iOS never derives a
	// key for a missing object. out.PerImage IS the ingested set (the manifest).
	out.PerImage = make([]ImageOutcome, 0, n)
	sawFailure := false
	for poolIdx := 0; len(out.PerImage) < n && poolIdx < len(eligible); poolIdx++ {
		pick := eligible[poolIdx]
		data, mime, derr := pick.dl.Download(ctx, pick.cand.DownloadURL)
		if derr != nil {
			// rendition may 404 — fall through to the next candidate (SPEC §2.5 #5).
			log.Printf("imageingest catalog download fall-through: id=%s url=%s err=%v",
				catalogID, pick.cand.DownloadURL, derr)
			sawFailure = true
			continue
		}
		slot := len(out.PerImage) + 1
		key := catalogKey(catalogID, slot)
		if uerr := in.store.Put(ctx, key, data, mime, imageCacheControl); uerr != nil {
			// transient R2 blip — fall through; don't burn the slot index.
			log.Printf("imageingest catalog upload fall-through: id=%s slot=%d err=%v",
				catalogID, slot, uerr)
			sawFailure = true
			continue
		}
		in.pace(ctx)
		out.PerImage = append(out.PerImage, in.ingestedOutcome(slot, key, pick, mime, int64(len(data))))
	}

	// Commit decision (Codex P2): the manifest is the AlreadyDone marker, so write
	// it ONLY when the gallery is either full (n images) OR fell short with NO
	// retryable failure — i.e. the candidate pool was genuinely exhausted (iNat
	// simply has fewer free photos, a stable result worth committing as-is). If we
	// fell short AND hit a download/upload failure, the shortfall may be transient,
	// so we DON'T write the marker and a later trigger retries. The already-uploaded
	// images are harmless contiguous orphans, overwritten on the next full run. An
	// empty gallery never writes (also retries).
	committable := len(out.PerImage) >= n || (len(out.PerImage) > 0 && !sawFailure)
	if committable {
		body, err := BuildExternalIndex(out.PerImage)
		if err != nil {
			return out, fmt.Errorf("imageingest catalog: marshal index %s: %w", catalogID, err)
		}
		if err := in.store.Put(ctx, idxKey, body, "application/json", externalIndexCacheControl); err != nil {
			return out, fmt.Errorf("imageingest catalog: put index %s: %w", catalogID, err)
		}
	}

	return out, nil
}

// catalogRequest is the POST /v1/plants/catalog-images body.
type catalogRequest struct {
	CatalogID      string `json:"catalog_id"`
	ScientificName string `json:"scientific_name"`
	ImageCount     int    `json:"image_count"`
}

// catalogIDPattern locks catalog_id to the AAA-id shape (3 uppercase letters + 4
// digits, e.g. AAA0001). This is a SECURITY boundary, not just validation: the id
// is interpolated straight into the R2 key (catalogKey), so without it an attested
// client could write arbitrary keys (path traversal / clobbering curated images).
// Fully anchored + fixed-length: no '/', '.', whitespace, or newline can pass.
var catalogIDPattern = regexp.MustCompile(`^[A-Z]{3}[0-9]{4}$`)

func isCatalogID(s string) bool { return catalogIDPattern.MatchString(s) }

// HandleCatalog returns the http.HandlerFunc for POST /v1/plants/catalog-images
// (in-catalog supplementary ingest). Same envelope + required headers +
// App-Attest-log-only stance + fire-and-forget 202 as HandlePublic, but keyed by
// catalog id and routed to IngestCatalogSpecies (external/ keys, zero-DB).
func HandleCatalog(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil || svc.ingestor == nil {
			writeError(w, http.StatusServiceUnavailable, "ingest_disabled")
			return
		}
		if !isUUID(r.Header.Get("X-Device-Install-Id")) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		if r.Header.Get("X-App-Version") == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}
		// App Attest signals: logged only in V1 (SPEC §5), same as HandlePublic.
		attKeyID := r.Header.Get("X-AppAttest-KeyID")
		attAssertPresent := r.Header.Get("X-AppAttest-Assertion") != ""

		var body catalogRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		if err := dec.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request")
			return
		}
		catalogID := strings.TrimSpace(body.CatalogID)
		name := strings.TrimSpace(body.ScientificName)
		if !isCatalogID(catalogID) || name == "" {
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

		// Fire-and-forget: the real work runs in a detached goroutine bounded by
		// single-flight per catalog id (concurrent duplicates coalesce). Args are
		// passed explicitly (no request-closure capture).
		go func(catalogID, name string, n int, attestKey string, attestAssert bool) {
			ctx, cancel := context.WithTimeout(context.Background(), publicWorkTimeout)
			defer cancel()
			out, err := svc.ingestor.IngestCatalogSpecies(ctx, catalogID, name, n)
			if err != nil {
				log.Printf("imageingest catalog err: id=%s err=%v", catalogID, err)
				return
			}
			switch {
			case out.Coalesced:
				log.Printf("imageingest catalog coalesced: id=%s", out.CatalogID)
			case out.AlreadyDone:
				log.Printf("imageingest catalog already done: id=%s", out.CatalogID)
			default:
				log.Printf("imageingest catalog ok: id=%s ingested=%d/%d attestKey=%q attestAssert=%v",
					out.CatalogID, len(out.PerImage), n, attestKey, attestAssert)
			}
		}(catalogID, name, n, attKeyID, attAssertPresent)

		writeJSON(w, http.StatusAccepted, map[string]any{
			"accepted":              true,
			"catalog_id":            catalogID,
			"image_count_requested": n,
		})
	}
}
