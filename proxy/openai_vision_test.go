package proxy

import (
	"context"
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
