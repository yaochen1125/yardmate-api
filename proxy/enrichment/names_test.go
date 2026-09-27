package enrichment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// wikidataStub serves a SPARQL JSON body built from (kind, lang, name) rows.
func wikidataStub(t *testing.T, status int, rows [][3]string) (*WikidataClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		var b strings.Builder
		b.WriteString(`{"results":{"bindings":[`)
		for i, row := range rows {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"kind":{"value":"` + row[0] + `"},"lang":{"value":"` + row[1] + `"},"name":{"value":"` + row[2] + `"}}`)
		}
		b.WriteString(`]}}`)
		w.Header().Set("Content-Type", "application/sparql-results+json")
		_, _ = w.Write([]byte(b.String()))
	}))
	return &WikidataClient{HTTP: srv.Client(), Endpoint: srv.URL}, srv.Close
}

func TestWikidataCommonNames(t *testing.T) {
	cases := []struct {
		name       string
		lang       string
		rows       [][3]string
		wantLocal  string
		wantEn     string
		wantNoMatc bool
	}{
		{
			name:      "label wins over P1843",
			lang:      "de",
			rows:      [][3]string{{"common", "de", "Kugeldistel"}, {"label", "de", "Banater Kugeldistel"}},
			wantLocal: "Banater Kugeldistel",
		},
		{
			name:      "label equal to scientific name is ignored, single P1843 used",
			lang:      "fr",
			rows:      [][3]string{{"label", "fr", "Echinops bannaticus"}, {"common", "fr", "Boule azurée"}},
			wantLocal: "Boule azurée",
		},
		{
			name: "ambiguous P1843 (two values) is not trusted",
			lang: "zh-Hans",
			rows: [][3]string{{"common", "zh-hans", "小蓝刺头"}, {"common", "zh-hans", "欧亚蓝刺头"}},
		},
		{
			name:      "zh-Hant takes zh-tw, en label becomes English",
			lang:      "zh-Hant",
			rows:      [][3]string{{"label", "zh-tw", "藍刺頭"}, {"label", "en", "blue globe thistle"}},
			wantLocal: "藍刺頭",
			wantEn:    "blue globe thistle",
		},
		{
			name:       "no bindings = no match",
			lang:       "en",
			rows:       nil,
			wantNoMatc: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wd, done := wikidataStub(t, http.StatusOK, tc.rows)
			defer done()
			local, en, err := wd.CommonNames(context.Background(), "Echinops bannaticus", tc.lang)
			if tc.wantNoMatc {
				if !errors.Is(err, errNameNoMatch) {
					t.Fatalf("err = %v, want errNameNoMatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if local != tc.wantLocal || en != tc.wantEn {
				t.Errorf("got (%q, %q), want (%q, %q)", local, en, tc.wantLocal, tc.wantEn)
			}
		})
	}
}

// fakeLookup builds a commonNameLookup returning fixed values.
func fakeLookup(local, en string, err error) commonNameLookup {
	return func(context.Context, string, string) (string, string, error) { return local, en, err }
}

func newTestResolver(inat, wd commonNameLookup) *NameResolver {
	r := NewNameResolver(nil, nil)
	r.inat, r.wikidata = inat, wd
	return r
}

func TestNameResolverPrecedence(t *testing.T) {
	outage := errors.New("timeout")
	cases := []struct {
		name     string
		inat, wd commonNameLookup
		want     ResolvedName
		wantOK   bool
	}{
		{"iNat localized wins", fakeLookup("玫瑰车轴草", "Rose Clover", nil), fakeLookup("别名", "x", nil),
			ResolvedName{"玫瑰车轴草", NameSourceINat}, true},
		{"Wikidata localized when iNat has none", fakeLookup("", "Rose Clover", nil), fakeLookup("玫瑰三叶草", "", nil),
			ResolvedName{"玫瑰三叶草", NameSourceWikidata}, true},
		{"Wikidata localized still used when iNat is down", fakeLookup("", "", outage), fakeLookup("玫瑰三叶草", "", nil),
			ResolvedName{"玫瑰三叶草", NameSourceWikidata}, true},
		{"no localized + a source down → unresolved", fakeLookup("", "Rose Clover", nil), fakeLookup("", "", outage),
			ResolvedName{}, false},
		{"English from iNat", fakeLookup("", "Rose Clover", nil), fakeLookup("", "rose clover wd", nil),
			ResolvedName{"Rose Clover", NameSourceINatEnglish}, true},
		{"English from Wikidata", fakeLookup("", "", errNameNoMatch), fakeLookup("", "rose clover", nil),
			ResolvedName{"rose clover", NameSourceWikidataEn}, true},
		{"nothing anywhere → scientific name", fakeLookup("", "", errNameNoMatch), fakeLookup("", "", errNameNoMatch),
			ResolvedName{"Trifolium hirtum", NameSourceScientificName}, true},
		{"name equal to scientific name is not a common name", fakeLookup("Trifolium hirtum", "", nil), nil,
			ResolvedName{"Trifolium hirtum", NameSourceScientificName}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := newTestResolver(tc.inat, tc.wd).Resolve(context.Background(), "Trifolium hirtum", "zh-Hans")
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("got (%+v, %v), want (%+v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A hung source is abandoned at the resolver timeout instead of blocking the
// user; with a localized answer from the other source the result still resolves.
func TestNameResolverTimeout(t *testing.T) {
	hang := func(ctx context.Context, _, _ string) (string, string, error) {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	r := newTestResolver(hang, fakeLookup("Sumpfporst", "", nil))
	r.timeout = 50 * time.Millisecond
	start := time.Now()
	got, ok := r.Resolve(context.Background(), "Rhododendron tomentosum", "de")
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Resolve took %v, want ≈ timeout", el)
	}
	if !ok || got.Name != "Sumpfporst" || got.Source != NameSourceWikidata {
		t.Errorf("got (%+v, %v), want Sumpfporst from wikidata", got, ok)
	}
}

// Fresh generation: a localized authoritative name is the LLM hint AND is baked
// into the stored master, so title and description prose agree.
func TestGetOrGenerate_GenerateUsesAuthoritativeNameAsHint(t *testing.T) {
	db := &stubDB{}
	llm := &stubLLM{ret: &proxy.PlantDetail{CommonName: "LLM编的名字", Description: "d"}}
	svc := NewService(nil, db, llm, NewCache(10, time.Hour), nil)
	svc.SetNameResolver(newTestResolver(fakeLookup("玫瑰车轴草", "Rose Clover", nil), nil))

	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Trifolium hirtum", CommonName: "rose clover", Lang: "zh-Hans"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceSupabaseMissGenerate {
		t.Fatalf("source = %q", src)
	}
	if len(llm.calls) != 1 || llm.calls[0].Common != "玫瑰车轴草" {
		t.Errorf("LLM hint = %+v, want 玫瑰车轴草", llm.calls)
	}
	if got.CommonName != "玫瑰车轴草" {
		t.Errorf("response name = %q", got.CommonName)
	}
	if len(db.insertCalls) != 1 || db.insertCalls[0].Data.CommonName != "玫瑰车轴草" || db.insertCalls[0].Data.CommonNameSource != NameSourceINat {
		t.Errorf("stored master name not baked: %+v", db.insertCalls)
	}
}

// English / scientific-name fallbacks are applied at response time but NOT
// baked into the stored row (a better source may appear later).
func TestGetOrGenerate_FallbackNameNotBaked(t *testing.T) {
	db := &stubDB{}
	llm := &stubLLM{ret: &proxy.PlantDetail{CommonName: "Rauhaariger Klee"}}
	svc := NewService(nil, db, llm, NewCache(10, time.Hour), nil)
	svc.SetNameResolver(newTestResolver(fakeLookup("", "Rose Clover", nil), nil))

	got, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Trifolium hirtum", Lang: "de"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if got.CommonName != "Rose Clover" {
		t.Errorf("response name = %q, want Rose Clover", got.CommonName)
	}
	if len(db.insertCalls) != 1 || db.insertCalls[0].Data.CommonName != "Rauhaariger Klee" {
		t.Errorf("fallback name must not be baked into the row: %+v", db.insertCalls)
	}
}

// The English-row display fallback (requested lang missing) still gets the
// name in the REQUESTED language.
func TestGetOrGenerate_FallbackEnRowGetsRequestedLangName(t *testing.T) {
	db := &stubDB{lookupQ: []dbLookupResult{{pd: nil}, {pd: &proxy.PlantDetail{CommonName: "Rose Clover"}}}}
	svc := NewService(nil, db, &stubLLM{}, NewCache(10, time.Hour), nil)
	svc.SetNameResolver(newTestResolver(fakeLookup("ビロードアカツメクサ", "Rose Clover", nil), nil))

	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Trifolium hirtum", Lang: "ja"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceSupabaseFallbackEn || got.CommonName != "ビロードアカツメクサ" {
		t.Errorf("src=%q name=%q", src, got.CommonName)
	}
}
