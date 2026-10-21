package proxy

// Pl@ntNet 请求分段计时（2026-10-21）。10-21 的 16 次超时日志只有一句
// `context deadline exceeded`，分不清是连不上 Pl@ntNet 还是连上了在等它识别。
// 请求失败 / 被取消时打一行 `plantnet trace:`，记录各阶段距请求开始的耗时：
//   conn       拿到连接（reused=true 表示复用已有连接，DNS/TCP/TLS 都省掉）
//   dns/tcp/tls 新建连接时各阶段完成点
//   wrote      图片上传写完
//   firstByte  收到 Pl@ntNet 第一个响应字节（之前的时间 = 它在识别）
// "-" 表示没走到这一步。只在失败时打，正常请求不加日志量。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

type plantNetTrace struct {
	mu     sync.Mutex
	start  time.Time
	reused bool
	marks  map[string]time.Duration
}

func newPlantNetTrace(ctx context.Context) (context.Context, *plantNetTrace) {
	t := &plantNetTrace{start: time.Now(), marks: map[string]time.Duration{}}
	ct := &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) {
			t.mu.Lock()
			t.reused = i.Reused
			t.mu.Unlock()
			t.mark("conn")
		},
		DNSDone:              func(httptrace.DNSDoneInfo) { t.mark("dns") },
		ConnectDone:          func(string, string, error) { t.mark("tcp") },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.mark("tls") },
		WroteRequest:         func(httptrace.WroteRequestInfo) { t.mark("wrote") },
		GotFirstResponseByte: func() { t.mark("firstByte") },
	}
	return httptrace.WithClientTrace(ctx, ct), t
}

func (t *plantNetTrace) mark(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.marks[name]; !ok {
		t.marks[name] = time.Since(t.start)
	}
}

// String — 例：`elapsed=12.0s conn=reused@1ms wrote=0.84s firstByte=-`。
func (t *plantNetTrace) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := func(name string) string {
		if d, ok := t.marks[name]; ok {
			return d.Round(time.Millisecond).String()
		}
		return "-"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "elapsed=%v", time.Since(t.start).Round(time.Millisecond))
	if t.reused {
		fmt.Fprintf(&b, " conn=reused@%s", f("conn"))
	} else {
		fmt.Fprintf(&b, " dns=%s tcp=%s tls=%s conn=%s", f("dns"), f("tcp"), f("tls"), f("conn"))
	}
	fmt.Fprintf(&b, " wrote=%s firstByte=%s", f("wrote"), f("firstByte"))
	return b.String()
}
