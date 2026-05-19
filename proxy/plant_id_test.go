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

// canned Plant.id v3 response (subset of fields we consume).
const cannedPlantIDOK = `{
  "access_token": "abc123",
  "model_version": "plant_id:3.4.5",
  "input": {},
  "result": {
    "is_plant": {
      "probability": 0.97,
      "binary": true,
      "threshold": 0.5
    },
    "classification": {
      "suggestions": [
        {
          "id": "abc",
          "name": "Monstera deliciosa",
          "probability": 0.94,
          "details": {
            "common_names": ["Swiss cheese plant", "Split-leaf philodendron"],
            "scientific_name": "Monstera deliciosa"
          }
        },
        {
          "id": "def",
          "name": "Other plant",
          "probability": 0.06,
          "details": {
            "common_names": [],
            "scientific_name": "Other plant"
          }
        },
        {
          "id": "ghi",
          "name": "Third",
          "probability": 0.01,
          "details": {
            "common_names": null,
            "scientific_name": "Third"
          }
        },
        {
          "id": "jkl",
          "name": "Fourth (kept by client; top-3 trim is the handler's job now)",
          "probability": 0.005,
          "details": {
            "common_names": [],
            "scientific_name": "Fourth"
          }
        }
      ]
    }
  }
}`

func newTestPlantIDClient(t *testing.T, handler http.HandlerFunc) (*PlantIDClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := &PlantIDClient{
		APIKey:   "test-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
	return c, srv
}

func TestPlantIDClient_Identify_Success(t *testing.T) {
	var (
		gotAPIKey, gotCT string
		gotImagesField   bool
	)
	c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("Api-Key")
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		if r.MultipartForm != nil {
			_, gotImagesField = r.MultipartForm.File["images"]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedPlantIDOK)
	})
	defer srv.Close()

	result, err := c.Identify(context.Background(),
		bytes.NewReader([]byte("dummy image bytes")), "image/jpeg")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("Api-Key header = %q, want %q", gotAPIKey, "test-key")
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data prefix", gotCT)
	}
	if !gotImagesField {
		t.Errorf(`multipart field must be named "images" (Plant.id v3); got false`)
	}
	if !result.IsPlant {
		t.Errorf("IsPlant = false, want true")
	}
	if got, want := result.IsPlantConfidence, 0.97; got != want {
		t.Errorf("IsPlantConfidence = %v, want %v", got, want)
	}
	// toIdentifyResult now returns up to 10 (catalog-preference cascade,
	// SPEC §2.1): the handler selects across the FULL set and trims the
	// RESPONSE to top-3 — the client layer no longer caps at 3. The 4-result
	// fixture therefore yields 4 suggestions here.
	if got, want := len(result.Suggestions), 4; got != want {
		t.Errorf("len(Suggestions) = %d, want %d (full set; handler trims to 3)", got, want)
	}
	s0 := result.Suggestions[0]
	if s0.Name != "Monstera deliciosa" || s0.ScientificName != "Monstera deliciosa" {
		t.Errorf("Suggestions[0] = %+v, want Monstera", s0)
	}
	if got, want := s0.Confidence, 0.94; got != want {
		t.Errorf("Suggestions[0].Confidence = %v, want %v", got, want)
	}
	if len(s0.CommonNames) != 2 {
		t.Errorf("Suggestions[0].CommonNames len = %d, want 2", len(s0.CommonNames))
	}
	// nil common_names normalized to empty slice
	if result.Suggestions[2].CommonNames == nil {
		t.Error("Suggestions[2].CommonNames is nil; expected normalized to []")
	}
}

func TestPlantIDClient_Identify_Unauthorized(t *testing.T) {
	c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDUnauthorized) {
		t.Errorf("err = %v, want ErrPlantIDUnauthorized", err)
	}
}

func TestPlantIDClient_Identify_RateLimit(t *testing.T) {
	c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDRateLimit) {
		t.Errorf("err = %v, want ErrPlantIDRateLimit", err)
	}
}

func TestPlantIDClient_Identify_Unavailable_5xx(t *testing.T) {
	c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDUnavailable) {
		t.Errorf("err = %v, want ErrPlantIDUnavailable", err)
	}
}

