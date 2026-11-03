package segment_test

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
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
	seam   *journaltest.Seam

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
	return e.seam.CallsFor(journaltest.OpOpen, n) - (e.seam.CallsFor(journaltest.OpClose, n) - e.stocked[id])
}

// newCache builds one cache over its own directory on a budget of limit
// slots. The budget is separate per cache; a check that shares one builds the
// second cache on the first's budget itself.
func newCache(t *testing.T, limit int) cacheEnv {
	t.Helper()

	seam, err := journaltest.NewSeam(t.TempDir())
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

// stocked fills segment files for one extra id, the helper the evictable
// check uses when its cache was stocked before it: creating a file through
// the seam updates the stocking's close count the same way stock does.
func stocked(t *testing.T, e cacheEnv, id core.SegmentID) {
	t.Helper()
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

// TestDescriptorsSharedBudgetBindsBothCaches checks the bound is per process,
// not per journal: two caches over two directories draw on one budget, their
// open descriptors together never pass it, and a read of every segment of
// both still serves its pattern. The reads interleave the two caches, so each
// usually holds an idle descriptor of its own to yield —
// TestDescriptorsAIdleJournalYieldsToAnother is the check that a journal
// holding every slot idle still yields to another journal's read.
func TestDescriptorsSharedBudgetBindsBothCaches(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seamA, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	seamB, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	budget := core.NewDescriptorBudget(4)
	a := cacheEnv{budget: budget, c: segment.NewDescriptors(seamA.FS(), name, budget), seam: seamA}
	b := cacheEnv{budget: budget, c: segment.NewDescriptors(seamB.FS(), name, budget), seam: seamB}
	a.stock(t, 16)
	b.stock(t, 16)
	shutRegister(t, a.c)
	shutRegister(t, b.c)

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
	shutRegister(t, e.c)

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
	shutRegister(t, e.c)

	for i := 0; i < 2; i++ {
		e.read(t, 1)
		e.read(t, 2)
	}
	if got := e.c.Stats().Reopens; got != 2 {
		t.Fatalf("Reopens = %d after reading two segments alternately at a bound of 1, want 2 (one reopen per miss, none per hit)", got)
	}
	if opens := e.seam.Calls(journaltest.OpOpen); opens != 4 {
		t.Fatalf("the seam saw %d opens, want 4, the two first opens and the two reopens", opens)
	}

	// The descriptor of 2 is open now: a hit re-reads through it and counts
	// no reopen and no open.
	e.read(t, 2)
	if got := e.c.Stats().Reopens; got != 2 {
		t.Fatalf("Reopens = %d after one more read of segment 2, want it still 2", got)
	}
	if opens := e.seam.Calls(journaltest.OpOpen); opens != 4 {
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
	shutRegister(t, e.c)

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
	shutRegister(t, e.c)

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
	// Cleanups run last-registered first: the Shutdown registered here runs
	// after the reference releases registered below, so a failure on any
	// assertion unwinds the held references first and the failure reports
	// itself — a deferred Shutdown would instead wedge on a reference the
	// failed test never dropped, and the run would hang to the timeout.
	shutRegister(t, e.c)
	rA, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	dropA := relRegister(t, rA)

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
	dropB := relRegister(t, rB)

	dropA()

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
	dropB()

	// A second Close of an already-closed segment is the idempotent nil, and
	// it returns at once even while a reader of another segment still holds
	// its reference.
	rHeld, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("acquiring segment 2 after segment 1's Close = %v, want a reference: Close(1) did not close it", err)
	}
	dropHeld := relRegister(t, rHeld)
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
	dropHeld()
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
	shutRegister(t, e.c)

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

// oneRelease releases a reference exactly once: whichever of the test body
// and the cleanup reaches it first wins, and the other's call is the no-op
// Release's exactly-once contract needs it to be.
type oneRelease struct {
	once sync.Once
	r    segment.Ref
}

func (x *oneRelease) drop() { x.once.Do(x.r.Release) }

// relRegister registers a t.Cleanup that drops a reference exactly once,
// whichever of the test body or the cleanup reaches the drop first. Register
// it after the Shutdown cleanup so the drop runs first: a failure on any
// assertion unwinds the reference before Shutdown waits on it, and the
// failure reports itself instead of wedging to the run's timeout.
func relRegister(t *testing.T, r segment.Ref) (drop func()) {
	x := &oneRelease{r: r}
	t.Cleanup(x.drop)
	return x.drop
}

// shutRegister registers the cache's Shutdown as the test's last cleanup,
// checking what the Shutdown returns: it is the joined errors of the files
// it closed, and a test whose cache cannot shut down cleanly must fail on
// that, not pass on an error its cleanup swallowed. Register it once the
// cache is built — before any hold or reference registers its own cleanup,
// so theirs run first: a failure on an assertion unwinds what keeps the
// Shutdown waiting before it runs.
func shutRegister(t *testing.T, c *segment.Descriptors) {
	t.Cleanup(func() {
		if err := c.Shutdown(); err != nil {
			t.Errorf("Shutdown = %v, want nil", err)
		}
	})
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
// find the mutex taken. A lock held across I/O would make every caller of the
// cache wait on the slowest operation in flight.
func TestDescriptorsNoSeamCallUnderTheCacheMutex(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 4)
	shutRegister(t, e.c)

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

	// The write path and the sync through it, so the fault consults them
	// too: a write or a sync made under the mutex would be the same lock
	// held across I/O as an open or a read.
	if _, err := r.WriteAt(make([]byte, 16), 0); err != nil {
		t.Fatalf("writing through segment 3's descriptor failed: %v", err)
	}
	if err := r.Sync(); err != nil {
		t.Fatalf("syncing segment 3's descriptor failed: %v", err)
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

// TestDescriptorsRefusalMakesNoIO checks Acquire's refusal: when every slot
// the budget allows is held by a reference, the acquire fails with
// ErrNoDescriptor at once, opens nothing and waits for nothing —
// the refusal is the misconfiguration surfacing, not a queue to wait in.
func TestDescriptorsRefusalMakesNoIO(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 3)
	shutRegister(t, e.c)

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
	opens := e.seam.Calls(journaltest.OpOpen)

	start := time.Now()
	if _, err := e.c.Acquire(3); !errors.Is(err, segment.ErrNoDescriptor) {
		t.Fatalf("acquiring segment 3 with both slots pinned = %v, want ErrNoDescriptor", err)
	}
	if past := time.Since(start); past > time.Second {
		t.Fatalf("the refusal took %v, want it at once: the bound is not a queue to wait in", past)
	}
	if got := e.seam.Calls(journaltest.OpOpen); got != opens {
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
	shutRegister(t, e.c)

	e.seam.SetFault(&eioFault{op: journaltest.OpOpen, key: name(1), n: 1})

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
	shutRegister(t, e.c)

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
	shutRegister(t, e.c)

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
	if prof.RaceEnabled {
		t.Skip("the race detector instruments the locks and atomics; the allocation count is taken without it")
	}
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 2)
	shutRegister(t, e.c)

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

// TestDescriptorsCloseRacingAnOpen checks the opener and the waiter of an
// in-flight open against a Close that starts while the open runs: both acquires
// answer ErrSegmentGone, the file the open fetched is closed rather than
// cached, and its slot returns. A Close that ignored its own gone mark would
// hand the opener a reference to a segment whose Close had already started, so
// the caller would free storage under a reader.
func TestDescriptorsCloseRacingAnOpen(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)
	// The Shutdown cleanup is registered before the seam hold below creates
	// its own, so it runs after the hold's release: a failure while a parked
	// call holds a reference unwinds the hold first and the failure reports
	// itself instead of wedging the Shutdown past the run's timeout.
	shutRegister(t, e.c)

	h := e.seam.HoldFirst(t, journaltest.OpOpen, name(1))
	opener := make(chan error, 1)
	go func() {
		_, err := e.c.Acquire(1)
		opener <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the open never reached the hold")
	}

	// A second acquire parks on the in-flight open's reference; its entry
	// reference is what the poll below waits for.
	waiter := make(chan error, 1)
	go func() {
		_, err := e.c.Acquire(1)
		waiter <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); segment.Refs(e.c, 1) != 2; {
		if time.Now().After(deadline) {
			t.Fatal("the second acquire never took the in-flight open's reference")
		}
		time.Sleep(time.Millisecond)
	}

	closed := make(chan error, 1)
	go func() { closed <- e.c.Close(1) }()
	for deadline := time.Now().Add(10 * time.Second); !segment.EntryGone(e.c, 1); {
		if time.Now().After(deadline) {
			t.Fatal("Close(1) never started")
		}
		time.Sleep(time.Millisecond)
	}

	h.Release()
	for name, ch := range map[string]chan error{"the opening acquire": opener, "the waiting acquire": waiter} {
		select {
		case err := <-ch:
			if !errors.Is(err, segment.ErrSegmentGone) {
				t.Fatalf("%s = %v after a Close of its segment, want ErrSegmentGone", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not return after the open was released", name)
		}
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close(1) racing its own open = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close(1) did not return after the open drained")
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d, want 0: the file the gone entry was opening was closed", got)
	}
	if got := e.budget.Open(); got != 0 {
		t.Fatalf("the budget holds %d descriptors, want 0: the declined open returned its slot", got)
	}
	if got := segment.EntryCount(e.c); got != 1 {
		t.Fatalf("the cache holds %d entries, want 1", got)
	}
}

// TestDescriptorsShutdownRacingAnOpen checks the opener of an in-flight open
// against a Shutdown that starts while the open runs: the acquire answers
// ErrCacheClosed, the file the open fetched is closed rather than cached, its
// slot returns, and Shutdown finds nothing left to close. An opener that
// ignored the shut mark would cache a file Shutdown never closes, leaking the
// slot past the bound forever.
func TestDescriptorsShutdownRacingAnOpen(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)

	h := e.seam.HoldFirst(t, journaltest.OpOpen, name(1))
	opener := make(chan error, 1)
	go func() {
		_, err := e.c.Acquire(1)
		opener <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the open never reached the hold")
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- e.c.Shutdown() }()
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, err := e.c.Acquire(9)
		if errors.Is(err, segment.ErrCacheClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown never started")
		}
		time.Sleep(time.Millisecond)
	}

	h.Release()
	select {
	case err := <-opener:
		if !errors.Is(err, segment.ErrCacheClosed) {
			t.Fatalf("the opening acquire = %v after Shutdown, want ErrCacheClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the opening acquire did not return after the open was released")
	}
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown racing an in-flight open = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the open drained")
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d, want 0: the file the shut cache was opening was closed", got)
	}
	if got := e.budget.Open(); got != 0 {
		t.Fatalf("the budget holds %d descriptors, want 0: the declined open returned its slot", got)
	}
	if _, err := e.c.Acquire(2); !errors.Is(err, segment.ErrCacheClosed) {
		t.Fatalf("acquiring after Shutdown = %v, want ErrCacheClosed", err)
	}
}

// TestDescriptorsASecondCloseWaitsForTheOpenDrain checks the drain a second
// Close of a segment joins: while the first Close waits on an in-flight open's
// reference, a second Close of the same segment waits on the same channel and
// returns only when the drain ends. Returning early would let its caller free
// storage while the descriptor was still being opened, and a fresh channel per
// Close would strand the first waiter forever.
func TestDescriptorsASecondCloseWaitsForTheOpenDrain(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)
	// The Shutdown cleanup is registered before the seam hold below creates
	// its own, so it runs after the hold's release: a failure while a parked
	// call holds a reference unwinds the hold first and the failure reports
	// itself instead of wedging the Shutdown past the run's timeout.
	shutRegister(t, e.c)

	h := e.seam.HoldFirst(t, journaltest.OpOpen, name(1))
	opener := make(chan error, 1)
	go func() {
		_, err := e.c.Acquire(1)
		opener <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the open never reached the hold")
	}

	first := make(chan error, 1)
	go func() { first <- e.c.Close(1) }()
	for deadline := time.Now().Add(10 * time.Second); !segment.EntryGone(e.c, 1); {
		if time.Now().After(deadline) {
			t.Fatal("Close(1) never started")
		}
		time.Sleep(time.Millisecond)
	}

	second := make(chan error, 1)
	go func() { second <- e.c.Close(1) }()
	time.Sleep(100 * time.Millisecond) // let the second Close arrive
	select {
	case err := <-second:
		t.Fatalf("the second Close returned %v while its segment's open was still in flight, want it to wait for the drain", err)
	default:
	}

	h.Release()
	if err := <-opener; !errors.Is(err, segment.ErrSegmentGone) {
		t.Fatalf("the opening acquire = %v after a Close of its segment, want ErrSegmentGone", err)
	}
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("the first Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first Close did not return after the drain: its channel was replaced by the second Close's")
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("the second Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second Close did not return after the drain")
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d, want 0", got)
	}
}

// TestDescriptorsCloseDuringShutdownClosesOnce checks a Close that drains
// while the cache's Shutdown waits on another segment's reference: the file
// and its budget slot are taken exactly once, by whichever of the two reaches
// them first under the mutex. A Shutdown that closed what a finished Close had
// already closed would give the slot twice and drive the count below zero.
func TestDescriptorsCloseDuringShutdownClosesOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)
	prior := e.budget.Open()

	r1, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	r2, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("acquiring segment 2 failed: %v", err)
	}

	closed1 := make(chan error, 1)
	go func() { closed1 <- e.c.Close(1) }()
	for deadline := time.Now().Add(10 * time.Second); !segment.EntryGone(e.c, 1); {
		if time.Now().After(deadline) {
			t.Fatal("Close(1) never started")
		}
		time.Sleep(time.Millisecond)
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- e.c.Shutdown() }()
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, err := e.c.Acquire(9)
		if errors.Is(err, segment.ErrCacheClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown never started")
		}
		time.Sleep(time.Millisecond)
	}

	r1.Release()
	select {
	case err := <-closed1:
		if err != nil {
			t.Fatalf("Close(1) during Shutdown = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close(1) did not return after its own reference dropped")
	}
	r2.Release()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown under a concurrent Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the last reference dropped")
	}

	if closes := e.seam.CallsFor(journaltest.OpClose, name(1)) - e.stocked[1]; closes != 1 {
		t.Fatalf("segment 1's file was closed %d times, want once: a Close and a Shutdown that overlap must not both take it", closes)
	}
	if closes := e.seam.CallsFor(journaltest.OpClose, name(2)) - e.stocked[2]; closes != 1 {
		t.Fatalf("segment 2's file was closed %d times, want once", closes)
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d, want 0", got)
	}
	if got := e.openCount(2); got != 0 {
		t.Fatalf("segment 2's descriptor count is %d, want 0", got)
	}
	if got := e.budget.Open(); got != prior {
		t.Fatalf("the budget holds %d descriptors, want %d: a slot was given back twice", got, prior)
	}
	if s := e.c.Stats(); s.Open != 0 {
		t.Fatalf("Stats().Open = %d after Shutdown, want 0", s.Open)
	}
	if err := e.c.Shutdown(); err != nil {
		t.Fatalf("a second Shutdown = %v, want nil", err)
	}
}

