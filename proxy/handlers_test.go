package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testUUID is a fixed RFC 4122 string the tests reuse for X-Device-Install-Id.
const testUUID = "ABCDEF01-2345-6789-ABCD-EF0123456789"

// jpegMagic is a valid JPEG SOI + APP0 prefix so http.DetectContentType
// returns "image/jpeg". 12 bytes is enough.
var jpegMagic = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}

func buildMultipart(t *testing.T, fieldName string, body []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(fieldName, "test.jpg")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func newIdentifyHandler(t *testing.T, upstream http.HandlerFunc) (http.Handler, *httptest.Server) {
	t.Helper()
	return newIdentifyHandlerWithVision(t, upstream, nil)
}

// newIdentifyHandlerWithVision wires HandleIdentify with an optional vision
// client (so ai_enhance tests can inject a mocked OpenAI server).
func newIdentifyHandlerWithVision(t *testing.T, upstream http.HandlerFunc, vision *VisionClient) (http.Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	c := &PlantIDClient{
		APIKey:   "test-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
	return HandleIdentify(c, vision), srv
}

func TestHandleIdentify_Success(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedPlantIDOK)
	})
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsPlant || len(result.Suggestions) != 3 {
		t.Errorf("result = %+v, want IsPlant=true + 3 suggestions", result)
	}
}

func TestHandleIdentify_MissingDeviceID(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-App-Version", "1.1.1")
	// missing X-Device-Install-Id
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_device_id"`) {
		t.Errorf("body = %s, want missing_device_id", rec.Body.String())
	}
}

func TestHandleIdentify_MissingAppVersion(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_app_version"`) {
		t.Errorf("body = %s, want missing_app_version", rec.Body.String())
	}
}

func TestHandleIdentify_BadMIME(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for bad MIME")
	})
	defer srv.Close()

	// random text, http.DetectContentType returns text/plain
	body, ct := buildMultipart(t, "image", []byte("this is not an image file, just plain text content"))
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"bad_image"`) {
		t.Errorf("body = %s, want bad_image", rec.Body.String())
	}
}

func TestHandleIdentify_MissingImageField(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	// Multipart but with wrong field name
	body, ct := buildMultipart(t, "not_image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_image"`) {
		t.Errorf("body = %s, want missing_image", rec.Body.String())
	}
}

func TestHandleIdentify_BadMultipart_WrongContentType(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/identify",
		bytes.NewReader([]byte(`{"not":"multipart"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"bad_multipart"`) {
		t.Errorf("body = %s, want bad_multipart", rec.Body.String())
	}
}

func TestHandleIdentify_ImageTooLarge(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called when body exceeds cap")
	})
	defer srv.Close()

	// Valid JPEG magic prefix (so MIME sniff passes) followed by a payload
	// that pushes the multipart body over identifyMaxBody (9 MiB). The
	// MaxBytesReader fires during multipart consumption.
	huge := make([]byte, 0, identifyMaxBody+1024)
	huge = append(huge, jpegMagic...)
	huge = append(huge, bytes.Repeat([]byte{0xAA}, identifyMaxBody+100)...)
	body, ct := buildMultipart(t, "image", huge)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"image_too_large"`) {
		t.Errorf("body = %s, want image_too_large", rec.Body.String())
	}
}

func TestHandleIdentify_PlantIDUnavailable(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id_unavailable"`) {
		t.Errorf("body = %s, want plant_id_unavailable", rec.Body.String())
	}
}

func TestHandleIdentify_PlantIDUnauthorized_MapsTo502(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id_unauthorized"`) {
		t.Errorf("body = %s, want plant_id_unauthorized", rec.Body.String())
	}
}

