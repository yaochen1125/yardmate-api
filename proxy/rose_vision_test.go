package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy/rosererank"
)

// roseImg is a non-empty fake image; RerankRose only checks len(image) > 0.
var roseImg = []byte{0xFF, 0xD8, 0xFF, 0xE0}

func newRoseVisionClient(t *testing.T, handler http.HandlerFunc) (*VisionClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	return &VisionClient{APIKey: "k", Endpoint: srv.URL, Model: "t", HTTP: srv.Client()}, srv
}

func testRoseCands() []rosererank.RoseCandidate {
	return []rosererank.RoseCandidate{
		{PlantID: "AAA1", ScientificName: "Rosa 'Peace'", CommonName: "Peace", FlowerColor: []string{"yellow", "pink"}, Description: "large yellow blooms edged pink"},
		{PlantID: "AAA2", ScientificName: "Rosa 'Blaze'", CommonName: "Blaze", FlowerColor: []string{"red"}, Description: "red climbing rose"},
	}
}

func TestRerankRose_CertainParse(t *testing.T) {
	canned := `{"choices":[{"message":{"content":"{\"cultivar_certain\":true,\"matches\":[{\"plant_id\":\"AAA1\",\"confidence\":0.82,\"reason\":\"yellow edged pink\"}]}"}}]}`
	c, srv := newRoseVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, canned)
	})
	defer srv.Close()
	res, err := c.RerankRose(context.Background(), roseImg, "image/jpeg", testRoseCands())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.CultivarCertain || len(res.Matches) != 1 || res.Matches[0].PlantID != "AAA1" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Matches[0].Confidence < 0.8 || res.Matches[0].Confidence > 0.85 {
		t.Errorf("confidence = %v, want ~0.82", res.Matches[0].Confidence)
	}
}

func TestRerankRose_NotCertain(t *testing.T) {
	canned := `{"choices":[{"message":{"content":"{\"cultivar_certain\":false,\"matches\":[]}"}}]}`
	c, srv := newRoseVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, canned)
	})
	defer srv.Close()
	res, err := c.RerankRose(context.Background(), roseImg, "image/jpeg", testRoseCands())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.CultivarCertain || len(res.Matches) != 0 {
		t.Errorf("want not-certain + empty, got %+v", res)
	}
}

func TestRerankRose_Non200_Errors(t *testing.T) {
	c, srv := newRoseVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"server"}`)
	})
	defer srv.Close()
	if _, err := c.RerankRose(context.Background(), roseImg, "image/jpeg", testRoseCands()); err == nil {
		t.Fatal("want error on 500")
	}
}

func TestRerankRose_ClampsConfidence(t *testing.T) {
	canned := `{"choices":[{"message":{"content":"{\"cultivar_certain\":true,\"matches\":[{\"plant_id\":\"AAA1\",\"confidence\":1.4,\"reason\":\"x\"},{\"plant_id\":\"AAA2\",\"confidence\":-0.2,\"reason\":\"y\"}]}"}}]}`
	c, srv := newRoseVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, canned)
	})
	defer srv.Close()
	res, err := c.RerankRose(context.Background(), roseImg, "image/jpeg", testRoseCands())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Matches[0].Confidence != 1.0 {
		t.Errorf("conf[0] = %v, want clamped 1.0", res.Matches[0].Confidence)
	}
	if res.Matches[1].Confidence != 0.0 {
		t.Errorf("conf[1] = %v, want clamped 0.0", res.Matches[1].Confidence)
	}
}

func TestRerankRose_GuardArgs(t *testing.T) {
	c := &VisionClient{Model: "t"}
	if _, err := c.RerankRose(context.Background(), roseImg, "image/jpeg", nil); err == nil {
		t.Fatal("want error on empty candidates")
	}
	if _, err := c.RerankRose(context.Background(), nil, "image/jpeg", testRoseCands()); err == nil {
		t.Fatal("want error on empty image")
	}
}
