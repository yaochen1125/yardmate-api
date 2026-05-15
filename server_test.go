package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/ratelimit"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// buildTestServer wires a Server to a fresh temp Store + synthetic Vault +
// permissive rate limits. Verifier uses the Apple production root pool by
// default — fine for surface tests that exercise error paths before cert
// validation runs. End-to-end happy-path tests using a test root pool live
// in c6 (integration test).
func buildTestServer(t *testing.T) *Server {
	return buildTestServerWithLimits(t, 1000, 1000)
}

// buildTestServerWithLimits builds a server with explicit rate-limit budgets,
// for tests that want to trip the limiter.
func buildTestServerWithLimits(t *testing.T, ipLimit, keyIDLimit int) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := attest.OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	verifier, err := attest.New(attest.Options{
		AppID: "TEAM.bundle",
		Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}

	vault, err := secrets.Parse(strings.NewReader(
		"OPENAI_API_KEY=test-openai\nPLANT_ID_API_KEY=test-plantid\n",
	))
	if err != nil {
		t.Fatal(err)
	}

	lim := ratelimit.New(ipLimit, time.Hour, keyIDLimit, 24*time.Hour, 1000, time.Hour)
	return newServer(verifier, vault, lim, nil, nil, nil)
}

func do(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var br io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		br = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, br)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

func TestHealthz(t *testing.T) {
	s := buildTestServer(t)
	rr := do(t, s, "GET", "/healthz", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body)
	}
	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q", resp["status"])
	}
}

func TestAttestChallenge_Returns32Bytes(t *testing.T) {
	s := buildTestServer(t)
	rr := do(t, s, "POST", "/v1/attest/challenge", map[string]string{})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body)
	}
	var resp challengeResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(resp.Challenge)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("challenge len %d, want 32", len(raw))
	}
}

func TestSecretsChallenge_Returns32Bytes(t *testing.T) {
	s := buildTestServer(t)
	rr := do(t, s, "POST", "/v1/secrets/challenge", map[string]string{})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body)
	}
	var resp challengeResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	raw, _ := base64.StdEncoding.DecodeString(resp.Challenge)
	if len(raw) != 32 {
		t.Errorf("challenge len %d, want 32", len(raw))
	}
}

func TestRegister_BadJSON(t *testing.T) {
	s := buildTestServer(t)
	req := httptest.NewRequest("POST", "/v1/attest/register", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code %d, want 400", rr.Code)
	}
}

