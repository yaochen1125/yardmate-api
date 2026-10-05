package enrichment

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
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
	inat       *proxy.INatClient   // optional iNat client; nil → skip name override (PR #24 follow-up, library-internal stays curated)
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
		// Share the ContentIndex so translations get the same kingdom merge + fungi
		// hard filter as the request path (finalizeKingdom). Set before the
		// backfiller can receive its first job.
		if b != nil {
			b.content = s.content
		}
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

// NewService builds a Service with the given dependencies. content may not
// be nil in production (path-1 catalog hit relies on it); db + llm + cache +
// inat may legitimately be nil during partial-degradation tests. inat is the
// iNaturalist taxa client used to override out-of-catalog common names with
// iNat preferred_common_name (PR #24 follow-up; library-internal catalog
// results stay curated, matching identify-side priority library > iNat >
// upstream).
//
// Computes the catalog disease ID set once for fast whitelisting of
// LLM-generated common_diseases_list (SPEC §1.1 + §7 whitelist decision).
func NewService(content *proxy.ContentIndex, db ServiceDB, llm ServiceLLM, cache *Cache, inat *proxy.INatClient) *Service {
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
		inat:       inat,
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

	// iNat preferred_common_name override applies to ENGLISH rows only (SPEC §7
	// common_name B): the iNat name is English, so injecting it into a localized
	// row would force English back in. The lookup itself is deferred until AFTER
	// the cache (Step 0) and catalog (Step 1) short-circuits so a hot-path hit
	// pays no 8s iNat round-trip; iNatName stays "" (a no-op override) until then.
	iNatName := ""
	// iNatKingdom is the authoritative kingdom from that SAME deferred iNat taxon
	// lookup (no extra round-trip). nil until/unless iNat resolves the name.
	var iNatKingdom *string
	iNatAsked := false // the taxon lookup already ran for this request
	overrideINat := func(d *proxy.PlantDetail, source string) *proxy.PlantDetail {
		if iNatName == "" || source == SourceCatalog || d == nil {
			return d
		}
		out := *d // copy: do NOT mutate cached / catalog-shared pointer
		out.CommonName = iNatName
		out.CommonNameSource = "inaturalist"
		return &out
	}

	// Step 0: in-process LRU cache.
	if cached, ok := s.cache.Get(cacheKey); ok {
		return overrideINat(cached, SourceCache), SourceCache, nil
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

	// Deferred iNat lookup (#1): only reached on a cache + catalog miss, i.e. a
	// request that will actually serve/generate a Supabase row. English-only —
	// the iNat preferred_common_name is English, so it must not touch localized
	// rows. Populated here so overrideINat refreshes Supabase/generated rows below
	// (cache + catalog hits already returned above and never pay this round-trip).
	//
	// The request is LookupTaxon (Plantae + Fungi scope), not the plants-only
	// PreferredCommonName: one round-trip yields the common name AND the kingdom
	// that drives the mushroom-safety notice (SPEC §7 kingdom).
	if lang == "en" && s.inat != nil {
		iNatAsked = true
		if t, ok := s.inat.LookupTaxon(ctx, name); ok {
			iNatName = t.CommonName
			iNatKingdom = proxy.NormalizeKingdom(t.Kingdom)
		}
	}

	row, err := s.db.Lookup(ctx, normalized, lang)
	if err != nil {
		return nil, "", err
	}
	if row != nil {
		// Rows written before v6 carry no kingdom; an iNat verdict fills it in on
		// the fly, and a fungal row is hard-filtered on read (legacy v1 rows can
		// hold culinary uses) — see applyKingdom.
		row = s.applyKingdom(row, name, iNatKingdom)
		s.cache.Set(cacheKey, row)
		return overrideINat(row, SourceSupabaseHit), SourceSupabaseHit, nil
	}

	// Step 3: display fallback — exact lang missing, serve the English row if it
	// exists (SPEC §7). Cache it under the ENGLISH key only, NEVER the requested
	// lang key (SPEC §9 #20) so the real lang row is not masked once backfilled.
	if lang != "en" {
		// Check the cache under the ENGLISH key first (#5): a prior English request
		// (or English fallback) may already hold the master, so serve it without a
		// second DB round-trip. Same source tag + no override as the DB path below.
		if enCached, ok := s.cache.Get(preciseName + "|en"); ok {
			return enCached, SourceSupabaseFallbackEn, nil
		}
		enRow, err := s.db.Lookup(ctx, normalized, "en")
		if err != nil {
			return nil, "", err
		}
		if enRow != nil {
			enRow = s.applyKingdom(enRow, name, nil) // stored kingdom only (no iNat on non-en)
			s.cache.Set(preciseName+"|en", enRow)
			return enRow, SourceSupabaseFallbackEn, nil
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
	// detail + source; overrideINat is applied per-caller AFTER Do so each caller
	// gets its own (copied) iNat-refreshed English name.
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
		if iNatName != "" { // only non-empty for English (gated above)
			hint = iNatName
		}

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
				// The copy inherits the master's kingdom; fold in iNat (en only)
				// and hard-filter before it is persisted.
				translated = s.applyKingdom(translated, name, iNatKingdom)
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
						row = s.applyKingdom(row, name, iNatKingdom)
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
		// the master in `lang`.
		//
		// Non-English requests skipped the iNat lookup above (its common name is
		// English-only), but the KINGDOM is language-independent and must be baked
		// into the master. Fetch it CONCURRENTLY with the multi-second LLM call so
		// it adds no latency; bounded by its own short timeout and best-effort — a
		// slow / failed iNat just leaves the model's self-report as the only source.
		var kingdomCh chan *string
		if !iNatAsked && s.inat != nil {
			kingdomCh = make(chan *string, 1) // buffered: never blocks if we bail out
			go func() {
				kctx, kcancel := context.WithTimeout(ctx, inatKingdomTimeout)
				defer kcancel()
				var k *string
				if t, ok := s.inat.LookupTaxon(kctx, name); ok {
					k = proxy.NormalizeKingdom(t.Kingdom)
				}
				kingdomCh <- k
			}()
		}

		generated, requestID, genErr := s.llm.Generate(ctx, name, hint, lang)
		if genErr != nil {
			return nil, genErr
		}
		if kingdomCh != nil {
			iNatKingdom = <-kingdomCh // returns within inatKingdomTimeout at worst
		}

		// The model's self-report is trusted ONLY when it says Fungi. A bare
		// "Plantae" from the LLM is not evidence: if iNat timed out / failed, a
		// mushroom the model mislabelled would be persisted as Plantae forever (the
		// kingdom backfill only selects rows with NO kingdom). Dropping it stores
		// null instead, which the backfill — or the next English read — can still
		// fix. Invariant: a STORED Plantae is always iNat-confirmed.
		if !proxy.IsFungi(generated.Kingdom) {
			generated.Kingdom = nil
		}
		// kingdom = iNat (authoritative) ⊕ the model's Fungi self-report, Fungi-wins;
		// a fungal master is hard-filtered BEFORE persistence (SPEC §7 kingdom).
		generated = s.applyKingdom(generated, name, iNatKingdom)

		// Whitelist common_diseases_list against the catalog (SPEC §1.1 + §7).
		generated.CommonDiseasesList = s.filterCatalogDiseaseIDs(generated.CommonDiseasesList)

		// Patch the English master with the iNat name (the LLM may ignore the
		// hint). Non-English masters keep their localized common_name (iNatName == "").
		if iNatName != "" {
			generated.CommonName = iNatName
			generated.CommonNameSource = "inaturalist"
		}

		// Step 6: INSERT ON CONFLICT (normalized, lang) DO NOTHING. On conflict,
		// re-Lookup to pick up the master another concurrent caller just wrote — and
		// let them own the backfill (SPEC §2.1 step 5).
		inserted, insErr := s.db.Insert(ctx, InsertParams{
			Normalized:      normalized,
			Lang:            lang,
			ScientificName:  name,
			CommonName:      hint, // iNat-resolved hint when iNat hit (en), else upstream
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
				row = s.applyKingdom(row, name, iNatKingdom)
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
	return overrideINat(res.detail, res.source), res.source, nil
}

// inatKingdomTimeout caps the kingdom-only iNat lookup that runs alongside a
// non-English master generation (the client's own timeout is 8 s). It is waited
// on only after the LLM call returns, so in practice it never extends the
// request; the cap bounds the worst case when the LLM answers unusually fast.
const inatKingdomTimeout = 4 * time.Second

// applyKingdom is finalizeKingdom bound to this Service's ContentIndex.
func (s *Service) applyKingdom(d *proxy.PlantDetail, scientificName string, authoritative *string) *proxy.PlantDetail {
	return finalizeKingdom(s.content, d, scientificName, authoritative)
}

// finalizeKingdom stamps d's biological kingdom and enforces the mushroom-safety
// hard filter (SPEC §7 kingdom). EVERY path that returns, caches or persists a
// path-2/3 row goes through it: the request path (Service.applyKingdom) and the
// async translation Backfiller (which also serves the Sweeper).
//
// The kingdom is the Fungi-wins merge of d's own value (a stored row's, or a
// fresh master's Fungi self-report), `authoritative` (iNat; nil when it was not
// asked or did not resolve) and what this process already learned about the
// species (content.KingdomFor — e.g. an earlier English request resolved it via
// iNat, so a legacy row translated later still gets it). When the result is
// Fungi, every edible/culinary use and the "edible" attribute are stripped
// (proxy.SanitizeFungiDetail) — on fresh masters and translations before they
// are persisted, AND on rows read back from Supabase, so a legacy row is safe to
// serve without waiting for the backfill. A resolved kingdom is noted back on
// the ContentIndex so /v1/identify + /v1/diagnose can reuse it with no I/O.
//
// content may be nil. Returns d itself when nothing changes, otherwise a
// modified COPY (d may be a pointer shared with the cache or a test stub — never
// mutated).
func finalizeKingdom(content *proxy.ContentIndex, d *proxy.PlantDetail, scientificName string, authoritative *string) *proxy.PlantDetail {
	if d == nil {
		return nil
	}
	out := *d
	out.Kingdom = proxy.MergeKingdom(d.Kingdom, authoritative, content.KingdomFor(scientificName))
	sanitized := proxy.SanitizeFungiDetail(&out)
	if out.Kingdom != nil {
		content.NoteKingdom(scientificName, out.Kingdom)
	}
	if !sanitized && sameKingdom(d.Kingdom, out.Kingdom) {
		return d
	}
	return &out
}

// sameKingdom reports whether two kingdom pointers hold the same value.
func sameKingdom(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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
// source-language regions verbatim. It sorts before 'v5' — the version that
// introduced localized native_region — keeping the row selectable by
// ListNativeRegionBackfillRows (source_version < 'v5') so the one-shot
// native_region backfill retries it; otherwise un-localized regions would escape
// re-selection forever under the current PromptVersion stamp (v5 or later).
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
