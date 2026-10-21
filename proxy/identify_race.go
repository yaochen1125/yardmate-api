package proxy

// Identify 引擎赛跑（SPEC §7「引擎赛跑：Pl@ntNet 不掐断、6s 叫后备、谁先出结果用谁」，
// 2026-10-21）。取代原先「Pl@ntNet 单跳 12s 切断 → 串行 Plant.id / GPT」：
//
//   0s   Pl@ntNet 开跑（GPT 由 HandleIdentify 同时并行开跑，见 gptCh）。
//   Pl@ntNet 快速失败（5xx/429/鉴权/坏响应）→ 立即按档位叫后备。
//   6s   Pl@ntNet 仍未回 → 叫后备，Pl@ntNet 不取消、继续跑。
//        后备 = Plant.id（档位允许 + 额度够 + 时间够）；否则后备就是已在跑的 GPT。
//   谁先给出「有候选」的结果用谁，其余取消。Pl@ntNet 回来的是空结果（valid no-match）
//   而 Plant.id 还在跑 → 不算赢，等 Plant.id；Plant.id 也空 / 失败 → 用 Pl@ntNet 的空结果
//   （下游 cascade 照旧让 GPT 帮着认）。
//   Plant.id 失败 → 后备换成 GPT。GPT 已出可用结果 → 立即交给 GPT（不再等 Pl@ntNet）。
//   赛跑截止（GPT 能兜底时 = reqStart + 15.5s，给 GPT 留 6s + 余量；否则 = 共享 22s ctx）
//   仍无结果 → 返回 err，HandleIdentify 的 GPT 兜底接管。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// identifyPlantNetHedgeDelay — Pl@ntNet 多久没回就叫后备（var 供测试缩短）。8-27 以来
// 151 次 Pl@ntNet 成功的整请求（含 GPT 复核）>6s 约 13 次，Pl@ntNet 单跳只会更少 ——
// 正常请求极少触发；触发也只多花一次后备（Plant.id 受每日上限约束）。
var identifyPlantNetHedgeDelay = 6 * time.Second

// identifyEngineRaceBudget — GPT 能兜底时引擎赛跑的截止（reqStart 起算，var 供测试缩短）：
// 22s 墙钟 − GPT 保底 6s − 调度余量 0.5s。截止后剩余 ≥ minIdentifyRescueBudget，
// GPT 兜底（含尚未回的并行 GPT）不会被 `<` 判跳过（独立评审复现过 6s−δ → 502）。
var identifyEngineRaceBudget = identifyWallClockBudget - minIdentifyRescueBudget - 500*time.Millisecond

// minPlantIDHopBudget — 离赛跑截止不足此值就不叫 Plant.id（大概率跑不完、白扣 credit），
// 后备直接是 GPT（var 供测试缩短）。
var minPlantIDHopBudget = 3 * time.Second

type engineOut struct {
	res *IdentifyResult
	err error
}

type engineRace struct {
	ctx          context.Context // 共享级联 ctx（reqStart + 22s）
	clientCtx    context.Context // r.Context()：客户端断开检测
	reqStart     time.Time
	plantNet     *PlantNetClient
	plantID      *PlantIDClient
	mode         PlantIDFallbackMode
	gptCanRescue bool                     // GPT_ARBITER_ON_DEMAND + vision：引擎全挂时 GPT 兜底
	gptCh        chan visionArbiterResult // 并行 GPT（nil = 未并行）；赛跑可能消费它
	clientIP     string
	deviceID     string
	img          []byte
	mime, organ  string
	lat, lon     *float64
}

// raceOutcome — result/engine/err 与旧串行级联同形（engine: "plantnet" /
// "plantid-fallback" / "plantid"）；gpt 非 nil 表示赛跑期间已消费了并行 GPT 的结果
// （调用方必须改用它、不得再读 gptCh）。
type raceOutcome struct {
	result *IdentifyResult
	engine string
	err    error
	gpt    *visionArbiterResult
	// fast — GPT 先回且种级、把握 ≥ 0.95，已交给 GPT（极速模式②，identify_fastmode.go）。
	fast bool
	// onlyGPT — 当天只剩 GPT：Pl@ntNet 当日额度用完（429 / 已知用完被跳过）且 Plant.id
	// 这次没被叫（档位不允许 / 每日上限满 / 无 Plant.id）（极速模式①）。
	onlyGPT bool
}

