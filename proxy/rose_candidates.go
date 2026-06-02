package proxy

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy/rosererank"
)

// minRoseBudget is the minimum budget for rose rerank to be worth attempting;
// below this we skip rather than spend a vision call doomed to time out.
const minRoseBudget = 6 * time.Second

// roseWallClockBudget bounds rose rerank by the server WriteTimeout (35 s)
// measured from request start, not just identify's ctx. A slow upload + fast
// cascade leaves plenty of ctx but little wall clock, and an 18 s rose call
// could then push the response past WriteTimeout (Codex #44 P2). 30 s leaves
// ~5 s margin for response serialization/write under the 35 s WriteTimeout.
const roseWallClockBudget = 30 * time.Second

// roseDescriptionWords caps the candidate description length sent to the model.
const roseDescriptionWords = 30

// genusOf returns the Title-cased genus token of a scientific name, e.g.
// "Rosa chinensis" -> "Rosa", "rosa 'Peace'" -> "Rosa". Exact-token, so it never
// fires on substrings like "Hibiscus rosa-sinensis" (SPEC §7 #1).
func genusOf(sci string) string {
	f := strings.Fields(strings.TrimSpace(sci))
	if len(f) == 0 {
		return ""
	}
	g := f[0]
	return strings.ToUpper(g[:1]) + strings.ToLower(g[1:])
}

// roseBudget computes how long rose rerank may run, bounded by BOTH identify's
// ctx deadline AND the WriteTimeout wall clock measured from reqStart (Codex
// #44 P2). The caller skips rose rerank when the result is < minRoseBudget.
func roseBudget(ctx context.Context, reqStart time.Time) time.Duration {
	budget := roseRerankTimeout
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d < budget {
			budget = d
		}
	}
	if wall := roseWallClockBudget - time.Since(reqStart); wall < budget {
		budget = wall
	}
	return budget
}

// buildRoseCandidates builds the static rose candidate index from the catalog —
// every full PlantDetail whose genus == "Rosa" (the 9 species + ~101 cultivars).
// Built once at startup; never per-request (SPEC §2.2 / §7 #5).
func buildRoseCandidates(content *ContentIndex) []rosererank.RoseCandidate {
	if content == nil {
		return nil
	}
	out := make([]rosererank.RoseCandidate, 0, 128)
	for _, p := range content.fullPlantByID {
		if p == nil || p.Genus != "Rosa" {
			continue
		}
		if p.ID == nil || *p.ID == "" {
			continue
		}
		out = append(out, rosererank.RoseCandidate{
			PlantID:        *p.ID,
			ScientificName: p.ScientificName,
			CommonName:     p.CommonName,
			FlowerColor:    p.FlowerColor,
			Description:    truncateWords(p.Description, roseDescriptionWords),
		})
	}
	// Deterministic order — fullPlantByID is a Go map (random iteration), so sort
	// by plantId to keep the model prompt stable across process starts
	// (reproducibility + OpenAI prompt-cache; Codex #44 P2).
	sort.Slice(out, func(i, j int) bool { return out[i].PlantID < out[j].PlantID })
	return out
}

// roseIDSet returns the set of candidate plantIds (for Decide's id validation).
func roseIDSet(candidates []rosererank.RoseCandidate) map[string]bool {
	m := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		m[c.PlantID] = true
	}
	return m
}

// roseByID indexes candidates by plantId (for rewriting suggestions).
func roseByID(candidates []rosererank.RoseCandidate) map[string]rosererank.RoseCandidate {
	m := make(map[string]rosererank.RoseCandidate, len(candidates))
	for _, c := range candidates {
		m[c.PlantID] = c
	}
	return m
}

// rewriteSuggestionsFromRose replaces result.Suggestions with the matched rose
// cultivars and marks the result AI-enhanced. The new scientific names are
// catalog "Rosa 'Cultivar'" forms, so the downstream resolvePlantID +
// common-name override run unchanged and resolve idempotently (SPEC §1.4).
func rewriteSuggestionsFromRose(result *IdentifyResult, matches []rosererank.RoseMatch, byID map[string]rosererank.RoseCandidate) {
	if result == nil || len(matches) == 0 {
		return
	}
	subs := make([]Suggestion, 0, len(matches))
	for _, m := range matches {
		cand, ok := byID[m.PlantID]
		if !ok {
			continue
		}
		pid := cand.PlantID
		subs = append(subs, Suggestion{
			Name:           cand.CommonName, // curated cultivar name for display (e.g. "About Face"), not "Rosa 'About Face'" (Codex #45 P2)
			ScientificName: cand.ScientificName,
			CommonNames:    []string{cand.CommonName},
			Confidence:     m.Confidence,
			PlantID:        &pid,
		})
	}
	if len(subs) == 0 {
		return
	}
	result.Suggestions = subs
	ts := time.Now().UTC().Format(time.RFC3339)
	result.AIEnhancedAt = &ts
}

// truncateWords keeps at most n whitespace-separated words, appending an ellipsis
// when truncated.
func truncateWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return s
	}
	return strings.Join(f[:n], " ") + " ..."
}
