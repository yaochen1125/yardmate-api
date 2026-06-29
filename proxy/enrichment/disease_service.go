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
	LookupDisease(ctx context.Context, normalized, lang string) (*proxy.StructuredDiseaseDetail, string, error)
	LookupDiseaseAny(ctx context.Context, normalized string) (*proxy.StructuredDiseaseDetail, string, string, error)
	InsertDisease(ctx context.Context, p DiseaseInsertParams) (string, bool, error)
}

// DiseaseLLM is the generation surface (satisfied by *DiseaseLLMClient).
type DiseaseLLM interface {
	Generate(ctx context.Context, diseaseName, plantContext, lang string, stepRefs, remedyRefs []proxy.DiseaseNameRef) (*diseaseGenResult, string, error)
	DiseaseTranslate(ctx context.Context, source *proxy.StructuredDiseaseDetail, toLang string) (*proxy.StructuredDiseaseDetail, string, error)
}

// DiseaseService implements proxy.DiseaseEnricher: cache → Supabase (exact lang)
// → Supabase English fallback → race-window translate → LLM generate → back-fill
// → insert (mint/share O id) → race re-lookup → async translation backfill. All
// collaborators are nil-safe for testing. See proxy/enrichment/SPEC_disease.md.
type DiseaseService struct {
	content    *proxy.ContentIndex
	db         DiseaseDB
	llm        DiseaseLLM
	cache      *DiseaseCache
	backfiller *DiseaseBackfiller // optional; nil → no async translation backfill (tests / DB-less mode)
}

func NewDiseaseService(content *proxy.ContentIndex, db DiseaseDB, llm DiseaseLLM, cache *DiseaseCache) *DiseaseService {
	return &DiseaseService{content: content, db: db, llm: llm, cache: cache}
}

// SetBackfiller attaches the async translation backfiller (SPEC §7). Wired by
// main after the service + DB + LLM exist; nil-safe so tests can skip it
// (mirrors Service.SetBackfiller on the plant side).
func (s *DiseaseService) SetBackfiller(b *DiseaseBackfiller) {
	if s != nil {
		s.backfiller = b
	}
}

