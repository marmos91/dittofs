package segment_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/segment"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// segSize is the size of every segment file the checks create. The pattern
// fills it, so a read of a whole segment compares every byte the cache serves.
const segSize = 4096

// name is the segment file name function, as the format passes it: the id,
// zero-padded, with the segment extension.
func name(id core.SegmentID) string { return fmt.Sprintf("%020d.seg", id) }

// fillPattern writes the pattern of segment id into p: one byte per segment,
// so a read of one segment never reads as another's.
func fillPattern(id core.SegmentID, p []byte) {
	for i := range p {
		p[i] = byte(id)
	}
}

// checkPattern fails t when p is not the pattern of segment id, length aside.
func checkPattern(t *testing.T, id core.SegmentID, p []byte) {
	t.Helper()
	for i, b := range p {
		if b != byte(id) {
			t.Fatalf("byte %d of segment %d is %d, want %d: the read served other bytes than the segment's pattern", i, id, b, byte(id))
			return
		}
	}
}

// cacheEnv is one descriptors cache over a real directory, reached through
// the crash-simulating seam: every check's counts of opens and closes come
// from the seam, so a descriptor that leaked shows in the count.
type cacheEnv struct {
	budget *core.DescriptorBudget
	c      *segment.Descriptors
	seam   *journalv2test.Seam

	// stocked is how many closes the stocking of segment files made, one per
	// file: stock writes each file through its own handle and closes it, and
	// the seam counts that close, so openCount must not count it as a
	// descriptor the cache opened.
	stocked map[core.SegmentID]int
}

// openCount returns the number of descriptors the seam shows open for the
// segment id: its opens less its closes, the stocking's closes aside. A
// leaked descriptor keeps it above zero after Close.
func (e cacheEnv) openCount(id core.SegmentID) int {
	n := name(id)
	return e.seam.CallsFor(journalv2test.OpOpen, n) - (e.seam.CallsFor(journalv2test.OpClose, n) - e.stocked[id])
}

// newCache builds one cache over its own directory on a budget of limit
// slots. The budget is separate per cache; a check that shares one builds the
// second cache on the first's budget itself.
func newCache(t *testing.T, limit int) cacheEnv {
	t.Helper()

	seam, err := journalv2test.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	budget := core.NewDescriptorBudget(limit)
	return cacheEnv{
		budget:  budget,
		c:       segment.NewDescriptors(seam.FS(), name, budget),
		seam:    seam,
		stocked: map[core.SegmentID]int{},
	}
}

