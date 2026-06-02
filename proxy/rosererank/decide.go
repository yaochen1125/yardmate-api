package rosererank

// MinConfidenceFloor is the defensive lower bound applied to the first SURVIVING
// (real, in-catalog) candidate. The primary gate is the model's CultivarCertain
// bool; this floor only catches the self-contradiction case (certain==true yet a
// trivially low top confidence). Model self-reported confidence is uncalibrated,
// so we deliberately do not lean on a tuned number (SPEC §2.4 / §5).
const MinConfidenceFloor = 0.35

// MaxMatches caps how many cultivars we surface — fills the ≤3 suggestions
// contract and conveys uncertainty honestly (SPEC §5).
const MaxMatches = 3

// Decide turns a raw vision result into the matches to apply, or (nil,false) to
// fall back to the species result. Pure function — no IO, no proxy import.
//
// Order matters (SPEC §2.4, Codex #44 P2): drop hallucinated ids FIRST, then
// apply the floor to the first surviving candidate. If the floor ran before
// validation, a hallucinated high-confidence first id could pass the floor, get
// dropped, and let a low-confidence real candidate through below the floor.
//
// validIDs is the set of known rose candidate plantIds (built by proxy from the
// candidate list); a match whose PlantID is absent is treated as hallucinated.
func Decide(res RoseRerankResult, validIDs map[string]bool) ([]RoseMatch, bool) {
	if !res.CultivarCertain {
		return nil, false
	}

	// Drop hallucinated ids AND duplicate ids, preserving the model's ranking
	// order (keep the first/highest-ranked occurrence of each — Codex #44 P2:
	// a repeated valid id would otherwise rewrite suggestions with duplicates).
	survivors := make([]RoseMatch, 0, len(res.Matches))
	seen := make(map[string]bool, len(res.Matches))
	for _, m := range res.Matches {
		if !validIDs[m.PlantID] || seen[m.PlantID] {
			continue
		}
		seen[m.PlantID] = true
		survivors = append(survivors, m)
	}
	if len(survivors) == 0 {
		return nil, false
	}

	// Floor applies to the first REAL surviving candidate, not the raw matches[0].
	if survivors[0].Confidence < MinConfidenceFloor {
		return nil, false
	}

	if len(survivors) > MaxMatches {
		survivors = survivors[:MaxMatches]
	}
	return survivors, true
}
