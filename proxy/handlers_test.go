package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
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
// client (so ai_enhance tests can inject a mocked OpenAI server). plantNet is
// nil here so the cascade runs Plant.id-only (the existing test corpus
// asserts Plant.id behavior unchanged). Pl@ntNet-primary + cascade behavior
// is covered by the dedicated tests further down.
func newIdentifyHandlerWithVision(t *testing.T, upstream http.HandlerFunc, vision *VisionClient) (http.Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	c := &PlantIDClient{
		APIKey:   "test-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	return HandleIdentify(nil, c, content, vision), srv
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

// TestHandleIdentify_ResolvesPlantID covers SPEC §2.1 "plant_id mapping":
// a catalog scientific_name resolves to its YardMate plantId (same resolver
// /v1/diagnose uses), a name outside the 1522 catalog stays null, and the
// wire key is snake_case `plant_id`. "Abelia chinensis" → "AAA0001" is the
// same fixture content_test.go + the diagnose plantId test rely on.
func TestHandleIdentify_ResolvesPlantID(t *testing.T) {
	const canned = `{
  "result": {
    "is_plant": {"probability": 0.98, "binary": true},
    "classification": {
      "suggestions": [
        {"name": "Abelia chinensis", "probability": 0.91,
         "details": {"common_names": ["Chinese Abelia"], "scientific_name": "Abelia chinensis"}},
        {"name": "Zzzz nonexistent", "probability": 0.04,
         "details": {"common_names": [], "scientific_name": "Zzzz nonexistent plantii"}}
      ]
    }
  }
}`
	h, srv := newIdentifyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, canned)
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
	if len(result.Suggestions) != 2 {
		t.Fatalf("suggestions = %d, want 2", len(result.Suggestions))
	}
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != "AAA0001" {
		t.Errorf("suggestions[0].PlantID = %v, want AAA0001 (Abelia chinensis)", result.Suggestions[0].PlantID)
	}
	if result.Suggestions[1].PlantID != nil {
		t.Errorf("suggestions[1].PlantID = %q, want nil (catalog miss)", *result.Suggestions[1].PlantID)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id":"AAA0001"`) {
		t.Errorf("wire body missing \"plant_id\":\"AAA0001\"; body=%s", rec.Body.String())
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

// --- HandleIdentify two-engine cascade (SPEC §1.1 / §7) ---

// newCascadeHandler wires HandleIdentify with BOTH a Pl@ntNet fake (primary)
// and a Plant.id fake (fallback). Either upstream may be nil → that engine's
// client is nil (Plant.id-only / Pl@ntNet-only). Returns both httptest
// servers so the caller can assert call counts via closures.
func newCascadeHandler(t *testing.T, plantNetUp, plantIDUp http.HandlerFunc) (http.Handler, func()) {
	t.Helper()
	var (
		pnClient *PlantNetClient
		piClient *PlantIDClient
		closers  []func()
	)
	if plantNetUp != nil {
		pnSrv := httptest.NewServer(plantNetUp)
		closers = append(closers, pnSrv.Close)
		pnClient = &PlantNetClient{
			APIKey: "test-key", Endpoint: pnSrv.URL,
			Lang: "en", NbResults: 10, HTTP: pnSrv.Client(),
		}
	}
	if plantIDUp != nil {
		piSrv := httptest.NewServer(plantIDUp)
		closers = append(closers, piSrv.Close)
		piClient = &PlantIDClient{
			APIKey: "test-key", Endpoint: piSrv.URL, HTTP: piSrv.Client(),
		}
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	cleanup := func() {
		for _, c := range closers {
			c()
		}
	}
	return HandleIdentify(pnClient, piClient, content, nil), cleanup
}

func doCascadeReq(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// cannedPlantNetIdentifyOK — minimal Pl@ntNet success used by cascade tests
// (a catalog scientific name so the handler's plantId resolver also runs).
const cannedPlantNetIdentifyOK = `{
  "bestMatch": "Abelia chinensis",
  "results": [
    {"score": 0.91, "species": {
      "scientificNameWithoutAuthor": "Abelia chinensis",
      "scientificName": "Abelia chinensis R.Br.",
      "commonNames": ["Chinese Abelia"]}}
  ],
  "remainingIdentificationRequests": 480
}`

// (a) Pl@ntNet primary success → Plant.id is NOT called.
func TestHandleIdentify_Cascade_PlantNetPrimarySuccess(t *testing.T) {
	plantIDCalled := false
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, cannedPlantNetIdentifyOK)
		},
		func(w http.ResponseWriter, r *http.Request) {
			plantIDCalled = true
			t.Error("Plant.id fallback must NOT be called on Pl@ntNet success")
		})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if plantIDCalled {
		t.Error("Plant.id was called; want Pl@ntNet-only")
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsPlant || len(result.Suggestions) != 1 {
		t.Fatalf("result = %+v, want IsPlant=true + 1 suggestion", result)
	}
	if result.Suggestions[0].Name != "Abelia chinensis" {
		t.Errorf("Suggestions[0].Name = %q, want Abelia chinensis", result.Suggestions[0].Name)
	}
	// Per-suggestion plantId resolver still runs on the Pl@ntNet path.
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != "AAA0001" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0001", result.Suggestions[0].PlantID)
	}
}

// (b) Pl@ntNet 5xx → Plant.id fallback success.
func TestHandleIdentify_Cascade_PlantNet5xx_FallsBackToPlantID(t *testing.T) {
	plantIDCalled := false
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, r *http.Request) {
			plantIDCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, cannedPlantIDOK)
		})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback), body=%s", rec.Code, rec.Body.String())
	}
	if !plantIDCalled {
		t.Error("Plant.id fallback was NOT called after Pl@ntNet 5xx")
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// cannedPlantIDOK top-1 is Monstera deliciosa — proves the Plant.id
	// response (not Pl@ntNet) is what got served.
	if result.Suggestions[0].Name != "Monstera deliciosa" {
		t.Errorf("Suggestions[0].Name = %q, want Monstera deliciosa (Plant.id fallback)", result.Suggestions[0].Name)
	}
}

