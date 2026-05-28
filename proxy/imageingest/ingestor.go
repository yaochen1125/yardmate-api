package imageingest

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// IngestOutcomeStatus is the per-species result of an ingest attempt (SPEC §3).
// It maps onto the persisted IngestStatus (e.g. skipped_exists → ingested,
// source_error/upload_error → failed).
type IngestOutcomeStatus string

const (
	OutcomeIngested        IngestOutcomeStatus = "ingested"
	OutcomeSkippedExists   IngestOutcomeStatus = "skipped_exists"
	OutcomeNoAcceptableImg IngestOutcomeStatus = "no_acceptable_image"
	OutcomeDeferredAttrib  IngestOutcomeStatus = "deferred_attribution"
	OutcomeSourceError     IngestOutcomeStatus = "source_error"
	OutcomeUploadError     IngestOutcomeStatus = "upload_error"
)

// IngestOutcome is the result of IngestOne (SPEC §1.4). Returned by the
// single-slug HTTP path verbatim (forensics only — not a client contract).
type IngestOutcome struct {
	Status         IngestOutcomeStatus `json:"status"`
	Slug           string              `json:"slug"`
	ScientificName string              `json:"scientific_name"`
	R2Key          string              `json:"r2_key,omitempty"`
	License        string              `json:"license,omitempty"` // machine code
	LicenseShort   string              `json:"license_short,omitempty"`
	Author         string              `json:"author,omitempty"`
	FilePage       string              `json:"file_page,omitempty"`
	ThumbURL       string              `json:"thumb_url,omitempty"` // chosen 1600px rendition (stored on deferred rows)
	MIME           string              `json:"mime,omitempty"`
	Bytes          int64               `json:"bytes,omitempty"`
	Width          int                 `json:"width,omitempty"`
	Height         int                 `json:"height,omitempty"`
	Note           string              `json:"note,omitempty"`
}

// BatchSummary is the result of RunBatch (SPEC §1.4). Counts only — no PII.
type BatchSummary struct {
	Seen      int `json:"seen"`      // seeds returned by the seed query
	Attempted int `json:"attempted"` // species actually processed this pass
	Ingested  int `json:"ingested"`
	NoImage   int `json:"no_image"`
	Deferred  int `json:"deferred"`
	Skipped   int `json:"skipped"` // skipped_exists (HEAD/ledger agree)
	Errors    int `json:"errors"`  // source_error + upload_error
}

// --- Collaborator interfaces (mocked in ingestor_test, SPEC §10) ---

// CommonsSearcher is the Wikimedia surface the ingestor needs. *CommonsClient
// satisfies it.
type CommonsSearcher interface {
	Search(ctx context.Context, scientificName string, limit int) ([]Candidate, error)
	Download(ctx context.Context, rawURL string) ([]byte, string, error)
}

// LedgerStore is the ledger surface the ingestor needs. *Ledger satisfies it.
type LedgerStore interface {
	Lookup(ctx context.Context, slug string) (*LedgerRow, error)
	Upsert(ctx context.Context, row LedgerRow) error
	IngestedRows(ctx context.Context) ([]LedgerRow, error)
}

// SeedSource is the seed surface the ingestor needs. *SeedReader satisfies it.
type SeedSource interface {
	Seeds(ctx context.Context) ([]string, error)
}

// Config holds the ingestor's tunable behavior (SPEC §1.3 / §2.4 / §4).
type Config struct {
	// AllowAttributionLicenses is the Codex #22 release-coordination gate. When
	// false (V1 default), CC-BY / CC-BY-SA are NOT uploaded — a species whose
	// only acceptable candidates are BY/SA → deferred_attribution. CC0 / PD are
	// always eligible. Flip true ONLY after the iOS Credits page is live.
	AllowAttributionLicenses bool
	// MinInterval paces serial Wikimedia calls within a batch (SPEC §4).
	MinInterval time.Duration
	// SearchLimit caps gsrlimit (default 10).
	SearchLimit int
	// BatchLimit caps species processed per RunBatch when the HTTP/ticker
	// caller passes 0 (SPEC §1.3).
	BatchLimit int
	// MaxAttempts caps the failed-attempt retry counter before a slug is
	// treated as negative-cached (SPEC §3, default 5).
	MaxAttempts int
}

const (
	defaultBatchLimit  = 25
	defaultMaxAttempts = 5
	defaultSearchLimit = 10
	heroCacheControl   = "public, max-age=31536000, immutable"
)

// heroKey is the R2 object key for a slug's hero (SPEC §2.2).
func heroKey(slug string) string { return "plant_images/" + slug + "/hero.png" }

