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

	"golang.org/x/sync/singleflight"
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

	sf singleflight.Group // coalesces concurrent refreshes into one upstream fetch

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
//
// Concurrency + failure notes:
//   - The network fetch runs WITHOUT c.mu held (the round-trip is up to 8 s;
//     holding the mutex across it would stall every concurrent verify). The lock
//     only guards the map + timestamp reads/writes.
//   - Concurrent refreshes are coalesced via singleflight: a burst of unknown-kid
//     requests at window-open triggers exactly ONE fetchJWKS, and every waiter
//     shares its result. This both caps upstream fan-out (forged-kid flood guard)
//     and, unlike a "reserve the window then 401 everyone else" scheme, still
//     resolves a legitimately freshly-rotated kid that arrives mid-fetch.
//   - lastFetch records the last fetch ATTEMPT (success OR failure), so a JWKS
//     endpoint outage doesn't turn a forged-kid flood into one upstream call per
//     request — at most one attempt per minRefetch window.
func (c *jwksCache) keyForKID(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	if kid == "" {
		return nil, errors.New("jwks: empty kid")
	}

	c.mu.Lock()
	if k, ok := c.keys[kid]; ok {
		c.mu.Unlock()
		return k, nil
	}
	// Cache miss: refetch unless we attempted too recently (balances key-rotation
	// pickup against the forged-kid / endpoint-outage flood guard). lastFetch is
	// zero only before the very first attempt.
	if !c.lastFetch.IsZero() && time.Since(c.lastFetch) < c.minRefetch {
		c.mu.Unlock()
		return nil, errors.New("jwks: unknown kid")
	}
	c.mu.Unlock()

	// Only ONE goroutine per burst runs the fetch; the rest block here and share
	// its (err) result. The shared work updates c.keys + c.lastFetch under the lock.
	_, err, _ := c.sf.Do("refresh", func() (interface{}, error) {
		// Re-check the window inside the flight: a just-completed refresh by a
		// prior leader may have set lastFetch, in which case skip re-fetching.
		c.mu.Lock()
		if !c.lastFetch.IsZero() && time.Since(c.lastFetch) < c.minRefetch {
			c.mu.Unlock()
			return nil, nil
		}
		c.mu.Unlock()

		fetched, ferr := fetchJWKS(ctx, c.httpClient, c.url)

		c.mu.Lock()
		defer c.mu.Unlock()
		c.lastFetch = time.Now() // record the attempt (success or failure) for the floor
		if ferr != nil {
			return nil, ferr
		}
		c.keys = fetched
		return nil, nil
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	// The fetch (this goroutine's or a coalesced leader's) has run; the key is
	// present iff the refreshed set contains it.
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	if err != nil {
		return nil, err
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