// (c) Pl@ntNet 404 "no match" → NO fallback; 200 with empty suggestions.
func TestHandleIdentify_Cascade_PlantNet404_NoFallback_EmptyResult(t *testing.T) {
	plantIDCalled := false
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"Not Found","message":"Species not found"}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			plantIDCalled = true
			t.Error("Plant.id must NOT be called on a Pl@ntNet 404 no-match")
		})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (valid empty), body=%s", rec.Code, rec.Body.String())
	}
	if plantIDCalled {
		t.Error("Plant.id was called; a Pl@ntNet 404 is a valid empty result")
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsPlant {
		t.Errorf("IsPlant = false, want true (SPEC §1.4 empty result still is_plant)")
	}
	if len(result.Suggestions) != 0 {
		t.Errorf("Suggestions len = %d, want 0 (no match)", len(result.Suggestions))
	}
}

// (d) Pl@ntNet image-rejected (400) → 400 bad_image, NO fallback.
func TestHandleIdentify_Cascade_PlantNetImageRejected_NoFallback_400(t *testing.T) {
	plantIDCalled := false
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		},
		func(w http.ResponseWriter, r *http.Request) {
			plantIDCalled = true
			t.Error("Plant.id must NOT be called when Pl@ntNet rejects the image")
		})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 bad_image", rec.Code)
	}
	if plantIDCalled {
		t.Error("Plant.id was called; image-rejected must not fall back")
	}
	if !strings.Contains(rec.Body.String(), `"bad_image"`) {
		t.Errorf("body = %s, want bad_image", rec.Body.String())
	}
}

// (e) Pl@ntNet down + Plant.id down → 502 plant_id_unavailable (both engines
// failed; wire code unchanged per SPEC §3).
func TestHandleIdentify_Cascade_BothEnginesDown_502(t *testing.T) {
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id_unavailable"`) {
		t.Errorf("body = %s, want plant_id_unavailable", rec.Body.String())
	}
}

// Pl@ntNet auth-fail + Plant.id auth-fail → plant_id_unauthorized (502).
func TestHandleIdentify_Cascade_BothUnauthorized_502Unauthorized(t *testing.T) {
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id_unauthorized"`) {
		t.Errorf("body = %s, want plant_id_unauthorized", rec.Body.String())
	}
}

// (f) `organ` form field is forwarded to the Pl@ntNet engine.
func TestHandleIdentify_Cascade_OrganForwardedToPlantNet(t *testing.T) {
	gotOrgan := ""
	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		if r.MultipartForm != nil {
			if vals, ok := r.MultipartForm.Value["organs"]; ok && len(vals) > 0 {
				gotOrgan = vals[0]
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedPlantNetIdentifyOK)
	}))
	defer pnSrv.Close()
	pn := &PlantNetClient{
		APIKey: "k", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client(),
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	h := HandleIdentify(pn, nil, content, nil)

	// Build a multipart body with image + organ=flower.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image", "test.jpg")
	_, _ = fw.Write(jpegMagic)
	if err := w.WriteField("organ", "flower"); err != nil {
		t.Fatalf("WriteField organ: %v", err)
	}
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/identify", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotOrgan != "flower" {
		t.Errorf("Pl@ntNet organs part = %q, want flower (forwarded from form field)", gotOrgan)
	}
}

// An unrecognized `organ` value falls back to "auto" (SPEC §2.1).
func TestHandleIdentify_Cascade_UnknownOrganDefaultsAuto(t *testing.T) {
	gotOrgan := ""
	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		if r.MultipartForm != nil {
			if vals, ok := r.MultipartForm.Value["organs"]; ok && len(vals) > 0 {
				gotOrgan = vals[0]
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedPlantNetIdentifyOK)
	}))
	defer pnSrv.Close()
	pn := &PlantNetClient{
		APIKey: "k", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client(),
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	h := HandleIdentify(pn, nil, content, nil)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image", "test.jpg")
	_, _ = fw.Write(jpegMagic)
	_ = w.WriteField("organ", "wingding")
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/identify", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotOrgan != "auto" {
		t.Errorf("Pl@ntNet organs part = %q, want auto (unknown organ → auto)", gotOrgan)
	}
}

// --- HandleIdentify tier-3 AI-vision fallback (SPEC §1.1 / §2.1 / §7) ---