func TestRegister_MissingField(t *testing.T) {
	s := buildTestServer(t)
	rr := do(t, s, "POST", "/v1/attest/register", map[string]string{
		"keyID": "",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code %d, want 400", rr.Code)
	}
}

func TestRegister_BadBase64(t *testing.T) {
	s := buildTestServer(t)
	rr := do(t, s, "POST", "/v1/attest/register", map[string]string{
		"keyID":       "not-base64!@#",
		"attestation": "AAAA",
		"challenge":   "AAAA",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code %d, want 400", rr.Code)
	}
}

func TestRegister_BadCBORReturnsBadCBOR(t *testing.T) {
	s := buildTestServer(t)
	junk := base64.StdEncoding.EncodeToString([]byte("garbage"))
	rr := do(t, s, "POST", "/v1/attest/register", map[string]string{
		"keyID":       base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"attestation": junk,
		"challenge":   base64.StdEncoding.EncodeToString(make([]byte, 32)),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code %d, want 400 body=%s", rr.Code, rr.Body)
	}
	var resp errorResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Error != "bad_cbor" {
		t.Errorf("error = %q, want bad_cbor", resp.Error)
	}
}

func TestAppSecrets_BadCBORReturnsBadCBOR(t *testing.T) {
	s := buildTestServer(t)
	junk := base64.StdEncoding.EncodeToString([]byte("garbage"))
	rr := do(t, s, "POST", "/v1/app-secrets", map[string]string{
		"keyID":     base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"assertion": junk,
		"challenge": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code %d, want 400 (ErrBadCBOR)", rr.Code)
	}
}

func TestMapAttestError_EveryAttestSentinel(t *testing.T) {
	cases := []struct {
		err  error
		code int
		tag  string
	}{
		{attest.ErrBadCBOR, 400, "bad_cbor"},
		{attest.ErrCertChain, 401, "cert_chain"},
		{attest.ErrAttestNonce, 401, "nonce_mismatch"},
		{attest.ErrAppIDMismatch, 401, "app_id_mismatch"},
		{attest.ErrAAGUIDMismatch, 401, "aaguid_mismatch"},
		{attest.ErrCredentialIDMismatch, 401, "credential_id_mismatch"},
		{attest.ErrCredentialUnknown, 401, "credential_unknown"},
		{attest.ErrBadSignature, 401, "signature_invalid"},
		{attest.ErrChallengeUnknown, 401, "challenge_unknown"},
		{attest.ErrChallengeExpired, 401, "challenge_expired"},
		{attest.ErrChallengeReplay, 401, "challenge_replay"},
		{attest.ErrCounterNotZero, 409, "counter_not_zero"},
		{attest.ErrCounterNotMonotonic, 409, "counter_not_monotonic"},
	}
	for _, tc := range cases {
		gotCode, gotTag := mapAttestError(tc.err)
		if gotCode != tc.code || gotTag != tc.tag {
			t.Errorf("%v → (%d %q), want (%d %q)", tc.err, gotCode, gotTag, tc.code, tc.tag)
		}
	}
	if c, _ := mapAttestError(nil); c != http.StatusOK {
		t.Errorf("nil → %d, want 200", c)
	}
}

func TestPerIPRateLimit_Triggers429(t *testing.T) {
	s := buildTestServerWithLimits(t, 2, 1000) // 2 req per IP per hour
	fire := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/attest/challenge", strings.NewReader("{}"))
		req.RemoteAddr = "10.1.2.3:9999"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		return rr
	}
	for i := 0; i < 2; i++ {
		rr := fire()
		if rr.Code != http.StatusOK {
			t.Errorf("req %d code %d, want 200", i, rr.Code)
		}
	}
	rr := fire()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd req code %d, want 429 body=%s", rr.Code, rr.Body)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
	var resp errorResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Error != "rate_limit_ip" {
		t.Errorf("error = %q, want rate_limit_ip", resp.Error)
	}
}

func TestPerIPRateLimit_IsolatesByIP(t *testing.T) {
	s := buildTestServerWithLimits(t, 1, 1000)
	hit := func(ip string) int {
		req := httptest.NewRequest("POST", "/v1/attest/challenge", strings.NewReader("{}"))
		req.RemoteAddr = ip
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := hit("10.0.0.1:1"); c != 200 {
		t.Errorf("10.0.0.1 first = %d", c)
	}
	if c := hit("10.0.0.1:2"); c != 429 {
		t.Errorf("10.0.0.1 second = %d", c)
	}
	if c := hit("10.0.0.2:1"); c != 200 {
		t.Errorf("10.0.0.2 first = %d", c)
	}
}

func TestHealthzNotRateLimited(t *testing.T) {
	s := buildTestServerWithLimits(t, 1, 1000)
	// /healthz is outside /v1 — no rate limit. Fire 5x from same IP.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/healthz", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("req %d code %d", i, rr.Code)
		}
	}
}

func TestRequestBodySizeCap_Returns413(t *testing.T) {
	s := buildTestServer(t)
	// Valid JSON > 64 KB cap. We need a syntactically valid body so the
	// JSON decoder doesn't bail with bad_json before MaxBytesReader fires.
	big := `{"keyID":"AAAA","attestation":"AAAA","challenge":"` + strings.Repeat("A", 128*1024) + `"}`
	req := httptest.NewRequest("POST", "/v1/attest/register", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d, want 413 body=%s", rr.Code, rr.Body)
	}
	var resp errorResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Error != "body_too_large" {
		t.Errorf("error = %q, want body_too_large", resp.Error)
	}
}

func TestVendedKeysAllPresent(t *testing.T) {
	// Sanity: every vended key resolves on a Vault populated with values.
	v, _ := secrets.Parse(strings.NewReader(
		"OPENAI_API_KEY=k1\nPLANT_ID_API_KEY=k2\n",
	))
	snap := v.Snapshot(vendedKeys)
	if snap["openai_api_key"] != "k1" || snap["plant_id_api_key"] != "k2" {
		t.Errorf("vended snapshot = %v", snap)
	}
}