// stock fills n segment files, one per id from 1 to n, each with its pattern
// and synced whole, so a read of any of them compares byte for byte.
func (e cacheEnv) stock(t *testing.T, n int) {
	t.Helper()
	if e.stocked == nil {
		e.stocked = map[core.SegmentID]int{}
	}

	for i := 1; i <= n; i++ {
		id := core.SegmentID(i)
		f, err := e.seam.Create(name(id))
		if err != nil {
			t.Fatalf("segment %d could not be created: %v", id, err)
		}
		buf := make([]byte, segSize)
		fillPattern(id, buf)
		if _, err := f.WriteAt(buf, 0); err != nil {
			t.Fatalf("segment %d could not be written: %v", id, err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("segment %d could not be synced: %v", id, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("segment %d could not be closed: %v", id, err)
		}
		e.stocked[id] = 1
	}
}

// read reads segment id whole through the cache. It fails the test when the
// acquire is refused or the read fails, and checks the pattern.
func (e cacheEnv) read(t *testing.T, id core.SegmentID) {
	t.Helper()

	r, err := e.c.Acquire(id)
	if err != nil {
		t.Fatalf("acquiring segment %d failed: %v", id, err)
	}
	buf := make([]byte, segSize)
	if _, err := r.ReadAt(buf, 0); err != nil {
		r.Release()
		t.Fatalf("reading segment %d failed: %v", id, err)
	}
	r.Release()
	checkPattern(t, id, buf)
}

// TestDescriptorsSharedBudgetBindsBothCaches checks the bound is per process,
// not per journal: two caches over two directories draw on one budget, their
// open descriptors together never pass it, and a read of every segment of
// both still serves its pattern. A per-journal bound would let the two hold
// the limit each; a cache that never yielded to the other's reclaim would
// wedge and the reads would fail.
func TestDescriptorsSharedBudgetBindsBothCaches(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seamA, err := journalv2test.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	seamB, err := journalv2test.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	budget := core.NewDescriptorBudget(4)
	a := cacheEnv{budget: budget, c: segment.NewDescriptors(seamA.FS(), name, budget), seam: seamA}
	b := cacheEnv{budget: budget, c: segment.NewDescriptors(seamB.FS(), name, budget), seam: seamB}
	a.stock(t, 16)
	b.stock(t, 16)
	defer a.c.Shutdown()
	defer b.c.Shutdown()

	for round := 0; round < 2; round++ {
		for i := 1; i <= 16; i++ {
			a.read(t, core.SegmentID(i))
			b.read(t, core.SegmentID(i))
			if open := budget.Open(); open > 4 {
				t.Fatalf("after reading segment %d of round %d, the budget holds %d descriptors, want at most 4", i, round+1, open)
			}
		}
	}

	if s1, s2 := a.c.Stats(), b.c.Stats(); s1.Open+s2.Open != budget.Open() {
		t.Fatalf("the caches hold %d and %d descriptors, but the budget holds %d: some descriptor is counted twice or not at all",
			s1.Open, s2.Open, budget.Open())
	}
}

// TestDescriptorsBoundHoldsWithManySegments checks RFC 1 §9.4's descriptor row:
// with ten times more segments than the bound, every read still succeeds and
// the open count stays at or below the bound — whatever order the cache
// evicts in, it cannot keep all forty descriptors open at once.
func TestDescriptorsBoundHoldsWithManySegments(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 40)
	defer e.c.Shutdown()

	for round := 0; round < 3; round++ {
		for i := 1; i <= 40; i++ {
			e.read(t, core.SegmentID(i))
			if s := e.c.Stats(); s.Open > 4 {
				t.Fatalf("after reading segment %d of round %d, the cache holds %d descriptors, want at most 4", i, round+1, s.Open)
			}
			if open := e.budget.Open(); open > 4 {
				t.Fatalf("after reading segment %d of round %d, the budget holds %d descriptors, want at most 4", i, round+1, open)
			}
		}
	}
}

// TestDescriptorsReopenCountedOncePerMiss checks the reopen counter: at a
// bound of one, reading two segments alternately reopens each of them, once
// per miss, and a segment read while its descriptor is still open costs no
// reopen — a hit counted as a reopen would read the metric as thrash where
// there is none.
func TestDescriptorsReopenCountedOncePerMiss(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 1)
	e.stock(t, 2)
	defer e.c.Shutdown()

	for i := 0; i < 2; i++ {
		e.read(t, 1)
		e.read(t, 2)
	}
	if got := e.c.Stats().Reopens; got != 2 {
		t.Fatalf("Reopens = %d after reading two segments alternately at a bound of 1, want 2 (one reopen per miss, none per hit)", got)
	}
	if opens := e.seam.Calls(journalv2test.OpOpen); opens != 4 {
		t.Fatalf("the seam saw %d opens, want 4, the two first opens and the two reopens", opens)
	}

	// The descriptor of 2 is open now: a hit re-reads through it and counts
	// no reopen and no open.
	e.read(t, 2)
	if got := e.c.Stats().Reopens; got != 2 {
		t.Fatalf("Reopens = %d after one more read of segment 2, want it still 2", got)
	}
	if opens := e.seam.Calls(journalv2test.OpOpen); opens != 4 {
		t.Fatalf("the seam saw %d opens after a hit, want it still 4", opens)
	}
}

// TestDescriptorsReferencePreventsEviction checks that a reference a reader
// holds keeps its descriptor open: the reference is taken on a hit, so its
// entry rides the idle list like any idle descriptor and the reclaimer's walk
// reaches it — and skips it, because a reference is held. Reading sixteen
// other segments at a bound of two never closes the held segment's
// descriptor, and the reader reads its pattern through the reference to the
// end. An evict-towards-the-bound that ignored references would close the
// descriptor under the reader and its reads would fail.
func TestDescriptorsReferencePreventsEviction(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 17)
	defer e.c.Shutdown()

	e.read(t, 1) // the open miss: the descriptor enters the idle list on its release
	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 on a hit failed: %v", err)
	}
	defer r.Release()

	for i := 2; i <= 17; i++ {
		e.read(t, core.SegmentID(i))
		if got := e.openCount(1); got != 1 {
			t.Fatalf("segment 1's descriptor count is %d after reading segment %d, want 1 held open by the reference", got, i)
		}
	}

	buf := make([]byte, segSize)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("the held reference's read failed: %v", err)
	}
	checkPattern(t, 1, buf)
}

