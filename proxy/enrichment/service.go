package enrichment

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/sync/singleflight"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// Typed errors returned by Service.GetOrGenerate.
//
// Handlers translate these to HTTP error codes per SPEC §3.
var (
	// ErrInvalidScientificName — empty / whitespace-only / no letter.
	ErrInvalidScientificName = errors.New("enrichment: invalid scientific name")

	// ErrScientificNameTooLong — > 200 chars after trim.
	ErrScientificNameTooLong = errors.New("enrichment: scientific name too long")
)

// Maximum trimmed length for scientificName (SPEC §2.1).
const maxScientificNameLen = 200

// maxCommonNameLen bounds the caller-supplied common name before it reaches the
// OpenAI prompt (token cost) and plants_pending.common_name. Over-length is
// silently truncated (rune-safe), never an error — the common name is optional
// prompt context, not an identity key.
const maxCommonNameLen = 150

// Source identifies which tier of GetOrGenerate's lookup produced the result.
// Returned alongside the *PlantDetail so handlers can log the path taken
// (SPEC §9 #10 forensic logging).
const (
	SourceCache                          = "cache"
	SourceCatalog                        = "catalog"
	SourceSupabaseHit                    = "supabase_hit"
	SourceSupabaseFallbackEn             = "supabase_fallback_en"          // requested lang missing, served English (§7)
	SourceSupabaseTranslatedOnDemand     = "supabase_translated_on_demand" // requested lang missing + no English yet, translated an existing other-language master (§7)
	SourceSupabaseMissGenerate           = "supabase_miss_generate"
	SourceSupabaseMissGenerateRaceWinner = "supabase_miss_generate_race_winner"
)

// Request bundles the validated handler-layer inputs.
type Request struct {
	ScientificName string // required; trimmed length 1..200; contains at least one letter
	CommonName     string // optional; passed to the LLM prompt as context
	PlantIDHint    string // optional, ignored in V1 (server re-derives via ContentIndex)
	Lang           string // optional; normalized via NormalizeLang (empty/unsupported → "en")
}

// ServiceDB is the small interface Service needs from the DB layer.
// *DB satisfies it; tests substitute a stub.
type ServiceDB interface {
	Lookup(ctx context.Context, normalized, lang string) (*proxy.PlantDetail, error)
	LookupAny(ctx context.Context, normalized string) (*proxy.PlantDetail, string, error)
	Insert(ctx context.Context, p InsertParams) (bool, error)
}

// ServiceLLM is the small interface Service needs from the LLM layer.
// *LLMClient satisfies it; tests substitute a stub.
type ServiceLLM interface {
	Generate(ctx context.Context, scientificName, commonName, lang string) (*proxy.PlantDetail, string, error)
	Translate(ctx context.Context, source *proxy.PlantDetail, toLang string) (*proxy.PlantDetail, string, error)
}

// Service orchestrates the three-tier lookup (catalog -> Supabase -> LLM)
// behind an in-process LRU+TTL cache. SPEC §1.1 + §2.1 lookup flow.
//
// All collaborators are optional in test scenarios — nil cache / nil db /
// nil llm gracefully short-circuit. In production main wires all four.
type Service struct {
	content    *proxy.ContentIndex
	db         ServiceDB
	llm        ServiceLLM
	cache      *Cache
	names      *NameResolver       // optional; nil → no authoritative-name override (SPEC §7 common_name C; catalog rows stay curated)
	backfill   *Backfiller         // optional; nil → no async translation backfill (tests / DB-less mode)
	diseaseIDs map[string]struct{} // for common_diseases_list whitelist

	// sf coalesces concurrent first-callers for the same (plant, lang) so N
	// simultaneous cache+DB misses pay ONE OpenAI generation/translation instead
	// of N. Zero value is ready to use; keyed by cacheKey (preciseName|lang).
	sf singleflight.Group
}

