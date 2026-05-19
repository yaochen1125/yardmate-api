package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cannedPlantNetOK is a canned Pl@ntNet v2 /identify/all response (subset of
// fields we consume). 4 results: toIdentifyResult now returns up to 10 (the
// handler does catalog-preference selection across the full set and trims the
// RESPONSE to top-3, SPEC §2.1), so the client layer keeps all 4 here.
// results[0] carries an `images` array (include-related-images=true shape) so
// ImageURL mapping + the medium-preferred pick is exercised; results[1..] omit
// images so the nil path is covered in the same fixture.
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
      },
      "images": [
        {
          "organ": "leaf",
          "author": "Some Botanist",
          "license": "cc-by-sa",
          "url": {
            "o": "https://bs.plantnet.org/image/o/monstera.jpg",
            "m": "https://bs.plantnet.org/image/m/monstera.jpg",
            "s": "https://bs.plantnet.org/image/s/monstera.jpg"
          }
        },
        {
          "organ": "flower",
          "url": {
            "o": "https://bs.plantnet.org/image/o/monstera2.jpg",
            "m": "https://bs.plantnet.org/image/m/monstera2.jpg",
            "s": "https://bs.plantnet.org/image/s/monstera2.jpg"
          }
        }
      ]
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
        "scientificNameWithoutAuthor": "Fourth (kept by client; top-3 trim is the handler's job now)",
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
		NbResults: 10,
		HTTP:      srv.Client(),
	}
	return c, srv
}

func TestPlantNetClient_Identify_Success(t *testing.T) {
	var (
		gotCT, gotAPIKey, gotLang, gotNbResults string
		gotInclRelImages                        string
		gotImagesField, gotOrgansField          bool
		gotOrgansValue                          string
	)
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAPIKey = r.URL.Query().Get("api-key") // SPEC §1.5: api-key is a QUERY param
		gotLang = r.URL.Query().Get("lang")
		gotNbResults = r.URL.Query().Get("nb-results")
		gotInclRelImages = r.URL.Query().Get("include-related-images")
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
	// SPEC §2.1: request 10 candidates so the handler can evaluate the FULL
	// set for a curated-catalog match (catalog-preference cascade).
	if gotNbResults != "10" {
		t.Errorf("nb-results query = %q, want 10 (catalog-preference cascade)", gotNbResults)
	}
	if gotInclRelImages != "true" {
		t.Errorf("include-related-images query = %q, want true", gotInclRelImages)
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
	// toIdentifyResult now returns up to 10 (catalog-preference cascade,
	// SPEC §2.1): selection across the full set + the top-3 trim are the
	// HANDLER's job, not the client's. The 4-result fixture → 4 suggestions.
	if got, want := len(result.Suggestions), 4; got != want {
		t.Errorf("len(Suggestions) = %d, want %d (full set; handler trims to 3)", got, want)
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
	// results[0] has an images array → ImageURL is the FIRST image's MEDIUM
	// url (m preferred over o/s; image #2 ignored — only images[0] is used).
	if s0.ImageURL == nil {
		t.Fatal("Suggestions[0].ImageURL is nil; expected the related image medium URL")
	}
	if got, want := *s0.ImageURL, "https://bs.plantnet.org/image/m/monstera.jpg"; got != want {
		t.Errorf("Suggestions[0].ImageURL = %q, want %q (images[0].url.m)", got, want)
	}
	// results[1..] omit images entirely → ImageURL stays nil (JSON null).
	if result.Suggestions[1].ImageURL != nil {
		t.Errorf("Suggestions[1].ImageURL = %q, want nil (no images in fixture)", *result.Suggestions[1].ImageURL)
	}
	if result.Suggestions[2].ImageURL != nil {
		t.Errorf("Suggestions[2].ImageURL = %q, want nil (no images in fixture)", *result.Suggestions[2].ImageURL)
	}
}

// TestPlantNetClient_NewClient_NbResults10 pins the PRODUCTION default to 10
// (catalog-preference cascade, SPEC §2.1 — request 10 candidates so the
// handler can evaluate the full set for a curated-catalog match).
func TestPlantNetClient_NewClient_NbResults10(t *testing.T) {
	c := NewPlantNetClient("k")
	if c.NbResults != 10 {
		t.Errorf("NewPlantNetClient().NbResults = %d, want 10", c.NbResults)
	}
}

// TestPlantNetClient_Identify_ReturnsUpTo10 asserts toIdentifyResult passes
// the FULL candidate set (up to 10) to the handler — it no longer caps at 3.
// A 12-result upstream is capped to exactly 10 (the handler then does the
// catalog-preference selection across all 10 and trims the RESPONSE to 3).
func TestPlantNetClient_Identify_ReturnsUpTo10(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"bestMatch":"R0","results":[`)
	for i := 0; i < 12; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"score":%f,"species":{"scientificNameWithoutAuthor":"R%d","scientificName":"R%d Auth.","commonNames":[]}}`,
			1.0-float64(i)*0.05, i, i)
	}
	sb.WriteString(`],"remainingIdentificationRequests":50}`)

	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sb.String())
	})
	defer srv.Close()

	result, err := c.Identify(context.Background(),
		strings.NewReader("x"), "image/jpeg", "auto")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if got, want := len(result.Suggestions), 10; got != want {
		t.Errorf("len(Suggestions) = %d, want %d (toIdentifyResult cap raised 3→10)", got, want)
	}
	// Order preserved (no client-side reorder; the handler selects).
	if result.Suggestions[0].Name != "R0" || result.Suggestions[9].Name != "R9" {
		t.Errorf("first/last = %q/%q, want R0/R9 (input order, first 10 kept)",
			result.Suggestions[0].Name, result.Suggestions[9].Name)
	}
}