// TestDescriptorsCloseClosesAndRejects checks Close's contract at rest: the
// segment's descriptor is closed, its budget slot returns, a new Acquire
// fails without opening anything, and a second Close is nil. A descriptor
// that survived Close would keep the unlinked file's storage allocated,
// which is what the close-before-unlink rule (R39) exists to prevent.
func TestDescriptorsCloseClosesAndRejects(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)
	defer e.c.Shutdown()

	e.read(t, 1)
	if got := e.openCount(1); got != 1 {
		t.Fatalf("segment 1's descriptor count is %d after a read, want 1", got)
	}
	open := e.budget.Open()

	if err := e.c.Close(1); err != nil {
		t.Fatalf("Close(1) = %v, want nil", err)
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after Close, want 0: a leaked descriptor keeps an unlinked file allocated", got)
	}
	if _, err := e.c.Acquire(1); !errors.Is(err, segment.ErrSegmentGone) {
		t.Fatalf("Acquire after Close = %v, want ErrSegmentGone", err)
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after a refused acquire, want no open attempted", got)
	}
	if err := e.c.Close(1); err != nil {
		t.Fatalf("a second Close(1) = %v, want nil", err)
	}

	if s := e.c.Stats(); s.Open != 0 {
		t.Fatalf("Stats().Open = %d after Close, want 0", s.Open)
	}
	if got := e.budget.Open(); got != open-1 {
		t.Fatalf("the budget holds %d descriptors after Close, want %d", got, open-1)
	}
}

// TestDescriptorsCloseWaitsOnlyForItsOwn checks that Close(id) waits for the
// references its own segment holds and no others: it blocks while a reader
// of that segment holds its reference — and that reader still reads intact
// bytes — while a Close of another segment with a reader under way does not
// wait on it. A store-wide drain would make every retirement wait on every
// reader, and a Close that did not wait would free storage under the
// reader's feet (R65).
func TestDescriptorsCloseWaitsOnlyForItsOwn(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)
	defer e.c.Shutdown()

	rA, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- e.c.Close(1) }()

	// Close(1) has started, so its rejects are visible, while the reference
	// outstanding keeps it from returning. A probe acquire that succeeds
	// releases at once, so the probes hold no reference of their own.
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := e.c.Acquire(1)
		if errors.Is(err, segment.ErrSegmentGone) {
			break
		}
		if err != nil {
			t.Fatalf("a probe acquire of segment 1 failed with %v, want ErrSegmentGone", err)
		}
		r.Release()
		if time.Now().After(deadline) {
			t.Fatal("acquires of segment 1 never failed, so Close(1) never started")
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case err := <-closed:
		t.Fatalf("Close(1) returned %v while a reference was still held, want it to wait", err)
	default:
	}

	// The reader reads intact bytes while the drain waits for it…
	buf := make([]byte, segSize)
	if _, err := rA.ReadAt(buf, 0); err != nil {
		t.Fatalf("the held reference's read failed under the drain: %v", err)
	}
	checkPattern(t, 1, buf)

	// …and a reader of segment 2 holds its own reference, which does not
	// extend this drain: another segment's reader must not keep a segment's
	// Close waiting.
	rB, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("acquiring segment 2 failed: %v", err)
	}

	rA.Release()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close(1) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close(1) did not return after the last reference dropped")
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after Close returned, want 0", got)
	}

	// The hold on segment 2 outlived the drain of segment 1, so Close does not
	// wait on another segment's readers — but it must not outlive its own
	// release either, and the release must not misaccount under a finished
	// drain.
	rB.Release()

	// A second Close of an already-closed segment is the idempotent nil, and
	// it returns at once even while a reader of another segment still holds
	// its reference.
	rHeld, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("acquiring segment 2 after segment 1's Close = %v, want a reference: Close(1) did not close it", err)
	}
	done := make(chan error, 1)
	go func() { done <- e.c.Close(1) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a second Close(1) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second Close(1) did not return: it waits on a reader of another segment")
	}
	rHeld.Release()
}

