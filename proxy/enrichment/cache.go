// Package enrichment implements POST /v1/plants/enrichment — the V1
// plant-detail enrichment endpoint. See SPEC.md for the full contract.
//
// Architecture: a Service orchestrates a three-tier lookup
//   (catalog embed → Supabase plants_pending → OpenAI gpt-4o-mini)
// behind an in-process LRU+TTL cache. The cache absorbs hot-plant
// traffic so investment-scale bursts don't all hit Supabase / OpenAI.
package enrichment

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// Default cache sizing per SPEC §7 cache decision.
const (
	DefaultCacheSize = 10_000
	DefaultCacheTTL  = 30 * time.Minute
)

// Cache is an in-memory LRU+TTL cache for PlantDetail lookups, keyed by the
// normalized scientific name (the same key used by Supabase plants_pending
// + the catalog's LookupPlantID). Wraps hashicorp/golang-lru/v2/expirable.
//
// The Service writes to the cache on EVERY successful 200 response — catalog
// hit, Supabase hit, AND fresh LLM generation. TTL bounds staleness when Yao
// updates a row in the Supabase Dashboard; immediate invalidation is V1.x
// (SPEC §9 #13).
//
// Concurrent reads + writes are safe (the underlying LRU holds its own mutex).
type Cache struct {
	lru *expirable.LRU[string, *proxy.PlantDetail]
}

// NewCache builds a cache with the given size cap and TTL. Non-positive
// arguments fall back to the package defaults.
func NewCache(size int, ttl time.Duration) *Cache {
	if size <= 0 {
		size = DefaultCacheSize
	}
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Cache{
		lru: expirable.NewLRU[string, *proxy.PlantDetail](size, nil, ttl),
	}
}

// Get returns the cached PlantDetail for key. Returns (nil, false) on miss
// or expired entries.
func (c *Cache) Get(key string) (*proxy.PlantDetail, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	return c.lru.Get(key)
}

// Set stores value under key. nil value or empty key are no-ops (never store
// a sentinel "miss" entry; misses are represented by absence).
func (c *Cache) Set(key string, value *proxy.PlantDetail) {
	if c == nil || key == "" || value == nil {
		return
	}
	c.lru.Add(key, value)
}

// Len reports the current (non-expired) entry count.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	return c.lru.Len()
}

// Purge removes every entry. Used by tests + the future V1.x admin invalidate
// endpoint (SPEC §9 #13).
func (c *Cache) Purge() {
	if c == nil {
		return
	}
	c.lru.Purge()
}
