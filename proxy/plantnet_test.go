package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cannedPlantNetOK is a canned Pl@ntNet v2 /identify/all response (subset of
// fields we consume). 4 results so the top-3 cap is exercised.
const cannedPlantNetOK = `{
  "query": {"project": "all", "images": ["img"], "organs": ["leaf"]},
  "language": "en",
  "preferedReferential": "the-plant-list",
  "bestMatch": "Monstera deliciosa Liebm.",
  "results": [
    {
      "score": 0.85,
      "species": {
        "scientificNameWithoutAuthor": "Monstera deliciosa",
        "scientificName": "Monstera deliciosa Liebm.",
        "commonNames": ["Swiss cheese plant", "Split-leaf philodendron"]
      }
    },
    {
      "score": 0.10,
      "species": {
        "scientificNameWithoutAuthor": "Other plant",
        "scientificName": "Other plant Auth.",
        "commonNames": []
      }
    },
    {
      "score": 0.03,
      "species": {
        "scientificNameWithoutAuthor": "Third",
        "scientificName": "Third Auth.",
        "commonNames": null
      }
    },
    {
      "score": 0.01,
      "species": {
        "scientificNameWithoutAuthor": "Fourth (should be dropped, top-3 cap)",
        "scientificName": "Fourth Auth.",
        "commonNames": []
      }
    }
  ],
  "remainingIdentificationRequests": 499
}`

func newTestPlantNetClient(t *testing.T, handler http.HandlerFunc) (*PlantNetClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := &PlantNetClient{
		APIKey:    "test-key",
		Endpoint:  srv.URL,
		Lang:      "en",
		NbResults: 5,
		HTTP:      srv.Client(),
	}
	return c, srv
}

func TestPlantNetClient_Identify_Success(t *testing.T) {
	var (
		gotCT, gotAPIKey, gotLang, gotNbResults string
		gotImagesField, gotOrgansField          bool
		gotOrgansValue                          string
	)
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAPIKey = r.URL.Query().Get("api-key") // SPEC §1.5: api-key is a QUERY param
		gotLang = r.URL.Query().Get("lang")
		gotNbResults = r.URL.Query().Get("nb-results")
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		if r.MultipartForm != nil {
			_, gotImagesField = r.MultipartForm.File["images"]
			if vals, ok := r.MultipartForm.Value["organs"]; ok {
				gotOrgansField = true
				if len(vals) > 0 {
					gotOrgansValue = vals[0]
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedPlantNetOK)
	})
	defer srv.Close()

	result, err := c.Identify(context.Background(),
		bytes.NewReader([]byte("dummy image bytes")), "image/jpeg", "leaf")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("api-key query = %q, want test-key", gotAPIKey)
	}
	if gotLang != "en" {
		t.Errorf("lang query = %q, want en", gotLang)
	}
	if gotNbResults != "5" {
		t.Errorf("nb-results query = %q, want 5", gotNbResults)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data prefix", gotCT)
	}
	if !gotImagesField {
		t.Errorf(`multipart file field must be named "images"; got false`)
	}
	if !gotOrgansField {
		t.Errorf(`multipart must carry an "organs" text part; got false`)
	}
	if gotOrgansValue != "leaf" {
		t.Errorf("organs part = %q, want leaf", gotOrgansValue)
	}
	if !result.IsPlant {
		t.Errorf("IsPlant = false, want true")
	}
	if got, want := result.IsPlantConfidence, 0.85; got != want {
		t.Errorf("IsPlantConfidence = %v, want %v (results[0].score)", got, want)
	}
	if got, want := len(result.Suggestions), 3; got != want {
		t.Errorf("len(Suggestions) = %d, want %d (top-3 cap)", got, want)
	}
	s0 := result.Suggestions[0]
	// Name + ScientificName both use scientificNameWithoutAuthor (SPEC §2.1
	// parity with Plant.id — author-less so the catalog resolver normalizes).
	if s0.Name != "Monstera deliciosa" || s0.ScientificName != "Monstera deliciosa" {
		t.Errorf("Suggestions[0] = %+v, want author-less Monstera deliciosa", s0)
	}
	if got, want := s0.Confidence, 0.85; got != want {
		t.Errorf("Suggestions[0].Confidence = %v, want %v", got, want)
	}
	if len(s0.CommonNames) != 2 {
		t.Errorf("Suggestions[0].CommonNames len = %d, want 2", len(s0.CommonNames))
	}
	// nil commonNames normalized to empty slice (not null on the wire).
	if result.Suggestions[2].CommonNames == nil {
		t.Error("Suggestions[2].CommonNames is nil; expected normalized to []")
	}
}

