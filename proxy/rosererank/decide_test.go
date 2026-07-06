package rosererank

import "testing"

func ids(list ...string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

func TestDecide(t *testing.T) {
	valid := ids("AAA1", "AAA2", "AAA3", "AAA4")
	tests := []struct {
		name     string
		res      RoseRerankResult
		wantTier Tier
		wantIDs  []string
	}{
		{
			name: "certain with strong first match -> certain (replace)",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.8}, {PlantID: "AAA2", Confidence: 0.4},
			}},
			wantTier: TierCertain, wantIDs: []string{"AAA1", "AAA2"},
		},
		{
			// NEW behaviour: not-certain no longer discards a plausible candidate —
			// it surfaces it as a "possibly" guess so 'Queen of Sweden' can reach
			// the user instead of collapsing to the bare genus.
			name: "not certain but strong candidate -> guess",
			res: RoseRerankResult{CultivarCertain: false, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.9},
			}},
			wantTier: TierGuess, wantIDs: []string{"AAA1"},
		},
		{
			name:     "empty matches -> none",
			res:      RoseRerankResult{CultivarCertain: true, Matches: nil},
			wantTier: TierNone,
		},
		{
			name: "all ids hallucinated -> none",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "ZZ9", Confidence: 0.9},
			}},
			wantTier: TierNone,
		},
		{
			// Codex #44 P2: a hallucinated high-confidence first id must NOT shield a
			// low-confidence real candidate. After dropping ZZ9, survivors[0] is
			// AAA2@0.2 — below MinConfidenceFloor so NOT certain, but above GuessFloor
			// so surfaced as a guess (evaluated at 0.2, never at ZZ9's 0.95).
			name: "hallucinated-first shields low real -> floor on survivor -> guess",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "ZZ9", Confidence: 0.95}, {PlantID: "AAA2", Confidence: 0.2},
			}},
			wantTier: TierGuess, wantIDs: []string{"AAA2"},
		},
		{
			name: "certain but first below MinConfidenceFloor -> guess",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.3},
			}},
			wantTier: TierGuess, wantIDs: []string{"AAA1"},
		},
		{
			name: "top survivor below GuessFloor -> none",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.1},
			}},
			wantTier: TierNone,
		},
		{
			name: "not certain and below GuessFloor -> none",
			res: RoseRerankResult{CultivarCertain: false, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.05},
			}},
			wantTier: TierNone,
		},
		{
			name: "drops hallucinated middle id, keeps order -> certain",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.7}, {PlantID: "ZZ9", Confidence: 0.6}, {PlantID: "AAA3", Confidence: 0.5},
			}},
			wantTier: TierCertain, wantIDs: []string{"AAA1", "AAA3"},
		},
		{
			name: "caps at MaxMatches=3 (certain)",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.9}, {PlantID: "AAA2", Confidence: 0.8},
				{PlantID: "AAA3", Confidence: 0.7}, {PlantID: "AAA4", Confidence: 0.6},
			}},
			wantTier: TierCertain, wantIDs: []string{"AAA1", "AAA2", "AAA3"},
		},
		{
			name: "caps + dedups (guess)",
			res: RoseRerankResult{CultivarCertain: false, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.3}, {PlantID: "AAA1", Confidence: 0.28},
				{PlantID: "AAA2", Confidence: 0.25}, {PlantID: "AAA3", Confidence: 0.2}, {PlantID: "AAA4", Confidence: 0.18},
			}},
			wantTier: TierGuess, wantIDs: []string{"AAA1", "AAA2", "AAA3"},
		},
		{
			name: "duplicate valid id -> deduped, first occurrence kept",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.9}, {PlantID: "AAA1", Confidence: 0.7}, {PlantID: "AAA2", Confidence: 0.6},
			}},
			wantTier: TierCertain, wantIDs: []string{"AAA1", "AAA2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.res, valid)
			if got.Tier != tt.wantTier {
				t.Fatalf("tier = %v, want %v", got.Tier, tt.wantTier)
			}
			if tt.wantTier == TierNone {
				if len(got.Matches) != 0 {
					t.Fatalf("TierNone must carry no matches, got %v", got.Matches)
				}
				return
			}
			if len(got.Matches) != len(tt.wantIDs) {
				t.Fatalf("got %d matches, want %d (%v)", len(got.Matches), len(tt.wantIDs), got.Matches)
			}
			for i, id := range tt.wantIDs {
				if got.Matches[i].PlantID != id {
					t.Errorf("match[%d] = %q, want %q", i, got.Matches[i].PlantID, id)
				}
			}
		})
	}
}