func TestPlantIDClient_Identify_ImageRejected_400(t *testing.T) {
	c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"unsupported_image"}`)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDImageRejected) {
		t.Errorf("err = %v, want ErrPlantIDImageRejected", err)
	}
}

func TestPlantIDClient_Identify_Accepts201Created(t *testing.T) {
	// Plant.id v3 returns 201 Created on success (not 200). Regression guard.
	c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, cannedPlantIDOK)
	})
	defer srv.Close()

	result, err := c.Identify(context.Background(),
		bytes.NewReader([]byte("img")), "image/jpeg")
	if err != nil {
		t.Fatalf("Identify on 201: %v", err)
	}
	if !result.IsPlant {
		t.Errorf("IsPlant = false, want true")
	}
}

func TestPlantIDClient_Identify_BadResponseJSON(t *testing.T) {
	c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{not valid json`)
	})
	defer srv.Close()
	_, err := c.Identify(context.Background(), strings.NewReader("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDBadResponse) {
		t.Errorf("err = %v, want ErrPlantIDBadResponse", err)
	}
}

// --- Diagnose ---

// cannedPlantIDDiagnoseOK is a canned Plant.id health_assessment response.
const cannedPlantIDDiagnoseOK = `{
  "result": {
    "is_plant": {"probability": 0.99, "binary": true},
    "is_healthy": {"probability": 0.12, "binary": false},
    "classification": {
      "suggestions": [
        {"name": "Monstera deliciosa", "probability": 0.96,
         "details": {"common_names": ["Swiss cheese plant"], "scientific_name": "Monstera deliciosa"}}
      ]
    },
    "disease": {
      "suggestions": [
        {"name": "Powdery mildew", "probability": 0.78,
         "details": {
           "local_name": "Powdery mildew",
           "description": "white fungal coating",
           "cause": "humid + poor airflow",
           "treatment": {
             "biological": ["neem oil"],
             "chemical": ["copper fungicide"],
             "prevention": ["increase airflow"]
           }
         }
        }
      ]
    }
  }
}`

func newTestDiagnoseClient(t *testing.T, handler http.HandlerFunc) (*PlantIDClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := &PlantIDClient{
		APIKey:           "test-key",
		Endpoint:         srv.URL,
		DiagnoseEndpoint: srv.URL,
		HTTP:             srv.Client(),
	}
	return c, srv
}

func TestPlantIDClient_Diagnose_Success(t *testing.T) {
	var (
		gotAPIKey, gotCT string
		gotBody          []byte
	)
	c, srv := newTestDiagnoseClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("Api-Key")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedPlantIDDiagnoseOK)
	})
	defer srv.Close()

	api, err := c.Diagnose(context.Background(), []byte("\xFF\xD8\xFF\xE0FAKE"), "image/jpeg")
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("Api-Key = %q", gotAPIKey)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if !strings.Contains(string(gotBody), `"images"`) || !strings.Contains(string(gotBody), `"health":"all"`) {
		t.Errorf("body = %s, want images[] + health:all", gotBody)
	}
	if api.Result.IsHealthy.Binary {
		t.Error("IsHealthy=true, want false (canned response is unhealthy)")
	}
	if got, want := len(api.Result.Disease.Suggestions), 1; got != want {
		t.Errorf("disease suggestions len = %d, want %d", got, want)
	}
}

func TestPlantIDClient_Diagnose_Accepts201Created(t *testing.T) {
	c, srv := newTestDiagnoseClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, cannedPlantIDDiagnoseOK)
	})
	defer srv.Close()
	if _, err := c.Diagnose(context.Background(), []byte("img"), "image/jpeg"); err != nil {
		t.Fatalf("Diagnose on 201: %v", err)
	}
}

func TestPlantIDClient_Diagnose_Unauthorized(t *testing.T) {
	c, srv := newTestDiagnoseClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()
	_, err := c.Diagnose(context.Background(), []byte("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDUnauthorized) {
		t.Errorf("err = %v, want ErrPlantIDUnauthorized", err)
	}
}

func TestPlantIDClient_Diagnose_ImageRejected_400(t *testing.T) {
	c, srv := newTestDiagnoseClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	defer srv.Close()
	_, err := c.Diagnose(context.Background(), []byte("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDImageRejected) {
		t.Errorf("err = %v, want ErrPlantIDImageRejected", err)
	}
}

func TestPlantIDClient_Diagnose_5xx_Unavailable(t *testing.T) {
	c, srv := newTestDiagnoseClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()
	_, err := c.Diagnose(context.Background(), []byte("x"), "image/jpeg")
	if !errors.Is(err, ErrPlantIDUnavailable) {
		t.Errorf("err = %v, want ErrPlantIDUnavailable", err)
	}
}
