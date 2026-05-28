package imageingest

import "testing"

func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"canonical binomial", "Rosa regina sueciae", "rosa-regina-sueciae"},
		{"already lower", "rosa", "rosa"},
		{"leading spaces trimmed", "   Rosa regina", "rosa-regina"},
		{"trailing spaces trimmed", "Rosa regina   ", "rosa-regina"},
		{"multiple spaces collapse", "Rosa    regina", "rosa-regina"},
		{"leading+trailing+multi", "  Rosa   regina  sueciae  ", "rosa-regina-sueciae"},
		{"hybrid multiplication sign dropped", "Abelia × grandiflora", "abelia-grandiflora"},
		// Precomposed accent: "é" is a single non-[a-z0-9] code point → acts as a
		// separator (NOT transliterated to "e"). "ArécES" → "ar" + sep + "ces".
		{"precomposed accent acts as separator", "ArécES", "ar-ces"},
		{"punctuation collapses", "Foo.bar, baz", "foo-bar-baz"},
		{"digits kept", "Crassula 123 ovata", "crassula-123-ovata"},
		{"empty string", "", ""},
		{"all separators", "   ×× -- .. ", ""},
		{"single separator only", "-", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Slug(c.in); got != c.want {
				t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestGenusSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"binomial → genus", "Rosa regina sueciae", "rosa"},
		{"leading spaces then genus", "   Rosa regina", "rosa"},
		{"single token", "Rosa", "rosa"},
		// First space-token is the non-empty "×"; Slug("×") == "" (it is a
		// separator). iOS genusSlug only skips EMPTY leading tokens (from
		// repeated/leading spaces), not all-separator tokens, so this slugs "×".
		{"hybrid sign token slugs to empty", "× Rosa regina", ""},
		{"empty string", "", ""},
		{"all separators", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GenusSlug(c.in); got != c.want {
				t.Errorf("GenusSlug(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
