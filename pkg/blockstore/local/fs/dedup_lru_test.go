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

func TestDedupLRU_GetMiss_ReturnsFalse(t *testing.T) {
	lru := newDedupLRU(8)
	if lru.Get(makeHash(1), "p") {
		t.Fatalf("empty LRU Get: got true, want false")
	}
}

func TestDedupLRU_PutThenGet_HitsForSamePayload(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x42)
	lru.Put(h, "payload-1")
	if !lru.Get(h, "payload-1") {
		t.Fatalf("after Put: Get(h, payload-1) = false, want true")
	}
}

// #669: compound (hash, payloadID) keying means a cross-payload
// lookup MUST miss. The audit prescription chose this scoping over
// "validate ownership in AddRef" so a stale LRU entry cannot reach
// AddRef and bump RefCount on a row owned by a different payload.
func TestDedupLRU_CrossPayload_Misses(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x42)
	lru.Put(h, "payload-1")
	if lru.Get(h, "payload-2") {
		t.Fatalf("cross-payload Get: got true, want false (compound key scoping)")
	}
	if lru.Has(h, "payload-2") {
		t.Fatalf("cross-payload Has: got true, want false")
	}
}

func TestDedupLRU_Has_ReturnsTrueAfterPut(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x7)
	lru.Put(h, "p")
	if !lru.Has(h, "p") {
		t.Fatalf("Has(known, p) = false, want true")
	}
	if lru.Has(makeHash(0xFF), "p") {
		t.Fatalf("Has(unknown, p) = true, want false")
	}
}

func TestDedupLRU_EvictsLRUWhenOverCapacity(t *testing.T) {
	lru := newDedupLRU(3)
	h1 := makeHash(1)
	h2 := makeHash(2)
	h3 := makeHash(3)
	h4 := makeHash(4)
	lru.Put(h1, "p")
	lru.Put(h2, "p")
	lru.Put(h3, "p")
	lru.Put(h4, "p") // forces eviction of h1 (LRU)

	if lru.Get(h1, "p") {
		t.Fatalf("h1 should have been evicted")
	}
	for i, h := range []blockstore.ContentHash{h2, h3, h4} {
		if !lru.Get(h, "p") {
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
	lru.Put(h1, "p")
	lru.Put(h2, "p")
	lru.Put(h3, "p")
	// Touch h1 — promotes it to MRU.
	if !lru.Get(h1, "p") {
		t.Fatalf("h1 missing before promote")
	}
	lru.Put(h4, "p") // evicts h2 (now the LRU)

	if !lru.Get(h1, "p") {
		t.Fatalf("h1 should still be present after promote")
	}
	if lru.Get(h2, "p") {
		t.Fatalf("h2 should have been evicted after promoting h1")
	}
}

func TestDedupLRU_DuplicatePut_PromotesAndDeduplicates(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x1)
	lru.Put(h, "p1")
	lru.Put(h, "p1")
	if !lru.Get(h, "p1") {
		t.Fatalf("duplicate Put should still hit: Get(h, p1) = false")
	}
	if got := lru.order.Len(); got != 1 {
		t.Fatalf("duplicate Put should keep order.Len()==1, got %d", got)
	}
}

// Distinct payloadIDs for the same hash occupy independent slots
// (compound key scoping); this differs from the pre-#669 hash-only
// scheme where a later Put overwrote the prior payloadID binding.
func TestDedupLRU_SameHash_DistinctPayloads_IndependentSlots(t *testing.T) {
	lru := newDedupLRU(8)
	h := makeHash(0x1)
	lru.Put(h, "p1")
	lru.Put(h, "p2")
	if !lru.Get(h, "p1") {
		t.Fatalf("Get(h, p1) = false after distinct-payload Puts")
	}
	if !lru.Get(h, "p2") {
		t.Fatalf("Get(h, p2) = false after distinct-payload Puts")
	}
	if got := lru.order.Len(); got != 2 {
		t.Fatalf("distinct payload Puts should yield order.Len()==2, got %d", got)
	}
}

func TestDedupLRU_ConcurrentAccess_NoRace(t *testing.T) {
	lru := newDedupLRU(64)
	keys := make([]blockstore.ContentHash, 32)
	for i := range keys {
		keys[i] = makeHash(byte(i))
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(stop) })
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
					_ = lru.Get(k, "p")
					_ = lru.Has(k, "p")
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
	if lru.Get(makeHash(1), "p") {
		t.Fatalf("zero-size LRU Get should always return false")
	}
	if lru.Has(makeHash(1), "p") {
		t.Fatalf("zero-size LRU Has should always return false")
	}
	lru.PutMany([]blockstore.ContentHash{makeHash(2)}, "p")
	if lru.Has(makeHash(2), "p") {
		t.Fatalf("zero-size LRU PutMany should be a no-op")
	}
}

func TestDedupLRU_PutMany_BatchPopulates(t *testing.T) {
	lru := newDedupLRU(8)
	hashes := []blockstore.ContentHash{makeHash(1), makeHash(2), makeHash(3)}
	lru.PutMany(hashes, "payload-batch")
	for i, h := range hashes {
		if !lru.Has(h, "payload-batch") {
			t.Fatalf("hash %d not populated by PutMany", i)
		}
		if lru.Has(h, "other-payload") {
			t.Fatalf("PutMany leaked into cross-payload lookup at hash %d", i)
		}
	}
}
