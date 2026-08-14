package proxy

import (
	"context"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func TestDexCountablePlantID(t *testing.T) {
	cases := []struct {
		name   string
		result *IdentifyResult
		want   string
	}{
		{"nil result", nil, ""},
		{"no suggestions", &IdentifyResult{}, ""},
		{"out-of-catalog (nil plant_id)", &IdentifyResult{Suggestions: []Suggestion{{PlantID: nil}}}, ""},
		{"blank plant_id", &IdentifyResult{Suggestions: []Suggestion{{PlantID: strPtr("  ")}}}, ""},
		{"unknown sentinel", &IdentifyResult{Suggestions: []Suggestion{{PlantID: strPtr(unknownSentinelPlantID)}}}, ""},
		{"catalog hit", &IdentifyResult{Suggestions: []Suggestion{{PlantID: strPtr("AAA0876")}}}, "AAA0876"},
		{
			"only the top suggestion counts",
			&IdentifyResult{Suggestions: []Suggestion{{PlantID: nil}, {PlantID: strPtr("AAA0002")}}},
			"",
		},
	}
	for _, c := range cases {
		if got := dexCountablePlantID(c.result); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

type chanRecorder struct{ got chan string }

func (r *chanRecorder) RecordIdentifyScan(_ context.Context, plantID string) error {
	r.got <- plantID
	return nil
}

// TestRecordIdentifyScanFires: a non-nil recorder + countable id fires exactly
// one detached write; nil recorder / empty id fire nothing.
func TestRecordIdentifyScanFires(t *testing.T) {
	rec := &chanRecorder{got: make(chan string, 1)}
	recordIdentifyScan(rec, "AAA0876")
	select {
	case id := <-rec.got:
		if id != "AAA0876" {
			t.Errorf("recorded %q, want AAA0876", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recorder never called")
	}

	// Empty id → no goroutine, channel stays silent.
	recordIdentifyScan(rec, "")
	// nil recorder → no-op (must not panic).
	recordIdentifyScan(nil, "AAA0001")
	select {
	case id := <-rec.got:
		t.Errorf("unexpected record %q", id)
	case <-time.After(50 * time.Millisecond):
	}
}
