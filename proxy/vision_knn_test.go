package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVisionKNNAgreesWithDecision(t *testing.T) {
	pid := "AAA0262"
	resp := func(topID string, in bool) *VisionKNNResponse {
		return &VisionKNNResponse{InCatalog: in, Candidates: []VisionKNNCandidate{{CatalogID: topID, VisionSim: 0.9}}}
	}
	cases := []struct {
		name string
		s0   *Suggestion
		r    *VisionKNNResponse
		want bool
	}{
		{"agrees", &Suggestion{PlantID: &pid}, resp("AAA0262", true), true},
		{"different top id", &Suggestion{PlantID: &pid}, resp("AAA0263", true), false},
		{"out-of-catalog verdict", &Suggestion{PlantID: &pid}, resp("AAA0262", false), false},
		{"decision out-of-catalog (nil PlantID)", &Suggestion{PlantID: nil}, resp("AAA0262", true), false},
		{"no candidates", &Suggestion{PlantID: &pid}, &VisionKNNResponse{InCatalog: true}, false},
		{"nil resp", &Suggestion{PlantID: &pid}, nil, false},
		{"nil s0", nil, resp("AAA0262", true), false},
	}
	for _, c := range cases {
		if got := visionKNNAgreesWithDecision(c.s0, c.r); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestVisionKNNIdentifyParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vision/identify" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"catalog_id":"AAA0262","vision_sim":0.85}],"nn_sim":0.85,"in_catalog":true,"in_catalog_confidence":0.61,"model":"bioclip-2"}`))
	}))
	defer srv.Close()

	c := NewVisionKNNClient(srv.URL)
	got, err := c.Identify(context.Background(), []byte("fakejpegbytes"), "image/jpeg")
	if err != nil {
		t.Fatalf("Identify err: %v", err)
	}
	if !got.InCatalog || len(got.Candidates) != 1 || got.Candidates[0].CatalogID != "AAA0262" || got.NNSim != 0.85 {
		t.Fatalf("bad parse: %+v", got)
	}
}

func TestVisionKNNIdentifyNon200FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("index not loaded"))
	}))
	defer srv.Close()

	c := NewVisionKNNClient(srv.URL)
	if _, err := c.Identify(context.Background(), []byte("x"), "image/jpeg"); err == nil {
		t.Fatal("expected error on 503 (handler must fail open on error)")
	}
}

func TestNewVisionKNNClientDefaultEndpoint(t *testing.T) {
	if c := NewVisionKNNClient(""); c.Endpoint != defaultVisionKNNEndpoint {
		t.Fatalf("empty endpoint should default to %s, got %s", defaultVisionKNNEndpoint, c.Endpoint)
	}
}
