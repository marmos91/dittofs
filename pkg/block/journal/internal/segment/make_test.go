package segment_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// segCap is the cap of every set these tests build; noMax is the maximum the
// tests that do not test the maximum pass, so a Make can never be refused by
// it.
const (
	segCap = int64(64 << 10)
	noMax  = math.MaxInt64
)

// newSeam returns a seam over its own empty directory, refusing the test on
// the seam's own errors.
func newSeam(t *testing.T) *journaltest.Seam {
	t.Helper()

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("NewSeam: %v", err)
	}
	return seam
}

// maker is a Maker over a fresh seam and a fresh set: every test's default.
// It gives Maker the seam's epoch-bound view, never the seam itself, so a
// crash the test makes ends the Maker's calls rather than silently acting on
// the directory the crash left.
func maker(t *testing.T, last core.SegmentID, max int64) (*journaltest.Seam, *segment.Set, *segment.Maker) {
	t.Helper()

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)
	return seam, set, segment.NewMaker(seam.FS(), set, max, last, nil)
}

// awaitReached fails t unless the Make parked at hold reaches it: a
// regression that skips the held step altogether must fail the test on its
// own assertion, not hang until the whole binary is timed out. Make's
// result arrives on res instead; whichever comes first wins.
func awaitReached(t *testing.T, step string, hold *journaltest.Hold, res <-chan error) {
	t.Helper()

	select {
	case <-hold.Reached():
	case err := <-res:
		t.Fatalf("the Make returned (%v) without ever reaching its %s step", err, step)
	}
}

// renameApplier is a storage.FS whose Rename calls through and then fails:
// the rename-half-done case the seam's own failing rule cannot spell, since
// a rule it applies refuses the call before anything moves. Everything
// except Rename the seam's view answers untouched. removeFails drives the
// wrapped Remove's refusal, for the leftover half of the case.
type renameApplier struct {
	storage.FS
	removeFails bool
}

func (a *renameApplier) Rename(oldName, newName string) error {
	if err := a.FS.Rename(oldName, newName); err != nil {
		return err
	}
	// decision: the EIO is returned after the rename applied, not instead of
	// it, because that is the case this wrapper exists to spell: storage that
	// moved the file and still reported a failure leaves the file under the
	// new name, so the cleanup must try both names. A refusal before
	// anything moved is the seam's own failing rule, already in the matrix.
	return syscall.EIO
}

func (a *renameApplier) Remove(name string) error {
	if a.removeFails {
		return syscall.EIO
	}
	return a.FS.Remove(name)
}

// classify splits the names a List returned into segment and spare names,
// in List's order, dropping every other name.
func classify(names []string) (segs, spares []string) {
	for _, name := range names {
		switch kind, _ := format.ClassifyName(name); kind {
		case format.NameSegment:
			segs = append(segs, name)
		case format.NameSpare:
			spares = append(spares, name)
		}
	}
	return segs, spares
}

// listNames returns the journal directory as the seam's current process sees
// it, which every post-crash check must: a view bound to an earlier epoch
// refuses the List.
func listNames(t *testing.T, seam *journaltest.Seam) []string {
	t.Helper()

	names, err := seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return names
}

// assertWhole fails t unless the seam's directory holds exactly the file
// name as a whole segment: present, at the cap, and zeros end to end. It is
// the "whole" half of whole-or-absent; the absence it is checked against is
// the caller's, by name.
func assertWhole(t *testing.T, seam *journaltest.Seam, name string) {
	t.Helper()

	f, err := seam.Open(name)
	if err != nil {
		t.Fatalf("whole %s: Open: %v", name, err)
	}
	defer func() { _ = f.Close() }()

	size, err := f.Size()
	if err != nil {
		t.Fatalf("whole %s: Size: %v", name, err)
	}
	if size != segCap {
		t.Fatalf("whole %s: the file is %d bytes, want the whole cap %d", name, size, segCap)
	}

	buf := make([]byte, segCap)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("whole %s: ReadAt: %v", name, err)
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("whole %s: byte %d is %#x, want the zeros a fresh spare is made of", name, i, b)
		}
	}
}

// recording is a test-local fault that records every call the seam consults
// it on — the op and the key of each, in order — and passes every call. It
// satisfies the seam's fault without the test importing the fault package,
// which only the test components may.
type recording struct {
	mu    sync.Mutex
	calls []string
}

func (r *recording) At(op string, key []byte) (time.Duration, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, op+" "+string(key))
	return 0, 0, nil
}

