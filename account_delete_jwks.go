package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// jwksCache fetches and caches the Supabase project's JWT signing public keys
// (ES256 / EC P-256) from the public JWKS endpoint and resolves them by `kid`.
//
// Supabase projects on asymmetric JWT signing publish their public keys at
// {SUPABASE_URL}/auth/v1/.well-known/jwks.json (public, no apikey required). We
// verify access tokens against these instead of a shared HS256 secret, so the
// account-delete endpoint needs SUPABASE_URL but no SUPABASE_JWT_SECRET.
type jwksCache struct {
	url        string // SUPABASE_URL (no trailing slash)
	httpClient *http.Client
	minRefetch time.Duration // floor between network refetches (forged-kid flood guard)

	mu        sync.Mutex
	keys      map[string]*ecdsa.PublicKey
	lastFetch time.Time
}

func newJWKSCache(supabaseURL string) *jwksCache {
	return &jwksCache{
		url:        supabaseURL,
		httpClient: &http.Client{Timeout: 8 * time.Second},
		minRefetch: time.Minute,
	}
}

// keyForKID returns the EC public key for kid, fetching/refreshing the JWKS on a
// cache miss. Refetch is rate-limited by minRefetch so an attacker streaming
// random kids cannot force unbounded upstream fetches. Returns an error when the
// kid is unknown (the caller maps that to a 401).
func (c *jwksCache) keyForKID(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	if kid == "" {
		return nil, errors.New("jwks: empty kid")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	// Cache miss: refetch unless we just did (balances key-rotation pickup
	// against the forged-kid flood guard).
	if c.keys != nil && time.Since(c.lastFetch) < c.minRefetch {
		return nil, errors.New("jwks: unknown kid")
	}
	fetched, err := fetchJWKS(ctx, c.httpClient, c.url)
	if err != nil {
		return nil, err
	}
	c.keys = fetched
	c.lastFetch = time.Now()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("jwks: unknown kid")
}

// fetchJWKS GETs the project JWKS and builds EC P-256 public keys keyed by kid.
// Only EC/P-256 entries are kept (Supabase's asymmetric signing alg is ES256).
func fetchJWKS(ctx context.Context, client *http.Client, baseURL string) (map[string]*ecdsa.PublicKey, error) {
	fctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, baseURL+"/auth/v1/.well-known/jwks.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: fetch status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("jwks: decode: %w", err)
	}
	out := make(map[string]*ecdsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "EC" || k.Crv != "P-256" || k.Kid == "" {
			continue
		}
		xb, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			continue
		}
		yb, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			continue
		}
		out[k.Kid] = &ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(xb),
			Y:     new(big.Int).SetBytes(yb),
		}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no usable EC P-256 keys")
	}
	return out, nil
}
