package imageingest

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy/imageingest/sources"
)

// ImageStatus is the per-(slug, image_index) result of an ingest attempt (SPEC
// §3). It is richer than the persisted IngestStatus: skipped_exists maps onto
// StatusIngested, and source_error / upload_error map onto StatusFailed.
type ImageStatus string

const (
	ImgIngested       ImageStatus = "ingested"
	ImgSkippedExists  ImageStatus = "skipped_exists"
	ImgNoAcceptable   ImageStatus = "no_acceptable_image"
	ImgDeferredAttrib ImageStatus = "deferred_attribution"
	ImgSourceError    ImageStatus = "source_error"
	ImgUploadError    ImageStatus = "upload_error"
)

// ImageOutcome is the result of one (slug, image_index) attempt (SPEC §1.4 /
// §3). Returned in IngestOutcome.PerImage (forensics for the internal endpoint;
// the public endpoint returns only 202 + slug).
type ImageOutcome struct {
	Index        int         `json:"index"`
	Status       ImageStatus `json:"status"`
	R2Key        string      `json:"r2_key,omitempty"`
	Source       string      `json:"source,omitempty"`
	License      string      `json:"license,omitempty"` // machine code
	LicenseShort string      `json:"license_short,omitempty"`
	LicenseURL   string      `json:"license_url,omitempty"`
	Author       string      `json:"author,omitempty"`
	SourceURL    string      `json:"source_url,omitempty"`
	MIME         string      `json:"mime,omitempty"`
	Bytes        int64       `json:"bytes,omitempty"`
	Width        int         `json:"width,omitempty"`
	Height       int         `json:"height,omitempty"`
	Note         string      `json:"note,omitempty"`
}

// IngestOutcome is the result of IngestSpecies (SPEC §1.4). A gallery can carry
// a mix of statuses (e.g. 2× ingested + 1× deferred_attribution + 1×
// source_error). Coalesced reports a single-flight skip (another goroutine owns
// the slug, SPEC §4.2) — PerImage is then nil.
type IngestOutcome struct {
	Slug      string         `json:"slug"`
	Coalesced bool           `json:"coalesced,omitempty"`
	PerImage  []ImageOutcome `json:"per_image,omitempty"`
}

// IngestRequest is the input to IngestSpecies (SPEC §1.3). ScientificName is the
// search term; Slug, when empty, is derived from it (the public endpoint leaves
// it empty; the internal single-slug endpoint may force a specific slug).
type IngestRequest struct {
	Slug           string
	ScientificName string
	ImageCount     int
}

// --- Collaborator interfaces (mocked in ingestor_test, SPEC §10) ---

// ImageSource is one cascade source (SPEC §2.4). *sources.INatClient and
// *sources.WikimediaClient both satisfy it (Download is promoted from the
// embedded fetcher). Returns RAW candidates; the ingestor classifies them.
type ImageSource interface {
	Search(ctx context.Context, scientificName string, limit int) ([]sources.Candidate, error)
	Download(ctx context.Context, rawURL string) ([]byte, string, error)
}

// LedgerStore is the two-table ledger surface the ingestor needs (SPEC §6.1).
// *Ledger satisfies it.
type LedgerStore interface {
	UpsertSpecies(ctx context.Context, row SpeciesRow) error
	UpsertFile(ctx context.Context, row FileRow) error
	LookupFile(ctx context.Context, slug string, imageIndex int) (*FileRow, error)
	RecomputeCount(ctx context.Context, slug string) (int, error)
	IngestedFiles(ctx context.Context) ([]FileRow, error)
}

// Config holds the ingestor's tunable behavior (SPEC §1.3 / §2.4 / §4).
type Config struct {
	// AllowAttributionLicenses is the Codex #22 release-coordination gate. When
	// false (V1 default), CC-BY / CC-BY-SA are NOT uploaded — a slot whose only
	// acceptable candidates are BY/SA → deferred_attribution. CC0 / PD are always
	// eligible. Flip true ONLY after the iOS Credits page is live (§9 #14).
	AllowAttributionLicenses bool
	// MinInterval paces serial source calls within an ingest (SPEC §4.1).
	MinInterval time.Duration
	// SearchLimit caps per-source candidate fetch (default 12).
	SearchLimit int
	// DefaultImageCount is the gallery size when the request omits image_count
	// (default 4; requests clamp to [1,6]).
	DefaultImageCount int
}

const (
	defaultSearchLimit = 12
	defaultImageCount  = 4
	minImageCount      = 1
	maxImageCount      = 6
	imageCacheControl  = "public, max-age=31536000, immutable"
)

