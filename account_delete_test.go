package main

// Unit tests for the security-critical pure functions behind
// POST /v1/account/delete: Supabase JWT verification (alg pinning, signature,
// expiry, subject extraction), bearer-token parsing, and Apple client_secret
// minting. Network paths (Apple token/revoke, Supabase admin REST) are not
// exercised here — they are best-effort / integration-level.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testJWTSecret = "super-secret-hs256-key-for-tests"

// signHS256 builds a Supabase-style HS256 token with the given claims signed by
// testJWTSecret.
func signHS256(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	return s
}

func TestVerifySupabaseToken_Valid(t *testing.T) {
	const sub = "11111111-2222-3333-4444-555555555555"
	tok := signHS256(t, jwt.MapClaims{
		"sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	got, err := verifySupabaseToken(tok, testJWTSecret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != sub {
		t.Fatalf("sub = %q, want %q", got, sub)
	}
}

func TestVerifySupabaseToken_Errors(t *testing.T) {
	now := time.Now()

	expired := signHS256(t, jwt.MapClaims{
		"sub": "u",
		"exp": now.Add(-time.Minute).Unix(),
	})
	noSub := signHS256(t, jwt.MapClaims{
		"exp": now.Add(time.Hour).Unix(),
	})
	emptySub := signHS256(t, jwt.MapClaims{
		"sub": "",
		"exp": now.Add(time.Hour).Unix(),
	})
	// Valid signature + valid sub but NO exp claim must be rejected: an
	// account-destroying endpoint refuses non-expiring tokens
	// (WithExpirationRequired). Without that option jwt/v5 would accept this.
	noExp := signHS256(t, jwt.MapClaims{
		"sub": "11111111-2222-3333-4444-555555555555",
	})

	// A token signed with a DIFFERENT secret must fail signature verification.
	wrongTok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u", "exp": now.Add(time.Hour).Unix(),
	})
	wrongSig, err := wrongTok.SignedString([]byte("a-totally-different-secret"))
	if err != nil {
		t.Fatal(err)
	}

	// alg=none token (the classic JWT downgrade). jwt/v5 requires
	// jwt.UnsafeAllowNoneSignatureType as the key for "none"; we craft it so the
	// string is well-formed, then assert our verifier rejects it.
	noneTok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "u", "exp": now.Add(time.Hour).Unix(),
	})
	noneStr, err := noneTok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not-a-jwt"},
		{"expired", expired},
		{"missing sub", noSub},
		{"empty sub", emptySub},
		{"missing exp", noExp},
		{"wrong signature", wrongSig},
		{"alg none", noneStr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifySupabaseToken(tc.token, testJWTSecret); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestVerifySupabaseToken_EmptySecret(t *testing.T) {
	tok := signHS256(t, jwt.MapClaims{"sub": "u", "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := verifySupabaseToken(tok, ""); err == nil {
		t.Fatal("expected error with empty secret, got nil")
	}
}

// TestVerifySupabaseToken_RejectsESWithHMACSecret guards the algorithm-confusion
// vector: a token presented as ES256 must not be accepted by the HS256 verifier
// even though our function holds an HMAC secret. (We can't sign a real ES256
// token with the HMAC secret, so we assert that an ES256-headed token built from
// a real EC key is rejected because the verifier only accepts HMAC.)
func TestVerifySupabaseToken_RejectsESAlg(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	esTok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"sub": "u", "exp": time.Now().Add(time.Hour).Unix(),
	})
	esStr, err := esTok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifySupabaseToken(esStr, testJWTSecret); err == nil {
		t.Fatal("expected ES256 token to be rejected by HS256-only verifier")
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc.def.ghi", "abc.def.ghi"},
		{"bearer abc", "abc"},         // case-insensitive scheme
		{"BEARER  spaced ", "spaced"}, // trims surrounding spaces of the token
		{"Basic abc", ""},             // wrong scheme
		{"", ""},                      // absent
		{"abc.def.ghi", ""},           // no scheme
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/v1/account/delete", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		if got := bearerToken(req); got != tc.want {
			t.Errorf("bearerToken(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestAppleRevokeConfig_Valid(t *testing.T) {
	base := appleRevokeConfig{
		teamID: "TEAM123456", keyID: "KEY1234567", bundleID: "com.example.app",
		privateKeyPEM: "x",
	}
	if !base.valid() {
		t.Fatal("expected valid config")
	}
	// Missing any required field => invalid.
	for _, mut := range []func(c *appleRevokeConfig){
		func(c *appleRevokeConfig) { c.teamID = "" },
		func(c *appleRevokeConfig) { c.keyID = "" },
		func(c *appleRevokeConfig) { c.bundleID = "" },
		func(c *appleRevokeConfig) { c.privateKeyPEM = ""; c.privateKeyPath = "" },
	} {
		c := base
		mut(&c)
		if c.valid() {
			t.Errorf("expected invalid config after mutation: %+v", c)
		}
	}
	// Path-only key material is still valid.
	c := base
	c.privateKeyPEM = ""
	c.privateKeyPath = "/some/key.p8"
	if !c.valid() {
		t.Fatal("expected valid config with path-only key")
	}
}

// genTestP8 produces a PKCS#8 PEM EC private key (the same encoding Apple's .p8
// uses) so clientSecret can parse + sign with it.
func genTestP8(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestAppleClientSecret_SignsAndCarriesKID(t *testing.T) {
	cfg := appleRevokeConfig{
		teamID:        "TEAM123456",
		keyID:         "KEY1234567",
		bundleID:      "com.example.app",
		privateKeyPEM: genTestP8(t),
	}
	now := time.Unix(1_700_000_000, 0)
	secret, err := cfg.clientSecret(now)
	if err != nil {
		t.Fatalf("clientSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("empty client secret")
	}
	// Parse the unverified header/claims to assert structure (we don't have the
	// public key handy in the prod struct; structure is what matters here).
	parser := jwt.NewParser()
	tok, _, err := parser.ParseUnverified(secret, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse unverified: %v", err)
	}
	if alg, _ := tok.Header["alg"].(string); alg != "ES256" {
		t.Errorf("alg = %v, want ES256", tok.Header["alg"])
	}
	if kid, _ := tok.Header["kid"].(string); kid != cfg.keyID {
		t.Errorf("kid = %v, want %q", tok.Header["kid"], cfg.keyID)
	}
	claims := tok.Claims.(jwt.MapClaims)
	if claims["iss"] != cfg.teamID {
		t.Errorf("iss = %v, want %q", claims["iss"], cfg.teamID)
	}
	if claims["sub"] != cfg.bundleID {
		t.Errorf("sub = %v, want %q", claims["sub"], cfg.bundleID)
	}
	if claims["aud"] != appleAudience {
		t.Errorf("aud = %v, want %q", claims["aud"], appleAudience)
	}
}

func TestAppleClientSecret_EscapedNewlinesInPEM(t *testing.T) {
	raw := genTestP8(t)
	// Simulate an env store that flattened newlines to the literal \n sequence.
	escaped := ""
	for _, r := range raw {
		if r == '\n' {
			escaped += `\n`
			continue
		}
		escaped += string(r)
	}
	cfg := appleRevokeConfig{
		teamID: "T", keyID: "K", bundleID: "B", privateKeyPEM: escaped,
	}
	if _, err := cfg.clientSecret(time.Now()); err != nil {
		t.Fatalf("clientSecret with escaped-newline PEM should succeed, got: %v", err)
	}
}