// TestDescriptorsAStaleAcquireOfAMissingFileLeavesNoTrace checks the entry an
// open of a missing file leaves: the acquire answers ErrSegmentGone, and the
// entry it took for the open drops from the map when the open's reference
// releases — no Forget is needed, so the map stays bounded by the live
// segments however many stale ids are looked up, and each later acquire fails
// the same way without touching the map again.
func TestDescriptorsAStaleAcquireOfAMissingFileLeavesNoTrace(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)
	shutRegister(t, e.c)

	for i := 0; i < 3; i++ {
		if _, err := e.c.Acquire(7); !errors.Is(err, segment.ErrSegmentGone) {
			t.Fatalf("acquiring a segment with no file = %v, want ErrSegmentGone", err)
		}
		if got := segment.EntryCount(e.c); got != 0 {
			t.Fatalf("the cache holds %d entries after a failed open, want 0: the entry would leak the map one stale id at a time", got)
		}
	}
	e.c.Forget(7) // a no-op: there is nothing left to forget
	if got := segment.EntryCount(e.c); got != 0 {
		t.Fatalf("the cache holds %d entries after Forget, want 0", got)
	}
}

// TestDescriptorsEvictionFollowsRecency checks the idle list's order: a hit
// moves its descriptor to the most recently used end, so with 1 and 2 open
// under a bound of two, one more read of 1 and then a read of 3 closes 2's
// descriptor, not 1's. An eviction by first release would close the hot
// segment ahead of the cold one every time.
func TestDescriptorsEvictionFollowsRecency(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 3)
	shutRegister(t, e.c)

	e.read(t, 1)
	e.read(t, 2)
	e.read(t, 1) // the hit makes 1 the most recently used
	e.read(t, 3)
	if got := e.openCount(1); got != 1 {
		t.Fatalf("segment 1's descriptor count is %d after the read of segment 3, want 1: the hit made it the most recently used, so eviction must close segment 2's descriptor", got)
	}
	if got := e.openCount(2); got != 0 {
		t.Fatalf("segment 2's descriptor count is %d after the read of segment 3, want 0: it is the least recently used", got)
	}
}

