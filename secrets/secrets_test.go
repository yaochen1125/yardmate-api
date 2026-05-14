package secrets

import (
	"strings"
	"testing"
)

func TestParse_HappyPath(t *testing.T) {
	src := `
# Comment line ignored.
OPENAI_API_KEY=sk-abc

PLANT_ID_API_KEY="quoted value"
ATTEST_ALLOW_DEV=true
EMPTY=
WITH_EQUALS=foo=bar=baz
`
	v, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		key, want string
	}{
		{"OPENAI_API_KEY", "sk-abc"},
		{"PLANT_ID_API_KEY", "quoted value"},
		{"ATTEST_ALLOW_DEV", "true"},
		{"EMPTY", ""},
		{"WITH_EQUALS", "foo=bar=baz"},
	}
	for _, c := range cases {
		if got := v.Get(c.key); got != c.want {
			t.Errorf("%s = %q, want %q", c.key, got, c.want)
		}
	}
	if !v.GetBool("ATTEST_ALLOW_DEV", false) {
		t.Error("ATTEST_ALLOW_DEV bool")
	}
	if !v.Has("EMPTY") {
		t.Error("Has(EMPTY) should be true")
	}
	if v.Has("MISSING") {
		t.Error("Has(MISSING) should be false")
	}
}

func TestParse_QuoteVariants(t *testing.T) {
	v, _ := Parse(strings.NewReader(`A="double"
B='single'
C=plain
D='with"inner'
E="ends_unbalanced'
`))
	if v.Get("A") != "double" {
		t.Errorf("A = %q", v.Get("A"))
	}
	if v.Get("B") != "single" {
		t.Errorf("B = %q", v.Get("B"))
	}
	if v.Get("C") != "plain" {
		t.Errorf("C = %q", v.Get("C"))
	}
	if v.Get("D") != `with"inner` {
		t.Errorf("D = %q", v.Get("D"))
	}
	// Unbalanced quotes are kept literally.
	if v.Get("E") != `"ends_unbalanced'` {
		t.Errorf("E = %q", v.Get("E"))
	}
}

func TestParse_MalformedLine(t *testing.T) {
	if _, err := Parse(strings.NewReader("not_a_key_value_line")); err == nil {
		t.Fatal("expected error on missing =")
	}
	if _, err := Parse(strings.NewReader("=val_without_key")); err == nil {
		t.Fatal("expected error on empty key")
	}
}

func TestGetBool_FallsBackToDefault(t *testing.T) {
	v, _ := Parse(strings.NewReader("FOO=notabool\nBAR=false"))
	if v.GetBool("FOO", true) != true {
		t.Error("unparseable → default")
	}
	if v.GetBool("MISSING", true) != true {
		t.Error("missing → default")
	}
	if v.GetBool("BAR", true) != false {
		t.Error("false should parse")
	}
}

func TestSnapshot_LowercaseAndStableShape(t *testing.T) {
	v, _ := Parse(strings.NewReader("OPENAI_API_KEY=k1\nPLANT_ID_API_KEY=k2"))
	snap := v.Snapshot([]string{"OPENAI_API_KEY", "PLANT_ID_API_KEY", "FUTURE_KEY"})
	if snap["openai_api_key"] != "k1" {
		t.Errorf("openai = %q", snap["openai_api_key"])
	}
	if snap["plant_id_api_key"] != "k2" {
		t.Errorf("plantid = %q", snap["plant_id_api_key"])
	}
	val, ok := snap["future_key"]
	if !ok || val != "" {
		t.Errorf("missing key should be present as empty string; got ok=%v val=%q", ok, val)
	}
	if len(snap) != 3 {
		t.Errorf("len = %d, want 3", len(snap))
	}
}
