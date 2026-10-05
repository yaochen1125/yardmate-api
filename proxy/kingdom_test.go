package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func kingdomStr(k *string) string {
	if k == nil {
		return "<nil>"
	}
	return *k
}

func TestNormalizeKingdom(t *testing.T) {
	cases := map[string]string{
		"Fungi":     "Fungi",
		"fungi":     "Fungi",
		" FUNGI ":   "Fungi",
		"Plantae":   "Plantae",
		"plantae":   "Plantae",
		"Other":     "<nil>",
		"Animalia":  "<nil>",
		"Chromista": "<nil>",
		"":          "<nil>",
		"Fungus":    "<nil>", // only the canonical kingdom name counts
	}
	for in, want := range cases {
		if got := kingdomStr(NormalizeKingdom(in)); got != want {
			t.Errorf("NormalizeKingdom(%q) = %s, want %s", in, got, want)
		}
	}
}

// The merge rule is biased toward safety: ANY Fungi verdict wins regardless of
// order; otherwise the first determinable verdict; all-unknown stays nil.
func TestMergeKingdom_FungiWins(t *testing.T) {
	cases := []struct {
		name string
		in   []*string
		want string
	}{
		{"none", nil, "<nil>"},
		{"all nil", []*string{nil, nil}, "<nil>"},
		{"only unknown tokens", []*string{strPtr("Other"), strPtr("")}, "<nil>"},
		{"plantae only", []*string{nil, strPtr("Plantae")}, "Plantae"},
		{"fungi only", []*string{strPtr("fungi")}, "Fungi"},
		{"llm plantae + inat fungi", []*string{strPtr("Plantae"), strPtr("Fungi")}, "Fungi"},
		{"llm fungi + inat plantae", []*string{strPtr("Fungi"), strPtr("Plantae")}, "Fungi"},
		{"unknown + fungi", []*string{strPtr("Other"), nil, strPtr("Fungi")}, "Fungi"},
		{"unknown + plantae", []*string{strPtr("Other"), strPtr("Plantae")}, "Plantae"},
	}
	for _, c := range cases {
		if got := kingdomStr(MergeKingdom(c.in...)); got != c.want {
			t.Errorf("%s: MergeKingdom = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestMergeKingdom_DoesNotAliasInput(t *testing.T) {
	in := strPtr("Fungi")
	out := MergeKingdom(in)
	if out == in {
		t.Fatal("MergeKingdom returned the caller's pointer; want a fresh one")
	}
}

func TestSanitizeFungiDetail_StripsFoodUses(t *testing.T) {
	uses := []UseItem{
		{Icon: "culinary", Text: "Prized in risotto"},
		{Icon: "ornamental", Text: "Striking red cap"},
		{Icon: "Edible", Text: "Eaten when young"},
		{Icon: "medicinal", Text: "Used in folk medicine"},
	}
	attrs := []string{"edible", "fast-growing", "EDIBLE", "shade-tolerant"}
	d := &PlantDetail{Kingdom: strPtr("Fungi"), UsesList: uses, Attributes: attrs}

	if !SanitizeFungiDetail(d) {
		t.Fatal("SanitizeFungiDetail = false, want true (food content present)")
	}
	wantUses := []UseItem{{Icon: "ornamental", Text: "Striking red cap"}, {Icon: "medicinal", Text: "Used in folk medicine"}}
	if !reflect.DeepEqual(d.UsesList, wantUses) {
		t.Errorf("UsesList = %+v, want %+v", d.UsesList, wantUses)
	}
	if want := []string{"fast-growing", "shade-tolerant"}; !reflect.DeepEqual(d.Attributes, want) {
		t.Errorf("Attributes = %v, want %v", d.Attributes, want)
	}
	// The caller's original backing arrays must be untouched (a shallow copy of a
	// cached record shares them).
	if uses[0].Icon != "culinary" || len(uses) != 4 || attrs[0] != "edible" {
		t.Errorf("input slices were mutated: uses=%+v attrs=%v", uses, attrs)
	}
	// Idempotent.
	if SanitizeFungiDetail(d) {
		t.Error("second SanitizeFungiDetail = true, want false (nothing left to strip)")
	}
}

func TestSanitizeFungiDetail_NonFungiUntouched(t *testing.T) {
	for _, k := range []*string{nil, strPtr("Plantae"), strPtr("Other")} {
		d := &PlantDetail{
			Kingdom:    k,
			UsesList:   []UseItem{{Icon: "culinary", Text: "Leaves used as a herb"}},
			Attributes: []string{"edible"},
		}
		if SanitizeFungiDetail(d) {
			t.Errorf("kingdom=%s: sanitized a non-fungus", kingdomStr(k))
		}
		if len(d.UsesList) != 1 || len(d.Attributes) != 1 {
			t.Errorf("kingdom=%s: content changed: %+v %v", kingdomStr(k), d.UsesList, d.Attributes)
		}
	}
	if SanitizeFungiDetail(nil) {
		t.Error("SanitizeFungiDetail(nil) = true")
	}
}

func TestContentIndex_KingdomHints(t *testing.T) {
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	if k := content.KingdomFor("Amanita muscaria"); k != nil {
		t.Fatalf("KingdomFor before any note = %s, want nil", *k)
	}
	content.NoteKingdom("Amanita muscaria", strPtr("fungi"))
	// Lookup is species-level + case/variety-insensitive like every other name key.
	for _, q := range []string{"Amanita muscaria", "amanita  MUSCARIA", "Amanita muscaria var. guessowii"} {
		if k := content.KingdomFor(q); kingdomStr(k) != "Fungi" {
			t.Errorf("KingdomFor(%q) = %s, want Fungi", q, kingdomStr(k))
		}
	}
	// Fungi is sticky: a later Plantae note must not downgrade it.
	content.NoteKingdom("Amanita muscaria", strPtr("Plantae"))
	if k := content.KingdomFor("Amanita muscaria"); kingdomStr(k) != "Fungi" {
		t.Errorf("after Plantae note = %s, want Fungi (sticky)", kingdomStr(k))
	}
	// Plantae → Fungi upgrade is allowed.
	content.NoteKingdom("Fakeus plantus", strPtr("Plantae"))
	content.NoteKingdom("Fakeus plantus", strPtr("Fungi"))
	if k := content.KingdomFor("Fakeus plantus"); kingdomStr(k) != "Fungi" {
		t.Errorf("Plantae→Fungi = %s, want Fungi", kingdomStr(k))
	}
	// Undeterminable notes are dropped.
	content.NoteKingdom("Otherus thingus", strPtr("Other"))
	content.NoteKingdom("Nilus thingus", nil)
	if content.KingdomFor("Otherus thingus") != nil || content.KingdomFor("Nilus thingus") != nil {
		t.Error("an undeterminable kingdom was recorded")
	}
	// A curated catalog plant carries no kingdom in plants_detail.json today → nil.
	if k := content.KingdomFor("Abelia chinensis"); k != nil {
		t.Errorf("catalog plant kingdom = %s, want nil (catalog ships no kingdom)", *k)
	}
}

func TestContentIndex_KingdomNilSafe(t *testing.T) {
	var nilIdx *ContentIndex
	nilIdx.NoteKingdom("Amanita muscaria", strPtr("Fungi")) // must not panic
	if nilIdx.KingdomFor("Amanita muscaria") != nil {
		t.Error("nil index returned a kingdom")
	}
	bare := &ContentIndex{} // hand-built index: no hint store
	bare.NoteKingdom("Amanita muscaria", strPtr("Fungi"))
	if bare.KingdomFor("Amanita muscaria") != nil {
		t.Error("bare index returned a kingdom")
	}
}

// The wire contract: `kingdom` is ALWAYS present, as a string or an explicit
// JSON null (never omitted), on all three shapes.
func TestKingdom_JSONSerialization(t *testing.T) {
	check := func(name string, v any, want string) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		got, ok := m["kingdom"]
		if !ok {
			t.Fatalf("%s: kingdom key missing from %s", name, raw)
		}
		if string(got) != want {
			t.Errorf("%s: kingdom = %s, want %s", name, got, want)
		}
	}
	check("Suggestion nil", Suggestion{}, `null`)
	check("Suggestion fungi", Suggestion{Kingdom: strPtr(KingdomFungi)}, `"Fungi"`)
	check("PlantSuggestion nil", PlantSuggestion{}, `null`)
	check("PlantSuggestion plantae", PlantSuggestion{Kingdom: strPtr(KingdomPlantae)}, `"Plantae"`)
	check("PlantDetail nil", PlantDetail{}, `null`)
	check("PlantDetail fungi", PlantDetail{Kingdom: strPtr(KingdomFungi)}, `"Fungi"`)

	// A row persisted before the field existed decodes to nil (→ null), not "".
	var legacy PlantDetail
	if err := json.Unmarshal([]byte(`{"scientific_name":"Amanita muscaria"}`), &legacy); err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if legacy.Kingdom != nil {
		t.Errorf("legacy row kingdom = %q, want nil", *legacy.Kingdom)
	}
}

// --- iNat LookupTaxon ---

const inatBodyFlyAgaric = `{"results":[
	{"name":"Amanita muscaria","rank":"species","preferred_common_name":"Fly Agaric","iconic_taxon_name":"Fungi"},
	{"name":"Amanita muscaria guessowii","rank":"variety","preferred_common_name":"American Yellow Fly Agaric","iconic_taxon_name":"Fungi"}
]}`

func TestINatLookupTaxon_FungusResolvedInOneRequest(t *testing.T) {
	var calls int
	var gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotScope = r.URL.Query().Get("taxon_id")
		_, _ = io.WriteString(w, inatBodyFlyAgaric)
	}))
	defer srv.Close()
	c := &INatClient{HTTP: srv.Client(), BaseURL: srv.URL}

	got, ok := c.LookupTaxon(context.Background(), "Amanita muscaria")
	if !ok || got.Kingdom != "Fungi" || got.CommonName != "Fly Agaric" {
		t.Fatalf("LookupTaxon = (%+v,%v), want Fungi / Fly Agaric", got, ok)
	}
	if calls != 1 {
		t.Errorf("iNat calls = %d, want 1 (name + kingdom share one round-trip)", calls)
	}
	// Must NOT be scoped to Plantae only — a fungus is invisible under 47126.
	if gotScope != "47126,47170" {
		t.Errorf("taxon_id scope = %q, want 47126,47170 (Plantae + Fungi)", gotScope)
	}
}

