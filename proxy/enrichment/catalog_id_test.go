package enrichment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A catalog id sent as the scientific name (2026-10-21: an app with a stale
// local index sent "AAA1758") is refused before ANY tier: no cache entry, no
// Supabase lookup or insert, no model call — so nothing like "Unknown Plant"
// can reach plants_pending.
func TestService_CatalogIDAsScientificName_Refused(t *testing.T) {
	for _, name := range []string{"AAA1758", "aaa1758", "  AAA1758 ", "AAA0001", "AAA10000"} {
		t.Run(name, func(t *testing.T) {
			cache := NewCache(10, time.Hour)
			db := &stubDB{}
			llm := &stubLLM{}
			svc := NewService(loadTestContent(t), db, llm, cache, nil)
			got, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: name, Lang: "ja"})
			if !errors.Is(err, ErrCatalogIDAsScientificName) {
				t.Fatalf("err = %v, want ErrCatalogIDAsScientificName", err)
			}
			if got != nil || source != "" {
				t.Errorf("got (%v,%q), want (nil,\"\")", got, source)
			}
			if len(db.lookupCalls) != 0 || len(db.insertCalls) != 0 || db.lookupAnyHits != 0 {
				t.Errorf("DB touched: lookups=%v inserts=%d lookupAny=%d", db.lookupCalls, len(db.insertCalls), db.lookupAnyHits)
			}
			if len(llm.calls) != 0 || len(llm.translateCalls) != 0 {
				t.Errorf("model called: generate=%d translate=%d", len(llm.calls), len(llm.translateCalls))
			}
			if cache.Len() != 0 {
				t.Errorf("cache len = %d, want 0", cache.Len())
			}
		})
	}
}

// Real names that only resemble an id are not refused.
func TestService_CatalogIDGuard_NamesPass(t *testing.T) {
	llm := &stubLLM{err: ErrEnrichmentUnavailable}
	svc := NewService(loadTestContent(t), &stubDB{}, llm, NewCache(10, time.Hour), nil)
	for _, name := range []string{"Madeup notreal", "AAA", "Abc 1758"} {
		if _, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: name}); errors.Is(err, ErrCatalogIDAsScientificName) {
			t.Errorf("%q refused as a catalog id", name)
		}
	}
}

// "Petunia species" lands on the catalog genus entry AAA1591 ("Petunia spp.")
// through LookupPlantID tier 3 — catalog path, no DB, no model.
func TestService_PlaceholderEpithet_CatalogGenusHit(t *testing.T) {
	for _, name := range []string{"Petunia species", "petunia sp.", "Petunia spp"} {
		t.Run(name, func(t *testing.T) {
			db := &stubDB{}
			llm := &stubLLM{}
			svc := NewService(loadTestContent(t), db, llm, NewCache(10, time.Hour), nil)
			got, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: name})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if source != SourceCatalog || got == nil || got.PlantDetailID() != "AAA1591" {
				t.Fatalf("got (%v,%q), want catalog AAA1591", got, source)
			}
			if len(db.lookupCalls) != 0 || len(llm.calls) != 0 {
				t.Errorf("DB/model touched on a catalog hit: lookups=%v llm=%d", db.lookupCalls, len(llm.calls))
			}
		})
	}
}

// No genus-level Calathea entry → unchanged: falls through to Supabase under the
// name's own (unchanged) PK.
func TestService_PlaceholderEpithet_NoGenusEntry_Unchanged(t *testing.T) {
	db := &stubDB{}
	llm := &stubLLM{err: ErrEnrichmentUnavailable}
	svc := NewService(loadTestContent(t), db, llm, NewCache(10, time.Hour), nil)
	_, source, _ := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Calathea species"})
	if source == SourceCatalog {
		t.Fatal("Calathea species hit the catalog without a genus entry")
	}
	if len(db.lookupCalls) == 0 || db.lookupCalls[0] != "calathea species" {
		t.Errorf("lookupCalls = %v, want first lookup on PK %q", db.lookupCalls, "calathea species")
	}
}

func TestHandler_CatalogIDAsScientificName_404(t *testing.T) {
	llm := &stubLLM{}
	db := &stubDB{}
	h := newTestHandler(db, llm)
	r := makeRequest(t, `{"scientificName":"AAA1758","lang":"zh-Hans"}`, testUUID, "2.1.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "catalog_id_not_scientific_name") {
		t.Errorf("body = %q, want catalog_id_not_scientific_name", w.Body.String())
	}
	if len(llm.calls) != 0 || len(db.lookupCalls) != 0 || len(db.insertCalls) != 0 {
		t.Errorf("model/DB touched: llm=%d lookups=%v inserts=%d", len(llm.calls), db.lookupCalls, len(db.insertCalls))
	}
}
