package proxy

// Plant.id 每日上限 + PLANTID_IDENTIFY_FALLBACK 三档的回归测试（SPEC §7「Plant.id
// 每日上限 + 按失败原因兜底」，2026-10-21）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePlantIDFallbackMode(t *testing.T) {
	cases := map[string]PlantIDFallbackMode{
		"":         PlantIDFallbackAlways,
		"true":     PlantIDFallbackAlways,
		"1":        PlantIDFallbackAlways,
		"garbage":  PlantIDFallbackAlways,
		"false":    PlantIDFallbackNever,
		"FALSE":    PlantIDFallbackNever,
		"0":        PlantIDFallbackNever,
		"outage":   PlantIDFallbackOutage,
		" Outage ": PlantIDFallbackOutage,
	}
	for in, want := range cases {
		if got := ParsePlantIDFallbackMode(in); got != want {
			t.Errorf("ParsePlantIDFallbackMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPlantIDFallbackMode_AllowsFallback(t *testing.T) {
	quota := fmt.Errorf("%w: 429", ErrPlantNetRateLimit)
	down := fmt.Errorf("%w: timeout", ErrPlantNetUnavailable)
	for _, c := range []struct {
		mode        PlantIDFallbackMode
		quota, down bool
	}{
		{PlantIDFallbackAlways, true, true},
		{PlantIDFallbackNever, false, false},
		{PlantIDFallbackOutage, false, true},
	} {
		if got := c.mode.allowsFallback(quota); got != c.quota {
			t.Errorf("%v.allowsFallback(429) = %v, want %v", c.mode, got, c.quota)
		}
		if got := c.mode.allowsFallback(down); got != c.down {
			t.Errorf("%v.allowsFallback(outage) = %v, want %v", c.mode, got, c.down)
		}
	}
}

func TestPlantIDBudget_PerIPAndGlobal(t *testing.T) {
	b := NewPlantIDBudget(5, 3)
	for i := 0; i < 3; i++ {
		if ok, _ := b.TryAcquire("1.1.1.1", 1); !ok {
			t.Fatalf("call %d from 1.1.1.1 denied, want allowed (per-IP cap 3)", i+1)
		}
	}
	if ok, why := b.TryAcquire("1.1.1.1", 1); ok || why != "per_ip" {
		t.Fatalf("4th call from same IP = %v/%q, want denied per_ip", ok, why)
	}
	if ok, _ := b.TryAcquire("2.2.2.2", 1); !ok {
		t.Fatal("other IP denied, want allowed (global 4/5)")
	}
	if ok, why := b.TryAcquire("3.3.3.3", 2); ok || why != "global" {
		t.Fatalf("2 credits at 4/5 = %v/%q, want denied global (never exceed the cap)", ok, why)
	}
	if ok, _ := b.TryAcquire("3.3.3.3", 1); !ok {
		t.Fatal("1 credit at 4/5 denied, want allowed (exactly reaches cap)")
	}
	if ok, why := b.TryAcquire("4.4.4.4", 1); ok || why != "global" {
		t.Fatalf("call at 5/5 = %v/%q, want denied global", ok, why)
	}
	if used, capN := b.Snapshot(); used != 5 || capN != 5 {
		t.Fatalf("Snapshot = %d/%d, want 5/5", used, capN)
	}
}

func TestPlantIDBudget_RollsOverAtUTCMidnight(t *testing.T) {
	now := time.Date(2026, 10, 21, 23, 59, 0, 0, time.UTC)
	b := NewPlantIDBudget(1, 1)
	b.now = func() time.Time { return now }
	if ok, _ := b.TryAcquire("1.1.1.1", 1); !ok {
		t.Fatal("first call denied")
	}
	if ok, _ := b.TryAcquire("1.1.1.1", 1); ok {
		t.Fatal("second call same day allowed, want denied")
	}
	now = now.Add(2 * time.Minute) // 次日 00:01 UTC
	if ok, _ := b.TryAcquire("1.1.1.1", 1); !ok {
		t.Fatal("first call of the new UTC day denied, want allowed (both caps reset)")
	}
}

func TestPlantIDBudget_SeedKeepsLarger(t *testing.T) {
	b := NewPlantIDBudget(30, 0)
	b.SeedUsedToday(29)
	if ok, _ := b.TryAcquire("", 1); !ok {
		t.Fatal("29/30 + 1 denied, want allowed")
	}
	b.SeedUsedToday(10) // 远端较小 → 不得冲掉本地已预占
	if used, _ := b.Snapshot(); used != 30 {
		t.Fatalf("used = %d after smaller seed, want 30", used)
	}
	if ok, _ := b.TryAcquire("", 1); ok {
		t.Fatal("31st credit allowed, want denied")
	}
}

func TestPlantIDBudget_NilAndUnlimited(t *testing.T) {
	var nilB *PlantIDBudget
	if ok, _ := nilB.TryAcquire("1.1.1.1", 99); !ok {
		t.Fatal("nil budget denied, want allowed")
	}
	nilB.SeedUsedToday(5) // must not panic
	b := NewPlantIDBudget(0, 0)
	for i := 0; i < 100; i++ {
		if ok, _ := b.TryAcquire("1.1.1.1", 1); !ok {
			t.Fatalf("unlimited budget denied call %d", i+1)
		}
	}
}

func TestPlantIDClient_UsedToday(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"active":true,"used":{"day":2.5,"week":3,"month":3,"total":134},"remaining":{"total":966}}`)
	}))
	defer srv.Close()
	c := &PlantIDClient{APIKey: "k", UsageEndpoint: srv.URL, HTTP: srv.Client()}
	got, err := c.UsedToday(context.Background())
	if err != nil || got != 3 {
		t.Fatalf("UsedToday = %d, %v; want 3 (2.5 rounded up), nil", got, err)
	}
	c.APIKey = "bad"
	if _, err := c.UsedToday(context.Background()); err == nil {
		t.Fatal("UsedToday with bad key: want error")
	}
}

// --- handler-level: identify routing by failure reason + caps ---

const cannedVisionRescueGenus = `{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"Rescuea fakeium\",\"common_names\":[],\"confidence\":0.42}"}}]}`

type outageRig struct {
	h          http.Handler
	plantIDHit *atomic.Int32
	visionHit  *atomic.Int32
	budget     *PlantIDBudget
	close      func()
}

// newOutageRig — prod 目标形态：GPT_ARBITER_ON_DEMAND=true + 给定档位 + Plant.id
// 带每日上限。plantIDUp 为 nil 时用 cannedPlantIDOK。
func newOutageRig(t *testing.T, mode PlantIDFallbackMode, plantNetUp, plantIDUp http.HandlerFunc, budget *PlantIDBudget) *outageRig {
	t.Helper()
	return newOutageRigVision(t, mode, plantNetUp, plantIDUp, budget, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedVisionRescueGenus)
	})
}

// newOutageRigVision — 同 newOutageRig，GPT（vision）上游可自定义。
func newOutageRigVision(t *testing.T, mode PlantIDFallbackMode, plantNetUp, plantIDUp http.HandlerFunc, budget *PlantIDBudget, visionUp http.HandlerFunc) *outageRig {
	t.Helper()
	rig := &outageRig{plantIDHit: &atomic.Int32{}, visionHit: &atomic.Int32{}, budget: budget}
	var pn *PlantNetClient
	pnClose := func() {}
	if plantNetUp != nil { // nil → Plant.id-only 部署（无 Pl@ntNet）
		pnSrv := httptest.NewServer(plantNetUp)
		pn = &PlantNetClient{APIKey: "test-key", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}
		pnClose = pnSrv.Close
	}
	if plantIDUp == nil {
		plantIDUp = func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, cannedPlantIDOK) }
	}
	piSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.plantIDHit.Add(1)
		plantIDUp(w, r)
	}))
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.visionHit.Add(1)
		visionUp(w, r)
	}))
	pi := &PlantIDClient{APIKey: "test-key", Endpoint: piSrv.URL, HTTP: piSrv.Client(), Budget: budget}
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	rig.h = HandleIdentify(pn, pi, content, vision, nil, nil,
		false, false, false, false, false, false,
		true /* arbiterOnDemand */, mode, nil, nil)
	rig.close = func() { pnClose(); piSrv.Close(); vsrv.Close() }
	return rig
}

func plantNetStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

func firstSci(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Suggestions) == 0 {
		t.Fatalf("no suggestions: %s", rec.Body.String())
	}
	return got.Suggestions[0].ScientificName
}

// outage 档：Pl@ntNet 5xx（故障）→ Plant.id 接管（认到种），不走 GPT 兜底。
func TestHandleIdentify_OutageMode_PlantNetDown_UsesPlantID(t *testing.T) {
	rig := newOutageRig(t, PlantIDFallbackOutage, plantNetStatus(http.StatusInternalServerError), nil, NewPlantIDBudget(30, 3))
	defer rig.close()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Monstera deliciosa" {
		t.Fatalf("top = %q, want Plant.id's Monstera deliciosa", sci)
	}
	if rig.plantIDHit.Load() != 1 {
		t.Fatalf("Plant.id calls = %d, want 1", rig.plantIDHit.Load())
	}
	if used, _ := rig.budget.Snapshot(); used != 1 {
		t.Fatalf("budget used = %d, want 1", used)
	}
}

// outage 档：Pl@ntNet 429（当日额度溢出）→ 跳过 Plant.id，GPT 兜底。
func TestHandleIdentify_OutageMode_PlantNetQuota_SkipsPlantID(t *testing.T) {
	rig := newOutageRig(t, PlantIDFallbackOutage, plantNetStatus(http.StatusTooManyRequests), nil, NewPlantIDBudget(30, 3))
	defer rig.close()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Rescuea fakeium" {
		t.Fatalf("top = %q, want GPT rescue Rescuea fakeium", sci)
	}
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0 (429 overflow goes to GPT)", rig.plantIDHit.Load())
	}
}

// 单 IP 每日 3 次：同一 IP 第 4 次 Pl@ntNet 故障 → 不再调 Plant.id，交 GPT。
func TestHandleIdentify_PlantIDPerIPCap(t *testing.T) {
	rig := newOutageRig(t, PlantIDFallbackOutage, plantNetStatus(http.StatusInternalServerError), nil, NewPlantIDBudget(30, 3))
	defer rig.close()
	for i := 0; i < 3; i++ {
		if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Monstera deliciosa" {
			t.Fatalf("call %d top = %q, want Plant.id", i+1, sci)
		}
	}
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Rescuea fakeium" {
		t.Fatalf("4th call top = %q, want GPT rescue (per-IP cap reached)", sci)
	}
	if rig.plantIDHit.Load() != 3 {
		t.Fatalf("Plant.id calls = %d, want 3", rig.plantIDHit.Load())
	}
}

// 全站上限：当日已用满（启动时从 usage_info 起算）→ 一次都不调 Plant.id。
func TestHandleIdentify_PlantIDGlobalCap(t *testing.T) {
	b := NewPlantIDBudget(30, 3)
	b.SeedUsedToday(30)
	rig := newOutageRig(t, PlantIDFallbackAlways, plantNetStatus(http.StatusInternalServerError), nil, b)
	defer rig.close()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Rescuea fakeium" {
		t.Fatalf("top = %q, want GPT rescue (global cap reached)", sci)
	}
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0", rig.plantIDHit.Load())
	}
}

// Pl@ntNet 与 Plant.id 都挂起 → 赛跑截止后 GPT（0s 起并行）兜底出 200（不 502）。
func TestHandleIdentify_PlantIDHang_HopCutGPTRescues(t *testing.T) {
	oldHedge, oldRace, oldMin := identifyPlantNetHedgeDelay, identifyEngineRaceBudget, minPlantIDHopBudget
	identifyPlantNetHedgeDelay, identifyEngineRaceBudget, minPlantIDHopBudget = 100*time.Millisecond, 400*time.Millisecond, 50*time.Millisecond
	defer func() {
		identifyPlantNetHedgeDelay, identifyEngineRaceBudget, minPlantIDHopBudget = oldHedge, oldRace, oldMin
	}()

	release := make(chan struct{})
	hang := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	rig := newOutageRig(t, PlantIDFallbackOutage, hang, hang, NewPlantIDBudget(30, 3))
	defer rig.close()
	defer close(release)

	start := time.Now()
	sci := firstSci(t, doCascadeReq(t, rig.h))
	if sci != "Rescuea fakeium" {
		t.Fatalf("top = %q, want GPT rescue after Plant.id hop cut", sci)
	}
	if rig.plantIDHit.Load() != 1 {
		t.Fatalf("Plant.id calls = %d, want 1", rig.plantIDHit.Load())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want < 5s (both hops must be cut)", elapsed)
	}
}

// diagnose 与 identify 共用额度：用满后 diagnose 不调 Plant.id，走既有 AI vision 兜底。
func TestHandleDiagnose_PlantIDBudgetExhausted_UsesVisionFallback(t *testing.T) {
	var plantIDHit atomic.Int32
	piSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plantIDHit.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer piSrv.Close()
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // vision 也挂 → 静态兜底网，仍 200
	}))
	defer vsrv.Close()
	b := NewPlantIDBudget(30, 3)
	b.SeedUsedToday(29) // diagnose 记 2 → 超 30
	pi := &PlantIDClient{APIKey: "k", Endpoint: piSrv.URL, DiagnoseEndpoint: piSrv.URL, HTTP: piSrv.Client(), Budget: b}
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	h := HandleDiagnose(pi, content, vision, nil, nil)

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/diagnose", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (AI / static fallback), body=%s", rec.Code, rec.Body.String())
	}
	if plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0 (budget exhausted)", plantIDHit.Load())
	}
}

// 赛跑截止算术：引擎赛跑结束后，GPT 兜底仍有 ≥ minIdentifyRescueBudget + 余量。
// 评审复现过的 bug：余量为 0 时剩余恰为 6s − δ，兜底被 `<` 判跳过 → 502。
func TestEngineRaceBudget_LeavesRescueBudget(t *testing.T) {
	left := identifyWallClockBudget - identifyEngineRaceBudget
	if left < minIdentifyRescueBudget+300*time.Millisecond {
		t.Fatalf("race budget %v leaves %v for GPT, want >= %v + margin", identifyEngineRaceBudget, left, minIdentifyRescueBudget)
	}
	if identifyPlantNetHedgeDelay+minPlantIDHopBudget > identifyEngineRaceBudget {
		t.Fatalf("hedge %v + min Plant.id hop %v exceeds race budget %v — Plant.id could never be hedged in", identifyPlantNetHedgeDelay, minPlantIDHopBudget, identifyEngineRaceBudget)
	}
}

// fail-closed：没拿到官方当日用量之前一律拒绝；种子即使是 0 也算起算完成。
func TestPlantIDBudget_RequireSeed(t *testing.T) {
	b := NewPlantIDBudget(30, 3)
	b.RequireSeed()
	if ok, why := b.TryAcquire("1.1.1.1", 1); ok || why != "unseeded" {
		t.Fatalf("before seed = %v/%q, want denied unseeded", ok, why)
	}
	b.SeedUsedToday(0)
	if ok, _ := b.TryAcquire("1.1.1.1", 1); !ok {
		t.Fatal("after seed(0) denied, want allowed")
	}
}

// 客户端已断开：不调 Plant.id、不预占额度。
func TestHandleIdentify_ClientGone_NoPlantIDReservation(t *testing.T) {
	b := NewPlantIDBudget(30, 3)
	rig := newOutageRig(t, PlantIDFallbackOutage, plantNetStatus(http.StatusInternalServerError), nil, b)
	defer rig.close()
	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req.WithContext(canceled))
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0 for a disconnected client", rig.plantIDHit.Load())
	}
	if used, _ := b.Snapshot(); used != 0 {
		t.Fatalf("budget used = %d, want 0 (no reservation for a disconnected client)", used)
	}
}

// Plant.id-only 部署（无 Pl@ntNet）被上限挡下 → 不调 Plant.id，GPT 兜底仍出 200。
func TestHandleIdentify_PlantIDOnly_CapDenied_GPTRescues(t *testing.T) {
	b := NewPlantIDBudget(30, 3)
	b.SeedUsedToday(30)
	rig := newOutageRig(t, PlantIDFallbackAlways, nil, nil, b)
	defer rig.close()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Rescuea fakeium" {
		t.Fatalf("top = %q, want GPT rescue", sci)
	}
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0", rig.plantIDHit.Load())
	}
}
