package proxy

import (
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy/rosererank"
)

func agStr(s string) *string { return &s }

// agCands is a 3-entry candidate set (testRoseCands has only 2, too few to
// exercise the top-3 cap).
func agCands() []rosererank.RoseCandidate {
	return []rosererank.RoseCandidate{
		{PlantID: "AAA1", ScientificName: "Rosa 'Peace'", CommonName: "Peace"},
		{PlantID: "AAA2", ScientificName: "Rosa 'Blaze'", CommonName: "Blaze"},
		{PlantID: "AAA3", ScientificName: "Rosa 'Iceberg'", CommonName: "Iceberg"},
	}
}

func agSpecies(pid *string) Suggestion {
	return Suggestion{Name: "Rose", ScientificName: "Rosa chinensis", CommonNames: []string{"Rose"}, Confidence: 0.9, PlantID: pid}
}

// TestAppendRoseGuesses covers the handler-side TierGuess apply: the trickiest new
// logic (species-at-0, top-3 cap, nil-PlantID dedup guard, self-id dedup) that
// Decide's tests don't reach.
func TestAppendRoseGuesses(t *testing.T) {
	byID := roseByID(agCands())

	t.Run("keeps species at 0, appends guesses, caps to top-3", func(t *testing.T) {
		res := &IdentifyResult{Suggestions: []Suggestion{agSpecies(nil)}}
		matches := []rosererank.RoseMatch{
			{PlantID: "AAA1", Confidence: 0.4}, {PlantID: "AAA2", Confidence: 0.3}, {PlantID: "AAA3", Confidence: 0.2},
		}
		appendRoseGuesses(res, matches, byID)

		if len(res.Suggestions) != rosererank.MaxMatches {
			t.Fatalf("want %d suggestions (species + 2 guesses), got %d", rosererank.MaxMatches, len(res.Suggestions))
		}
		if res.Suggestions[0].Name != "Rose" || res.Suggestions[0].MatchKind != "" {
			t.Errorf("species must stay at index 0 unchanged, got %+v", res.Suggestions[0])
		}
		for _, i := range []int{1, 2} {
			if res.Suggestions[i].MatchKind != "cultivar_guess" {
				t.Errorf("suggestion[%d] match_kind = %q, want cultivar_guess", i, res.Suggestions[i].MatchKind)
			}
		}
		g := res.Suggestions[1]
		if g.Name != "Peace" || g.ScientificName != "Rosa 'Peace'" || g.PlantID == nil || *g.PlantID != "AAA1" || g.Confidence != 0.4 {
			t.Errorf("guess[1] fields wrong: %+v", g)
		}
		for _, s := range res.Suggestions {
			if s.PlantID != nil && *s.PlantID == "AAA3" {
				t.Errorf("AAA3 (4th slot) should have been dropped by the top-3 cap")
			}
		}
		if res.AIEnhancedAt == nil {
			t.Errorf("AIEnhancedAt must be set on apply")
		}
	})

	t.Run("nil species PlantID guard does not drop guesses", func(t *testing.T) {
		res := &IdentifyResult{Suggestions: []Suggestion{agSpecies(nil)}}
		matches := []rosererank.RoseMatch{{PlantID: "AAA1", Confidence: 0.4}, {PlantID: "AAA2", Confidence: 0.3}}
		appendRoseGuesses(res, matches, byID)
		if len(res.Suggestions) != 3 {
			t.Fatalf("nil-PlantID species must not false-drop guesses; want 3, got %d", len(res.Suggestions))
		}
	})

	t.Run("drops a guess that IS the species plant_id", func(t *testing.T) {
		res := &IdentifyResult{Suggestions: []Suggestion{agSpecies(agStr("AAA1"))}}
		matches := []rosererank.RoseMatch{{PlantID: "AAA1", Confidence: 0.4}, {PlantID: "AAA2", Confidence: 0.3}}
		appendRoseGuesses(res, matches, byID)
		if len(res.Suggestions) != 2 {
			t.Fatalf("AAA1 == species id must be dropped; want 2, got %d", len(res.Suggestions))
		}
		if res.Suggestions[1].PlantID == nil || *res.Suggestions[1].PlantID != "AAA2" {
			t.Errorf("remaining guess should be AAA2, got %+v", res.Suggestions[1])
		}
	})

	t.Run("empty matches is a no-op", func(t *testing.T) {
		res := &IdentifyResult{Suggestions: []Suggestion{agSpecies(nil)}}
		appendRoseGuesses(res, nil, byID)
		if len(res.Suggestions) != 1 || res.AIEnhancedAt != nil {
			t.Errorf("empty matches must not touch result: %+v aiEnhanced=%v", res.Suggestions, res.AIEnhancedAt)
		}
	})

	t.Run("all-unknown ids is a no-op", func(t *testing.T) {
		res := &IdentifyResult{Suggestions: []Suggestion{agSpecies(nil)}}
		appendRoseGuesses(res, []rosererank.RoseMatch{{PlantID: "ZZZ9", Confidence: 0.9}}, byID)
		if len(res.Suggestions) != 1 || res.AIEnhancedAt != nil {
			t.Errorf("all-unknown matches must not touch result: %+v aiEnhanced=%v", res.Suggestions, res.AIEnhancedAt)
		}
	})

	t.Run("empty suggestions is a no-op", func(t *testing.T) {
		res := &IdentifyResult{Suggestions: nil}
		appendRoseGuesses(res, []rosererank.RoseMatch{{PlantID: "AAA1", Confidence: 0.4}}, byID)
		if len(res.Suggestions) != 0 || res.AIEnhancedAt != nil {
			t.Errorf("no species suggestion must be a no-op: %+v", res.Suggestions)
		}
	})
}