// Ingestor orchestrates per-species ingest + batch passes (SPEC §2.1). All
// collaborators are interfaces so the core is fully mockable.
type Ingestor struct {
	commons CommonsSearcher
	store   ObjectStore
	ledger  LedgerStore
	seeds   SeedSource
	cfg     Config

	// single-flight guard so a ticker pass + manual trigger (or two ticks)
	// never run concurrently (SPEC §9 #11).
	runMu   sync.Mutex
	running bool
}

// NewIngestor builds an Ingestor. Defaults are applied to zero Config fields.
func NewIngestor(commons CommonsSearcher, store ObjectStore, ledger LedgerStore, seeds SeedSource, cfg Config) *Ingestor {
	if cfg.MinInterval < 0 {
		cfg.MinInterval = 0
	}
	if cfg.SearchLimit <= 0 {
		cfg.SearchLimit = defaultSearchLimit
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = defaultBatchLimit
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	return &Ingestor{commons: commons, store: store, ledger: ledger, seeds: seeds, cfg: cfg}
}

// IngestOne fills one species' hero (SPEC §2.1 IngestOne flow). slug is the R2
// key segment (caller-derived, parameterized for future genus reuse);
// searchTerm is the Wikimedia query (the scientific name). It NEVER returns a
// hard error for an ordinary per-species failure — those collapse into the
// Status; an error is returned only for a programmer/precondition fault.
func (in *Ingestor) IngestOne(ctx context.Context, slug, searchTerm string) (IngestOutcome, error) {
	out := IngestOutcome{Slug: slug, ScientificName: searchTerm}

	// 1. R2 is the source of truth for "image present" (SPEC §9 #8). HEAD first.
	exists, err := in.store.Exists(ctx, heroKey(slug))
	if err != nil {
		out.Status = OutcomeSourceError
		out.Note = "head check failed"
		return out, nil
	}
	if exists {
		out.Status = OutcomeSkippedExists
		out.R2Key = heroKey(slug)
		return out, nil
	}

	// 2. Search Wikimedia.
	cands, err := in.commons.Search(ctx, searchTerm, in.cfg.SearchLimit)
	if err != nil {
		out.Status = OutcomeSourceError
		out.Note = "search failed"
		return out, nil
	}

	// 3. Partition acceptable (license-allowed) candidates into upload-eligible
	//    (per the attribution gate) vs gated (BY/SA while the flag is OFF).
	var eligible, gated []Candidate
	for _, c := range cands {
		if !c.License.Allowed {
			continue
		}
		if strings.TrimSpace(c.ThumbURL) == "" {
			// Commons couldn't render a 1600px rendition for this file; we only
			// ever store the scaled rendition (D2), so it's unusable — skip it
			// rather than later soft-fail Download("") and risk a spurious
			// source_error when it was the only candidate.
			continue
		}
		if c.License.AttributionRequired && !in.cfg.AllowAttributionLicenses {
			gated = append(gated, c)
			continue
		}
		eligible = append(eligible, c)
	}

	if len(eligible) == 0 {
		if len(gated) > 0 {
			// Only acceptable candidates are gated BY/SA → defer. Store the
			// chosen candidate so flipping the gate uploads WITHOUT re-search
			// (SPEC §2.4 / §9 #13).
			pick := selectBest(gated)
			out.Status = OutcomeDeferredAttrib
			out.License = pick.License.Code
			out.LicenseShort = pick.License.ShortName
			out.Author = pick.License.Author
			out.FilePage = pick.PageURL
			out.ThumbURL = pick.ThumbURL
			out.Note = "attribution gate off"
			return out, nil
		}
		out.Status = OutcomeNoAcceptableImg
		out.Note = "no acceptable license"
		return out, nil
	}

	// 4. Rank eligible candidates (license tier → photo-likeness/size) and try
	//    each in order; first that downloads + MIME-checks wins. On download
	//    failure fall through to the next before declaring source_error.
	ranked := rankCandidates(eligible)
	var lastErr error
	for _, pick := range ranked {
		data, mime, derr := in.commons.Download(ctx, pick.ThumbURL)
		if derr != nil {
			lastErr = derr
			continue
		}
		// 5. Upload verbatim with the real MIME (SPEC §2.6).
		if uerr := in.store.Put(ctx, heroKey(slug), data, mime, heroCacheControl); uerr != nil {
			out.Status = OutcomeUploadError
			out.Note = "put failed"
			return out, nil
		}
		out.Status = OutcomeIngested
		out.R2Key = heroKey(slug)
		out.License = pick.License.Code
		out.LicenseShort = pick.License.ShortName
		out.Author = pick.License.Author
		out.FilePage = pick.PageURL
		out.MIME = mime
		out.Bytes = int64(len(data))
		out.Width = pick.Width
		out.Height = pick.Height
		return out, nil
	}

	// All eligible candidates failed to download.
	out.Status = OutcomeSourceError
	if lastErr != nil {
		out.Note = "all downloads failed"
	} else {
		out.Note = "no downloadable candidate"
	}
	return out, nil
}

// RunBatch processes up to `limit` un-done seeds serially (SPEC §2.1 RunBatch
// flow), then regenerates credits.json. limit<=0 uses cfg.BatchLimit. It is
// guarded by a single-flight mutex (SPEC §9 #11) so concurrent ticker/manual
// passes don't overlap; a second concurrent caller returns an empty summary.
func (in *Ingestor) RunBatch(ctx context.Context, limit int) (BatchSummary, error) {
	in.runMu.Lock()
	if in.running {
		in.runMu.Unlock()
		log.Printf("imageingest batch skipped: a pass is already running")
		return BatchSummary{}, nil
	}
	in.running = true
	in.runMu.Unlock()
	defer func() {
		in.runMu.Lock()
		in.running = false
		in.runMu.Unlock()
	}()

	if limit <= 0 {
		limit = in.cfg.BatchLimit
	}

	var summary BatchSummary
	seeds, err := in.seeds.Seeds(ctx)
	if err != nil {
		return summary, err
	}
	summary.Seen = len(seeds)

	for _, name := range seeds {
		if summary.Attempted >= limit {
			break
		}
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		slug := Slug(name)
		if slug == "" {
			// Name has no ASCII slug characters (e.g. "×" / a non-Latin string),
			// so there is no valid R2 key (plant_images//hero.png) and it can
			// never be ingested. Do NOT write a ledger row: its PK would be the
			// raw name, but every lookup keys on slug=="" and would never find it
			// (Codex #23) — a dead, never-read upsert each pass. Just skip;
			// re-evaluating next pass is a cheap Slug() call with no I/O.
			log.Printf("imageingest skip: name=%q slugs to empty (no ASCII slug chars)", name)
			continue
		}

		// Skip already-done / negative-cached slugs (SPEC §3). deferred_attribution
		// is re-processed when the gate is ON (NOT permanent negative cache,
		// SPEC §9 #13).
		prior, lerr := in.ledger.Lookup(ctx, slug)
		if lerr != nil {
			log.Printf("imageingest ledger lookup err: slug=%s err=%v", slug, lerr)
			summary.Errors++
			continue
		}
		if in.shouldSkip(prior) {
			summary.Skipped++
			continue
		}

		out, _ := in.IngestOne(ctx, slug, name)
		summary.Attempted++
		in.recordOutcome(ctx, slug, name, out, prior)

		switch out.Status {
		case OutcomeIngested, OutcomeSkippedExists:
			summary.Ingested++
		case OutcomeNoAcceptableImg:
			summary.NoImage++
		case OutcomeDeferredAttrib:
			summary.Deferred++
		case OutcomeSourceError, OutcomeUploadError:
			summary.Errors++
		}

		if in.cfg.MinInterval > 0 {
			select {
			case <-ctx.Done():
				return summary, ctx.Err()
			case <-time.After(in.cfg.MinInterval):
			}
		}
	}

	// Regenerate the public credits manifest from the ledger (full rebuild,
	// SPEC §2.7 / §9 #15). A failure here is logged but does not fail the batch.
	if err := in.rebuildCredits(ctx); err != nil {
		log.Printf("imageingest credits rebuild err: %v", err)
	}

	return summary, nil
}

// shouldSkip decides whether a prior ledger row means "don't re-attempt this
// pass" (SPEC §3). nil prior → never skip (fresh). The NOIMAGE_TTL re-check is
// a §8 refinement (SPEC §6.1); V1 treats no_acceptable_image + capped failures
// as skip-this-pass. deferred_attribution is skipped only while the gate is OFF.
func (in *Ingestor) shouldSkip(prior *LedgerRow) bool {
	if prior == nil {
		return false
	}
	switch prior.Status {
	case StatusIngested:
		return true
	case StatusNoAcceptableImg:
		return true
	case StatusDeferredAttrib:
		// Re-process when the gate is ON (so it can upload); skip while OFF.
		return !in.cfg.AllowAttributionLicenses
	case StatusFailed:
		// Always retry in V1: `failed` means a transient / infra error (Wikimedia
		// 5xx, R2 HEAD/PUT blip), which should self-heal — NOT a stable "no free
		// image" (that's no_acceptable_image, cached above). Permanently
		// negative-caching transient failures would park a healthy species
		// forever on an outage (there is no TTL re-check in V1). `attempts` is
		// retained as an observability counter only (SPEC §3).
		return false
	default:
		return false
	}
}

// recordOutcome upserts the ledger row for an outcome (SPEC §3 status mapping).
func (in *Ingestor) recordOutcome(ctx context.Context, slug, name string, out IngestOutcome, prior *LedgerRow) {
	row := LedgerRow{
		Slug:           slug,
		ScientificName: name,
		Source:         SourceWikimediaCommons,
	}
	priorAttempts := 0
	if prior != nil {
		priorAttempts = prior.Attempts
	}

	switch out.Status {
	case OutcomeSkippedExists:
		// R2 already has the hero (HEAD hit). The skipped outcome carries NO
		// license/author (we never searched), so preserve any prior attribution
		// row UNCHANGED — re-writing would wipe a live image's credit, e.g. on a
		// re-run of the single-slug smoke path. If there's no prior row (object
		// in R2 but ledger missing — manual upload / ledger reset), record only a
		// minimal ingested marker; attribution is unknown and not re-derivable
		// here (a future reconcile job could backfill it from Commons).
		if prior != nil {
			return
		}
		row.Status = StatusIngested
		row.R2Key = heroKey(slug)
		row.Attempts = priorAttempts
	case OutcomeIngested:
		row.Status = StatusIngested
		row.R2Key = heroKey(slug)
		row.LicenseCode = out.License
		row.LicenseShort = out.LicenseShort
		row.LicenseURL = licenseURLFor(out)
		row.AttributionAuthor = out.Author
		// CC-BY / CC-BY-SA require attribution regardless of whether we captured
		// an author string (extmetadata Artist can be absent); derive from the
		// license family only, never gate on Author presence.
		row.AttributionRequired = attributionRequiredFamily(out.License)
		row.SourceFilePage = out.FilePage
		row.MIME = out.MIME
		row.Bytes = out.Bytes
		row.Width = out.Width
		row.Height = out.Height
		row.Attempts = priorAttempts
	case OutcomeNoAcceptableImg:
		row.Status = StatusNoAcceptableImg
		row.LastError = out.Note
		row.Attempts = priorAttempts
	case OutcomeDeferredAttrib:
		row.Status = StatusDeferredAttrib
		// Store the chosen 1600px rendition URL (NOT the File: page) for a future
		// flip-fast-path that uploads without re-searching (§8). V1 re-searches on
		// flip, so this is currently write-only provenance.
		row.PendingThumbURL = out.ThumbURL
		row.LicenseCode = out.License
		row.LicenseShort = out.LicenseShort
		row.LicenseURL = licenseURLFor(out)
		row.AttributionAuthor = out.Author
		row.AttributionRequired = true
		row.SourceFilePage = out.FilePage
		row.Attempts = priorAttempts
	case OutcomeSourceError, OutcomeUploadError:
		row.Status = StatusFailed
		row.LastError = out.Note
		row.Attempts = priorAttempts + 1
	default:
		row.Status = StatusFailed
		row.LastError = "unknown outcome"
		row.Attempts = priorAttempts + 1
	}

	if err := in.ledger.Upsert(ctx, row); err != nil {
		log.Printf("imageingest ledger upsert err: slug=%s status=%s err=%v", slug, row.Status, err)
	}
}

// Start launches a background ticker that runs RunBatch(cfg.BatchLimit) every
// interval (SPEC §2.1). Returns a stop func. interval<=0 is a no-op (ticker
// disabled — manual-only). The single-flight guard in RunBatch prevents a slow
// pass from overlapping the next tick (SPEC §9 #11).
func (in *Ingestor) Start(interval time.Duration) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				summary, err := in.RunBatch(ctx, 0)
				cancel()
				if err != nil {
					log.Printf("imageingest ticker batch err: %v", err)
				} else {
					log.Printf("imageingest ticker batch: seen=%d attempted=%d ingested=%d noImage=%d deferred=%d skipped=%d errors=%d",
						summary.Seen, summary.Attempted, summary.Ingested, summary.NoImage,
						summary.Deferred, summary.Skipped, summary.Errors)
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// --- candidate ranking (SPEC §2.5) ---

// deprioritizeTokens in a title signal a non-photo (map / diagram / herbarium).
var deprioritizeTokens = []string{
	"map", "range", "distribution", "herbarium", "illustration", "diagram", "chart", "locator",
}

// rankCandidates orders by license tier (CC0=PD > CC_BY > CC_BY_SA), then
// photo-likeness (non-deprioritized title), then larger pixel area (SPEC §2.5).
// SVG/GIF/TIFF are excluded outright. Returns a new slice; input untouched.
func rankCandidates(cands []Candidate) []Candidate {
	filtered := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if isExcludedFormat(c) {
			continue
		}
		filtered = append(filtered, c)
	}
	// Stable insertion sort by the comparison below (small N ≤ 10).
	for i := 1; i < len(filtered); i++ {
		for j := i; j > 0 && less(filtered[j], filtered[j-1]); j-- {
			filtered[j], filtered[j-1] = filtered[j-1], filtered[j]
		}
	}
	return filtered
}

// selectBest returns the single best candidate per the ranking (SPEC §2.5).
// Used for the deferred_attribution pick where we only need the top choice.
func selectBest(cands []Candidate) Candidate {
	ranked := rankCandidates(cands)
	if len(ranked) > 0 {
		return ranked[0]
	}
	// All excluded by format — fall back to the first raw candidate so we still
	// capture some provenance for the deferred row.
	if len(cands) > 0 {
		return cands[0]
	}
	return Candidate{}
}

// less reports whether a should rank before b (a is "better").
func less(a, b Candidate) bool {
	at, bt := licenseTier(a.License.Family), licenseTier(b.License.Family)
	if at != bt {
		return at < bt // lower tier number = less restrictive = better
	}
	ap, bp := isPhotoLikely(a), isPhotoLikely(b)
	if ap != bp {
		return ap // photo-likely ranks before deprioritized
	}
	return pixelArea(a) > pixelArea(b) // larger area first
}

// licenseTier maps a family to a sort rank (lower = preferred). CC0 and PD tie.
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

func isExcludedFormat(c Candidate) bool {
	switch strings.ToLower(c.MIME) {
	case "image/svg+xml", "image/gif", "image/tiff":
		return true
	}
	// Also exclude by title extension as a backstop (mime can be blank).
	t := strings.ToLower(c.Title)
	return strings.HasSuffix(t, ".svg") || strings.HasSuffix(t, ".gif") || strings.HasSuffix(t, ".tif") || strings.HasSuffix(t, ".tiff")
}

func isPhotoLikely(c Candidate) bool {
	t := strings.ToLower(c.Title)
	for _, tok := range deprioritizeTokens {
		if strings.Contains(t, tok) {
			return false
		}
	}
	return true
}

func pixelArea(c Candidate) int64 {
	return int64(c.Width) * int64(c.Height)
}

// attributionRequiredFamily reports whether a machine code denotes a
// attribution-required family (CC_BY / CC_BY_SA). CC0 / PD → false.
func attributionRequiredFamily(code string) bool {
	tokens := tokenize(code)
	if hasToken(tokens, "cc0") || hasToken(tokens, "pd") || hasToken(tokens, "publicdomain") {
		return false
	}
	return hasToken(tokens, "by")
}

// licenseURLFor returns a license deed URL for the outcome. The extmetadata URL
// is captured into the FilePage path only for provenance; the credits manifest
// uses the canonical CC deed derived from the family (stable, never null for
// attribution-required rows). For V1 we derive from the machine code.
func licenseURLFor(out IngestOutcome) string {
	return ccDeedURL(out.License)
}

// ccDeedURL maps a machine code to the canonical Creative Commons deed URL.
// Empty for unknown codes (PD/ARR have no single deed). Used for credits.json.
func ccDeedURL(code string) string {
	tokens := tokenize(code)
	if hasToken(tokens, "cc0") {
		return "https://creativecommons.org/publicdomain/zero/1.0/"
	}
	if !hasToken(tokens, "cc") || !hasToken(tokens, "by") {
		return ""
	}
	// Build the path: by, by-sa, etc. + version.
	parts := []string{"by"}
	if hasToken(tokens, "sa") {
		parts = append(parts, "sa")
	}
	version := ccVersion(tokens)
	return "https://creativecommons.org/licenses/" + strings.Join(parts, "-") + "/" + version + "/"
}

// ccVersion extracts a "X.Y" version token from the license code tokens,
// defaulting to "4.0".
func ccVersion(tokens []string) string {
	// tokens like [cc by sa 4 0] → join trailing numerics into "4.0".
	var nums []string
	for _, t := range tokens {
		if t == "" {
			continue
		}
		isNum := true
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