// TestPlantNetClient_Identify_ImageURLFallbackAndAbsent covers the size
// fallback (m missing → o), the no-images-array case (nil), and a
// non-http(s) url being rejected (nil) — all in one upstream response.
func TestPlantNetClient_Identify_ImageURLFallbackAndAbsent(t *testing.T) {
	const canned = `{
  "bestMatch": "A",
  "results": [
    {
      "score": 0.9,
      "species": {"scientificNameWithoutAuthor": "A", "scientificName": "A Auth.", "commonNames": []},
      "images": [ { "url": { "o": "https://bs.plantnet.org/o/a.jpg", "m": "", "s": "https://bs.plantnet.org/s/a.jpg" } } ]
    },
    {
      "score": 0.5,
      "species": {"scientificNameWithoutAuthor": "B", "scientificName": "B Auth.", "commonNames": []},
      "images": []
    },
    {
      "score": 0.3,
      "species": {"scientificNameWithoutAuthor": "C", "scientificName": "C Auth.", "commonNames": []},
      "images": [ { "url": { "o": "ftp://bad/c.jpg", "m": "  ", "s": "not-a-url" } } ]
    }
  ],
  "remainingIdentificationRequests": 100
}`
	c, srv := newTestPlantNetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, canned)
	})
	defer srv.Close()

	result, err := c.Identify(context.Background(),
		strings.NewReader("x"), "image/jpeg", "leaf")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(result.Suggestions) != 3 {
		t.Fatalf("len(Suggestions) = %d, want 3", len(result.Suggestions))
	}
	// [0]: m is empty → falls back to o (original).
	if result.Suggestions[0].ImageURL == nil {
		t.Fatal("Suggestions[0].ImageURL is nil; expected fallback to original url")
	}
	if got, want := *result.Suggestions[0].ImageURL, "https://bs.plantnet.org/o/a.jpg"; got != want {
		t.Errorf("Suggestions[0].ImageURL = %q, want %q (o fallback when m empty)", got, want)
	}
	// [1]: empty images array → nil.
	if result.Suggestions[1].ImageURL != nil {
		t.Errorf("Suggestions[1].ImageURL = %q, want nil (empty images array)", *result.Suggestions[1].ImageURL)
	}
	// [2]: no http(s) candidate (ftp / blank / bare string) → nil.
	if result.Suggestions[2].ImageURL != nil {
		t.Errorf("Suggestions[2].ImageURL = %q, want nil (no valid http(s) url)", *result.Suggestions[2].ImageURL)
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
	// 4-result fixture → 4 (client returns the full set now; handler trims).
	if !result.IsPlant || len(result.Suggestions) != 4 {
		t.Errorf("result = %+v, want IsPlant=true + 4 suggestions on 201", result)
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
