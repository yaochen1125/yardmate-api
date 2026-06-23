package enrichment

import (
	"context"
	"errors"
	"strings"
	"unicode"

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

// Source identifies which tier of GetOrGenerate's lookup produced the result.
// Returned alongside the *PlantDetail so handlers can log the path taken
// (SPEC §9 #10 forensic logging).
const (
	SourceCache                          = "cache"
	SourceCatalog                        = "catalog"
	SourceSupabaseHit                    = "supabase_hit"
	SourceSupabaseFallbackEn             = "supabase_fallback_en"           // requested lang missing, served English (§7)
	SourceSupabaseTranslatedOnDemand     = "supabase_translated_on_demand"  // requested lang missing + no English yet, translated an existing other-language master (§7)
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
}

// SetBackfiller attaches the async translation backfiller (SPEC §7). Wired by
// main after the Service + DB + LLM exist; nil-safe so tests can skip it.
func (s *Service) SetBackfiller(b *Backfiller) {
	if s != nil {
		s.backfill = b
	}
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
	// row would force English back in. Skip the lookup entirely for non-English.
	iNatName := ""
	if lang == "en" && s.inat != nil {
		if n, ok := s.inat.PreferredCommonName(ctx, name); ok {
			iNatName = n
		}
	}
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
	row, err := s.db.Lookup(ctx, normalized, lang)
	if err != nil {
		return nil, "", err
	}
	if row != nil {
		s.cache.Set(cacheKey, row)
		return overrideINat(row, SourceSupabaseHit), SourceSupabaseHit, nil
	}

	// Step 3: display fallback — exact lang missing, serve the English row if it
	// exists (SPEC §7). Cache it under the ENGLISH key only, NEVER the requested
	// lang key (SPEC §9 #20) so the real lang row is not masked once backfilled.
	if lang != "en" {
		enRow, err := s.db.Lookup(ctx, normalized, "en")
		if err != nil {
			return nil, "", err
		}
		if enRow != nil {
			s.cache.Set(preciseName+"|en", enRow)
			return enRow, SourceSupabaseFallbackEn, nil
		}
		// English also missing → fall through and generate the master in `lang`.
	}

	if s.llm == nil {
		return nil, "", ErrEnrichmentUnavailable
	}
	hint := req.CommonName
	if iNatName != "" { // only non-empty for English (gated above)
		hint = iNatName
	}

	// Step 4: before generating a fresh master, check whether a master already
	// exists for this plant in ANOTHER language — i.e. we are racing this plant's
	// backfill (neither the exact lang nor English is present yet). If so,
	// translate that master into `lang` rather than generating an independent
	// second master, which would let care facts diverge across language rows
	// (Codex P2 — the SPEC §7 one-master invariant). Only a true first-caller
	// (no row in ANY language) reaches the Generate call below. The remaining
	// concurrent-double-first-caller race (two langs generated at once) is the
	// pre-existing accepted §7 race.
	if existing, _, lookErr := s.db.LookupAny(ctx, normalized); lookErr != nil {
		return nil, "", lookErr
	} else if existing != nil {
		if translated, reqID, tErr := s.llm.Translate(ctx, existing, lang); tErr == nil && translated != nil {
			inserted, insErr := s.db.Insert(ctx, InsertParams{
				Normalized:      normalized,
				Lang:            lang,
				ScientificName:  name,
				CommonName:      hint,
				Data:            translated,
				Source:          TranslatedSourceTag,
				SourceVersion:   PromptVersion,
				GenerationReqID: reqID,
			})
			if insErr != nil {
				return nil, "", insErr
			}
			if !inserted {
				// Another caller wrote this lang first — return their row.
				if row, _ := s.db.Lookup(ctx, normalized, lang); row != nil {
					s.cache.Set(cacheKey, row)
					return overrideINat(row, SourceSupabaseHit), SourceSupabaseHit, nil
				}
			}
			s.cache.Set(cacheKey, translated)
			return overrideINat(translated, SourceSupabaseTranslatedOnDemand), SourceSupabaseTranslatedOnDemand, nil
		}
		// Translation failed (rare) → fall through to generate a master in `lang`.
		// This is the only path that can produce a second independent master, and
		// only on translation failure.
	}

	// Step 5: true first-caller (or translation-failure fallback) — generate the
	// master in `lang`.
	generated, requestID, err := s.llm.Generate(ctx, name, hint, lang)
	if err != nil {
		return nil, "", err
	}

	// Whitelist common_diseases_list against the catalog (SPEC §1.1 + §7).
	generated.CommonDiseasesList = s.filterCatalogDiseaseIDs(generated.CommonDiseasesList)

	// Patch the English master with the iNat name (the LLM may ignore the hint).
	// Non-English masters keep their localized LLM common_name (iNatName == "").
	if iNatName != "" {
		generated.CommonName = iNatName
		generated.CommonNameSource = "inaturalist"
	}

	// Step 6: INSERT ON CONFLICT (normalized, lang) DO NOTHING. On conflict,
	// re-Lookup to pick up the master another concurrent caller just wrote — and
	// let them own the backfill (SPEC §2.1 step 5).
	inserted, err := s.db.Insert(ctx, InsertParams{
		Normalized:      normalized,
		Lang:            lang,
		ScientificName:  name,
		CommonName:      hint, // iNat-resolved hint when iNat hit (en), else upstream
		Data:            generated,
		Source:          SourceTag,
		SourceVersion:   PromptVersion,
		GenerationReqID: requestID,
	})
	if err != nil {
		return nil, "", err
	}

	if !inserted {
		// Concurrent race resolved by ON CONFLICT. The conflicting writer's
		// master is now available — return that to keep all callers consistent.
		if row, lookupErr := s.db.Lookup(ctx, normalized, lang); lookupErr == nil && row != nil {
			s.cache.Set(cacheKey, row)
			return overrideINat(row, SourceSupabaseMissGenerateRaceWinner), SourceSupabaseMissGenerateRaceWinner, nil
		}
		// Race re-Lookup also failed — return our generated copy. Same shape,
		// just a different LLM sample. The conflicting writer owns the backfill.
		s.cache.Set(cacheKey, generated)
		return generated, SourceSupabaseMissGenerate, nil
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

	return generated, SourceSupabaseMissGenerate, nil
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
