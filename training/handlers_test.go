package training

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testUUID = "123e4567-e89b-42d3-a456-426614174000"

// uploadReq builds a multipart POST /v1/training/photo request.
func uploadReq(t *testing.T, headers map[string]string, fields map[string]string, image []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if image != nil {
		fw, err := mw.CreateFormFile("image", "photo.jpg")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		if _, err := fw.Write(image); err != nil {
			t.Fatalf("write image: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/training/photo", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// Sensible defaults; individual tests override via headers map.
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.3.0")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	return req
}

func TestUploadHappyPath(t *testing.T) {
	st := openTestStore(t)
	h := HandleUpload(st)
	rec := httptest.NewRecorder()
	h(rec, uploadReq(t,
		nil,
		map[string]string{"consent_version": "2026-07-14", "label_kind": "accept", "plant_id": "AAA0701"},
		baseJPEG(t)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID     string `json:"id"`
		Stored bool   `json:"stored"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" || !resp.Stored {
		t.Fatalf("bad response: %+v", resp)
	}
	if stats, _ := st.Stats(context.Background()); stats.Total != 1 {
		t.Errorf("store total = %d, want 1", stats.Total)
	}
}

func TestUploadValidation(t *testing.T) {
	st := openTestStore(t)
	h := HandleUpload(st)
	img := baseJPEG(t)

	cases := []struct {
		name    string
		headers map[string]string
		fields  map[string]string
		image   []byte
		want    int
		code    string
	}{
		{"missing device id", map[string]string{"X-Device-Install-Id": ""},
			map[string]string{"consent_version": "v1"}, img, http.StatusBadRequest, "missing_device_id"},
		{"bad device id", map[string]string{"X-Device-Install-Id": "not-a-uuid"},
			map[string]string{"consent_version": "v1"}, img, http.StatusBadRequest, "missing_device_id"},
		{"missing app version", map[string]string{"X-App-Version": ""},
			map[string]string{"consent_version": "v1"}, img, http.StatusBadRequest, "missing_app_version"},
		{"missing image", nil,
			map[string]string{"consent_version": "v1"}, nil, http.StatusBadRequest, "missing_image"},
		{"missing consent", nil,
			map[string]string{}, img, http.StatusBadRequest, "missing_consent_version"},
		{"bad image bytes", nil,
			map[string]string{"consent_version": "v1"}, []byte("this is not a jpeg at all, just text"), http.StatusBadRequest, "bad_image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, uploadReq(t, tc.headers, tc.fields, tc.image))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &e)
			if e.Error != tc.code {
				t.Errorf("error = %q, want %q", e.Error, tc.code)
			}
		})
	}
}

func TestUploadStripsExifEndToEnd(t *testing.T) {
	st := openTestStore(t)
	h := HandleUpload(st)
	rec := httptest.NewRecorder()
	h(rec, uploadReq(t, nil, map[string]string{"consent_version": "v1"}, withExif(baseJPEG(t))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// The stored file must not contain the GPS marker.
	var found bool
	walkStoredJPEGs(t, st, func(b []byte) {
		if bytes.Contains(b, []byte("GPSLATITUDE-SECRET")) {
			found = true
		}
	})
	if found {
		t.Error("stored training photo still contains EXIF/GPS payload")
	}
}

func TestDeleteEndpoint(t *testing.T) {
	st := openTestStore(t)
	up := HandleUpload(st)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		up(rec, uploadReq(t, nil, map[string]string{"consent_version": "v1"}, baseJPEG(t)))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload %d: %d", i, rec.Code)
		}
	}
	del := HandleDelete(st)
	req := httptest.NewRequest(http.MethodPost, "/v1/training/delete", nil)
	req.Header.Set("X-Device-Install-Id", testUUID)
	rec := httptest.NewRecorder()
	del(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Deleted != 2 {
		t.Errorf("deleted = %d, want 2", resp.Deleted)
	}
	if stats, _ := st.Stats(context.Background()); stats.Total != 0 {
		t.Errorf("store total after delete = %d, want 0", stats.Total)
	}

	// Missing device id → 400.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/training/delete", nil)
	del(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("delete without device id: status = %d, want 400", rec2.Code)
	}
}

// walkStoredJPEGs reads every stored .jpg and hands its bytes to fn.
func walkStoredJPEGs(t *testing.T, st *Store, fn func([]byte)) {
	t.Helper()
	rows, err := st.db.Query("SELECT rel_path FROM photos WHERE deleted = 0")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var rels []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rels = append(rels, rel)
	}
	for _, rel := range rels {
		b := readStored(t, st, rel)
		fn(b)
	}
}
