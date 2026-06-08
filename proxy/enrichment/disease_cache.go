package enrichment

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// cachedDisease is the disease-cache value: the back-filled detail plus its
// O-series catalog id (both needed to reconstruct a HealthIssue without a DB
// round-trip).
type cachedDisease struct {
	Detail    *proxy.StructuredDiseaseDetail
	CatalogID string
}

// DiseaseCache is an in-process LRU+TTL over normalized disease name →
// cachedDisease. Mirrors Cache; key = proxy.NormalizeDiseaseName(name)
// (plant-agnostic, same SOT as the diseases_pending PK).
type DiseaseCache struct {
	lru *expirable.LRU[string, cachedDisease]
}

func NewDiseaseCache(size int, ttl time.Duration) *DiseaseCache {
	if size <= 0 {
		size = DefaultCacheSize
	}
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &DiseaseCache{lru: expirable.NewLRU[string, cachedDisease](size, nil, ttl)}
}

func (c *DiseaseCache) Get(key string) (cachedDisease, bool) {
	if c == nil || key == "" {
		return cachedDisease{}, false
	}
	return c.lru.Get(key)
}

// Set never stores an incomplete value (nil detail or empty catalog id) so a
// DB-down generation (no minted O id) is not cached as a permanent hit.
func (c *DiseaseCache) Set(key string, value cachedDisease) {
	if c == nil || key == "" || value.Detail == nil || value.CatalogID == "" {
		return
	}
	c.lru.Add(key, value)
}

func (c *DiseaseCache) Len() int {
	if c == nil {
		return 0
	}
	return c.lru.Len()
}