// TestDescriptorsForgetKeepsALiveEntry checks Forget's no-op contract: a
// Forget of a segment whose descriptor the cache has merely evicted deletes
// nothing, so the entry keeps its everOpen and the next miss counts as a
// reopen. Deleting it would read the reopen counter as flat where the cache
// was thrashing.
func TestDescriptorsForgetKeepsALiveEntry(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 1)
	e.stock(t, 2)
	shutRegister(t, e.c)

	e.read(t, 1)
	e.read(t, 2) // evicts 1: the entry's descriptor is closed, but the entry is live

	// Forget of the evicted entry is the no-op: the segment is not gone, so
	// the entry keeps its everOpen.
	e.c.Forget(1)
	if got := segment.EntryCount(e.c); got != 2 {
		t.Fatalf("the cache holds %d entries after Forget of a live segment, want 2: Forget drops only gone entries", got)
	}

	// The retained entry still knows it was opened, so the miss counts as a
	// reopen. A Forget that deleted the live entry would lose that mark and
	// read the reopen counter as flat where the cache was thrashing.
	before := e.c.Stats().Reopens
	e.read(t, 1)
	if got := e.c.Stats().Reopens; got != before+1 {
		t.Fatalf("Reopens = %d after re-reading the forgotten-but-live segment 1, want %d: Forget deleted the live entry and lost its everOpen", got, before+1)
	}
}

// TestDescriptorsAIdleJournalYieldsToAnother checks the cross-journal half of
// the bound: one journal holds every slot of the shared budget in idle
// descriptors — nothing of its own held — and a second
// journal's first read still succeeds, because asking the budget reclaims an
// idle descriptor from the first journal rather than refusing. A takeSlot
// that only asked its own cache would answer ErrNoDescriptor and starve every
// other journal of the process for as long as one holds its descriptors idle.
func TestDescriptorsAIdleJournalYieldsToAnother(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seamA, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the first seam could not be made: %v", err)
	}
	seamB, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the second seam could not be made: %v", err)
	}
	budget := core.NewDescriptorBudget(4)
	a := cacheEnv{budget: budget, c: segment.NewDescriptors(seamA.FS(), name, budget), seam: seamA, stocked: map[core.SegmentID]int{}}
	b := cacheEnv{budget: budget, c: segment.NewDescriptors(seamB.FS(), name, budget), seam: seamB, stocked: map[core.SegmentID]int{}}
	a.stock(t, 4)
	b.stock(t, 4)
	shutRegister(t, a.c)
	shutRegister(t, b.c)

	// Journal A fills the whole budget with idle descriptors: four reads of
	// four distinct segments, each released — nothing held.
	for i := 1; i <= 4; i++ {
		a.read(t, core.SegmentID(i))
	}
	if got := budget.Open(); got != 4 {
		t.Fatalf("the budget holds %d descriptors after journal A's reads, want 4", got)
	}
	aOpen := a.c.Stats().Open
	if aOpen != 4 {
		t.Fatalf("journal A holds %d descriptors, want 4", aOpen)
	}

	// Journal B's first read must not be refused: it takes a slot the budget
	// reclaims from journal A's idle descriptors, so A's held count drops.
	b.read(t, 1)
	if got := budget.Open(); got > 4 {
		t.Fatalf("the budget holds %d descriptors after journal B's read, want at most 4", got)
	}
	after := a.c.Stats().Open
	if after != aOpen-1 {
		t.Fatalf("journal A holds %d descriptors after yielding one, want %d: an idle journal must give a descriptor to another journal's read", after, aOpen-1)
	}

	// The yielded descriptor is A's least recently used idle one, segment 1,
	// and re-reading it costs a reopen rather than failing.
	if got := a.openCount(1); got != 0 {
		t.Fatalf("segment 1 of journal A shows %d open descriptors, want 0: it is the descriptor the reclaim took", got)
	}
	a.read(t, 1)
}

