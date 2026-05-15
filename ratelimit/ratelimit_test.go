package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBucket_AllowsUntilLimit(t *testing.T) {
	b := NewBucket(3, time.Hour)
	now := time.Now()
	for i := 0; i < 3; i++ {
		ok, _ := b.Allow("k", now)
		if !ok {
			t.Errorf("req %d should be allowed", i)
		}
	}
	ok, retry := b.Allow("k", now)
	if ok {
		t.Error("4th req should be denied")
	}
	if retry <= 0 {
		t.Errorf("retry %v should be positive", retry)
	}
}

func TestBucket_ResetsAfterWindow(t *testing.T) {
	b := NewBucket(1, time.Minute)
	now := time.Now()
	if ok, _ := b.Allow("k", now); !ok {
		t.Fatal("first denied")
	}
	if ok, _ := b.Allow("k", now); ok {
		t.Fatal("second within window allowed")
	}
	if ok, _ := b.Allow("k", now.Add(2*time.Minute)); !ok {
		t.Error("after window should be allowed")
	}
}

func TestBucket_PerKeyIsolation(t *testing.T) {
	b := NewBucket(1, time.Minute)
	now := time.Now()
	if ok, _ := b.Allow("a", now); !ok {
		t.Fatal("a first")
	}
	if ok, _ := b.Allow("a", now); ok {
		t.Fatal("a second should deny")
	}
	if ok, _ := b.Allow("b", now); !ok {
		t.Error("b should be independent")
	}
}

func TestBucket_Sweep(t *testing.T) {
	b := NewBucket(1, time.Minute)
	now := time.Now()
	b.Allow("fresh1", now)
	b.Allow("fresh2", now)
	b.Allow("expired", now.Add(-2*time.Minute))
	if got := b.Size(); got != 3 {
		t.Fatalf("size %d, want 3", got)
	}
	removed := b.Sweep(now)
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}
	if b.Size() != 2 {
		t.Errorf("size after sweep %d, want 2", b.Size())
	}
}

func TestBucket_ConcurrentAllow(t *testing.T) {
	b := NewBucket(100, time.Hour)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Allow("shared", now)
		}()
	}
	wg.Wait()
	if got := b.Size(); got != 1 {
		t.Errorf("size %d, want 1 (single key)", got)
	}
}

func TestPerIPMiddleware_AllowThenDeny(t *testing.T) {
	b := NewBucket(2, time.Minute)
	handler := PerIPMiddleware(b, "rate_limit_ip")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	fire := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	for i := 0; i < 2; i++ {
		if rr := fire("192.168.1.1:1234"); rr.Code != 200 {
			t.Errorf("req %d code %d, want 200", i, rr.Code)
		}
	}
	rr := fire("192.168.1.1:9999")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("code %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
	if rr.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content-type %q", rr.Header().Get("Content-Type"))
	}
	if rr.Body.String() != `{"error":"rate_limit_ip"}` {
		t.Errorf("body %q", rr.Body.String())
	}
}

func TestPerIPMiddleware_IsolatesByIP(t *testing.T) {
	b := NewBucket(1, time.Minute)
	handler := PerIPMiddleware(b, "rate_limit_ip")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	fire := func(ip string) int {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = ip
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := fire("1.1.1.1:1"); c != 200 {
		t.Errorf("1.1.1.1 first = %d", c)
	}
	if c := fire("1.1.1.1:2"); c != 429 {
		t.Errorf("1.1.1.1 second = %d", c)
	}
	if c := fire("2.2.2.2:1"); c != 200 {
		t.Errorf("2.2.2.2 first = %d", c)
	}
}

func TestLimiter_StartSweeperStopsCleanly(t *testing.T) {
	l := New(10, time.Hour, 5, 24*time.Hour, 10, time.Hour)
	stop := l.StartSweeper(10 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	close(stop)
	// If the goroutine leaked, `go test -race` would report it; this test
	// also serves as a smoke that StartSweeper doesn't panic.
}

// testUUID is a fixed RFC 4122 string used for PerDevice tests.
const testUUID = "ABCDEF01-2345-6789-ABCD-EF0123456789"

func TestPerDeviceMiddleware_AllowThenDeny(t *testing.T) {
	b := NewBucket(2, time.Minute)
	handler := PerDeviceMiddleware(b, "rate_limit_device")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	fire := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/identify", nil)
		req.Header.Set("X-Device-Install-Id", testUUID)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	for i := 0; i < 2; i++ {
		if rr := fire(); rr.Code != 200 {
			t.Errorf("req %d code %d, want 200", i, rr.Code)
		}
	}
	rr := fire()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd code %d, want 429 body=%s", rr.Code, rr.Body)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
	if rr.Body.String() != `{"error":"rate_limit_device"}` {
		t.Errorf("body %q", rr.Body.String())
	}
}

func TestPerDeviceMiddleware_MissingHeader_PassThrough(t *testing.T) {
	// Missing X-Device-Install-Id: middleware passes through; handler is
	// expected to 400 with missing_device_id. The bucket must not record
	// anything for the empty key (otherwise every malformed-header request
	// would share one global bucket).
	b := NewBucket(10, time.Minute)
	called := false
	handler := PerDeviceMiddleware(b, "rate_limit_device")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusBadRequest)
	}))
	req := httptest.NewRequest("POST", "/v1/identify", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if !called {
		t.Fatal("downstream handler should be called when device id is missing")
	}
	if b.Size() != 0 {
		t.Errorf("bucket size %d after missing-header, want 0", b.Size())
	}
}

func TestPerDeviceMiddleware_BadUUID_PassThrough(t *testing.T) {
	b := NewBucket(10, time.Minute)
	called := false
	handler := PerDeviceMiddleware(b, "rate_limit_device")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusBadRequest)
	}))
	req := httptest.NewRequest("POST", "/v1/identify", nil)
	req.Header.Set("X-Device-Install-Id", "not-a-uuid")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if !called {
		t.Fatal("downstream handler should be called for malformed device id (handler returns its own 400)")
	}
	if b.Size() != 0 {
		t.Errorf("bucket size %d after bad UUID, want 0", b.Size())
	}
}

func TestPerDeviceMiddleware_IsolatesByDeviceID(t *testing.T) {
	b := NewBucket(1, time.Minute)
	handler := PerDeviceMiddleware(b, "rate_limit_device")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	fire := func(id string) int {
		req := httptest.NewRequest("POST", "/v1/identify", nil)
		req.Header.Set("X-Device-Install-Id", id)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr.Code
	}
	uuidA := "11111111-2222-3333-4444-555555555555"
	uuidB := "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
	if c := fire(uuidA); c != 200 {
		t.Errorf("A first = %d", c)
	}
	if c := fire(uuidA); c != 429 {
		t.Errorf("A second = %d", c)
	}
	if c := fire(uuidB); c != 200 {
		t.Errorf("B first (different device) = %d", c)
	}
}

func TestIsUUID_RatelimitPkg(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{testUUID, true},
		{"abcdef01-2345-6789-abcd-ef0123456789", true},
		{"", false},
		{"abc", false},
		{"11111111222233334444555555555555", false},
	}
	for _, tc := range cases {
		if got := isUUID(tc.s); got != tc.want {
			t.Errorf("isUUID(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