func TestINatLookupTaxon_OnlyExactNameTrusted(t *testing.T) {
	// q is fuzzy: a different (fungal) taxon first must not flag the queried plant.
	c, done := newINatStub(t, `{"results":[
		{"name":"Amanita pantherina","rank":"species","preferred_common_name":"Panthercap","iconic_taxon_name":"Fungi"}
	]}`)
	defer done()
	if got, ok := c.LookupTaxon(context.Background(), "Amanita muscaria"); ok {
		t.Fatalf("mismatched taxon trusted: %+v", got)
	}
}

func TestINatLookupTaxon_KingdomWithoutCommonName(t *testing.T) {
	c, done := newINatStub(t, `{"results":[
		{"name":"Obscurus fungus","rank":"species","iconic_taxon_name":"Fungi"}
	]}`)
	defer done()
	got, ok := c.LookupTaxon(context.Background(), "obscurus fungus")
	if !ok || got.Kingdom != "Fungi" || got.CommonName != "" {
		t.Fatalf("LookupTaxon = (%+v,%v), want kingdom Fungi with empty common name", got, ok)
	}
}

func TestINatLookupTaxon_CrossKingdomHomonymPrefersFungi(t *testing.T) {
	c, done := newINatStub(t, `{"results":[
		{"name":"Homonymus dubius","rank":"species","preferred_common_name":"Plant One","iconic_taxon_name":"Plantae"},
		{"name":"Homonymus dubius","rank":"species","preferred_common_name":"Fungus Two","iconic_taxon_name":"Fungi"}
	]}`)
	defer done()
	got, ok := c.LookupTaxon(context.Background(), "Homonymus dubius")
	if !ok || got.Kingdom != "Fungi" {
		t.Fatalf("LookupTaxon = (%+v,%v), want Fungi (safety bias)", got, ok)
	}
}