// TestDescriptorsAStaleAcquireAfterForgetLeavesNoTrace checks the entry a
// retire-and-forget segment's stray acquires leave: a reader that resolved a
// segment before its retirement re-acquires it after Close, the unlink and
// Forget have all run — the only outcome is ErrSegmentGone, and the entry map
// ends where it started. An entry left behind per stray reader would grow the
// map permanently, one tombstone per read that raced a retirement, with no
// further Forget coming to drop it.
func TestDescriptorsAStaleAcquireAfterForgetLeavesNoTrace(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)
	shutRegister(t, e.c)

	e.read(t, 1)
	if err := e.c.Close(1); err != nil {
		t.Fatalf("Close(1) = %v, want nil", err)
	}
	if err := e.seam.Remove(name(1)); err != nil {
		t.Fatalf("removing segment 1's file failed: %v", err)
	}
	e.c.Forget(1)
	if got := segment.EntryCount(e.c); got != 0 {
		t.Fatalf("the cache holds %d entries after the Forget, want 0", got)
	}

	// Stray acquires of the retired id: each answers gone, and none may
	// leave an entry behind — retirement makes its one Forget call.
	for i := 0; i < 3; i++ {
		if _, err := e.c.Acquire(1); !errors.Is(err, segment.ErrSegmentGone) {
			t.Fatalf("a stray acquire of the retired segment = %v, want ErrSegmentGone", err)
		}
		if got := segment.EntryCount(e.c); got != 0 {
			t.Fatalf("the cache holds %d entries after a stray acquire, want 0: each stray reader must not grow the map", got)
		}
	}
	if s := e.c.Stats(); s.Open != 0 {
		t.Fatalf("Stats().Open = %d, want 0", s.Open)
	}
}

// TestDescriptorsShutdownDoesNotPinItsIdleDescriptors checks that a cache
// whose Shutdown is waiting on one slow reader does not hold the process's
// other journals hostage: its idle descriptors remain reclaimable while it
// waits, so another journal on the same budget still reads. Shutdown
// takes its files only once the wait ends — a reclaim that detached one
// first is skipped — so no descriptor is closed or given twice, however the
// two interleave.
func TestDescriptorsShutdownDoesNotPinItsIdleDescriptors(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seamA, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the first seam could not be made: %v", err)
	}
	seamB, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the second seam could not be made: %v", err)
	}
	budget := core.NewDescriptorBudget(4)
	a := cacheEnv{budget: budget, c: segment.NewDescriptors(seamA.FS(), name, budget), seam: seamA, stocked: map[core.SegmentID]int{}}
	b := cacheEnv{budget: budget, c: segment.NewDescriptors(seamB.FS(), name, budget), seam: seamB, stocked: map[core.SegmentID]int{}}
	a.stock(t, 2)
	b.stock(t, 4)

	// Journal A fills the budget: one reference it holds, one idle.
	held, err := a.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring journal A's segment 1 failed: %v", err)
	}
	a.read(t, 2)
	if got := budget.Open(); got != 2 {
		t.Fatalf("the budget holds %d descriptors, want 2", got)
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- a.c.Shutdown() }()
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, err := a.c.Acquire(9)
		if errors.Is(err, segment.ErrCacheClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("journal A's Shutdown never started")
		}
		time.Sleep(time.Millisecond)
	}

	// Shutdown is waiting on the held reference. Journal B takes references
	// until the budget is full, so its next acquire needs a slot only
	// another journal can yield — and the one shutting down holds one idle.
	// The acquire must succeed by reclaiming A's idle descriptor, not be
	// refused: a journal that is shutting down pins only the descriptors its
	// readers still hold, nothing a close of its own must wait for.
	var heldB []segment.Ref
	id := core.SegmentID(1)
	for budget.Open() < 4 {
		r, err := b.c.Acquire(id)
		if err != nil {
			t.Fatalf("filling the budget with journal B's references failed: %v", err)
		}
		heldB = append(heldB, r)
		id++
	}
	if int(id) > 4 {
		t.Fatalf("journal B took %d references before the budget filled, want 2: journal A holds the other two slots", len(heldB))
	}
	next, err := b.c.Acquire(id)
	if err != nil {
		t.Fatalf("journal B's acquire past the full budget = %v, want it to reclaim the shutting-down journal's idle descriptor", err)
	}
	if got := a.openCount(2); got != 0 {
		t.Fatalf("journal A's segment 2 shows %d open descriptors, want 0: it is the idle descriptor the reclaim took", got)
	}

	select {
	case <-shutdown:
		t.Fatal("journal A's Shutdown returned while its reader still held its reference, want it to wait")
	default:
	}

	// The held references end the wait; A's Shutdown closes what it still
	// holds — its reclaimed-away idle descriptor is not one of them — and
	// the budget's count and the seam's show every descriptor closed once.
	next.Release()
	for _, r := range heldB {
		r.Release()
	}
	held.Release()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("journal A's Shutdown after the release = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("journal A's Shutdown did not return after the release")
	}
	if s := a.c.Stats(); s.Open != 0 {
		t.Fatalf("journal A holds %d descriptors after its Shutdown, want 0", s.Open)
	}

	// Journal B still holds its three idle descriptors: its Shutdown is where
	// they close, and settling both journals is what empties the budget — the
	// shutting-down wait never left A's closed behind either.
	if err := b.c.Shutdown(); err != nil {
		t.Fatalf("journal B's Shutdown after the settle = %v, want nil", err)
	}
	for _, e := range []cacheEnv{a, b} {
		if s := e.c.Stats(); s.Open != 0 {
			t.Fatalf("a journal holds %d descriptors after both Shutdowns, want 0", s.Open)
		}
	}
	if got := budget.Open(); got != 0 {
		t.Fatalf("the budget holds %d descriptors after both journals settled, want 0", got)
	}
}

// TestDescriptorsCloseWaitsAReclaimClosingThatSegment checks the window the
// reclaimer leaves: a close of a descriptor runs with the cache's mutex
// released, so a Close of the same segment can otherwise see the entry with
// no file and return while the descriptor is still being closed — its caller
// would unlink the file under an open handle. The reclaim must hold a
// reference for the length of its close, so Close's drain waits for it.
func TestDescriptorsCloseWaitsAReclaimClosingThatSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 1)
	e.stock(t, 2)
	// The Shutdown cleanup is registered before the seam hold below creates
	// its own, so it runs after the hold's release: a failure while a parked
	// call holds a reference unwinds the hold first and the failure reports
	// itself instead of wedging the Shutdown past the run's timeout.
	shutRegister(t, e.c)

	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	e.read(t, 1)

	// The read of 2 evicts 1: the reclaimer is inside segment 1's close, held
	// at the seam — the exact window.
	evicted := make(chan error, 1)
	go func() {
		r, err := e.c.Acquire(2)
		if err == nil {
			r.Release()
		}
		evicted <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaim's close never reached the hold")
	}

	closed := make(chan error, 1)
	go func() { closed <- e.c.Close(1) }()
	for deadline := time.Now().Add(10 * time.Second); !segment.EntryGone(e.c, 1); {
		if time.Now().After(deadline) {
			t.Fatal("Close(1) never started")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close(1) returned %v while the reclaim's close of that segment was still in flight, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}

	h.Release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close(1) after the reclaim's close returned = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close(1) did not return after the reclaim's close ended")
	}
	if err := <-evicted; err != nil {
		t.Fatalf("the acquire of segment 2 under the evicting close = %v, want nil", err)
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d, want 0", got)
	}
}