// Backfiller returns the attached async backfiller (nil if none / nil service).
// Used by main to hand the worker pool to the periodic Sweeper (mirrors
// Service.Backfiller on the plant side).
func (s *DiseaseService) Backfiller() *DiseaseBackfiller {
	if s == nil {
		return nil
	}
	return s.backfiller
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
//
// Order (mirrors the plant side's Service.GetOrGenerate): cache → Supabase
// (exact lang) → Supabase English fallback → race-window translate of an
// existing other-language master → LLM master generation (back-fill + INSERT ON
// CONFLICT + race re-Lookup) → async translation backfill. The cache + Supabase
// PK are keyed by (normalized disease name, lang).
func (s *DiseaseService) GetOrGenerate(ctx context.Context, diseaseName, plantContext, lang string) (*proxy.StructuredDiseaseDetail, string, error) {
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
	lang = NormalizeLang(lang)
	cacheKey := key + "|" + lang

	// Step 0: in-process cache (keyed by name+lang so two languages of the same
	// disease never alias one another within the TTL).
	if cached, ok := s.cache.Get(cacheKey); ok {
		return cached.Detail, cached.CatalogID, nil
	}

	// Step 1: Supabase, exact (name, lang). A DB error does NOT abort (SPEC §6) —
	// fall through to generate (uncached). A hit returns immediately.
	if s.db != nil {
		if detail, catalogID, err := s.db.LookupDisease(ctx, key, lang); err != nil {
			log.Printf("disease enrich lookup err: name=%q lang=%s err=%v", name, lang, err)
		} else if detail != nil {
			s.cache.Set(cacheKey, cachedDisease{Detail: detail, CatalogID: catalogID})
			return detail, catalogID, nil
		}

		// Step 2: display fallback — exact lang missing, serve the English row if
		// it exists (SPEC §7). Cache under the ENGLISH key only, never the
		// requested lang key, so the real lang row is not masked once backfilled.
		if lang != "en" {
			if enDetail, enID, err := s.db.LookupDisease(ctx, key, "en"); err != nil {
				log.Printf("disease enrich en-fallback lookup err: name=%q err=%v", name, err)
			} else if enDetail != nil {
				s.cache.Set(key+"|en", cachedDisease{Detail: enDetail, CatalogID: enID})
				return enDetail, enID, nil
			}
		}
	}

	if s.llm == nil {
		return nil, "", ErrDiseaseEnrichmentUnavailable
	}

	// Step 3: race window — before generating a fresh master, check whether a
	// master already exists in ANOTHER language (neither the exact lang nor
	// English is present). If so, translate that master into `lang` and REUSE its
	// O id rather than generating a second independent master (the §7 one-master
	// invariant). Only a true first-caller (no row in ANY language) generates.
	if s.db != nil {
		if existing, existingID, _, lookErr := s.db.LookupDiseaseAny(ctx, key); lookErr != nil {
			log.Printf("disease enrich lookup-any err: name=%q err=%v", name, lookErr)
		} else if existing != nil {
			if translated, reqID, tErr := s.llm.DiseaseTranslate(ctx, existing, lang); tErr == nil && translated != nil {
				newID, inserted, insErr := s.db.InsertDisease(ctx, DiseaseInsertParams{
					Normalized:      key,
					Lang:            lang,
					CatalogID:       existingID, // share the master's O id
					DiseaseName:     name,
					Detail:          translated,
					Source:          DiseaseTranslatedSourceTag,
					SourceVersion:   DiseasePromptVersion,
					GenerationReqID: reqID,
				})
				if insErr != nil {
					log.Printf("disease enrich translate-insert err: name=%q lang=%s err=%v", name, lang, insErr)
					return translated, existingID, nil
				}
				if !inserted {
					// Another caller wrote this lang first — return their row.
					if wd, wid, _ := s.db.LookupDisease(ctx, key, lang); wd != nil {
						s.cache.Set(cacheKey, cachedDisease{Detail: wd, CatalogID: wid})
						return wd, wid, nil
					}
					return translated, existingID, nil
				}
				s.cache.Set(cacheKey, cachedDisease{Detail: translated, CatalogID: newID})
				return translated, newID, nil
			}
			// Translation failed (rare) → fall through to generate a master in `lang`.
		}
	}

	// Step 4: true first-caller (or translation-failure fallback) — generate the
	// master in `lang` (ids only) + back-fill.
	gen, reqID, err := s.llm.Generate(ctx, name, plantContext, lang, s.content.AllStepRefs(), s.content.AllRemedyRefs())
	if err != nil {
		return nil, "", err
	}
	detail := s.backfill(gen)

	// Step 5: persist + mint O id (ON CONFLICT race-safe). A DB failure still
	// returns the generated detail (uncached, empty catalog id → caller emits
	// it with catalogId=null + structuredDetail) — SPEC §6, never 502.
	if s.db != nil {
		catalogID, inserted, err := s.db.InsertDisease(ctx, DiseaseInsertParams{
			Normalized:      key,
			Lang:            lang,
			DiseaseName:     name,
			Detail:          detail,
			Source:          DiseaseSourceTag,
			SourceVersion:   DiseasePromptVersion,
			GenerationReqID: reqID,
		})
		if err != nil {
			log.Printf("disease enrich insert err: name=%q lang=%s err=%v", name, lang, err)
			return detail, "", nil
		}
		if !inserted {
			// Race loser: return the winner so every caller agrees on one O id.
			if wd, wid, lookupErr := s.db.LookupDisease(ctx, key, lang); lookupErr == nil && wd != nil {
				s.cache.Set(cacheKey, cachedDisease{Detail: wd, CatalogID: wid})
				return wd, wid, nil
			}
			return detail, "", nil
		}
		s.cache.Set(cacheKey, cachedDisease{Detail: detail, CatalogID: catalogID})

		// Step 6: async backfill the other languages from this master (English
		// first), translate-only-prose, ON CONFLICT DO NOTHING, reusing this O id.
		if s.backfiller != nil {
			s.backfiller.Enqueue(DiseaseBackfillJob{
				Normalized:  key,
				DiseaseName: name,
				CatalogID:   catalogID,
				SourceLang:  lang,
				Master:      detail,
			})
		}
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
		// Severity is language-independent; whitelist to the known enum (defense
		// beyond the schema, same posture as dropping unknown step ids) so a stray
		// value never reaches the wire. nil when ungrouped / not severity-specific.
		var severity *string
		if sv := strings.TrimSpace(grp.Severity); sv == "mild" || sv == "severe" {
			severity = &sv
		}
		out.Groups = append(out.Groups, proxy.DiseaseStepGroup{Label: label, Severity: severity, Steps: steps})
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