// gptUsable — 兜底阶段（引擎已全部失败）能否直接用并行 GPT 的结果：有猜测，或明确
// 判定不是植物（→ Unknown sentinel，与旧串行兜底一致）。
func gptUsable(a visionArbiterResult) bool {
	return (a.err == nil && a.sug != nil) || errors.Is(a.err, ErrVisionNotAPlant)
}

// gptPreemptMinConfidence — GPT 在赛跑中抢先（不再等引擎）所需的最低自报置信。与
// plantnetConfidentSkipAIConfidence 同一条「有把握」线（用户 2026-10-21 拍板：把握低就
// 等另外的结果，都没有再用 GPT）。参考：8-27 起 GPT 与 Pl@ntNet 一致的 84 次，GPT 自报
// 0.85–0.99、中位 0.95 —— 有把握的答案基本不受影响。
const gptPreemptMinConfidence = 0.80

// gptCanPreempt — 赛跑中能否让 GPT 抢先：给出了**种级**植物名（只到属的 "Begonia spp."
// 算没把握）且置信 ≥ gptPreemptMinConfidence。不满足 → 继续等 Pl@ntNet / Plant.id 到截止，
// 都没有再由兜底用 GPT 的答案。「不是植物」也不能顶掉还在跑的引擎（原有规则：AI 判非植物
// 不得压过引擎结果；评审复现过慢 Pl@ntNet 的库内结果被 101ms 的 not-a-plant 抢成「没认出来」）。
func gptCanPreempt(a visionArbiterResult) bool {
	return a.err == nil && a.sug != nil &&
		a.sug.Confidence >= gptPreemptMinConfidence &&
		hasUsableScientificName(a.sug.ScientificName)
}

func hasCandidates(r *IdentifyResult) bool { return r != nil && len(r.Suggestions) > 0 }

