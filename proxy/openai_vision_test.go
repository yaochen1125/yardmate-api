package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestVisionClient(t *testing.T, handler http.HandlerFunc) (*VisionClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := &VisionClient{
		APIKey:   "test-key",
		Endpoint: srv.URL,
		Model:    "test-model",
		HTTP:     srv.Client(),
	}
	return c, srv
}

const cannedOpenAIPick = `{
  "id": "chatcmpl-abc",
  "choices": [
    {"message": {"role": "assistant", "content": "L20"}}
  ]
}`

func TestDisambiguateDiseaseName_PickHit(t *testing.T) {
	var gotAuth, gotCT string
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, cannedOpenAIPick)
	})
	defer srv.Close()
	refs := []DiseaseNameRef{
		{ID: "L01", Name: "Brown spot"},
		{ID: "L20", Name: "Powdery mildew"},
	}
	id, err := c.DisambiguateDiseaseName(context.Background(), "white dusty mildew", refs)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "L20" {
		t.Errorf("id = %q, want L20", id)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("ct = %q", gotCT)
	}
}

func TestDisambiguateDiseaseName_NoneReply(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"NONE"}}]}`)
	})
	defer srv.Close()
	id, err := c.DisambiguateDiseaseName(context.Background(), "alien disease",
		[]DiseaseNameRef{{ID: "L01", Name: "Brown spot"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty", id)
	}
}

func TestDisambiguateDiseaseName_TrailingProse(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"L20 — Powdery mildew"}}]}`)
	})
	defer srv.Close()
	id, err := c.DisambiguateDiseaseName(context.Background(), "white powder",
		[]DiseaseNameRef{{ID: "L20", Name: "Powdery mildew"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "L20" {
		t.Errorf("id = %q, want L20 (first token)", id)
	}
}

func TestDisambiguateDiseaseName_Hallucinated(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ZZ99"}}]}`)
	})
	defer srv.Close()
	id, err := c.DisambiguateDiseaseName(context.Background(), "anything",
		[]DiseaseNameRef{{ID: "L01", Name: "Brown spot"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty (hallucinated id treated as miss)", id)
	}
}

func TestDisambiguateDiseaseName_Non200(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"server"}`)
	})
	defer srv.Close()
	_, err := c.DisambiguateDiseaseName(context.Background(), "x",
		[]DiseaseNameRef{{ID: "L01", Name: "Brown spot"}})
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err = %v, want status 500", err)
	}
}

func TestDisambiguateDiseaseName_EmptyRefs(t *testing.T) {
	c := &VisionClient{}
	_, err := c.DisambiguateDiseaseName(context.Background(), "x", nil)
	if err == nil {
		t.Error("expected error on empty refs")
	}
}

func TestDisambiguateDiseaseName_NilReceiver(t *testing.T) {
	var c *VisionClient
	_, err := c.DisambiguateDiseaseName(context.Background(), "x",
		[]DiseaseNameRef{{ID: "L01", Name: "Brown spot"}})
	if err == nil {
		t.Error("expected error on nil receiver")
	}
}

// --- SuggestCommonDisease ---

func TestSuggestCommonDisease_PickHit(t *testing.T) {
	var gotBody, gotAuth string
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"P05"}}]}`)
	})
	defer srv.Close()
	refs := []DiseaseNameRef{
		{ID: "R01", Name: "Root rot"},
		{ID: "P05", Name: "Spider mites"},
	}
	id, err := c.SuggestCommonDisease(context.Background(), "Abelia chinensis", 0.15, refs)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "P05" {
		t.Errorf("id = %q, want P05", id)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	// Prompt must carry the plant name + the candidate catalog (id: name).
	if !strings.Contains(gotBody, "Abelia chinensis") {
		t.Errorf("request body missing plant name: %s", gotBody)
	}
	if !strings.Contains(gotBody, "P05: Spider mites") {
		t.Errorf("request body missing candidate catalog line: %s", gotBody)
	}
}

