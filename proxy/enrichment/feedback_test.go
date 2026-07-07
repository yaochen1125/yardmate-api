package enrichment

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testInstallID = "3f2b8a9c-1d4e-4f6a-9b0c-7e5d2a8f1c3b"

func postFeedback(t *testing.T, db *DB, deviceID, appVersion, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", strings.NewReader(body))
	if deviceID != "" {
		req.Header.Set("X-Device-Install-Id", deviceID)
	}
	if appVersion != "" {
		req.Header.Set("X-App-Version", appVersion)
	}
	rec := httptest.NewRecorder()
	HandleFeedback(db, nil)(rec, req)
	return rec
}

func TestHandleFeedbackValidation(t *testing.T) {
	valid := `{"message":"hello"}`
	cases := []struct {
		name       string
		deviceID   string
		appVersion string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing device id", "", "1.0.4", valid, http.StatusBadRequest, "missing_device_id"},
		{"non-uuid device id", "not-a-uuid", "1.0.4", valid, http.StatusBadRequest, "missing_device_id"},
		{"missing app version", testInstallID, "", valid, http.StatusBadRequest, "missing_app_version"},
		{"bad json", testInstallID, "1.0.4", "{", http.StatusBadRequest, "bad_json"},
		{"empty message", testInstallID, "1.0.4", `{"message":"   \n  "}`, http.StatusBadRequest, "missing_message"},
		{"message too long", testInstallID, "1.0.4",
			`{"message":"` + strings.Repeat("あ", 1001) + `"}`, http.StatusBadRequest, "message_too_long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postFeedback(t, nil, tc.deviceID, tc.appVersion, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body = %s, want code %q", rec.Body.String(), tc.wantCode)
			}
		})
	}
}

func TestHandleFeedbackMessageAtCapPassesValidation(t *testing.T) {
	// Exactly 1000 runes passes validation and reaches the DB layer — with a
	// nil pool that surfaces as db_unavailable, proving the length gate let it
	// through.
	body := `{"message":"` + strings.Repeat("あ", 1000) + `"}`
	rec := postFeedback(t, nil, testInstallID, "1.0.4", body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "db_unavailable") {
		t.Fatalf("body = %s, want db_unavailable", rec.Body.String())
	}
}

func TestFeedbackMailerCompose(t *testing.T) {
	m := NewFeedbackMailer("", "", "from@example.com", "pw", "to@example.com")
	if m == nil {
		t.Fatal("mailer should be enabled when from/pass/to set")
	}
	row := feedbackRow{
		deviceID:   testInstallID,
		appVersion: "1.0.4\r\nBcc: evil@example.com", // header-injection attempt
		message:    "多语言 message ✓",
		device:     "iPhone 17 (iPhone18,3)",
		system:     "iOS 26.0", appLanguage: "zh-Hans", region: "US",
	}
	msg := m.compose("fid-1", row, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	// CR/LF stripped → "Bcc:" survives only as inline Subject text, never as
	// its own header line.
	header := strings.SplitN(msg, "\r\n\r\n", 2)[0]
	for _, line := range strings.Split(header, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("header injection not neutralized:\n%s", header)
		}
	}
	for _, want := range []string{"多语言 message ✓", "iPhone 17", "zh-Hans", "fid-1", "2026-07-06T12:00:00Z", testInstallID} {
		if !strings.Contains(msg, want) {
			t.Fatalf("compose missing %q:\n%s", want, msg)
		}
	}
}

func TestNewFeedbackMailerDisabledWhenIncomplete(t *testing.T) {
	if NewFeedbackMailer("", "", "from@example.com", "", "to@example.com") != nil {
		t.Fatal("mailer must be nil when pass missing")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("héllo", 3); got != "hél" {
		t.Fatalf("truncateRunes = %q, want %q", got, "hél")
	}
	if got := truncateRunes("ok", 10); got != "ok" {
		t.Fatalf("truncateRunes = %q, want %q", got, "ok")
	}
}