// TestDescriptorsCloseWaitsAShutdownClosingThatSegment checks the same window
// on Shutdown's side: the shutdown's close walk collects the files under the
// mutex and closes them with it released, so a Close of a segment whose file
// that walk holds can otherwise return while the file is still being closed,
// green-lighting an unlink under an open handle. The walk must hold a
// reference per file for the length of its close, so the Close waits too.
func TestDescriptorsCloseWaitsAShutdownClosingThatSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)

	e.read(t, 1)
	held, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("acquiring segment 2 failed: %v", err)
	}
	// The held reference is dropped exactly once — by the body on the path
	// that reaches it, by this cleanup otherwise — so a failure before the
	// drop does not keep the Shutdown this test runs wedged on it.
	dropHeld := relRegister(t, held)

	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	shutdown := make(chan error, 1)
	go func() { shutdown <- e.c.Shutdown() }()
	// Shutdown has started once its rejects answer shut.
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, err := e.c.Acquire(9)
		if errors.Is(err, segment.ErrCacheClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown never started")
		}
		time.Sleep(time.Millisecond)
	}

	dropHeld()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown's close never reached the hold")
	}

	closed := make(chan error, 1)
	go func() { closed <- e.c.Close(1) }()
	select {
	case err := <-closed:
		t.Fatalf("Close(1) returned %v while the shutdown was still closing that segment's file, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}

	h.Release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close(1) after the shutdown's close returned = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close(1) did not return after the shutdown's close ended")
	}
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the close ended")
	}
	if closes := e.seam.CallsFor(journaltest.OpClose, name(2)) - e.stocked[2]; closes != 1 {
		t.Fatalf("segment 2's file was closed %d times, want once", closes)
	}
}

// TestDescriptorsCloseOfANeverAcquiredSegmentTombstonesIt pins the tombstone
// a Close of a segment the cache never held leaves: the entry it writes
// answers every later acquire ErrSegmentGone without an open, so a read
// racing that Close cannot reach storage the caller is about to free. An
// entry that merely vanished would fall back to the open, which finds the
// file still there — the caller frees it only after Close returns — and
// reads bytes from under the free.
func TestDescriptorsCloseOfANeverAcquiredSegmentTombstonesIt(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)

	// Segment 9 exists on storage too, so only the tombstone tells a later
	// acquire from an open: the open would find the file.
	stocked(t, e, 9)
	opens := e.seam.Calls(journaltest.OpOpen)

	if err := e.c.Close(9); err != nil {
		t.Fatalf("Close(9) of a never-acquired segment = %v, want nil", err)
	}
	if got := segment.EntryCount(e.c); got != 1 {
		t.Fatalf("the cache holds %d entries after the Close, want 1: the tombstone", got)
	}
	if !segment.EntryGone(e.c, 9) {
		t.Fatal("segment 9's entry is not gone: the Close left no tombstone")
	}

	// The tombstone answers before any open is attempted, so the seam sees
	// no second call of the segment's open.
	for i := 0; i < 3; i++ {
		if _, err := e.c.Acquire(9); !errors.Is(err, segment.ErrSegmentGone) {
			t.Fatalf("acquiring segment 9 after its Close = %v, want ErrSegmentGone", err)
		}
	}
	if got := e.seam.Calls(journaltest.OpOpen) - opens; got != 0 {
		t.Fatalf("the acquires after the Close made %d opens, want 0: the tombstone must answer them, not the file", got)
	}

	// The tombstone is not Forget-proof per the map's bound: it holds until
	// then, but it must not hold up a Cache-wide Shutdown.
	if err := e.c.Shutdown(); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
}

// TestDescriptorsShutdownSurvivesAReclaimAcrossItsWait pins the pairing of
// Shutdown's wait with the reclaimer's reference. The reference a reclaim
// takes and drops around its close can cross the signal the wait was
// answered by — the last release closed the wait's channel, and a reclaim
// has not yet let Shutdown take the mutex again — so closing the channel
// again on the crossing drop would panic on a closed channel. GOMAXPROCS(1)
// keeps the reclaim on one goroutine with the signal: no yield sits between
// the two, so the crossing is staged, not raced for.
func TestDescriptorsShutdownSurvivesAReclaimAcrossItsWait(t *testing.T) {
	prof.Watch(t, prof.Options{})

	runtime.GOMAXPROCS(1)
	defer func(n int) { runtime.GOMAXPROCS(n) }(runtime.GOMAXPROCS(1))

	for round := 0; round < 20; round++ {
		e := newCache(t, 8)
		e.stock(t, 3)
		e.read(t, 1) // idle
		e.read(t, 2) // idle

		r3, err := e.c.Acquire(3)
		if err != nil {
			t.Fatalf("acquiring segment 3 failed: %v", err)
		}
		dropR3 := relRegister(t, r3)

		shutdown := make(chan error, 1)
		go func() { shutdown <- e.c.Shutdown() }()
		for deadline := time.Now().Add(10 * time.Second); ; {
			_, err := e.c.Acquire(9)
			if errors.Is(err, segment.ErrCacheClosed) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Shutdown never started")
			}
			time.Sleep(time.Millisecond)
		}

		// Shutdown is parked in its wait before the reference drops: the
		// signal must find an armed wait, so the poll reads the probe
		// rather than guessing the scheduler reached the park in time.
		for deadline := time.Now().Add(10 * time.Second); !segment.WaitArmed(e.c); {
			if time.Now().After(deadline) {
				t.Fatal("Shutdown never armed its wait for the held reference")
			}
			time.Sleep(time.Millisecond)
		}

		// The last reference drops, which is the signal the wait answers
		// on. The shutdown goroutine is runnable but this one holds the
		// single processor, so nothing runs between the signal and the
		// reclaims below: each closes an idle file on this goroutine and
		// drops its reference — a crossing the closed channel must survive.
		dropR3()
		for i := 0; i < 2; i++ {
			if !e.budget.Reclaim() {
				t.Fatalf("Reclaim %d found no idle descriptor, want the cache's two idle segments", i)
			}
		}

		select {
		case err := <-shutdown:
			if err != nil {
				t.Fatalf("Shutdown = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Shutdown did not return once the reclaims ran")
		}
	}
}

// TestDescriptorsShutdownWaitsOutACloseTheReclaimIsStillMaking pins the wait
// itself. A reclaim running across the wait can take its reference right
// after the wait's signal fires, so a wait answered once walks off and
// returns while the reclaim is still mid-close, and its caller may free
// storage the close has not finished with. The wait must be taken back
// against the cache's total under the mutex.
//
// The order is staged, not left to the scheduler: the Shutdown is parked
// after its wake, the reclaim then takes segment 1's file and parks in its
// file close with the reference held, and only then does the Shutdown go on
// to retake the mutex.
func TestDescriptorsShutdownWaitsOutACloseTheReclaimIsStillMaking(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 8)
	shutRegister(t, e.c)
	e.stock(t, 2)
	e.read(t, 1) // idle, the reclaim's victim
	e.read(t, 2) // idle once released, the shutdown walk's to take

	r2, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("acquiring segment 2 failed: %v", err)
	}
	dropR2 := relRegister(t, r2)

	woke := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	restore := segment.ShutWakeHook(func() {
		once.Do(func() {
			close(woke)
			<-proceed
		})
	})
	defer restore()
	var proceedOnce sync.Once
	goOn := func() { proceedOnce.Do(func() { close(proceed) }) }
	t.Cleanup(goOn)

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- e.c.Shutdown() }()
	for deadline := time.Now().Add(10 * time.Second); !segment.WaitArmed(e.c); {
		if time.Now().After(deadline) {
			t.Fatal("Shutdown never armed its wait for the held reference")
		}
		time.Sleep(time.Millisecond)
	}

	// The last reference drops: the wait's signal fires, and the Shutdown
	// parks right after waking, before it retakes the mutex.
	dropR2()
	select {
	case <-woke:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never woke from its wait")
	}

	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	reclaimDone := make(chan bool, 1)
	go func() { reclaimDone <- e.budget.Reclaim() }()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaim's close of segment 1 never reached the hold")
	}

	// The close of segment 1 is in flight, its reference held, and the
	// Shutdown goes on: it must wait that close out. An answered-once wait
	// walks off over it (RFC 1 §7.4).
	goOn()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned %v with a close of segment 1 still in flight, want it to wait the close out", err)
	case <-time.After(300 * time.Millisecond):
	}

	h.Release()
	select {
	case <-reclaimDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaim did not finish behind the released close")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the close ended")
	}
}

