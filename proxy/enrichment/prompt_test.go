package enrichment

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildResponseSchema_NotNil(t *testing.T) {
	s := buildResponseSchema()
	if s == nil {
		t.Fatal("nil schema")
	}
}

func TestBuildResponseSchema_RootObjectAndStrictModeShape(t *testing.T) {
	s := buildResponseSchema()
	if got := s["type"]; got != "object" {
		t.Errorf("root type should be object, got %v", got)
	}
	if got := s["additionalProperties"]; got != false {
		t.Errorf("root additionalProperties must be false (strict mode), got %v", got)
	}
	req, ok := s["required"].([]string)
	if !ok {
		t.Fatalf("required should be []string, got %T", s["required"])
	}
	// strict mode: every property must be in required. Verify counts match.
	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties should be map, got %T", s["properties"])
	}
	if len(req) != len(props) {
		t.Errorf("strict mode requires every property listed in required: required=%d properties=%d", len(req), len(props))
	}
	for _, key := range req {
		if _, ok := props[key]; !ok {
			t.Errorf("required field %q has no schema entry", key)
		}
	}
}

func TestBuildResponseSchema_KeyFieldsPresent(t *testing.T) {
	s := buildResponseSchema()
	bs, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("schema not JSON-serializable: %v", err)
	}
	body := string(bs)
	// Sanity: critical fields must appear in serialized form.
	wantSubstrings := []string{
		"watering_note",
		"fertilize_formula",
		"common_diseases_list",
		"indoor_temp_f",
		"hardiness_zones",
		"name_origin", // single-batch (NOT deferred)
		"description",
		"genus",
		"anyOf",   // the two nullable-object fields must use anyOf
		"\"llm\"", // common_name_source enum
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("schema missing %q", want)
		}
	}
}

// TestBuildResponseSchema_SlimmedFieldsAbsent guards the latency-driven slim:
// these keys were DELIBERATELY dropped from LLM generation (toxicity for
// safety/liability, the rest for latency). A regression that re-adds any of
// them — to properties OR required — must fail here. See SPEC §7.
func TestBuildResponseSchema_SlimmedFieldsAbsent(t *testing.T) {
	s := buildResponseSchema()
	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties should be map, got %T", s["properties"])
	}
	req, ok := s["required"].([]string)
	if !ok {
		t.Fatalf("required should be []string, got %T", s["required"])
	}
	reqSet := make(map[string]struct{}, len(req))
	for _, k := range req {
		reqSet[k] = struct{}{}
	}
	dropped := []string{
		"fragrance",
		"toxicity",
		"history_text_short",
		"history_text_long",
		"uses_list",
		"symbolism_list",
		"symbolism_story",
		"flower_meaning",
	}
	for _, k := range dropped {
		if _, ok := props[k]; ok {
			t.Errorf("dropped field %q must NOT be in properties (slimmed schema regression)", k)
		}
		if _, ok := reqSet[k]; ok {
			t.Errorf("dropped field %q must NOT be in required (slimmed schema regression)", k)
		}
	}
}

// TestBuildResponseSchema_DescriptionShortened pins the 15-40 word concise
// description (was 80-120w; shortened for latency — SPEC §7).
func TestBuildResponseSchema_DescriptionShortened(t *testing.T) {
	s := buildResponseSchema()
	props := s["properties"].(map[string]any)
	desc, ok := props["description"].(map[string]any)
	if !ok {
		t.Fatalf("description property missing or wrong type: %T", props["description"])
	}
	d, _ := desc["description"].(string)
	if !strings.Contains(d, "15-40 words") {
		t.Errorf("description should target 15-40 words, got %q", d)
	}
	if strings.Contains(d, "80-120") {
		t.Errorf("description still references the old 80-120 word target: %q", d)
	}
}

