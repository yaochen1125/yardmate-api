// Package inflight bounds the number of concurrently-processing expensive
// proxy requests (/v1/identify + /v1/diagnose) to keep the process from being
// OOM-killed under a burst.
//
// Each such request reads the uploaded image (≤8 MB) into memory, and systemd
// caps the service cgroup (MemoryMax). With no concurrency bound, a burst of
// simultaneous uploads can exceed the cap and the whole service is OOM-killed
// — dropping EVERY in-flight request, not just the excess. This limiter caps
// how many requests are processing at once (peak memory ≈ cap × per-request
// image), so instead of crashing, only the overflow is shed cleanly.
//
// Wait-then-reject (not reject-fast), for backward compatibility:
// the already-shipped iOS client maps 503 → .serviceUnavailable and times out
// at 30 s (identify) / 20 s (diagnose). Overflow requests therefore WAIT for a
// slot up to a short budget (< client timeout) instead of failing immediately.
// Crucially the wait happens in middleware, BEFORE the handler reads the body,
// so a waiting request holds only a goroutine + connection (~KB) — NOT the
// ~8 MB image. Under realistic bursts a slot frees within a second or two and
// the client just sees a slightly slower identify (a normal 200); only
// sustained extreme overload yields a 503, which old clients already render as
// a normal "try again" state. See SPEC.md.
package inflight

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Limiter caps concurrent in-flight requests to a fixed number of slots, with
// a bounded wait queue in front. It is safe for concurrent use.
type Limiter struct {
	slots      chan struct{} // buffered; capacity == concurrency cap
	maxWait    int64         // max simultaneous queued waiters (0 = reject immediately when full)
	waitBudget time.Duration // how long an overflow request waits for a slot
	waiters    int64         // atomic: current queued waiters
}

// New builds a Limiter.
//
//   - maxInflight: concurrency cap. Peak memory ≈ maxInflight × per-request
//     image size, so this is the real OOM guard (clamped to ≥1).
//   - maxWait: max requests allowed to queue for a slot at once. Bounds
//     goroutine/connection growth under sustained overload (clamped to ≥0).
//   - waitBudget: how long an overflow request waits before 503. Keep it well
//     under the client's request timeout so the wait usually resolves into a
//     served 200 (clamped to ≥0).
func New(maxInflight, maxWait int, waitBudget time.Duration) *Limiter {
	if maxInflight < 1 {
		maxInflight = 1
	}
	if maxWait < 0 {
		maxWait = 0
	}
	if waitBudget < 0 {
		waitBudget = 0
	}
	return &Limiter{
		slots:      make(chan struct{}, maxInflight),
		maxWait:    int64(maxWait),
		waitBudget: waitBudget,
	}
}

// acquire takes a slot, returning a release func + true on success. It returns
// (nil, false) when the wait queue is already full, or when the wait budget or
// the request context elapses before a slot frees.
func (l *Limiter) acquire(ctx context.Context) (release func(), ok bool) {
	// Fast path: a slot is free right now — no queueing.
	select {
	case l.slots <- struct{}{}:
		return l.release, true
	default:
	}

	// Slots full. Join the bounded wait queue (reject if it's already at cap so
	// waiters can't pile up without limit).
	if atomic.AddInt64(&l.waiters, 1) > l.maxWait {
		atomic.AddInt64(&l.waiters, -1)
		return nil, false
	}
	defer atomic.AddInt64(&l.waiters, -1)

	timer := time.NewTimer(l.waitBudget)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		return l.release, true
	case <-timer.C:
		return nil, false // waited too long → shed
	case <-ctx.Done():
		return nil, false // client hung up → shed
	}
}

// release frees one slot. Each successful acquire must call it exactly once.
func (l *Limiter) release() { <-l.slots }

// Waiters reports the current number of queued waiters (diagnostic / tests).
func (l *Limiter) Waiters() int { return int(atomic.LoadInt64(&l.waiters)) }

// Middleware bounds concurrency for the wrapped handler. On overflow it writes
// 503 + Retry-After + {"error":"<errCode>"} — the same JSON envelope shape the
// rate limiter uses for 429, so clients parse one body. A nil Limiter is a
// pass-through (bound disabled / tests).
func Middleware(l *Limiter, errCode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			release, ok := l.acquire(r.Context())
			if !ok {
				write503(w, l.waitBudget, errCode)
				return
			}
			defer release()
			next.ServeHTTP(w, r)
		})
	}
}

// write503 mirrors ratelimit.Write429's shape but with 503 Service Unavailable
// (semantically "temporarily overloaded", which the shipped client renders as
// .serviceUnavailable). Retry-After is at least 1 s.
func write503(w http.ResponseWriter, retry time.Duration, code string) {
	sec := int(retry.Seconds())
	if sec < 1 {
		sec = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(sec))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
}
