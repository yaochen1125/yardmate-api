package enrichment

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// Whole-WORD severity keywords. \b boundaries stop a disease term from leaking a
// false signal: "mildew" (powdery/downy mildew) must NOT read as "mild", and
// "For severe mildew cases" must read as severe (not ambiguous) — a bare substring
// check would set both flags and drop the badge. Case-insensitive.
var (
	mildKeywordRe   = regexp.MustCompile(`(?i)\bmild\b`)
	severeKeywordRe = regexp.MustCompile(`(?i)\bsevere\b`)
)

// One-time disease-severity backfill (DiseasePromptVersion v3).
//
// Before v3, enriched out-of-catalog disease rows (diseases_pending) carried no
// language-independent `severity` on their treatment/prevention groups, so iOS
// could only badge MILD/SEVERE by sniffing the English label — which fails once
// the label is localized (the bug iOS PR #661 fixed in-catalog). v3 emits
// severity on freshly GENERATED masters, but a row translated from a pre-v3
// master copies the master's groups verbatim (nil severity) and is still stamped
// v3, so source_version is NOT a reliable selector here (see DiseasePromptVersion
// doc in disease_prompt.go). This runner closes the gap by CONTENT.
//
// It is fully DETERMINISTIC — no LLM. severity is language-independent, so it is
// derived ONCE per disease from the ENGLISH row (its own severity field when a v3
// row, else sniffed from its English label) and stamped positionally onto every
// language row's matching group. Only groups that currently LACK severity are
// filled; an existing (LLM-authored) severity is left untouched.
//
// LIMIT (honest): the derivation is a keyword sniff of the English label
// ("mild" / "severe"). A pre-v3 disease that IS severity-graded but whose English
// labels avoid those words (e.g. "For early-stage infection") cannot be
// classified deterministically — it is reported as `Undetermined` (NOT silently
// folded into AlreadyOK) and left unbadged for manual / future-LLM classification.
// A disease with no English row is skipped (re-run after the translation sweep
// creates one). Idempotent: a re-run re-derives the same targets and writes
// nothing once every classifiable group is filled.

// diseaseSeverityBackfillTimeout bounds one row's UPDATE.
const diseaseSeverityBackfillTimeout = 30 * time.Second

// DiseaseSeverityBackfillDB is the slice of the DB layer the backfill needs.
// *DB satisfies it; tests substitute a stub.
type DiseaseSeverityBackfillDB interface {
	ListDiseaseSeverityBackfillRows(ctx context.Context) ([]DiseaseSeverityRow, error)
	UpdateDiseaseSeverity(ctx context.Context, normalized, lang string, detail *proxy.StructuredDiseaseDetail, version string) (int64, error)
}

var _ DiseaseSeverityBackfillDB = (*DB)(nil)

// DiseaseSeverityBackfillReport summarizes a run for the operator / caller.
type DiseaseSeverityBackfillReport struct {
	Diseases     int // distinct diseases inspected
	NoEnglish    int // diseases skipped — no English row to derive severity from (retry after the translation sweep)
	Updated      int // rows whose nil severities were filled and persisted
	AlreadyOK    int // rows that needed no change (genuinely non-severity, or already filled)
	Undetermined int // diseases with a severity-split section whose English labels lack the mild/severe keyword — left unbadged, need manual/LLM classification (NOT re-run-fixable)
	Mismatch     int // rows whose section group count diverged from the English row — that section skipped, never positionally mislabeled (data anomaly; should not happen since translations preserve structure)
	Vanished     int // UPDATE matched 0 rows — row deleted/changed between list and update (benign)
	Failed       int // update errored (logged, run continues)
}

