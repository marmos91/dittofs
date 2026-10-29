//go:build race

package index

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestInspectRace: four writers, each on its own file plus one shared file,
// write, deallocate and settle, their versions drawn from one atomic counter
// inside each update, while two readers loop the per-file read-out and drain
// the bulk one, checking the order of each file's entries and that its held
// extents are disjoint. The race detector catches a read-out that reads a
// file's state without its lock; it cannot see whether a lock is held across
// a yield, which TestInspectAllYieldsHoldingNoLock pins on the non-race
// build, where a held lock would deadlock the timed drain.
func TestInspectRace(t *testing.T) {
	var fs files
	fs.setExtentLimit(1 << 24)
	var counter atomic.Uint64
	files := make([]core.FileID, 5)
	for i := range files {
		files[i] = fid(byte(i + 1))
	}
	shared := files[4]

	var wg sync.WaitGroup
	for w := range 4 {
		own := files[w]
		wg.Go(func() {
			for i := range 300 {
				for _, id := range []core.FileID{own, shared} {
					_ = fs.update(id, func(f *file) error {
						v := ver(counter.Add(1))
						seg := core.SegmentID(counter.Load() % 8)
						off := int64(i%16) * 4096
						if err := f.recorded(seg, ext(off, 4096), nums(v.Counter, v.Counter), 1, id); err != nil {
							return err
						}
						f.write(write(off, 4096, rec(seg, uint32(off)), v.Counter))
						if i%5 == 0 {
							_ = f.deallocate(ext(off, 4096), ver(counter.Add(1)))
						}
						if i%7 == 0 {
							f.settleRemovals(ver(counter.Load()))
						}
						return nil
					})
				}
			}
		})
	}
	wg.Go(func() {
		for range 2000 {
			for _, id := range files {
				checkEntries(fs.inspect(id, nil))
			}
		}
	})
	wg.Go(func() {
		for range 200 {
			for id, es := range fs.inspectAll() {
				_ = id
				checkEntries(es)
			}
		}
	})
	wg.Wait()
}

// checkEntries asserts the read-out invariant a reader can check alone: the
// entries are ordered by offset — with the kind as the tie-break, as the
// read-out's stable sort leaves them — and no two held extents overlap.
func checkEntries(es []Entry) {
	disjointFrom := int64(0)
	lastOff := int64(-1)
	lastKind := EntryKind(0)
	for _, e := range es {
		if e.Extent.Off < lastOff || e.Extent.Off == lastOff && e.Kind < lastKind {
			panic("read-out out of order")
		}
		lastOff, lastKind = e.Extent.Off, e.Kind
		if e.Kind == EntryHeld {
			if e.Extent.Off < disjointFrom {
				panic("held extents overlap")
			}
			disjointFrom = e.Extent.Off + e.Extent.Len
		}
	}
}