func TestINatLookupTaxon_FailuresAreNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, ok := (&INatClient{HTTP: srv.Client(), BaseURL: srv.URL}).LookupTaxon(context.Background(), "Amanita muscaria"); ok {
		t.Error("5xx reported ok")
	}
	var nilClient *INatClient
	if _, ok := nilClient.LookupTaxon(context.Background(), "Amanita muscaria"); ok {
		t.Error("nil client reported ok")
	}
	bad, done := newINatStub(t, `not json`)
	defer done()
	if _, ok := bad.LookupTaxon(context.Background(), "Amanita muscaria"); ok {
		t.Error("malformed body reported ok")
	}
}

// The identify-path helper keeps its plants-only scope — widening it would let a
// fungal common name leak onto a plant candidate.
func TestINatPreferredCommonName_StillPlantaeScoped(t *testing.T) {
	var gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotScope = r.URL.Query().Get("taxon_id")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	(&INatClient{HTTP: srv.Client(), BaseURL: srv.URL}).PreferredCommonName(context.Background(), "Triteleia ixioides")
	if gotScope != "47126" {
		t.Errorf("taxon_id scope = %q, want 47126", gotScope)
	}
}

// --- GPT vision self-report ---

func TestIdentifyPlant_KingdomSelfReport(t *testing.T) {
	reply := func(kingdomField string) string {
		return `{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"Amanita muscaria\",\"common_names\":[],\"confidence\":0.8` + kingdomField + `}"}}]}`
	}
	cases := []struct{ field, want string }{
		{`,\"kingdom\":\"Fungi\"`, "Fungi"},
		{`,\"kingdom\":\"Plantae\"`, "Plantae"},
		{`,\"kingdom\":\"Other\"`, "<nil>"},
		{``, "<nil>"}, // field absent (older model reply) → undetermined
	}
	for _, c := range cases {
		vision, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, reply(c.field))
		})
		sug, err := vision.IdentifyPlant(context.Background(), jpegMagic, "image/jpeg")
		srv.Close()
		if err != nil {
			t.Fatalf("field=%q: IdentifyPlant: %v", c.field, err)
		}
		if got := kingdomStr(sug.Kingdom); got != c.want {
			t.Errorf("field=%q: Kingdom = %s, want %s", c.field, got, c.want)
		}
	}
}

