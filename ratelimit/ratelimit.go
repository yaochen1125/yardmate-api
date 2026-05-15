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

// Limiter bundles the production buckets used by yardmate-api:
//   - PerIP — applied at /v1 router scope via PerIPMiddleware
//   - PerKeyID — checked inside /v1/app-secrets after assertion verification
//   - PerDevice — applied at proxy-endpoint group (/v1/identify, /v1/diagnose)
//     via PerDeviceMiddleware, keyed by X-Device-Install-Id
//
// See SPEC §4 for application points and the rationale for layering per-IP +
// per-device on the same proxy endpoints (defense-in-depth against IP-rotation
// attackers who reuse the same client install).
type Limiter struct {
	PerIP     *Bucket
	PerKeyID  *Bucket
	PerDevice *Bucket
}

// New constructs a Limiter with the three production buckets. The device
// bucket is independent of the IP bucket (different key universe, different
// quota); both are applied as middleware on the proxy endpoint group.
func New(
	ipLimit int, ipWindow time.Duration,
	keyIDLimit int, keyIDWindow time.Duration,
	deviceLimit int, deviceWindow time.Duration,
) *Limiter {
	return &Limiter{
		PerIP:     NewBucket(ipLimit, ipWindow),
		PerKeyID:  NewBucket(keyIDLimit, keyIDWindow),
		PerDevice: NewBucket(deviceLimit, deviceWindow),
	}
}

// StartSweeper runs a background goroutine that calls Sweep on all buckets
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
				l.PerDevice.Sweep(now)
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

// PerDeviceMiddleware wraps a handler with a rate limit keyed by the
// X-Device-Install-Id header (an RFC 4122 UUID generated client-side and
// stored in the iOS Keychain).
//
// Behaviour on a missing or malformed X-Device-Install-Id: the middleware
// passes through to the next handler instead of writing its own 400. The
// proxy handlers already perform missing_device_id validation, and we don't
// want every malformed-header request to share one global empty-string bucket
// (defeats the per-device isolation). Counter side effect: the empty/malformed
// case isn't rate-limited at the device layer, but per-IP still applies and
// the handler 400s before any upstream cost.
//
// On limit: 429 with Retry-After header + JSON body {"error":"<errCode>"}.
func PerDeviceMiddleware(b *Bucket, errCode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Device-Install-Id")
			if !isUUID(id) {
				next.ServeHTTP(w, r)
				return
			}
			allowed, retry := b.Allow(id, time.Now())
			if !allowed {
				Write429(w, retry, errCode)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isUUID accepts RFC 4122 canonical form (36 chars with dashes at positions
// 8/13/18/23). Case-insensitive for hex digits. Duplicated from proxy.isUUID
// because both packages need the check and adding a third package only for
// this 12-line helper isn't worth it.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