// SetBackfiller attaches the async translation backfiller (SPEC §7). Wired by
// main after the Service + DB + LLM exist; nil-safe so tests can skip it.
func (s *Service) SetBackfiller(b *Backfiller) {
	if s != nil {
		s.backfill = b
	}
}

// Backfiller returns the attached async backfiller (nil if none / nil service).
// Used by main to hand the worker pool to the periodic Sweeper.
func (s *Service) Backfiller() *Backfiller {
	if s == nil {
		return nil
	}
	return s.backfill
}

// SetNameResolver replaces the authoritative-name resolver (main wires one with
// both iNat and Wikidata; NewService's default uses iNat only). nil disables
// the override.
func (s *Service) SetNameResolver(r *NameResolver) {
	if s != nil {
		s.names = r
	}
}

// NewService builds a Service with the given dependencies. content may not
// be nil in production (path-1 catalog hit relies on it); db + llm + cache +
// inat may legitimately be nil during partial-degradation tests. inat backs the
// default authoritative-name resolver that overrides out-of-catalog common
// names (SPEC §7 common_name C; library-internal catalog results stay curated,
// matching identify-side priority library > iNat > upstream).
//
// Computes the catalog disease ID set once for fast whitelisting of
// LLM-generated common_diseases_list (SPEC §1.1 + §7 whitelist decision).
func NewService(content *proxy.ContentIndex, db ServiceDB, llm ServiceLLM, cache *Cache, inat *proxy.INatClient) *Service {
	var names *NameResolver
	if inat != nil {
		names = NewNameResolver(inat, nil)
	}
	diseaseIDs := make(map[string]struct{})
	if content != nil {
		for _, ref := range content.AllDiseaseNames() {
			diseaseIDs[ref.ID] = struct{}{}
		}
	}
	return &Service{
		content:    content,
		db:         db,
		llm:        llm,
		cache:      cache,
		names:      names,
		diseaseIDs: diseaseIDs,
	}
}

