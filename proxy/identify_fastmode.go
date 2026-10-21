package proxy

// 识别极速模式（2026-10-21，SPEC §7「识别极速模式」）。用户拍板：
//   ① 当天只剩 GPT 可用（Pl@ntNet 当日额度用完 + Plant.id 这次也不会被叫）→ 直接用 GPT；
//   ② GPT 给出种级名且自报把握 ≥ 0.95 → 也直接出（比引擎先回就不再等引擎）。
// 两种情况响应带 `fast_mode: true`，App 收到即跳转、不走最短 ~5s 的进度节奏。
// Pl@ntNet 回 429 后记「额度已用完」，之后不再白打它，每 10 分钟放一个请求过去试探恢复。

import (
	"sync"
	"time"
)

// gptFastModeMinConfidence — GPT 自报把握达到此值（且种级名）即走极速模式。
const gptFastModeMinConfidence = 0.95

// plantNetQuotaProbeInterval — Pl@ntNet 额度用完后多久放一个请求过去试探（var 供测试缩短）。
// 不依赖 Pl@ntNet 的每日重置时刻（不确定其时区）。
var plantNetQuotaProbeInterval = 10 * time.Minute

// gptFastGuess — GPT 结果够不够极速模式：种级名 + 把握 ≥ 0.95。
func gptFastGuess(a visionArbiterResult) bool {
	return a.err == nil && a.sug != nil && isFastGPTSuggestion(a.sug)
}

func isFastGPTSuggestion(s *Suggestion) bool {
	return s != nil && s.Confidence >= gptFastModeMinConfidence && hasUsableScientificName(s.ScientificName)
}

// plantNetQuotaGate — Pl@ntNet 当日额度状态。零值可用（未用完）。
type plantNetQuotaGate struct {
	mu        sync.Mutex
	exhausted bool
	lastProbe time.Time
	now       func() time.Time
}

func (g *plantNetQuotaGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// skip 报告这次是否不打 Pl@ntNet（额度已用完、离上次试探不足 probe 间隔）。到了试探点
// 返回 false 并记下本次为试探。
func (g *plantNetQuotaGate) skip() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.exhausted {
		return false
	}
	now := g.clock()
	if now.Sub(g.lastProbe) < plantNetQuotaProbeInterval {
		return true
	}
	g.lastProbe = now
	return false
}

// note 按 Pl@ntNet 这次的结果更新状态：429 → 用完；成功（含 valid no-match）→ 恢复。
// 其它错误（超时 / 5xx / 被取消）不改状态。
func (g *plantNetQuotaGate) note(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case err == nil:
		g.exhausted = false
	case isPlantNetQuotaErr(err):
		if !g.exhausted {
			g.lastProbe = g.clock()
		}
		g.exhausted = true
	}
}

func (g *plantNetQuotaGate) isExhausted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exhausted
}
