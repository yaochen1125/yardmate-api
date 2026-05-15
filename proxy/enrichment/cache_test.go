package enrichment

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// stubDetail is a small helper to build distinct *proxy.PlantDetail pointers
// for cache identity checks (pointer comparison reveals which entry came back).
func stubDetail(scientificName string) *proxy.PlantDetail {
	return &proxy.PlantDetail{ScientificName: scientificName}
}

func TestCache_BasicGetSet(t *testing.T) {
	c := NewCache(10, time.Hour)
	pd := stubDetail("Monstera test")
	c.Set("monstera test", pd)
	got, ok := c.Get("monstera test")
	if !ok {
		t.Fatal("expected hit")
	}
	if got != pd {
		t.Errorf("expected same pointer, got different value: %+v", got)
	}
}

func TestCache_Miss(t *testing.T) {
	c := NewCache(10, time.Hour)
	if _, ok := c.Get("missing"); ok {
		t.Error("expected miss on empty cache")
	}
}

func TestCache_EmptyKeyAndNilValueAreNoOps(t *testing.T) {
	c := NewCache(10, time.Hour)
	c.Set("", stubDetail("never stored"))
	c.Set("k", nil)
	if c.Len() != 0 {
		t.Errorf("expected len 0 after invalid Sets, got %d", c.Len())
	}
	if _, ok := c.Get(""); ok {
		t.Error("empty key should never hit")
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	c := NewCache(10, 80*time.Millisecond)
	c.Set("k", stubDetail("ttl"))
	if _, ok := c.Get("k"); !ok {
		t.Fatal("expected hit immediately after Set")
	}
	time.Sleep(150 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Error("expected miss after TTL window")
	}
}

func TestCache_EvictionOnSizeLimit(t *testing.T) {
	c := NewCache(2, time.Hour)
	c.Set("a", stubDetail("a"))
	c.Set("b", stubDetail("b"))
	c.Set("c", stubDetail("c")) // evicts oldest (a)
	if _, ok := c.Get("a"); ok {
		t.Error("expected 'a' evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("expected 'b' still present")
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("expected 'c' present")
	}
}

func TestCache_Purge(t *testing.T) {
	c := NewCache(10, time.Hour)
	c.Set("a", stubDetail("a"))
	c.Set("b", stubDetail("b"))
	if c.Len() != 2 {
		t.Errorf("expected len 2, got %d", c.Len())
	}
	c.Purge()
	if c.Len() != 0 {
		t.Errorf("expected len 0 after Purge, got %d", c.Len())
	}
}

func TestCache_NilSafe(t *testing.T) {
	var c *Cache
	// All methods must tolerate a nil receiver.
	c.Set("k", stubDetail("a"))
	if got, ok := c.Get("k"); ok || got != nil {
		t.Errorf("nil-cache Get should miss; got=%v ok=%v", got, ok)
	}
	if c.Len() != 0 {
		t.Errorf("nil-cache Len should be 0, got %d", c.Len())
	}
	c.Purge()
}

func TestCache_DefaultsOnNonPositiveArgs(t *testing.T) {
	c := NewCache(0, 0)
	if c == nil {
		t.Fatal("NewCache(0, 0) should still return a valid cache")
	}
	c.Set("k", stubDetail("x"))
	if _, ok := c.Get("k"); !ok {
		t.Error("default-configured cache should still hold entries")
	}
}

// TestCache_Concurrent runs concurrent Set/Get under -race. The actual race
// detection is the value; the test only sanity-checks that some entries
// survive.
func TestCache_Concurrent(t *testing.T) {
	c := NewCache(1000, time.Hour)
	var wg sync.WaitGroup
	const goroutines = 50
	const opsPer = 20
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < opsPer; j++ {
				key := fmt.Sprintf("k%d_%d", i, j%5) // collisions across goroutines
				c.Set(key, stubDetail(key))
				_, _ = c.Get(key)
			}
		}(i)
	}
	wg.Wait()
	if c.Len() == 0 {
		t.Error("expected at least some entries after concurrent ops")
	}
}
