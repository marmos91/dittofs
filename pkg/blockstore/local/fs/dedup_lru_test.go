package fs

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// makeHash returns a ContentHash with first byte set to b for compact test
// readability. All other bytes are zero — adequate for LRU-key uniqueness
// inside individual tests.
func makeHash(b byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	h[0] = b
	return h
}

func TestDedupLRU_GetMiss_ReturnsNotOk(t *testing.T) {
	lru := newDedupLRU(8)
	if pid, ok := lru.Get(makeHash(1)); ok || pid != "" {
		t.Fatalf("empty LRU Get: got (%q,%v), want (\"\",false)", pid, ok)
	}
}

func TestDedupLRU_PutThenGet_ReturnsValue(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x42)
	lru.Put(h, "payload-1")
	pid, ok := lru.Get(h)
	if !ok || pid != "payload-1" {
		t.Fatalf("after Put: got (%q,%v), want (\"payload-1\",true)", pid, ok)
	}
}

func TestDedupLRU_Has_ReturnsTrueAfterPut(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x7)
	lru.Put(h, "p")
	if !lru.Has(h) {
		t.Fatalf("Has(known) = false, want true")
	}
	if lru.Has(makeHash(0xFF)) {
		t.Fatalf("Has(unknown) = true, want false")
	}
}

func TestDedupLRU_EvictsLRUWhenOverCapacity(t *testing.T) {
	lru := newDedupLRU(3)
	h1 := makeHash(1)
	h2 := makeHash(2)
	h3 := makeHash(3)
	h4 := makeHash(4)
	lru.Put(h1, "p1")
	lru.Put(h2, "p2")
	lru.Put(h3, "p3")
	lru.Put(h4, "p4") // forces eviction of h1 (LRU)

	if _, ok := lru.Get(h1); ok {
		t.Fatalf("h1 should have been evicted")
	}
	for i, h := range []blockstore.ContentHash{h2, h3, h4} {
		if _, ok := lru.Get(h); !ok {
			t.Fatalf("h%d should still be present", i+2)
		}
	}
}

func TestDedupLRU_PromoteOnGet(t *testing.T) {
	lru := newDedupLRU(3)
	h1 := makeHash(1)
	h2 := makeHash(2)
	h3 := makeHash(3)
	h4 := makeHash(4)
	lru.Put(h1, "p1")
	lru.Put(h2, "p2")
	lru.Put(h3, "p3")
	// Touch h1 — promotes it to MRU.
	if _, ok := lru.Get(h1); !ok {
		t.Fatalf("h1 missing before promote")
	}
	lru.Put(h4, "p4") // evicts h2 (now the LRU)

	if _, ok := lru.Get(h1); !ok {
		t.Fatalf("h1 should still be present after promote")
	}
	if _, ok := lru.Get(h2); ok {
		t.Fatalf("h2 should have been evicted after promoting h1")
	}
}

func TestDedupLRU_DuplicatePut_PromotesAndUpdates(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x1)
	lru.Put(h, "p1")
	lru.Put(h, "p2")
	pid, ok := lru.Get(h)
	if !ok || pid != "p2" {
		t.Fatalf("duplicate Put should update payloadID: got (%q,%v), want (\"p2\",true)", pid, ok)
	}
	if got := lru.order.Len(); got != 1 {
		t.Fatalf("duplicate Put should keep order.Len()==1, got %d", got)
	}
}

func TestDedupLRU_ConcurrentAccess_NoRace(t *testing.T) {
	lru := newDedupLRU(64)
	// Shared key set: ensures concurrent writers contend.
	keys := make([]blockstore.ContentHash, 32)
	for i := range keys {
		keys[i] = makeHash(byte(i))
	}
	var wg sync.WaitGroup
	stop := time.After(100 * time.Millisecond)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				k := keys[i%len(keys)]
				if i%2 == 0 {
					lru.Put(k, "p")
				} else {
					_, _ = lru.Get(k)
					_ = lru.Has(k)
				}
				i++
			}
		}(g)
	}
	wg.Wait()
}

func TestDedupLRU_ZeroSize_DegradesToNoop(t *testing.T) {
	lru := newDedupLRU(0)
	// Operations must be safe and never panic.
	lru.Put(makeHash(1), "p")
	if pid, ok := lru.Get(makeHash(1)); ok || pid != "" {
		t.Fatalf("zero-size LRU Get should always return (\"\",false), got (%q,%v)", pid, ok)
	}
	if lru.Has(makeHash(1)) {
		t.Fatalf("zero-size LRU Has should always return false")
	}
}