// TestDescriptorsContinuousReadersDoNotStarveClose checks RFC 1 §8.6's
// progress rule as it binds the drain: eight readers looping on one segment,
// a new acquire the moment the last one releases, cannot keep Close from
// completing, because once Close starts the new acquires fail and the drain
// ends — no read that began after it can extend it (R67).
func TestDescriptorsContinuousReadersDoNotStarveClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)
	defer e.c.Shutdown()

	const readers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r, err := e.c.Acquire(1)
				if err != nil {
					return
				}
				buf := make([]byte, segSize)
				if _, err := r.ReadAt(buf, 0); err != nil {
					r.Release()
					return
				}
				r.Release()
				checkPattern(t, 1, buf)
			}
		}()
	}

	time.Sleep(5 * time.Millisecond) // let the readers start
	done := make(chan error, 1)
	go func() { done <- e.c.Close(1) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close under continuous readers = %v, want it to return nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return: a continuous stream of readers starved the drain")
	}
	close(stop)
	wg.Wait()
}

// heldFault is a fault that records whether the cache's mutex was held when
// the seam consulted it: a storage call made under the mutex is a leaf lock
// held across I/O, which the cache's contract forbids (R69).
type heldFault struct {
	c  *segment.Descriptors
	ok bool
}

func (f *heldFault) At(op string, key []byte) (time.Duration, int, error) {
	if segment.MutexHeld(f.c) {
		f.ok = false
	}
	return 0, 0, nil
}

// eioFault fails the first n calls of op on the file named key with EIO; every
// other call passes. It counts its calls atomically, so concurrent calls are
// numbered one of them's.
type eioFault struct {
	op  string
	key string
	n   int64 // how many matching calls remain to fail

	calls atomic.Int64 // matching calls seen; a call fails while calls < n
}

func (f *eioFault) At(op string, key []byte) (time.Duration, int, error) {
	if op == f.op && string(key) == f.key && f.calls.Add(1) <= f.n {
		return 0, 0, syscall.EIO
	}
	return 0, 0, nil
}

// TestDescriptorsNoSeamCallUnderTheCacheMutex checks the leaf-lock contract
// (R69): no storage call — open, read, write, sync or close — runs while the
// cache's mutex is held, so a fault consulted at every seam call must never
// find the mutex taken. A lock held across I/O would make every reader of
// every segment wait on the slowest operation in flight.
func TestDescriptorsNoSeamCallUnderTheCacheMutex(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 4)
	defer e.c.Shutdown()

	f := &heldFault{c: e.c, ok: true}
	e.seam.SetFault(f)

	// A miss and its open, a hit, and an eviction: reading three segments at
	// a bound of two closes one.
	e.read(t, 1)
	e.read(t, 2)
	r, err := e.c.Acquire(3)
	if err != nil {
		t.Fatalf("acquiring segment 3 failed: %v", err)
	}
	// A reopen: 1 was evicted when 3 was opened.
	e.read(t, 1)

	// A write and a sync through the reference.
	buf := make([]byte, 16)
	fillPattern(3, buf)
	if _, err := r.WriteAt(buf, 0); err != nil {
		t.Fatalf("writing through the reference failed: %v", err)
	}
	if err := r.Sync(); err != nil {
		t.Fatalf("syncing through the reference failed: %v", err)
	}
	r.Release()

	// A Close of a segment with its descriptor open, and a Shutdown of the
	// cache with one open.
	e.read(t, 4)
	if err := e.c.Close(4); err != nil {
		t.Fatalf("Close(4) = %v, want nil", err)
	}
	if err := e.c.Shutdown(); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}

	if !f.ok {
		t.Fatal("a seam call was made while the cache's mutex was held: a storage call under it is a lock held across I/O")
	}
}

