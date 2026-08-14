package doctor

import (
	"encoding/json"
	"testing"
)

// 这条测试的存在理由：曾经 required 里加了 caseTitle、properties 里的字段块
// 却没加上（编辑脚本静默失配），strict 模式下 OpenAI 校验 schema 直接 400，
// 所有 Live 调用全挂 —— 而单元测试全绿。schema 的内部一致性必须在提交时炸响。
func TestReplySchemaRequiredMatchesProperties(t *testing.T) {
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(replySchema), &schema); err != nil {
		t.Fatalf("replySchema is not valid JSON: %v", err)
	}
	if len(schema.Required) == 0 || len(schema.Properties) == 0 {
		t.Fatal("schema missing required/properties")
	}
	req := map[string]bool{}
	for _, k := range schema.Required {
		req[k] = true
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("required key %q has no property definition (this 400s every upstream call)", k)
		}
	}
	for k := range schema.Properties {
		if !req[k] {
			t.Errorf("property %q missing from required (strict mode demands every key)", k)
		}
	}
}

// responseFormat 整体也必须是合法 JSON（拼接出错同样是静默 400）。
func TestResponseFormatIsValidJSON(t *testing.T) {
	if !json.Valid([]byte(responseFormat)) {
		t.Fatal("responseFormat is not valid JSON")
	}
}
