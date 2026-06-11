package main

// Unit tests for the security-critical pure functions behind
// POST /v1/account/delete: Supabase JWT verification (alg pinning, signature,
// expiry, subject extraction), bearer-token parsing, and Apple client_secret
// minting. Network paths (Apple token/revoke, Supabase admin REST) are not
// exercised here — they are best-effort / integration-level.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testES256Key generates a fresh P-256 signing key for a test.
func testES256Key(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen EC key: %v", err)
	}
	return k
}

// signES256 builds a Supabase-style ES256 token with the given claims + kid,
// signed by key.
func signES256(t *testing.T, key *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return s
}

// resolverFor returns a keyForKID func serving key's public key for kid only.
func resolverFor(kid string, key *ecdsa.PrivateKey) func(context.Context, string) (*ecdsa.PublicKey, error) {
	return func(_ context.Context, k string) (*ecdsa.PublicKey, error) {
		if k == kid {
			return &key.PublicKey, nil
		}
		return nil, errors.New("unknown kid")
	}
}

func TestVerifySupabaseToken_Valid(t *testing.T) {
	key := testES256Key(t)
	const kid = "k1"
	const sub = "11111111-2222-3333-4444-555555555555"
	tok := signES256(t, key, kid, jwt.MapClaims{
		"sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	got, err := verifySupabaseToken(context.Background(), tok, resolverFor(kid, key))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != sub {
		t.Fatalf("sub = %q, want %q", got, sub)
	}
}

func TestVerifySupabaseToken_Errors(t *testing.T) {
	now := time.Now()
	key := testES256Key(t)
	const kid = "k1"
	resolver := resolverFor(kid, key)

	expired := signES256(t, key, kid, jwt.MapClaims{
		"sub": "u",
		"exp": now.Add(-time.Minute).Unix(),
	})
	noSub := signES256(t, key, kid, jwt.MapClaims{
		"exp": now.Add(time.Hour).Unix(),
	})
	emptySub := signES256(t, key, kid, jwt.MapClaims{
		"sub": "",
		"exp": now.Add(time.Hour).Unix(),
	})
	// Valid signature + valid sub but NO exp claim must be rejected: an
	// account-destroying endpoint refuses non-expiring tokens
	// (WithExpirationRequired).
	noExp := signES256(t, key, kid, jwt.MapClaims{
		"sub": "11111111-2222-3333-4444-555555555555",
	})
	// Signed with a DIFFERENT key but the SAME kid → the resolver returns the
	// first key, so signature verification must fail.
	wrongSig := signES256(t, testES256Key(t), kid, jwt.MapClaims{
		"sub": "u", "exp": now.Add(time.Hour).Unix(),
	})
	// Valid ES256 token but an UNKNOWN kid → the resolver returns an error.
	unknownKID := signES256(t, key, "other-kid", jwt.MapClaims{
		"sub": "u", "exp": now.Add(time.Hour).Unix(),
	})

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
		{"unknown kid", unknownKID},
		{"alg none", noneStr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifySupabaseToken(context.Background(), tc.token, resolver); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestVerifySupabaseToken_RejectsHS256 guards the algorithm-confusion vector: an
// HS256 token must be rejected by the ES256-only verifier (we never fall back to
// a symmetric secret).
func TestVerifySupabaseToken_RejectsHS256(t *testing.T) {
	key := testES256Key(t)
	const kid = "k1"
	hsTok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u", "exp": time.Now().Add(time.Hour).Unix(),
	})
	hsTok.Header["kid"] = kid
	hsStr, err := hsTok.SignedString([]byte("some-hmac-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifySupabaseToken(context.Background(), hsStr, resolverFor(kid, key)); err == nil {
		t.Fatal("expected HS256 token to be rejected by ES256-only verifier")
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
