package doctor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testDevice = "12345678-1234-1234-1234-1234567890AB"

func allowGate(http.ResponseWriter) bool { return true }

func denyGate(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"rate_limit_global"}`))
	return false
}

// buildMultipart assembles a doctor request body.
func buildMultipart(t *testing.T, fields map[string]string, images [][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for i, img := range images {
		fw, err := mw.CreateFormFile("image", fmt.Sprintf("p%d.png", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(img); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func doctorRequest(t *testing.T, body *bytes.Buffer, contentType string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/doctor", body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("X-Device-Install-Id", testDevice)
	r.Header.Set("X-App-Version", "1.7.0")
	return r
}

func testService(t *testing.T, upstream string) *Service {
	t.Helper()
	return NewService("test-key", upstream, "gpt-4o-mini", false)
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body not JSON: %q", rec.Body.String())
	}
	return e.Error
}

func TestHandleValidation(t *testing.T) {
	svc := testService(t, "http://127.0.0.1:1") // never dialed in these cases

	t.Run("missing app version", func(t *testing.T) {
		body, ct := buildMultipart(t, map[string]string{"text": "hi"}, nil)
		r := doctorRequest(t, body, ct)
		r.Header.Del("X-App-Version")
		rec := httptest.NewRecorder()
		Handle(svc, allowGate)(rec, r)
		if rec.Code != http.StatusBadRequest || decodeError(t, rec) != "missing_app_version" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bad device id", func(t *testing.T) {
		body, ct := buildMultipart(t, map[string]string{"text": "hi"}, nil)
		r := doctorRequest(t, body, ct)
		r.Header.Set("X-Device-Install-Id", "not-a-uuid")
		rec := httptest.NewRecorder()
		Handle(svc, allowGate)(rec, r)
		if rec.Code != http.StatusBadRequest || decodeError(t, rec) != "missing_device_id" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("not multipart", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/doctor", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Device-Install-Id", testDevice)
		r.Header.Set("X-App-Version", "1.7.0")
		rec := httptest.NewRecorder()
		Handle(svc, allowGate)(rec, r)
		if rec.Code != http.StatusBadRequest || decodeError(t, rec) != "bad_multipart" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty request", func(t *testing.T) {
		body, ct := buildMultipart(t, map[string]string{"text": "   "}, nil)
		rec := httptest.NewRecorder()
		Handle(svc, allowGate)(rec, doctorRequest(t, body, ct))
		if rec.Code != http.StatusBadRequest || decodeError(t, rec) != "empty_request" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("too many images", func(t *testing.T) {
		body, ct := buildMultipart(t, nil, [][]byte{pngBytes(), pngBytes(), pngBytes(), pngBytes()})
		rec := httptest.NewRecorder()
		Handle(svc, allowGate)(rec, doctorRequest(t, body, ct))
		if rec.Code != http.StatusBadRequest || decodeError(t, rec) != "too_many_images" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-image bytes rejected", func(t *testing.T) {
		body, ct := buildMultipart(t, nil, [][]byte{[]byte("plain text pretending")})
		rec := httptest.NewRecorder()
		Handle(svc, allowGate)(rec, doctorRequest(t, body, ct))
		if rec.Code != http.StatusBadRequest || decodeError(t, rec) != "bad_image" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bad history", func(t *testing.T) {
		body, ct := buildMultipart(t, map[string]string{
			"text":    "hi",
			"history": `[{"user":"x","had_images":true,"reply":{broken`,
		}, nil)
		rec := httptest.NewRecorder()
		Handle(svc, allowGate)(rec, doctorRequest(t, body, ct))
		if rec.Code != http.StatusBadRequest || decodeError(t, rec) != "bad_history" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("spend gate denies with 429 before upstream", func(t *testing.T) {
		body, ct := buildMultipart(t, map[string]string{"text": "hi"}, nil)
		rec := httptest.NewRecorder()
		Handle(svc, denyGate)(rec, doctorRequest(t, body, ct))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

// parseSSE splits a recorded SSE body into (event, data) pairs.
func parseSSE(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	var event string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, [2]string{event, strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

func TestHandleStreamsTypedEvents(t *testing.T) {
	i := strings.Index(wireReply, "drooping")
	j := strings.Index(wireReply, "clarification")
	up := fakeUpstream(t, []string{wireReply[:i], wireReply[i:j], wireReply[j:]}, true)
	defer up.Close()

	body, ct := buildMultipart(t, map[string]string{
		"text": "what is wrong", "language": "en", "units": "imperial",
	}, [][]byte{pngBytes()})
	rec := httptest.NewRecorder()
	Handle(testService(t, up.URL), allowGate)(rec, doctorRequest(t, body, ct))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type=%q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("x-accel-buffering=%q (nginx would buffer the stream)", got)
	}

	events := parseSSE(t, rec.Body.String())
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e[0])
	}
	want := []string{"observation", "observation", "usage", "reply"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", kinds, want)
	}

	var first struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(events[0][1]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Index != 0 || first.Text != "Leaves drooping" {
		t.Fatalf("first observation = %+v", first)
	}

	var reply map[string]json.RawMessage
	if err := json.Unmarshal([]byte(events[3][1]), &reply); err != nil {
		t.Fatalf("reply event not valid JSON: %v", err)
	}
	if _, ok := reply["spokenSummary"]; !ok {
		t.Fatal("reply missing spokenSummary")
	}
}

func TestHandleUpstreamFailureIsSSEError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer up.Close()

	body, ct := buildMultipart(t, map[string]string{"text": "hi"}, nil)
	rec := httptest.NewRecorder()
	Handle(testService(t, up.URL), allowGate)(rec, doctorRequest(t, body, ct))

	// SSE already started → HTTP status stays 200, error arrives as an event.
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	events := parseSSE(t, rec.Body.String())
	if len(events) != 1 || events[0][0] != "error" {
		t.Fatalf("events = %v, want single error", events)
	}
	if !strings.Contains(events[0][1], "bad_upstream") {
		t.Fatalf("error data = %s", events[0][1])
	}
}

func TestModelOverridePolicy(t *testing.T) {
	locked := NewService("k", "", "gpt-4o-mini", false)
	if got := locked.resolveModel("gpt-4o", 0); got != "gpt-4o-mini" {
		t.Fatalf("override without permission resolved to %q", got)
	}
	open := NewService("k", "", "gpt-4o-mini", true)
	if got := open.resolveModel("gpt-4o", 0); got != "gpt-4o" {
		t.Fatalf("permitted override resolved to %q", got)
	}
	if got := open.resolveModel("gpt-999-experimental", 0); got != "gpt-4o-mini" {
		t.Fatalf("non-whitelisted override resolved to %q — whitelist must bound dev spend", got)
	}
	if got := open.resolveModel("", 0); got != "gpt-4o-mini" {
		t.Fatalf("empty override resolved to %q", got)
	}
}

// 分轮模型：前 LeadTurns 轮主力档，之后省钱档；覆盖仍最高优先；
// FollowupModel 未设 = 全程主力档（部署不配即旧行为）。
func TestTurnBasedModelRouting(t *testing.T) {
	svc := NewService("k", "", "gpt-5", false)
	svc.FollowupModel = "gpt-5-mini"
	svc.LeadTurns = 2
	for prior, want := range map[int]string{0: "gpt-5", 1: "gpt-5", 2: "gpt-5-mini", 7: "gpt-5-mini"} {
		if got := svc.resolveModel("", prior); got != want {
			t.Fatalf("prior=%d resolved %q, want %q", prior, got, want)
		}
	}
	svc.AllowOverride = true
	if got := svc.resolveModel("gpt-4o-mini", 0); got != "gpt-4o-mini" {
		t.Fatalf("override should beat routing, got %q", got)
	}
	plain := NewService("k", "", "gpt-5-mini", false)
	if got := plain.resolveModel("", 9); got != "gpt-5-mini" {
		t.Fatalf("unset followup must keep single-model behaviour, got %q", got)
	}
}

func TestNewServiceRejectsUnknownDefaultModel(t *testing.T) {
	svc := NewService("k", "", "gpt-999", false)
	if svc.Model != "gpt-4o-mini" {
		t.Fatalf("unknown DOCTOR_MODEL resolved to %q, want gpt-4o-mini fallback", svc.Model)
	}
}

// fakeBrokenUpstream streams `prefix` as one delta chunk, then closes the
// response without [DONE] or a complete reply — a mid-generation death.
func fakeBrokenUpstream(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunk := map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{"content": prefix}}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}))
}

// 灾备链（用户拍板 2026-08-13）：主档半途死 → FallbackModel 整轮重跑，
// observation 续号（客户端序号闸恰好接续），usage/reply 照常，全程零提示。
func TestHandleFallbackRetriesWithContinuedIndex(t *testing.T) {
	// 主档（grok → XAIClient）：吐完第一条 observation 就断，reply 永不来。
	cut := strings.Index(wireReply, `","Soil`) + 2
	primary := fakeBrokenUpstream(t, wireReply[:cut])
	defer primary.Close()
	fallback := fakeUpstream(t, []string{wireReply}, true)
	defer fallback.Close()

	svc := NewService("k", fallback.URL, "grok-4.6", false)
	svc.XAIClient = NewClient("k", primary.URL)
	svc.FallbackModel = "gpt-5-mini"

	body, ct := buildMultipart(t, map[string]string{"text": "hi"}, nil)
	rec := httptest.NewRecorder()
	Handle(svc, allowGate)(rec, doctorRequest(t, body, ct))

	events := parseSSE(t, rec.Body.String())
	type obs struct {
		Index int    `json:"index"`
		Text  string `json:"text"`
	}
	var seq []obs
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e[0])
		if e[0] == "observation" {
			var o obs
			if err := json.Unmarshal([]byte(e[1]), &o); err != nil {
				t.Fatal(err)
			}
			seq = append(seq, o)
		}
	}
	// 主跑 1 条（index 0）+ 灾备全量 2 条续号（index 1、2），然后 usage+reply。
	want := []string{"observation", "observation", "observation", "usage", "reply"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i, o := range seq {
		if o.Index != i {
			t.Fatalf("observation %d has index %d — fallback must continue numbering, not restart", i, o.Index)
		}
	}
	if seq[1].Text != "Leaves drooping" || seq[2].Text != "Soil pale and dry" {
		t.Fatalf("fallback observations = %+v", seq)
	}
}

func TestHandleFallbackBothFailIsSSEError(t *testing.T) {
	boom := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	bad1 := httptest.NewServer(http.HandlerFunc(boom))
	defer bad1.Close()
	bad2 := httptest.NewServer(http.HandlerFunc(boom))
	defer bad2.Close()

	svc := NewService("k", bad2.URL, "grok-4.6", false)
	svc.XAIClient = NewClient("k", bad1.URL)
	svc.FallbackModel = "gpt-5-mini"

	body, ct := buildMultipart(t, map[string]string{"text": "hi"}, nil)
	rec := httptest.NewRecorder()
	Handle(svc, allowGate)(rec, doctorRequest(t, body, ct))

	events := parseSSE(t, rec.Body.String())
	if len(events) != 1 || events[0][0] != "error" {
		t.Fatalf("events = %v, want single error", events)
	}
}

// fallbackFor 策略：未配置 / 配成当轮自身 / grok 灾备缺 xAI client → 不重跑。
func TestFallbackForPolicy(t *testing.T) {
	svc := NewService("k", "", "grok-4.6", false)
	if got := svc.fallbackFor("grok-4.6"); got != "" {
		t.Fatalf("unset FallbackModel returned %q", got)
	}
	svc.FallbackModel = "gpt-5-mini"
	if got := svc.fallbackFor("gpt-5-mini"); got != "" {
		t.Fatalf("self-fallback returned %q", got)
	}
	if got := svc.fallbackFor("grok-4.6"); got != "gpt-5-mini" {
		t.Fatalf("fallback = %q, want gpt-5-mini", got)
	}
	svc.FallbackModel = "grok-4.6"
	if got := svc.fallbackFor("gpt-5"); got != "" {
		t.Fatalf("grok fallback without XAIClient returned %q", got)
	}
}