// TestDescriptorsDirtyDescriptorPinned checks that a descriptor with a write
// issued through it stays open until a sync through it has covered the write
// or failed: reclaiming it would drop writeback errors the platform reports
// only through the open handle. The pinned descriptor is counted by the bound,
// and a failed sync unpins it, because its writes already failed.
func TestDescriptorsDirtyDescriptorPinned(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 20)
	defer e.c.Shutdown()

	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := r.WriteAt(buf, 0); err != nil {
		t.Fatalf("writing through segment 1's descriptor failed: %v", err)
	}
	r.Release()

	// Reading nineteen more segments under a bound of four must not close
	// segment 1's dirty descriptor.
	for i := 2; i <= 20; i++ {
		e.read(t, core.SegmentID(i))
		if got := e.openCount(1); got != 1 {
			t.Fatalf("segment 1's descriptor count is %d after reading segment %d, want its dirty descriptor held open", got, i)
		}
	}

	// A sync through its descriptor covers the write, so the descriptor
	// becomes evictable and is closed to make room.
	r, err = e.c.Acquire(1)
	if err != nil {
		t.Fatalf("reacquiring segment 1 failed: %v", err)
	}
	if err := r.Sync(); err != nil {
		t.Fatalf("syncing segment 1 failed: %v", err)
	}
	r.Release()
	closeCalls := e.seam.CallsFor(journalv2test.OpClose, name(1)) - e.stocked[1]
	if closeCalls != 0 {
		t.Fatalf("segment 1's descriptor was closed %d times before its sync, want it held", closeCalls)
	}
	for i := 2; i <= 20; i++ {
		e.read(t, core.SegmentID(i))
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after a sync and more reads, want it evicted", got)
	}
}

// TestDescriptorsFailedSyncUnpins checks the pin's other side: a sync that
// fails un-pins the descriptor, because its writes already failed and the
// caller saw it — the bound is not to hold a descriptor open for writes nobody
// will retry through it.
func TestDescriptorsFailedSyncUnpins(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 20)
	defer e.c.Shutdown()

	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := r.WriteAt(buf, 0); err != nil {
		t.Fatalf("writing through segment 1's descriptor failed: %v", err)
	}
	if err := r.Sync(); err != nil {
		t.Fatalf("syncing segment 1 failed: %v", err)
	}
	r.Release()

	// Dirty again, and this time the sync fails.
	r, err = e.c.Acquire(1)
	if err != nil {
		t.Fatalf("reacquiring segment 1 failed: %v", err)
	}
	if _, err := r.WriteAt(buf, 0); err != nil {
		t.Fatalf("writing through segment 1's descriptor failed: %v", err)
	}
	e.seam.SetFault(&eioFault{op: journalv2test.OpSync, key: name(1), n: 1})
	if err := r.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("syncing segment 1 under the fault = %v, want EIO", err)
	}
	e.seam.SetFault(nil)
	r.Release()

	// The failed sync covered the writes it started at, so the descriptor is
	// evictable: reads past the bound close it to make room — its pin was
	// the unsynced write, and the sync's failure is the write's failure too.
	for i := 2; i <= 20; i++ {
		e.read(t, core.SegmentID(i))
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after a failed sync and nineteen reads, want the descriptor evicted: a failed sync must unpin it", got)
	}
}

// TestDescriptorsRefusalMakesNoIO checks Acquire's refusal: when every slot the
// budget allows is pinned by a reference or an unsynced write, the acquire
// fails with ErrNoDescriptor at once, opens nothing and waits for nothing —
// the refusal is the misconfiguration surfacing, not a queue to wait in.
func TestDescriptorsRefusalMakesNoIO(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 3)
	defer e.c.Shutdown()

	r1, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	defer r1.Release()
	r2, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("acquiring segment 2 failed: %v", err)
	}
	defer r2.Release()
	opens := e.seam.Calls(journalv2test.OpOpen)

	start := time.Now()
	if _, err := e.c.Acquire(3); !errors.Is(err, segment.ErrNoDescriptor) {
		t.Fatalf("acquiring segment 3 with both slots pinned = %v, want ErrNoDescriptor", err)
	}
	if past := time.Since(start); past > time.Second {
		t.Fatalf("the refusal took %v, want it at once: the bound is not a queue to wait in", past)
	}
	if got := e.seam.Calls(journalv2test.OpOpen); got != opens {
		t.Fatalf("the seam saw %d opens after the refusal, want still %d: the refusal performs no I/O", got, opens)
	}

	// One reference returned: one slot is open, so the acquire succeeds.
	r2.Release()
	if _, err := e.c.Acquire(3); err != nil {
		t.Fatalf("acquiring segment 3 after a release = %v, want it to succeed", err)
	}
}

