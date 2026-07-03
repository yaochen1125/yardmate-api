package inflight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitFor polls cond until true or the deadline, so timing-sensitive tests
// don't rely on fixed sleeps.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestAcquireUpToCapThenReject(t *testing.T) {
	l := New(2, 0, 0) // cap 2, no wait queue, no budget
	r1, ok := l.acquire(context.Background())
	if !ok {
		t.Fatal("1st acquire should succeed")
	}
	if _, ok := l.acquire(context.Background()); !ok {
		t.Fatal("2nd acquire should succeed (cap=2)")
	}
	// 3rd is over cap and maxWait=0 → immediate reject.
	if _, ok := l.acquire(context.Background()); ok {
		t.Fatal("3rd acquire should be rejected when full and maxWait=0")
	}
	// Freeing one slot lets the next in.
	r1()
	if _, ok := l.acquire(context.Background()); !ok {
		t.Fatal("acquire should succeed after a release")
	}
}

func TestWaitThenServedAfterRelease(t *testing.T) {
	l := New(1, 5, 500*time.Millisecond)
	r1, ok := l.acquire(context.Background())
	if !ok {
		t.Fatal("1st acquire should succeed")
	}

	got := make(chan bool, 1)
	go func() {
		_, ok := l.acquire(context.Background())
		got <- ok
	}()

	// The waiter should be queued, not yet served.
	waitFor(t, func() bool { return l.Waiters() == 1 }, time.Second)
	select {
	case <-got:
		t.Fatal("waiter should still be blocked while the slot is held")
	case <-time.After(20 * time.Millisecond):
	}

	// Release the held slot → the waiter is served (a normal 200 path).
	r1()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("waiter should be served after release")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not served in time after release")
	}
}

func TestWaitBudgetTimeout(t *testing.T) {
	l := New(1, 5, 40*time.Millisecond)
	if _, ok := l.acquire(context.Background()); !ok {
		t.Fatal("1st acquire should succeed")
	}
	start := time.Now()
	if _, ok := l.acquire(context.Background()); ok {
		t.Fatal("2nd acquire should time out (slot never released)")
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("acquire returned too early (%s); should wait ~budget", elapsed)
	}
}

func TestWaitQueueCapRejectsExcess(t *testing.T) {
	l := New(1, 1, 500*time.Millisecond) // 1 slot, at most 1 waiter
	if _, ok := l.acquire(context.Background()); !ok {
		t.Fatal("1st acquire should succeed")
	}
	// Fill the single waiter slot with a blocked acquire.
	go l.acquire(context.Background())
	waitFor(t, func() bool { return l.Waiters() == 1 }, time.Second)

	// A 2nd waiter exceeds maxWait=1 → immediate reject (no waiting).
	start := time.Now()
	if _, ok := l.acquire(context.Background()); ok {
		t.Fatal("acquire should be rejected when the wait queue is full")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("over-queue acquire should reject immediately, took %s", elapsed)
	}
}

func TestContextCancelStopsWaiting(t *testing.T) {
	l := New(1, 5, time.Second)
	if _, ok := l.acquire(context.Background()); !ok {
		t.Fatal("1st acquire should succeed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan bool, 1)
	go func() { _, ok := l.acquire(ctx); got <- ok }()
	waitFor(t, func() bool { return l.Waiters() == 1 }, time.Second)
	cancel()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("acquire should fail when context is cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled acquire did not return")
	}
}

func TestMiddleware503WhenFull(t *testing.T) {
	l := New(1, 0, 0) // cap 1, no queue → 2nd concurrent request 503s
	entered := make(chan struct{})
	release := make(chan struct{})
	h := Middleware(l, "server_busy")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered) // req1 only (req2 never enters the handler)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	// req1 occupies the single slot.
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/identify", nil))
	<-entered

	// req2 → 503 with Retry-After + server_busy body.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/identify", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 should carry a Retry-After header")
	}
	if !strings.Contains(rr.Body.String(), "server_busy") {
		t.Fatalf("503 body should contain the error code, got %q", rr.Body.String())
	}

	// Let req1 finish → its slot frees → a new request is served again.
	close(release)
	waitFor(t, func() bool { return len(l.slots) == 0 }, time.Second)
	rr2 := httptest.NewRecorder()
	h2 := Middleware(l, "server_busy")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h2.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/v1/identify", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("slot should be reusable after release; got %d", rr2.Code)
	}
}

func TestNilLimiterIsPassThrough(t *testing.T) {
	h := Middleware(nil, "server_busy")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("nil limiter should pass through; got %d", rr.Code)
	}
}
