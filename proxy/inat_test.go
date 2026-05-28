package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newINatStub returns an INatClient pointed at a test server that replies with
// the given JSON body and 200.
func newINatStub(t *testing.T, body string) (*INatClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return &INatClient{HTTP: srv.Client(), BaseURL: srv.URL}, srv.Close
}

func TestINatPreferredCommonName_SpeciesMatch(t *testing.T) {
	// Species row + a subspecies row (lower preferred name). The species-level
	// query must return the species' "Prettyface", not the subspecies name.
	c, done := newINatStub(t, `{"results":[
		{"name":"Triteleia ixioides","rank":"species","preferred_common_name":"Prettyface"},
		{"name":"Triteleia ixioides anilina","rank":"subspecies","preferred_common_name":"mountain pretty face"}
	]}`)
	defer done()

	got, ok := c.PreferredCommonName(context.Background(), "Triteleia ixioides")
	if !ok || got != "Prettyface" {
		t.Fatalf("PreferredCommonName = (%q,%v), want (Prettyface,true)", got, ok)
	}
}

func TestINatPreferredCommonName_NameMismatchRejected(t *testing.T) {
	// q is fuzzy: iNat returns a DIFFERENT taxon first → must not be trusted.
	c, done := newINatStub(t, `{"results":[
		{"name":"Ixia polystachya","rank":"species","preferred_common_name":"Wand Flower"}
	]}`)
	defer done()

	if got, ok := c.PreferredCommonName(context.Background(), "Triteleia ixioides"); ok {
		t.Fatalf("mismatched taxon should be rejected, got %q", got)
	}
}

func TestINatPreferredCommonName_EmptyNameIsMiss(t *testing.T) {
	c, done := newINatStub(t, `{"results":[
		{"name":"Triteleia ixioides","rank":"species","preferred_common_name":""}
	]}`)
	defer done()

	if _, ok := c.PreferredCommonName(context.Background(), "Triteleia ixioides"); ok {
		t.Fatal("empty preferred_common_name should be a miss")
	}
}

func TestINatPreferredCommonName_Non200IsMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &INatClient{HTTP: srv.Client(), BaseURL: srv.URL}

	if _, ok := c.PreferredCommonName(context.Background(), "Triteleia ixioides"); ok {
		t.Fatal("non-2xx response should be a miss")
	}
}

func TestINatPreferredCommonName_NilAndEmptyInputs(t *testing.T) {
	var nilClient *INatClient
	if _, ok := nilClient.PreferredCommonName(context.Background(), "Triteleia ixioides"); ok {
		t.Error("nil client should return false")
	}
	c, done := newINatStub(t, `{"results":[]}`)
	defer done()
	if _, ok := c.PreferredCommonName(context.Background(), "   "); ok {
		t.Error("blank scientific name should return false")
	}
}