func TestSuggestCommonDisease_NoneReply(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"NONE"}}]}`)
	})
	defer srv.Close()
	id, err := c.SuggestCommonDisease(context.Background(), "Some plant", 0.2,
		[]DiseaseNameRef{{ID: "R01", Name: "Root rot"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty (NONE → safety net)", id)
	}
}

func TestSuggestCommonDisease_TrailingProse(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"P05 — Spider mites"}}]}`)
	})
	defer srv.Close()
	id, err := c.SuggestCommonDisease(context.Background(), "Abelia chinensis", 0.1,
		[]DiseaseNameRef{{ID: "P05", Name: "Spider mites"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "P05" {
		t.Errorf("id = %q, want P05 (first token)", id)
	}
}

func TestSuggestCommonDisease_Hallucinated(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"L20"}}]}`)
	})
	defer srv.Close()
	// L20 is NOT in the candidate set → must be treated as a miss so the
	// caller degrades to the static safety net.
	id, err := c.SuggestCommonDisease(context.Background(), "Abelia chinensis", 0.1,
		[]DiseaseNameRef{{ID: "R01", Name: "Root rot"}, {ID: "P05", Name: "Spider mites"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty (non-candidate id treated as miss)", id)
	}
}

func TestSuggestCommonDisease_Non200(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"server"}`)
	})
	defer srv.Close()
	_, err := c.SuggestCommonDisease(context.Background(), "x", 0.1,
		[]DiseaseNameRef{{ID: "R01", Name: "Root rot"}})
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("err = %v, want status 500 (returned for graceful degrade)", err)
	}
}

func TestSuggestCommonDisease_EmptyRefs(t *testing.T) {
	c := &VisionClient{}
	_, err := c.SuggestCommonDisease(context.Background(), "x", 0.1, nil)
	if err == nil {
		t.Error("expected error on empty refs")
	}
}

func TestSuggestCommonDisease_EmptyPlantName(t *testing.T) {
	c := &VisionClient{}
	_, err := c.SuggestCommonDisease(context.Background(), "   ", 0.1,
		[]DiseaseNameRef{{ID: "R01", Name: "Root rot"}})
	if err == nil {
		t.Error("expected error on empty plant name")
	}
}

func TestSuggestCommonDisease_NilReceiver(t *testing.T) {
	var c *VisionClient
	_, err := c.SuggestCommonDisease(context.Background(), "x", 0.1,
		[]DiseaseNameRef{{ID: "R01", Name: "Root rot"}})
	if err == nil {
		t.Error("expected error on nil receiver")
	}
}

func TestDataURL(t *testing.T) {
	got := dataURL("image/jpeg", []byte{0xff, 0xd8})
	want := "data:image/jpeg;base64," + "/9g="
	if got != want {
		t.Errorf("dataURL = %q, want %q", got, want)
	}
}

// --- RerankIdentify ---

func TestRerankIdentify_PickFromList(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Monstera deliciosa"}}]}`)
	})
	defer srv.Close()
	candidates := []Suggestion{
		{Name: "Philodendron hederaceum", ScientificName: "Philodendron hederaceum"},
		{Name: "Monstera deliciosa", ScientificName: "Monstera deliciosa"},
	}
	pick, err := c.RerankIdentify(context.Background(), []byte("\xff\xd8img"), "image/jpeg", candidates)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pick != "Monstera deliciosa" {
		t.Errorf("pick = %q, want Monstera deliciosa", pick)
	}
}

func TestRerankIdentify_StripsRankPrefix(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"2. Monstera deliciosa"}}]}`)
	})
	defer srv.Close()
	candidates := []Suggestion{
		{Name: "X plant", ScientificName: "X plant"},
		{Name: "Monstera deliciosa", ScientificName: "Monstera deliciosa"},
	}
	pick, err := c.RerankIdentify(context.Background(), []byte("img"), "image/jpeg", candidates)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pick != "Monstera deliciosa" {
		t.Errorf("pick = %q, want Monstera deliciosa (after rank prefix stripped)", pick)
	}
}

func TestRerankIdentify_CaseInsensitive(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"MONSTERA DELICIOSA"}}]}`)
	})
	defer srv.Close()
	candidates := []Suggestion{
		{Name: "Monstera deliciosa", ScientificName: "Monstera deliciosa"},
	}
	pick, err := c.RerankIdentify(context.Background(), []byte("img"), "image/jpeg", candidates)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pick != "Monstera deliciosa" {
		t.Errorf("pick = %q, want Monstera deliciosa", pick)
	}
}

func TestRerankIdentify_NotInList_Error(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Some hallucinated plant"}}]}`)
	})
	defer srv.Close()
	candidates := []Suggestion{
		{Name: "Monstera deliciosa", ScientificName: "Monstera deliciosa"},
	}
	_, err := c.RerankIdentify(context.Background(), []byte("img"), "image/jpeg", candidates)
	if err == nil {
		t.Error("expected error for hallucinated pick")
	}
}

func TestRerankIdentify_EmptyCandidates_Error(t *testing.T) {
	c := &VisionClient{}
	_, err := c.RerankIdentify(context.Background(), []byte("x"), "image/jpeg", nil)
	if err == nil {
		t.Error("expected error on empty candidates")
	}
}