// TestMakeStorageCallOrder checks that one Make drives storage through exactly
// the whole-or-not-at-all sequence: create the spare's temporary name,
// allocate the whole cap through it, sync it, close it, rename it into place,
// sync the directory, and only then return. It catches a Make that renames
// before its sync, that closes over unsynced writes, skips the directory
// sync, or returns before the segment is durable under its final name.
func TestMakeStorageCallOrder(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, _, m := maker(t, 0, noMax)
	rec := &recording{}
	seam.SetFault(rec)

	id, err := m.Make()
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if id != 1 {
		t.Fatalf("Make returned id %d, want 1: with last 0 the first spare is id 1", id)
	}

	rec.mu.Lock()
	got := slices.Clone(rec.calls)
	rec.mu.Unlock()

	spare := format.SpareName(id)
	// The directory sync's key is the empty one, so its record ends in the
	// joining space.
	want := []string{
		journaltest.OpCreate + " " + spare,
		journaltest.OpAllocate + " " + spare,
		journaltest.OpSync + " " + spare,
		journaltest.OpClose + " " + spare,
		journaltest.OpRename + " " + spare,
		journaltest.OpSyncDir + " ",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Make drove storage as %v, want exactly %v", got, want)
	}
	if name := format.SegmentName(id); !slices.Contains(listNames(t, seam), name) {
		t.Fatalf("after Make the directory holds %v, want %s", listNames(t, seam), name)
	}
}

// TestMakeCrashLeavesTheSpareWholeOrAbsent crashes a Make at each of its
// steps — create, allocate, sync, close, rename, directory sync — and checks
// the crash leaves the spare the RFC promises: no segment file under the
// final name, or one that is whole (never a partial .seg), and at most one
// temporary spare name. A crash after Make returned leaves exactly one whole
// segment: everything Make finished is durable, which is what the directory
// sync was for.
func TestMakeCrashLeavesTheSpareWholeOrAbsent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	spare := format.SpareName(core.SegmentID(1))

	for _, tc := range []struct {
		op  string
		key string // "*" for the directory sync: its own key is empty, which a hold cannot name
	}{
		{journaltest.OpCreate, spare},
		{journaltest.OpAllocate, spare},
		{journaltest.OpSync, spare},
		{journaltest.OpClose, spare},
		{journaltest.OpRename, spare},
		{journaltest.OpSyncDir, "*"},
	} {
		t.Run(tc.op, func(t *testing.T) {
			seam, set, m := maker(t, 0, noMax)

			h := seam.HoldFirst(t, tc.op, tc.key)
			idc := make(chan core.SegmentID, 1)
			errc := make(chan error, 1)
			go func() {
				id, err := m.Make()
				idc <- id
				errc <- err
			}()
			awaitReached(t, tc.op, h, errc)

			if err := seam.Crash(); err != nil {
				t.Fatalf("Crash at %s: %v", tc.op, err)
			}
			h.Release()

			id := <-idc
			err := <-errc
			if id != 0 {
				t.Fatalf("a Make crashed at %s returned id %d, want 0", tc.op, id)
			}
			if err == nil {
				t.Fatalf("a Make crashed at %s returned no error", tc.op)
			}
			if !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("a Make crashed at %s returned %v, want the crash's fs.ErrClosed", tc.op, err)
			}

			names := listNames(t, seam)
			segs, spares := classify(names)
			if len(segs) > 1 {
				t.Fatalf("a crash at %s left %v, want at most one segment name", tc.op, segs)
			}
			for _, name := range segs {
				assertWhole(t, seam, name)
			}
			if len(spares) > 1 {
				t.Fatalf("a crash at %s left %v, want at most one spare name", tc.op, spares)
			}
			if err := set.Check(); err != nil {
				t.Fatalf("a crash at %s: %v", tc.op, err)
			}
		})
	}

	t.Run("after Make returned", func(t *testing.T) {
		seam, set, m := maker(t, 0, noMax)

		id, err := m.Make()
		if err != nil {
			t.Fatalf("Make: %v", err)
		}
		if err := seam.Crash(); err != nil {
			t.Fatalf("Crash: %v", err)
		}

		segs, spares := classify(listNames(t, seam))
		if len(segs) != 1 || segs[0] != format.SegmentName(id) {
			t.Fatalf("a crash after Make left the segments %v and the spares %v, want exactly %s",
				segs, spares, format.SegmentName(id))
		}
		if len(spares) != 0 {
			t.Fatalf("a crash after Make left the spare names %v, want none", spares)
		}
		assertWhole(t, seam, segs[0])
		if c := set.Counters(); c.UsedBytes != segCap || c.Spares != 1 {
			t.Fatalf("after the crash the ledger holds %d bytes in %d spares, want one spare at %d",
				c.UsedBytes, c.Spares, segCap)
		}
	})
}

