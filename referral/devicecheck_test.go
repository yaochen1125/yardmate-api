package referral

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	dcKeyID  = "W3YN83SB24"
	dcTeamID = "PMX32RG52M"
)

// testP8 is a fresh P-256 key in the PKCS#8 PEM form Apple's .p8 uses.
func testP8(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// appleCall is one request the fake Apple saw.
type appleCall struct {
	path   string
	kid    string
	alg    string
	iss    string
	iat    int64
	body   map[string]any
	jwtErr error
}

// fakeApple is an httptest DeviceCheck host: it verifies the JWT with the
// public key and answers with the canned status / body.
type fakeApple struct {
	mu     sync.Mutex
	calls  []appleCall
	status int
	body   string
	delay  time.Duration
}

func (f *fakeApple) server(t *testing.T, pub *ecdsa.PublicKey) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := appleCall{path: r.URL.Path}
		_ = json.Unmarshal(raw, &c.body)
		tok, err := jwt.Parse(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			func(tk *jwt.Token) (any, error) { return pub, nil },
			jwt.WithValidMethods([]string{"ES256"}))
		c.jwtErr = err
		if tok != nil {
			c.kid, _ = tok.Header["kid"].(string)
			c.alg, _ = tok.Header["alg"].(string)
			if claims, ok := tok.Claims.(jwt.MapClaims); ok {
				c.iss, _ = claims["iss"].(string)
				if iat, ok := claims["iat"].(float64); ok {
					c.iat = int64(iat)
				}
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		status, body, delay := f.status, f.body, f.delay
		f.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeApple) answer(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeApple) slow(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

func (f *fakeApple) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeApple) last() appleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func newTestApple(t *testing.T) (*AppleDeviceCheck, *fakeApple, *fakeApple) {
	t.Helper()
	key, p8 := testP8(t)
	prod, dev := &fakeApple{}, &fakeApple{}
	c, err := NewAppleDeviceCheck(dcKeyID, dcTeamID, p8)
	if err != nil {
		t.Fatal(err)
	}
	c.SetBases(prod.server(t, &key.PublicKey).URL, dev.server(t, &key.PublicKey).URL)
	return c, prod, dev
}

func TestAppleDeviceCheckSignsAndShapesTheRequest(t *testing.T) {
	c, prod, _ := newTestApple(t)
	prod.answer(200, `{"bit0":false,"bit1":false,"last_update_time":"2026-10"}`)
	before := time.Now()
	if _, err := c.QueryBit0(context.Background(), DeviceEnvProduction, "TOKEN-B64"); err != nil {
		t.Fatal(err)
	}
	q := prod.last()
	if q.jwtErr != nil {
		t.Fatalf("JWT does not verify with the key's public half: %v", q.jwtErr)
	}
	if q.path != "/v1/query_two_bits" || q.alg != "ES256" || q.kid != dcKeyID || q.iss != dcTeamID {
		t.Errorf("query = path %s alg %s kid %s iss %s", q.path, q.alg, q.kid, q.iss)
	}
	if q.iat < before.Unix()-1 || q.iat > time.Now().Unix()+1 {
		t.Errorf("iat = %d, want now", q.iat)
	}
	if q.body["device_token"] != "TOKEN-B64" {
		t.Errorf("device_token = %v", q.body["device_token"])
	}
	if id, _ := q.body["transaction_id"].(string); len(id) != 36 {
		t.Errorf("transaction_id = %q, want a uuid", id)
	}
	if ts, _ := q.body["timestamp"].(float64); int64(ts) < before.UnixMilli()-1000 || int64(ts) > time.Now().UnixMilli()+1000 {
		t.Errorf("timestamp = %v, want now in ms", q.body["timestamp"])
	}

	if err := c.SetBit0(context.Background(), DeviceEnvProduction, "TOKEN-B64"); err != nil {
		t.Fatal(err)
	}
	u := prod.last()
	if u.path != "/v1/update_two_bits" || u.body["bit0"] != true {
		t.Errorf("update = %s %v, want update_two_bits bit0=true", u.path, u.body)
	}
	if _, sent := u.body["bit1"]; sent {
		t.Error("bit1 must not be sent (left for whatever else uses it)")
	}
	if a, b := q.body["transaction_id"], u.body["transaction_id"]; a == b {
		t.Error("each request needs its own transaction_id")
	}
}

func TestAppleDeviceCheckPicksTheHostByEnvironment(t *testing.T) {
	c, prod, dev := newTestApple(t)
	prod.answer(200, `{"bit0":true}`)
	dev.answer(200, `{"bit0":false}`)
	if used, err := c.QueryBit0(context.Background(), DeviceEnvDevelopment, "t"); err != nil || used {
		t.Errorf("development: %v %v, want the dev host's false", used, err)
	}
	if used, err := c.QueryBit0(context.Background(), DeviceEnvProduction, "t"); err != nil || !used {
		t.Errorf("production: %v %v, want the prod host's true", used, err)
	}
	if prod.count() != 1 || dev.count() != 1 {
		t.Errorf("prod calls %d, dev calls %d, want 1 and 1", prod.count(), dev.count())
	}
	for in, want := range map[string]string{"": DeviceEnvProduction, "production": DeviceEnvProduction,
		"DEVELOPMENT": DeviceEnvDevelopment, " development ": DeviceEnvDevelopment, "staging": DeviceEnvProduction} {
		if got := deviceEnv(in); got != want {
			t.Errorf("deviceEnv(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAppleDeviceCheckReadsEveryAnswer(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		used    bool
		invalid bool
		fails   bool
	}{
		{"bit0 set", 200, `{"bit0":true,"bit1":false,"last_update_time":"2026-10"}`, true, false, false},
		{"bit0 clear", 200, `{"bit0":false,"bit1":true}`, false, false, false},
		{"never set (live wording)", 200, "Failed to find bit state", false, false, false},
		{"never set (doc wording)", 200, "Bit State Not Found", false, false, false},
		{"bad token", 400, "Missing or incorrectly formatted device token payload", false, true, false},
		{"bad key", 401, "Unable to verify authorization token", false, false, true},
		{"apple down", 503, "", false, false, true},
		{"odd 200", 200, "hello", false, false, true},
		{"broken json", 200, `{"bit0":`, false, false, true},
	}
	for _, c := range cases {
		cl, prod, _ := newTestApple(t)
		prod.answer(c.status, c.body)
		used, err := cl.QueryBit0(context.Background(), DeviceEnvProduction, "t")
		if used != c.used || errors.Is(err, ErrDeviceTokenInvalid) != c.invalid || (err != nil) != (c.fails || c.invalid) {
			t.Errorf("%s: used=%v err=%v", c.name, used, err)
		}
	}
	for _, c := range []struct {
		status  int
		invalid bool
		fails   bool
	}{{200, false, false}, {400, true, true}, {401, false, true}, {500, false, true}} {
		cl, prod, _ := newTestApple(t)
		prod.answer(c.status, "")
		err := cl.SetBit0(context.Background(), DeviceEnvProduction, "t")
		if (err != nil) != c.fails || errors.Is(err, ErrDeviceTokenInvalid) != c.invalid {
			t.Errorf("update %d: %v", c.status, err)
		}
	}
}

func TestAppleDeviceCheckTimesOut(t *testing.T) {
	c, prod, _ := newTestApple(t)
	prod.slow(5 * time.Second)
	c.SetTimeout(100 * time.Millisecond)
	start := time.Now()
	_, err := c.QueryBit0(context.Background(), DeviceEnvProduction, "t")
	if err == nil || errors.Is(err, ErrDeviceTokenInvalid) {
		t.Fatalf("err = %v, want a timeout (Apple unavailable)", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v; the client timeout must cut it short", took)
	}
	// The caller's context bounds it too.
	c.SetTimeout(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := c.SetBit0(ctx, DeviceEnvProduction, "t"); err == nil {
		t.Fatal("want a context timeout")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v; the context deadline must cut it short", took)
	}
}

func TestNewAppleDeviceCheckRefusesBadConfig(t *testing.T) {
	_, p8 := testP8(t)
	if _, err := NewAppleDeviceCheck("", dcTeamID, p8); err == nil {
		t.Error("empty key id accepted")
	}
	if _, err := NewAppleDeviceCheck(dcKeyID, " ", p8); err == nil {
		t.Error("empty team id accepted")
	}
	secret := "-----BEGIN PRIVATE KEY-----\nTOPSECRETNOTAKEY\n-----END PRIVATE KEY-----"
	_, err := NewAppleDeviceCheck(dcKeyID, dcTeamID, []byte(secret))
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("garbage key: %v (must fail without echoing the key)", err)
	}
	var none *AppleDeviceCheck
	if _, err := none.QueryBit0(context.Background(), DeviceEnvProduction, "t"); err == nil {
		t.Error("nil client must error")
	}
}

func TestParseDeviceCheckMode(t *testing.T) {
	cases := []struct {
		in   string
		want DeviceCheckMode
		ok   bool
	}{
		{"", DeviceCheckEnforce, true},
		{"enforce", DeviceCheckEnforce, true},
		{" ENFORCE ", DeviceCheckEnforce, true},
		{"log", DeviceCheckLog, true},
		{"Log", DeviceCheckLog, true},
		{"off", DeviceCheckEnforce, false},
	}
	for _, c := range cases {
		if got, ok := ParseDeviceCheckMode(c.in); got != c.want || ok != c.ok {
			t.Errorf("ParseDeviceCheckMode(%q) = %s %v, want %s %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