func TestRerankIdentify_NilReceiver_Error(t *testing.T) {
	var c *VisionClient
	_, err := c.RerankIdentify(context.Background(), []byte("x"), "image/jpeg",
		[]Suggestion{{Name: "X"}})
	if err == nil {
		t.Error("expected error on nil receiver")
	}
}

func TestRerankIdentify_Non200_Error(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer srv.Close()
	_, err := c.RerankIdentify(context.Background(), []byte("x"), "image/jpeg",
		[]Suggestion{{Name: "X"}})
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Errorf("err = %v, want status 502", err)
	}
}

// --- IdentifyPlant (tier-3 identify fallback, SPEC §1.1 / §2.1 / §7) ---

func TestIdentifyPlant_Success(t *testing.T) {
	var gotBody, gotAuth string
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// json_schema strict reply: the message content is the JSON string.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Monstera deliciosa\",\"common_names\":[\"Swiss cheese plant\"],\"confidence\":0.83}"}}]}`)
	})
	defer srv.Close()

	sug, err := c.IdentifyPlant(context.Background(), []byte("\xff\xd8img"), "image/jpeg")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sug.Name != "Monstera deliciosa" || sug.ScientificName != "Monstera deliciosa" {
		t.Errorf("name/scientific = %q/%q, want Monstera deliciosa", sug.Name, sug.ScientificName)
	}
	if len(sug.CommonNames) != 1 || sug.CommonNames[0] != "Swiss cheese plant" {
		t.Errorf("CommonNames = %v, want [Swiss cheese plant]", sug.CommonNames)
	}
	if sug.Confidence != 0.83 {
		t.Errorf("Confidence = %v, want 0.83", sug.Confidence)
	}
	if sug.PlantID != nil {
		t.Errorf("PlantID = %v, want nil (handler fills it)", sug.PlantID)
	}
	if sug.ImageURL != nil {
		t.Errorf("ImageURL = %v, want nil (no reference image on AI path)", sug.ImageURL)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	// Request must carry the image data URL + the json_schema response_format.
	if !strings.Contains(gotBody, "data:image/jpeg;base64,") {
		t.Errorf("request body missing image data URL: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"response_format"`) || !strings.Contains(gotBody, `"json_schema"`) {
		t.Errorf("request body missing json_schema response_format: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"strict":true`) {
		t.Errorf("request body json_schema not strict: %s", gotBody)
	}
}

func TestIdentifyPlant_NilCommonNamesBecomesEmptySlice(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Ficus lyrata\",\"common_names\":null,\"confidence\":0.6}"}}]}`)
	})
	defer srv.Close()
	sug, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sug.CommonNames == nil || len(sug.CommonNames) != 0 {
		t.Errorf("CommonNames = %v, want non-nil empty slice (wire []  not null)", sug.CommonNames)
	}
}

func TestIdentifyPlant_ConfidenceClamped(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Aloe vera\",\"common_names\":[],\"confidence\":1.7}"}}]}`)
	})
	defer srv.Close()
	sug, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sug.Confidence != 1 {
		t.Errorf("Confidence = %v, want clamped to 1", sug.Confidence)
	}
}

func TestIdentifyPlant_Non200_SentinelError(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"server"}`)
	})
	defer srv.Close()
	_, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err == nil || !errors.Is(err, ErrVisionIdentifyUnavailable) {
		t.Errorf("err = %v, want ErrVisionIdentifyUnavailable", err)
	}
}

func TestIdentifyPlant_MalformedJSON_SentinelError(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		// 200 OK but the message content is not valid JSON.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"not json at all"}}]}`)
	})
	defer srv.Close()
	_, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err == nil || !errors.Is(err, ErrVisionIdentifyUnavailable) {
		t.Errorf("err = %v, want ErrVisionIdentifyUnavailable (decode failure)", err)
	}
}

func TestIdentifyPlant_Refusal_EmptyContent_SentinelError(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Model refusal / safety stop → empty message content.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
	})
	defer srv.Close()
	_, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err == nil || !errors.Is(err, ErrVisionIdentifyUnavailable) {
		t.Errorf("err = %v, want ErrVisionIdentifyUnavailable (refusal)", err)
	}
}