// GetOrGenerate runs the multi-tier, language-aware lookup per SPEC §2.1.
//
// Order: cache -> embedded catalog -> Supabase (exact lang) -> Supabase English
// fallback -> OpenAI LLM master generation (INSERT ON CONFLICT DO NOTHING +
// re-Lookup on race) -> async translation backfill. The cache is keyed by
// (precise scientific name, lang) and written on every successful row path.
//
// Returns the result plus a Source* string identifying which tier produced it
// (SPEC §9 #10 forensic logging). On error the source is "".
func (s *Service) GetOrGenerate(ctx context.Context, req Request) (*proxy.PlantDetail, string, error) {
	name := strings.TrimSpace(req.ScientificName)
	if name == "" || !hasLetter(name) {
		return nil, "", ErrInvalidScientificName
	}
	if len(name) > maxScientificNameLen {
		return nil, "", ErrScientificNameTooLong
	}
	normalized := proxy.NormalizeScientificName(name)
	if normalized == "" {
		return nil, "", ErrInvalidScientificName
	}
	lang := NormalizeLang(req.Lang)

	// Bound the optional common name (rune-safe) before it feeds the LLM prompt or
	// the plants_pending.common_name column. Over-length is truncated, not
	// rejected — it is prompt context, not an identity key (SPEC §2.1).
	req.CommonName = truncateRunes(strings.TrimSpace(req.CommonName), maxCommonNameLen)

	// Cache key is the PRECISE (infraspecific-preserving) normalization PLUS the
	// language (SPEC §9 #16 + #19). The catalog (Step 1) resolves distinct
	// plantIds for sibling varieties via scientificNameToIDPrecise, and each
	// language is a distinct entry — a species-level OR lang-less key would alias
	// one variety/language's *PlantDetail onto another within the TTL. Supabase
	// Lookup/Insert keep the species-level `normalized` as the PK first component,
	// with `lang` as the second.
	preciseName := proxy.NormalizeScientificNamePrecise(name)
	cacheKey := preciseName + "|" + lang

	// Authoritative display name (SPEC §7 common_name C): every NON-catalog
	// response gets its common_name from iNat / Wikidata in the request language
	// (→ English → scientific name), never the LLM's. Resolution is started once
	// (startName) and awaited lazily (awaitName) so it overlaps the DB lookups /
	// LLM call; the resolver's own 24h cache makes repeat lookups free and its
	// 1.5s cap bounds the first one. Catalog hits never start it. All closures
	// run on this goroutine (singleflight runs fn on the leader's goroutine).
	var nameCh chan resolveOutcome
	var nameResult *resolveOutcome
	startName := func() {
		if nameCh != nil {
			return
		}
		nameCh = make(chan resolveOutcome, 1)
		go func() {
			n, ok := s.names.Resolve(ctx, name, lang)
			nameCh <- resolveOutcome{n, ok}
		}()
	}
	awaitName := func() resolveOutcome {
		startName()
		if nameResult == nil {
			o := <-nameCh
			nameResult = &o
		}
		return *nameResult
	}
	applyName := func(d *proxy.PlantDetail, source string) *proxy.PlantDetail {
		if source == SourceCatalog || d == nil {
			return d
		}
		o := awaitName()
		if !o.ok {
			return d // unresolved (source outage) → keep the row's own name
		}
		out := *d // copy: do NOT mutate cached / DB-shared pointer
		out.CommonName = o.name.Name
		out.CommonNameSource = o.name.Source
		return &out
	}

	// Step 0: in-process LRU cache.
	if cached, ok := s.cache.Get(cacheKey); ok {
		return applyName(cached, SourceCache), SourceCache, nil
	}

	// Step 1: embedded 1522 catalog. Lang-agnostic today — the curated catalog is
	// English; its multi-language is the separate offline plants_detail.json
	// pipeline (SPEC §7). A non-English request for a catalog plant returns the
	// English curated text, which is exactly the display-fallback behavior.
	if s.content != nil {
		if plantID, ok := s.content.LookupPlantID(name); ok {
			if full, ok := s.content.LookupFullDetail(plantID); ok {
				// Do NOT cache catalog hits (PR #26 self-review P0): the LRU has
				// no source tag, so it would come back as SourceCache and become
				// eligible for the iNat override. Catalog is already an O(1)
				// in-memory map lookup.
				return full, SourceCatalog, nil
			}
			// Index inconsistency — fall through; treat as miss rather than crash.
		}
	}

	// Step 2: Supabase plants_pending, exact (normalized, lang).
	if s.db == nil {
		// No DB configured AND not in the catalog -> enrichment unavailable.
		return nil, "", ErrEnrichmentUnavailable
	}

	// Kick off name resolution now so it runs concurrently with the DB lookups
	// and any LLM call below.
	startName()

	row, err := s.db.Lookup(ctx, normalized, lang)
	if err != nil {
		return nil, "", err
	}
	if row != nil {
		s.cache.Set(cacheKey, row)
		return applyName(row, SourceSupabaseHit), SourceSupabaseHit, nil
	}

	// Step 3: display fallback — exact lang missing, serve the English row if it
	// exists (SPEC §7). Cache it under the ENGLISH key only, NEVER the requested
	// lang key (SPEC §9 #20) so the real lang row is not masked once backfilled.
	if lang != "en" {
		// Check the cache under the ENGLISH key first (#5): a prior English request
		// (or English fallback) may already hold the master, so serve it without a
		// second DB round-trip. Same source tag + no override as the DB path below.
		if enCached, ok := s.cache.Get(preciseName + "|en"); ok {
			return applyName(enCached, SourceSupabaseFallbackEn), SourceSupabaseFallbackEn, nil
		}
		enRow, err := s.db.Lookup(ctx, normalized, "en")
		if err != nil {
			return nil, "", err
		}
		if enRow != nil {
			s.cache.Set(preciseName+"|en", enRow)
			return applyName(enRow, SourceSupabaseFallbackEn), SourceSupabaseFallbackEn, nil
		}
		// English also missing → fall through and generate the master in `lang`.
	}

	if s.llm == nil {
		return nil, "", ErrEnrichmentUnavailable
	}

	// Steps 4–7 are the expensive, uncached path: translate-on-demand or a fresh
	// OpenAI master generation, plus INSERT + async backfill. Coalesce concurrent
	// first-callers for the SAME (plant, lang) via singleflight so N simultaneous
	// cache+DB misses pay ONE OpenAI round-trip; late followers reuse the leader's
	// result. Key = cacheKey (preciseName|lang), matching the LRU key. On error
	// every follower receives the leader's error. The result carries the raw
	// detail + source; applyName is applied per-caller AFTER Do so each caller
	// gets its own (copied) authoritative name.
	v, err, _ := s.sf.Do(cacheKey, func() (any, error) {
		// Detach the SHARED generation from any single caller's context. This
		// flight is coalesced across all concurrent first-callers, so if the
		// leader's client disconnects (or its deadline expires) mid-generation,
		// the followers — whose own connections are still alive — must NOT inherit
		// its context.Canceled and 5xx. WithoutCancel keeps values but drops the
		// leader's cancellation + deadline; re-impose an independent timeout so the
		// flight still can't hang. Shadows ctx so every downstream call below uses it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
		defer cancel()

		// A leader that finished while this follower was blocked entering Do has
		// already populated the cache (callers arriving AFTER the leader returns do
		// NOT coalesce) — re-check before paying for generation.
		if cached, ok := s.cache.Get(cacheKey); ok {
			return genResult{cached, SourceCache}, nil
		}

		hint := req.CommonName

		// Step 4: before generating a fresh master, check whether a master already
		// exists for this plant in ANOTHER language — i.e. we are racing this
		// plant's backfill (neither the exact lang nor English is present yet). If
		// so, translate that master into `lang` rather than generating an
		// independent second master, which would let care facts diverge across
		// language rows (Codex P2 — the SPEC §7 one-master invariant). Only a true
		// first-caller (no row in ANY language) reaches the Generate call below.
		if existing, _, lookErr := s.db.LookupAny(ctx, normalized); lookErr != nil {
			return nil, lookErr
		} else if existing != nil {
			if translated, reqID, tErr := s.llm.Translate(ctx, existing, lang); tErr == nil && translated != nil {
				inserted, insErr := s.db.Insert(ctx, InsertParams{
					Normalized:      normalized,
					Lang:            lang,
					ScientificName:  name,
					CommonName:      hint,
					Data:            translated,
					Source:          TranslatedSourceTag,
					SourceVersion:   translatedRowVersion(existing, translated),
					GenerationReqID: reqID,
				})
				if insErr != nil {
					return nil, insErr
				}
				if !inserted {
					// Another caller wrote this lang first — return their row.
					if row, _ := s.db.Lookup(ctx, normalized, lang); row != nil {
						s.cache.Set(cacheKey, row)
						return genResult{row, SourceSupabaseHit}, nil
					}
				}
				s.cache.Set(cacheKey, translated)
				return genResult{translated, SourceSupabaseTranslatedOnDemand}, nil
			}
			// Translation failed (rare) → fall through to generate a master in
			// `lang`. This is the only path that can produce a second independent
			// master, and only on translation failure.
		}

		// Step 5: true first-caller (or translation-failure fallback) — generate
		// the master in `lang`. A localized authoritative name becomes the hint so
		// the description prose uses the same name as the title (awaited only
		// here: resolution has been running since before the DB lookups).
		authName := awaitName()
		if authName.ok && authName.name.Localized() {
			hint = authName.name.Name
		}
		generated, requestID, genErr := s.llm.Generate(ctx, name, hint, lang)
		if genErr != nil {
			return nil, genErr
		}

		// Whitelist common_diseases_list against the catalog (SPEC §1.1 + §7).
		generated.CommonDiseasesList = s.filterCatalogDiseaseIDs(generated.CommonDiseasesList)

		// Bake a localized authoritative name into the stored master (the LLM may
		// ignore the hint). English / scientific-name fallbacks are NOT baked — they
		// are applied at response time so a better source can win later.
		if authName.ok && authName.name.Localized() {
			generated.CommonName = authName.name.Name
			generated.CommonNameSource = authName.name.Source
		}

		// Step 6: INSERT ON CONFLICT (normalized, lang) DO NOTHING. On conflict,
		// re-Lookup to pick up the master another concurrent caller just wrote — and
		// let them own the backfill (SPEC §2.1 step 5).
		inserted, insErr := s.db.Insert(ctx, InsertParams{
			Normalized:      normalized,
			Lang:            lang,
			ScientificName:  name,
			CommonName:      hint, // localized authoritative name when resolved, else upstream
			Data:            generated,
			Source:          SourceTag,
			SourceVersion:   PromptVersion,
			GenerationReqID: requestID,
		})
		if insErr != nil {
			return nil, insErr
		}

		if !inserted {
			// Concurrent race resolved by ON CONFLICT. The conflicting writer's
			// master is now available — return that to keep all callers consistent.
			if row, lookupErr := s.db.Lookup(ctx, normalized, lang); lookupErr == nil && row != nil {
				s.cache.Set(cacheKey, row)
				return genResult{row, SourceSupabaseMissGenerateRaceWinner}, nil
			}
			// Race re-Lookup also failed — return our generated copy. Same shape,
			// just a different LLM sample. The conflicting writer owns the backfill.
			s.cache.Set(cacheKey, generated)
			return genResult{generated, SourceSupabaseMissGenerate}, nil
		}

		s.cache.Set(cacheKey, generated)

		// Step 7: async backfill the other languages from this master (English
		// first), translate-only-prose, ON CONFLICT DO NOTHING (SPEC §7 + §9 #18).
		if s.backfill != nil {
			s.backfill.Enqueue(BackfillJob{
				Normalized:     normalized,
				ScientificName: name,
				CommonHint:     hint,
				SourceLang:     lang,
				Master:         generated,
			})
		}

		return genResult{generated, SourceSupabaseMissGenerate}, nil
	})
	if err != nil {
		return nil, "", err
	}
	res := v.(genResult)
	return applyName(res.detail, res.source), res.source, nil
}

