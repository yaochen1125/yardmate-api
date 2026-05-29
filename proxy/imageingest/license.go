package imageingest

import (
	"regexp"
	"strings"
)

// LicenseFamily is the classified, allowed license group for a Commons file.
// Anything outside these four is rejected (SPEC §2.4 conservative default).
type LicenseFamily string

const (
	FamilyCC0     LicenseFamily = "CC0"
	FamilyPD      LicenseFamily = "PD"
	FamilyCCBY    LicenseFamily = "CC_BY"
	FamilyCCBYSA  LicenseFamily = "CC_BY_SA"
	FamilyUnknown LicenseFamily = "" // not allowed
)

// License is the result of classifying a candidate's extmetadata (SPEC §2.4).
// Allowed=false means REJECT (NC / ND / ARR / GFDL-only / unknown). The
// remaining fields are only meaningful when Allowed is true.
type License struct {
	Allowed             bool
	Family              LicenseFamily
	Code                string // machine code, lowercased, e.g. "cc-by-sa-4.0" / "cc0" / "pd"
	ShortName           string // display, e.g. "CC BY-SA 4.0"
	URL                 string // license deed URL (from extmetadata when present)
	Author              string // HTML-stripped Artist; empty for many CC0/PD files
	AttributionRequired bool   // true for CC_BY + CC_BY_SA; false for CC0 / PD
}

var (
	htmlTagRe = regexp.MustCompile(`<[^>]*>`)
	wsRe      = regexp.MustCompile(`\s+`)
)

// ClassifyLicenseCode is the shared token-membership classifier (SPEC §2.4.3),
// usable by BOTH sources: Wikimedia's VERSIONED codes ("cc-by-sa-4.0") and
// iNaturalist's UNVERSIONED codes ("cc-by", "cc0"). It is membership, NOT glob:
// the probe is tokenized on non-alphanumeric runs and we test for the presence
// of tokens ("cc" & "by" & "sa"). A literal "cc-by-*" glob would wrongly reject
// a bare "cc-by" and silently filter out the iNat primary source (Codex #29-B).
//
//	rawCode    machine license code, lowercased internally (primary signal)
//	rawShort   display short name; also the fallback signal when rawCode is empty
//	rawURL     license deed URL (Commons LicenseUrl; empty for iNat)
//	author     already HTML-stripped / plain-text author (caller's responsibility)
//
// The code is tokenized; any "nc" or "nd" token → REJECT. Allowed families:
//
//	cc0                              → CC0      (no attribution)
//	pd / shortName "public domain"   → PD       (no attribution)
//	cc & by & sa  (no nc/nd)         → CC_BY_SA (attribution + ShareAlike)
//	cc & by       (no sa/nc/nd)      → CC_BY    (attribution)
//
// Everything else (ARR / GFDL-only / unknown / empty) → REJECT (conservative —
// a placeholder beats a license violation).
func ClassifyLicenseCode(rawCode, rawShort, rawURL, author string) License {
	code := strings.ToLower(strings.TrimSpace(rawCode))
	short := strings.TrimSpace(rawShort)
	shortLower := strings.ToLower(short)

	// Fall back to the short name as the machine signal when the code is absent.
	probe := code
	if probe == "" {
		probe = shortLower
	}

	author = strings.TrimSpace(author)
	licURL := strings.TrimSpace(rawURL)

	reject := License{
		Allowed:   false,
		Family:    FamilyUnknown,
		Code:      code,
		ShortName: short,
		URL:       licURL,
		Author:    author,
	}

	// Token-based NC / ND rejection (SPEC §2.4): "cc-by-nc-nd-4.0" → has nc/nd;
	// "public domain" → no nc/nd token. Split the probe (which is the machine
	// code, or the short name when the code is absent) on non-alphanumeric runs
	// so "CC BY-NC 4.0" tokenizes the same as "cc-by-nc-4.0".
	tokens := tokenize(probe)
	if hasToken(tokens, "nc") || hasToken(tokens, "nd") {
		return reject
	}

	switch {
	case hasToken(tokens, "cc0"):
		return License{
			Allowed: true, Family: FamilyCC0, Code: orDefault(code, "cc0"),
			ShortName: orDefault(short, "CC0"), URL: licURL, Author: author,
			AttributionRequired: false,
		}

	case hasToken(tokens, "pd") || hasToken(tokens, "publicdomain") ||
		strings.Contains(shortLower, "public domain"):
		return License{
			Allowed: true, Family: FamilyPD, Code: orDefault(code, "pd"),
			ShortName: orDefault(short, "Public domain"), URL: licURL, Author: author,
			AttributionRequired: false,
		}

	case hasToken(tokens, "cc") && hasToken(tokens, "by") && hasToken(tokens, "sa"):
		return License{
			Allowed: true, Family: FamilyCCBYSA, Code: code,
			ShortName: orDefault(short, "CC BY-SA"), URL: licURL, Author: author,
			AttributionRequired: true,
		}

	case hasToken(tokens, "cc") && hasToken(tokens, "by"):
		return License{
			Allowed: true, Family: FamilyCCBY, Code: code,
			ShortName: orDefault(short, "CC BY"), URL: licURL, Author: author,
			AttributionRequired: true,
		}

	default:
		// ARR / GFDL-only / unknown → reject. The Copyrighted=="True" case is
		// subsumed here: nothing positively classified, so we reject regardless.
		return reject
	}
}

// tokenize splits on any run of non-[a-z0-9] characters, lowercased. So
// "cc-by-sa-4.0" → [cc by sa 4 0] and "CC BY-NC 4.0" → [cc by nc 4 0].
func tokenize(s string) []string {
	s = strings.ToLower(s)
	return strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

func hasToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}

// stripHTML removes HTML tags from the extmetadata Artist field (Commons often
// returns an <a> link) and collapses whitespace. Security: never render this;
// we only store the plain-text author for the credits manifest (SPEC §5).
func stripHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#039;", "'")
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
