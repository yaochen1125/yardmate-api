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
		// 杂交记法：Binomial filter 掉单字符 hybrid marker (× / 小写 x / 大写 X)
		// 再取前 2，所以杂交学名的 binomial 等于"genus + epithet"，slug 跟非杂交
		// 同种共享一张 hero。catalog 实际用 ASCII `x`（`Abelia x grandiflora`），
		// 修过 Codex #27 后必须对齐。EXACT 单 token 匹配——"Xanthium" / "xanthopinus"
		// 等首字 X/x 的正常学名不受影响。
		{"hybrid Unicode × dropped", "Abelia × grandiflora", "abelia-grandiflora"},
		{"hybrid ASCII x dropped (catalog form)", "Abelia x grandiflora", "abelia-grandiflora"},
		{"hybrid caps X dropped", "Abelia X grandiflora", "abelia-grandiflora"},
		{"hybrid leading × marker dropped", "× Cupressocyparis leylandii", "cupressocyparis-leylandii"},
		{"genus starts with X (not filtered)", "Xanthium strumarium", "xanthium-strumarium"},
		{"epithet starts with x (not filtered)", "Pinus xanthopinus", "pinus-xanthopinus"},
		{"standalone hybrid marker only", "x", ""},
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
		// Hybrid marker (× / x / X) 作为独立 token 时被 filter 掉再取前 2——
		// 杂交学名的 binomial = genus + epithet（跟非杂交同种共享）。
		{"4 words with × marker → binomial drops marker", "Aster × alpinus var. dolomitica", "Aster alpinus"},
		{"hybrid Unicode × filtered", "Abelia × grandiflora", "Abelia grandiflora"},
		{"hybrid ASCII x filtered (catalog form)", "Abelia x grandiflora", "Abelia grandiflora"},
		{"hybrid caps X filtered", "Abelia X grandiflora", "Abelia grandiflora"},
		{"hybrid leading × filtered", "× Cupressocyparis leylandii", "Cupressocyparis leylandii"},
		{"genus starts with X (not filtered)", "Xanthium", "Xanthium"},
		{"standalone hybrid marker only", "x", ""},
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
