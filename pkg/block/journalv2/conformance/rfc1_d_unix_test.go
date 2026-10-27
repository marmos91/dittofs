//go:build journal_conformance && unix

package conformance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// The checks here compare the journal's accounting with what the filesystem
// allocates, which is read through stat on unix only.

// TestRFC1D_AccountingFollowsCap checks that the journal's accounting follows
// the segment cap, not what the filesystem happens to allocate around a
// growing file: through many small appends each unsealed segment is accounted
// at its cap and each sealed one at its size after the seal, UsedBytes covers
// them all, and the filesystem never allocates more than UsedBytes. A segment
// sealed early, when idle, returns its free tail and is accounted at its size
// after the seal.
//
// decision: UsedBytes is held to the listed segments' sum plus a whole number
// of segment caps, not to the sum alone. It also counts the spares, each
// allocated at its cap, and whether Segments lists a spare is not pinned. Pin
// that and this becomes an equality.
//
// The leg on a filesystem that allocates speculatively past the end of a
// growing file needs such a host and does not run here.
func TestRFC1D_AccountingFollowsCap(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)

	// check compares every segment's accounting with its file, and UsedBytes
	// and the filesystem's allocation with the sum, and returns the segments
	// sealed short of their cap.
	check := func(step string) (early int) {
		t.Helper()

		var sum int64
		for _, s := range f.J.Inspect().Segments() {
			acc := int64(journalv2test.FixtureSegmentCap)
			if s.Sealed {
				fi, err := os.Stat(filepath.Join(f.Dir, segmentName(s.ID)))
				if err != nil {
					t.Fatalf("after %s: %v", step, err)
				}
				acc = fi.Size()
				if acc < journalv2test.FixtureSegmentCap {
					early++
				}
			}
			if s.Allocated != acc {
				t.Fatalf("after %s: segment %d (sealed %t) is accounted at %d, want %d", step, s.ID, s.Sealed, s.Allocated, acc)
			}
			sum += acc
		}

		used := f.J.Stats().UsedBytes
		if extra := used - sum; extra < 0 || extra%journalv2test.FixtureSegmentCap != 0 {
			t.Fatalf("after %s: UsedBytes = %d with its listed segments accounted at %d: want them plus whole segment caps for its spares", step, used, sum)
		}
		if alloc := f.Allocated(); alloc > used {
			t.Fatalf("after %s: the filesystem allocates %d, more than UsedBytes %d", step, alloc, used)
		}
		return early
	}

	const n = 3 * journalv2test.FixtureSegmentCap / 4096
	for i := range n {
		write(t, h, journalv2test.File(i%2), int64(i/2)*4096, make([]byte, 4096))
		check(fmt.Sprintf("write %d", i))
	}

	// An idle segment past the idle-seal threshold is sealed early, on the
	// journal's own goroutine.
	write(t, h, journalv2test.File(2), 0, make([]byte, 2*journalv2test.FixtureIdleSealBytes))
	f.Advance(journalv2test.IdleSealAge + time.Second)

	// decision: the seal runs on the journal's goroutine once the clock
	// passes the idle age, so only a real-time wait sees it land.
	for deadline := time.Now().Add(2 * time.Second); check("an idle seal") == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no segment was sealed short of its cap after the idle-seal age")
		}
	}
}

// TestRFC1D_NamedInequalitiesHold checks that every Stats sample keeps the
// inequalities Stats guarantees while writes, releases, truncates and repack
// run concurrently.
func TestRFC1D_NamedInequalitiesHold(t *testing.T) {
	f := journalv2test.New(t)
	ctx := t.Context()

	var errs []error
	var mu sync.Mutex
	report := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}

	for round := range 4 {
		done := make(chan struct{})
		var workers, poller sync.WaitGroup

		// The poller stops at its first violation: one is enough, and a tight
		// loop reporting every sample would fill memory.
		poller.Go(func() {
			for {
				s := f.J.Stats()
				if s.DirtyBytes > s.HeldBytes {
					report(fmt.Errorf("round %d: DirtyBytes %d > HeldBytes %d", round, s.DirtyBytes, s.HeldBytes))
					return
				}
				if s.ReservedBytes < 0 {
					report(fmt.Errorf("round %d: ReservedBytes %d < 0", round, s.ReservedBytes))
					return
				}
				select {
				case <-done:
					return
				default:
				}
			}
		})

		for k := range 2 {
			h, id := f.Share(k), journalv2test.File(10*round+k)
			workers.Go(func() {
				for i := range 32 {
					off := int64(i) * 16 << 10
					v, err := tryWrite(t, h, id, off, pattern(fmt.Sprint(round, k, i), 16<<10))
					if err != nil {
						report(err)
						return
					}
					if i%2 == 0 {
						continue
					}
					e := []journalv2.Extent{{Off: off, Len: 16 << 10}}
					if err := h.MarkOffloaded(ctx, id, e, v, v); err != nil {
						report(err)
						return
					}
					if err := h.Release(ctx, id, e); err != nil {
						report(err)
						return
					}
				}
				if _, err := h.Truncate(ctx, id, 64<<10); err != nil {
					report(err)
				}
			})
		}
		workers.Go(func() {
			if _, err := f.J.Repack(ctx, 4*journalv2test.FixtureSegmentCap, journalv2.RepackScope{}); err != nil {
				report(err)
			}
		})

		workers.Wait()
		close(done)
		poller.Wait()

		// decision: Stats and the directory cannot be read at one instant, and
		// the journal makes spares and retires segments in the background even
		// with the test's own work joined. Allocation raises UsedBytes before
		// it happens and an unlink lowers it only after, so UsedBytes read
		// between two reads of the directory is never below the smaller of
		// them, whatever happened in between.
		a1 := f.Allocated()
		s := f.J.Stats()
		a2 := f.Allocated()
		if alloc := min(a1, a2); s.UsedBytes < alloc {
			report(fmt.Errorf("round %d: UsedBytes %d < %d allocated on disk", round, s.UsedBytes, alloc))
		}
	}

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
}
