package enrichment

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/yaochen1125/yardmate-api/proxy"
)

var (
	ErrDiseaseEnrichmentUnavailable = errors.New("enrichment: disease enrichment unavailable")
	ErrInvalidDiseaseName           = errors.New("enrichment: invalid disease name")
)

// DiseaseDB is the persistence surface DiseaseService needs (satisfied by *DB).
type DiseaseDB interface {
	LookupDisease(ctx context.Context, normalized string) (*proxy.StructuredDiseaseDetail, string, error)
	InsertDisease(ctx context.Context, p DiseaseInsertParams) (string, bool, error)
}

// DiseaseLLM is the generation surface (satisfied by *DiseaseLLMClient).
type DiseaseLLM interface {
	Generate(ctx context.Context, diseaseName, plantContext string, stepRefs, remedyRefs []proxy.DiseaseNameRef) (*diseaseGenResult, string, error)
}

// DiseaseService implements proxy.DiseaseEnricher: cache → Supabase → LLM
// generate → back-fill → insert (mint O id) → race re-lookup. All collaborators
// are nil-safe for testing. See proxy/enrichment/SPEC_disease.md.
type DiseaseService struct {
	content *proxy.ContentIndex
	db      DiseaseDB
	llm     DiseaseLLM
	cache   *DiseaseCache
}

func NewDiseaseService(content *proxy.ContentIndex, db DiseaseDB, llm DiseaseLLM, cache *DiseaseCache) *DiseaseService {
	return &DiseaseService{content: content, db: db, llm: llm, cache: cache}
}

// Compile-time assertions: DiseaseService satisfies the proxy injection point,
// and the concrete DB / LLM clients satisfy the service's collaborators.
var (
	_ proxy.DiseaseEnricher = (*DiseaseService)(nil)
	_ DiseaseDB             = (*DB)(nil)
	_ DiseaseLLM            = (*DiseaseLLMClient)(nil)
)

// GetOrGenerate implements proxy.DiseaseEnricher. The caller (HandleDiagnose)
// treats ANY error as "keep the issue slim" and never 502s — disease enrichment
// is a tail enhancement (SPEC_disease §6).
func (s *DiseaseService) GetOrGenerate(ctx context.Context, diseaseName, plantContext string) (*proxy.StructuredDiseaseDetail, string, error) {
	if s == nil || s.content == nil {
		return nil, "", ErrDiseaseEnrichmentUnavailable
	}
	name := strings.TrimSpace(diseaseName)
	if name == "" {
		return nil, "", ErrInvalidDiseaseName
	}
	key := proxy.NormalizeDiseaseName(name)
	if key == "" {
		return nil, "", ErrInvalidDiseaseName
	}

	// Step 0: in-process cache.
	if cached, ok := s.cache.Get(key); ok {
		return cached.Detail, cached.CatalogID, nil
	}

	// Step 1: Supabase. A DB error does NOT abort (SPEC §6) — fall through to
	// generate (uncached). A hit returns immediately.
	if s.db != nil {
		if detail, catalogID, err := s.db.LookupDisease(ctx, key); err != nil {
			log.Printf("disease enrich lookup err: name=%q err=%v", name, err)
		} else if detail != nil {
			s.cache.Set(key, cachedDisease{Detail: detail, CatalogID: catalogID})
			return detail, catalogID, nil
		}
	}

	// Step 2: generate (ids only).
	if s.llm == nil {
		return nil, "", ErrDiseaseEnrichmentUnavailable
	}
	gen, reqID, err := s.llm.Generate(ctx, name, plantContext, s.content.AllStepRefs(), s.content.AllRemedyRefs())
	if err != nil {
		return nil, "", err
	}
	detail := s.backfill(gen)

	// Step 3: persist + mint O id (ON CONFLICT race-safe). A DB failure still
	// returns the generated detail (uncached, empty catalog id → caller emits
	// it with catalogId=null + structuredDetail) — SPEC §6, never 502.
	if s.db != nil {
		catalogID, inserted, err := s.db.InsertDisease(ctx, DiseaseInsertParams{
			Normalized:      key,
			DiseaseName:     name,
			Detail:          detail,
			Source:          DiseaseSourceTag,
			SourceVersion:   DiseasePromptVersion,
			GenerationReqID: reqID,
		})
		if err != nil {
			log.Printf("disease enrich insert err: name=%q err=%v", name, err)
			return detail, "", nil
		}
		if !inserted {
			// Race loser: return the winner so every caller agrees on one O id.
			if wd, wid, lookupErr := s.db.LookupDisease(ctx, key); lookupErr == nil && wd != nil {
				s.cache.Set(key, cachedDisease{Detail: wd, CatalogID: wid})
				return wd, wid, nil
			}
			return detail, "", nil
		}
		s.cache.Set(key, cachedDisease{Detail: detail, CatalogID: catalogID})
		return detail, catalogID, nil
	}

	// No DB configured: return generated (no O id, not reusable).
	return detail, "", nil
}

// backfill turns the LLM's id-only output into a full StructuredDiseaseDetail by
// resolving each ref from the shared step/remedy pools (denormalized, same copy +
// image as in-catalog diseases). Unknown ids are dropped (defense beyond the
// schema enum); a group emptied by dropping is dropped.
func (s *DiseaseService) backfill(gen *diseaseGenResult) *proxy.StructuredDiseaseDetail {
	return &proxy.StructuredDiseaseDetail{
		ShortDescription: gen.ShortDescription,
		SymptomAnalysis:  gen.SymptomAnalysis,
		Cause:            gen.Cause,
		Treatment:        s.backfillGroups(gen.Treatment),
		Prevention:       s.backfillGroups(gen.Prevention),
		HomeRemedies:     s.backfillRemedies(gen.HomeRemedyRefs),
	}
}

func (s *DiseaseService) backfillGroups(g diseaseGenGroups) proxy.DiseaseStepGroups {
	out := proxy.DiseaseStepGroups{Groups: make([]proxy.DiseaseStepGroup, 0, len(g.Groups))}
	for _, grp := range g.Groups {
		steps := make([]proxy.DiseaseStep, 0, len(grp.StepRefs))
		num := 1
		for _, ref := range grp.StepRefs {
			st, ok := s.content.StepByID(ref)
			if !ok || st == nil {
				continue
			}
			steps = append(steps, proxy.DiseaseStep{
				Num:      num,
				Title:    st.Title,
				Body:     st.Body,
				Image:    st.Image,
				Ref:      st.ID,
				SubSteps: []proxy.DiseaseStep{},
			})
			num++
		}
		if len(steps) == 0 {
			continue
		}
		var label *string
		if l := strings.TrimSpace(grp.Label); l != "" {
			label = &l
		}
		out.Groups = append(out.Groups, proxy.DiseaseStepGroup{Label: label, Steps: steps})
	}
	return out
}

func (s *DiseaseService) backfillRemedies(refs []string) []proxy.DiseaseRemedy {
	out := make([]proxy.DiseaseRemedy, 0, len(refs))
	for _, ref := range refs {
		r, ok := s.content.RemedyByID(ref)
		if !ok || r == nil {
			continue
		}
		out = append(out, proxy.DiseaseRemedy{
			Ref:    r.ID,
			Title:  r.Title,
			Recipe: r.Recipe,
			Usage:  r.Usage,
			Image:  r.Image,
		})
	}
	return out
}

// CacheLen exposes the disease cache size for diagnose-side logging.
func (s *DiseaseService) CacheLen() int {
	if s == nil {
		return 0
	}
	return s.cache.Len()
}
