package proxy

// Plant.id 花费闸门（SPEC §7「Plant.id 每日上限 + 按失败原因兜底」）。
//
// Plant.id 按 credit 计费、没有日额度：放任不管时，一次 Pl@ntNet 长时间 outage
// 或一个刷图的客户端就能把预购 credit 烧光。两道上限都按 UTC 自然日计：
//   - 全站上限（PLANTID_DAILY_CAP，默认 30 credit）：identify 每次记 1，diagnose
//     每次按最坏情况记 2（health=auto 不健康时扣第二个 credit）。启动时用 Plant.id
//     官方 usage_info 的当日已用量起算 → 发版重启不会把当日额度归零。
//   - 单 IP 上限（PLANTID_PER_IP_DAILY_CAP，默认 3 次）：按 realIPFromNginx 写回的
//     客户端 IP 计调用次数。只在内存里，重启清零（可接受：全站上限仍兜底）。
// 额度在调用前预占、调用失败也不退回 —— 宁可少用，不可超。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PlantIDFallbackMode 决定 Pl@ntNet 失败时 identify 是否花 Plant.id 的 credit
// （PLANTID_IDENTIFY_FALLBACK）。
type PlantIDFallbackMode int

const (
	// PlantIDFallbackAlways — Pl@ntNet 任何失败都兜底到 Plant.id（"true"，默认）。
	PlantIDFallbackAlways PlantIDFallbackMode = iota
	// PlantIDFallbackNever — Pl@ntNet 失败一律跳过 Plant.id，交给 GPT 兜底（"false"）。
	PlantIDFallbackNever
	// PlantIDFallbackOutage — 只在 Pl@ntNet 故障（超时 / 5xx / 鉴权失效 / 坏响应）时
	// 用 Plant.id；当日额度用完（429）交给 GPT（"outage"）。额度溢出是放量常态，
	// 走 GPT 便宜一个数量级；故障是偶发，Plant.id 认种更准。
	PlantIDFallbackOutage
)

// ParsePlantIDFallbackMode 解析 PLANTID_IDENTIFY_FALLBACK。"outage" 为新档；其余
// 沿用原 bool 语义（strconv.ParseBool 认的写法），空值 / 无法解析 → 默认 Always。
func ParsePlantIDFallbackMode(raw string) PlantIDFallbackMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "outage":
		return PlantIDFallbackOutage
	case "0", "f", "false":
		return PlantIDFallbackNever
	default:
		return PlantIDFallbackAlways
	}
}

func (m PlantIDFallbackMode) String() string {
	switch m {
	case PlantIDFallbackNever:
		return "false"
	case PlantIDFallbackOutage:
		return "outage"
	default:
		return "true"
	}
}

// allowsFallback 报告在 Pl@ntNet 以 pnErr 失败时，本档位是否允许花 Plant.id。
func (m PlantIDFallbackMode) allowsFallback(pnErr error) bool {
	switch m {
	case PlantIDFallbackNever:
		return false
	case PlantIDFallbackOutage:
		return !isPlantNetQuotaErr(pnErr)
	default:
		return true
	}
}

// isPlantNetQuotaErr — Pl@ntNet 429（当日免费额度用完）。
func isPlantNetQuotaErr(err error) bool {
	return errors.Is(err, ErrPlantNetRateLimit)
}

const (
	defaultPlantIDDailyCap      = 30
	defaultPlantIDPerIPDailyCap = 3

	// plantIDDiagnoseCredits — diagnose 一次最多扣 2 个 credit（health=auto）。
	plantIDDiagnoseCredits = 2
	// plantIDIdentifyCredits — identify 一次扣 1 个 credit。
	plantIDIdentifyCredits = 1
)