// TestDescriptorsFailedOpenRetries checks that a failed open leaves the segment
// retryable: the fault was the platform's, not the segment's, so the next
// acquire opens again. A missing file, though, is the segment's own state:
// it answers ErrSegmentGone.
func TestDescriptorsFailedOpenRetries(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)
	defer e.c.Shutdown()

	e.seam.SetFault(&eioFault{op: journalv2test.OpOpen, key: name(1), n: 1})

	if _, err := e.c.Acquire(1); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the first acquire of segment 1 = %v, want the fault's EIO", err)
	}
	if got := e.budget.Open(); got != 0 {
		t.Fatalf("the budget holds %d descriptors after a failed open, want 0: the refused slot returned", got)
	}

	e.seam.SetFault(nil)
	e.read(t, 1) // retries the open and succeeds

	// A file that is not there is not retried: the segment is gone.
	if _, err := e.c.Acquire(7); !errors.Is(err, segment.ErrSegmentGone) {
		t.Fatalf("acquiring a segment with no file = %v, want ErrSegmentGone", err)
	}
}

// TestDescriptorsForget checks Forget: after Close returned and the file is
// unlinked, Forget drops the cache's entry, so the entry map stays bounded by
// the live segments; the segment still answers ErrSegmentGone, whose
// tombstone Forget removed along with the entry.
func TestDescriptorsForget(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)
	defer e.c.Shutdown()

	e.read(t, 1)
	if err := e.c.Close(1); err != nil {
		t.Fatalf("Close(1) = %v, want nil", err)
	}
	if err := e.seam.Remove(name(1)); err != nil {
		t.Fatalf("removing segment 1's file failed: %v", err)
	}
	e.c.Forget(1)

	if _, err := e.c.Acquire(1); !errors.Is(err, segment.ErrSegmentGone) {
		t.Fatalf("acquiring a forgotten segment = %v, want ErrSegmentGone", err)
	}
	if s := e.c.Stats(); s.Open != 0 {
		t.Fatalf("Stats().Open = %d after a forget of the only open segment, want 0", s.Open)
	}
}

// TestDescriptorsShutdownWaitsAndUnregisters checks Shutdown: it waits for the
// references outstanding, refuses new acquires after, returns every slot the
// cache held to the budget, and takes the cache out of the budget's reclaim
// walk, so another cache on the same budget cannot be asked to evict for a
// cache that is gone.
func TestDescriptorsShutdownWaitsAndUnregisters(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)
	budgetBefore := e.budget.Open()

	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- e.c.Shutdown() }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a reference was still held, want it to wait")
	case <-time.After(50 * time.Millisecond):
	}

	// The reader still reads through the reference the shutdown waits for.
	buf := make([]byte, segSize)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("the held reference's read failed under Shutdown: %v", err)
	}
	checkPattern(t, 1, buf)
	r.Release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the last reference dropped")
	}

	if _, err := e.c.Acquire(2); !errors.Is(err, segment.ErrCacheClosed) {
		t.Fatalf("acquiring after Shutdown = %v, want ErrCacheClosed", err)
	}
	if err := e.c.Shutdown(); err != nil {
		t.Fatalf("a second Shutdown = %v, want nil", err)
	}
	if got := e.budget.Open(); got != budgetBefore {
		t.Fatalf("the budget holds %d descriptors after Shutdown, want %d: every slot the cache held is back", got, budgetBefore)
	}

	// The cache is out of the reclaim walk: a second cache on the same budget
	// can use every slot of it, with nothing evicted for the gone one.
	seam2, err := journalv2test.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the second seam could not be made: %v", err)
	}
	e2 := cacheEnv{
		budget:  e.budget,
		c:       segment.NewDescriptors(seam2.FS(), name, e.budget),
		seam:    seam2,
		stocked: map[core.SegmentID]int{},
	}
	e2.stock(t, 8)
	defer e2.c.Shutdown()
	for i := 1; i <= 8; i++ {
		e2.read(t, core.SegmentID(i))
	}
	if s2 := e2.c.Stats(); s2.Open > 4 {
		t.Fatalf("the second cache holds %d descriptors, want at most 4: the shut-down cache still counts against the bound or evicts for it", s2.Open)
	}
}

