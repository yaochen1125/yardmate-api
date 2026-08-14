package enrichment

import (
	"context"
	"errors"
	"testing"
)

// The dex counter methods must be nil-safe and surface the typed
// ErrDBUnavailable (same contract as RecordSignal / RecordCatalogSignal), so a
// missing pool degrades to a logged best-effort failure — never a panic from
// the flush worker or the rarity publisher.
func TestAddIdentifyScansNilDB(t *testing.T) {
	var d *DB
	if err := d.AddIdentifyScans(context.Background(), map[string]int64{"AAA0001": 1}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("nil DB: got %v, want ErrDBUnavailable", err)
	}
	if err := (&DB{}).AddIdentifyScans(context.Background(), map[string]int64{"AAA0001": 1}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("nil pool: got %v, want ErrDBUnavailable", err)
	}
}

// An all-junk batch (blank ids, non-positive counts) short-circuits to nil
// without touching the pool — verified via a DB whose pool would fail.
func TestAddIdentifyScansSkipsJunkWithoutQuery(t *testing.T) {
	if err := (&DB{}).AddIdentifyScans(context.Background(), map[string]int64{"": 5, "AAA0001": 0, "AAA0002": -1}); err != nil {
		t.Errorf("junk-only batch: got %v, want nil (no query issued)", err)
	}
	if err := (&DB{}).AddIdentifyScans(context.Background(), nil); err != nil {
		t.Errorf("nil batch: got %v, want nil", err)
	}
}

func TestIdentifyCountTotalsNilDB(t *testing.T) {
	var d *DB
	if _, err := d.IdentifyCountTotals(context.Background()); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("nil DB: got %v, want ErrDBUnavailable", err)
	}
	if _, err := (&DB{}).IdentifyCountTotals(context.Background()); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("nil pool: got %v, want ErrDBUnavailable", err)
	}
}
