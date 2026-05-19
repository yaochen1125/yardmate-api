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
