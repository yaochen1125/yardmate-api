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
	srv := httptest.NewServer(upstream)
	c := &PlantIDClient{
		APIKey:   "test-key",
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
	}
	return HandleIdentify(c), srv
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

// ---------- HandleAIChat tests ----------

func newAIChatHandler(t *testing.T, upstream http.HandlerFunc) (http.Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	c := &OpenAIClient{
		APIKey:      "test-key",
		Endpoint:    srv.URL,
		Model:       "gpt-4o-mini",
		MaxTokens:   500,
		Temperature: 0.7,
		HTTP:        srv.Client(),
	}
	return HandleAIChat(c), srv
}

func aiChatReq(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ai-chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	return req
}

func TestHandleAIChat_Success(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedOpenAIOK)
	})
	defer srv.Close()

	req := aiChatReq(t, `{"plant_name":"Monstera deliciosa","question":"How often water?"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got ChatResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got.Answer, "Monstera") {
		t.Errorf("answer = %q, want assistant content", got.Answer)
	}
}

func TestHandleAIChat_MissingDeviceID(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/ai-chat",
		strings.NewReader(`{"plant_name":"P","question":"Q"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-App-Version", "1.1.1")
	// no X-Device-Install-Id
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_device_id"`) {
		t.Errorf("body = %s, want missing_device_id", rec.Body.String())
	}
}

func TestHandleAIChat_MissingAppVersion(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/ai-chat",
		strings.NewReader(`{"plant_name":"P","question":"Q"}`))
	req.Header.Set("Content-Type", "application/json")
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

func TestHandleAIChat_BadJSON(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{this is not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"bad_json"`) {
		t.Errorf("body = %s, want bad_json", rec.Body.String())
	}
}

func TestHandleAIChat_BadContentType(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/ai-chat",
		strings.NewReader(`{"plant_name":"P","question":"Q"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"bad_json"`) {
		t.Errorf("body = %s, want bad_json (no JSON content-type)", rec.Body.String())
	}
}

func TestHandleAIChat_MissingField_PlantName(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"","question":"Q"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_field"`) {
		t.Errorf("body = %s, want missing_field", rec.Body.String())
	}
}

func TestHandleAIChat_MissingField_Question(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"P","question":""}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"missing_field"`) {
		t.Errorf("body = %s, want missing_field", rec.Body.String())
	}
}

func TestHandleAIChat_PlantNameTooLong(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	longName := strings.Repeat("a", aiChatPlantNameMaxLen+1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"`+longName+`","question":"Q"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"plant_name_too_long"`) {
		t.Errorf("body = %s, want plant_name_too_long", rec.Body.String())
	}
}

func TestHandleAIChat_QuestionTooLong(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	})
	defer srv.Close()

	longQ := strings.Repeat("q", aiChatQuestionMaxLen+1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"P","question":"`+longQ+`"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"question_too_long"`) {
		t.Errorf("body = %s, want question_too_long", rec.Body.String())
	}
}

func TestHandleAIChat_BodyTooLarge(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called when body exceeds cap")
	})
	defer srv.Close()

	// Build a JSON body > aiChatMaxBody (64 KiB)
	// stuff plant_scientific_name field (optional) with junk to inflate body
	junk := strings.Repeat("x", aiChatMaxBody+100)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"P","plant_scientific_name":"`+junk+`","question":"Q"}`))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"body_too_large"`) {
		t.Errorf("body = %s, want body_too_large", rec.Body.String())
	}
}

func TestHandleAIChat_OpenAIUnavailable(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"P","question":"Q"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"openai_unavailable"`) {
		t.Errorf("body = %s, want openai_unavailable", rec.Body.String())
	}
}

func TestHandleAIChat_OpenAIUnauthorized(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"P","question":"Q"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"openai_unauthorized"`) {
		t.Errorf("body = %s, want openai_unauthorized", rec.Body.String())
	}
}

func TestHandleAIChat_OpenAIBadResponse(t *testing.T) {
	h, srv := newAIChatHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	defer srv.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aiChatReq(t, `{"plant_name":"P","question":"Q"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (empty choices is bad_response → openai_unavailable)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"openai_unavailable"`) {
		t.Errorf("body = %s, want openai_unavailable", rec.Body.String())
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
