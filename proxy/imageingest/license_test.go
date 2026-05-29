package imageingest

import "testing"

func meta(license, short, url, artist, copyrighted string) extMetadata {
	return extMetadata{
		License:          extMetaField{Value: license},
		LicenseShortName: extMetaField{Value: short},
		LicenseURL:       extMetaField{Value: url},
		Artist:           extMetaField{Value: artist},
		Copyrighted:      extMetaField{Value: copyrighted},
	}
}

func TestClassifyLicense(t *testing.T) {
	cases := []struct {
		name        string
		meta        extMetadata
		wantAllowed bool
		wantFamily  LicenseFamily
		wantAttrib  bool
	}{
		{"cc0", meta("cc0", "CC0", "", "", "False"), true, FamilyCC0, false},
		{"public domain code pd", meta("pd", "Public domain", "", "", "False"), true, FamilyPD, false},
		{"public domain via shortName fallback", meta("", "Public domain", "", "", "False"), true, FamilyPD, false},
		{"cc-by-2.0", meta("cc-by-2.0", "CC BY 2.0", "https://creativecommons.org/licenses/by/2.0/", "<a href='x'>Jane</a>", "True"), true, FamilyCCBY, true},
		{"cc-by-4.0", meta("cc-by-4.0", "CC BY 4.0", "", "Jane", "True"), true, FamilyCCBY, true},
		{"cc-by-sa-4.0", meta("cc-by-sa-4.0", "CC BY-SA 4.0", "https://creativecommons.org/licenses/by-sa/4.0/", "Jane Doe", "True"), true, FamilyCCBYSA, true},
		{"cc-by-sa-3.0", meta("cc-by-sa-3.0", "CC BY-SA 3.0", "", "Jane", "True"), true, FamilyCCBYSA, true},

		// Rejections.
		{"cc-by-nc-4.0 rejected", meta("cc-by-nc-4.0", "CC BY-NC 4.0", "", "", "True"), false, FamilyUnknown, false},
		{"cc-by-nd-4.0 rejected", meta("cc-by-nd-4.0", "CC BY-ND 4.0", "", "", "True"), false, FamilyUnknown, false},
		{"cc-by-nc-nd-4.0 rejected", meta("cc-by-nc-nd-4.0", "CC BY-NC-ND 4.0", "", "", "True"), false, FamilyUnknown, false},
		{"nc via shortName fallback rejected", meta("", "CC BY-NC 2.0", "", "", "True"), false, FamilyUnknown, false},
		{"all rights reserved rejected", meta("", "", "", "Some Author", "True"), false, FamilyUnknown, false},
		{"gfdl-only rejected", meta("gfdl", "GFDL", "", "", "True"), false, FamilyUnknown, false},
		{"unknown code rejected", meta("attribution-only-weird", "Weird", "", "", "True"), false, FamilyUnknown, false},
		{"empty everything rejected", meta("", "", "", "", ""), false, FamilyUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyLicense(c.meta)
			if got.Allowed != c.wantAllowed {
				t.Errorf("Allowed = %v, want %v", got.Allowed, c.wantAllowed)
			}
			if got.Family != c.wantFamily {
				t.Errorf("Family = %q, want %q", got.Family, c.wantFamily)
			}
			if got.AttributionRequired != c.wantAttrib {
				t.Errorf("AttributionRequired = %v, want %v", got.AttributionRequired, c.wantAttrib)
			}
		})
	}
}

// TestClassifyLicenseCode_INatUnversioned locks the token-membership behavior
// for iNaturalist's UNVERSIONED codes (SPEC §2.4.3 / Codex #29-B). A literal
// "cc-by-*" glob would reject bare "cc-by"; membership must accept it.
func TestClassifyLicenseCode_INatUnversioned(t *testing.T) {
	cases := []struct {
		name        string
		code        string
		wantAllowed bool
		wantFamily  LicenseFamily
		wantAttrib  bool
	}{
		{"bare cc0", "cc0", true, FamilyCC0, false},
		{"bare cc-by", "cc-by", true, FamilyCCBY, true},
		{"bare cc-by-sa", "cc-by-sa", true, FamilyCCBYSA, true},
		{"cc-by-nc rejected", "cc-by-nc", false, FamilyUnknown, false},
		{"cc-by-nc-nd rejected", "cc-by-nc-nd", false, FamilyUnknown, false},
		{"cc-by-nd rejected", "cc-by-nd", false, FamilyUnknown, false},
		{"uppercase CC-BY normalized", "CC-BY", true, FamilyCCBY, true},
		{"empty (ARR) rejected", "", false, FamilyUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyLicenseCode(c.code, "", "", "(c) Some User, some rights reserved")
			if got.Allowed != c.wantAllowed {
				t.Errorf("Allowed = %v, want %v", got.Allowed, c.wantAllowed)
			}
			if got.Family != c.wantFamily {
				t.Errorf("Family = %q, want %q", got.Family, c.wantFamily)
			}
			if got.AttributionRequired != c.wantAttrib {
				t.Errorf("AttributionRequired = %v, want %v", got.AttributionRequired, c.wantAttrib)
			}
		})
	}
}

// TestClassifyLicenseCode_DerivesShortName verifies a bare iNat code (no short
// name supplied) gets a sensible display ShortName from orDefault.
func TestClassifyLicenseCode_DerivesShortName(t *testing.T) {
	got := ClassifyLicenseCode("cc-by", "", "", "(c) Jane")
	if !got.Allowed || got.ShortName != "CC BY" {
		t.Errorf("ShortName = %q (allowed=%v), want \"CC BY\"", got.ShortName, got.Allowed)
	}
}

func TestClassifyLicense_AuthorHTMLStripped(t *testing.T) {
	got := ClassifyLicense(meta("cc-by-4.0", "CC BY 4.0", "", `<a rel="nofollow" href="//commons">Jane &amp; John</a>`, "True"))
	if !got.Allowed {
		t.Fatalf("expected allowed")
	}
	if got.Author != "Jane & John" {
		t.Errorf("Author = %q, want %q", got.Author, "Jane & John")
	}
}

func TestStripHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<a href="x">Jane Doe</a>`, "Jane Doe"},
		{`Plain Author`, "Plain Author"},
		{`<span>Foo</span> <b>Bar</b>`, "Foo Bar"},
		{`A &amp; B`, "A & B"},
		{`  spaced   out  `, "spaced out"},
	}
	for _, c := range cases {
		if got := stripHTML(c.in); got != c.want {
			t.Errorf("stripHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