// TestDescriptorsRaceSoak checks the same contract under the race detector
// with contention: readers over eight segments under a bound of four, and a
// Close of a segment under way. Every successful read serves its segment's
// pattern, every reader of the closed segment ends on ErrSegmentGone, and the
// detector stays silent.
func TestDescriptorsRaceSoak(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 8)
	defer e.c.Shutdown()

	const readers = 32
	const closed = core.SegmentID(5) // the segment whose Close runs midway

	// Readers of the segment being closed stop reading when their acquires
	// answer ErrSegmentGone; the rest keep reading until the drain is
	// observed and the test stops them.
	stopped := make(chan struct{})
	var goneWg, restWg sync.WaitGroup
	for i := 0; i < readers; i++ {
		id := core.SegmentID(1 + i%8)
		if id == closed {
			goneWg.Add(1)
			go func(id core.SegmentID) {
				defer goneWg.Done()
				buf := make([]byte, segSize)
				for {
					r, err := e.c.Acquire(id)
					if errors.Is(err, segment.ErrSegmentGone) {
						return
					}
					if err != nil {
						continue // ErrNoDescriptor under contention: try again
					}
					if _, err := r.ReadAt(buf, 0); err != nil {
						t.Errorf("reading segment %d failed: %v", id, err)
						r.Release()
						return
					}
					if buf[0] != byte(id) {
						t.Errorf("segment %d read as %d, want its own pattern", id, buf[0])
						r.Release()
						return
					}
					r.Release()
				}
			}(id)
			continue
		}
		restWg.Add(1)
		go func(id core.SegmentID) {
			defer restWg.Done()
			buf := make([]byte, segSize)
			for {
				select {
				case <-stopped:
					return
				default:
				}
				r, err := e.c.Acquire(id)
				if errors.Is(err, segment.ErrSegmentGone) {
					t.Errorf("acquiring segment %d answered gone, but only segment %d was closed", id, closed)
					return
				}
				if err != nil {
					continue
				}
				if _, err := r.ReadAt(buf, 0); err != nil {
					t.Errorf("reading segment %d failed: %v", id, err)
					r.Release()
					return
				}
				if buf[0] != byte(id) {
					t.Errorf("segment %d read as %d, want its own pattern", id, buf[0])
					r.Release()
					return
				}
				r.Release()
			}
		}(id)
	}

	// Let the readers reach their segments, then close one under them. The
	// drain must complete even with its own readers looping, and the rest
	// must never see their acquires refused.
	time.Sleep(2 * time.Millisecond)
	if err := e.c.Close(closed); err != nil {
		t.Errorf("Close(%d) under the soak = %v, want nil", closed, err)
	}
	close(stopped)
	restWg.Wait()

	// The readers of the closed segment end on ErrSegmentGone, and promptly:
	// the drain is what stops them.
	done := make(chan struct{})
	go func() { goneWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the readers of the closed segment did not end on ErrSegmentGone: acquires of a closed segment still succeed")
	}
}

// sinkErr carries a hit-path error out of the allocation-counted closure, so
// the checked path itself allocates nothing to report one.
var sinkErr error

// TestDescriptorsHitAllocatesNothing checks the read path's cost: on a hit —
// the read a busy journal makes most — Acquire, ReadAt and Release together
// allocate nothing, with the buffer preallocated. An allocation per hit would
// put every read through the allocator and keep the descriptor's place in the
// idle list out of the cache's hands. The race detector instruments the locks
// and atomics, so the count is taken only without it.
func TestDescriptorsHitAllocatesNothing(t *testing.T) {
	if descRace {
		t.Skip("the race detector instruments the locks and atomics; the allocation count is taken without it")
	}
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 2)
	defer e.c.Shutdown()

	// Warm: the descriptor is open and its entry rides the idle list.
	e.read(t, 1)

	buf := make([]byte, segSize)
	allocs := testing.AllocsPerRun(200, func() {
		r, err := e.c.Acquire(1)
		if err != nil {
			sinkErr = err
			return
		}
		if _, err := r.ReadAt(buf, 0); err != nil {
			sinkErr = err
			r.Release()
			return
		}
		r.Release()
	})
	if sinkErr != nil {
		t.Fatalf("the hit path failed under the allocation count: %v", sinkErr)
	}
	if allocs != 0 {
		t.Fatalf("a warm hit allocated %v per round, want 0: the read path must not run through the allocator", allocs)
	}
}
