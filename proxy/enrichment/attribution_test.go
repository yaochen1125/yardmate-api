package enrichment

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubApple builds an AppleAdServicesClient pointed at an httptest server that
// returns the given status + body for every request.
func stubApple(t *testing.T, status int, body string) (*AppleAdServicesClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return &AppleAdServicesClient{httpClient: srv.Client(), baseURL: srv.URL}, srv.Close
}

func postAttribution(t *testing.T, db *DB, apple *AppleAdServicesClient, deviceID, appVersion, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/attribution", strings.NewReader(body))
	if deviceID != "" {
		req.Header.Set("X-Device-Install-Id", deviceID)
	}
	if appVersion != "" {
		req.Header.Set("X-App-Version", appVersion)
	}
	rec := httptest.NewRecorder()
	HandleAttribution(db, apple)(rec, req)
	return rec
}

// Validation short-circuits before Apple / the DB, so a nil client + nil DB is fine.
func TestHandleAttributionValidation(t *testing.T) {
	cases := []struct {
		name       string
		deviceID   string
		appVersion string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing device id", "", "1.0.4", `{"token":"abc"}`, http.StatusBadRequest, "missing_device_id"},
		{"missing app version", testInstallID, "", `{"token":"abc"}`, http.StatusBadRequest, "missing_app_version"},
		{"bad json", testInstallID, "1.0.4", "{", http.StatusBadRequest, "bad_json"},
		{"empty token", testInstallID, "1.0.4", `{"token":""}`, http.StatusBadRequest, "invalid_token"},
		{"whitespace token", testInstallID, "1.0.4", `{"token":"   "}`, http.StatusBadRequest, "invalid_token"},
		{"oversized token", testInstallID, "1.0.4", `{"token":"` + strings.Repeat("x", attributionTokenMaxLen+1) + `"}`,
			http.StatusBadRequest, "invalid_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postAttribution(t, nil, nil, tc.deviceID, tc.appVersion, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body = %s, want code %q", rec.Body.String(), tc.wantCode)
			}
		})
	}
}

// Apple 404 → {"status":"pending"} without touching the DB (nil pool never reached).
func TestHandleAttributionPending(t *testing.T) {
	apple, closeSrv := stubApple(t, http.StatusNotFound, "")
	defer closeSrv()

	rec := postAttribution(t, nil, apple, testInstallID, "1.0.4", `{"token":"tok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pending") {
		t.Fatalf("body = %s, want pending", rec.Body.String())
	}
}

// Apple 200 → the handler reaches RecordAttribution; a nil pool surfaces as
// db_unavailable, proving the token exchange + routing let it through to the DB.
func TestHandleAttributionReachesDB(t *testing.T) {
	apple, closeSrv := stubApple(t, http.StatusOK, `{"attribution":true,"campaignId":123,"keywordId":9}`)
	defer closeSrv()

	rec := postAttribution(t, &DB{}, apple, testInstallID, "1.0.4", `{"token":"tok"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "db_unavailable") {
		t.Fatalf("body = %s, want db_unavailable", rec.Body.String())
	}
}

// Apple 5xx → the handler reports attribution_upstream (not stored, not pending).
func TestHandleAttributionUpstreamError(t *testing.T) {
	apple, closeSrv := stubApple(t, http.StatusInternalServerError, "boom")
	defer closeSrv()

	rec := postAttribution(t, &DB{}, apple, testInstallID, "1.0.4", `{"token":"tok"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "attribution_upstream") {
		t.Fatalf("body = %s, want attribution_upstream", rec.Body.String())
	}
}

// The Apple client decodes a 200 record and maps 404 to the pending sentinel.
func TestAppleClientFetch(t *testing.T) {
	t.Run("200 decodes", func(t *testing.T) {
		apple, closeSrv := stubApple(t, http.StatusOK,
			`{"attribution":true,"orgId":40669820,"campaignId":542,"keywordId":87,"conversionType":"Download","countryOrRegion":"US"}`)
		defer closeSrv()
		rec, err := apple.Fetch(context.Background(), "tok")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !rec.Attribution || rec.CampaignID != 542 || rec.KeywordID != 87 || rec.CountryOrRegion != "US" {
			t.Fatalf("decoded = %+v", rec)
		}
	})
	t.Run("404 is pending", func(t *testing.T) {
		apple, closeSrv := stubApple(t, http.StatusNotFound, "")
		defer closeSrv()
		if _, err := apple.Fetch(context.Background(), "tok"); err != errAttributionPending {
			t.Fatalf("err = %v, want errAttributionPending", err)
		}
	})
}
