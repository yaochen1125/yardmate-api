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
		name      string
		res       RoseRerankResult
		wantApply bool
		wantIDs   []string
	}{
		{
			name: "certain with strong first match -> apply",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.8}, {PlantID: "AAA2", Confidence: 0.4},
			}},
			wantApply: true, wantIDs: []string{"AAA1", "AAA2"},
		},
		{
			// Solution A (rose-guess-loosen): CultivarCertain is no longer a gate.
			// A confident best-guess (>= floor) applies even when not certain.
			name: "not certain but top >= floor -> apply (solution A)",
			res: RoseRerankResult{CultivarCertain: false, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.9},
			}},
			wantApply: true, wantIDs: []string{"AAA1"},
		},
		{
			// The confidence floor is now the SOLE gate — a low-confidence guess
			// still falls back even without the certain gate.
			name: "not certain and top < floor -> fall back",
			res: RoseRerankResult{CultivarCertain: false, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.3},
			}},
			wantApply: false,
		},
		{
			name:      "empty matches -> fall back",
			res:       RoseRerankResult{CultivarCertain: true, Matches: nil},
			wantApply: false,
		},
		{
			name: "all ids hallucinated -> fall back",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "ZZ9", Confidence: 0.9},
			}},
			wantApply: false,
		},
		{
			// Codex #44 P2: a hallucinated high-confidence first id must NOT shield
			// a low-confidence real candidate. After dropping ZZ9, survivors[0] is
			// AAA2@0.2 which is below the floor -> fall back (not apply at 0.2).
			name: "hallucinated-first shields low real -> floor on survivor -> fall back",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "ZZ9", Confidence: 0.95}, {PlantID: "AAA2", Confidence: 0.2},
			}},
			wantApply: false,
		},
		{
			name: "real first below floor -> fall back",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.3},
			}},
			wantApply: false,
		},
		{
			name: "drops hallucinated middle id, keeps order",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.7}, {PlantID: "ZZ9", Confidence: 0.6}, {PlantID: "AAA3", Confidence: 0.5},
			}},
			wantApply: true, wantIDs: []string{"AAA1", "AAA3"},
		},
		{
			name: "caps at MaxMatches=3",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.9}, {PlantID: "AAA2", Confidence: 0.8},
				{PlantID: "AAA3", Confidence: 0.7}, {PlantID: "AAA4", Confidence: 0.6},
			}},
			wantApply: true, wantIDs: []string{"AAA1", "AAA2", "AAA3"},
		},
		{
			name: "duplicate valid id -> deduped, first occurrence kept",
			res: RoseRerankResult{CultivarCertain: true, Matches: []RoseMatch{
				{PlantID: "AAA1", Confidence: 0.9}, {PlantID: "AAA1", Confidence: 0.7}, {PlantID: "AAA2", Confidence: 0.6},
			}},
			wantApply: true, wantIDs: []string{"AAA1", "AAA2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Decide(tt.res, valid)
			if ok != tt.wantApply {
				t.Fatalf("apply = %v, want %v", ok, tt.wantApply)
			}
			if !tt.wantApply {
				return
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("got %d matches, want %d (%v)", len(got), len(tt.wantIDs), got)
			}
			for i, id := range tt.wantIDs {
				if got[i].PlantID != id {
					t.Errorf("match[%d] = %q, want %q", i, got[i].PlantID, id)
				}
			}
		})
	}
}