// sniffSeverity maps an English group label to its severity bucket: "mild" /
// "severe" / "" (not classifiable). Mirrors the keyword the in-catalog labels use
// ("For mild cases" / "For severe cases"). Returns "" when the label is
// nil/absent, matches neither, or (defensively) matches both.
func sniffSeverity(label *string) string {
	if label == nil {
		return ""
	}
	hasMild := mildKeywordRe.MatchString(*label)
	hasSevere := severeKeywordRe.MatchString(*label)
	switch {
	case hasSevere && !hasMild:
		return "severe"
	case hasMild && !hasSevere:
		return "mild"
	default:
		return ""
	}
}

// deriveTargets returns the per-group target severity for one section, taken from
// the English row: a group's own severity field when already set (a v3 English
// row), else sniffed from its English label (a pre-v3 English row).
func deriveTargets(groups []proxy.DiseaseStepGroup) []string {
	out := make([]string, len(groups))
	for i := range groups {
		if sv := groups[i].Severity; sv != nil && (*sv == "mild" || *sv == "severe") {
			out[i] = *sv
			continue
		}
		out[i] = sniffSeverity(groups[i].Label)
	}
	return out
}

func hasAnySeverity(targets []string) bool {
	for _, t := range targets {
		if t == "mild" || t == "severe" {
			return true
		}
	}
	return false
}

// sectionUndetermined reports whether a section is a severity SPLIT (>=2 groups —
// per the generation contract the only grouping dimension is severity) whose
// English-derived targets are NOT all mild/severe, i.e. labels the keyword sniff
// cannot classify. Such a section is a real coverage gap: it must be surfaced
// (Undetermined), never silently counted AlreadyOK.
func sectionUndetermined(groups []proxy.DiseaseStepGroup, targets []string) bool {
	if len(groups) < 2 {
		return false // single group → ungrouped, not a severity split
	}
	for _, t := range targets {
		if t != "mild" && t != "severe" {
			return true
		}
	}
	return false
}

// fillSection returns a copy of groups with each currently-nil severity filled
// from targets positionally, whether any group changed, and whether the group
// count diverged from targets. On a count mismatch it makes NO change and returns
// mismatch=true (only when there was severity to assign) — so a reordered/dropped
// group can never be positionally MISLABELED. Translations preserve structure, so
// this fires only on corrupt/edited data. An existing severity is never
// overwritten; a "" target is a no-op (group not classifiable). Steps are shared
// verbatim (the group struct is value-copied; its Steps slice header is shared).
func fillSection(groups []proxy.DiseaseStepGroup, targets []string) (out []proxy.DiseaseStepGroup, changed, mismatch bool) {
	out = make([]proxy.DiseaseStepGroup, len(groups))
	copy(out, groups)
	if len(groups) != len(targets) {
		return out, false, hasAnySeverity(targets)
	}
	for i := range out {
		if out[i].Severity == nil && (targets[i] == "mild" || targets[i] == "severe") {
			sv := targets[i]
			out[i].Severity = &sv
			changed = true
		}
	}
	return out, changed, false
}