// TestMakeSpareTmpDurableByAnotherSyncBoundsTheNextID checks the R16 half of
// a restart a Make did not finish: a `.spare.tmp` that some other directory
// sync made durable survives the crash, and the next spare is drawn above it,
// starting from what the directory names — never from a count of this
// process's makes. It catches an allocator that ignores the spare's temporary
// name, which would draw its id again and collide with the file the crash
// kept.
func TestMakeSpareTmpDurableByAnotherSyncBoundsTheNextID(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, _, m := maker(t, 0, noMax)

	spare := format.SpareName(core.SegmentID(1))
	h := seam.HoldFirst(t, journaltest.OpRename, spare)
	errc := make(chan error, 1)
	go func() { errc <- errOf(m.Make()) }()
	awaitReached(t, journaltest.OpRename, h, errc)

	// Another directory sync — not Make's own, which is parked at the rename
	// — makes the temporary name durable.
	if err := seam.SyncDir(); err != nil {
		t.Fatalf("the test's own SyncDir: %v", err)
	}
	if err := seam.Crash(); err != nil {
		t.Fatalf("Crash: %v", err)
	}
	h.Release()
	if err := <-errc; !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("the crashed Make returned %v, want the crash's fs.ErrClosed", err)
	}

	names := listNames(t, seam)
	if _, spares := classify(names); len(spares) != 1 || spares[0] != spare {
		t.Fatalf("after the crash the directory holds %v, want the durable spare %s", spares, spare)
	}

	if top := segment.HighestID(names); top != 1 {
		t.Fatalf("HighestID over the directory holding %s returned %d, want 1", spare, top)
	}

	_, _, m2 := maker(t, 1, noMax)
	id, err := m2.Make()
	if err != nil {
		t.Fatalf("the Maker seeded with last 1: Make: %v", err)
	}
	if id != 2 {
		t.Fatalf("the Maker seeded with last 1 drew id %d, want 2: the first spare a restart makes is above every name the directory holds", id)
	}
}

// TestHighestID checks the id a restart resumes drawing from: the largest the
// directory names over its segments and spares — spare included, since a
// durable spare's id is taken — and nothing else. A name the format refuses,
// however large a number it carries, counts for nothing, and an empty
// directory starts the ids at 0.
func TestHighestID(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name  string
		names []string
		want  core.SegmentID
	}{
		{"no names", nil, 0},
		{"only a spare", []string{"00000000000000000042.spare.tmp"}, 42},
		{"the larger id on the spare", []string{
			"00000000000000000007.seg",
			"00000000000000000009.spare.tmp",
		}, 9},
		{"the larger id on the segment", []string{
			"00000000000000000090.seg",
			"00000000000000000009.spare.tmp",
		}, 90},
		{"a wrong width counts for nothing", []string{
			"00000000000000000007.seg",
			"999999999999999999999999.seg", // twenty-five digits
		}, 7},
		{"id 0 counts for nothing", []string{
			"00000000000000000007.seg",
			"00000000000000000000.seg",
		}, 7},
		{"the format files count for nothing", []string{
			format.FormatFileName,
			format.FormatTmpName,
			"00000000000000000007.seg",
		}, 7},
		{"any other name counts for nothing", []string{
			"foo.spare.tmp", // not the format's twenty-digit prefix
			"99999999999999999999.txt",
			"00000000000000000007.seg",
		}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := segment.HighestID(tc.names); got != tc.want {
				t.Fatalf("HighestID(%v) = %d, want %d", tc.names, got, tc.want)
			}
		})
	}
}