// galleryKey is the R2 object key for a slug's image_index slot (SPEC §2.2).
func galleryKey(slug string, index int) string {
	return fmt.Sprintf("plant_images/%s/%d.png", slug, index)
}

// Ingestor orchestrates on-demand multi-image species ingest (SPEC §2.1). All
// collaborators are interfaces so the core is fully mockable.
type Ingestor struct {
	cascade []ImageSource // ordered: iNaturalist (primary) → Wikimedia (fallback)
	store   ObjectStore
	ledger  LedgerStore
	cfg     Config

	// single-flight per slug: concurrent requests for the same slug coalesce
	// (SPEC §4.2 / §9 #11 / #16). Process-local mutex map (single-instance OK).
	flightMu sync.Mutex
	inflight map[string]bool
}

// NewIngestor builds an Ingestor. Defaults are applied to zero Config fields.
// Cascade order is iNat → Wikimedia; nil sources are skipped (e.g. in tests
// that exercise a single source).
func NewIngestor(inat, wikimedia ImageSource, store ObjectStore, ledger LedgerStore, cfg Config) *Ingestor {
	if cfg.MinInterval < 0 {
		cfg.MinInterval = 0
	}
	if cfg.SearchLimit <= 0 {
		cfg.SearchLimit = defaultSearchLimit
	}
	if cfg.DefaultImageCount <= 0 {
		cfg.DefaultImageCount = defaultImageCount
	}
	var cascade []ImageSource
	if inat != nil {
		cascade = append(cascade, inat)
	}
	if wikimedia != nil {
		cascade = append(cascade, wikimedia)
	}
	return &Ingestor{cascade: cascade, store: store, ledger: ledger, cfg: cfg, inflight: map[string]bool{}}
}

// IngestSpecies fills an out-of-catalog plant's gallery (SPEC §2.1 flow). It
// NEVER returns a hard error for an ordinary per-image failure — those collapse
// into ImageOutcome.Status; an error is returned only for a precondition fault
// (empty slug after derivation) or a ledger species-row upsert failure.
func (in *Ingestor) IngestSpecies(ctx context.Context, req IngestRequest) (IngestOutcome, error) {
	slug := req.Slug
	if slug == "" {
		slug = Slug(req.ScientificName)
	}
	out := IngestOutcome{Slug: slug}
	if slug == "" {
		// Name has no [a-z0-9] characters → no valid R2 key. Nothing to do (the
		// public handler 400s before reaching here; defense in depth).
		return out, nil
	}

	// Single-flight: a concurrent ingest for this slug already owns the work.
	if !in.acquire(slug) {
		out.Coalesced = true
		return out, nil
	}
	defer in.release(slug)

	n := req.ImageCount
	if n <= 0 {
		n = in.cfg.DefaultImageCount
	}
	if n < minImageCount {
		n = minImageCount
	}
	if n > maxImageCount {
		n = maxImageCount
	}

	// Species row FIRST (SPEC §2.1 step 3 / §9 #17: the FK parent must exist
	// before any plant_image_files insert, and before RecomputeCount).
	if err := in.ledger.UpsertSpecies(ctx, SpeciesRow{
		Slug: slug, ScientificName: req.ScientificName, ImageCountRequested: n,
	}); err != nil {
		return out, fmt.Errorf("imageingest: upsert species %q: %w", slug, err)
	}

	// Gather + rank the candidate pool once (SPEC §2.4 cascade + §2.5 ranking).
	eligible, gated := in.gatherCandidates(ctx, req.ScientificName, n)

	// Fill each slot. Eligible candidates are consumed sequentially → distinct
	// photo per slot (within-gallery dedup, SPEC §2.5 #4); already-deduped on
	// DedupKey in gatherCandidates. sawDownloadFail tracks whether the eligible
	// pool was depleted by transient download failures (vs genuinely having no
	// acceptable image) so later empty slots are classified retryable (§3).
	poolIdx, gatedIdx := 0, 0
	sawDownloadFail := false
	for i := 1; i <= n; i++ {
		oc := in.fillSlot(ctx, slug, i, eligible, &poolIdx, gated, &gatedIdx, &sawDownloadFail)
		out.PerImage = append(out.PerImage, oc)
	}

	// Recompute the species aggregate AFTER all file rows are written (SPEC §2.1
	// step 5 / §9 #17 — never trust a separately-incremented counter; the species
	// row already exists from the upsert above, so this never errors on missing).
	if _, err := in.ledger.RecomputeCount(ctx, slug); err != nil {
		log.Printf("imageingest recompute count err: slug=%s err=%v", slug, err)
	}

	// Rebuild the public credits manifest (SPEC §2.7 / §9 #15 — full rebuild).
	if err := in.rebuildCredits(ctx); err != nil {
		log.Printf("imageingest credits rebuild err: slug=%s err=%v", slug, err)
	}

	return out, nil
}