// newCascadeHandlerWithVision is newCascadeHandler plus an injected vision
// client (the base helper hard-codes nil). Either upstream may be nil.
func newCascadeHandlerWithVision(t *testing.T, plantNetUp, plantIDUp http.HandlerFunc, vision *VisionClient) (http.Handler, func()) {
	t.Helper()
	var (
		pnClient *PlantNetClient
		piClient *PlantIDClient
		closers  []func()
	)
	if plantNetUp != nil {
		pnSrv := httptest.NewServer(plantNetUp)
		closers = append(closers, pnSrv.Close)
		pnClient = &PlantNetClient{
			APIKey: "test-key", Endpoint: pnSrv.URL,
			Lang: "en", NbResults: 10, HTTP: pnSrv.Client(),
		}
	}
	if plantIDUp != nil {
		piSrv := httptest.NewServer(plantIDUp)
		closers = append(closers, piSrv.Close)
		piClient = &PlantIDClient{
			APIKey: "test-key", Endpoint: piSrv.URL, HTTP: piSrv.Client(),
		}
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	cleanup := func() {
		for _, c := range closers {
			c()
		}
	}
	return HandleIdentify(pnClient, piClient, content, vision), cleanup
}

// cannedPlantNetNoMatch — Pl@ntNet 404 "no match" canned upstream (a VALID
// empty result, NOT an engine failure → no Plant.id fallback → tier-3 AI
// vision fires).
const cannedPlantNetNoMatch = `{"error":"Not Found","message":"Species not found"}`

// (g) Pl@ntNet no-match (200/empty) + no Plant.id + vision present →
// 200 with the AI-vision suggestion; engine path is ai-vision-fallback;
// the in-catalog AI species resolves to its YardMate plant_id (AAA0001).
func TestHandleIdentify_Tier3_PlantNetEmpty_VisionFills(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Abelia chinensis\",\"common_names\":[\"Chinese Abelia\"],\"confidence\":0.71}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, cannedPlantNetNoMatch)
		},
		nil, // no Plant.id (unfunded) — cascade yields zero suggestions
		vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsPlant {
		t.Errorf("IsPlant = false, want true (AI fallback always is_plant)")
	}
	if len(result.Suggestions) != 1 {
		t.Fatalf("Suggestions len = %d, want 1 (AI fallback)", len(result.Suggestions))
	}
	s := result.Suggestions[0]
	if s.Name != "Abelia chinensis" || s.ScientificName != "Abelia chinensis" {
		t.Errorf("Suggestion name/scientific = %q/%q, want Abelia chinensis", s.Name, s.ScientificName)
	}
	if s.Confidence != 0.71 {
		t.Errorf("Confidence = %v, want 0.71 (AI's own confidence, not rewritten)", s.Confidence)
	}
	if result.IsPlantConfidence != 0.71 {
		t.Errorf("IsPlantConfidence = %v, want 0.71 (from AI suggestion)", result.IsPlantConfidence)
	}
	// Existing downstream resolver still runs on the AI path.
	if s.PlantID == nil || *s.PlantID != "AAA0001" {
		t.Errorf("PlantID = %v, want AAA0001 (in-catalog AI species resolved)", s.PlantID)
	}
	if s.ImageURL != nil {
		t.Errorf("ImageURL = %v, want nil (AI path has no reference image)", s.ImageURL)
	}
	// AI fallback is NOT an ai_enhance rerank — AIEnhancedAt stays null.
	if result.AIEnhancedAt != nil {
		t.Errorf("AIEnhancedAt = %v, want nil (tier-3 is not a rerank)", *result.AIEnhancedAt)
	}
}

// (h) Pl@ntNet 5xx → Plant.id fallback returns a valid no-match (empty
// suggestions) → tier-3 AI vision fires (cascade succeeded but zero
// suggestions). Out-of-catalog AI species → plant_id stays null.
func TestHandleIdentify_Tier3_PlantIDFallbackEmpty_VisionFills(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Notaplant fakeium\",\"common_names\":[],\"confidence\":0.42}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	// Plant.id 200 with zero classification suggestions = valid no-match.
	const plantIDEmpty = `{"result":{"is_plant":{"probability":0.3,"binary":false},"classification":{"suggestions":[]}}}`

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError) // Pl@ntNet 5xx → fall back
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, plantIDEmpty)
		},
		vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Suggestions) != 1 || result.Suggestions[0].Name != "Notaplant fakeium" {
		t.Fatalf("Suggestions = %+v, want 1 AI suggestion 'Notaplant fakeium'", result.Suggestions)
	}
	if result.Suggestions[0].PlantID != nil {
		t.Errorf("PlantID = %v, want nil (out-of-catalog AI species)", result.Suggestions[0].PlantID)
	}
}

// (i) Pl@ntNet no-match + vision present but vision ERRORS → 200 with empty
// suggestions (UNCHANGED "can't identify" behavior; no crash, wire code
// unchanged).
func TestHandleIdentify_Tier3_VisionError_KeepsEmptyResult(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // vision down
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, cannedPlantNetNoMatch)
		},
		nil,
		vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unchanged can't-identify), body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsPlant {
		t.Errorf("IsPlant = false, want true (Pl@ntNet empty result still is_plant)")
	}
	if len(result.Suggestions) != 0 {
		t.Errorf("Suggestions len = %d, want 0 (vision error → unchanged empty result)", len(result.Suggestions))
	}
}

// (j) Pl@ntNet no-match + vision==nil (no OPENAI key) → 200 empty
// suggestions, exactly as before tier-3 existed (graceful degrade).
func TestHandleIdentify_Tier3_VisionNil_UnchangedBehavior(t *testing.T) {
	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, cannedPlantNetNoMatch)
		},
		nil,
		nil) // vision == nil
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsPlant || len(result.Suggestions) != 0 {
		t.Errorf("result = %+v, want IsPlant=true + 0 suggestions (vision nil → unchanged)", result)
	}
}

