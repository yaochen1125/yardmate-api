package proxy

// GPT on-demand 兜底的预算解耦回归测试（SPEC §7「Identify 单跳预算 + GPT on-demand
// 兜底与主级联 ctx 解耦（2026-08-26 事故）」）。三个断言对应事故的三个断裂面：
//   1. Pl@ntNet 挂起被单跳预算切断，GPT 兜底在客户端仍在线时接管出 200；
//   2. 客户端已断开时兜底整个跳过（不烧 GPT 钱），保留 502；
//   3. GPT 兜底自身失败不吞主级联错误，仍 502（错因另行落盘）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newOnDemandRescueHandler — arbiterOnDemand=true + plantIDIdentifyFallback=false
// + plantID=nil：prod 现役配置形态（Pl@ntNet 主引擎，失败只靠 GPT 兜底）。
func newOnDemandRescueHandler(t *testing.T, plantNetUp http.HandlerFunc, vision *VisionClient) (http.Handler, func()) {
	t.Helper()
	pnSrv := httptest.NewServer(plantNetUp)
	pnClient := &PlantNetClient{
		APIKey: "test-key", Endpoint: pnSrv.URL,
		Lang: "en", NbResults: 10, HTTP: pnSrv.Client(),
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	h := HandleIdentify(pnClient, nil, content, vision, nil, nil,
		false, false, false, false, false, false,
		true /* arbiterOnDemand */, false /* plantIDIdentifyFallback */, nil, nil)
	return h, pnSrv.Close
}

const cannedVisionRescueOK = `{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"Rescuea fakeium\",\"common_names\":[],\"confidence\":0.42}"}}]}`

// (1) Pl@ntNet 挂起 → identifyPlantNetTimeout 切断 → GPT 兜底接管 → 200。
// 事故形态：挂起吃满共享 30s ctx，兜底拿到已取消的 ctx 必死 → 502。
func TestHandleIdentify_OnDemandRescue_PlantNetHang_GPTRescues(t *testing.T) {
	oldTimeout := identifyPlantNetTimeout
	identifyPlantNetTimeout = 200 * time.Millisecond
	defer func() { identifyPlantNetTimeout = oldTimeout }()

	// 挂到调用方放弃为止，模拟上游无响应。必须先读完 body：multipart 还有未读字节时
	// Go server 的 background read 被 body 数据打断，检测不到客户端断连 → r.Context()
	// 永不取消 → Server.Close 永久阻塞（本测试初版就挂在这）。release 兜底保证 teardown。
	release := make(chan struct{})
	hang := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}

	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cannedVisionRescueOK)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newOnDemandRescueHandler(t, hang, vision)
	defer cleanup()
	defer close(release) // LIFO：先放行挂着的 handler，再 cleanup 关服务器

	start := time.Now()
	rec := doCascadeReq(t, h)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (GPT rescue), body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Suggestions) != 1 || result.Suggestions[0].Name != "Rescuea fakeium" {
		t.Fatalf("Suggestions = %+v, want 1 AI rescue suggestion 'Rescuea fakeium'", result.Suggestions)
	}
	// 挂起被单跳预算切断的证明：总耗时远小于 identifyUpstreamTimeout（30s）。
	// 5s 上限给 CI 抖动留裕量（正常 <1s：200ms 切断 + 本地假 vision）。
	if elapsed > 5*time.Second {
		t.Fatalf("elapsed = %v, want < 5s (per-hop budget must cut the hang)", elapsed)
	}
}

// (2) 客户端已断开 → 兜底整个跳过（vision 上游零调用），保留 502。
// 烧已断连请求的 GPT 钱没有意义 —— 响应写不回去。
func TestHandleIdentify_OnDemandRescue_ClientGone_SkipsGPT(t *testing.T) {
	visionCalled := false
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionCalled = true
		_, _ = io.WriteString(w, cannedVisionRescueOK)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newOnDemandRescueHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError) // Pl@ntNet down（快速失败路径）
		},
		vision)
	defer cleanup()

	body, ct := buildMultipart(t, "image", jpegMagic)
	req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Device-Install-Id", testUUID)
	req.Header.Set("X-App-Version", "1.1.1")
	canceled, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已断开
	req = req.WithContext(canceled)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (client gone, rescue skipped), body=%s", rec.Code, rec.Body.String())
	}
	if visionCalled {
		t.Fatal("vision upstream was called for a disconnected client — rescue must be skipped")
	}
}

// (3) Pl@ntNet down + GPT 兜底也失败 → err 不被吞，仍 502（default 分支只落盘错因）。
func TestHandleIdentify_OnDemandRescue_VisionAlsoFails_Keeps502(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // vision down
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newOnDemandRescueHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError) // Pl@ntNet down
		},
		vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (rescue failed, upstream error preserved), body=%s", rec.Code, rec.Body.String())
	}
}