// fillSlot resolves one (slug, image_index). It honors the prior ledger row +
// R2 HEAD for idempotency, then consumes eligible candidates with download
// fall-through before declaring source_error (SPEC §2.1 / §2.5 #5). poolIdx /
// gatedIdx advance past candidates consumed across slots.
func (in *Ingestor) fillSlot(ctx context.Context, slug string, i int, eligible []scoredCandidate, poolIdx *int, gated []scoredCandidate, gatedIdx *int, sawDownloadFail *bool) ImageOutcome {
	oc := ImageOutcome{Index: i}

	// Prior ledger row: skip terminal / fresh-negative / gated-while-off slots.
	prior, lerr := in.ledger.LookupFile(ctx, slug, i)
	if lerr != nil {
		log.Printf("imageingest lookup file err: slug=%s i=%d err=%v", slug, i, lerr)
		// Treat as fresh; a transient lookup error shouldn't permanently block.
		prior = nil
	}
	if prior != nil && in.shouldSkip(prior) {
		oc.Status = statusToOutcome(prior.Status)
		oc.R2Key = prior.R2Key
		oc.Note = "ledger skip"
		return oc
	}

	// R2 is the source of truth (SPEC §9 #8). HEAD before any source work.
	key := galleryKey(slug, i)
	exists, err := in.store.Exists(ctx, key)
	if err != nil {
		oc.Status = ImgSourceError
		oc.Note = "head check failed"
		in.recordFailed(ctx, slug, i, oc, prior)
		return oc
	}
	if exists {
		oc.Status = ImgSkippedExists
		oc.R2Key = key
		in.recordSkippedExists(ctx, slug, i, key, prior)
		return oc
	}

	// Try eligible candidates in rank order; first download+upload that succeeds
	// wins. Advance poolIdx so the next slot gets a distinct candidate.
	entryIdx := *poolIdx
	for *poolIdx < len(eligible) {
		pick := eligible[*poolIdx]
		*poolIdx++

		data, mime, derr := pick.dl.Download(ctx, pick.cand.DownloadURL)
		if derr != nil {
			// deriveLarge/thumb rendition may 404 — fall through to the next
			// candidate before giving up (SPEC §2.5 #5; Slice 2 review item).
			log.Printf("imageingest download fail (fall-through): slug=%s i=%d url=%s err=%v",
				slug, i, pick.cand.DownloadURL, derr)
			continue
		}
		if uerr := in.store.Put(ctx, key, data, mime, imageCacheControl); uerr != nil {
			oc.Status = ImgUploadError
			oc.Note = "put failed"
			in.recordFailed(ctx, slug, i, oc, prior)
			return oc
		}
		in.pace(ctx)
		oc = in.ingestedOutcome(i, key, pick, mime, int64(len(data)))
		in.recordIngested(ctx, slug, oc)
		return oc
	}

	// Eligible candidates existed for this slot but every download failed →
	// source_error (a transient/infra failure that should self-heal on retry,
	// SPEC §3), distinct from "no acceptable license" below. Mark the gallery so
	// later slots emptied by the same depletion are also treated as retryable.
	if *poolIdx > entryIdx {
		*sawDownloadFail = true
		oc.Status = ImgSourceError
		oc.Note = "all downloads failed"
		in.recordFailed(ctx, slug, i, oc, prior)
		return oc
	}

	// No eligible candidate left for this slot. If gated BY/SA candidates exist,
	// defer (store provenance — re-cascades when the gate flips ON). Otherwise,
	// if earlier slots depleted the eligible pool via download failures, this is
	// a transient shortfall → source_error (retryable, NOT a permanent negative
	// cache that an outage could park forever, §3 / §9 #13). Only a genuinely
	// empty acceptable pool is no_acceptable_image.
	if *gatedIdx < len(gated) {
		pick := gated[*gatedIdx]
		*gatedIdx++
		oc.Status = ImgDeferredAttrib
		oc.Source = pick.cand.Source
		oc.License = pick.lic.Code
		oc.LicenseShort = pick.lic.ShortName
		oc.LicenseURL = licenseURLOf(pick.lic)
		oc.Author = pick.lic.Author
		oc.SourceURL = pick.cand.PageURL
		oc.Note = "attribution gate off"
		in.recordDeferred(ctx, slug, oc, pick.cand.DownloadURL, prior)
		return oc
	}

	if *sawDownloadFail {
		oc.Status = ImgSourceError
		oc.Note = "eligible pool depleted by download failures"
		in.recordFailed(ctx, slug, i, oc, prior)
		return oc
	}

	oc.Status = ImgNoAcceptable
	oc.Note = "no acceptable license"
	in.recordNoAcceptable(ctx, slug, i, oc, prior)
	return oc
}