// (k) Both engines DOWN (Pl@ntNet 5xx + Plant.id 5xx) + vision present →
// STILL 502 plant_id_unavailable. Tier-3 must NOT mask an upstream error
// (it only fires when the cascade SUCCEEDED with zero suggestions); wire
// code unchanged.
func TestHandleIdentify_Tier3_BothEnginesDown_StillReturns502(t *testing.T) {
	visionCalled := false
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionCalled = true
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"X\",\"common_names\":[],\"confidence\":0.9}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (both engines down, tier-3 must not mask)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id_unavailable"`) {
		t.Errorf("body = %s, want plant_id_unavailable (wire code unchanged)", rec.Body.String())
	}
	if visionCalled {
		t.Error("vision was called on a both-engines-down error; tier-3 must only fire on a SUCCESSFUL empty cascade")
	}
}

// --- HandleIdentify catalog-preference selection cascade (SPEC §1.1/§2.1/§7) ---
//
// One test per decision-table row. The selection runs across the FULL
// PlantNet/Plant.id candidate set (up to 10), prefers an in-catalog match
// (rule B = highest engine confidence among in-catalog), then AI catalog-
// recovery (conf ≥ 0.55), then raw out-of-catalog fallbacks. Response is
// trimmed to top-3 with the chosen candidate at [0]. iOS contract unchanged.

// catalogScientificName is a name guaranteed in the embedded 1522 catalog
// (Abelia chinensis → AAA0001, the same fixture the other identify tests use).
// Abelia floribunda → AAA0002 is a SECOND in-catalog name for the
// multiple-in-catalog rule-B test.

// (a) A LOWER-ranked PlantNet candidate is in the catalog → it is promoted to
// Suggestions[0] with its plant_id; the higher-ranked out-of-catalog top is
// demoted. engine=plantnet-catalog.
func TestHandleIdentify_CatalogPref_LowerRankedInCatalogBecomesTop(t *testing.T) {
	// results[0] out-of-catalog (highest score 0.90), results[1] IN catalog
	// (Abelia chinensis, lower score 0.40), results[2] out-of-catalog.
	const pn = `{
  "bestMatch": "Fakeplant nonexistus",
  "results": [
    {"score": 0.90, "species": {"scientificNameWithoutAuthor": "Fakeplant nonexistus",
      "scientificName": "Fakeplant nonexistus Auth.", "commonNames": []}},
    {"score": 0.40, "species": {"scientificNameWithoutAuthor": "Abelia chinensis",
      "scientificName": "Abelia chinensis R.Br.", "commonNames": ["Chinese Abelia"]}},
    {"score": 0.20, "species": {"scientificNameWithoutAuthor": "Otherfake speciesum",
      "scientificName": "Otherfake speciesum Auth.", "commonNames": []}}
  ],
  "remainingIdentificationRequests": 480
}`
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, pn)
		}, nil)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Selected [0] must be the in-catalog candidate (not the higher-score
	// out-of-catalog one) with its plant_id resolved (rule B accepted
	// tradeoff: low-score in-catalog overrides higher-score out-of-catalog).
	if result.Suggestions[0].ScientificName != "Abelia chinensis" {
		t.Errorf("Suggestions[0] = %q, want Abelia chinensis (in-catalog promoted)", result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != "AAA0001" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0001", result.Suggestions[0].PlantID)
	}
	// Response trimmed to ≤3, the demoted out-of-catalog ones survive after [0].
	if len(result.Suggestions) != 3 {
		t.Errorf("len(Suggestions) = %d, want 3 (trimmed)", len(result.Suggestions))
	}
}

// (b) MULTIPLE candidates in catalog → the one with the HIGHEST engine
// confidence wins (rule B). engine=plantnet-catalog.
func TestHandleIdentify_CatalogPref_MultipleInCatalog_HighestConfidenceWins(t *testing.T) {
	// Two in-catalog: Abelia floribunda (AAA0002, score 0.55) and Abelia
	// chinensis (AAA0001, score 0.30). Highest-confidence in-catalog =
	// floribunda even though chinensis appears first.
	const pn = `{
  "bestMatch": "Abelia chinensis",
  "results": [
    {"score": 0.30, "species": {"scientificNameWithoutAuthor": "Abelia chinensis",
      "scientificName": "Abelia chinensis R.Br.", "commonNames": ["Chinese Abelia"]}},
    {"score": 0.80, "species": {"scientificNameWithoutAuthor": "Outofcatalog fakeum",
      "scientificName": "Outofcatalog fakeum Auth.", "commonNames": []}},
    {"score": 0.55, "species": {"scientificNameWithoutAuthor": "Abelia floribunda",
      "scientificName": "Abelia floribunda M.Martens", "commonNames": ["Mexican Abelia"]}}
  ],
  "remainingIdentificationRequests": 470
}`
	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, pn)
		}, nil)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Suggestions[0].ScientificName != "Abelia floribunda" {
		t.Errorf("Suggestions[0] = %q, want Abelia floribunda (highest-confidence in-catalog, rule B)", result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != "AAA0002" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0002 (Abelia floribunda)", result.Suggestions[0].PlantID)
	}
}

// (c) NO candidate in catalog + AI returns a catalog hit with conf ≥ 0.55 →
// ai-catalog-recovery; the AI suggestion becomes the sole in-catalog result.
func TestHandleIdentify_CatalogPref_NoneInCatalog_AICatalogRecovery(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Abelia chinensis\",\"common_names\":[\"Chinese Abelia\"],\"confidence\":0.62}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	// Both PlantNet candidates out-of-catalog → triggers AI catalog-recovery.
	const pn = `{
  "bestMatch": "Fakeone speciesum",
  "results": [
    {"score": 0.70, "species": {"scientificNameWithoutAuthor": "Fakeone speciesum",
      "scientificName": "Fakeone speciesum Auth.", "commonNames": []}},
    {"score": 0.20, "species": {"scientificNameWithoutAuthor": "Faketwo speciesum",
      "scientificName": "Faketwo speciesum Auth.", "commonNames": []}}
  ],
  "remainingIdentificationRequests": 460
}`
	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, pn)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Suggestions) != 1 {
		t.Fatalf("len(Suggestions) = %d, want 1 (AI catalog-recovery replaces set)", len(result.Suggestions))
	}
	s := result.Suggestions[0]
	if s.ScientificName != "Abelia chinensis" {
		t.Errorf("Suggestions[0] = %q, want Abelia chinensis (AI catalog-recovery)", s.ScientificName)
	}
	if s.PlantID == nil || *s.PlantID != "AAA0001" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0001", s.PlantID)
	}
	if s.Confidence != 0.62 {
		t.Errorf("Confidence = %v, want 0.62 (AI's own, not rewritten)", s.Confidence)
	}
	if result.IsPlantConfidence != 0.62 {
		t.Errorf("IsPlantConfidence = %v, want 0.62", result.IsPlantConfidence)
	}
	if result.AIEnhancedAt != nil {
		t.Errorf("AIEnhancedAt = %v, want nil (catalog-recovery is not a rerank)", *result.AIEnhancedAt)
	}
}

// (d) NO candidate in catalog + engine's ORIGINAL TOP is highly confident
// (score 0.88 ≥ plantnetConfidentSkipAIConfidence 0.80) → the AI catalog-
// recovery probe is SKIPPED entirely; the engine's original top is kept
// (out-of-catalog, plant_id null → iOS enrichment). engine=plantnet-
// confident-oob. (Change 1: this fixture used to exercise the AI-low-conf
// rejection path; under the new ordering a confident engine top short-
// circuits before vision is ever called — vision must NOT be hit.)
func TestHandleIdentify_CatalogPref_ConfidentTop_SkipsAI_EngineTopUsed(t *testing.T) {
	visionCalled := false
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If reached this would (under the new 0.10 floor) be ACCEPTED as a
		// catalog recovery — so reaching it at all is the bug we guard against.
		visionCalled = true
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Abelia chinensis\",\"common_names\":[],\"confidence\":0.40}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	const pn = `{
  "bestMatch": "Engineschoice fakeum",
  "results": [
    {"score": 0.88, "species": {"scientificNameWithoutAuthor": "Engineschoice fakeum",
      "scientificName": "Engineschoice fakeum Auth.", "commonNames": ["Engine Top"]}},
    {"score": 0.12, "species": {"scientificNameWithoutAuthor": "Secondfake speciesum",
      "scientificName": "Secondfake speciesum Auth.", "commonNames": []}}
  ],
  "remainingIdentificationRequests": 450
}`
	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, pn)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// AI must have been SKIPPED — a confident engine top (0.88 ≥ 0.80) +
	// none-in-catalog short-circuits before the vision probe.
	if visionCalled {
		t.Error("vision was called; a confident engine top (≥0.80) + none-in-catalog must SKIP the AI catalog-recovery probe")
	}
	// Engine's ORIGINAL top kept as-is (out-of-catalog → iOS enrichment).
	if result.Suggestions[0].ScientificName != "Engineschoice fakeum" {
		t.Errorf("Suggestions[0] = %q, want Engineschoice fakeum (confident engine top kept)", result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID != nil {
		t.Errorf("Suggestions[0].PlantID = %v, want nil (out-of-catalog → enrichment)", result.Suggestions[0].PlantID)
	}
	// AI's "Abelia chinensis" must NOT have leaked in anywhere (probe skipped).
	for i, s := range result.Suggestions {
		if s.ScientificName == "Abelia chinensis" {
			t.Errorf("Suggestions[%d] = Abelia chinensis; AI probe was skipped, its guess must not appear", i)
		}
	}
}

// (e) PlantNet returns ZERO suggestions + AI returns a NON-catalog guess →
// ai-raw-oob: the AI guess is the single out-of-catalog suggestion (preserves
// the "always a result" guarantee #18). plant_id null.
func TestHandleIdentify_CatalogPref_EngineZero_AINonCatalog_AIRawOOB(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Notincatalog fakeium\",\"common_names\":[],\"confidence\":0.33}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound) // PlantNet valid no-match (zero)
			_, _ = io.WriteString(w, cannedPlantNetNoMatch)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Suggestions) != 1 || result.Suggestions[0].ScientificName != "Notincatalog fakeium" {
		t.Fatalf("Suggestions = %+v, want 1 AI suggestion 'Notincatalog fakeium' (ai-raw-oob)", result.Suggestions)
	}
	if result.Suggestions[0].PlantID != nil {
		t.Errorf("PlantID = %v, want nil (out-of-catalog AI guess)", result.Suggestions[0].PlantID)
	}
	if result.Suggestions[0].Confidence != 0.33 {
		t.Errorf("Confidence = %v, want 0.33 (low conf still used because engine returned zero)", result.Suggestions[0].Confidence)
	}
}

// (f) PlantNet ZERO + vision == nil → empty suggestions, iOS "can't
// identify" (unchanged behavior; graceful degrade).
func TestHandleIdentify_CatalogPref_EngineZero_VisionNil_Empty(t *testing.T) {
	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, cannedPlantNetNoMatch)
		}, nil, nil) // vision nil
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsPlant || len(result.Suggestions) != 0 {
		t.Errorf("result = %+v, want IsPlant=true + 0 suggestions (vision nil → unchanged)", result)
	}
}

// (g) BOTH engines unavailable → still 502 plant_id_unavailable; vision is
// NOT called (AI must not substitute for engine-unavailable, locked #18).
func TestHandleIdentify_CatalogPref_BothEnginesDown_502_VisionNotCalled(t *testing.T) {
	visionCalled := false
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionCalled = true
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Abelia chinensis\",\"common_names\":[],\"confidence\":0.99}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (both engines down)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"plant_id_unavailable"`) {
		t.Errorf("body = %s, want plant_id_unavailable (wire code unchanged)", rec.Body.String())
	}
	if visionCalled {
		t.Error("vision called on both-engines-down; AI must NOT substitute for engine-unavailable")
	}
}

