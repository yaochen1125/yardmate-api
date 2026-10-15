package enrichment

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postShopClick(t *testing.T, db *DB, deviceID, appVersion, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/shop/click", strings.NewReader(body))
	if deviceID != "" {
		req.Header.Set("X-Device-Install-Id", deviceID)
	}
	if appVersion != "" {
		req.Header.Set("X-App-Version", appVersion)
	}
	rec := httptest.NewRecorder()
	HandleShopClick(db)(rec, req)
	return rec
}

func TestHandleShopClickValidation(t *testing.T) {
	ok := `{"itemId":"seeds-lavender","screen":"shop_category","context":"seeds","locale":"en"}`
	cases := []struct {
		name       string
		deviceID   string
		appVersion string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing device id", "", "2.0.0", ok, http.StatusBadRequest, "missing_device_id"},
		{"bad device id", "not-a-uuid", "2.0.0", ok, http.StatusBadRequest, "missing_device_id"},
		{"missing app version", testInstallID, "", ok, http.StatusBadRequest, "missing_app_version"},
		{"bad json", testInstallID, "2.0.0", "{", http.StatusBadRequest, "bad_json"},
		{"empty item", testInstallID, "2.0.0", `{"itemId":"","screen":"shop"}`, http.StatusBadRequest, "invalid_item_id"},
		{"item with space", testInstallID, "2.0.0", `{"itemId":"seeds lavender","screen":"shop"}`, http.StatusBadRequest, "invalid_item_id"},
		{"item too long", testInstallID, "2.0.0", `{"itemId":"` + strings.Repeat("a", 65) + `","screen":"shop"}`, http.StatusBadRequest, "invalid_item_id"},
		{"bad screen", testInstallID, "2.0.0", `{"itemId":"seeds-lavender","screen":"shop_search"}`, http.StatusBadRequest, "invalid_screen"},
		{"bad context", testInstallID, "2.0.0", `{"itemId":"seeds-lavender","screen":"shop","context":"a/b"}`, http.StatusBadRequest, "invalid_context"},
		{"locale too long", testInstallID, "2.0.0", `{"itemId":"seeds-lavender","screen":"shop","locale":"` + strings.Repeat("x", 17) + `"}`, http.StatusBadRequest, "invalid_locale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postShopClick(t, nil, tc.deviceID, tc.appVersion, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body = %s, want code %q", rec.Body.String(), tc.wantCode)
			}
		})
	}
}

// Well-formed clicks (with and without the optional fields) pass validation and
// reach the DB layer — with a nil pool that surfaces as db_unavailable.
func TestHandleShopClickReachesDB(t *testing.T) {
	for _, body := range []string{
		`{"itemId":"seeds-lavender","screen":"shop_category","context":"seeds","locale":"en"}`,
		`{"itemId":"seeds-lavender","screen":"shop"}`,
		`{"itemId":"pots.self_watering-2","screen":"shop_collection","context":"editors-picks","locale":"zh-Hans"}`,
	} {
		rec := postShopClick(t, nil, testInstallID, "2.0.0", body)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("body %s: status = %d, want %d (resp %s)", body, rec.Code, http.StatusBadGateway, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "db_unavailable") {
			t.Fatalf("body %s: resp = %s, want db_unavailable", body, rec.Body.String())
		}
	}
}
