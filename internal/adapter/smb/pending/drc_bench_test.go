package pending

import (
	"encoding/binary"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Both caches sit on the per-request path behind one mutex, so the numbers that
// matter are the contended ones — a serial benchmark measures the map, not the
// lock. The NFS side's equivalent is BenchmarkDRC_Contended.

// BenchmarkCreateDRC_OpenCloseCycle is the shape a live server actually makes:
// a durable-handle CREATE records a response, a replay may look it up, and the
// clean close forgets it. The cache therefore tracks live opens rather than
// growing without bound, which is the case worth being fast.
func BenchmarkCreateDRC_OpenCloseCycle(b *testing.B) {
	c := NewCreateDRC[int, int]()
	var ctr atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var g [16]byte
			binary.LittleEndian.PutUint64(g[:], ctr.Add(1))
			c.Record(1, g, 0, 0)
			c.Lookup(1, g)
			c.Forget(g)
		}
	})
}

// BenchmarkCreateDRC_Contended never forgets, so the cache saturates at the cap
// and stays there. This is the pathological end — every open held open — kept
// alongside the cycle benchmark so the two costs stay distinguishable.
func BenchmarkCreateDRC_Contended(b *testing.B) {
	c := NewCreateDRC[int, int]()
	var ctr atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var g [16]byte
			binary.LittleEndian.PutUint64(g[:], ctr.Add(1))
			c.Record(1, g, 0, 0)
			c.Lookup(1, g)
		}
	})
}

// BenchmarkCreateDRC_RecordAtCap isolates the prune. Record calls pruneLocked
// every time, which walks every entry to drop expired ones and then, once the
// cap is reached, walks them again to find the oldest. That cost scales with
// occupancy rather than with request rate, so it is measured at a full cache
// rather than folded into the contended number above.
func BenchmarkCreateDRC_RecordAtCap(b *testing.B) {
	c := NewCreateDRC[int, int]()
	// Keys start at 1: Record drops the all-zero guid, so a loop from 0 fills
	// one entry short of the cap and the first timed call takes the under-cap
	// fast path instead of pruning.
	for i := 1; i <= maxCreateDRCEntries; i++ {
		var g [16]byte
		binary.LittleEndian.PutUint64(g[:], uint64(i))
		c.Record(1, g, 0, 0)
	}
	if got := len(c.entries); got != maxCreateDRCEntries {
		b.Fatalf("setup filled %d entries, want the %d-entry cap", got, maxCreateDRCEntries)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var g [16]byte
		binary.LittleEndian.PutUint64(g[:], uint64(maxCreateDRCEntries+1+i))
		c.Record(1, g, 0, 0)
	}
}

// BenchmarkCreateDRC_ReserveRelease covers the reservation half, which the
// CREATE path takes on every durable-handle open and which uses a second map
// under the same mutex.
func BenchmarkCreateDRC_ReserveRelease(b *testing.B) {
	c := NewCreateDRC[int, int]()
	var ctr atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var g [16]byte
			binary.LittleEndian.PutUint64(g[:], ctr.Add(1))
			c.Reserve(1, g)
			c.IsReserved(1, g)
			c.Release(1, g)
		}
	})
}

// BenchmarkLockDRC_Contended exercises the LOCK cache, which is read-mostly:
// Lookup takes RLock, Record takes the write lock. Indices stay inside
// [1, LockSequenceIndexMax] because anything outside is untracked and would
// measure the rejection path instead.
func BenchmarkLockDRC_Contended(b *testing.B) {
	c := NewLockDRC()
	var ctr atomic.Uint64
	fileID := [16]byte{1}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := ctr.Add(1)
			idx := uint32(n%uint64(LockSequenceIndexMax)) + 1
			c.Record(fileID, idx, uint8(n), types.StatusSuccess)
			c.Lookup(fileID, idx, uint8(n))
		}
	})
}