// Response is trimmed to top-3 even when selection ran across 10 candidates,
// and the chosen in-catalog candidate stays at [0] (kept + next 2 by conf).
func TestHandleIdentify_CatalogPref_TrimsTo3_ChosenStaysFirst(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"bestMatch":"Oo0","results":[`)
	// 9 out-of-catalog (descending score 0.95..0.55) then the in-catalog
	// Abelia chinensis LAST at the lowest score 0.10.
	for i := 0; i < 9; i++ {
		fmt.Fprintf(&sb, `{"score":%f,"species":{"scientificNameWithoutAuthor":"Oo%d fakeum","scientificName":"Oo%d Auth.","commonNames":[]}},`,
			0.95-float64(i)*0.05, i, i)
	}
	sb.WriteString(`{"score":0.10,"species":{"scientificNameWithoutAuthor":"Abelia chinensis","scientificName":"Abelia chinensis R.Br.","commonNames":["Chinese Abelia"]}}`)
	sb.WriteString(`],"remainingIdentificationRequests":40}`)

	h, cleanup := newCascadeHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, sb.String())
		}, nil)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Suggestions) != 3 {
		t.Fatalf("len(Suggestions) = %d, want 3 (selected across 10, trimmed to 3)", len(result.Suggestions))
	}
	// The lowest-score in-catalog candidate is the chosen [0] (rule B).
	if result.Suggestions[0].ScientificName != "Abelia chinensis" {
		t.Errorf("Suggestions[0] = %q, want Abelia chinensis (in-catalog stays first after trim)", result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != "AAA0001" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0001", result.Suggestions[0].PlantID)
	}
	// The other 2 are the highest-confidence of the remaining out-of-catalog
	// (Oo0 = 0.95, Oo1 = 0.90); plant_id null.
	if result.Suggestions[1].ScientificName != "Oo0 fakeum" || result.Suggestions[2].ScientificName != "Oo1 fakeum" {
		t.Errorf("trimmed tail = %q,%q, want Oo0 fakeum,Oo1 fakeum (top-2 by confidence of remainder)",
			result.Suggestions[1].ScientificName, result.Suggestions[2].ScientificName)
	}
}

// --- Change 1 + Change 2 (cascade tune, 2026-05-19) ---
//
// Change 1: when 0 candidates resolve to the 1522 catalog AND the engine's
// ORIGINAL top candidate is highly confident (Confidence ≥
// plantnetConfidentSkipAIConfidence = 0.80), the AI catalog-recovery probe is
// SKIPPED — engine top is used as-is (out-of-catalog), vision NOT called.
// Change 2: aiCatalogRecoveryMinConfidence lowered 0.55 → 0.10 (maximize
// curated-library hits; user accepts occasional wrong-but-curated).
// Rule-B precedence (≥1 in catalog wins, even over a higher-conf OOB top) is
// UNCHANGED. The 502/both-down + iOS shape are UNCHANGED.

// (i) NONE in catalog + engine TOP Confidence 0.85 (≥ 0.80) → the engine top
// is used as-is (out-of-catalog, plant_id null) and the AI catalog-recovery
// probe is SKIPPED entirely (vision must NOT be called — asserted via a
// vision fake that fails the test if hit). engine=plantnet-confident-oob.
func TestHandleIdentify_CatalogPref_ConfidentTop085_SkipsAIProbe(t *testing.T) {
	visionCalled := false
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionCalled = true
		// Would resolve to catalog at conf 0.99 (≥ new 0.10 floor) → if the
		// probe were NOT skipped this would wrongly become the answer.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Abelia chinensis\",\"common_names\":[],\"confidence\":0.99}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	// Both candidates out-of-catalog; original top score 0.85 ≥ 0.80.
	const pn = `{
  "bestMatch": "Confident fakeum",
  "results": [
    {"score": 0.85, "species": {"scientificNameWithoutAuthor": "Confident fakeum",
      "scientificName": "Confident fakeum Auth.", "commonNames": ["Conf Top"]}},
    {"score": 0.30, "species": {"scientificNameWithoutAuthor": "Lesser fakeum",
      "scientificName": "Lesser fakeum Auth.", "commonNames": []}}
  ],
  "remainingIdentificationRequests": 440
}`
	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, pn)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if visionCalled {
		t.Error("vision was called; engine top 0.85 ≥ 0.80 + none-in-catalog must SKIP the AI probe (Change 1)")
	}
	if result.Suggestions[0].ScientificName != "Confident fakeum" {
		t.Errorf("Suggestions[0] = %q, want Confident fakeum (confident engine top used as-is)", result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID != nil {
		t.Errorf("Suggestions[0].PlantID = %v, want nil (out-of-catalog → iOS enrichment)", result.Suggestions[0].PlantID)
	}
	for i, s := range result.Suggestions {
		if s.ScientificName == "Abelia chinensis" {
			t.Errorf("Suggestions[%d] = Abelia chinensis; AI probe was skipped, its guess must not appear", i)
		}
	}
}

// (ii) NONE in catalog + engine top 0.50 (< 0.80, AI path NOT skipped) + AI
// returns a CATALOG plant at conf 0.12 → adopted as ai-catalog-recovery.
// 0.12 ≥ new floor 0.10 but < old 0.55 → this proves Change 2 (would have
// been rejected under the old 0.55 bar and kept the engine OOB top).
func TestHandleIdentify_CatalogPref_AIConf012_RecoveredUnderNew010Floor(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Abelia chinensis\",\"common_names\":[\"Chinese Abelia\"],\"confidence\":0.12}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	// Engine top score 0.50 < 0.80 → AI catalog-recovery path runs.
	const pn = `{
  "bestMatch": "Notcatalog oneum",
  "results": [
    {"score": 0.50, "species": {"scientificNameWithoutAuthor": "Notcatalog oneum",
      "scientificName": "Notcatalog oneum Auth.", "commonNames": []}},
    {"score": 0.20, "species": {"scientificNameWithoutAuthor": "Notcatalog twoum",
      "scientificName": "Notcatalog twoum Auth.", "commonNames": []}}
  ],
  "remainingIdentificationRequests": 430
}`
	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, pn)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Suggestions) != 1 {
		t.Fatalf("len(Suggestions) = %d, want 1 (AI catalog-recovery replaces set)", len(result.Suggestions))
	}
	s := result.Suggestions[0]
	if s.ScientificName != "Abelia chinensis" {
		t.Errorf("Suggestions[0] = %q, want Abelia chinensis (recovered at conf 0.12 ≥ 0.10 floor)", s.ScientificName)
	}
	if s.PlantID == nil || *s.PlantID != "AAA0001" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0001", s.PlantID)
	}
	if s.Confidence != 0.12 {
		t.Errorf("Confidence = %v, want 0.12 (AI's own; proves 0.10 floor — old 0.55 would reject)", s.Confidence)
	}
	if result.IsPlantConfidence != 0.12 {
		t.Errorf("IsPlantConfidence = %v, want 0.12", result.IsPlantConfidence)
	}
}

// (iii) NONE in catalog + engine top 0.50 (< 0.80) + AI returns a CATALOG
// plant but at conf 0.05 (< new floor 0.10) → NOT recovered; the engine's
// ORIGINAL top is kept as out-of-catalog. engine=plantnet-raw-oob. Proves the
// 0.10 floor still rejects below it (the floor is lowered, not removed).
func TestHandleIdentify_CatalogPref_AIConf005_BelowFloor_KeepsEngineTop(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Abelia chinensis\",\"common_names\":[],\"confidence\":0.05}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	const pn = `{
  "bestMatch": "Keptengine topum",
  "results": [
    {"score": 0.50, "species": {"scientificNameWithoutAuthor": "Keptengine topum",
      "scientificName": "Keptengine topum Auth.", "commonNames": ["Kept Top"]}},
    {"score": 0.10, "species": {"scientificNameWithoutAuthor": "Other lowum",
      "scientificName": "Other lowum Auth.", "commonNames": []}}
  ],
  "remainingIdentificationRequests": 420
}`
	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, pn)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// AI guess (conf 0.05 < 0.10 floor) rejected → engine's ORIGINAL top kept.
	if result.Suggestions[0].ScientificName != "Keptengine topum" {
		t.Errorf("Suggestions[0] = %q, want Keptengine topum (AI 0.05 < 0.10 floor → engine top kept)", result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID != nil {
		t.Errorf("Suggestions[0].PlantID = %v, want nil (out-of-catalog → iOS enrichment)", result.Suggestions[0].PlantID)
	}
	for i, s := range result.Suggestions {
		if s.ScientificName == "Abelia chinensis" {
			t.Errorf("Suggestions[%d] = Abelia chinensis; AI guess below 0.10 floor must not be used", i)
		}
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

// cannedDiagnoseUnhealthyEmptyUnknownPlant — unhealthy + zero disease
// suggestions, and a scientific name absent from plants_index.json so
// plantId resolves to null (exercises the AI full-catalog fallback branch).
const cannedDiagnoseUnhealthyEmptyUnknownPlant = `{
  "result": {
    "is_plant": {"probability": 0.97, "binary": true},
    "is_healthy": {"probability": 0.18, "binary": false},
    "classification": {
      "suggestions": [
        {"name": "Notaplant fakeium", "probability": 0.80,
         "details": {"common_names": [], "scientific_name": "Notaplant fakeium"}}
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

// runDiagnoseFallback drives a full /v1/diagnose request through the handler
// with the given canned Plant.id upstream + vision client, and returns the
// decoded result. Shared by the AI-fallback branch/degrade tests below.
func runDiagnoseFallback(t *testing.T, upstream string, vision *VisionClient) DiagnoseResult {
	t.Helper()
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, upstream)
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
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body)
	}
	return result
}

// AI layer engaged: plantId resolves (AAA0001), so the candidate set is the
// plant's curated common_diseases_list. The model picks P05 (index 1, NOT
// [0]=R01) — proves the AI pick supersedes the old mechanical [0], and the
// wire shape stays byte-identical to the static fallback (isFallback=true,
// Probability 0, empty Cause/Treatment).
func TestHandleDiagnose_EmptySuggestions_AIPicksFromCommonDiseasesList(t *testing.T) {
	vision, vsrv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"P05"}}]}`)
	})
	defer vsrv.Close()

	result := runDiagnoseFallback(t, cannedDiagnoseUnhealthyEmpty, vision)
	if len(result.Issues) != 1 {
		t.Fatalf("Issues len = %d, want 1", len(result.Issues))
	}
	issue := result.Issues[0]
	if !issue.IsFallback {
		t.Error("IsFallback = false, want true")
	}
	if issue.CatalogID == nil || *issue.CatalogID != "P05" {
		t.Errorf("CatalogID = %v, want P05 (AI pick from plant common list, overrides [0]=R01)", issue.CatalogID)
	}
	if issue.Name != "Spider mites" {
		t.Errorf("Name = %q, want Spider mites", issue.Name)
	}
	// Contract invariants — identical to the static safety-net shape.
	if issue.Probability != 0 || issue.Cause != "" {
		t.Errorf("Probability/Cause = %v/%q, want 0/empty", issue.Probability, issue.Cause)
	}
	if issue.Treatment.Biological == nil || len(issue.Treatment.Biological) != 0 ||
		issue.Treatment.Chemical == nil || issue.Treatment.Prevention == nil {
		t.Errorf("Treatment = %+v, want empty (non-nil) slices", issue.Treatment)
	}
}

// AI returns a valid catalog id (L20) that is NOT in AAA0001's
// common_diseases_list. With plantId resolved the candidate set is the
// plant's own list, so L20 is a non-candidate → miss → degrade to the
// static [0] = R01. Proves the candidate set is constrained per-plant.
func TestHandleDiagnose_EmptySuggestions_AIOutsideCommonList_Degrades(t *testing.T) {
	vision, vsrv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"L20"}}]}`)
	})
	defer vsrv.Close()

	result := runDiagnoseFallback(t, cannedDiagnoseUnhealthyEmpty, vision)
	issue := result.Issues[0]
	if !issue.IsFallback {
		t.Error("IsFallback = false, want true")
	}
	if issue.CatalogID == nil || *issue.CatalogID != "R01" {
		t.Errorf("CatalogID = %v, want R01 (L20 not in plant common list → degrade)", issue.CatalogID)
	}
	if issue.Name != "Root rot" {
		t.Errorf("Name = %q, want Root rot", issue.Name)
	}
}

