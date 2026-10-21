package proxy

// 引擎赛跑回归测试（identify_race.go，SPEC §7「引擎赛跑」）。用户拍板的规则：
//   - Pl@ntNet 到点不掐断，6s 未回叫后备，几路一起跑，谁先出结果用谁；
//   - 后备期间 Pl@ntNet 回来了有结果 → 直接用 Pl@ntNet；
//   - Pl@ntNet 回来的是空结果、后备已在跑 → 等后备；
//   - Plant.id 不可用时后备就是 0s 起并行的 GPT。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortRace 把赛跑的时间参数缩到毫秒级。
func shortRace(t *testing.T, hedge, race time.Duration) {
	t.Helper()
	oldHedge, oldRace, oldMin := identifyPlantNetHedgeDelay, identifyEngineRaceBudget, minPlantIDHopBudget
	identifyPlantNetHedgeDelay, identifyEngineRaceBudget, minPlantIDHopBudget = hedge, race, 10*time.Millisecond
	t.Cleanup(func() {
		identifyPlantNetHedgeDelay, identifyEngineRaceBudget, minPlantIDHopBudget = oldHedge, oldRace, oldMin
	})
}

// after 返回一个先等 d 再按 fn 应答的上游；客户端取消时提前返回。
func after(d time.Duration, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(d):
			fn(w, r)
		case <-r.Context().Done():
		}
	}
}

func plantNetOK(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, cannedPlantNetIdentifyOK) // Abelia chinensis（库内）
}

func plantNetEmpty(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, cannedPlantNetNoMatch)
}

func plantIDOK(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, cannedPlantIDOK) // Monstera deliciosa
}

// Pl@ntNet 慢（过了 hedge 点）、Plant.id 先回 → 用 Plant.id，Pl@ntNet 被取消。
func TestEngineRace_PlantNetSlow_PlantIDWins(t *testing.T) {
	shortRace(t, 100*time.Millisecond, 2*time.Second)
	rig := newOutageRig(t, PlantIDFallbackOutage, after(800*time.Millisecond, plantNetOK), plantIDOK, NewPlantIDBudget(30, 3))
	defer rig.close()
	start := time.Now()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Monstera deliciosa" {
		t.Fatalf("top = %q, want Plant.id's Monstera deliciosa (it answered first)", sci)
	}
	if el := time.Since(start); el > 700*time.Millisecond {
		t.Fatalf("elapsed = %v, want well before slow Pl@ntNet's 800ms", el)
	}
}

// 后备期间 Pl@ntNet 先回来、有结果 → 直接用 Pl@ntNet（Plant.id 已叫起但被取消）。
func TestEngineRace_PlantNetReturnsDuringHedge_PlantNetWins(t *testing.T) {
	shortRace(t, 100*time.Millisecond, 2*time.Second)
	rig := newOutageRig(t, PlantIDFallbackOutage, after(250*time.Millisecond, plantNetOK), after(1500*time.Millisecond, plantIDOK), NewPlantIDBudget(30, 3))
	defer rig.close()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Abelia chinensis" {
		t.Fatalf("top = %q, want Pl@ntNet's Abelia chinensis", sci)
	}
	if rig.plantIDHit.Load() != 1 {
		t.Fatalf("Plant.id calls = %d, want 1 (hedged in, then canceled)", rig.plantIDHit.Load())
	}
}

// Pl@ntNet 回来的是空结果、Plant.id 还在跑 → 不算赢，等 Plant.id。
func TestEngineRace_PlantNetEmptyWhileHedged_WaitsForPlantID(t *testing.T) {
	shortRace(t, 100*time.Millisecond, 2*time.Second)
	rig := newOutageRig(t, PlantIDFallbackOutage, after(200*time.Millisecond, plantNetEmpty), after(400*time.Millisecond, plantIDOK), NewPlantIDBudget(30, 3))
	defer rig.close()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Monstera deliciosa" {
		t.Fatalf("top = %q, want Plant.id's Monstera deliciosa (empty Pl@ntNet must not win)", sci)
	}
}

// Pl@ntNet 在 hedge 点之前就回了空结果 → 照旧用它（下游 GPT 帮着认），不叫 Plant.id。
func TestEngineRace_PlantNetEmptyBeforeHedge_NoPlantID(t *testing.T) {
	shortRace(t, 300*time.Millisecond, 2*time.Second)
	rig := newOutageRig(t, PlantIDFallbackOutage, plantNetEmpty, plantIDOK, NewPlantIDBudget(30, 3))
	defer rig.close()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Rescuea fakeium" {
		t.Fatalf("top = %q, want GPT recovery of the empty Pl@ntNet result", sci)
	}
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0", rig.plantIDHit.Load())
	}
}

// false 档（prod 现状）：Pl@ntNet 慢 → 后备就是已并行算好的 GPT，过 hedge 点立即用它，
// 不叫 Plant.id、不等 Pl@ntNet。
func TestEngineRace_NeverMode_PlantNetSlow_GPTWinsAtHedge(t *testing.T) {
	shortRace(t, 150*time.Millisecond, 3*time.Second)
	rig := newOutageRig(t, PlantIDFallbackNever, after(2*time.Second, plantNetOK), plantIDOK, NewPlantIDBudget(30, 3))
	defer rig.close()
	start := time.Now()
	if sci := firstSci(t, doCascadeReq(t, rig.h)); sci != "Rescuea fakeium" {
		t.Fatalf("top = %q, want GPT (backup in false mode)", sci)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("elapsed = %v, want ≈ hedge point (150ms), not Pl@ntNet's 2s", el)
	}
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0 in false mode", rig.plantIDHit.Load())
	}
}

// 正常路径：GPT 与 Pl@ntNet 同时开跑（不是先后），且只调一次 GPT。
func TestEngineRace_GPTRunsInParallelWithPlantNet(t *testing.T) {
	shortRace(t, 2*time.Second, 5*time.Second)
	var visionHit atomic.Int32
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionHit.Add(1)
		after(300*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, cannedVisionRescueGenus)
		})(w, r)
	}))
	defer vsrv.Close()
	pnSrv := httptest.NewServer(after(300*time.Millisecond, plantNetOK))
	defer pnSrv.Close()
	pn := &PlantNetClient{APIKey: "test-key", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	h := HandleIdentify(pn, nil, content, vision, nil, nil,
		false, false, false, false, false, false,
		true /* arbiterOnDemand, KNN off → GPT parallel */, PlantIDFallbackNever, nil, nil)
	start := time.Now()
	rec := doCascadeReq(t, h)
	el := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if el > 550*time.Millisecond {
		t.Fatalf("elapsed = %v, want ≈300ms (parallel), not ≈600ms (serial)", el)
	}
	if visionHit.Load() != 1 {
		t.Fatalf("GPT calls = %d, want exactly 1", visionHit.Load())
	}
}

// Pl@ntNet 挂起被取消时，trace 能看出卡在哪一步（上传完了、一直没等到第一个字节）。
func TestPlantNetTrace_ShowsWherePlantNetHung(t *testing.T) {
	srv := httptest.NewServer(after(5*time.Second, plantNetOK))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	traceCtx, tr := newPlantNetTrace(ctx)
	req, _ := http.NewRequestWithContext(traceCtx, http.MethodPost, srv.URL, strings.NewReader("img"))
	if _, err := srv.Client().Do(req); err == nil {
		t.Fatal("want timeout error")
	}
	got := tr.String()
	if !strings.Contains(got, "firstByte=-") || strings.Contains(got, "wrote=-") {
		t.Fatalf("trace = %q, want upload done (wrote=<dur>) and no first byte (firstByte=-)", got)
	}
}