// TestMakerDrawsAscendingIDs checks that ids ascend in draw order from the
// last one the Maker was seeded with: 1, 2, 3 from 0; 42 from 41. Sixteen
// concurrent Makes each draw their own id — the allocator is one atomic, so
// no two draws agree — and each finished Make leaves its whole segment under
// the id it drew.
func TestMakerDrawsAscendingIDs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("ascending from zero", func(t *testing.T) {
		_, _, m := maker(t, 0, noMax)
		for want := core.SegmentID(1); want <= 3; want++ {
			id, err := m.Make()
			if err != nil {
				t.Fatalf("Make %d: %v", want, err)
			}
			if id != want {
				t.Fatalf("the %d-th Make returned id %d", want, id)
			}
		}
	})

	t.Run("ascending from the seed", func(t *testing.T) {
		_, _, m := maker(t, 41, noMax)
		id, err := m.Make()
		if err != nil {
			t.Fatalf("Make: %v", err)
		}
		if id != 42 {
			t.Fatalf("a Maker seeded with 41 drew %d, want 42", id)
		}
	})

	t.Run("a lost id is never redrawn", func(t *testing.T) {
		seam, _, m := maker(t, 0, noMax)

		spare := format.SpareName(core.SegmentID(1))
		if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, spare, 1, syscall.EIO)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if _, err := m.Make(); err == nil {
			t.Fatal("the first Make: no error, want the allocation's EIO")
		}

		id, err := m.Make()
		if err != nil {
			t.Fatalf("the second Make: %v", err)
		}
		if id != 2 {
			t.Fatalf("the Make after a failed one drew id %d, want 2: a lost id is never redrawn", id)
		}
	})

	t.Run("sixteen concurrent", func(t *testing.T) {
		const makes = 16

		seam, _, m := maker(t, 0, int64(makes)*segCap)
		ids := make(chan core.SegmentID, makes)
		errs := make(chan error, makes)
		for range makes {
			go func() {
				id, err := m.Make()
				ids <- id
				errs <- err
			}()
		}

		seen := make(map[core.SegmentID]bool, makes)
		for range makes {
			id := <-ids
			if id == 0 {
				t.Fatal("a Make returned id 0 alongside an error it should also have returned")
			}
			if seen[id] {
				t.Fatalf("two concurrent Makes drew the same id %d", id)
			}
			seen[id] = true
		}
		for range makes {
			if err := <-errs; err != nil {
				t.Errorf("a concurrent Make: %v", err)
			}
		}

		segs, _ := classify(listNames(t, seam))
		if len(segs) != makes {
			t.Fatalf("after %d concurrent Makes the directory holds %v, want one whole segment per id", makes, segs)
		}
		for _, name := range segs {
			assertWhole(t, seam, name)
		}
	})
}

// errOf turns a Make call that returns its id into the error it returned, so
// a goroutine can hand the call's result to the test as one value.
func errOf(_ core.SegmentID, err error) error { return err }

// TestMakeRefusedAtTheMaximumMakesNoStorageCall checks that a Make that the
// maximum refuses touches storage never: the reservation comes before any
// I/O, and a Make that read Counters and decided after its own call could
// pass two through the maximum when concurrent. Eight Makes against a maximum
// of two and a half caps: exactly two succeed, the six others return the
// NoSpaceError the journal limit owes, and exactly two files are created —
// the refusals created nothing (the create count, not just the outcome,
// catches a Make that calls Create and only then asks the ledger).
func TestMakeRefusedAtTheMaximumMakesNoStorageCall(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const makes = 8
	const fitted = 2 // a maximum of 2.5 caps holds exactly two whole caps

	seam, set, m := maker(t, 0, 2*segCap+segCap/2)

	var (
		ids  = make(chan core.SegmentID, makes)
		errs = make(chan error, makes)
		wg   sync.WaitGroup
	)
	for range makes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := m.Make()
			ids <- id
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)

	won := 0
	drawn := 0
	for err := range errs {
		if err != nil {
			var ns *core.NoSpaceError
			if errors.As(err, &ns) && ns.Limit == core.LimitJournal {
				continue
			}
			t.Errorf("a Make refused at the maximum returned %v, want a *core.NoSpaceError naming the journal limit", err)
			continue
		}
		won++
	}
	for id := range ids {
		if id != 0 {
			drawn++
		}
	}
	if drawn != won {
		t.Fatalf("%d Makes returned an id, %d succeeded, want the successes to be exactly the ones that drew",
			drawn, won)
	}

	if calls := seam.Calls(journaltest.OpCreate); calls != fitted {
		t.Fatalf("the Makes created %d files, want %d: a refused Make must make no seam call", calls, fitted)
	}
	if c := set.Counters(); c.UsedBytes != fitted*segCap || c.Spares != fitted {
		t.Fatalf("after the Makes the ledger holds %d bytes in %d spares, want %d in %d",
			c.UsedBytes, c.Spares, fitted*segCap, fitted)
	}
	segs, _ := classify(listNames(t, seam))
	if len(segs) != fitted {
		t.Fatalf("the directory holds %v, want %d whole segments and nothing else", segs, fitted)
	}
}