func TestPlantNetClient_Identify_EmptyOrganDefaultsToAuto(t *testing.T) {
	var gotOrgansValue string
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		if r.MultipartForm != nil {
			if vals, ok := r.MultipartForm.Value["organs"]; ok && len(vals) > 0 {
				gotOrgansValue = vals[0]
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedPlantNetOK)
	})
	defer srv.Close()

	if _, err := c.Identify(context.Background(),
		strings.NewReader("img"), "image/jpeg", ""); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if gotOrgansValue != "auto" {
		t.Errorf(`organs part = %q, want "auto" (empty organ defaults)`, gotOrgansValue)
	}
}

func TestPlantNetClient_Identify_Accepts201Created(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, cannedPlantNetOK)
	})
	defer srv.Close()

	result, err := c.Identify(context.Background(),
		bytes.NewReader([]byte("img")), "image/jpeg", "flower")
	if err != nil {
		t.Fatalf("Identify on 201: %v", err)
	}
	if !result.IsPlant || len(result.Suggestions) != 3 {
		t.Errorf("result = %+v, want IsPlant=true + 3 suggestions on 201", result)
	}
}

// 404 "Species not found" is a VALID empty result (SPEC §1.4) — NOT an
// error, and must NOT trigger the Plant.id fallback.
func TestPlantNetClient_Identify_404_NoMatch_ValidEmpty(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"Not Found","message":"Species not found"}`)
	})
	defer srv.Close()

	result, err := c.Identify(context.Background(),
		strings.NewReader("x"), "image/jpeg", "auto")
	if err != nil {
		t.Fatalf("404 should be a valid empty result, got err = %v", err)
	}
	if result == nil {
		t.Fatal("result = nil, want non-nil empty result")
	}
	if !result.IsPlant {
		t.Errorf("IsPlant = false, want true (SPEC §1.4 empty result is still is_plant)")
	}
	if result.IsPlantConfidence != 0 {
		t.Errorf("IsPlantConfidence = %v, want 0", result.IsPlantConfidence)
	}
	if result.Suggestions == nil || len(result.Suggestions) != 0 {
		t.Errorf("Suggestions = %v, want empty non-nil slice", result.Suggestions)
	}
}

func TestPlantNetClient_Identify_400_ImageRejected(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"Bad Request"}`)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg", "leaf")
	if !errors.Is(err, ErrPlantNetImageRejected) {
		t.Errorf("err = %v, want ErrPlantNetImageRejected", err)
	}
}

func TestPlantNetClient_Identify_413_ImageRejected(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg", "leaf")
	if !errors.Is(err, ErrPlantNetImageRejected) {
		t.Errorf("err = %v, want ErrPlantNetImageRejected (413)", err)
	}
}

func TestPlantNetClient_Identify_401_Unauthorized(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg", "leaf")
	if !errors.Is(err, ErrPlantNetUnauthorized) {
		t.Errorf("err = %v, want ErrPlantNetUnauthorized", err)
	}
}

func TestPlantNetClient_Identify_429_RateLimit(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg", "leaf")
	if !errors.Is(err, ErrPlantNetRateLimit) {
		t.Errorf("err = %v, want ErrPlantNetRateLimit", err)
	}
}

func TestPlantNetClient_Identify_500_Unavailable(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg", "leaf")
	if !errors.Is(err, ErrPlantNetUnavailable) {
		t.Errorf("err = %v, want ErrPlantNetUnavailable", err)
	}
}

func TestPlantNetClient_Identify_BadResponseJSON(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{not valid json`)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg", "leaf")
	if !errors.Is(err, ErrPlantNetBadResponse) {
		t.Errorf("err = %v, want ErrPlantNetBadResponse", err)
	}
}

func TestPlantNetClient_Identify_UnexpectedStatus_BadResponse(t *testing.T) {
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // 418 — unmapped
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg", "leaf")
	if !errors.Is(err, ErrPlantNetBadResponse) {
		t.Errorf("err = %v, want ErrPlantNetBadResponse (unmapped status)", err)
	}
}
