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

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// buildTestServer wires a Server to a fresh temp Store + synthetic Vault.
// Verifier uses the Apple production root pool by default — fine for surface
// tests that exercise error paths before cert validation runs. End-to-end
// happy-path tests using a test root pool live in c6 (integration test).
func buildTestServer(t *testing.T) *Server {
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

	return newServer(verifier, vault)
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