// RunDiseaseSeverityBackfill fills the language-independent severity field on
// every enriched disease row that lacks it, deriving the value from each
// disease's English row and stamping it onto all languages positionally. Rows are
// processed per-disease, sequentially (volume is small — out-of-catalog only);
// per-row failures are logged and skipped, the run never aborts on one bad row.
// Returns a report plus the first fatal error (only the list query is fatal).
func RunDiseaseSeverityBackfill(ctx context.Context, db DiseaseSeverityBackfillDB) (DiseaseSeverityBackfillReport, error) {
	var rep DiseaseSeverityBackfillReport
	if db == nil {
		return rep, fmt.Errorf("enrichment disease severity backfill: nil db")
	}
	rows, err := db.ListDiseaseSeverityBackfillRows(ctx)
	if err != nil {
		return rep, fmt.Errorf("enrichment disease severity backfill: list rows: %w", err)
	}

	// Group rows by disease (normalized), preserving first-seen order for stable
	// logging. Within a disease the English row is found explicitly below.
	byDisease := make(map[string][]DiseaseSeverityRow)
	order := make([]string, 0)
	for _, r := range rows {
		if _, ok := byDisease[r.Normalized]; !ok {
			order = append(order, r.Normalized)
		}
		byDisease[r.Normalized] = append(byDisease[r.Normalized], r)
	}
	rep.Diseases = len(order)
	log.Printf("enrichment disease severity backfill: %d diseases, %d rows", rep.Diseases, len(rows))

	for _, norm := range order {
		group := byDisease[norm]

		// The English row is the only reliably sniffable source of truth. Its label
		// is present regardless of version, so a pre-v3 English row still yields the
		// bucket; a v3 English row contributes its own severity field directly.
		var en *DiseaseSeverityRow
		for i := range group {
			if group[i].Lang == "en" && group[i].Detail != nil {
				en = &group[i]
				break
			}
		}
		if en == nil {
			rep.NoEnglish++
			log.Printf("enrichment disease severity backfill: no English row for %q — skipped (re-run after translation sweep)", norm)
			continue
		}

		tTargets := deriveTargets(en.Detail.Treatment.Groups)
		pTargets := deriveTargets(en.Detail.Prevention.Groups)

		// Surface (don't hide) a severity-split section whose English labels the
		// keyword sniff can't classify — it's a real, deterministic coverage gap.
		// We still fill whatever IS derivable below (a partially-keyworded disease).
		if sectionUndetermined(en.Detail.Treatment.Groups, tTargets) || sectionUndetermined(en.Detail.Prevention.Groups, pTargets) {
			rep.Undetermined++
			log.Printf("enrichment disease severity backfill: %q is severity-grouped but its English labels lack a mild/severe keyword — left unbadged, needs manual/LLM classification", norm)
		}

		if !hasAnySeverity(tTargets) && !hasAnySeverity(pTargets) {
			// Nothing classifiable to propagate (genuinely ungrouped, or fully
			// undetermined — already counted above). No row needs a write.
			rep.AlreadyOK += len(group)
			continue
		}

		for i := range group {
			if err := ctx.Err(); err != nil {
				return rep, err // shutting down — stop cleanly, resume next run
			}
			r := group[i]
			if r.Detail == nil {
				rep.Failed++
				log.Printf("enrichment disease severity backfill: nil detail normalized=%q lang=%s", r.Normalized, r.Lang)
				continue
			}
			newTreat, c1, m1 := fillSection(r.Detail.Treatment.Groups, tTargets)
			newPrev, c2, m2 := fillSection(r.Detail.Prevention.Groups, pTargets)
			if m1 || m2 {
				rep.Mismatch++
				log.Printf("enrichment disease severity backfill: group-count mismatch vs English normalized=%q lang=%s — section left unfilled (data anomaly)", r.Normalized, r.Lang)
			}
			if !c1 && !c2 {
				rep.AlreadyOK++
				continue
			}
			patched := *r.Detail // shallow copy; rebuild only the group slices below
			patched.Treatment = proxy.DiseaseStepGroups{Groups: newTreat}
			patched.Prevention = proxy.DiseaseStepGroups{Groups: newPrev}

			rowCtx, cancel := context.WithTimeout(ctx, diseaseSeverityBackfillTimeout)
			n, err := db.UpdateDiseaseSeverity(rowCtx, r.Normalized, r.Lang, &patched, DiseasePromptVersion)
			cancel()
			if err != nil {
				rep.Failed++
				log.Printf("enrichment disease severity backfill: update failed normalized=%q lang=%s err=%v", r.Normalized, r.Lang, err)
				continue
			}
			if n == 0 {
				rep.Vanished++
				continue
			}
			rep.Updated++
		}
	}
	log.Printf("enrichment disease severity backfill: done diseases=%d noEnglish=%d updated=%d alreadyOK=%d undetermined=%d mismatch=%d vanished=%d failed=%d",
		rep.Diseases, rep.NoEnglish, rep.Updated, rep.AlreadyOK, rep.Undetermined, rep.Mismatch, rep.Vanished, rep.Failed)
	return rep, nil
}