// genResult bundles the (detail, source) pair returned through singleflight.Do,
// whose func returns a single any — so concurrent followers share one generation.
type genResult struct {
	detail *proxy.PlantDetail
	source string
}

// nativeRegionStaleVersion is stamped (instead of PromptVersion) on a translated
// row whose native_region could NOT be localized: the translator returned a
// mismatched element count so Translate (prompt.go) kept the master's
// source-language regions verbatim. It sorts before PromptVersion ("v5"), keeping
// the row selectable by ListNativeRegionBackfillRows (source_version < 'v5') so
// the one-shot native_region backfill retries it — otherwise un-localized regions
// would escape re-selection forever under a v5 stamp.
const nativeRegionStaleVersion = "v4"

// translatedRowVersion returns the source_version to stamp on a freshly translated
// row: PromptVersion normally, or nativeRegionStaleVersion when the master carried
// a non-empty native_region that Translate left unchanged (kept the master's
// regions because the model returned the wrong element count).
func translatedRowVersion(master, translated *proxy.PlantDetail) string {
	if master != nil && translated != nil &&
		len(master.NativeRegion) > 0 &&
		slices.Equal(master.NativeRegion, translated.NativeRegion) {
		return nativeRegionStaleVersion
	}
	return PromptVersion
}

// filterCatalogDiseaseIDs preserves order, drops entries not in the catalog
// disease ID set. Returns a non-nil slice (empty if all were dropped) so
// callers + JSON marshal produce [] not null.
func (s *Service) filterCatalogDiseaseIDs(input []string) []string {
	out := make([]string, 0, len(input))
	for _, id := range input {
		if _, ok := s.diseaseIDs[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// hasLetter reports whether s contains at least one Unicode letter. Used to
// reject inputs that are pure whitespace / digits / punctuation.
func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