// Strict json_schema requires every property to be listed in `required`, or
// OpenAI 400s the whole request — which on the identify path means losing the
// GPT fallback entirely.
func TestVisionSchemas_KingdomIsRequiredEnum(t *testing.T) {
	schemas := map[string]map[string]any{
		"identify": visionIdentifySchema["json_schema"].(map[string]any)["schema"].(map[string]any),
		"diagnose": buildVisionDiagnoseSchema("")["json_schema"].(map[string]any)["schema"].(map[string]any),
	}
	for name, schema := range schemas {
		props := schema["properties"].(map[string]any)
		k, ok := props["kingdom"].(map[string]any)
		if !ok {
			t.Fatalf("%s: kingdom property missing", name)
		}
		if got := k["enum"].([]string); !reflect.DeepEqual(got, []string{"Plantae", "Fungi", "Other"}) {
			t.Errorf("%s: kingdom enum = %v", name, got)
		}
		required := schema["required"].([]string)
		if len(required) != len(props) {
			t.Errorf("%s: required has %d keys, properties %d — strict mode needs them equal", name, len(required), len(props))
		}
		found := false
		for _, r := range required {
			found = found || r == "kingdom"
		}
		if !found {
			t.Errorf("%s: kingdom not in required", name)
		}
	}
}

// --- handler responses ---

// rawSuggestionKingdoms returns each suggestion's raw `kingdom` JSON value,
// failing if the key is absent (the contract is an explicit null, not omission).
func rawSuggestionKingdoms(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Suggestions []map[string]json.RawMessage `json:"suggestions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	out := make([]string, 0, len(resp.Suggestions))
	for i, s := range resp.Suggestions {
		raw, ok := s["kingdom"]
		if !ok {
			t.Fatalf("suggestions[%d] has no kingdom key: %s", i, body)
		}
		out = append(out, string(raw))
	}
	return out
}

func TestHandleIdentify_GPTFungusCandidateCarriesKingdom(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"Amanita muscaria\",\"common_names\":[\"Fly agaric\"],\"confidence\":0.71,\"kingdom\":\"Fungi\"}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound) // PlantNet valid no-match → GPT names it
			_, _ = io.WriteString(w, cannedPlantNetNoMatch)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	got := rawSuggestionKingdoms(t, rec.Body.Bytes())
	if len(got) != 1 || got[0] != `"Fungi"` {
		t.Fatalf("suggestion kingdoms = %v, want [\"Fungi\"] body=%s", got, rec.Body.String())
	}
}

// Engine candidates have no kingdom source on the identify path (no network call
// may be added there) → explicit null on every suggestion.
func TestHandleIdentify_EngineCandidatesKingdomNull(t *testing.T) {
	h, cleanup := newCascadeHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedPlantNetIdentifyOK)
	}, nil)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	got := rawSuggestionKingdoms(t, rec.Body.Bytes())
	if len(got) == 0 {
		t.Fatalf("no suggestions: %s", rec.Body.String())
	}
	for i, k := range got {
		if k != `null` {
			t.Errorf("suggestions[%d].kingdom = %s, want null", i, k)
		}
	}
}

func diagnoseTopKingdom(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Top map[string]json.RawMessage `json:"top"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	raw, ok := resp.Top["kingdom"]
	if !ok {
		t.Fatalf("top has no kingdom key: %s", body)
	}
	return string(raw)
}