func TestHandleIdentify_PlantIDImageRejected_MapsToClient400(t *testing.T) {
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (Plant.id rejected, client retake)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"bad_image"`) {
		t.Errorf("body = %s, want bad_image", rec.Body.String())
	}
}

// --- HandleIdentify ai_enhance ---

// buildMultipartWithFlag builds a multipart body containing both an image
// file part and a free-form ai_enhance text part, in that order.
func buildMultipartWithFlag(t *testing.T, image []byte, flag string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("image", "test.jpg")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(image); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if flag != "" {
		if err := w.WriteField("ai_enhance", flag); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func TestHandleIdentify_AIEnhance_False_NoRerank(t *testing.T) {
	h, srv := newIdentifyHandlerWithVision(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedPlantIDOK)
	}, nil)
	defer srv.Close()
	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var result IdentifyResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result.AIEnhancedAt != nil {
		t.Errorf("AIEnhancedAt = %v, want nil (ai_enhance not requested)", *result.AIEnhancedAt)
	}
}

func TestHandleIdentify_AIEnhance_True_NoVisionClient(t *testing.T) {
	// ai_enhance=true but vision client is nil — server gracefully skips
	// the rerank and ships the Plant.id ordering unchanged.
	h, srv := newIdentifyHandlerWithVision(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedPlantIDOK)
	}, nil)
	defer srv.Close()
	body, ct := buildMultipartWithFlag(t, jpegMagic, "true")
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var result IdentifyResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result.AIEnhancedAt != nil {
		t.Errorf("AIEnhancedAt = %v, want nil (vision client absent)", *result.AIEnhancedAt)
	}
	if result.Suggestions[0].Name != "Monstera deliciosa" {
		t.Errorf("top-1 should be unchanged Plant.id top: %s", result.Suggestions[0].Name)
	}
}

func TestHandleIdentify_AIEnhance_True_RerankPromotesTopN(t *testing.T) {
	// Vision returns the 2nd candidate ("Other plant"); handler must swap
	// it into position 0 and set AIEnhancedAt.
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Other plant"}}]}`)
	}))
	defer llm.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: llm.URL, Model: "t", HTTP: llm.Client()}

	h, srv := newIdentifyHandlerWithVision(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedPlantIDOK)
	}, vision)
	defer srv.Close()
	body, ct := buildMultipartWithFlag(t, jpegMagic, "true")
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body)
	}
	var result IdentifyResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result.AIEnhancedAt == nil {
		t.Fatal("AIEnhancedAt = nil, want timestamp")
	}
	if result.Suggestions[0].Name != "Other plant" {
		t.Errorf("top-1 = %q, want \"Other plant\" (after rerank)", result.Suggestions[0].Name)
	}
}

func TestHandleIdentify_AIEnhance_True_VisionError_KeepsOriginal(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer llm.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: llm.URL, Model: "t", HTTP: llm.Client()}

	h, srv := newIdentifyHandlerWithVision(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedPlantIDOK)
	}, vision)
	defer srv.Close()
	body, ct := buildMultipartWithFlag(t, jpegMagic, "true")
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var result IdentifyResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result.AIEnhancedAt != nil {
		t.Errorf("AIEnhancedAt = %v, want nil on LLM failure", *result.AIEnhancedAt)
	}
	// Plant.id order preserved.
	if result.Suggestions[0].Name != "Monstera deliciosa" {
		t.Errorf("top-1 = %q, want \"Monstera deliciosa\" (Plant.id top-1)", result.Suggestions[0].Name)
	}
}

func TestHandleIdentify_AIEnhance_FlagAcceptsTrueAndOne(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Monstera deliciosa"}}]}`)
	}))
	defer llm.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: llm.URL, Model: "t", HTTP: llm.Client()}

	for _, flag := range []string{"true", "1", "yes"} {
		t.Run("flag="+flag, func(t *testing.T) {
			h, srv := newIdentifyHandlerWithVision(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, cannedPlantIDOK)
			}, vision)
			defer srv.Close()
			body, ct := buildMultipartWithFlag(t, jpegMagic, flag)
			req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
			req.Header.Set("Content-Type", ct)
			req.Header.Set("X-Device-Install-Id", testUUID)
			req.Header.Set("X-App-Version", "1.1.1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			var result IdentifyResult
			_ = json.Unmarshal(rec.Body.Bytes(), &result)
			if result.AIEnhancedAt == nil {
				t.Errorf("flag=%q: AIEnhancedAt should be non-nil", flag)
			}
		})
	}
}

