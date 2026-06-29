package enrichment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// fakeTranslateServer stands in for OpenAI chat-completions. It reads the user
// message (a JSON object), prefixes every string value and every native_region
// array element with prefix, and returns it as the completion content — a
// deterministic stand-in for "translation" that lets the test assert the value
// round-trips element-wise without a real model.
func fakeTranslateServer(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("fake server: bad request body: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var userMsg string
		for _, m := range req.Messages {
			if m.Role == "user" {
				userMsg = m.Content
			}
		}
		var in map[string]json.RawMessage
		if err := json.Unmarshal([]byte(userMsg), &in); err != nil {
			t.Errorf("fake server: user content not JSON object: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		out := make(map[string]any, len(in))
		for k, v := range in {
			if k == "native_region" {
				var arr []string
				if err := json.Unmarshal(v, &arr); err != nil {
					t.Errorf("fake server: native_region not array: %v", err)
					http.Error(w, "bad", http.StatusBadRequest)
					return
				}
				for i := range arr {
					arr[i] = prefix + arr[i]
				}
				out[k] = arr
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				t.Errorf("fake server: value %q not string: %v", k, err)
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			out[k] = prefix + s
		}
		content, _ := json.Marshal(out)
		resp := map[string]any{
			"id":      "chatcmpl-fake",
			"choices": []map[string]any{{"message": map[string]any{"content": string(content)}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func fakeLLMClient(endpoint string) *LLMClient {
	return &LLMClient{
		APIKey:   "test",
		Endpoint: endpoint,
		Model:    "gpt-4o-mini-test",
		HTTP:     &http.Client{Timeout: 5 * time.Second},
	}
}

// arityBreakingServer translates prose normally (prefix) but returns a
// native_region array with a DIFFERENT element count than the input — simulating
// a model that merges/drops/invents an origin. Used to prove Translate's arity
// guard keeps the source regions rather than persisting a mangled list.
func arityBreakingServer(t *testing.T, prefix string, regions []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		var userMsg string
		for _, m := range req.Messages {
			if m.Role == "user" {
				userMsg = m.Content
			}
		}
		var in map[string]json.RawMessage
		_ = json.Unmarshal([]byte(userMsg), &in)
		out := make(map[string]any, len(in))
		for k, v := range in {
			if k == "native_region" {
				out[k] = regions // arity deliberately != input
				continue
			}
			var s string
			_ = json.Unmarshal(v, &s)
			out[k] = prefix + s
		}
		content, _ := json.Marshal(out)
		resp := map[string]any{"id": "chatcmpl-fake", "choices": []map[string]any{{"message": map[string]any{"content": string(content)}}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestTranslate_NativeRegionArityMismatch_KeepsSource: when the model returns a
// native_region array with the wrong element count, Translate must KEEP the
// source (English) regions rather than persist the mangled list — the same
// invariant the one-time backfill enforces. Prose still localizes.
func TestTranslate_NativeRegionArityMismatch_KeepsSource(t *testing.T) {
	// Source has 2 regions; model returns 1 (a merge).
	srv := arityBreakingServer(t, "de:", []string{"de:Mittelmeerraum"})
	defer srv.Close()
	c := fakeLLMClient(srv.URL)

	master := &proxy.PlantDetail{
		ScientificName: "Olea europaea",
		CommonName:     "Olive",
		NativeRegion:   []string{"Mediterranean", "North Africa"},
	}
	out, _, err := c.Translate(context.Background(), master, "de")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// Arity mismatch → source regions retained verbatim (not the 1-element reply).
	if !reflect.DeepEqual(out.NativeRegion, []string{"Mediterranean", "North Africa"}) {
		t.Errorf("arity mismatch must keep source regions, got %v", out.NativeRegion)
	}
	// Prose still localized (the guard is scoped to native_region only).
	if out.CommonName != "de:Olive" {
		t.Errorf("prose must still localize, got %q", out.CommonName)
	}

	// Empty-array reply is also an arity mismatch (1+ source → 0) → keep source.
	srv2 := arityBreakingServer(t, "de:", []string{})
	defer srv2.Close()
	c2 := fakeLLMClient(srv2.URL)
	m2 := &proxy.PlantDetail{ScientificName: "Camellia japonica", NativeRegion: []string{"East Asia"}}
	out2, _, err := c2.Translate(context.Background(), m2, "de")
	if err != nil {
		t.Fatalf("Translate (empty): %v", err)
	}
	if !reflect.DeepEqual(out2.NativeRegion, []string{"East Asia"}) {
		t.Errorf("empty translated array must keep source regions, got %v", out2.NativeRegion)
	}
}

// TestTranslate_LocalizesNativeRegionArray is the v5 guard (SPEC §7): Translate
// carries native_region in the same call and localizes it element-wise, while
// every non-prose field stays byte-equal to the master.
func TestTranslate_LocalizesNativeRegionArray(t *testing.T) {
	srv := fakeTranslateServer(t, "de:")
	defer srv.Close()
	c := fakeLLMClient(srv.URL)

	wn := 2
	master := &proxy.PlantDetail{
		ScientificName: "Camellia japonica",
		CommonName:     "Japanese camellia",
		Description:    "An evergreen shrub.",
		NameOrigin:     "From Latin.",
		NativeRegion:   []string{"East Asia", "Japan"},
		Sunlight:       4,
		WateringNote:   &wn,
		FlowerColor:    []string{"red", "white"},
		Soil:           []string{"acidic"},
		Genus:          "Camellia",
	}
	out, reqID, err := c.Translate(context.Background(), master, "de")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if reqID != "chatcmpl-fake" {
		t.Errorf("reqID = %q, want chatcmpl-fake", reqID)
	}
	// native_region localized element-wise, order + arity preserved.
	if !reflect.DeepEqual(out.NativeRegion, []string{"de:East Asia", "de:Japan"}) {
		t.Errorf("native_region not localized element-wise: %v", out.NativeRegion)
	}
	// Prose localized.
	if out.CommonName != "de:Japanese camellia" || out.Description != "de:An evergreen shrub." {
		t.Errorf("prose not localized: name=%q desc=%q", out.CommonName, out.Description)
	}
	// Non-prose verbatim.
	if out.Sunlight != 4 || out.Genus != "Camellia" {
		t.Errorf("non-prose changed: sunlight=%d genus=%q", out.Sunlight, out.Genus)
	}
	if !reflect.DeepEqual(out.FlowerColor, []string{"red", "white"}) || !reflect.DeepEqual(out.Soil, []string{"acidic"}) {
		t.Errorf("enum arrays must stay English keys: flower=%v soil=%v", out.FlowerColor, out.Soil)
	}
	// Master not mutated.
	if !reflect.DeepEqual(master.NativeRegion, []string{"East Asia", "Japan"}) {
		t.Errorf("master native_region mutated: %v", master.NativeRegion)
	}
}

// TestTranslate_RegionsOnlyStillCallsLLM: a row whose ONLY localizable content is
// native_region (no prose strings) must still hit the LLM, not short-circuit to a
// verbatim copy.
func TestTranslate_RegionsOnlyStillCallsLLM(t *testing.T) {
	srv := fakeTranslateServer(t, "ja:")
	defer srv.Close()
	c := fakeLLMClient(srv.URL)

	master := &proxy.PlantDetail{
		ScientificName: "Plantus nameless",
		NativeRegion:   []string{"Mediterranean"},
	}
	out, _, err := c.Translate(context.Background(), master, "ja")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !reflect.DeepEqual(out.NativeRegion, []string{"ja:Mediterranean"}) {
		t.Errorf("regions-only row must still localize native_region, got %v", out.NativeRegion)
	}
}

// TestTranslate_NoProseNoRegions_NoLLM: a row with neither prose nor regions
// returns an unchanged copy without an LLM call (endpoint is unreachable to prove
// no call is made).
func TestTranslate_NoProseNoRegions_NoLLM(t *testing.T) {
	c := fakeLLMClient("http://127.0.0.1:0/unreachable")
	master := &proxy.PlantDetail{ScientificName: "Bare plant", Sunlight: 3}
	out, reqID, err := c.Translate(context.Background(), master, "fr")
	if err != nil {
		t.Fatalf("Translate must not error on empty prose: %v", err)
	}
	if reqID != "" {
		t.Errorf("no LLM call expected, got reqID %q", reqID)
	}
	if out.Sunlight != 3 || out == master {
		t.Errorf("expected an unchanged copy, got %+v", out)
	}
}

// TestTranslateRegions translates ONLY the array (the backfill primitive).
func TestTranslateRegions(t *testing.T) {
	srv := fakeTranslateServer(t, "zh:")
	defer srv.Close()
	c := fakeLLMClient(srv.URL)

	got, reqID, err := c.TranslateRegions(context.Background(), []string{"North Africa", "Mediterranean"}, "zh-Hans")
	if err != nil {
		t.Fatalf("TranslateRegions: %v", err)
	}
	if reqID != "chatcmpl-fake" {
		t.Errorf("reqID = %q", reqID)
	}
	if !reflect.DeepEqual(got, []string{"zh:North Africa", "zh:Mediterranean"}) {
		t.Errorf("regions not localized: %v", got)
	}

	// Empty input → no call, nil result.
	got, reqID, err = c.TranslateRegions(context.Background(), nil, "zh-Hans")
	if err != nil || got != nil || reqID != "" {
		t.Errorf("empty input must no-op, got %v reqID=%q err=%v", got, reqID, err)
	}
}

// TestSystemPrompt_NonEnglish_LocalizesNativeRegion: native_region joins the
// non-English free-text list with geographic-name guidance; English prompt does
// not localize it.
func TestSystemPrompt_NonEnglish_LocalizesNativeRegion(t *testing.T) {
	de := systemPrompt("de")
	if !strings.Contains(de, "native_region") {
		t.Error("non-en system prompt must list native_region as a free-text field")
	}
	if !strings.Contains(de, "geographic") && !strings.Contains(de, "place name") {
		t.Errorf("non-en system prompt must explain native_region holds geographic place names, got: %q", de)
	}
	en := systemPrompt("en")
	if strings.Contains(en, "native_region") {
		t.Errorf("English system prompt must not single out native_region for localization, got: %q", en)
	}
}

// TestBuildResponseSchema_NativeRegionLocalizedForNonEnglish: the native_region
// description embeds the target language for non-en, and not for en.
func TestBuildResponseSchema_NativeRegionLocalizedForNonEnglish(t *testing.T) {
	deProps := buildResponseSchema("de")["properties"].(map[string]any)
	deDesc := deProps["native_region"].(map[string]any)["description"].(string)
	if !strings.Contains(deDesc, "German") {
		t.Errorf("native_region description must embed the target language for non-en, got: %q", deDesc)
	}
	enProps := buildResponseSchema("en")["properties"].(map[string]any)
	enDesc := enProps["native_region"].(map[string]any)["description"].(string)
	if strings.Contains(enDesc, "Write this field in") {
		t.Errorf("English native_region description must not carry a language directive, got: %q", enDesc)
	}
}

// --- backfill runner ---

type stubBackfillDB struct {
	rows        []NativeRegionRow
	listErr     error
	updateCalls []nrUpdate
	updateN     int64
}

type nrUpdate struct {
	normalized string
	lang       string
	regions    []string
	version    string
}

func (s *stubBackfillDB) ListNativeRegionBackfillRows(_ context.Context) ([]NativeRegionRow, error) {
	return s.rows, s.listErr
}

func (s *stubBackfillDB) UpdateNativeRegion(_ context.Context, normalized, lang string, regions []string, version string) (int64, error) {
	s.updateCalls = append(s.updateCalls, nrUpdate{normalized, lang, regions, version})
	return s.updateN, nil
}

type stubRegionLLM struct {
	prefix      string
	dropElement bool // simulate a model that drops an element (arity mismatch)
	calls       int
}

func (s *stubRegionLLM) TranslateRegions(_ context.Context, regions []string, _ string) ([]string, string, error) {
	s.calls++
	out := make([]string, 0, len(regions))
	for _, r := range regions {
		out = append(out, s.prefix+r)
	}
	if s.dropElement && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, "chatcmpl-x", nil
}

func TestRunNativeRegionBackfill_LocalizesAndStamps(t *testing.T) {
	db := &stubBackfillDB{
		rows: []NativeRegionRow{
			{Normalized: "camellia japonica", Lang: "de", NativeRegion: []string{"East Asia"}},
			{Normalized: "olea europaea", Lang: "ja", NativeRegion: []string{"Mediterranean", "North Africa"}},
		},
		updateN: 1,
	}
	llm := &stubRegionLLM{prefix: "T:"}

	rep, err := RunNativeRegionBackfill(context.Background(), db, llm)
	if err != nil {
		t.Fatalf("RunNativeRegionBackfill: %v", err)
	}
	if rep.Total != 2 || rep.Updated != 2 || rep.Skipped != 0 || rep.Failed != 0 {
		t.Errorf("report = %+v, want total2 updated2", rep)
	}
	if len(db.updateCalls) != 2 {
		t.Fatalf("expected 2 update calls, got %d", len(db.updateCalls))
	}
	if !reflect.DeepEqual(db.updateCalls[0].regions, []string{"T:East Asia"}) {
		t.Errorf("first update regions = %v", db.updateCalls[0].regions)
	}
	if !reflect.DeepEqual(db.updateCalls[1].regions, []string{"T:Mediterranean", "T:North Africa"}) {
		t.Errorf("second update regions = %v", db.updateCalls[1].regions)
	}
	for _, u := range db.updateCalls {
		if u.version != PromptVersion {
			t.Errorf("update must stamp source_version=%s, got %q", PromptVersion, u.version)
		}
	}
}

// TestRunNativeRegionBackfill_ArityMismatchSkips: a translation that drops an
// element must NOT be persisted — the row is skipped (stays English, retried on a
// later run).
func TestRunNativeRegionBackfill_ArityMismatchSkips(t *testing.T) {
	db := &stubBackfillDB{
		rows:    []NativeRegionRow{{Normalized: "olea europaea", Lang: "ja", NativeRegion: []string{"Mediterranean", "North Africa"}}},
		updateN: 1,
	}
	llm := &stubRegionLLM{prefix: "T:", dropElement: true}

	rep, err := RunNativeRegionBackfill(context.Background(), db, llm)
	if err != nil {
		t.Fatalf("RunNativeRegionBackfill: %v", err)
	}
	if rep.Skipped != 1 || rep.Updated != 0 {
		t.Errorf("arity mismatch must skip without update, report = %+v", rep)
	}
	if len(db.updateCalls) != 0 {
		t.Errorf("no update must be issued on arity mismatch, got %d", len(db.updateCalls))
	}
}