// TestMakeRaisesTheFootprintBeforeTheAllocation checks the R52 order: the
// footprint is raised before the allocation it accounts for happens, so a
// Make parked at its allocation is seen in the counter first.
func TestMakeRaisesTheFootprintBeforeTheAllocation(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, set, m := maker(t, 0, noMax)
	before := set.Counters()

	spare := format.SpareName(core.SegmentID(1))
	h := seam.HoldFirst(t, journaltest.OpAllocate, spare)
	errc := make(chan error, 1)
	go func() { _, err := m.Make(); errc <- err }()
	awaitReached(t, journaltest.OpAllocate, h, errc)

	c := set.Counters()
	if c.UsedBytes != before.UsedBytes+segCap {
		t.Fatalf("while the allocation is held, the footprint is %d, want the cap %d reserved before it: it must be raised before the allocation happens",
			c.UsedBytes, before.UsedBytes+segCap)
	}
	if c.Spares != before.Spares+1 {
		t.Fatalf("while the allocation is held, the ledger holds %d spares, want %d", c.Spares, before.Spares+1)
	}

	h.Release()
	if err := <-errc; err != nil {
		t.Fatalf("the released Make: %v", err)
	}
}

// TestFailedMakeUnlinksBothNamesOrStaysAccounted walks every step a Make can
// fail at, under ENOSPC and EIO, with the cleanup's own remove succeeding
// and failing. A remove that succeeds leaves no file under either name, the
// reservation returned; a remove that fails leaves the spare in the set,
// End-ed at its cap, for a retirement to unlink. It catches a Make that
// returns the reservation before the unlink has happened. The rename that
// applied and still failed is TestFailedMakeUnlinksBothNamesAfterTheAppliedRename's,
// not a rule's.
func TestFailedMakeUnlinksBothNamesOrStaysAccounted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	type step struct {
		op   string
		key  string // the spare's name, except the directory sync's own key
		name string // the step's own name the error carries
		errs []error
	}

	storageErrs := []error{syscall.ENOSPC, syscall.EIO}

	spare := format.SpareName(core.SegmentID(1))
	steps := []step{
		{journaltest.OpCreate, spare, "create", storageErrs},
		{journaltest.OpAllocate, spare, "allocate", storageErrs},
		{journaltest.OpSync, spare, "sync", storageErrs},
		{journaltest.OpClose, spare, "close", storageErrs},
		{journaltest.OpRename, spare, "rename", storageErrs},
		{journaltest.OpSyncDir, "*", "sync_dir", storageErrs},
	}

	for _, st := range steps {
		for _, failWith := range st.errs {
			for _, removeFails := range []bool{false, true} {
				name := fmt.Sprintf("%s under %v removeFails=%v", st.op, failWith, removeFails)
				t.Run(name, func(t *testing.T) {
					seam, set, m := maker(t, 0, noMax)
					before := set.Counters()

					rules := []journaltest.Rule{
						journaltest.Failing(st.op, st.key, 1, failWith),
					}
					if removeFails {
						rules = append(rules, journaltest.Failing(journaltest.OpRemove, "*", 0, syscall.EIO))
					}
					if err := seam.Schedule(rules...); err != nil {
						t.Fatalf("Schedule: %v", err)
					}

					id, err := m.Make()
					if err == nil {
						t.Fatalf("a Make failed at %s: no error", st.op)
					}
					if !errors.Is(err, failWith) {
						t.Fatalf("a Make failed at %s with %v, want the step's %v in it", st.op, err, failWith)
					}
					// The error names the step that failed, so the pool's
					// log says which call to act on; the plan's format, one
					// prefix, one step, no repeats.
					wantMsg := fmt.Sprintf("segment: making 1: %s: ", st.name)
					if !strings.HasPrefix(err.Error(), wantMsg) {
						t.Fatalf("a Make failed at %s with %q, want it to begin %q", st.op, err.Error(), wantMsg)
					}
					if n := strings.Count(err.Error(), "segment: making"); n != 1 {
						t.Fatalf("a Make failed at %s with %q, the prefix appears %d times, want once", st.op, err.Error(), n)
					}
					if id != 0 {
						t.Fatalf("a failed Make returned id %d, want 0", id)
					}
					if set.Check() != nil {
						t.Fatalf("a Make failed at %s: %v", st.op, set.Check())
					}

					segs, spares := classify(listNames(t, seam))
					if removeFails {
						c := set.Counters()
						if c.UsedBytes != before.UsedBytes+segCap {
							t.Fatalf("a leftover: the footprint is %d, want the cap %d held to the retirement",
								c.UsedBytes, before.UsedBytes+segCap)
						}
						if c.Segments != before.Segments+1 {
							t.Fatalf("a leftover: the set holds %d segments, want the spare End-ed as a segment at its cap",
								c.Segments)
						}
						if c.Spares != before.Spares {
							t.Fatalf("a leftover: the set holds %d spares, want %d", c.Spares, before.Spares)
						}
						if err := set.Take(core.SegmentID(1)); err == nil {
							t.Fatal("a failed spare was taken: it must never be")
						}
						freed, rerr := set.Retire(core.SegmentID(1))
						if freed != segCap || rerr != nil {
							t.Fatalf("the leftover's retire freed %d, %v, want the whole cap %d, nil: it was left in the set, End-ed, for exactly this",
								freed, rerr, segCap)
						}
						if len(segs)+len(spares) > 1 {
							t.Fatalf("the failed Make left %v and %v: one remove failed, so at most one name may survive", segs, spares)
						}
						// The retire freed the cap through the ledger alone.
						// Every step but the create leaves exactly one file on
						// disk — the spare's own name up to the rename, the
						// segment's from it on — still waiting for the
						// retirement executor's unlink, which must try both of
						// the id's format names, for the ledger that just freed
						// the cap keeps no name. A create that failed owns no
						// file at all, and its leftover is ledger-only.
						if st.op != journaltest.OpCreate && len(segs)+len(spares) != 1 {
							t.Fatalf("after the retire the directory holds %v and %v, want the one file the failed Make could not unlink", segs, spares)
						}
						return
					}

					if len(segs) != 0 || len(spares) != 0 {
						t.Fatalf("the failed Make left the segments %v and the spares %v, want nothing under either name", segs, spares)
					}
					if c := set.Counters(); c != before {
						t.Fatalf("after the failed Make the counters are %+v, want %+v: the reservation was returned",
							c, before)
					}
					if freed, rerr := set.Retire(core.SegmentID(1)); freed != 0 || !errors.Is(rerr, segment.ErrNotInSet) {
						t.Fatalf("after the returned reservation a retire of %d freed %d, %v, want nothing and %v",
							core.SegmentID(1), freed, rerr, segment.ErrNotInSet)
					}
				})
			}
		}
	}

	// The close of a failed attempt can fail of its own accord, joined onto
	// the step that broke the sequence first. Whichever call failed first
	// keeps its label: the error names the step that acted, not the one that
	// followed. This row fails allocate and close together; the sync row
	// fails sync and close, and the close-alone case is already in the table.
	t.Run("allocate and close failing together", func(t *testing.T) {
		seam, set, m := maker(t, 0, noMax)
		before := set.Counters()

		spare := format.SpareName(core.SegmentID(1))
		rules := []journaltest.Rule{
			journaltest.Failing(journaltest.OpAllocate, spare, 1, syscall.EIO),
			journaltest.Failing(journaltest.OpClose, spare, 1, syscall.EIO),
		}
		if err := seam.Schedule(rules...); err != nil {
			t.Fatalf("Schedule: %v", err)
		}

		_, err := m.Make()
		if err == nil {
			t.Fatal("a Make with both allocate and close failing: no error")
		}
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("a Make with both failing returned %v, want the EIO in it", err)
		}
		if wantMsg := "segment: making 1: allocate: "; !strings.HasPrefix(err.Error(), wantMsg) {
			t.Fatalf("a Make whose allocate failed first is named %q, want it to begin %q: the close that failed after it cannot claim the label", err.Error(), wantMsg)
		}
		if _, spares := classify(listNames(t, seam)); len(spares) != 0 {
			t.Fatalf("the failed Make left the spares %v, want none", spares)
		}
		if c := set.Counters(); c != before {
			t.Fatalf("after the failed Make the counters are %+v, want %+v", c, before)
		}
	})
	t.Run("sync and close failing together", func(t *testing.T) {
		seam, set, m := maker(t, 0, noMax)
		before := set.Counters()

		spare := format.SpareName(core.SegmentID(1))
		rules := []journaltest.Rule{
			journaltest.Failing(journaltest.OpSync, spare, 1, syscall.EIO),
			journaltest.Failing(journaltest.OpClose, spare, 1, syscall.EIO),
		}
		if err := seam.Schedule(rules...); err != nil {
			t.Fatalf("Schedule: %v", err)
		}

		_, err := m.Make()
		if err == nil {
			t.Fatal("a Make with both sync and close failing: no error")
		}
		if wantMsg := "segment: making 1: sync: "; !strings.HasPrefix(err.Error(), wantMsg) {
			t.Fatalf("a Make whose sync failed first is named %q, want it to begin %q: the close that failed after it cannot claim the label", err.Error(), wantMsg)
		}
		if c := set.Counters(); c != before {
			t.Fatalf("after the failed Make the counters are %+v, want %+v", c, before)
		}
	})
}