// gatherCandidates walks the cascade, classifies each raw candidate, dedups on
// DedupKey, and splits into upload-eligible vs gated (BY/SA while the flag is
// OFF). Both slices are returned ranked (SPEC §2.5). It stops probing further
// sources once it has enough eligible candidates for the gallery (SPEC §2.4).
func (in *Ingestor) gatherCandidates(ctx context.Context, name string, n int) (eligible, gated []scoredCandidate) {
	seen := map[string]bool{}
	for idx, src := range in.cascade {
		if idx > 0 {
			in.pace(ctx) // §4.1 etiquette between source calls
		}
		cands, err := src.Search(ctx, name, in.cfg.SearchLimit)
		if err != nil {
			log.Printf("imageingest source search err: name=%q err=%v", name, err)
			continue // fall through to the next source
		}
		for _, c := range cands {
			if c.DedupKey != "" {
				if seen[c.DedupKey] {
					continue
				}
				seen[c.DedupKey] = true
			}
			if isExcludedFormat(c) {
				continue
			}
			lic := classifyCandidate(c)
			if !lic.Allowed {
				continue
			}
			sc := scoredCandidate{cand: c, lic: lic, dl: src}
			if lic.AttributionRequired && !in.cfg.AllowAttributionLicenses {
				gated = append(gated, sc)
				continue
			}
			eligible = append(eligible, sc)
		}
		// Over-provision before stopping (SPEC §2.4 "len(accumulator) >= N*2"):
		// download fall-through (§2.5 #5) consumes a candidate per failed
		// rendition, so an exactly-N pool under-fills the gallery on any 404.
		// The extra headroom also ensures the Wikimedia fallback is still probed
		// for gated BY/SA candidates when iNat is CC0-rich.
		if len(eligible) >= n*2 {
			break
		}
	}
	rankScored(eligible)
	rankScored(gated)
	return eligible, gated
}

// pace sleeps MinInterval between source operations (SPEC §4.1 etiquette).
func (in *Ingestor) pace(ctx context.Context) {
	if in.cfg.MinInterval <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(in.cfg.MinInterval):
	}
}

// --- single-flight (SPEC §4.2 / §9 #11 / #16) ---

func (in *Ingestor) acquire(slug string) bool {
	in.flightMu.Lock()
	defer in.flightMu.Unlock()
	if in.inflight[slug] {
		return false
	}
	in.inflight[slug] = true
	return true
}

func (in *Ingestor) release(slug string) {
	in.flightMu.Lock()
	delete(in.inflight, slug)
	in.flightMu.Unlock()
}

// shouldSkip reports whether a prior file row means "don't re-attempt this
// trigger" (SPEC §3). ingested / no_acceptable_image are terminal-this-pass;
// deferred_attribution is re-cascaded once the gate is ON (SPEC §9 #13); failed
// always retries (transient/infra error must self-heal — never a permanent
// negative cache, so an outage cannot park a healthy slot forever).
func (in *Ingestor) shouldSkip(prior *FileRow) bool {
	switch prior.Status {
	case StatusIngested:
		return true
	case StatusNoAcceptableImg:
		return true
	case StatusDeferredAttrib:
		return !in.cfg.AllowAttributionLicenses
	case StatusFailed:
		return false
	default:
		return false
	}
}

// statusToOutcome maps a persisted IngestStatus back to the ImageStatus reported
// when a prior row causes a skip (so the outcome reflects the existing state).
func statusToOutcome(s IngestStatus) ImageStatus {
	switch s {
	case StatusIngested:
		return ImgSkippedExists
	case StatusNoAcceptableImg:
		return ImgNoAcceptable
	case StatusDeferredAttrib:
		return ImgDeferredAttrib
	default:
		return ImgSourceError
	}
}

// --- ledger writes (SPEC §3 status mapping) ---

