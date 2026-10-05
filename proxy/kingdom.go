package proxy

import (
	"strings"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Biological kingdom tokens shipped on the wire as `kingdom` (detail record,
// /v1/identify suggestions[], /v1/diagnose top). The iOS mushroom-safety feature
// treats "Fungi" (case-insensitive) as a mushroom and EVERYTHING else — null,
// missing, "Plantae", any other value — as not a mushroom. Only these two
// canonical spellings are ever emitted; anything undeterminable is JSON null.
const (
	KingdomFungi   = "Fungi"
	KingdomPlantae = "Plantae"
)

// NormalizeKingdom maps a raw kingdom string (iNat iconic_taxon_name, an LLM
// self-report, a stored row value) to the canonical wire token, or nil when it
// is neither Fungi nor Plantae ("Other", "Animalia", "", …). Case- and
// whitespace-insensitive so a sloppy source can't smuggle a non-canonical value
// onto the wire.
func NormalizeKingdom(s string) *string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fungi":
		k := KingdomFungi
		return &k
	case "plantae":
		k := KingdomPlantae
		return &k
	}
	return nil
}

// IsFungi reports whether k is the Fungi kingdom (nil-safe, case-insensitive).
func IsFungi(k *string) bool {
	return k != nil && strings.EqualFold(strings.TrimSpace(*k), KingdomFungi)
}

// MergeKingdom combines kingdom verdicts from several sources with a SAFETY
// bias: if ANY source says Fungi the result is Fungi (a missed mushroom is the
// dangerous error; a false mushroom only shows an extra warning). Otherwise the
// first determinable verdict wins, and all-unknown stays nil. Always returns a
// fresh canonical pointer (never aliases an argument).
func MergeKingdom(sources ...*string) *string {
	var first *string
	for _, s := range sources {
		if s == nil {
			continue
		}
		n := NormalizeKingdom(*s)
		if n == nil {
			continue
		}
		if IsFungi(n) {
			return n
		}
		if first == nil {
			first = n
		}
	}
	return first
}

// fungiBannedUseIcons are the uses_list icon keys that assert a food use.
// fungiBannedAttribute is the matching attributes tag.
var fungiBannedUseIcons = map[string]struct{}{"edible": {}, "culinary": {}}

const fungiBannedAttribute = "edible"

// SanitizeFungiDetail is the server-side HARD filter for fungal detail records:
// when d.Kingdom is Fungi it strips every uses_list entry whose icon is
// edible/culinary and the "edible" attribute, in place, and reports whether
// anything was removed. A no-op for non-fungi / nil. Toxic species are routinely
// misidentified as edible ones, and out-of-catalog rows carry no toxicity
// assessment, so a fungal record must never assert a food use — the prompt only
// asks the model not to; this is the guarantee. Replaced slices are freshly
// allocated, so a shallow copy of a shared record can be sanitized safely.
//
// Prose (description / history / …) is NOT inspected here: free text cannot be
// filtered reliably, so it is steered by the prompt only (enrichment/SPEC §7).
func SanitizeFungiDetail(d *PlantDetail) bool {
	if d == nil || !IsFungi(d.Kingdom) {
		return false
	}
	changed := false
	if len(d.UsesList) > 0 {
		kept := make([]UseItem, 0, len(d.UsesList))
		for _, u := range d.UsesList {
			if _, banned := fungiBannedUseIcons[strings.ToLower(strings.TrimSpace(u.Icon))]; banned {
				changed = true
				continue
			}
			kept = append(kept, u)
		}
		if changed {
			d.UsesList = kept
		}
	}
	if len(d.Attributes) > 0 {
		kept := make([]string, 0, len(d.Attributes))
		dropped := false
		for _, a := range d.Attributes {
			if strings.EqualFold(strings.TrimSpace(a), fungiBannedAttribute) {
				dropped = true
				continue
			}
			kept = append(kept, a)
		}
		if dropped {
			d.Attributes = kept
			changed = true
		}
	}
	return changed
}

// kingdomHintCap bounds the learned-kingdom memory (one short string per
// species; 10k entries is a few hundred KB).
const kingdomHintCap = 10_000

// kingdomHints is the in-process memory of kingdoms the enrichment service has
// already resolved (iNat / LLM / stored row), keyed by the species-level
// normalized scientific name. It exists so /v1/identify and /v1/diagnose can
// stamp `kingdom` on an out-of-catalog candidate WITHOUT any network or DB
// round-trip on the latency-budgeted identify path (2026-08-26 timeout
// incident): a pure memory read, nil on a miss.
type kingdomHints struct {
	lru *lru.Cache[string, string]
}

func newKingdomHints() *kingdomHints {
	c, err := lru.New[string, string](kingdomHintCap)
	if err != nil { // only on size <= 0 — unreachable with the constant above
		return nil
	}
	return &kingdomHints{lru: c}
}

// NoteKingdom records a resolved kingdom for scientificName so later identify /
// diagnose responses can reuse it. No-op for a nil index, an undeterminable
// kingdom, or a name that normalizes to "". A Fungi verdict is sticky: a later
// Plantae note for the same species never downgrades it (safety bias, same rule
// as MergeKingdom).
func (c *ContentIndex) NoteKingdom(scientificName string, kingdom *string) {
	if c == nil || c.kingdoms == nil || kingdom == nil {
		return
	}
	k := NormalizeKingdom(*kingdom)
	key := normalizeScientificName(scientificName)
	if k == nil || key == "" {
		return
	}
	if prev, ok := c.kingdoms.lru.Peek(key); ok && prev == KingdomFungi {
		return
	}
	c.kingdoms.lru.Add(key, *k)
}

// KingdomFor returns the kingdom known for scientificName WITHOUT any I/O, or
// nil when undetermined. Sources, merged Fungi-wins: the curated catalog record
// (when the name resolves to a catalog plant AND that record carries a kingdom)
// and the learned hints written by NoteKingdom. nil-safe.
func (c *ContentIndex) KingdomFor(scientificName string) *string {
	if c == nil {
		return nil
	}
	var catalog, hint *string
	if id, ok := c.LookupPlantID(scientificName); ok {
		if d, ok := c.LookupFullDetail(id); ok && d != nil {
			catalog = d.Kingdom
		}
	}
	if c.kingdoms != nil {
		if k, ok := c.kingdoms.lru.Get(normalizeScientificName(scientificName)); ok {
			hint = &k
		}
	}
	return MergeKingdom(catalog, hint)
}
