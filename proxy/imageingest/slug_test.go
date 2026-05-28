package imageingest

import "testing"

func TestSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Slug 内部先取 binomial(前 2 词)再 slugify——所有亚种/变种归种级。
		// 设计契约见 Slug + Binomial 注释。
		{"binomial unchanged", "Rosa regina", "rosa-regina"},
		{"trinomial → species binomial", "Rosa regina sueciae", "rosa-regina"},
		{"subspecies (Monstera 实际遇到的)", "Monstera adansonii blanchetii", "monstera-adansonii"},
		{"single word (genus only)", "Monstera", "monstera"},
		{"already lower", "rosa", "rosa"},
		{"leading spaces trimmed", "   Rosa regina", "rosa-regina"},
		{"trailing spaces trimmed", "Rosa regina   ", "rosa-regina"},
		{"multiple inner spaces collapse", "Rosa    regina", "rosa-regina"},
		{"leading+trailing+multi (trinomial→binomial)", "  Rosa   regina  sueciae  ", "rosa-regina"},
		// 杂交符 × 落在 binomial 第 2 词位置：Binomial="Abelia ×"，× 是分隔符且
		// binomial 内后续无 alpha-numeric → slugify 收敛到 "abelia"。V1 接受这个
		// 边界（杂交记法少见，产出仍是有效种级 slug）。
		{"hybrid sign in 2nd word position", "Abelia × grandiflora", "abelia"},
		// Precomposed accent: "é" is a single non-[a-z0-9] code point → acts as a
		// separator (NOT transliterated to "e"). Single-word input passes binomial
		// extraction unchanged → slugify "ArécES" → "ar" + sep + "ces".
		{"precomposed accent acts as separator", "ArécES", "ar-ces"},
		{"punctuation within binomial", "Foo.bar, baz", "foo-bar-baz"},
		{"digits kept; 3rd word dropped", "Crassula 123 ovata", "crassula-123"},
		{"empty string", "", ""},
		{"all separators (no alpha-numeric)", "   ×× -- .. ", ""},
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

func TestBinomial(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"binomial unchanged", "Rosa regina", "Rosa regina"},
		{"trinomial → binomial", "Rosa regina sueciae", "Rosa regina"},
		{"subspecies", "Monstera adansonii blanchetii", "Monstera adansonii"},
		{"4 words → binomial", "Aster × alpinus var. dolomitica", "Aster ×"},
		{"single word passes through", "Rosa", "Rosa"},
		// strings.Fields 把任何空白（含 leading/trailing/multi-space/tab）
		// 都规范化掉——这是 binomial 跨平台一致性的根。
		{"whitespace normalized", "  Rosa   regina   sueciae  ", "Rosa regina"},
		{"empty string", "", ""},
		{"whitespace only", "   \t  ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Binomial(c.in); got != c.want {
				t.Errorf("Binomial(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