func (in *Ingestor) ingestedOutcome(i int, key string, pick scoredCandidate, mime string, n int64) ImageOutcome {
	return ImageOutcome{
		Index:        i,
		Status:       ImgIngested,
		R2Key:        key,
		Source:       pick.cand.Source,
		License:      pick.lic.Code,
		LicenseShort: pick.lic.ShortName,
		LicenseURL:   licenseURLOf(pick.lic),
		Author:       pick.lic.Author,
		SourceURL:    pick.cand.PageURL,
		MIME:         mime,
		Bytes:        n,
		Width:        pick.cand.Width,
		Height:       pick.cand.Height,
	}
}

func (in *Ingestor) recordIngested(ctx context.Context, slug string, oc ImageOutcome) {
	in.upsertFile(ctx, FileRow{
		Slug:                slug,
		ImageIndex:          oc.Index,
		Status:              StatusIngested,
		R2Key:               oc.R2Key,
		Source:              oc.Source,
		SourceURL:           oc.SourceURL,
		LicenseCode:         oc.License,
		LicenseShort:        oc.LicenseShort,
		LicenseURL:          oc.LicenseURL,
		AttributionAuthor:   oc.Author,
		AttributionRequired: attributionRequiredFamily(oc.License),
		MIME:                oc.MIME,
		Bytes:               oc.Bytes,
		Width:               oc.Width,
		Height:              oc.Height,
	})
}

// recordSkippedExists handles a HEAD hit (R2 has the key). The skip carries no
// license/author (we never searched), so a PRIOR attribution row is preserved
// UNCHANGED — re-writing would wipe a live CC-BY/SA image's required credit. No
// prior row (object present but ledger missing) → minimal ingested marker.
func (in *Ingestor) recordSkippedExists(ctx context.Context, slug string, i int, key string, prior *FileRow) {
	if prior != nil {
		return
	}
	in.upsertFile(ctx, FileRow{
		Slug: slug, ImageIndex: i, Status: StatusIngested, R2Key: key,
	})
}

func (in *Ingestor) recordNoAcceptable(ctx context.Context, slug string, i int, oc ImageOutcome, prior *FileRow) {
	in.upsertFile(ctx, FileRow{
		Slug: slug, ImageIndex: i, Status: StatusNoAcceptableImg,
		LastError: oc.Note, Attempts: priorAttempts(prior),
	})
}

func (in *Ingestor) recordDeferred(ctx context.Context, slug string, oc ImageOutcome, pendingURL string, prior *FileRow) {
	in.upsertFile(ctx, FileRow{
		Slug: slug, ImageIndex: oc.Index, Status: StatusDeferredAttrib,
		Source: oc.Source, SourceURL: oc.SourceURL, PendingURL: pendingURL,
		LicenseCode: oc.License, LicenseShort: oc.LicenseShort, LicenseURL: oc.LicenseURL,
		AttributionAuthor: oc.Author, AttributionRequired: true,
		Attempts: priorAttempts(prior),
	})
}

func (in *Ingestor) recordFailed(ctx context.Context, slug string, i int, oc ImageOutcome, prior *FileRow) {
	in.upsertFile(ctx, FileRow{
		Slug: slug, ImageIndex: i, Status: StatusFailed,
		LastError: oc.Note, Attempts: priorAttempts(prior) + 1,
	})
}

func (in *Ingestor) upsertFile(ctx context.Context, row FileRow) {
	if err := in.ledger.UpsertFile(ctx, row); err != nil {
		log.Printf("imageingest upsert file err: slug=%s i=%d status=%s err=%v",
			row.Slug, row.ImageIndex, row.Status, err)
	}
}

func priorAttempts(prior *FileRow) int {
	if prior == nil {
		return 0
	}
	return prior.Attempts
}

// --- candidate classification + ranking (SPEC §2.4.3 / §2.5) ---

// scoredCandidate pairs a raw source candidate with its classified license and
// the source client to download it from (UA differs per source, SPEC §4.1).
type scoredCandidate struct {
	cand sources.Candidate
	lic  License
	dl   ImageSource
}

// classifyCandidate runs the token-membership classifier (SPEC §2.4.3) on a raw
// candidate, stripping any HTML in the author first (Commons Artist is HTML;
// iNat attribution is plain text — stripHTML is a no-op there).
func classifyCandidate(c sources.Candidate) License {
	return ClassifyLicenseCode(c.LicenseCode, c.LicenseShortName, c.LicenseURL, stripHTML(c.Author))
}

