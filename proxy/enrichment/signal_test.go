package enrichment

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postSignal(t *testing.T, db *DB, deviceID, appVersion, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/plants/signal", strings.NewReader(body))
	if deviceID != "" {
		req.Header.Set("X-Device-Install-Id", deviceID)
	}
	if appVersion != "" {
		req.Header.Set("X-App-Version", appVersion)
	}
	rec := httptest.NewRecorder()
	HandleSignal(db)(rec, req)
	return rec
}

func TestHandleSignalValidation(t *testing.T) {
	cases := []struct {
		name       string
		deviceID   string
		appVersion string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing device id", "", "1.0.4", `{"scientificName":"Rosa","kind":"search"}`,
			http.StatusBadRequest, "missing_device_id"},
		{"missing app version", testInstallID, "", `{"scientificName":"Rosa","kind":"search"}`,
			http.StatusBadRequest, "missing_app_version"},
		{"bad json", testInstallID, "1.0.4", "{", http.StatusBadRequest, "bad_json"},

		// Out-of-catalog (no plantId) branch.
		{"ooc missing name", testInstallID, "1.0.4", `{"scientificName":"","kind":"search"}`,
			http.StatusBadRequest, "missing_scientific_name"},
		{"ooc invalid kind", testInstallID, "1.0.4", `{"scientificName":"Rosa","kind":"bogus"}`,
			http.StatusBadRequest, "invalid_kind"},

		// In-catalog (plantId set) branch — validated before the DB, so nil pool never reached.
		{"catalog bad id", testInstallID, "1.0.4", `{"plantId":"505","kind":"identify"}`,
			http.StatusBadRequest, "invalid_plant_id"},
		{"catalog lowercase id", testInstallID, "1.0.4", `{"plantId":"aaa0505","kind":"identify"}`,
			http.StatusBadRequest, "invalid_plant_id"},
		{"catalog garden rejected", testInstallID, "1.0.4", `{"plantId":"AAA0505","kind":"garden"}`,
			http.StatusBadRequest, "invalid_kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postSignal(t, nil, tc.deviceID, tc.appVersion, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body = %s, want code %q", rec.Body.String(), tc.wantCode)
			}
		})
	}
}

// A well-formed catalog signal passes all validation and reaches the DB layer —
// with a nil pool that surfaces as db_unavailable, proving plantId routing +
// kind gate let it through to RecordCatalogSignal.
func TestHandleSignalCatalogPathReachesDB(t *testing.T) {
	for _, kind := range []string{"identify", "search"} {
		rec := postSignal(t, nil, testInstallID, "1.0.4",
			`{"plantId":"AAA0505","kind":"`+kind+`"}`)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("kind %s: status = %d, want %d (body %s)",
				kind, rec.Code, http.StatusBadGateway, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "db_unavailable") {
			t.Fatalf("kind %s: body = %s, want db_unavailable", kind, rec.Body.String())
		}
	}
}
