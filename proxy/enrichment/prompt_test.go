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
		"anyOf",   // the two nullable-object fields must use anyOf
		"\"llm\"", // common_name_source enum
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("schema missing %q", want)
		}
	}
	if len(bs) < 4000 {
		t.Errorf("schema looks suspiciously small (%d bytes); should be ~5-8KB", len(bs))
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
