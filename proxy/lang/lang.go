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

// monthAbbrs maps a supported code to its 12 localized month abbreviations
// (index 0 = January … index 11 = December). Used to render bloom/fruit period
// labels deterministically from the canonical month-integer arrays, so the
// label always agrees with the per-month chart (see enrichment.RenderPeriodShort).
var monthAbbrs = map[string][12]string{
	"en":      {"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	"de":      {"Jan", "Feb", "Mär", "Apr", "Mai", "Jun", "Jul", "Aug", "Sep", "Okt", "Nov", "Dez"},
	"es":      {"Ene", "Feb", "Mar", "Abr", "May", "Jun", "Jul", "Ago", "Sep", "Oct", "Nov", "Dic"},
	"fr":      {"Janv", "Févr", "Mars", "Avr", "Mai", "Juin", "Juil", "Août", "Sept", "Oct", "Nov", "Déc"},
	"it":      {"Gen", "Feb", "Mar", "Apr", "Mag", "Giu", "Lug", "Ago", "Set", "Ott", "Nov", "Dic"},
	"pt":      {"Jan", "Fev", "Mar", "Abr", "Mai", "Jun", "Jul", "Ago", "Set", "Out", "Nov", "Dez"},
	"ja":      {"1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	"ko":      {"1월", "2월", "3월", "4월", "5월", "6월", "7월", "8월", "9월", "10월", "11월", "12월"},
	"vi":      {"Tháng 1", "Tháng 2", "Tháng 3", "Tháng 4", "Tháng 5", "Tháng 6", "Tháng 7", "Tháng 8", "Tháng 9", "Tháng 10", "Tháng 11", "Tháng 12"},
	"zh-Hans": {"1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	"zh-Hant": {"一月", "二月", "三月", "四月", "五月", "六月", "七月", "八月", "九月", "十月", "十一月", "十二月"},
}

// yearRound maps a supported code to its "blooms in every month" label.
var yearRound = map[string]string{
	"en": "Year-round", "de": "Ganzjährig", "es": "Todo el año", "fr": "Toute l'année",
	"it": "Tutto l'anno", "ja": "通年", "ko": "연중", "pt": "O ano todo",
	"vi": "Quanh năm", "zh-Hans": "全年", "zh-Hant": "全年",
}

// MonthAbbr returns the localized abbreviation for month (1..12) in the given
// supported code. Falls back to English for unknown codes or out-of-range months
// (callers should Normalize first); month 0 or >12 returns "".
func MonthAbbr(code string, month int) string {
	if month < 1 || month > 12 {
		return ""
	}
	t, ok := monthAbbrs[code]
	if !ok {
		t = monthAbbrs["en"]
	}
	return t[month-1]
}

// YearRound returns the localized "every month" label for a supported code
// (defaults to English for unknown codes — callers should Normalize first).
func YearRound(code string) string {
	if v, ok := yearRound[code]; ok {
		return v
	}
	return yearRound["en"]
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
