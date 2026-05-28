// Package imageingest fills the out-of-catalog plant hero image key
// (plant_images/{slug}/hero.png on Cloudflare R2) with a license-clean photo
// from Wikimedia Commons. See proxy/imageingest/SPEC.md for the full contract.
//
// This package is an independent domain: it only READS the enrichment-owned
// plants_pending table (seed names) and never imports the enrichment / proxy
// Go packages. It is the only server-side writer of plant imagery to R2.
package imageingest

import "strings"

// Slug mirrors iOS PlantImageURL.slug byte-for-byte (shipped iOS PR #227).
// Do NOT add unidecode / NFD / NFC / transliteration — iOS treats every
// non-[a-z0-9] code point as a separator (it is NOT in the allowed set), so
// "é" / "×" / spaces all collapse to a single "-". Transliterating "é"→"e"
// here would produce a DIFFERENT slug than iOS → a permanent 404 on the hero
// (SPEC §2.3, pitfall §9 #1). This is THE central cross-platform invariant:
// same function, same input ⇒ same R2 key on both sides.
func Slug(scientificName string) string {
	out := make([]byte, 0, len(scientificName))
	pendingDash := false
	for _, r := range strings.ToLower(scientificName) {
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