// TestDescriptorsASecondCloseWaitsForTheFirstOnesFileClose pins the gap
// between two Closes of one segment: the first detaches the file under the
// mutex and closes it after unlocking, so a second that arrives while that
// close runs sees no file and no reference and returns nil — green-lighting
// an unlink of storage the first close is still flushing (RFC 1 §7.4). The
// close holds a reference for its own file, as a reclaim's does, so the
// second Close waits the close out.
func TestDescriptorsASecondCloseWaitsForTheFirstOnesFileClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)
	e.read(t, 1)

	// The first Close parks in the middle of its file close, the exact
	// window the second can arrive in.
	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	first := make(chan error, 1)
	go func() { first <- e.c.Close(1) }()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the first Close's file close never reached the hold")
	}

	second := make(chan error, 1)
	go func() { second <- e.c.Close(1) }()
	// The second must arrive while the first's close is parked: it is
	// blocked from returning only if the drain owns it.
	select {
	case err := <-second:
		t.Fatalf("the second Close returned %v while the first one's file close was still in flight, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}

	h.Release()
	if err := <-first; err != nil {
		t.Fatalf("the first Close = %v, want nil", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("the second Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second Close did not return after the first one's file close ended")
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after both Closes, want 0", got)
	}
	if closes := e.seam.CallsFor(journaltest.OpClose, name(1)) - e.stocked[1]; closes != 1 {
		t.Fatalf("segment 1's file was closed %d times, want once", closes)
	}
}

// TestDescriptorsShutdownWaitsOutACloseStillRunning checks the same gap on
// Shutdown's side: a Close that detached its file and is closing it outside
// the mutex holds a reference, which is what keeps the shutdown's close walk
// from returning over it — the walk takes files, and a walk that walked over
// an in-flight close would shut the cache down and free storage the close
// had not finished with.
func TestDescriptorsShutdownWaitsOutACloseStillRunning(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 2)
	e.read(t, 1)

	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	closed := make(chan error, 1)
	go func() { closed <- e.c.Close(1) }()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the Close's file close never reached the hold")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- e.c.Shutdown() }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned %v while a Close was still in the middle of its file close, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}

	h.Release()
	if err := <-closed; err != nil {
		t.Fatalf("the Close = %v, want nil", err)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the Close's file close ended")
	}
}

// TestDescriptorsAFailedOpensDropLeavesTheEvictable checks the failed-open
// path's drop against a descriptor the cache already holds: another acquire
// opens and releases the segment while a failed waiter still holds its
// reference, so the descriptor is open, unreferenced and clean when the
// waiter's drop runs — through dropRefLocked, on a wake, not through
// Release. A drop path that never relinks strands the descriptor off the
// idle list: open, holding its slot, and invisible to the reclaimer for
// good. Every path that drops a reference must run the tail Release runs,
// the idle linking included.
func TestDescriptorsAFailedOpensDropLeavesTheEvictable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 2)
	shutRegister(t, e.c)

	// A first open of segment 1 fails EIO, held at the seam while the
	// cache's slot is taken: the opener's Acquire is inside fs.Open.
	e.seam.SetFault(&eioFault{op: journaltest.OpOpen, key: name(1), n: 1})
	h := e.seam.HoldFirst(t, journaltest.OpOpen, name(1))
	opener := make(chan error, 1)
	go func() {
		_, err := e.c.Acquire(1)
		opener <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the opening acquire never reached the hold")
	}

	// The waiter parks on the in-flight open, its reference held from
	// before its wait — and parks again, later, at the hook between its
	// wake and refAfterOpen's lock, which is where the staging below opens
	// the segment against its outstanding reference. The hook window sits
	// inside refAfterOpen, where no seam call runs, so the check reaches it
	// through the probe instead.
	waiterAtHook := make(chan struct{})
	letWaiterThrough := make(chan struct{})
	restore := segment.WaiterHook(func() {
		waiterAtHook <- struct{}{}
		<-letWaiterThrough
	})
	defer restore()
	waiter := make(chan error, 1)
	go func() {
		_, err := e.c.Acquire(1)
		waiter <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); segment.Refs(e.c, 1) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("the waiting acquire never parked on the in-flight open")
		}
		time.Sleep(time.Millisecond)
	}

	// The held open fails EIO; the waiter wakes into the hook and parks
	// there, holding the last reference on an entry with no file.
	h.Release()
	if err := <-opener; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the opening acquire = %v, want the fault's EIO", err)
	}
	select {
	case <-waiterAtHook:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting acquire never woke into the hook")
	}
	e.seam.SetFault(nil)

	// The staging the case turns on: another acquire opens the segment
	// successfully and releases at once, so the descriptor is open and
	// unreferenced — the failed waiter's reference still the one
	// outstanding on the entry.
	stocked(t, e, 3)
	e.read(t, 1)
	if s := e.c.Stats(); s.Open != 1 {
		t.Fatalf("the cache holds %d descriptors after the retry, want 1", s.Open)
	}
	close(letWaiterThrough)

	// The waiter's drop runs now, against an open, clean, unreferenced
	// descriptor, and it must leave the entry exactly as Release would: in
	// the idle list. Fill the budget with segment 2 — both slots taken —
	// and acquire a third: the reclaimer must find segment 1's idle
	// descriptor and close it, not refuse.
	if err := <-waiter; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the waiting acquire = %v, want the open's EIO carried to it", err)
	}
	e.read(t, 2)
	r3, err := e.c.Acquire(3)
	if err != nil {
		t.Fatalf("acquiring segment 3 with the budget full = %v, want segment 1's idle descriptor reclaimed to make room", err)
	}
	r3.Release()

	// The fault's open was counted by the seam like any other, but it is no
	// descriptor this cache ever held, so the victim is named not by
	// openCount but by which file the reclaim closed: segment 1's close
	// count reaches its second — the stocking's and the reclaim's — while
	// segment 2's keeps its stocking's alone. A drop path that skipped the
	// idle linking under this staging leaves 2 as the only victim the
	// reclaimer finds.
	if got := e.seam.CallsFor(journaltest.OpClose, name(1)) - e.stocked[1]; got != 1 {
		t.Fatalf("segment 1's descriptor was closed %d times by the reclaim, want 1: the failed waiter's late drop left it out of the idle list", got)
	}
	if got := e.seam.CallsFor(journaltest.OpClose, name(2)) - e.stocked[2]; got != 0 {
		t.Fatalf("segment 2's descriptor was closed %d times, want 0: segment 1 was the least recently used idle one", got)
	}
}

