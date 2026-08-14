package enrichment

import (
	"context"
	"errors"
	"testing"
)

// The dex counter methods must be nil-safe and surface the typed
// ErrDBUnavailable (same contract as RecordSignal / RecordCatalogSignal), so a
// missing pool degrades to a logged best-effort failure — never a panic on the
// identify hot path.
func TestRecordIdentifyScanNilDB(t *testing.T) {
	var d *DB
	if err := d.RecordIdentifyScan(context.Background(), "AAA0001"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("nil DB: got %v, want ErrDBUnavailable", err)
	}
	if err := (&DB{}).RecordIdentifyScan(context.Background(), "AAA0001"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("nil pool: got %v, want ErrDBUnavailable", err)
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