func (e *engineRace) run() raceOutcome {
	deadline := e.reqStart.Add(identifyUpstreamTimeout)
	if e.gptCanRescue {
		deadline = e.reqStart.Add(identifyEngineRaceBudget)
	}
	raceCtx, raceCancel := context.WithDeadline(e.ctx, deadline)
	defer raceCancel()
	pnCtx, pnCancel := context.WithCancel(raceCtx)
	defer pnCancel()
	pidCtx, pidCancel := context.WithCancel(raceCtx)
	defer pidCancel()

	var (
		out      raceOutcome
		pnCh     chan engineOut
		pidCh    chan engineOut
		pnEmpty  *IdentifyResult // Pl@ntNet 的空结果（等 Plant.id 期间暂存）
		pidEmpty *IdentifyResult
		lastErr  error
		gptRes   *visionArbiterResult
		hedged   bool
		// pidStarted — 一个请求最多叫一次 Plant.id（pidCh 在它返回后会置 nil，不能拿它防重入；
		// 独立评审复现过 hedge 叫一次、Pl@ntNet 随后报错又叫一次 → 扣 2 次额度）。
		pidStarted bool
		pnDown     bool // 本请求的 `identify plantnet down:` 已打（报警按请求计数，只打一行）
		pnQuota    bool // Pl@ntNet 这次是因当日额度用完而没结果（429 或已知用完被跳过）
	)
	onlyGPTNow := func() bool { return !pidStarted && (e.plantNet == nil || pnQuota) }
	out.engine = "plantid"
	if e.plantNet != nil {
		out.engine = "plantnet" // 失败时日志 / 错误映射也知道是哪个引擎
	}
	markPlantNetDown := func(reason string, pnErr error) {
		if pnDown || e.plantNet == nil {
			return
		}
		pnDown = true
		log.Printf("identify plantnet down: deviceID=%s reason=%s after=%v err=%v", e.deviceID, reason, time.Since(e.reqStart).Round(time.Millisecond), pnErr)
	}
	gptCh := e.gptCh
	if !e.gptCanRescue {
		gptCh = nil // always 模式：GPT 只当复核、不当后备（引擎全挂保持 502）
	}

	// startPlantID 返回是否真的叫起了 Plant.id（被断连 / 时间 / 额度挡下返回 false）。
	startPlantID := func(trigger string, pnErr error) bool {
		if e.plantID == nil || pidStarted {
			return false
		}
		if e.clientCtx.Err() != nil {
			log.Printf("identify plantid-skip: deviceID=%s reason=client_gone plantnetErr=%v", e.deviceID, pnErr)
			return false
		}
		if left := time.Until(deadline); left < minPlantIDHopBudget {
			log.Printf("identify plantid-skip: deviceID=%s reason=hop_budget left=%v plantnetErr=%v", e.deviceID, left, pnErr)
			return false
		}
		if ok, limit := e.plantID.Budget.TryAcquire(e.clientIP, plantIDIdentifyCredits); !ok {
			used, capN := e.plantID.Budget.Snapshot()
			log.Printf("identify plantid-budget: deviceID=%s limit=%s used=%d cap=%d plantnetErr=%v → 跳过 Plant.id", e.deviceID, limit, used, capN, pnErr)
			if e.plantNet == nil {
				lastErr = fmt.Errorf("%w: daily budget (%s)", ErrPlantIDRateLimit, limit)
			}
			return false
		}
		if trigger == "plantnet-error" {
			log.Printf("identify plantnet fallback: deviceID=%s err=%v", e.deviceID, pnErr)
		}
		pidStarted = true
		pidCh = make(chan engineOut, 1)
		go func() {
			r, err := e.plantID.Identify(pidCtx, bytes.NewReader(e.img), e.mime, e.lat, e.lon)
			pidCh <- engineOut{r, err}
		}()
		return true
	}

	// plantIDAllowed — 档位是否允许为这次 Pl@ntNet 失败 / 慢请求花 Plant.id。#112 守卫：
	// GPT 兜不了时任何档都保留 Plant.id。慢请求（hedge）按故障对待。
	plantIDAllowed := func(pnErr error) bool {
		if !e.gptCanRescue {
			return true
		}
		if pnErr == nil {
			return e.mode != PlantIDFallbackNever
		}
		return e.mode.allowsFallback(pnErr)
	}

	var hedge <-chan time.Time
	if e.plantNet != nil {
		pnCh = make(chan engineOut, 1)
		skipPN, probe := e.plantNet.quota.skip()
		if skipPN {
			// 当日额度已知用完：不再白打 Pl@ntNet（省一趟 429 往返），每 plantNetQuotaProbeInterval
			// 放一个请求过去试探恢复（identify_fastmode.go）。
			log.Printf("identify plantnet quota-skip: deviceID=%s → 当日额度已用完，不打 Pl@ntNet（每 %v 试探一次）", e.deviceID, plantNetQuotaProbeInterval)
			pnCh <- engineOut{nil, fmt.Errorf("%w: skipped, daily quota exhausted", ErrPlantNetRateLimit)}
		} else {
			go func() {
				r, err := e.plantNet.Identify(pnCtx, bytes.NewReader(e.img), e.mime, e.organ)
				e.plantNet.quota.note(err)
				if probe && err != nil && !isPlantNetQuotaErr(err) {
					e.plantNet.quota.probeInconclusive()
				}
				pnCh <- engineOut{r, err}
			}()
		}
		t := time.NewTimer(time.Until(e.reqStart.Add(identifyPlantNetHedgeDelay)))
		defer t.Stop()
		hedge = t.C
	} else {
		startPlantID("primary", nil)
	}

	finishEngine := func(r *IdentifyResult, engine string) raceOutcome {
		out.result, out.engine, out.err = r, engine, nil
		return out
	}
	plantIDEngine := "plantid"
	if e.plantNet != nil {
		plantIDEngine = "plantid-fallback"
	}

	for {
		pnPending := pnCh != nil
		pidPending := pidCh != nil
		if !pnPending && !pidPending {
			// 两个引擎都结束了且没有「有候选」的赢家。
			switch {
			case pidEmpty != nil:
				return finishEngine(pidEmpty, plantIDEngine)
			case pnEmpty != nil:
				return finishEngine(pnEmpty, "plantnet")
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("%w: no engine available", ErrPlantIDUnavailable)
			}
			out.err = lastErr
			out.onlyGPT = onlyGPTNow()
			return out
		}
		// 已收下的 GPT 答案（hedge 前就回了、当时没把握到 0.95）：过了 hedge 点、Plant.id
		// 不在跑时，按 0.80 那条线交给 GPT（用户 2026-10-21：把握低就继续等引擎）。
		if gptRes != nil && hedged && !pidPending && pnPending && gptCanPreempt(*gptRes) {
			log.Printf("identify engine race: deviceID=%s winner=gpt after=%v (Pl@ntNet slow, no Plant.id)", e.deviceID, time.Since(e.reqStart).Round(time.Millisecond))
			if lastErr == nil {
				lastErr = fmt.Errorf("%w: slow, GPT answered first", ErrPlantNetUnavailable)
			}
			out.err = lastErr
			return out
		}
		// 并行 GPT 从 0s 起就盯着：种级且把握 ≥ 0.95 → 极速模式直接出（不等引擎）；否则先收下，
		// 过了 hedge 点再按 0.80 那条线判。GPT 在 hedge 之前就答完的，结果留在 buffered gptCh。
		var gptWait chan visionArbiterResult
		if gptCh != nil && gptRes == nil {
			gptWait = gptCh
		}

		select {
		case o := <-pnCh:
			pnCh = nil
			switch {
			case o.err == nil && hasCandidates(o.res):
				if pidPending {
					log.Printf("identify engine race: deviceID=%s winner=plantnet after=%v (Plant.id canceled)", e.deviceID, time.Since(e.reqStart).Round(time.Millisecond))
				}
				return finishEngine(o.res, "plantnet")
			case o.err == nil:
				// valid no-match：Plant.id 在跑就等它，否则照旧用空结果。
				if !pidPending {
					return finishEngine(o.res, "plantnet")
				}
				pnEmpty = o.res
			case errors.Is(o.err, ErrPlantNetImageRejected):
				out.err = o.err
				return out
			case errors.Is(o.err, ErrPlantNetUnavailable), errors.Is(o.err, ErrPlantNetRateLimit),
				errors.Is(o.err, ErrPlantNetUnauthorized), errors.Is(o.err, ErrPlantNetBadResponse):
				if lastErr == nil {
					lastErr = o.err
				}
				if isPlantNetQuotaErr(o.err) {
					pnQuota = true // 额度用完不是故障：不打 `plantnet down`（报警只数故障）
				} else {
					markPlantNetDown("error", o.err)
				}
				if pidPending || pidStarted {
					break // 后备已叫过（一个请求只叫一次 Plant.id）
				}
				if plantIDAllowed(o.err) {
					startPlantID("plantnet-error", o.err)
				} else {
					log.Printf("identify plantid-skip: deviceID=%s mode=%s plantnetErr=%v → 跳过 Plant.id, GPT 兜底接管", e.deviceID, e.mode, o.err)
				}
			default:
				lastErr = o.err
			}
		case o := <-pidCh:
			pidCh = nil
			switch {
			case o.err == nil && hasCandidates(o.res):
				if pnPending {
					log.Printf("identify engine race: deviceID=%s winner=plantid after=%v (Pl@ntNet canceled)", e.deviceID, time.Since(e.reqStart).Round(time.Millisecond))
				}
				return finishEngine(o.res, plantIDEngine)
			case o.err == nil:
				pidEmpty = o.res
			default:
				lastErr = o.err
				out.engine = plantIDEngine // 最后失败的是 Plant.id（错误映射 / 日志用）
			}
		case <-hedge:
			hedge = nil
			hedged = true
			if !pnPending || pidPending || pidStarted {
				break
			}
			markPlantNetDown("slow", nil)
			switch {
			case plantIDAllowed(nil) && e.plantID != nil && startPlantID("hedge", nil):
				log.Printf("identify plantnet hedge: deviceID=%s after=%v → 并行叫 Plant.id（Pl@ntNet 继续跑）", e.deviceID, identifyPlantNetHedgeDelay)
			case gptCh != nil:
				log.Printf("identify plantnet hedge: deviceID=%s after=%v mode=%s → 后备是并行 GPT（Pl@ntNet 继续跑）", e.deviceID, identifyPlantNetHedgeDelay, e.mode)
			default:
				log.Printf("identify plantnet hedge: deviceID=%s after=%v mode=%s → 无并行后备，继续等 Pl@ntNet 到截止", e.deviceID, identifyPlantNetHedgeDelay, e.mode)
			}
		case a := <-gptWait:
			gptRes = &a
			out.gpt = gptRes
			gptCh = nil
			switch {
			case gptFastGuess(a):
				// 极速模式②：GPT 种级、把握 ≥ 0.95，比引擎先回 → 直接出，引擎取消。
				log.Printf("identify engine race: deviceID=%s winner=gpt-fast after=%v conf=%.2f (engines canceled)", e.deviceID, time.Since(e.reqStart).Round(time.Millisecond), gptConfOf(a))
				out.fast = true
				if lastErr == nil {
					lastErr = fmt.Errorf("%w: GPT confident first (fast mode)", ErrPlantNetUnavailable)
				}
				out.err = lastErr
				return out
			case hedged && !pidPending && pnPending && gptCanPreempt(a):
				// Pl@ntNet 慢、Plant.id 不在跑，GPT 有把握 → 交给 GPT（不再等 Pl@ntNet）。
				log.Printf("identify engine race: deviceID=%s winner=gpt after=%v (Pl@ntNet slow, no Plant.id)", e.deviceID, time.Since(e.reqStart).Round(time.Millisecond))
				if lastErr == nil {
					lastErr = fmt.Errorf("%w: slow, GPT answered first", ErrPlantNetUnavailable)
				}
				out.err = lastErr
				return out
			case hedged:
				// GPT 没把握（低置信 / 只到属）、失败或判「不是植物」：不抢先，继续等引擎到截止；
				// 都没有结果时兜底照旧用 GPT 的答案（gptUsable）。
				log.Printf("identify engine race: deviceID=%s gpt answered but not confident enough to preempt (sci=%q conf=%.2f err=%v) → 继续等引擎", e.deviceID, gptSciOf(a), gptConfOf(a), a.err)
			}
			// hedge 前回的 GPT（没到 0.95）：先收下，引擎回来照常合判；过了 hedge 点由循环开头按 0.80 判。
		case <-raceCtx.Done():
			if pnPending {
				markPlantNetDown("deadline", raceCtx.Err())
			}
			switch {
			case pidEmpty != nil:
				return finishEngine(pidEmpty, plantIDEngine)
			case pnEmpty != nil:
				return finishEngine(pnEmpty, "plantnet")
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("%w: engine race deadline: %v", ErrPlantNetUnavailable, raceCtx.Err())
			}
			log.Printf("identify engine race: deviceID=%s no engine result by %v (err=%v)", e.deviceID, time.Since(e.reqStart).Round(time.Millisecond), lastErr)
			out.err = lastErr
			out.onlyGPT = onlyGPTNow()
			return out
		}
	}
}

func gptSciOf(a visionArbiterResult) string {
	if a.sug == nil {
		return ""
	}
	return a.sug.ScientificName
}

func gptConfOf(a visionArbiterResult) float64 {
	if a.sug == nil {
		return 0
	}
	return a.sug.Confidence
}
