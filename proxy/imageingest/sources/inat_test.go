package sources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const inatTaxaHitJSON = `{
  "results": [
    {
      "id": 42,
      "name": "Monstera adansonii",
      "rank": "species",
      "default_photo": {
        "id": 100,
        "medium_url": "https://static.inaturalist.org/photos/100/medium.jpg",
        "large_url": "https://static.inaturalist.org/photos/100/large.jpg",
        "license_code": "cc-by-sa",
        "attribution": "(c) Bob, some rights reserved (CC BY-SA)"
      }
    }
  ]
}`

// default_photo with a null license_code (All Rights Reserved) → must be skipped.
const inatTaxaARRJSON = `{
  "results": [
    {
      "id": 42,
      "name": "Monstera adansonii",
      "rank": "species",
      "default_photo": {"id": 100, "url": "https://static.inaturalist.org/photos/100/square.jpg", "license_code": null}
    }
  ]
}`

const inatTaxaNoneJSON = `{"results": []}`

const inatObsJSON = `{
  "results": [
    {
      "id": 7,
      "photos": [
        {"id": 200, "url": "https://static.inaturalist.org/photos/200/square.jpg", "license_code": "cc-by", "attribution": "(c) Alice"}
      ]
    }
  ]
}`

const inatObsEmptyJSON = `{"results": []}`

// inatRouter serves /taxa and /observations from the given bodies. 403 on empty UA.
func inatRouter(t *testing.T, taxaBody, obsBody string, sawObsLicense *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/taxa"):
			if got := r.URL.Query().Get("rank"); got != "species,genus,subspecies,variety" {
				t.Errorf("taxa rank = %q, want species,genus,subspecies,variety", got)
			}
			_, _ = w.Write([]byte(taxaBody))
		case strings.HasPrefix(r.URL.Path, "/observations"):
			if sawObsLicense != nil {
				*sawObsLicense = r.URL.Query().Get("photo_license")
			}
			_, _ = w.Write([]byte(obsBody))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})
}

func newINatTestClient(srv *httptest.Server) *INatClient {
	return NewINatClient(INatOptions{APIBase: srv.URL, HTTPClient: srv.Client(), UserAgent: "test-ua"})
}

func TestINatSearch_DefaultPhotoPlusObservation(t *testing.T) {
	var obsLicense string
	srv := httptest.NewServer(inatRouter(t, inatTaxaHitJSON, inatObsJSON, &obsLicense))
	defer srv.Close()

	c := newINatTestClient(srv)
	cands, err := c.Search(context.Background(), "Monstera adansonii", 12)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates (default + obs), got %d", len(cands))
	}

	// 1st: default_photo — large_url used directly, raw cc-by-sa code.
	d := cands[0]
	if d.Source != SourceINaturalist {
		t.Errorf("Source = %q", d.Source)
	}
	if d.DownloadURL != "https://static.inaturalist.org/photos/100/large.jpg" {
		t.Errorf("default DownloadURL = %q", d.DownloadURL)
	}
	if d.LicenseCode != "cc-by-sa" {
		t.Errorf("default LicenseCode = %q, want raw cc-by-sa", d.LicenseCode)
	}
	if d.DedupKey != "inat-photo-100" {
		t.Errorf("default DedupKey = %q", d.DedupKey)
	}
	if d.PageURL != "https://www.inaturalist.org/photos/100" {
		t.Errorf("default PageURL = %q", d.PageURL)
	}

	// 2nd: observation photo — square URL upgraded to large via deriveLargeURL.
	o := cands[1]
	if o.DownloadURL != "https://static.inaturalist.org/photos/200/large.jpg" {
		t.Errorf("obs DownloadURL = %q, want derived large", o.DownloadURL)
	}
	if o.LicenseCode != "cc-by" {
		t.Errorf("obs LicenseCode = %q", o.LicenseCode)
	}

	// Observations API call must carry the license filter (SPEC §2.4.1).
	if obsLicense != inatObservationLicenses {
		t.Errorf("photo_license = %q, want %q", obsLicense, inatObservationLicenses)
	}
}

// A fuzzy / misspelled query that iNat answers with a DIFFERENT-named taxon
// must yield no candidates (we only trust an exact scientific-name match), so
// the cascade falls through to the next source instead of attaching the wrong
// species' photos to this slug.
func TestINatSearch_FuzzyNonExactMatch_NoCandidates(t *testing.T) {
	srv := httptest.NewServer(inatRouter(t, inatTaxaHitJSON, inatObsJSON, nil))
	defer srv.Close()

	c := newINatTestClient(srv)
	// inatTaxaHitJSON's taxon Name is "Monstera adansonii"; query a misspelling.
	cands, err := c.Search(context.Background(), "Monstera adansoni", 12)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected 0 candidates on non-exact match, got %d", len(cands))
	}
}

func TestINatSearch_NoTaxonMatch(t *testing.T) {
	srv := httptest.NewServer(inatRouter(t, inatTaxaNoneJSON, inatObsJSON, nil))
	defer srv.Close()

	c := newINatTestClient(srv)
	cands, err := c.Search(context.Background(), "Nonexistent plantus", 12)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(cands))
	}
}

// A null-license default_photo is skipped; observations still supply candidates.
func TestINatSearch_ARRDefaultFallsBackToObservations(t *testing.T) {
	srv := httptest.NewServer(inatRouter(t, inatTaxaARRJSON, inatObsJSON, nil))
	defer srv.Close()

	c := newINatTestClient(srv)
	cands, err := c.Search(context.Background(), "Monstera adansonii", 12)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate (obs only), got %d", len(cands))
	}
	if cands[0].DedupKey != "inat-photo-200" {
		t.Errorf("DedupKey = %q, want obs photo", cands[0].DedupKey)
	}
}

// When the default photo is usable but observations are empty, return just the
// default photo without error.
func TestINatSearch_DefaultOnlyWhenNoObservations(t *testing.T) {
	srv := httptest.NewServer(inatRouter(t, inatTaxaHitJSON, inatObsEmptyJSON, nil))
	defer srv.Close()

	c := newINatTestClient(srv)
	cands, err := c.Search(context.Background(), "Monstera adansonii", 12)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate (default only), got %d", len(cands))
	}
	if cands[0].DedupKey != "inat-photo-100" {
		t.Errorf("DedupKey = %q", cands[0].DedupKey)
	}
}