// TestFailedMakeReturnsTheReservationAfterTheUnlink checks the R52 tail of a
// failed make: the cap stays accounted while the file is still unlinked —
// held at its remove — and only once the remove has happened is the
// reservation returned. It catches a Make that returns the reservation
// before the unlink, whose footprint would run below the storage still in
// use.
func TestFailedMakeReturnsTheReservationAfterTheUnlink(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, set, m := maker(t, 0, noMax)
	before := set.Counters()

	spare := format.SpareName(core.SegmentID(1))
	if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, spare, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	h := seam.HoldFirst(t, journaltest.OpRemove, "*")
	errc := make(chan error, 1)
	go func() { _, err := m.Make(); errc <- err }()
	awaitReached(t, journaltest.OpRemove, h, errc)

	c := set.Counters()
	if c.UsedBytes != before.UsedBytes+segCap {
		t.Fatalf("while the unlink is held, the footprint is %d, want the cap %d still held: the reservation returns only after the unlink",
			c.UsedBytes, before.UsedBytes+segCap)
	}

	h.Release()
	if err := <-errc; err == nil || !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Make returned %v, want the allocation's EIO", err)
	}
	if c := set.Counters(); c != before {
		t.Fatalf("after the unlink the counters are %+v, want %+v: the reservation was returned", c, before)
	}
}

