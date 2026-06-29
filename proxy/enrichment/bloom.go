package enrichment

import (
	"sort"
	"strings"

	"github.com/yaochen1125/yardmate-api/proxy"
	"github.com/yaochen1125/yardmate-api/proxy/lang"
)

// bloom.go — deterministic reconciliation of the *_period_short labels against
// the canonical *_months_north arrays.
//
// Why: bloom_period_short / fruit_period_short and bloom_months_north /
// fruit_months_north used to be three independent LLM-authored fields. The app
// renders the per-month chart from the months array and the "Apr → Nov" header
// from period_short, so an LLM that emitted [1..12] for the chart but "Apr → Nov"
// for the header produced a visible contradiction (the chart said year-round, the
// header said spring–autumn). period_short is also localized prose, so on the
// translation path the range could drift again.
//
// Fix: the months array is the single source of truth. We sanitize it (1..12,
// deduped, sorted) and DERIVE the label from it on every LLM output path
// (Generate + Translate), reproducing the curated catalog's exact format so new
// rows look identical to the 1522 hand-authored ones. The chart and the header
// can no longer disagree because both come from the same array.

// reconcilePeriods sanitizes the month arrays in place and overwrites the
// period_short labels so they always agree with the months. langCode must be a
// normalized supported code (the row's language); month names are localized to
// it. Nil-safe.
func reconcilePeriods(p *proxy.PlantDetail, langCode string) {
	if p == nil {
		return
	}

	p.BloomMonthsNorth = sanitizeMonths(p.BloomMonthsNorth)
	p.BloomPeriodShort = RenderPeriodShort(p.BloomMonthsNorth, langCode)

	p.FruitMonthsNorth = sanitizeMonths(p.FruitMonthsNorth)
	if short := RenderPeriodShort(p.FruitMonthsNorth, langCode); short != "" {
		p.FruitPeriodShort = &short
	} else {
		// No notable fruit months → fruit_period_short is null on the wire
		// (matches the curated schema: nullable string).
		p.FruitPeriodShort = nil
	}
}

// sanitizeMonths returns the input months clamped to 1..12, de-duplicated and
// sorted ascending. Returns an empty (non-nil) slice when nothing valid remains
// so JSON marshals [] not null, matching the curated arrays.
func sanitizeMonths(months []int) []int {
	seen := [13]bool{}
	out := make([]int, 0, len(months))
	for _, m := range months {
		if m < 1 || m > 12 || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Ints(out)
	return out
}

// RenderPeriodShort renders a short, localized period label from a month-integer
// array (1..12), reproducing the curated catalog's format:
//
//	[]                      -> ""                  (non-flowering / no fruit)
//	[1..12]                 -> "Year-round"        (localized)
//	[5]                     -> "May"               (single month)
//	[7,8,9,10]              -> "Jul → Oct"          (one contiguous run, arrow + spaces)
//	[1,2,3,4,11,12]         -> "Nov → Apr"          (one run wrapping Dec→Jan)
//	[3,4,5,9,10,11]         -> "Mar–May / Sep–Nov"  (multiple runs, en-dash, no spaces)
//	[5,7]                   -> "May / Jul"          (multiple single-month runs)
//
// months is assumed sanitized (1..12, deduped, sorted); it is treated read-only.
func RenderPeriodShort(months []int, langCode string) string {
	if len(months) == 0 {
		return ""
	}
	if len(months) == 12 {
		return lang.YearRound(langCode)
	}

	runs := circularRuns(months)
	mn := func(m int) string { return lang.MonthAbbr(langCode, m) }

	if len(runs) == 1 {
		r := runs[0]
		if len(r) == 1 {
			return mn(r[0])
		}
		// Single contiguous run → arrow with surrounding spaces (curated style).
		return mn(r[0]) + " → " + mn(r[len(r)-1])
	}

	parts := make([]string, 0, len(runs))
	for _, r := range runs {
		if len(r) == 1 {
			parts = append(parts, mn(r[0]))
			continue
		}
		// Multiple runs → en-dash without spaces, joined by " / " (curated style).
		parts = append(parts, mn(r[0])+"–"+mn(r[len(r)-1]))
	}
	return strings.Join(parts, " / ")
}

// circularRuns groups sanitized months into maximal contiguous runs on the
// 1..12 circle (December is adjacent to January, so a winter bloomer like
// [1,2,3,4,11,12] is one run [11,12,1,2,3,4], not two). Each returned run lists
// its months in calendar order from its start. Runs are ordered by their start
// month ascending. months must be sorted ascending, 1..12, deduped, and not all
// twelve (callers handle the year-round case first).
func circularRuns(months []int) [][]int {
	present := [13]bool{}
	for _, m := range months {
		present[m] = true
	}
	prev := func(m int) int {
		if m == 1 {
			return 12
		}
		return m - 1
	}
	next := func(m int) int {
		if m == 12 {
			return 1
		}
		return m + 1
	}

	var runs [][]int
	for _, start := range months {
		// A run begins at a present month whose predecessor is absent.
		if present[prev(start)] {
			continue
		}
		run := []int{start}
		for m := next(start); present[m]; m = next(m) {
			run = append(run, m)
		}
		runs = append(runs, run)
	}
	return runs
}
