// Package lang is the single source of truth for YardMate's supported display
// languages: the canonical code set, BCP-47 normalization, and the English
// display name used in LLM prompts.
//
// It is a stdlib-only leaf package so BOTH proxy (e.g. VisionClient.DiagnosePlant)
// and proxy/enrichment can import it without an import cycle (enrichment imports
// proxy, so proxy must not import enrichment — the language table can't live in
// enrichment if proxy also needs it). enrichment keeps its existing
// NormalizeLang / langDisplayName names by delegating here, so there is exactly
// ONE table and ONE normalization rule across the codebase.
package lang

import "strings"

// names maps a supported code to its English display name for LLM prompts.
var names = map[string]string{
	"en": "English", "de": "German", "es": "Spanish", "fr": "French",
	"it": "Italian", "ja": "Japanese", "ko": "Korean", "pt": "Portuguese",
	"vi": "Vietnamese", "zh-Hans": "Simplified Chinese", "zh-Hant": "Traditional Chinese",
}

// DisplayName returns the English name of a supported code (defaults to English
// for unknown codes — callers should Normalize first).
func DisplayName(code string) string {
	if n, ok := names[code]; ok {
		return n
	}
	return "English"
}

// Normalize maps an incoming BCP-47 tag to the nearest supported code (SPEC §1.3
// + §9 #21). Region subtags drop for single-variant languages, but Chinese
// script is preserved (zh-Hans ≠ zh-Hant). Unsupported / empty → "en".
func Normalize(tag string) string {
	t := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(tag), "_", "-"))
	if t == "" {
		return "en"
	}
	// Chinese: preserve simplified vs traditional (collapsing to "zh" loses data).
	if t == "zh" || strings.HasPrefix(t, "zh-") {
		if strings.Contains(t, "hant") || strings.Contains(t, "-tw") ||
			strings.Contains(t, "-hk") || strings.Contains(t, "-mo") {
			return "zh-Hant"
		}
		return "zh-Hans" // zh, zh-hans, zh-cn, zh-sg, …
	}
	base := t
	if i := strings.IndexByte(t, '-'); i > 0 {
		base = t[:i]
	}
	switch base {
	case "en", "de", "es", "fr", "it", "ja", "ko", "pt", "vi":
		return base
	}
	return "en"
}