// TestDescriptorsClosesOnOneDrainWaitOutTheFileClose pins the Closes that
// wait on one drain: the last Release wakes every one of them, and whichever
// locks first detaches the file and closes it outside the mutex. A Close that
// locks after it sees no file and must still not return while the file close
// runs, or its caller frees storage the close has not finished with.
func TestDescriptorsClosesOnOneDrainWaitOutTheFileClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	shutRegister(t, e.c)
	e.stock(t, 1)

	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	drop := relRegister(t, r)

	results := make(chan error, 2)
	go func() { results <- e.c.Close(1) }()
	for deadline := time.Now().Add(10 * time.Second); !segment.EntryGone(e.c, 1); {
		if time.Now().After(deadline) {
			t.Fatal("the first Close never started its drain")
		}
		time.Sleep(time.Millisecond)
	}
	go func() { results <- e.c.Close(1) }()
	time.Sleep(100 * time.Millisecond)

	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	drop()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("no Close reached the file close")
	}

	select {
	case err := <-results:
		t.Fatalf("a Close returned %v while the file close was still in flight, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}

	h.Release()
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a Close did not return after the file close ended")
		}
	}
	e.c.Forget(1)
	if n := segment.EntryCount(e.c); n != 0 {
		t.Fatalf("the cache holds %d entries after Forget, want 0", n)
	}
	if closes := e.seam.CallsFor(journaltest.OpClose, name(1)) - e.stocked[1]; closes != 1 {
		t.Fatalf("segment 1's file was closed %d times, want once", closes)
	}
}

// TestDescriptorsCloseAndShutdownOnOneReleaseWaitOutTheFileClose is the same
// gap with a Shutdown among the waiters: its walk may take the file the last
// Release woke it for, and a Close woken by the same Release must not return
// while that close runs.
func TestDescriptorsCloseAndShutdownOnOneReleaseWaitOutTheFileClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	shutRegister(t, e.c)
	e.stock(t, 1)

	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	drop := relRegister(t, r)

	closeDone := make(chan error, 1)
	go func() { closeDone <- e.c.Close(1) }()
	for deadline := time.Now().Add(10 * time.Second); !segment.EntryGone(e.c, 1); {
		if time.Now().After(deadline) {
			t.Fatal("Close never started its drain")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- e.c.Shutdown() }()
	for deadline := time.Now().Add(10 * time.Second); !segment.WaitArmed(e.c); {
		if time.Now().After(deadline) {
			t.Fatal("Shutdown never armed its wait")
		}
		time.Sleep(time.Millisecond)
	}

	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	drop()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("no close of segment 1 reached the hold")
	}

	select {
	case err := <-closeDone:
		t.Fatalf("Close returned %v while a close of its file was still in flight, want it to wait", err)
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned %v while a close of segment 1 was still in flight, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}

	h.Release()
	if err := <-closeDone; err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	e.c.Forget(1)
	if n := segment.EntryCount(e.c); n != 0 {
		t.Fatalf("the cache holds %d entries after Forget, want 0", n)
	}
}

// TestDescriptorsCloseKeepsItsSlotUntilTheFileIsClosed checks the budget's
// bound across a Close: the slot returns only once the file is closed, so a
// read of another segment cannot open a descriptor while this one is still
// being closed and take the process past its limit.
func TestDescriptorsCloseKeepsItsSlotUntilTheFileIsClosed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 1)
	shutRegister(t, e.c)
	e.stock(t, 2)
	e.read(t, 1)

	h := e.seam.HoldFirst(t, journaltest.OpClose, name(1))
	closed := make(chan error, 1)
	go func() { closed <- e.c.Close(1) }()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the Close's file close never reached the hold")
	}

	if r, err := e.c.Acquire(2); err == nil {
		r.Release()
		t.Fatal("Acquire(2) succeeded while segment 1's file was still closing, want ErrNoDescriptor")
	} else if !errors.Is(err, segment.ErrNoDescriptor) {
		t.Fatalf("Acquire(2) = %v, want ErrNoDescriptor", err)
	}

	h.Release()
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	r, err := e.c.Acquire(2)
	if err != nil {
		t.Fatalf("Acquire(2) after the close = %v, want nil", err)
	}
	r.Release()
}

// TestDescriptorsASecondShutdownWaitsForTheFirst checks that a Shutdown that
// arrives while another is still waiting on references does not return over
// them: a caller that sees Shutdown return may assume no descriptor of the
// cache is open.
func TestDescriptorsASecondShutdownWaitsForTheFirst(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 1)

	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 failed: %v", err)
	}
	drop := relRegister(t, r)

	first := make(chan error, 1)
	go func() { first <- e.c.Shutdown() }()
	for deadline := time.Now().Add(10 * time.Second); !segment.WaitArmed(e.c); {
		if time.Now().After(deadline) {
			t.Fatal("the first Shutdown never armed its wait")
		}
		time.Sleep(time.Millisecond)
	}

	second := make(chan error, 1)
	go func() { second <- e.c.Shutdown() }()
	select {
	case err := <-second:
		t.Fatalf("the second Shutdown returned %v with segment 1's descriptor still open (%d open), want it to wait", err, e.openCount(1))
	case <-time.After(300 * time.Millisecond):
	}

	drop()
	for name, ch := range map[string]chan error{"first": first, "second": second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("the %s Shutdown = %v, want nil", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s Shutdown did not return after the release", name)
		}
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after Shutdown, want 0", got)
	}
}

