package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fastModeOf(t *testing.T, rec *httptest.ResponseRecorder) (bool, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	v, present := m["fast_mode"]
	b, _ := v.(bool)
	return b, present
}

func TestPlantNetQuotaGate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	g := plantNetQuotaGate{now: func() time.Time { return now }}
	if skip, _ := g.skip(); skip {
		t.Fatal("fresh gate skipped")
	}
	g.note(fmt.Errorf("%w: 429", ErrPlantNetRateLimit))
	if skip, _ := g.skip(); !skip {
		t.Fatal("after 429 the next request must skip Pl@ntNet")
	}
	now = now.Add(plantNetQuotaProbeInterval)
	if skip, probe := g.skip(); skip || !probe {
		t.Fatal("after the probe interval one request must probe Pl@ntNet")
	}
	if skip, _ := g.skip(); !skip {
		t.Fatal("only one probe per interval")
	}
	g.note(fmt.Errorf("%w: timeout", ErrPlantNetUnavailable))
	if !g.isExhausted() {
		t.Fatal("a non-quota error must not clear the exhausted state")
	}
	// 试探没结论（取消 / 超时 / 5xx）→ 下一个请求立即再试，不白等一个间隔。
	g.probeInconclusive()
	if skip, probe := g.skip(); skip || !probe {
		t.Fatal("an inconclusive probe must let the next request probe immediately")
	}
	g.note(nil)
	if skip, _ := g.skip(); g.isExhausted() || skip {
		t.Fatal("a successful probe must clear the exhausted state")
	}
}

// 极速模式①：Pl@ntNet 回 429（outage 档不叫 Plant.id）→ fast_mode；之后的请求不再打 Pl@ntNet，
// 到试探点放一个过去。
func TestFastMode_OnlyGPT_PlantNetQuotaExhausted(t *testing.T) {
	old := plantNetQuotaProbeInterval
	plantNetQuotaProbeInterval = 2 * time.Second // 3 个请求要在试探间隔内跑完，留足 CI 余量
	defer func() { plantNetQuotaProbeInterval = old }()

	var pnHit atomic.Int32
	rig := newOutageRigVision(t, PlantIDFallbackOutage,
		func(w http.ResponseWriter, r *http.Request) {
			pnHit.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		}, plantIDOK, NewPlantIDBudget(30, 3), gptAnswer("Rescuea fakeium", 0.42))
	defer rig.close()

	for i := 0; i < 3; i++ {
		rec := doCascadeReq(t, rig.h)
		if sci := firstSci(t, rec); sci != "Rescuea fakeium" {
			t.Fatalf("req %d top = %q, want GPT", i+1, sci)
		}
		if fast, _ := fastModeOf(t, rec); !fast {
			t.Fatalf("req %d fast_mode = false, want true (only GPT left)", i+1)
		}
	}
	if n := pnHit.Load(); n != 1 {
		t.Fatalf("Pl@ntNet calls = %d, want 1 (429 once, then skipped)", n)
	}
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0", rig.plantIDHit.Load())
	}
	time.Sleep(plantNetQuotaProbeInterval + 50*time.Millisecond)
	_ = doCascadeReq(t, rig.h)
	if n := pnHit.Load(); n != 2 {
		t.Fatalf("Pl@ntNet calls after probe interval = %d, want 2 (one probe)", n)
	}
}

// 极速模式②：GPT 种级、把握 ≥ 0.95 先回 → 直接出，不等慢 Pl@ntNet。
func TestFastMode_GPTConfidentFirst_DoesNotWaitForPlantNet(t *testing.T) {
	shortRace(t, 2*time.Second, 5*time.Second)
	rig := newOutageRigVision(t, PlantIDFallbackOutage, after(1500*time.Millisecond, plantNetOK), plantIDOK, NewPlantIDBudget(30, 3),
		after(100*time.Millisecond, gptAnswer("Rescuea fakeium", 0.97)))
	defer rig.close()
	start := time.Now()
	rec := doCascadeReq(t, rig.h)
	el := time.Since(start)
	if sci := firstSci(t, rec); sci != "Rescuea fakeium" {
		t.Fatalf("top = %q, want GPT's confident answer", sci)
	}
	if fast, _ := fastModeOf(t, rec); !fast {
		t.Fatal("fast_mode = false, want true")
	}
	if el > time.Second {
		t.Fatalf("elapsed = %v, want ≈ GPT's 100ms (must not wait for Pl@ntNet's 1.5s)", el)
	}
	if rig.plantIDHit.Load() != 0 {
		t.Fatalf("Plant.id calls = %d, want 0", rig.plantIDHit.Load())
	}
}

// GPT 把握 0.95 但只到属 / 把握 0.90：不算极速，照常等 Pl@ntNet。
func TestFastMode_NotForGenusOrBelowThreshold(t *testing.T) {
	for _, g := range []struct {
		sci  string
		conf float64
	}{{"Begonia spp.", 0.99}, {"Rescuea fakeium", 0.90}} {
		t.Run(g.sci, func(t *testing.T) {
			shortRace(t, 2*time.Second, 5*time.Second)
			rig := newOutageRigVision(t, PlantIDFallbackOutage, after(300*time.Millisecond, plantNetOK), plantIDOK, NewPlantIDBudget(30, 3),
				after(50*time.Millisecond, gptAnswer(g.sci, g.conf)))
			defer rig.close()
			start := time.Now()
			rec := doCascadeReq(t, rig.h)
			if el := time.Since(start); el < 250*time.Millisecond {
				t.Fatalf("elapsed = %v: preempted Pl@ntNet without a confident species-level GPT", el)
			}
			if fast, present := fastModeOf(t, rec); fast || present {
				t.Fatalf("fast_mode present=%v value=%v, want absent", present, fast)
			}
		})
	}
}

// 引擎先回、GPT 随后种级且 ≥ 0.95：答案照原有规则合判，fast_mode=true（App 免等）。
func TestFastMode_EngineFirstThenConfidentGPT(t *testing.T) {
	rig := newOutageRigVision(t, PlantIDFallbackOutage, plantNetOK, plantIDOK, NewPlantIDBudget(30, 3),
		after(150*time.Millisecond, gptAnswer("Abelia chinensis", 0.96)))
	defer rig.close()
	rec := doCascadeReq(t, rig.h)
	if sci := firstSci(t, rec); sci != "Abelia chinensis" {
		t.Fatalf("top = %q, want Abelia chinensis", sci)
	}
	if fast, _ := fastModeOf(t, rec); !fast {
		t.Fatal("fast_mode = false, want true")
	}
}

// 常规路径：不带 fast_mode 字段（omitempty，旧 App 不受影响）。
func TestFastMode_AbsentOnNormalPath(t *testing.T) {
	rig := newOutageRig(t, PlantIDFallbackOutage, plantNetOK, nil, NewPlantIDBudget(30, 3))
	defer rig.close()
	rec := doCascadeReq(t, rig.h)
	if _, present := fastModeOf(t, rec); present {
		t.Fatalf("fast_mode present on normal path: %s", rec.Body.String())
	}
}
