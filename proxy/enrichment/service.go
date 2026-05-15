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
	diseaseIDs map[string]struct{} // for common_diseases_list whitelist
}

// NewService builds a Service with the given dependencies. content may not
// be nil in production (path-1 catalog hit relies on it); db + llm + cache
// may legitimately be nil during partial-degradation tests.
//
// Computes the catalog disease ID set once for fast whitelisting of
// LLM-generated common_diseases_list (SPEC §1.1 + §7 whitelist decision).
func NewService(content *proxy.ContentIndex, db ServiceDB, llm ServiceLLM, cache *Cache) *Service {
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
		diseaseIDs: diseaseIDs,
	}
}

// GetOrGenerate runs the three-tier lookup per SPEC §2.1.
//
// Order: cache -> embedded catalog -> Supabase plants_pending -> OpenAI LLM
// (with INSERT ON CONFLICT DO NOTHING + re-Lookup on race). The cache is
// written on every successful path so subsequent calls skip lower tiers.
func (s *Service) GetOrGenerate(ctx context.Context, req Request) (*proxy.PlantDetail, error) {
	name := strings.TrimSpace(req.ScientificName)
	if name == "" || !hasLetter(name) {
		return nil, ErrInvalidScientificName
	}
	if len(name) > maxScientificNameLen {
		return nil, ErrScientificNameTooLong
	}
	normalized := proxy.NormalizeScientificName(name)
	if normalized == "" {
		return nil, ErrInvalidScientificName
	}

	// Step 0: in-process LRU cache.
	if cached, ok := s.cache.Get(normalized); ok {
		return cached, nil
	}

	// Step 1: embedded 1522 catalog. ContentIndex.LookupPlantID re-normalizes
	// internally; we pass the trimmed user form.
	if s.content != nil {
		if plantID, ok := s.content.LookupPlantID(name); ok {
			if full, ok := s.content.LookupFullDetail(plantID); ok {
				s.cache.Set(normalized, full)
				return full, nil
			}
			// Index inconsistency (LookupPlantID hit but LookupFullDetail miss).
			// Fall through; treat as miss rather than crash.
		}
	}

	// Step 2: Supabase plants_pending.
	if s.db == nil {
		// No DB configured AND not in the catalog -> enrichment unavailable.
		return nil, ErrEnrichmentUnavailable
	}
	row, err := s.db.Lookup(ctx, normalized)
	if err != nil {
		return nil, err
	}
	if row != nil {
		s.cache.Set(normalized, row)
		return row, nil
	}

	// Step 3: LLM generation.
	if s.llm == nil {
		return nil, ErrEnrichmentUnavailable
	}
	generated, requestID, err := s.llm.Generate(ctx, name, req.CommonName)
	if err != nil {
		return nil, err
	}

	// Whitelist common_diseases_list against the catalog (SPEC §1.1 + §7).
	generated.CommonDiseasesList = s.filterCatalogDiseaseIDs(generated.CommonDiseasesList)

	// Step 4: INSERT ON CONFLICT DO NOTHING. On conflict, re-Lookup to pick
	// up the row another concurrent caller just wrote (SPEC §2.1 step 5).
	inserted, err := s.db.Insert(ctx, InsertParams{
		Normalized:      normalized,
		ScientificName:  name,
		CommonName:      req.CommonName,
		Data:            generated,
		Source:          SourceTag,
		SourceVersion:   PromptVersion,
		GenerationReqID: requestID,
	})
	if err != nil {
		return nil, err
	}

	if !inserted {
		// Concurrent race resolved by ON CONFLICT. The conflicting writer's
		// row is now available — return that to keep all callers consistent.
		if row, lookupErr := s.db.Lookup(ctx, normalized); lookupErr == nil && row != nil {
			s.cache.Set(normalized, row)
			return row, nil
		}
		// Race re-Lookup also failed — return our generated copy. Same shape,
		// just a different LLM sample.
		s.cache.Set(normalized, generated)
		return generated, nil
	}

	s.cache.Set(normalized, generated)
	return generated, nil
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