// TestFailedMakeUnlinksBothNamesAfterTheAppliedRename checks the cleanup's
// second name at the one step that can leave a file under it without the
// rename having reported success: a storage Rename that moved the file and
// still failed. The seam's own failing rule refuses the call before anything
// moves, which the matrix already rows over; this test drives the applier
// wrapper, whose Rename calls through first. The cleanup must remove under
// both names before the reservation returns, and with the remove failing the
// leftover is the `.seg` — a whole segment, held at its cap in the ledger
// until a retirement unlinks it — never storage the ledger accounts for
// nothing.
func TestFailedMakeUnlinksBothNamesAfterTheAppliedRename(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, removeFails := range []bool{false, true} {
		name := fmt.Sprintf("removeFails=%v", removeFails)
		t.Run(name, func(t *testing.T) {
			seam := newSeam(t)
			set := segment.NewSet(segCap, nil)
			applier := &renameApplier{FS: seam.FS(), removeFails: removeFails}
			m := segment.NewMaker(applier, set, noMax, 0, nil)
			before := set.Counters()

			id, err := m.Make()
			if err == nil || !errors.Is(err, syscall.EIO) {
				t.Fatalf("the Make whose rename applied and failed returned (%d, %v), want the rename's EIO", id, err)
			}
			if id != 0 {
				t.Fatalf("the failed Make returned id %d, want 0", id)
			}
			if !strings.HasPrefix(err.Error(), "segment: making 1: rename: ") {
				t.Fatalf("the failed Make's error is %q, want it to begin %q", err.Error(),
					"segment: making 1: rename: ")
			}
			if set.Check() != nil {
				t.Fatalf("after the failed Make: %v", set.Check())
			}

			segs, spares := classify(listNames(t, seam))
			id1 := core.SegmentID(1)
			seg, spare := format.SegmentName(id1), format.SpareName(id1)

			if removeFails {
				// The `.seg` is the leftover: the rename put the file under
				// it, and the removal of both names failed.
				c := set.Counters()
				if c.UsedBytes != before.UsedBytes+segCap {
					t.Fatalf("the leftover: the footprint is %d, want the cap %d held to the retirement", c.UsedBytes, before.UsedBytes+segCap)
				}
				if c.Segments != before.Segments+1 || c.Spares != before.Spares {
					t.Fatalf("the leftover: the set holds %d segments and %d spares, want the spare End-ed as a segment and no spares", c.Segments, c.Spares)
				}
				if err := set.Take(id1); err == nil {
					t.Fatal("a failed spare was taken: it must never be")
				}
				freed, rerr := set.Retire(id1)
				if freed != segCap || rerr != nil {
					t.Fatalf("the leftover's retire freed %d, %v, want the whole cap %d, nil", freed, rerr, segCap)
				}
				// The retire freed the cap through the ledger alone: the file
				// the rename left under the segment name is still on disk,
				// waiting for the retirement executor's unlink — which must
				// try both of the id's format names, for the ledger keeps
				// none.
				if !slices.Contains(segs, seg) || len(segs) != 1 || len(spares) != 0 {
					t.Fatalf("the failed Make left %v and %v, want the renamed %s alone on disk after the ledger's retire", segs, spares, seg)
				}
				return
			}

			if len(segs) != 0 || len(spares) != 0 {
				t.Fatalf("the failed Make left the segments %v and the spares %v, want nothing under either %s or %s", segs, spares, seg, spare)
			}
			if c := set.Counters(); c != before {
				t.Fatalf("after the failed Make the counters are %+v, want %+v: the reservation was returned only after both names went", c, before)
			}
			if freed, rerr := set.Retire(id1); freed != 0 || !errors.Is(rerr, segment.ErrNotInSet) {
				t.Fatalf("after the returned reservation a retire of %d freed %d, %v, want nothing and %v", id1, freed, rerr, segment.ErrNotInSet)
			}
		})
	}
}

