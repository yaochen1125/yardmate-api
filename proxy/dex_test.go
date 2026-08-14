package proxy

import (
	"context"
	"errors"
	"testing"
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

type fakeSink struct {
	batches []map[string]int64
	err     error
}

func (f *fakeSink) AddIdentifyScans(_ context.Context, counts map[string]int64) error {
	if f.err != nil {
		return f.err
	}
	f.batches = append(f.batches, counts)
	return nil
}

// TestIdentifyScanCounterAccumulateAndFlush: Adds coalesce in memory and one
// flush delivers the whole batch, clearing pending.
func TestIdentifyScanCounterAccumulateAndFlush(t *testing.T) {
	sink := &fakeSink{}
	c := NewIdentifyScanCounter(sink)

	c.Add("AAA0001")
	c.Add("AAA0001")
	c.Add("AAA0002")
	c.Add("") // no-op

	c.flush(context.Background())
	if len(sink.batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(sink.batches))
	}
	got := sink.batches[0]
	if got["AAA0001"] != 2 || got["AAA0002"] != 1 || len(got) != 2 {
		t.Errorf("batch = %v", got)
	}

	// Nothing pending → flush is a no-op (no empty batch hits the sink).
	c.flush(context.Background())
	if len(sink.batches) != 1 {
		t.Errorf("empty flush must not call the sink; batches = %d", len(sink.batches))
	}
}

// TestIdentifyScanCounterFlushErrorRequeues: a failed flush merges the batch
// back so counts survive a transient DB outage.
func TestIdentifyScanCounterFlushErrorRequeues(t *testing.T) {
	sink := &fakeSink{err: errors.New("db down")}
	c := NewIdentifyScanCounter(sink)
	c.Add("AAA0001")
	c.flush(context.Background())

	// Counts accrued during the outage merge with the re-queued batch.
	c.Add("AAA0001")
	sink.err = nil
	c.flush(context.Background())
	if len(sink.batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(sink.batches))
	}
	if got := sink.batches[0]["AAA0001"]; got != 2 {
		t.Errorf("AAA0001 = %d, want 2 (re-queued + new)", got)
	}
}

// TestIdentifyScanCounterNilSafe: a nil counter (feature disabled) must be
// inert on both Add and Start.
func TestIdentifyScanCounterNilSafe(t *testing.T) {
	var c *IdentifyScanCounter
	c.Add("AAA0001") // must not panic
	c.Start(context.Background())
}