// TestDescriptorsDirtyDescriptorPinned checks that a descriptor a write has
// gone through is never closed to make room under the bound: reopening its
// file can no longer see a writeback error the platform reported only through
// the open handle, so the descriptor stays open until a sync through it has
// covered the write or failed. It catches an eviction of a dirty descriptor —
// the pin would be gone and nineteen reads past the bound would close it.
func TestDescriptorsDirtyDescriptorPinned(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 20)
	shutRegister(t, e.c)

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
	// segment 1's dirty descriptor: each one of these needs its slot.
	for i := 2; i <= 20; i++ {
		e.read(t, core.SegmentID(i))
		if got := e.openCount(1); got != 1 {
			t.Fatalf("segment 1's descriptor count is %d after reading segment %d, want its dirty descriptor held open", got, i)
		}
	}

	// A sync through its descriptor covers the write, so the descriptor
	// becomes evictable once its reference drops, and later reads close it
	// to make room.
	r, err = e.c.Acquire(1)
	if err != nil {
		t.Fatalf("reacquiring segment 1 failed: %v", err)
	}
	if err := r.Sync(); err != nil {
		t.Fatalf("syncing segment 1 failed: %v", err)
	}
	r.Release()
	if closeCalls := e.seam.CallsFor(journalv2test.OpClose, name(1)) - e.stocked[1]; closeCalls != 0 {
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
// returns an error still covers the writes it started at, because its caller
// has seen the failure — the durability layer owns the failed window from
// there, and the bound is not to hold a descriptor open for writes nobody
// will retry through it. It catches a failed sync that leaves the entry
// pinned forever.
func TestDescriptorsFailedSyncUnpins(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 4)
	e.stock(t, 20)
	shutRegister(t, e.c)

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
	// Dirty again, and this time the sync fails.
	if _, err := r.WriteAt(buf, 0); err != nil {
		t.Fatalf("writing through segment 1's descriptor a second time failed: %v", err)
	}
	if err := e.seam.Schedule(journalv2test.Failing(journalv2test.OpSync, name(1), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := r.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("syncing segment 1 under the fault = %v, want EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	r.Release()

	// The failed sync covered the writes it started at, so the descriptor is
	// evictable: reads past the bound close it to make room.
	for i := 2; i <= 20; i++ {
		e.read(t, core.SegmentID(i))
	}
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after a failed sync and nineteen reads, want the descriptor evicted: a failed sync must unpin it", got)
	}
}

// TestDescriptorsSyncDoesNotCoverAWriteInFlight checks the pin's numbering
// rule: a write is counted once it has landed, so a Sync that starts while
// another reference's write is still in flight does not cover it. The
// group-commit shape — one reference appending, another syncing the same
// descriptor — otherwise lets the fsync finish before the write lands yet mark
// it covered, and the descriptor is evicted carrying a writeback error nobody
// would ever see.
func TestDescriptorsSyncDoesNotCoverAWriteInFlight(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 3)
	// Cleanups run last-registered first: the Shutdown registered here runs
	// after the holds and references below unwind, so a failure on any
	// assertion reports itself rather than wedging a parked call.
	shutRegister(t, e.c)

	a, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 for the write failed: %v", err)
	}
	h := e.seam.HoldFirst(t, journalv2test.OpWrite, name(1))
	written := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := a.WriteAt(buf, 0)
		written <- err
	}()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the write never reached the hold")
	}

	// The write is in flight: a Sync through a second reference returns nil,
	// but it cannot have covered a write that had not landed.
	b, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 for the sync failed: %v", err)
	}
	select {
	case err := <-written:
		t.Fatalf("the held write returned %v with the hold up, want it parked", err)
	default:
	}
	if err := b.Sync(); err != nil {
		t.Fatalf("the sync through the second reference = %v, want nil: nothing fails it", err)
	}
	h.Release()
	if err := <-written; err != nil {
		t.Fatalf("the released write failed: %v", err)
	}
	a.Release()
	b.Release()

	// The write landed after the sync started, so no sync covers it: the
	// descriptor is dirty and must not be closed to make room.
	e.read(t, 2)
	e.read(t, 3)
	if got := e.openCount(1); got != 1 {
		t.Fatalf("segment 1's descriptor count is %d after reads past the bound, want 1: the concurrent Sync covered a write that landed after the fsync", got)
	}

	// The pin lifts once a sync that started after the write returned.
	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("reacquiring segment 1 failed: %v", err)
	}
	if err := r.Sync(); err != nil {
		t.Fatalf("syncing segment 1 failed: %v", err)
	}
	r.Release()
	e.read(t, 2)
	e.read(t, 3)
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after a covering sync and reads past the bound, want it evicted", got)
	}
}

// TestDescriptorsEverySlotDirtyOrReferenced checks the refusal when the bound
// is full of descriptors that may not be closed: two dirty ones at a bound of
// two leave the third segment's acquire with ErrNoDescriptor, performed with
// no I/O and no wait — the refusal is the misconfiguration surfacing, which
// is why the bound must exceed what the process's streams hold at once. It
// catches a reclaim that falls back to closing a dirty entry.
func TestDescriptorsEverySlotDirtyOrReferenced(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 3)
	shutRegister(t, e.c)

	for _, id := range []core.SegmentID{1, 2} {
		r, err := e.c.Acquire(id)
		if err != nil {
			t.Fatalf("acquiring segment %d failed: %v", id, err)
		}
		if _, err := r.WriteAt(make([]byte, 16), 0); err != nil {
			t.Fatalf("writing through segment %d's descriptor failed: %v", id, err)
		}
		r.Release()
	}

	opens := e.seam.Calls(journalv2test.OpOpen)
	start := time.Now()
	r3, err := e.c.Acquire(3)
	if err != nil {
		if !errors.Is(err, segment.ErrNoDescriptor) {
			t.Fatalf("acquiring segment 3 with every slot dirty = %v, want ErrNoDescriptor", err)
		}
	} else {
		// The acquire succeeded where it must not: release it before the
		// failure is reported, or the cleanup's Shutdown waits on it
		// forever and the refusal the test exists to raise never surfaces.
		r3.Release()
		t.Fatal("acquiring segment 3 with every slot dirty succeeded, want ErrNoDescriptor")
	}
	if past := time.Since(start); past > time.Second {
		t.Fatalf("the refusal took %v, want it at once: a dirty slot is not a queue to wait in", past)
	}
	if got := e.seam.Calls(journalv2test.OpOpen); got != opens {
		t.Fatalf("the seam saw %d opens after the refusal, want still %d: the refusal performs no I/O", got, opens)
	}
	for _, id := range []core.SegmentID{1, 2} {
		if got := e.openCount(id); got != 1 {
			t.Fatalf("segment %d's descriptor count is %d after the refusal, want its dirty descriptor still held", id, got)
		}
	}
}

// TestDescriptorsAWriteUnderTheParkedSyncStaysUncovered checks the pin's
// numbering rule from the sync side: the fsync itself is parked at the seam —
// not the write — while a second reference completes a write that lands
// before the sync returns. The write is counted once it lands, so a Sync
// that started before it cannot cover it, and the descriptor stays pinned
// through reads past the bound; only a sync that starts after the write
// covers it. It catches a Sync that re-reads the count after its fsync and
// so marks an in-flight write covered.
func TestDescriptorsAWriteUnderTheParkedSyncStaysUncovered(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := newCache(t, 2)
	e.stock(t, 3)
	// Cleanups run last-registered first: the Shutdown registered here runs
	// after the holds and references below unwind, so a failure on any
	// assertion reports itself rather than wedging a parked call.
	shutRegister(t, e.c)

	a, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 for the sync failed: %v", err)
	}
	h := e.seam.HoldFirst(t, journalv2test.OpSync, name(1))
	syncErr := make(chan error, 1)
	go func() { syncErr <- a.Sync() }()
	select {
	case <-h.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the sync never reached the hold")
	}

	// The fsync runs while this write lands: the sync started first, so no
	// count it could have loaded includes the write.
	b, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("acquiring segment 1 for the write failed: %v", err)
	}
	if _, err := b.WriteAt(make([]byte, 16), 0); err != nil {
		t.Fatalf("writing under the parked sync failed: %v", err)
	}
	select {
	case err := <-syncErr:
		t.Fatalf("the parked sync returned %v with the hold up, want it parked", err)
	default:
	}
	h.Release()
	select {
	case err := <-syncErr:
		if err != nil {
			t.Fatalf("the released sync failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the released sync did not return")
	}
	a.Release()
	b.Release()

	// The write landed after the sync started, so no sync covers it: the
	// descriptor is dirty and must not be closed to make room.
	e.read(t, 2)
	e.read(t, 3)
	if got := e.openCount(1); got != 1 {
		t.Fatalf("segment 1's descriptor count is %d after reads past the bound, want 1: the sync counted a write that landed while its fsync ran", got)
	}

	// The pin lifts once a sync that started after the write returned.
	r, err := e.c.Acquire(1)
	if err != nil {
		t.Fatalf("reacquiring segment 1 failed: %v", err)
	}
	if err := r.Sync(); err != nil {
		t.Fatalf("syncing segment 1 failed: %v", err)
	}
	r.Release()
	e.read(t, 2)
	e.read(t, 3)
	if got := e.openCount(1); got != 0 {
		t.Fatalf("segment 1's descriptor count is %d after a covering sync and reads past the bound, want it evicted", got)
	}
}
