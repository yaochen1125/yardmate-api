// Package imageingest fills the out-of-catalog plant hero image key
// (plant_images/{slug}/hero.png on Cloudflare R2) with a license-clean photo
// from Wikimedia Commons. See proxy/imageingest/SPEC.md for the full contract.
//
// This package is an independent domain: it only READS the enrichment-owned
// plants_pending table (seed names) and never imports the enrichment / proxy
// Go packages. It is the only server-side writer of plant imagery to R2.
package imageingest

import "strings"

// Slug computes the R2 species-level slug for a scientific name. Mirrors iOS
// PlantImageURL.slug byte-for-byte (SPEC §2.3, pitfall §9 #1 — THE central
// cross-platform invariant).
//
// The slug is computed from the SPECIES BINOMIAL (first 2 space-delimited
// non-empty words). Every subspecies / variety / cultivar of a species
// collapses to the same slug + one R2 hero — no per-trinomial ingest. iOS
// PlantImageURL.slug applies the equivalent binomial-then-slugify chain;
// both sides MUST stay in lock-step.
//
//	"Monstera adansonii"            → "monstera-adansonii"
//	"Monstera adansonii blanchetii" → "monstera-adansonii"   (subspecies)
//	"Rosa"                          → "rosa"                 (single word)
//
// Slugify rules (applied to the binomial):
//   - lowercase
//   - every non-[a-z0-9] code point is a separator (collapsed to single "-")
//   - no leading / trailing "-"
//   - NO unidecode / NFD / NFC / transliteration. "é" / "×" / spaces all
//     collapse to a separator (NOT mapped to "e", "x", etc.). Diverging
//     from iOS here = permanent 404 on the hero.
func Slug(scientificName string) string {
	return slugifyByteExact(Binomial(scientificName))
}

// Binomial returns the species binomial — the first 2 non-empty space-delimited
// tokens of the input scientific name (any whitespace is normalized away via
// strings.Fields). Names with fewer than 2 words pass through unchanged;
// empty / whitespace-only input yields "".
//
// Exposed so callers also normalize the *Wikimedia search term* (not just
// the slug), keeping the entire pipeline canonical at species level —
// different subspecies seeds map to the same search + same slug + same hero
// (no last-writer-wins churn on the binomial R2 key).
//
//	"Monstera adansonii blanchetii" → "Monstera adansonii"
//	"  Rosa  regina  sueciae  "     → "Rosa regina"
//	"Rosa regina"                   → "Rosa regina"
//	"Monstera"                      → "Monstera"
//	""                              → ""
//
// iOS PlantImageURL applies the equivalent inside slug(for:); both sides
// extract the same first-2-words before slugifying — the cross-platform
// byte-exact invariant.
func Binomial(name string) string {
	fields := strings.Fields(name)
	switch len(fields) {
	case 0:
		return ""
	case 1:
		return fields[0]
	default:
		return fields[0] + " " + fields[1]
	}
}

// slugifyByteExact is the pure byte-exact lowercase + [^a-z0-9]→"-" + collapse
// + trim transform. Input pre-processing (binomial extraction) is the caller's
// job. Kept private: every external caller wants the canonical Slug entry point.
func slugifyByteExact(s string) string {
	out := make([]byte, 0, len(s))
	pendingDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingDash && len(out) > 0 {
				out = append(out, '-')
			}
			out = append(out, byte(r))
			pendingDash = false
		} else if len(out) > 0 {
			pendingDash = true // collapse separators; never flush at end
		}
	}
	return string(out)
}

// GenusSlug mirrors iOS PlantImageURL.genusSlug: split the name on " ",
// take the first non-empty token, and Slug it. If there is no non-empty
// token (empty / whitespace-only input), Slug the whole string (which yields
// "" for those inputs). Implemented for completeness + future genus-level
// fill; NOT invoked by the V1 batch (SPEC §2.3 / §1.2 D-genus).
func GenusSlug(name string) string {
	for _, tok := range strings.Split(name, " ") {
		if tok != "" {
			return Slug(tok)
		}
	}
	return Slug(name)
}