// TestFailedMakeWarnsAboutTheLeftover checks the plan's leftover log: a
// spare whose unlink failed is the one case a failed Make leaves in the
// ledger, and the operator reads of it from a Warn naming the id and the
// names the cleanup actually tried — never a name it never reached, which
// would point the operator at a file that is not there. Deleting the Warn
// would stay green against a nil-loggered Maker alone, so here the Maker
// logs into a text handler the test reads back.
func TestFailedMakeWarnsAboutTheLeftover(t *testing.T) {
	prof.Watch(t, prof.Options{})

	setup := func(failRename bool) (*journaltest.Seam, *segment.Set, *segment.Maker, *bytes.Buffer) {
		seam := newSeam(t)
		set := segment.NewSet(segCap, nil)
		var buf bytes.Buffer
		fsys := storage.FS(seam.FS())
		if failRename {
			fsys = &renameApplier{FS: seam.FS(), removeFails: true}
		} else {
			spare := format.SpareName(core.SegmentID(1))
			if err := seam.Schedule(
				journaltest.Failing(journaltest.OpAllocate, spare, 1, syscall.EIO),
				journaltest.Failing(journaltest.OpRemove, "*", 0, syscall.EIO),
			); err != nil {
				t.Fatalf("Schedule: %v", err)
			}
		}
		return seam, set, segment.NewMaker(fsys, set, noMax, 0, slog.New(slog.NewTextHandler(&buf, nil))), &buf
	}

	t.Run("before the rename, the spare's own name alone", func(t *testing.T) {
		_, _, m, buf := setup(false)
		if _, err := m.Make(); err == nil {
			t.Fatal("a Make whose cleanup could not unlink: no error")
		}

		spare := format.SpareName(core.SegmentID(1))
		if got, want := strings.Count(buf.String(), "level=WARN"), 1; got != want {
			t.Fatalf("the failed Make logged %d Warn records, want exactly one: a leftover is the one case the operator is told of", got)
		}
		for _, want := range []string{"id=1", "names=[", spare} {
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("the leftover Warn reads %q, want it to name %s: the one name the cleanup tried", buf.String(), want)
			}
		}
		if strings.Contains(buf.String(), format.SegmentName(core.SegmentID(1))) {
			t.Fatalf("the leftover Warn reads %q, want no segment name in it: the cleanup never tried the one before the rename", buf.String())
		}
	})

	t.Run("after the applied rename, both names", func(t *testing.T) {
		_, _, m, buf := setup(true)
		if _, err := m.Make(); err == nil {
			t.Fatal("a Make whose cleanup could not unlink: no error")
		}

		id1 := core.SegmentID(1)
		seg, spare := format.SegmentName(id1), format.SpareName(id1)
		log := buf.String()
		if got, want := strings.Count(log, "level=WARN"), 1; got != want {
			t.Fatalf("the failed Make logged %d Warn records, want exactly one: a leftover is the one case the operator is told of", got)
		}
		if !strings.Contains(log, "id=1") || !strings.Contains(log, spare) || !strings.Contains(log, seg) {
			t.Fatalf("the leftover Warn reads %q, want it to name both, %s and %s", log, spare, seg)
		}
	})
}

// TestNewMakerRefusesItsArguments checks the caller bugs NewMaker names as
// panics, as NewSet does: no storage, no ledger, a maximum that is not a
// size. A nil logger, by contrast, is a Maker that simply logs nothing.
func TestNewMakerRefusesItsArguments(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)

	for name, fn := range map[string]func(){
		"nil fs":    func() { segment.NewMaker(nil, set, noMax, 0, nil) },
		"nil set":   func() { segment.NewMaker(seam.FS(), nil, noMax, 0, nil) },
		"zero max":  func() { segment.NewMaker(seam.FS(), set, 0, 0, nil) },
		"minus max": func() { segment.NewMaker(seam.FS(), set, -1, 0, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewMaker(%s) returned, want the panic of a caller bug", name)
				}
			}()
			fn()
		})
	}
}