// --- HandleDiagnose ---

func newDiagnoseHandler(t *testing.T, upstream http.HandlerFunc, vision *VisionClient) (http.Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	c := &PlantIDClient{
		APIKey:           "test-key",
		Endpoint:         srv.URL,
		DiagnoseEndpoint: srv.URL,
		HTTP:             srv.Client(),
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	return HandleDiagnose(c, content, vision), srv
}

// cannedDiagnoseHealthy mimics Plant.id when the plant is healthy.
const cannedDiagnoseHealthy = `{
  "result": {
    "is_plant": {"probability": 0.99, "binary": true},
    "is_healthy": {"probability": 0.92, "binary": true},
    "classification": {
      "suggestions": [
        {"name": "Abelia chinensis", "probability": 0.94,
         "details": {"common_names": ["Chinese Abelia"], "scientific_name": "Abelia chinensis"}}
      ]
    },
    "disease": {"suggestions": []}
  }
}`

// cannedDiagnoseUnhealthy has Plant.id reporting Powdery mildew (maps to L20).
const cannedDiagnoseUnhealthy = `{
  "result": {
    "is_plant": {"probability": 0.98, "binary": true},
    "is_healthy": {"probability": 0.20, "binary": false},
    "classification": {
      "suggestions": [
        {"name": "Abelia chinensis", "probability": 0.92,
         "details": {"common_names": ["Chinese Abelia"], "scientific_name": "Abelia chinensis"}}
      ]
    },
    "disease": {
      "suggestions": [
        {"name": "Powdery mildew", "probability": 0.76,
         "details": {
           "local_name": "Powdery mildew",
           "description": "white powdery coating",
           "cause": "high humidity",
           "treatment": {"biological": ["neem"], "chemical": ["copper"], "prevention": ["airflow"]}
         }
        }
      ]
    }
  }
}`

// cannedDiagnoseUnhealthyEmpty — Plant.id says unhealthy but returns zero
// disease suggestions; server must fall back to plant common_diseases_list[0]
// (AAA0001 → R01 "Root rot").
const cannedDiagnoseUnhealthyEmpty = `{
  "result": {
    "is_plant": {"probability": 0.99, "binary": true},
    "is_healthy": {"probability": 0.15, "binary": false},
    "classification": {
      "suggestions": [
        {"name": "Abelia chinensis", "probability": 0.90,
         "details": {"common_names": ["Chinese Abelia"], "scientific_name": "Abelia chinensis"}}
      ]
    },
    "disease": {"suggestions": []}
  }
}`

func TestHandleDiagnose_Healthy_EmptyIssues(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedDiagnoseHealthy)
	}, nil)
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body)
	}
	var result DiagnoseResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsHealthy {
		t.Error("IsHealthy = false, want true")
	}
	if len(result.Issues) != 0 {
		t.Errorf("Issues len = %d, want 0 (healthy)", len(result.Issues))
	}
	if result.PlantID == nil || *result.PlantID != "AAA0001" {
		t.Errorf("PlantID = %v, want AAA0001", result.PlantID)
	}
	if result.Top == nil || result.Top.Name != "Abelia chinensis" {
		t.Errorf("Top = %+v, want Abelia chinensis", result.Top)
	}
	if result.IdentifiedName != "Abelia chinensis" {
		t.Errorf("IdentifiedName = %q", result.IdentifiedName)
	}
}