func TestIdentifyPlant_EmptyScientificName_SentinelError(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"   \",\"common_names\":[],\"confidence\":0.4}"}}]}`)
	})
	defer srv.Close()
	_, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err == nil || !errors.Is(err, ErrVisionIdentifyUnavailable) {
		t.Errorf("err = %v, want ErrVisionIdentifyUnavailable (blank scientific_name)", err)
	}
}

func TestIdentifyPlant_NilReceiver_SentinelError(t *testing.T) {
	var c *VisionClient
	_, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err == nil || !errors.Is(err, ErrVisionIdentifyUnavailable) {
		t.Errorf("err = %v, want ErrVisionIdentifyUnavailable (nil receiver)", err)
	}
}

func TestIdentifyPlant_EmptyImage_SentinelError(t *testing.T) {
	c := &VisionClient{}
	_, err := c.IdentifyPlant(context.Background(), nil, "image/jpeg")
	if err == nil || !errors.Is(err, ErrVisionIdentifyUnavailable) {
		t.Errorf("err = %v, want ErrVisionIdentifyUnavailable (empty image)", err)
	}
}

// TestVisionClient_IdentifyUsesLongerClient locks the timeout-bug fix
// (Codex #18 P2) structurally: the tier-3 identify path must run on a
// SEPARATE http.Client whose Timeout is longer than the 15 s IdentifyPlant
// context deadline (so the context — not the client's hard Timeout cap — is
// the effective deadline), while rerank / disambiguation stay on the
// unchanged shared 8 s client. (A real >8 s sleep is impractical in a unit
// test, so this asserts the wiring instead.)
func TestVisionClient_IdentifyUsesLongerClient(t *testing.T) {
	c := NewVisionClient("test-key")

	if c.HTTP == nil || c.identifyHTTP == nil {
		t.Fatalf("both HTTP and identifyHTTP must be set: HTTP=%v identifyHTTP=%v", c.HTTP, c.identifyHTTP)
	}
	// Shared client: unchanged 8 s (rerank / disambiguation depend on this).
	if c.HTTP.Timeout != defaultVisionTimeout {
		t.Errorf("HTTP.Timeout = %v, want %v (shared rerank/disambiguation client must stay 8 s)", c.HTTP.Timeout, defaultVisionTimeout)
	}
	// Identify client: the dedicated longer client.
	if c.identifyHTTP.Timeout != visionIdentifyClientTimeout {
		t.Errorf("identifyHTTP.Timeout = %v, want %v", c.identifyHTTP.Timeout, visionIdentifyClientTimeout)
	}
	// The identify client MUST be a distinct instance, not aliased to HTTP
	// (aliasing would re-clamp identify to 8 s).
	if c.identifyHTTP == c.HTTP {
		t.Error("identifyHTTP must be a separate *http.Client, not the shared 8 s HTTP client")
	}
	// The client cap must sit ABOVE the 15 s IdentifyPlant context deadline
	// so the context is the real deadline (not silently clamped), and still
	// under the 30 s handler identifyUpstreamTimeout.
	if !(c.identifyHTTP.Timeout > c.HTTP.Timeout) {
		t.Errorf("identifyHTTP.Timeout (%v) must exceed HTTP.Timeout (%v)", c.identifyHTTP.Timeout, c.HTTP.Timeout)
	}
	if !(visionIdentifyClientTimeout > visionIdentifyTimeout) {
		t.Errorf("visionIdentifyClientTimeout (%v) must exceed the 15 s IdentifyPlant context deadline visionIdentifyTimeout (%v) so the context is the effective deadline", visionIdentifyClientTimeout, visionIdentifyTimeout)
	}
}

// TestIdentifyPlant_NilIdentifyHTTP_FallsBackToHTTP guards the nil path:
// a VisionClient built as a struct literal with only HTTP set (as the test
// helpers and any external callers do) must still work — IdentifyPlant
// falls back to HTTP when identifyHTTP is nil. The httptest server answers
// instantly so the shorter cap is harmless here.
func TestIdentifyPlant_NilIdentifyHTTP_FallsBackToHTTP(t *testing.T) {
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Ficus lyrata\",\"common_names\":[],\"confidence\":0.7}"}}]}`)
	})
	defer srv.Close()
	if c.identifyHTTP != nil {
		t.Fatalf("test helper unexpectedly set identifyHTTP; this test asserts the nil-guard path")
	}
	sug, err := c.IdentifyPlant(context.Background(), []byte("img"), "image/jpeg")
	if err != nil {
		t.Fatalf("err: %v (nil identifyHTTP must fall back to HTTP)", err)
	}
	if sug.ScientificName != "Ficus lyrata" {
		t.Errorf("ScientificName = %q, want Ficus lyrata", sug.ScientificName)
	}
}
