package rosererank

// MinConfidenceFloor is the defensive lower bound applied to the first SURVIVING
// (real, in-catalog) candidate for the CERTAIN tier. The primary gate is the
// model's CultivarCertain bool; this floor only catches the self-contradiction
// case (certain==true yet a trivially low top confidence). Model self-reported
// confidence is uncalibrated, so we deliberately do not lean on a tuned number
// (SPEC §2.4 / §5).
const MinConfidenceFloor = 0.35

// GuessFloor is the coarse noise gate for the GUESS tier: below it, even a real
// in-catalog candidate is too weak to be worth surfacing as a "possibly". Like
// MinConfidenceFloor it is a defensive backstop, NOT a tuned threshold — model
// self-reported confidence is uncalibrated (SPEC §5); this only trims near-zero
// noise so we don't append meaningless guesses.
const GuessFloor = 0.15

// MaxMatches caps how many cultivars we surface — fills the ≤3 suggestions
// contract and conveys uncertainty honestly (SPEC §5).
const MaxMatches = 3

// Decide turns a raw vision result into a tiered Outcome. Pure function — no IO,
// no proxy import.
//
// Order matters (SPEC §2.4, Codex #44 P2): drop hallucinated ids FIRST, then
// apply the floor to the first surviving candidate. If the floor ran before
// validation, a hallucinated high-confidence first id could pass the floor, get
// dropped, and let a low-confidence real candidate through below the floor.
//
// Tiers (SPEC §2.4):
//   - TierCertain: cultivar_certain AND survivors[0].Confidence ≥ MinConfidenceFloor
//     → caller REPLACES the species suggestions with these cultivars.
//   - TierGuess:  not certain (or below MinConfidenceFloor) BUT survivors[0]
//     .Confidence ≥ GuessFloor → caller surfaces these as "possibly" guesses
//     ALONGSIDE the species result.
//   - TierNone:   no real survivors, or the top survivor is below GuessFloor
//     → caller keeps the species result verbatim.
//
// validIDs is the set of known rose candidate plantIds (built by proxy from the
// candidate list); a match whose PlantID is absent is treated as hallucinated.
func Decide(res RoseRerankResult, validIDs map[string]bool) Outcome {
	// Drop hallucinated ids AND duplicate ids, preserving the model's ranking
	// order (keep the first/highest-ranked occurrence of each — Codex #44 P2:
	// a repeated valid id would otherwise rewrite suggestions with duplicates).
	// Done BEFORE any confidence gate so a hallucinated high-confidence first id
	// can never shield a low-confidence real candidate behind it (Codex #44 P2).
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
		return Outcome{Tier: TierNone}
	}
	if len(survivors) > MaxMatches {
		survivors = survivors[:MaxMatches]
	}

	// Floors apply to the first REAL surviving candidate, not the raw matches[0].
	top := survivors[0].Confidence
	switch {
	case res.CultivarCertain && top >= MinConfidenceFloor:
		return Outcome{Tier: TierCertain, Matches: survivors}
	case top >= GuessFloor:
		return Outcome{Tier: TierGuess, Matches: survivors}
	default:
		return Outcome{Tier: TierNone}
	}
}