// AI transport error → degrade to the static safety net. Zero regression:
// identical to the pre-AI result for this input (R01 "Root rot").
func TestHandleDiagnose_EmptySuggestions_AIError_Degrades(t *testing.T) {
	vision, vsrv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer vsrv.Close()

	result := runDiagnoseFallback(t, cannedDiagnoseUnhealthyEmpty, vision)
	issue := result.Issues[0]
	if !issue.IsFallback || issue.CatalogID == nil || *issue.CatalogID != "R01" || issue.Name != "Root rot" {
		t.Errorf("issue = %+v, want R01 Root rot isFallback=true (AI error → safety net)", issue)
	}
}

// AI replies NONE → degrade to the static safety net (R01 "Root rot").
func TestHandleDiagnose_EmptySuggestions_AINone_Degrades(t *testing.T) {
	vision, vsrv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"NONE"}}]}`)
	})
	defer vsrv.Close()

	result := runDiagnoseFallback(t, cannedDiagnoseUnhealthyEmpty, vision)
	issue := result.Issues[0]
	if !issue.IsFallback || issue.CatalogID == nil || *issue.CatalogID != "R01" || issue.Name != "Root rot" {
		t.Errorf("issue = %+v, want R01 Root rot isFallback=true (NONE → safety net)", issue)
	}
}

// plantId miss → candidate set is the full ~70-entry catalog. The model
// picks L20 ("Powdery mildew"), which is a valid catalog id but not any
// plant's [0] — proves the miss branch uses the full catalog, not an empty
// per-plant list.
func TestHandleDiagnose_EmptySuggestions_PlantIdMiss_AIFullCatalog(t *testing.T) {
	vision, vsrv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"L20"}}]}`)
	})
	defer vsrv.Close()

	result := runDiagnoseFallback(t, cannedDiagnoseUnhealthyEmptyUnknownPlant, vision)
	if result.PlantID != nil {
		t.Errorf("PlantID = %v, want nil (unknown plant)", result.PlantID)
	}
	issue := result.Issues[0]
	if !issue.IsFallback || issue.CatalogID == nil || *issue.CatalogID != "L20" || issue.Name != "Powdery mildew" {
		t.Errorf("issue = %+v, want L20 Powdery mildew isFallback=true (AI full-catalog pick)", issue)
	}
}

// plantId miss + AI error → degrade. plantId is nil so the [0] branch is
// skipped; safety net lands on generic L06 "Leaf spot" (unchanged pre-AI
// behavior for the unknown-plant case).
func TestHandleDiagnose_EmptySuggestions_PlantIdMiss_AIError_DegradesToL06(t *testing.T) {
	vision, vsrv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer vsrv.Close()

	result := runDiagnoseFallback(t, cannedDiagnoseUnhealthyEmptyUnknownPlant, vision)
	if result.PlantID != nil {
		t.Errorf("PlantID = %v, want nil", result.PlantID)
	}
	issue := result.Issues[0]
	if !issue.IsFallback || issue.CatalogID == nil || *issue.CatalogID != "L06" || issue.Name != "Leaf spot" {
		t.Errorf("issue = %+v, want L06 Leaf spot isFallback=true (miss + AI error → L06)", issue)
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
