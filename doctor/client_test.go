package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeUpstream streams OpenAI-style SSE chunks: the reply JSON split into
// pieces, then a usage chunk, then [DONE].
func fakeUpstream(t *testing.T, pieces []string, includeUsage bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, p := range pieces {
			chunk := map[string]any{
				"choices": []map[string]any{{"delta": map[string]any{"content": p}}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
		if includeUsage {
			fmt.Fprintf(w, `data: {"choices":[],"usage":{"prompt_tokens":1842,"completion_tokens":318}}`+"\n\n")
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
}

const wireReply = `{"photoProblem":null,"observations":["Leaves drooping","Soil pale and dry"],"clarification":null,"healthLevel":"needs_treatment","diagnosis":{"primary":"Water stress","confidence":"high"},"possibleCauses":[{"cause":"Dry root zone","likelihood":"high","why":"Soil pulled from pot edge."}],"actionsNow":["Water deeply now","Shade this afternoon"],"expectedRecovery":{"shortTerm":"Leaves lift in a day","longTerm":"New growth in two weeks"},"followUp":{"reminderTitle":"Check the plant","afterDays":2,"whatToLookFor":"Are leaves standing again?"},"spokenSummary":"The root zone is badly dry."}`

func TestStreamEmitsObservationsBeforeReply(t *testing.T) {
	// Split so the second observation completes in a later chunk than the
	// first — the order of onObservation callbacks is the product behaviour.
	i := strings.Index(wireReply, "drooping")
	j := strings.Index(wireReply, "clarification")
	pieces := []string{wireReply[:i], wireReply[i:j], wireReply[j:]}

	up := fakeUpstream(t, pieces, true)
	defer up.Close()

	c := NewClient("test-key", up.URL)
	var got []string
	reply, usage, err := c.Stream(context.Background(), StreamRequest{
		Model:    "gpt-4o-mini",
		UserText: "help",
	}, func(index int, s string) {
		if index != len(got) {
			t.Fatalf("out-of-order observation index %d at position %d", index, len(got))
		}
		got = append(got, s)
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 2 || got[0] != "Leaves drooping" || got[1] != "Soil pale and dry" {
		t.Fatalf("observations = %#v", got)
	}
	if usage == nil || usage.PromptTokens != 1842 || usage.CompletionTokens != 318 {
		t.Fatalf("usage = %+v", usage)
	}
	var decoded map[string]any
	if err := json.Unmarshal(reply, &decoded); err != nil {
		t.Fatalf("reply not valid JSON: %v", err)
	}
	if decoded["spokenSummary"] != "The root zone is badly dry." {
		t.Fatalf("spokenSummary = %v", decoded["spokenSummary"])
	}
}

func TestStreamTruncatedReplyIsBadReply(t *testing.T) {
	up := fakeUpstream(t, []string{wireReply[:80]}, false)
	defer up.Close()
	c := NewClient("k", up.URL)
	_, _, err := c.Stream(context.Background(), StreamRequest{Model: "gpt-4o-mini", UserText: "x"},
		func(int, string) {})
	if !errors.Is(err, ErrBadReply) {
		t.Fatalf("err = %v, want ErrBadReply", err)
	}
}

func TestStreamNon200IsUnavailable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusBadRequest)
	}))
	defer up.Close()
	c := NewClient("k", up.URL)
	_, _, err := c.Stream(context.Background(), StreamRequest{Model: "gpt-4o-mini", UserText: "x"},
		func(int, string) {})
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrUpstreamUnavailable", err)
	}
}

func TestStreamContextCancelIsTimeout(t *testing.T) {
	blocked := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-blocked // hold the stream open, no data
	}))
	defer up.Close()
	defer close(blocked)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c := NewClient("k", up.URL)
	_, _, err := c.Stream(ctx, StreamRequest{Model: "gpt-4o-mini", UserText: "x"},
		func(int, string) {})
	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("err = %v, want ErrUpstreamTimeout", err)
	}
}

func TestBuildBodyShape(t *testing.T) {
	body, err := buildBody(StreamRequest{
		Model:    "gpt-5-mini",
		Language: "zh-Hans",
		Units:    "imperial",
		History: []HistoryTurn{{
			User: "it wilted", HadImages: true, Reply: json.RawMessage(`{"a":1}`),
		}},
		UserText: "still droopy",
		Images:   [][]byte{pngBytes()},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Model           string          `json:"model"`
		Stream          bool            `json:"stream"`
		StreamOptions   map[string]bool `json:"stream_options"`
		ReasoningEffort string          `json:"reasoning_effort"`
		ResponseFormat  json.RawMessage `json:"response_format"`
		Messages        []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Stream || !decoded.StreamOptions["include_usage"] {
		t.Fatal("stream/include_usage not set")
	}
	if decoded.ReasoningEffort != "minimal" {
		t.Fatalf("gpt-5 family must send reasoning_effort=minimal, got %q", decoded.ReasoningEffort)
	}
	// system + (user, assistant) history pair + final user = 4
	if len(decoded.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(decoded.Messages))
	}
	var system string
	_ = json.Unmarshal(decoded.Messages[0].Content, &system)
	if !strings.Contains(system, "Simplified Chinese") || !strings.Contains(system, "inches and feet") {
		t.Fatal("system prompt missing language/units substitution")
	}
	var hist string
	_ = json.Unmarshal(decoded.Messages[1].Content, &hist)
	if !strings.HasPrefix(hist, "[sent a photo]") {
		t.Fatalf("history user line = %q, want [sent a photo] prefix", hist)
	}
	// Schema field order survives the round trip: observations before
	// spokenSummary in the raw response_format bytes (order is the contract).
	rf := string(decoded.ResponseFormat)
	if strings.Index(rf, `"observations"`) > strings.Index(rf, `"spokenSummary"`) {
		t.Fatal("schema order broken: observations must precede spokenSummary")
	}
	// reasoning_effort must be ABSENT for non-gpt-5 models (they 400 on it).
	body4o, _ := buildBody(StreamRequest{Model: "gpt-4o-mini", UserText: "x"})
	if strings.Contains(string(body4o), "reasoning_effort") {
		t.Fatal("gpt-4o-mini body must not carry reasoning_effort")
	}
}

// pngBytes returns a minimal valid PNG header so DetectContentType sniffs
// image/png (the handler validates mime; the fake upstream ignores bodies).
func pngBytes() []byte {
	return []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
}
