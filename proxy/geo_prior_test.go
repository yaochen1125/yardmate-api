package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestParseCoord covers the coarse-coordinate parser: valid values round to 2
// decimals, everything invalid (out of range, non-finite, garbage, empty) →
// nil so it never reaches an engine or a log.
func TestParseCoord(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name     string
		in       string
		min, max float64
		want     *float64
	}{
		{"lat rounds to 2dp", "37.7749", -90, 90, f(37.77)},
		{"lon rounds to 2dp", "-122.4194", -180, 180, f(-122.42)},
		{"trims whitespace", "  45.126 ", -90, 90, f(45.13)},
		{"zero is valid", "0", -90, 90, f(0)},
		{"lat out of range high", "90.1", -90, 90, nil},
		{"lat out of range low", "-90.1", -90, 90, nil},
		{"lon out of range", "181", -180, 180, nil},
		{"NaN rejected", "NaN", -90, 90, nil},
		{"Inf rejected", "Inf", -90, 90, nil},
		{"garbage rejected", "abc", -90, 90, nil},
		{"empty rejected", "", -90, 90, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCoord(tc.in, tc.min, tc.max)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("parseCoord(%q) = %v, want nil", tc.in, *got)
			case tc.want != nil && got == nil:
				t.Fatalf("parseCoord(%q) = nil, want %v", tc.in, *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Fatalf("parseCoord(%q) = %v, want %v", tc.in, *got, *tc.want)
			}
		})
	}
}

// TestPlantIDClient_Identify_GeoPrior asserts the geographic prior travels only
// in the multipart BODY, packed into Plant.id's single `data` JSON field (NOT
// separate lat/lon form fields, which the API ignores), and is absent when the
// caller passes a nil pair.
func TestPlantIDClient_Identify_GeoPrior(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	run := func(t *testing.T, lat, lon *float64) (query string, vals map[string][]string) {
		t.Helper()
		var capturedQuery string
		var capturedVals map[string][]string
		c, srv := newTestPlantIDClient(t, func(w http.ResponseWriter, r *http.Request) {
			capturedQuery = r.URL.RawQuery
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Fatalf("ParseMultipartForm: %v", err)
			}
			if r.MultipartForm != nil {
				capturedVals = r.MultipartForm.Value
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, cannedPlantIDOK)
		})
		defer srv.Close()
		if _, err := c.Identify(context.Background(),
			bytes.NewReader([]byte("img")), "image/jpeg", lat, lon); err != nil {
			t.Fatalf("Identify: %v", err)
		}
		return capturedQuery, capturedVals
	}

	t.Run("forwards coords in the data JSON field, body only", func(t *testing.T) {
		query, vals := run(t, f(37.77), f(-122.42))
		data := vals["data"]
		if len(data) != 1 {
			t.Fatalf("data field = %v, want exactly one JSON string", data)
		}
		var got map[string]float64
		if err := json.Unmarshal([]byte(data[0]), &got); err != nil {
			t.Fatalf("data field is not valid JSON (%q): %v", data[0], err)
		}
		if got["latitude"] != 37.77 || got["longitude"] != -122.42 {
			t.Errorf("data = %v, want latitude 37.77 / longitude -122.42", got)
		}
		// Plant.id ignores separate lat/lon form fields — ensure we didn't send them.
		if len(vals["latitude"]) != 0 || len(vals["longitude"]) != 0 {
			t.Errorf("unexpected separate lat/lon form fields: %v %v", vals["latitude"], vals["longitude"])
		}
		// Privacy invariant: coordinates must never leak into the URL/query.
		if query != "" {
			t.Errorf("upstream query string = %q, want empty (coords must stay in body)", query)
		}
	})

	t.Run("omits data when nil coords", func(t *testing.T) {
		_, vals := run(t, nil, nil)
		if got := vals["data"]; len(got) != 0 {
			t.Errorf("data field present with nil coords: %v", got)
		}
	})
}
