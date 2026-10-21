package proxy

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
)

func TestIdentifyDecisionLine(t *testing.T) {
	pid := "AAA0001"
	eng := &Suggestion{ScientificName: "Abelia chinensis", Confidence: 0.61, PlantID: &pid}
	gptSame := &Suggestion{ScientificName: "Abelia chinensis", Confidence: 0.8}
	gptOther := &Suggestion{ScientificName: "Monstera deliciosa", Confidence: 0.9}
	res := func(s *Suggestion) *IdentifyResult { return &IdentifyResult{Suggestions: []Suggestion{*s}} }

	cases := []struct {
		name        string
		engTop, gpt *Suggestion
		gptErr      error
		final       *IdentifyResult
		sentinel    bool
		want        []string
	}{
		{"agree", eng, gptSame, nil, res(eng), false, []string{`agree=true`, `from=both`, `gptState=ok`, `engCat=AAA0001`}},
		{"gpt overrides", eng, gptOther, nil, res(gptOther), false, []string{`agree=false`, `from=gpt`, `final="Monstera deliciosa"`}},
		{"engine kept over disagreeing gpt", eng, gptOther, nil, res(eng), false, []string{`agree=false`, `from=engine`}},
		{"gpt not a plant", eng, nil, ErrVisionNotAPlant, res(eng), false, []string{`gptState=not_plant`, `agree=-`, `gpt="-"`}},
		{"gpt error", eng, nil, errors.New("boom"), res(eng), false, []string{`gptState=err`}},
		{"gpt only (engines down)", nil, gptOther, nil, res(gptOther), false, []string{`engTop="-"`, `from=gpt`}},
		{"sentinel", eng, nil, ErrVisionNotAPlant, unknownSentinelResult(), true, []string{`from=sentinel`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cands []string
			if c.engTop != nil {
				cands = []string{c.engTop.ScientificName}
			}
			got := identifyDecisionLine("dev", "plantnet-catalog", c.engTop, cands, c.gpt, c.gptErr, c.final, c.sentinel, nil)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("line %q missing %q", got, w)
				}
			}
		})
	}
}

// 识别成功的每个请求都打一行 decision，引擎原始第一名与 GPT 都在里面。
func TestHandleIdentify_LogsDecision(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	rig := newOutageRig(t, PlantIDFallbackOutage, plantNetOK, nil, NewPlantIDBudget(30, 3))
	defer rig.close()
	if rec := doCascadeReq(t, rig.h); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "identify decision:") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no `identify decision:` line in log:\n%s", buf.String())
	}
	for _, w := range []string{`engTop="Abelia chinensis"`, `gpt="Rescuea fakeium"`, `agree=false`, `gptState=ok`} {
		if !strings.Contains(line, w) {
			t.Errorf("decision line %q missing %q", line, w)
		}
	}
}

// 评审补的两处归因：库内优先选中引擎第 3 名（GPT 恰好同意）→ both 而非 gpt；
// 属级保留 "Begonia spp." → "Begonia" → gpt 而非 other。
func TestIdentifyDecisionLine_Attribution(t *testing.T) {
	adansonii := &Suggestion{ScientificName: "Monstera adansonii", Confidence: 0.5}
	deliciosa := &Suggestion{ScientificName: "Monstera deliciosa", Confidence: 0.3}
	got := identifyDecisionLine("dev", "plantnet-catalog", adansonii,
		[]string{"Monstera adansonii", "Philodendron hederaceum", "Monstera deliciosa"},
		&Suggestion{ScientificName: "Monstera deliciosa", Confidence: 0.9}, nil,
		&IdentifyResult{Suggestions: []Suggestion{*deliciosa}}, false, nil)
	if !strings.Contains(got, "from=both") {
		t.Errorf("catalog-preferred 3rd engine candidate that GPT agrees with: %q, want from=both", got)
	}
	got = identifyDecisionLine("dev", "ai-raw-oob-genus", nil, nil,
		&Suggestion{ScientificName: "Begonia spp.", Confidence: 0.6}, nil,
		&IdentifyResult{Suggestions: []Suggestion{{ScientificName: "Begonia"}}}, false, nil)
	if !strings.Contains(got, "from=gpt") {
		t.Errorf("genus-keep of GPT's Begonia spp.: %q, want from=gpt", got)
	}
}