func doDiagnoseReq(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandleDiagnose_VisionFungusTopCarriesKingdom(t *testing.T) {
	vision, vsrv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Amanita muscaria\",\"common_names\":[],\"confidence\":0.8,\"kingdom\":\"Fungi\",\"is_healthy\":false,\"health_probability\":0.3,\"issues\":[{\"name\":\"Powdery mildew\",\"confidence\":0.5,\"cause\":\"c\",\"description\":\"d\",\"treatment\":{\"biological\":[],\"chemical\":[],\"prevention\":[]}}]}"}}]}`)
	})
	defer vsrv.Close()
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // Plant.id down → GPT diagnose fallback
	}, vision)
	defer srv.Close()

	rec := doDiagnoseReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 body=%s", rec.Code, rec.Body)
	}
	if got := diagnoseTopKingdom(t, rec.Body.Bytes()); got != `"Fungi"` {
		t.Errorf("top.kingdom = %s, want \"Fungi\"", got)
	}
}

func TestHandleDiagnose_PlantIDTopKingdomNull(t *testing.T) {
	h, srv := newDiagnoseHandler(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedDiagnoseUnhealthy)
	}, nil)
	defer srv.Close()

	rec := doDiagnoseReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 body=%s", rec.Code, rec.Body)
	}
	if got := diagnoseTopKingdom(t, rec.Body.Bytes()); got != `null` {
		t.Errorf("top.kingdom = %s, want null (Plant.id reports no kingdom)", got)
	}
	if !strings.Contains(rec.Body.String(), `"kingdom":null`) {
		t.Errorf("body lacks an explicit kingdom null: %s", rec.Body)
	}
}

// --- fungi are identifiable subjects (product decision 2026-10) ---

// The prompt must not define is_plant as "plant only" while asking for
// kingdom=Fungi: a model following it literally would answer is_plant=false for
// every mushroom and the handler would drop it before reading the kingdom.
func TestIdentifyPlant_PromptTreatsFungiAsIdentifiable(t *testing.T) {
	var sys, user string
	vision, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "system" {
				_ = json.Unmarshal(m.Content, &sys)
			} else {
				user = string(m.Content)
			}
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"Amanita muscaria\",\"common_names\":[],\"confidence\":0.8,\"kingdom\":\"Fungi\"}"}}]}`)
	})
	defer srv.Close()
	if _, err := vision.IdentifyPlant(context.Background(), jpegMagic, "image/jpeg"); err != nil {
		t.Fatalf("IdentifyPlant: %v", err)
	}
	for _, want := range []string{
		"plant OR fungus species",
		"is_plant = true if the image shows a real plant or a real fungus",
		"MUST be reported with is_plant = true",
		"to species level for fungi",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "false for anything else (an object") && !strings.Contains(sys, "false only for anything else") {
		t.Errorf("system prompt still gates is_plant on plants only:\n%s", sys)
	}
	if !strings.Contains(user, "plant or fungus") {
		t.Errorf("user message = %s, want it to mention fungus", user)
	}
	isPlantDesc := visionIdentifySchema["json_schema"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["is_plant"].(map[string]any)["description"].(string)
	if !strings.Contains(isPlantDesc, "OR a fungus") {
		t.Errorf("is_plant schema description still plant-only: %q", isPlantDesc)
	}
}

