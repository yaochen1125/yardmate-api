package enrichment

import (
	"encoding/json"
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// TestReconcileRow covers the pure decode/reconcile/diff decision the backfill
// makes per row (the DB read/write glue around it needs a live Postgres and is
// exercised via the dry-run of cmd/backfill-periods).
func TestReconcileRow(t *testing.T) {
	mustRaw := func(p proxy.PlantDetail) []byte {
		b, err := json.Marshal(&p)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		return b
	}

	t.Run("drifted label is repaired and flagged changed", func(t *testing.T) {
		raw := mustRaw(proxy.PlantDetail{
			BloomMonthsNorth: []int{4, 5, 6, 7, 8, 9, 10, 11},
			BloomPeriodShort: "全年", // wrong: months say Apr–Nov, label says year-round
		})
		pd, oldBloom, changed, err := reconcileRow(raw, "zh-Hant")
		if err != nil {
			t.Fatalf("reconcileRow: %v", err)
		}
		if !changed {
			t.Fatal("drifted row must be flagged changed")
		}
		if oldBloom != "全年" {
			t.Errorf("oldBloom = %q, want 全年", oldBloom)
		}
		if pd.BloomPeriodShort != "四月 → 十一月" {
			t.Errorf("derived label = %q, want 四月 → 十一月", pd.BloomPeriodShort)
		}
	})

	t.Run("already-consistent row is left unchanged", func(t *testing.T) {
		raw := mustRaw(proxy.PlantDetail{
			BloomMonthsNorth: []int{7, 8, 9, 10},
			BloomPeriodShort: "Jul → Oct",
			FruitMonthsNorth: []int{}, // empty → fruit label nil, already consistent
		})
		_, _, changed, err := reconcileRow(raw, "en")
		if err != nil {
			t.Fatalf("reconcileRow: %v", err)
		}
		if changed {
			t.Error("consistent row must not be flagged changed")
		}
	})

	t.Run("unsorted/duped months count as changed (sanitized)", func(t *testing.T) {
		raw := mustRaw(proxy.PlantDetail{
			BloomMonthsNorth: []int{8, 7, 7, 9, 10},
			BloomPeriodShort: "Jul → Oct",
		})
		pd, _, changed, err := reconcileRow(raw, "en")
		if err != nil {
			t.Fatalf("reconcileRow: %v", err)
		}
		if !changed {
			t.Fatal("unsorted/duped months must be sanitized → changed")
		}
		want := []int{7, 8, 9, 10}
		if len(pd.BloomMonthsNorth) != len(want) {
			t.Fatalf("months = %v, want %v", pd.BloomMonthsNorth, want)
		}
		for i := range want {
			if pd.BloomMonthsNorth[i] != want[i] {
				t.Fatalf("months = %v, want %v", pd.BloomMonthsNorth, want)
			}
		}
	})

	t.Run("malformed JSON returns an error", func(t *testing.T) {
		if _, _, _, err := reconcileRow([]byte(`{not json`), "en"); err == nil {
			t.Error("expected decode error for malformed JSON")
		}
	})
}
