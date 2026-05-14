// Package ratelimit implements fixed-window per-key rate limiting for the
// yardmate-api endpoints. See SPEC.md.
package ratelimit

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Bucket is a fixed-window rate limiter keyed by an opaque string.
// Memory is bounded by periodic Sweep calls.
type Bucket struct {
	limit  int
	window time.Duration

	mu    sync.Mutex
	cache map[string]*windowEntry
}

type windowEntry struct {
	count   int
	resetAt time.Time
}

// NewBucket creates a Bucket allowing limit requests per window per key.
func NewBucket(limit int, window time.Duration) *Bucket {
	if limit < 1 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Bucket{
		limit:  limit,
		window: window,
		cache:  make(map[string]*windowEntry),
	}
}

// Allow records a request against key and reports whether it's within budget.
// retryAfter is the duration until the current window resets (0 when allowed).
func (b *Bucket) Allow(key string, now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.cache[key]
	if !ok || now.After(e.resetAt) {
		b.cache[key] = &windowEntry{count: 1, resetAt: now.Add(b.window)}
		return true, 0
	}
	if e.count >= b.limit {
		return false, e.resetAt.Sub(now)
	}
	e.count++
	return true, 0
}

// Sweep deletes entries whose window has elapsed. Returns the number removed.
func (b *Bucket) Sweep(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for k, e := range b.cache {
		if now.After(e.resetAt) {
			delete(b.cache, k)
			n++
		}
	}
	return n
}

// Size returns the current number of tracked keys (test-only diagnostic).
func (b *Bucket) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.cache)
}

// Limiter bundles the per-IP and per-keyID buckets used by yardmate-api.
// Wire PerIP via PerIPMiddleware on the /v1 routes; check PerKeyID inside
// /v1/app-secrets after assertion verification (SPEC §4).
type Limiter struct {
	PerIP    *Bucket
	PerKeyID *Bucket
}

// New constructs a Limiter with the two production buckets.
func New(ipLimit int, ipWindow time.Duration, keyIDLimit int, keyIDWindow time.Duration) *Limiter {
	return &Limiter{
		PerIP:    NewBucket(ipLimit, ipWindow),
		PerKeyID: NewBucket(keyIDLimit, keyIDWindow),
	}
}

// StartSweeper runs a background goroutine that calls Sweep on both buckets
// every interval. Returns a stop channel; close it to terminate the goroutine.
func (l *Limiter) StartSweeper(interval time.Duration) chan<- struct{} {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				l.PerIP.Sweep(now)
				l.PerKeyID.Sweep(now)
			}
		}
	}()
	return stop
}

// PerIPMiddleware wraps a handler. The bucket key is the remote IP (port stripped).
// Assumes chi's middleware.RealIP has already populated r.RemoteAddr with the
// client IP (see SPEC §6.1).
//
// On limit: 429 with Retry-After header + JSON body {"error":"<errCode>"}.
func PerIPMiddleware(b *Bucket, errCode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := extractIP(r)
			allowed, retry := b.Allow(ip, time.Now())
			if !allowed {
				Write429(w, retry, errCode)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func extractIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Write429 writes the standard 429 response shape: Retry-After header (seconds,
// always at least 1) + a JSON body {"error":"<code>"}. Exposed so handler-side
// per-keyID denials produce identical wire shape.
func Write429(w http.ResponseWriter, retry time.Duration, code string) {
	sec := int(retry.Seconds()) + 1
	if sec < 1 {
		sec = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(sec))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
}
