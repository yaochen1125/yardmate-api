package proxy

// 引擎 vs GPT 决策记录（2026-10-21）。此前日志只有最终 engine 标签，统计「GPT 与 Pl@ntNet
// 认得不一样」只能倒推范围（8-27 起真实用户 126 次库内命中里 GPT 改判 ≥43、
// 一致 ≥51、32 次无法判断）。现在每次识别成功打一行 `identify decision:`：
//   engTop/engConf/engCat   引擎（Pl@ntNet 或 Plant.id）原始第一名、置信、库内 id（"-" = 库外 / 无）
//   gpt/gptConf/gptCat      GPT 的答案；gptState = ok / not_plant / err / none（没调）
//   agree                   两边是不是同一个种（任一缺失为 "-"）
//   final/finalCat/from     最终返回的第一名，以及它来自 engine / gpt / both / sentinel / other
// 只记学名与库内 id，不记照片、坐标等个人信息。

import (
	"errors"
	"fmt"
	"log"
	"strings"
)

func decisionName(s *Suggestion) string {
	if s == nil {
		return "-"
	}
	return s.ScientificName
}

func decisionConf(s *Suggestion) string {
	if s == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", s.Confidence)
}

func decisionCat(s *Suggestion, content *ContentIndex) string {
	if s == nil {
		return "-"
	}
	if s.PlantID != nil && strings.TrimSpace(*s.PlantID) != "" {
		return *s.PlantID
	}
	if content != nil {
		if id, ok := content.LookupPlantID(s.ScientificName); ok {
			return id
		}
	}
	return "-"
}

// sameTaxon — 两边都是属级（"Begonia" / "Begonia spp."）按裸属名比；否则种级比较
// （speciesKey）；折不出种级键时退回整名比较。
func sameTaxon(a, b string) bool {
	// 属级先比（speciesKey 会把 "Begonia spp." 折成非空的 "begonia spp."，与 "begonia" 不等）。
	if ga, gb := genusOnlyName(a), genusOnlyName(b); ga != "" && gb != "" {
		return ga == gb
	}
	ka, kb := speciesKey(a), speciesKey(b)
	if ka != "" && kb != "" {
		return ka == kb
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) && strings.TrimSpace(a) != ""
}

// identifyDecisionLine 生成 `identify decision:` 日志内容（拆出来便于单测）。
// engCands — 引擎原始候选的学名（含 engTop）。库内优先可能选中第 2/3 名，只要最终结果
// 在引擎候选里就算来自引擎。
func identifyDecisionLine(deviceID, engine string, engTop *Suggestion, engCands []string, gpt *Suggestion, gptErr error, res *IdentifyResult, sentinel bool, content *ContentIndex) string {
	gptState := "ok"
	switch {
	case gpt != nil:
	case errors.Is(gptErr, ErrVisionNotAPlant):
		gptState = "not_plant"
	case gptErr != nil:
		gptState = "err"
	default:
		gptState = "none"
	}
	agree := "-"
	if engTop != nil && gpt != nil {
		agree = fmt.Sprintf("%v", sameTaxon(engTop.ScientificName, gpt.ScientificName))
	}
	var final *Suggestion
	if res != nil && len(res.Suggestions) > 0 {
		final = &res.Suggestions[0]
	}
	from := "other"
	switch {
	case sentinel:
		from = "sentinel"
	case final == nil:
		from = "none"
	default:
		fromEng := false
		for _, c := range engCands {
			if sameTaxon(final.ScientificName, c) {
				fromEng = true
				break
			}
		}
		fromGPT := gpt != nil && sameTaxon(final.ScientificName, gpt.ScientificName)
		switch {
		case fromEng && fromGPT:
			from = "both"
		case fromEng:
			from = "engine"
		case fromGPT:
			from = "gpt"
		}
	}
	fast := res != nil && res.FastMode
	return fmt.Sprintf("identify decision: deviceID=%s engine=%s engTop=%q engConf=%s engCat=%s gpt=%q gptConf=%s gptCat=%s gptState=%s agree=%s final=%q finalCat=%s from=%s fast=%v",
		deviceID, engine,
		decisionName(engTop), decisionConf(engTop), decisionCat(engTop, content),
		decisionName(gpt), decisionConf(gpt), decisionCat(gpt, content), gptState,
		agree,
		decisionName(final), decisionCat(final, content), from, fast)
}

func logIdentifyDecision(deviceID, engine string, engTop *Suggestion, engCands []string, gpt *Suggestion, gptErr error, res *IdentifyResult, sentinel bool, content *ContentIndex) {
	log.Print(identifyDecisionLine(deviceID, engine, engTop, engCands, gpt, gptErr, res, sentinel, content))
}
