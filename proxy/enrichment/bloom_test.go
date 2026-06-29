package enrichment

import (
	"reflect"
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// TestRenderPeriodShort_CuratedFormats pins the label format against the exact
// conventions observed in the curated 1522-plant catalog, so LLM-generated rows
// look identical to hand-authored ones.
func TestRenderPeriodShort_CuratedFormats(t *testing.T) {
	cases := []struct {
		name   string
		months []int
		want   string
	}{
		{"empty -> blank", []int{}, ""},
		{"all twelve -> year-round", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, "Year-round"},
		{"single month", []int{5}, "May"},
		{"one contiguous run -> arrow", []int{7, 8, 9, 10}, "Jul → Oct"},
		{"two-month run -> arrow", []int{5, 6}, "May → Jun"},
		{"winter wrap-around -> single arrow run", []int{1, 2, 3, 4, 11, 12}, "Nov → Apr"},
		{"two runs -> en-dash + slash", []int{3, 4, 5, 9, 10, 11}, "Mar–May / Sep–Nov"},
		{"two single-month runs", []int{5, 7}, "May / Jul"},
	}
	for _, c := range cases {
		if got := RenderPeriodShort(c.months, "en"); got != c.want {
			t.Errorf("%s: RenderPeriodShort(%v,en) = %q, want %q", c.name, c.months, got, c.want)
		}
	}
}

// TestRenderPeriodShort_Localized verifies month tokens localize while the
// arrow/dash/slash punctuation stays constant. zh-Hant must match the in-app
// header "四月 → 十一月".
func TestRenderPeriodShort_Localized(t *testing.T) {
	cases := []struct {
		lang   string
		months []int
		want   string
	}{
		{"zh-Hant", []int{4, 5, 6, 7, 8, 9, 10, 11}, "四月 → 十一月"},
		{"zh-Hans", []int{4, 5, 6, 7, 8, 9, 10, 11}, "4月 → 11月"},
		{"ja", []int{7, 8, 9, 10}, "7月 → 10月"},
		{"de", []int{3, 4, 5, 9, 10, 11}, "Mär–Mai / Sep–Nov"},
		{"fr", []int{6, 7, 8}, "Juin → Août"},
		{"zh-Hant", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, "全年"},
	}
	for _, c := range cases {
		if got := RenderPeriodShort(c.months, c.lang); got != c.want {
			t.Errorf("RenderPeriodShort(%v,%s) = %q, want %q", c.months, c.lang, got, c.want)
		}
	}
}

// TestSanitizeMonths drops out-of-range values, de-dupes, and sorts.
func TestSanitizeMonths(t *testing.T) {
	cases := []struct {
		in   []int
		want []int
	}{
		{[]int{11, 12, 1, 2}, []int{1, 2, 11, 12}},
		{[]int{4, 4, 5, 0, 13, -1, 5}, []int{4, 5}},
		{nil, []int{}},
	}
	for _, c := range cases {
		if got := sanitizeMonths(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("sanitizeMonths(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestReconcilePeriods is the core consistency guarantee: the label is derived
// from the (sanitized) month array, so the chart and the header cannot disagree.
// This is the exact bug from the screenshot: chart all-12 but header "Apr → Nov".
func TestReconcilePeriods(t *testing.T) {
	t.Run("inconsistent LLM output is repaired", func(t *testing.T) {
		fruit := "garbage that should be replaced"
		p := &proxy.PlantDetail{
			BloomMonthsNorth: []int{12, 4, 5, 6, 7, 8, 9, 10, 11, 4}, // unsorted + dupe
			BloomPeriodShort: "Apr → Nov",                            // disagrees with the months
			FruitMonthsNorth: []int{9, 10},
			FruitPeriodShort: &fruit,
		}
		reconcilePeriods(p, "en")

		if want := []int{4, 5, 6, 7, 8, 9, 10, 11, 12}; !reflect.DeepEqual(p.BloomMonthsNorth, want) {
			t.Errorf("bloom months not sanitized: got %v want %v", p.BloomMonthsNorth, want)
		}
		if p.BloomPeriodShort != "Apr → Dec" {
			t.Errorf("label not derived from months: got %q want %q", p.BloomPeriodShort, "Apr → Dec")
		}
		if p.FruitPeriodShort == nil || *p.FruitPeriodShort != "Sep → Oct" {
			t.Errorf("fruit label not derived: got %v", p.FruitPeriodShort)
		}
	})

	t.Run("non-flowering clears the label and nils fruit", func(t *testing.T) {
		p := &proxy.PlantDetail{
			BloomMonthsNorth: []int{},
			BloomPeriodShort: "stale",
			FruitMonthsNorth: []int{},
		}
		reconcilePeriods(p, "en")
		if p.BloomPeriodShort != "" {
			t.Errorf("empty bloom months must clear the label, got %q", p.BloomPeriodShort)
		}
		if p.FruitPeriodShort != nil {
			t.Errorf("empty fruit months must null the label, got %v", *p.FruitPeriodShort)
		}
	})

	t.Run("nil-safe", func(t *testing.T) { reconcilePeriods(nil, "en") })
}
