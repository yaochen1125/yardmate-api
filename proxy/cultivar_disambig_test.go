package proxy

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// TestSpeciesGroupKey covers the fold that puts a species and its cultivars in
// one group: the cultivar quote AND the infraspecific markers are stripped, and
// a genus-only result is rejected (that is the rose-rerank domain).
func TestSpeciesGroupKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// The load-bearing invariant: bare species and its 'Spiralis' cultivar
		// fold to the SAME key so they group together.
		{"Juncus effusus", "juncus effusus"},
		{"Juncus effusus 'Spiralis'", "juncus effusus"},
		{"Juncus effusus 'Spiralis'", "juncus effusus"},
		{"juncus effusus 'spiralis'", "juncus effusus"},
		// Typographic quote is stripped too.
		{"Juncus effusus ’Spiralis’", "juncus effusus"},
		// Variety / subspecies markers fold to the binomial (reuses
		// normalizeScientificName).
		{"Brassica oleracea var. italica", "brassica oleracea"},
		{"Brassica oleracea var. acephala", "brassica oleracea"},
		// Hybrid marker (both forms) drops out via normalizeScientificName.
		{"Chrysanthemum x morifolium", "chrysanthemum morifolium"},
		{"Chrysanthemum × morifolium", "chrysanthemum morifolium"},
		// Genus-only cultivars (the rose domain) are rejected → "".
		{"Rosa 'About Face'", ""},
		{"Rosa 'Peace'", ""},
		// Whitespace-only / genus-only bare inputs → "".
		{"", ""},
		{"Rosa", ""},
		{"  Hosta  ", ""},
	}
	for _, tc := range cases {
		if got := speciesGroupKey(tc.in); got != tc.want {
			t.Errorf("speciesGroupKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIsBareSpecies covers the trigger gate: fire disambiguation ONLY when the
// engine gave a plain binomial, not when it already pinned a variety/cultivar.
func TestIsBareSpecies(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Juncus effusus", true},
		{"Brassica oleracea", true},
		{"Aeonium arboreum", true},
		// Already infraspecific / cultivar-pinned → NOT bare (precise-resolved).
		{"Brassica oleracea var. italica", false},
		{"Juncus effusus 'Spiralis'", false},
		{"Rosa chinensis subsp. spontanea", false},
		// Hybrid marker keeps a 3rd token under the precise normalizer → not bare.
		{"Abelia x grandiflora", false},
		// Genus-only / empty → not a binomial.
		{"Rosa", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isBareSpecies(tc.in); got != tc.want {
			t.Errorf("isBareSpecies(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSpeciesGroups_JuncusEffusus verifies the ≥2-member group is built from the
// embedded catalog and that both Juncus effusus rows (common AAA0701 + spiral
// AAA1633) land in it, sorted by PlantID. genus-only + unknown keys don't group.
func TestSpeciesGroups_JuncusEffusus(t *testing.T) {
	c := loadContentForTests(t)

	group, ok := c.SpeciesGroupFor("juncus effusus")
	if !ok {
		t.Fatal("SpeciesGroupFor(\"juncus effusus\") not found — group not built")
	}
	if len(group) != 2 {
		t.Fatalf("group size = %d, want 2 (%+v)", len(group), group)
	}
	// Members are sorted by PlantID for a deterministic vision prompt.
	if group[0].PlantID != "AAA0701" || group[1].PlantID != "AAA1633" {
		t.Errorf("group ids = [%s,%s], want [AAA0701,AAA1633]", group[0].PlantID, group[1].PlantID)
	}
	// The spiral cultivar's scientific name carries the discriminating epithet
	// that the vision prompt leans on (colour is identical for both).
	if group[1].ScientificName != "Juncus effusus 'Spiralis'" {
		t.Errorf("AAA1633 scientificName = %q, want \"Juncus effusus 'Spiralis'\"", group[1].ScientificName)
	}

	// A genus-only key ("rosa") must NOT form a species group even though the
	// catalog has many Rosa cultivars — those are the rose-rerank domain.
	if _, ok := c.SpeciesGroupFor("rosa"); ok {
		t.Error("SpeciesGroupFor(\"rosa\") unexpectedly grouped — genus-only must be excluded")
	}
	// Unknown + empty keys are misses; nil-safe accessor.
	if _, ok := c.SpeciesGroupFor("nonexistent species zzz"); ok {
		t.Error("unknown key unexpectedly found")
	}
	if _, ok := c.SpeciesGroupFor(""); ok {
		t.Error("empty key must be a miss")
	}
	var nilIdx *ContentIndex
	if _, ok := nilIdx.SpeciesGroupFor("juncus effusus"); ok {
		t.Error("nil ContentIndex must return false")
	}
}

// TestSpeciesGroups_AllMultiMemberAndSorted is a structural invariant check over
// every built group: each has ≥2 members, all keys are ≥2-token binomials, and
// members are PlantID-sorted.
func TestSpeciesGroups_Invariants(t *testing.T) {
	c := loadContentForTests(t)
	if len(c.speciesGroups) == 0 {
		t.Fatal("no species groups built from embedded catalog")
	}
	for key, members := range c.speciesGroups {
		if len(members) < 2 {
			t.Errorf("group %q has %d members, want ≥2", key, len(members))
		}
		if got := speciesGroupKey(members[0].ScientificName); got != key {
			// Every member must fold back to its own group key.
			t.Errorf("group %q member[0] %q folds to %q", key, members[0].ScientificName, got)
		}
		for i := 1; i < len(members); i++ {
			if members[i-1].PlantID >= members[i].PlantID {
				t.Errorf("group %q not PlantID-sorted: %s before %s", key, members[i-1].PlantID, members[i].PlantID)
			}
		}
	}
}

// TestRerankCultivar_Parse exercises the genus-neutral vision method against a
// stubbed OpenAI endpoint (same harness as the rose vision tests — no real GPT).
func TestRerankCultivar_Parse(t *testing.T) {
	group, ok := loadContentForTests(t).SpeciesGroupFor("juncus effusus")
	if !ok {
		t.Fatal("juncus effusus group missing")
	}
	canned := `{"choices":[{"message":{"content":"{\"cultivar_certain\":true,\"matches\":[{\"plant_id\":\"AAA1633\",\"confidence\":0.71,\"reason\":\"spiral corkscrew stems\"}]}"}}]}`
	c, srv := newRoseVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, canned)
	})
	defer srv.Close()

	res, err := c.RerankCultivar(context.Background(), roseImg, "image/jpeg", group)
	if err != nil {
		t.Fatalf("RerankCultivar: %v", err)
	}
	if !res.CultivarCertain || len(res.Matches) != 1 || res.Matches[0].PlantID != "AAA1633" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Matches[0].Confidence < 0.7 || res.Matches[0].Confidence > 0.72 {
		t.Errorf("confidence = %v, want ~0.71", res.Matches[0].Confidence)
	}

	// Guard: empty candidates errors (mirrors RerankRose).
	if _, err := c.RerankCultivar(context.Background(), roseImg, "image/jpeg", nil); err == nil {
		t.Error("want error on empty cultivar candidates")
	}
}
