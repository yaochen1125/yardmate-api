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

// sameTaxon — 种级比较（speciesKey）；任一方只到属等折不出种级键时退回整名比较。
func sameTaxon(a, b string) bool {
	ka, kb := speciesKey(a), speciesKey(b)
	if ka != "" && kb != "" {
		return ka == kb
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) && strings.TrimSpace(a) != ""
}

// identifyDecisionLine 生成 `identify decision:` 日志内容（拆出来便于单测）。
func identifyDecisionLine(deviceID, engine string, engTop, gpt *Suggestion, gptErr error, res *IdentifyResult, sentinel bool, content *ContentIndex) string {
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
		fromEng := engTop != nil && sameTaxon(final.ScientificName, engTop.ScientificName)
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
	return fmt.Sprintf("identify decision: deviceID=%s engine=%s engTop=%q engConf=%s engCat=%s gpt=%q gptConf=%s gptCat=%s gptState=%s agree=%s final=%q finalCat=%s from=%s",
		deviceID, engine,
		decisionName(engTop), decisionConf(engTop), decisionCat(engTop, content),
		decisionName(gpt), decisionConf(gpt), decisionCat(gpt, content), gptState,
		agree,
		decisionName(final), decisionCat(final, content), from)
}

func logIdentifyDecision(deviceID, engine string, engTop, gpt *Suggestion, gptErr error, res *IdentifyResult, sentinel bool, content *ContentIndex) {
	log.Print(identifyDecisionLine(deviceID, engine, engTop, gpt, gptErr, res, sentinel, content))
}
