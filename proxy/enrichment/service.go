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
	SourceSupabaseMissGenerate           = "supabase_miss_generate"
	SourceSupabaseMissGenerateRaceWinner = "supabase_miss_generate_race_winner"
)

// Request bundles the validated handler-layer inputs.
type Request struct {
	ScientificName string // required; trimmed length 1..200; contains at least one letter
	CommonName     string // optional; passed to the LLM prompt as context
	PlantIDHint    string // optional, ignored in V1 (server re-derives via ContentIndex)
}

// ServiceDB is the small interface Service needs from the DB layer.
// *DB satisfies it; tests substitute a stub.
type ServiceDB interface {
	Lookup(ctx context.Context, normalized string) (*proxy.PlantDetail, error)
	Insert(ctx context.Context, p InsertParams) (bool, error)
}

// ServiceLLM is the small interface Service needs from the LLM layer.
// *LLMClient satisfies it; tests substitute a stub.
type ServiceLLM interface {
	Generate(ctx context.Context, scientificName, commonName string) (*proxy.PlantDetail, string, error)
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
	diseaseIDs map[string]struct{} // for common_diseases_list whitelist
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

// GetOrGenerate runs the three-tier lookup per SPEC §2.1.
//
// Order: cache -> embedded catalog -> Supabase plants_pending -> OpenAI LLM
// (with INSERT ON CONFLICT DO NOTHING + re-Lookup on race). The cache is
// written on every successful path so subsequent calls skip lower tiers.
//
// Returns the result plus a Source* string identifying which lookup tier
// produced it (SPEC §9 #10 forensic logging). On error the source is "".
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
	// Cache key is the PRECISE (infraspecific-preserving) normalization, NOT the
	// species-level Supabase PK `normalized`. The catalog (Step 1) resolves
	// distinct plantIds for sibling varieties via scientificNameToIDPrecise — the
	// five Brassica oleracea cultivars all fold to "brassica oleracea" under
	// NormalizeScientificName but to distinct keys here — so a species-level cache
	// key would alias one variety's *PlantDetail onto its siblings within the TTL.
	// The cache must be at least as fine-grained as the catalog's precise index.
	// Supabase Lookup/Insert below keep using `normalized` (SPEC §8 + §9 #1 —
	// intentional cross-variety row sharing at the species-level PK).
	cacheKey := proxy.NormalizeScientificNamePrecise(name)

	// iNat preferred_common_name lookup (best-effort; never blocks). Used
	// BOTH as the LLM hint (so a newly-generated row writes the iNat name)
	// AND as a post-return override of CommonName / CommonNameSource on
	// cache / DB / LLM results — but NOT on catalog hits, which keep their
	// curated common_name (PR #24 priority: catalog > iNat > upstream).
	iNatName := ""
	if s.inat != nil {
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

	// Step 1: embedded 1522 catalog. ContentIndex.LookupPlantID re-normalizes
	// internally; we pass the trimmed user form.
	if s.content != nil {
		if plantID, ok := s.content.LookupPlantID(name); ok {
			if full, ok := s.content.LookupFullDetail(plantID); ok {
				// NOTE: do NOT cache catalog hits. The LRU has no source tag,
				// so a SourceCatalog write would come back as SourceCache on
				// the next call and become eligible for the iNat override —
				// silently violating the catalog > iNat > upstream priority
				// (PR #26 self-review P0). Catalog is already an O(1)
				// in-memory map lookup, the LRU saved nothing here.
				return full, SourceCatalog, nil
			}
			// Index inconsistency (LookupPlantID hit but LookupFullDetail miss).
			// Fall through; treat as miss rather than crash.
		}
	}

	// Step 2: Supabase plants_pending.
	if s.db == nil {
		// No DB configured AND not in the catalog -> enrichment unavailable.
		return nil, "", ErrEnrichmentUnavailable
	}
	row, err := s.db.Lookup(ctx, normalized)
	if err != nil {
		return nil, "", err
	}
	if row != nil {
		s.cache.Set(cacheKey, row)
		return overrideINat(row, SourceSupabaseHit), SourceSupabaseHit, nil
	}

	// Step 3: LLM generation.
	if s.llm == nil {
		return nil, "", ErrEnrichmentUnavailable
	}
	// LLM hint: iNat name overrides the upstream hint when present so the
	// LLM ideally returns the iNat name directly (and the persisted row has it).
	hint := req.CommonName
	if iNatName != "" {
		hint = iNatName
	}
	generated, requestID, err := s.llm.Generate(ctx, name, hint)
	if err != nil {
		return nil, "", err
	}

	// Whitelist common_diseases_list against the catalog (SPEC §1.1 + §7).
	generated.CommonDiseasesList = s.filterCatalogDiseaseIDs(generated.CommonDiseasesList)

	// Patch generated with iNat name too — the LLM may ignore the hint, and
	// we want the persisted row + cache + response to all surface iNat.
	if iNatName != "" {
		generated.CommonName = iNatName
		generated.CommonNameSource = "inaturalist"
	}

	// Step 4: INSERT ON CONFLICT DO NOTHING. On conflict, re-Lookup to pick
	// up the row another concurrent caller just wrote (SPEC §2.1 step 5).
	inserted, err := s.db.Insert(ctx, InsertParams{
		Normalized:      normalized,
		ScientificName:  name,
		CommonName:      hint, // iNat-resolved hint when iNat hit, else upstream
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
		// row is now available — return that to keep all callers consistent.
		if row, lookupErr := s.db.Lookup(ctx, normalized); lookupErr == nil && row != nil {
			s.cache.Set(cacheKey, row)
			return overrideINat(row, SourceSupabaseMissGenerateRaceWinner), SourceSupabaseMissGenerateRaceWinner, nil
		}
		// Race re-Lookup also failed — return our generated copy. Same shape,
		// just a different LLM sample.
		s.cache.Set(cacheKey, generated)
		return generated, SourceSupabaseMissGenerate, nil
	}

	s.cache.Set(cacheKey, generated)
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