// deprioritizeTokens in a title signal a non-photo (map / diagram / herbarium).
var deprioritizeTokens = []string{
	"map", "range", "distribution", "herbarium", "illustration", "diagram", "chart", "locator",
}

// rankScored stable-sorts in place by license tier → source tier → photo-
// likeness → pixel area (SPEC §2.5). Small N (≤ ~24) → insertion sort.
func rankScored(cands []scoredCandidate) {
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && lessScored(cands[j], cands[j-1]); j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
}

func lessScored(a, b scoredCandidate) bool {
	at, bt := licenseTier(a.lic.Family), licenseTier(b.lic.Family)
	if at != bt {
		return at < bt // less restrictive first
	}
	as, bs := sourceTier(a.cand.Source), sourceTier(b.cand.Source)
	if as != bs {
		return as < bs // iNat before Wikimedia (SPEC §2.5 #2)
	}
	ap, bp := isPhotoLikely(a.cand), isPhotoLikely(b.cand)
	if ap != bp {
		return ap // photo-likely before deprioritized
	}
	return pixelArea(a.cand) > pixelArea(b.cand) // larger area first
}

// licenseTier maps a family to a sort rank (lower = preferred). CC0 / PD tie.
func licenseTier(f LicenseFamily) int {
	switch f {
	case FamilyCC0, FamilyPD:
		return 0
	case FamilyCCBY:
		return 1
	case FamilyCCBYSA:
		return 2
	default:
		return 3
	}
}

// sourceTier prefers iNat over Wikimedia within a license tier (SPEC §2.5 #2).
func sourceTier(source string) int {
	if source == sources.SourceINaturalist {
		return 0
	}
	return 1
}

func isExcludedFormat(c sources.Candidate) bool {
	switch strings.ToLower(c.MIME) {
	case "image/svg+xml", "image/gif", "image/tiff":
		return true
	}
	t := strings.ToLower(c.Title)
	return strings.HasSuffix(t, ".svg") || strings.HasSuffix(t, ".gif") ||
		strings.HasSuffix(t, ".tif") || strings.HasSuffix(t, ".tiff")
}

func isPhotoLikely(c sources.Candidate) bool {
	t := strings.ToLower(c.Title)
	for _, tok := range deprioritizeTokens {
		if strings.Contains(t, tok) {
			return false
		}
	}
	return true
}

func pixelArea(c sources.Candidate) int64 {
	return int64(c.Width) * int64(c.Height)
}

// attributionRequiredFamily reports whether a machine code denotes an
// attribution-required family (CC_BY / CC_BY_SA). CC0 / PD → false.
func attributionRequiredFamily(code string) bool {
	tokens := tokenize(code)
	if hasToken(tokens, "cc0") || hasToken(tokens, "pd") || hasToken(tokens, "publicdomain") {
		return false
	}
	return hasToken(tokens, "by")
}

// licenseURLOf returns the license deed URL for a classified candidate: the
// upstream-provided URL (Commons LicenseUrl) when present, else the canonical
// CC deed derived from the machine code (iNat carries no URL).
func licenseURLOf(lic License) string {
	if strings.TrimSpace(lic.URL) != "" {
		return lic.URL
	}
	return ccDeedURL(lic.Code)
}

// ccDeedURL maps a machine code to the canonical Creative Commons deed URL.
// Empty for codes with no single deed (PD / unknown). Used for credits.json.
func ccDeedURL(code string) string {
	tokens := tokenize(code)
	if hasToken(tokens, "cc0") {
		return "https://creativecommons.org/publicdomain/zero/1.0/"
	}
	if !hasToken(tokens, "cc") || !hasToken(tokens, "by") {
		return ""
	}
	parts := []string{"by"}
	if hasToken(tokens, "sa") {
		parts = append(parts, "sa")
	}
	return "https://creativecommons.org/licenses/" + strings.Join(parts, "-") + "/" + ccVersion(tokens) + "/"
}

// ccVersion extracts an "X.Y" version from the license tokens, defaulting 4.0.
func ccVersion(tokens []string) string {
	var nums []string
	for _, t := range tokens {
		isNum := t != ""
		for _, r := range t {
			if r < '0' || r > '9' {
				isNum = false
				break
			}
		}
		if isNum {
			nums = append(nums, t)
		}
	}
	if len(nums) >= 2 {
		return nums[len(nums)-2] + "." + nums[len(nums)-1]
	}
	if len(nums) == 1 {
		return nums[0] + ".0"
	}
	return "4.0"
}