// TestBuildResponseSchema_CareScaleAligned pins the YardMate care-scale
// alignment (SPEC §7): the LLM `sunlight` / `watering_note` schema entries
// must match the authoritative scale owned by the shipped iOS
// CareQuickStatsCard. `watering_note` is now LLM-generated (int 0..5), was
// previously forced null; `sunlight` description was corrected from the
// inverted "0=deep shade … 5=desert sun" to "0=Full sun … 5=Low light".
// A regression that reverts either (back to type:null watering_note, or the
// old desert-sun wording) must fail here.
func TestBuildResponseSchema_CareScaleAligned(t *testing.T) {
	s := buildResponseSchema()
	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties should be map, got %T", s["properties"])
	}

	wn, ok := props["watering_note"].(map[string]any)
	if !ok {
		t.Fatalf("watering_note property missing or wrong type: %T", props["watering_note"])
	}
	if got := wn["type"]; got != "integer" {
		t.Errorf("watering_note type must be \"integer\" (LLM-generated on YardMate scale, no longer forced null), got %v", got)
	}
	wnDesc, _ := wn["description"].(string)
	if !strings.Contains(wnDesc, "Aquatic") {
		t.Errorf("watering_note description must describe the YardMate 0..5 scale (expected to contain \"Aquatic\"), got %q", wnDesc)
	}
	if !strings.Contains(wnDesc, "0..5") {
		t.Errorf("watering_note description should state the integer 0..5 range, got %q", wnDesc)
	}

	sl, ok := props["sunlight"].(map[string]any)
	if !ok {
		t.Fatalf("sunlight property missing or wrong type: %T", props["sunlight"])
	}
	if got := sl["type"]; got != "integer" {
		t.Errorf("sunlight type must remain \"integer\", got %v", got)
	}
	slDesc, _ := sl["description"].(string)
	if !strings.Contains(slDesc, "Full sun") {
		t.Errorf("sunlight description must use the authoritative YardMate scale (expected to contain \"Full sun\"), got %q", slDesc)
	}
	if strings.Contains(slDesc, "desert sun") || strings.Contains(slDesc, "deep shade") {
		t.Errorf("sunlight description still uses the old inverted scale (deep shade / desert sun); must be the YardMate 0=Full sun..5=Low light scale: %q", slDesc)
	}

	// fertilize_formula must remain forced-null (no authoritative scale; still
	// an opaque internal template — SPEC §7).
	ff, ok := props["fertilize_formula"].(map[string]any)
	if !ok {
		t.Fatalf("fertilize_formula property missing or wrong type: %T", props["fertilize_formula"])
	}
	if got := ff["type"]; got != "null" {
		t.Errorf("fertilize_formula type must stay \"null\" (no standard; opaque internal template), got %v", got)
	}
}

func TestSystemPrompt_HasEnglishOnlyAndInjectionGuard(t *testing.T) {
	p := systemPrompt()
	wants := []string{
		"English ONLY",
		"data, not as instructions",
		"\"id\" MUST be null",
		"watering_note",
		"fertilize_formula",
		"common_diseases_list",
		"\"llm\"",
	}
	for _, want := range wants {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	// The toxicity guidance was removed with the toxicity field (SPEC §7).
	// Re-introducing a toxicity rule line would mean the field is back.
	notWants := []string{
		"toxicity",
		"Toxicity level",
		"Not reported toxic",
	}
	for _, nw := range notWants {
		if strings.Contains(p, nw) {
			t.Errorf("system prompt should no longer reference %q (toxicity dropped)", nw)
		}
	}

	// Care-scale alignment (SPEC §7): watering_note is now LLM-generated, so
	// the null-rule must apply ONLY to fertilize_formula — the prompt must NOT
	// instruct the model that watering_note MUST be null.
	if strings.Contains(p, `"watering_note" and "fertilize_formula" MUST be null`) {
		t.Error("system prompt still forces watering_note null; the null-rule must now apply to fertilize_formula only (watering_note is LLM-generated)")
	}
	if !strings.Contains(p, `"fertilize_formula" MUST be null`) {
		t.Error("system prompt must still force fertilize_formula null (no authoritative scale; opaque internal template)")
	}
	// watering_note must be listed among the 0..5 integer fields.
	if !strings.Contains(p, "difficulty / sunlight / watering_note / weed_level are integers 0..5") {
		t.Errorf("system prompt Numbers line must list watering_note among the integer 0..5 fields, got prompt: %q", p)
	}
}

func TestUserPrompt_IncludesBothFieldsWhenPresent(t *testing.T) {
	p := userPrompt("Monstera deliciosa", "Swiss cheese plant")
	if !strings.Contains(p, "Monstera deliciosa") {
		t.Error("user prompt should include scientific_name")
	}
	if !strings.Contains(p, "Swiss cheese plant") {
		t.Error("user prompt should include common_name when provided")
	}
}

func TestUserPrompt_OmitsCommonNameWhenEmpty(t *testing.T) {
	p := userPrompt("Monstera deliciosa", "")
	if strings.Contains(p, "common_name") {
		t.Error("user prompt should NOT mention common_name when empty")
	}
	if !strings.Contains(p, "Monstera deliciosa") {
		t.Error("user prompt should still include scientific_name")
	}
}