// PlantIDBudget 是全站 + 单 IP 两道每日上限。nil 安全：nil 预算永远放行。
type PlantIDBudget struct {
	mu        sync.Mutex
	day       string // 当前计数所属的 UTC 日期 yyyy-mm-dd
	used      int    // 当日全站已用 credit（含启动时从 usage_info 起算的部分）
	perIP     map[string]int
	globalCap int // <= 0 → 不限
	perIPCap  int // <= 0 → 不限
	now       func() time.Time
	// requireSeed=true 时，第一次 SeedUsedToday 之前一律拒绝（reason "unseeded"）：
	// 没拿到 Plant.id 官方当日已用量就放行，重启一次就可能多花一整份全站额度，
	// 种子在途时本地预占也会被取较大值吞掉（独立评审）。prod 由 main.go 打开。
	requireSeed bool
	seeded      bool
}

// NewPlantIDBudget 构造两道上限；cap <= 0 表示该道不限。
func NewPlantIDBudget(globalCap, perIPCap int) *PlantIDBudget {
	return &PlantIDBudget{
		perIP:     map[string]int{},
		globalCap: globalCap,
		perIPCap:  perIPCap,
		now:       time.Now,
	}
}

// rollLocked 跨 UTC 日时清零。调用方须持锁。
func (b *PlantIDBudget) rollLocked() {
	today := b.now().UTC().Format("2006-01-02")
	if b.day != today {
		b.day = today
		b.used = 0
		b.perIP = map[string]int{}
	}
}

// RequireSeed 让预算在第一次 SeedUsedToday 之前拒绝一切预占（fail-closed）。
func (b *PlantIDBudget) RequireSeed() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requireSeed = true
}

// TryAcquire 为 ip 预占 credits 个 credit。任一道上限会被突破时不预占、返回
// false + 原因（"unseeded" / "global" / "per_ip"）。ip 为空时只查全站上限。
func (b *PlantIDBudget) TryAcquire(ip string, credits int) (bool, string) {
	if b == nil {
		return true, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.requireSeed && !b.seeded {
		return false, "unseeded"
	}
	b.rollLocked()
	if b.globalCap > 0 && b.used+credits > b.globalCap {
		return false, "global"
	}
	if ip != "" && b.perIPCap > 0 && b.perIP[ip] >= b.perIPCap {
		return false, "per_ip"
	}
	b.used += credits
	if ip != "" {
		b.perIP[ip]++
	}
	return true, ""
}

// SeedUsedToday 把 Plant.id 官方的当日已用量并入计数（取较大者）并标记已起算。
// requireSeed 下种子之前不会有本地预占，取较大者不会吞掉任何预占。
func (b *PlantIDBudget) SeedUsedToday(used int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	b.seeded = true
	if used > b.used {
		b.used = used
	}
}

// Snapshot 返回当日已用 credit 与全站上限（日志 / 测试用）。
func (b *PlantIDBudget) Snapshot() (used, globalCap int) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.used, b.globalCap
}

// defaultPlantIDUsageEndpoint — Plant.id v3 用量查询（不扣 credit）。
const defaultPlantIDUsageEndpoint = "https://plant.id/api/v3/usage_info"

// UsedToday 查 Plant.id 官方的当日已用 credit（usage_info.used.day，向上取整）。
func (c *PlantIDClient) UsedToday(ctx context.Context) (int, error) {
	endpoint := c.UsageEndpoint
	if endpoint == "" {
		endpoint = defaultPlantIDUsageEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("plant_id: build usage request: %w", err)
	}
	req.Header.Set("Api-Key", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("plant_id: usage request: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("plant_id: usage status %d", resp.StatusCode)
	}
	var body struct {
		Used struct {
			Day float64 `json:"day"`
		} `json:"used"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("plant_id: decode usage: %w", err)
	}
	return int(math.Ceil(body.Used.Day)), nil
}

// clientIPFromRequest — 单 IP 上限的计数键。realIPFromNginx 已把 nginx 的
// X-Real-IP 写进 RemoteAddr（无端口）；直连时 RemoteAddr 带端口，剥掉。
func clientIPFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