// Code backstop: is_plant=false but a named Fungi species IS an identification.
func TestIdentifyPlant_FungiBackstopOverridesIsPlantFalse(t *testing.T) {
	reply := func(inner string) string {
		return `{"choices":[{"message":{"content":"` + inner + `"}}]}`
	}
	cases := []struct {
		name       string
		inner      string
		wantErr    error
		wantSci    string
		wantFungus bool
	}{
		{"false + Fungi + species → identified",
			`{\"is_plant\":false,\"scientific_name\":\"Amanita phalloides\",\"common_names\":[\"Death cap\"],\"confidence\":0.7,\"kingdom\":\"Fungi\"}`,
			nil, "Amanita phalloides", true},
		{"false + fungi lowercase → identified",
			`{\"is_plant\":false,\"scientific_name\":\"Amanita phalloides\",\"common_names\":[],\"confidence\":0.7,\"kingdom\":\"fungi\"}`,
			nil, "Amanita phalloides", true},
		{"false + Fungi + blank name → still not a plant",
			`{\"is_plant\":false,\"scientific_name\":\"  \",\"common_names\":[],\"confidence\":0.1,\"kingdom\":\"Fungi\"}`,
			ErrVisionNotAPlant, "", false},
		{"false + Plantae → not a plant (unchanged)",
			`{\"is_plant\":false,\"scientific_name\":\"Rosa chinensis\",\"common_names\":[],\"confidence\":0.1,\"kingdom\":\"Plantae\"}`,
			ErrVisionNotAPlant, "", false},
		{"false + Other → not a plant (unchanged)",
			`{\"is_plant\":false,\"scientific_name\":\"Felis catus\",\"common_names\":[],\"confidence\":0.9,\"kingdom\":\"Other\"}`,
			ErrVisionNotAPlant, "", false},
	}
	for _, c := range cases {
		vision, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, reply(c.inner))
		})
		sug, err := vision.IdentifyPlant(context.Background(), jpegMagic, "image/jpeg")
		srv.Close()
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("%s: err = %v, want %v", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected err %v", c.name, err)
			continue
		}
		if sug.ScientificName != c.wantSci || IsFungi(sug.Kingdom) != c.wantFungus {
			t.Errorf("%s: got %q kingdom=%s", c.name, sug.ScientificName, kingdomStr(sug.Kingdom))
		}
	}
}

