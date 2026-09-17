package doctor

import (
	"encoding/json"
	"strings"
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

// 字段顺序是流式进度的承重墙（SPEC §5）：observations 必须最先完成、
// spokenSummary 在结构之后。改 prompt/描述时顺带重排字段 = 静默失效。
func TestReplySchemaFieldOrderUnchanged(t *testing.T) {
	want := []string{"photoProblem", "observations", "clarification", "healthLevel", "diagnosis", "possibleCauses", "actionsNow", "expectedRecovery", "followUp", "spokenSummary", "caseTitle"}
	dec := json.NewDecoder(strings.NewReader(replySchema))
	var got []string
	depth := 0
	inProps := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case json.Delim:
			if v == '{' || v == '[' {
				depth++
			} else {
				depth--
				if inProps && depth == 1 {
					inProps = false
				}
			}
		case string:
			if depth == 1 && v == "properties" {
				inProps = true
				continue
			}
			// 顶层 properties 对象内（depth 2）的 key 就是字段名；值都是对象，
			// 所以 depth==2 的字符串 token 只会是 key。
			if inProps && depth == 2 {
				got = append(got, v)
			}
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("schema property order changed:\n got %v\nwant %v", got, want)
	}
}

// 家用疗法规则：必须在 system prompt 与 actionsNow 描述里都在场，
// 且新增文字不能引入裸 % 把 fmt 渲染弄坏。
func TestSystemPromptHouseholdRemedies(t *testing.T) {
	for _, units := range []string{"metric", "imperial"} {
		p := SystemPrompt("en", units)
		if strings.Contains(p, "%!") {
			t.Fatalf("%s: fmt verb error in rendered prompt", units)
		}
		for _, must := range []string{
			"HOUSEHOLD REMEDIES",
			"castile soap",
			"baking soda",
			"sticky traps",
			"cinnamon",
			"never both",
			"70 percent rubbing (isopropyl) alcohol",
			"NEVER suggest bleach, vinegar sprayed or poured on the plant or soil, salt on the soil",
			"Do not force it",
			"never appears when actionsNow must be null",
			"still within the 2 to 4 limit",
		} {
			if !strings.Contains(p, must) {
				t.Errorf("%s: prompt missing %q", units, must)
			}
		}
	}

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(replySchema), &schema); err != nil {
		t.Fatal(err)
	}
	d := schema.Properties["actionsNow"].Description
	for _, must := range []string{"2 to 4", "Null when clarification or photoProblem is set", "household or kitchen item", "Never bleach", "Do not force"} {
		if !strings.Contains(d, must) {
			t.Errorf("actionsNow description missing %q", must)
		}
	}
}