func TestHandleDiagnose_Unhealthy_TopIssueMapsToCatalog(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedDiagnoseUnhealthy)
	}, nil)
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body)
	}
	var result DiagnoseResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result.IsHealthy {
		t.Error("IsHealthy = true, want false")
	}
	if len(result.Issues) != 1 {
		t.Fatalf("Issues len = %d, want 1", len(result.Issues))
	}
	issue := result.Issues[0]
	if issue.Name != "Powdery mildew" {
		t.Errorf("Issue Name = %q, want Powdery mildew", issue.Name)
	}
	if issue.CatalogID == nil || *issue.CatalogID != "L20" {
		t.Errorf("CatalogID = %v, want L20 (name-match)", issue.CatalogID)
	}
	if issue.IsFallback {
		t.Error("IsFallback = true, want false (Plant.id provided the issue)")
	}
	if got := issue.Treatment.Biological; len(got) != 1 || got[0] != "neem" {
		t.Errorf("Treatment.Biological = %v, want [neem]", got)
	}
}

func TestHandleDiagnose_Unhealthy_EmptySuggestions_FallbackFromPlant(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedDiagnoseUnhealthyEmpty)
	}, nil)
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body)
	}
	var result DiagnoseResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if result.IsHealthy {
		t.Error("IsHealthy = true, want false")
	}
	if len(result.Issues) != 1 {
		t.Fatalf("Issues len = %d, want 1 (fallback)", len(result.Issues))
	}
	issue := result.Issues[0]
	if !issue.IsFallback {
		t.Error("IsFallback = false, want true (Plant.id returned empty suggestions)")
	}
	if issue.CatalogID == nil || *issue.CatalogID != "R01" {
		t.Errorf("Fallback CatalogID = %v, want R01 (plant common_diseases_list[0])", issue.CatalogID)
	}
	if issue.Name != "Root rot" {
		t.Errorf("Fallback Name = %q, want Root rot", issue.Name)
	}
}

func TestHandleDiagnose_MissingDeviceID_400(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called")
	}, nil)
	defer srv.Close()
	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_device_id"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHandleDiagnose_MissingAppVersion_400(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called")
	}, nil)
	defer srv.Close()
	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_app_version"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHandleDiagnose_BadMIME_400(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called")
	}, nil)
	defer srv.Close()
	body, ct := buildMultipart(t, "image", []byte("not an image at all just plain text"))
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"bad_image"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHandleDiagnose_UpstreamUnavailable_502(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)
	defer srv.Close()
	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id_unavailable"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// TestHandleDiagnose_CatalogID_LLMDisambiguation exercises the fallback path
// where Plant.id returns a disease name that does not match any catalog name,
// and the vision client (mocked) maps it to L02.
func TestHandleDiagnose_CatalogID_LLMDisambiguation(t *testing.T) {
	upstreamResp := strings.Replace(cannedDiagnoseUnhealthy, "Powdery mildew", "Phaeoramularia leaf-spot disease", -1)

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"L02"}}]}`)
	}))
	defer llm.Close()
	vision := &VisionClient{
		APIKey:   "test",
		Endpoint: llm.URL,
		Model:    "test-model",
		HTTP:     llm.Client(),
	}

	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, upstreamResp)
	}, vision)
	defer srv.Close()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body)
	}
	var result DiagnoseResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)
	if len(result.Issues) != 1 {
		t.Fatalf("Issues len = %d, want 1", len(result.Issues))
	}
	issue := result.Issues[0]
	if issue.CatalogID == nil || *issue.CatalogID != "L02" {
		t.Errorf("CatalogID = %v, want L02 (via LLM disambiguation)", issue.CatalogID)
	}
}

func TestIsUUID(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{testUUID, true},
		{"abcdef01-2345-6789-abcd-ef0123456789", true}, // lowercase ok
		{"abc", false},
		{"", false},
		{"ABCDEF01-2345-6789-ABCD-EF012345678X", false}, // bad hex
		{"ABCDEF01-2345-6789ABCD-EF0123456789", false},  // missing dash
		{strings.Repeat("a", 36), false},                // no dashes
	}
	for _, tc := range tests {
		if got := isUUID(tc.s); got != tc.want {
			t.Errorf("isUUID(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