func TestDiagnosePlant_PromptCoversFungi(t *testing.T) {
	var sys string
	vision, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "system" {
				_ = json.Unmarshal(m.Content, &sys)
			}
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"scientific_name\":\"Amanita muscaria\",\"common_names\":[],\"confidence\":0.8,\"kingdom\":\"Fungi\",\"is_healthy\":true,\"health_probability\":0.9,\"issues\":[]}"}}]}`)
	})
	defer srv.Close()
	vr, err := vision.DiagnosePlant(context.Background(), jpegMagic, "image/jpeg", "en")
	if err != nil {
		t.Fatalf("DiagnosePlant: %v", err)
	}
	if vr.Kingdom != "Fungi" {
		t.Errorf("kingdom = %q, want Fungi", vr.Kingdom)
	}
	if !strings.Contains(sys, "If the subject is a fungus") || !strings.Contains(sys, "still identify it to species") {
		t.Errorf("diagnose system prompt does not cover fungi:\n%s", sys)
	}
}

const gptFlyAgaric = `{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"Amanita muscaria\",\"common_names\":[\"Fly agaric\"],\"confidence\":0.7,\"kingdom\":\"Fungi\"}"}}]}`

func plantIDEngineBody(sci string, prob string) string {
	return `{"result":{"is_plant":{"probability":0.98,"binary":true},"classification":{"suggestions":[{"name":"` + sci + `","probability":` + prob + `,"details":{"common_names":[],"scientific_name":"` + sci + `"}}]}}}`
}

// The engines are plant-only, so on a mushroom photo they return plants. A GPT
// Fungi verdict must win over a WEAK engine candidate — in-catalog or not —
// exactly where the not-a-plant verdict used to win (it produced the Unknown
// sentinel; now the mushroom is identified instead of shown as a garden plant).
func TestHandleIdentify_GPTFungusBeatsWeakEngineCandidates(t *testing.T) {
	cases := map[string]string{
		"weak in-catalog hit":       plantIDEngineBody("Abelia chinensis", "0.40"),
		"weak out-of-catalog guess": plantIDEngineBody("Zzzz nonexistent plantii", "0.30"),
	}
	for name, engine := range cases {
		result := runArbiterIdentify(t, engine, gptFlyAgaric)
		if len(result.Suggestions) != 1 {
			t.Fatalf("%s: suggestions = %+v, want only the fungus", name, result.Suggestions)
		}
		s0 := result.Suggestions[0]
		if s0.ScientificName != "Amanita muscaria" || !IsFungi(s0.Kingdom) || s0.PlantID != nil {
			t.Errorf("%s: top = %+v kingdom=%s, want Amanita muscaria / Fungi / no plant_id", name, s0, kingdomStr(s0.Kingdom))
		}
		if !result.IsPlant {
			t.Errorf("%s: is_plant = false, want true (a mushroom is a successful identification)", name)
		}
	}
}

// A CONFIDENT engine answer keeps its existing trust (>= 0.80), mirroring how the
// not-a-plant verdict is ignored there — guards against a GPT false Fungi call.
func TestHandleIdentify_GPTFungusDoesNotOverrideConfidentEngine(t *testing.T) {
	inCat := runArbiterIdentify(t, plantIDEngineBody("Abelia chinensis", "0.90"), gptFlyAgaric)
	if inCat.Suggestions[0].PlantID == nil || *inCat.Suggestions[0].PlantID != "AAA0001" {
		t.Errorf("confident in-catalog engine displaced: %+v", inCat.Suggestions[0])
	}
	oob := runConfidentOOBIdentify(t, gptFlyAgaric)
	if oob.Suggestions[0].Name != "Zzzz nonexistent plantii" || IsFungi(oob.Suggestions[0].Kingdom) {
		t.Errorf("confident out-of-catalog engine displaced: %+v", oob.Suggestions[0])
	}
}

// is_plant=false + Fungi must reach the client as the mushroom, not the Unknown
// sentinel, even through the full handler.
func TestHandleIdentify_FungiBackstopNotUnknownSentinel(t *testing.T) {
	result := runArbiterIdentify(t, plantIDEngineBody("Abelia chinensis", "0.40"),
		`{"choices":[{"message":{"content":"{\"is_plant\":false,\"scientific_name\":\"Amanita muscaria\",\"common_names\":[],\"confidence\":0.6,\"kingdom\":\"Fungi\"}"}}]}`)
	s0 := result.Suggestions[0]
	if s0.ScientificName != "Amanita muscaria" || !IsFungi(s0.Kingdom) {
		t.Errorf("top = %+v, want the fungus (not AAA0000 / the weak plant)", s0)
	}
}

// Fungi stickiness must hold under concurrent writers (run with -race).
func TestContentIndex_KingdomHintsConcurrentFungiSticky(t *testing.T) {
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	for round := 0; round < 50; round++ {
		name := "Concurrentus fungus" + strings.Repeat("a", round%7)
		content.NoteKingdom(name, strPtr("Fungi"))
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					content.NoteKingdom(name, strPtr("Plantae"))
				} else {
					content.NoteKingdom(name, strPtr("Fungi"))
				}
			}(i)
		}
		wg.Wait()
		if k := content.KingdomFor(name); !IsFungi(k) {
			t.Fatalf("round %d: kingdom = %s, want Fungi (sticky)", round, kingdomStr(k))
		}
	}
}

func TestINatLookupTaxonErr_DistinguishesFailureFromMiss(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	c := &INatClient{HTTP: srv.Client(), BaseURL: srv.URL}

	if _, ok, err := c.LookupTaxonErr(context.Background(), "Nomatchus here"); ok || err != nil {
		t.Errorf("empty result = (ok=%v, err=%v), want a definitive miss (false, nil)", ok, err)
	}
	for _, code := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		status = code
		_, ok, err := c.LookupTaxonErr(context.Background(), "Amanita muscaria")
		var se *INatStatusError
		if ok || !errors.As(err, &se) || se.Status != code {
			t.Errorf("status %d = (ok=%v, err=%v), want INatStatusError{%d}", code, ok, err, code)
		}
	}
}
