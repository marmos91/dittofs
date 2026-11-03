package segment_test

import (
	"context"
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

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/format"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/segment"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
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
func newSeam(t *testing.T) *journalv2test.Seam {
	t.Helper()

	seam, err := journalv2test.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("NewSeam: %v", err)
	}
	return seam
}

// maker is a Maker over a fresh seam and a fresh set: every test's default.
// It gives Maker the seam's epoch-bound view, never the seam itself, so a
// crash the test makes ends the Maker's calls rather than silently acting on
// the directory the crash left.
func maker(t *testing.T, last core.SegmentID, max int64) (*journalv2test.Seam, *segment.Set, *segment.Maker) {
	t.Helper()

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)
	return seam, set, segment.NewMaker(seam.FS(), set, max, last, nil)
}

// logCapture is a slog Handler keeping every record handed to it, so a test
// can assert what the Maker logged and at which level. Nothing of the Make
// runs concurrently once its result has been read, so no mutex is held over
// the one goroutine that logs.
type logCapture struct {
	recs []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.recs = append(c.recs, r.Clone())
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *logCapture) WithGroup(string) slog.Handler { return c }

// warnings returns every record captured at the Warn level.
func (c *logCapture) warnings() (ws []slog.Record) {
	for _, r := range c.recs {
		if r.Level == slog.LevelWarn {
			ws = append(ws, r)
		}
	}
	return ws
}

// attrOf returns the value a record carries for key, or nil when it has none.
func attrOf(rec slog.Record, key string) any {
	var val any
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value.Any()
			return false
		}
		return true
	})
	return val
}

// awaitReached fails t unless the Make parked at hold reaches it: a
// regression that skips the held step altogether must fail the test on its
// own assertion, not hang until the whole binary is timed out. Make's
// result arrives on res instead; whichever comes first wins.
func awaitReached(t *testing.T, step string, hold *journalv2test.Hold, res <-chan error) {
	t.Helper()

	select {
	case <-hold.Reached():
	case err := <-res:
		t.Fatalf("the Make returned (%v) without ever reaching its %s step", err, step)
	}
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
func listNames(t *testing.T, seam *journalv2test.Seam) []string {
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
func assertWhole(t *testing.T, seam *journalv2test.Seam, name string) {
	t.Helper()

	f, err := seam.Open(name)
	if err != nil {
		t.Fatalf("whole %s: Open: %v", name, err)
	}
	defer f.Close()

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
		journalv2test.OpCreate + " " + spare,
		journalv2test.OpAllocate + " " + spare,
		journalv2test.OpSync + " " + spare,
		journalv2test.OpClose + " " + spare,
		journalv2test.OpRename + " " + spare,
		journalv2test.OpSyncDir + " ",
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
		{journalv2test.OpCreate, spare},
		{journalv2test.OpAllocate, spare},
		{journalv2test.OpSync, spare},
		{journalv2test.OpClose, spare},
		{journalv2test.OpRename, spare},
		{journalv2test.OpSyncDir, "*"},
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
	h := seam.HoldFirst(t, journalv2test.OpRename, spare)
	errc := make(chan error, 1)
	go func() { errc <- errOf(m.Make()) }()
	awaitReached(t, journalv2test.OpRename, h, errc)

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

	if calls := seam.Calls(journalv2test.OpCreate); calls != fitted {
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
	h := seam.HoldFirst(t, journalv2test.OpAllocate, spare)
	errc := make(chan error, 1)
	go func() { _, err := m.Make(); errc <- err }()
	awaitReached(t, journalv2test.OpAllocate, h, errc)

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
// End-ed at its cap, for a retirement to unlink — and the sparing engine
// never unlinks a name it failed to create. It catches a Make that returns
// the reservation before the unlink has happened, one that forgets the
// second name after a rename-half-done, and one that removes a name an
// earlier failure kept.
func TestFailedMakeUnlinksBothNamesOrStaysAccounted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	type step struct {
		op   string
		key  string // the spare's name, except the directory sync's own key
		name string // the step's own name the error carries
		errs []error
	}

	// The create step takes no EEXIST: a create refused with it is a
	// different path — a name the attempt does not own, never removed
	// (TestMakeNeverRemovesANameItDidNotCreate) — while the steps with a
	// file of their own must unlink it even when the refusal the platform
	// hands them is an exists-class one.
	storageErrs := []error{syscall.ENOSPC, syscall.EIO}
	withExist := []error{syscall.ENOSPC, syscall.EIO, syscall.EEXIST}

	spare := format.SpareName(core.SegmentID(1))
	steps := []step{
		{journalv2test.OpCreate, spare, "create", storageErrs},
		{journalv2test.OpAllocate, spare, "allocate", withExist},
		{journalv2test.OpSync, spare, "sync", withExist},
		{journalv2test.OpClose, spare, "close", withExist},
		{journalv2test.OpRename, spare, "rename", storageErrs},
		{journalv2test.OpSyncDir, "*", "sync_dir", storageErrs},
	}

	for _, st := range steps {
		for _, failWith := range st.errs {
			for _, removeFails := range []bool{false, true} {
				name := fmt.Sprintf("%s under %v removeFails=%v", st.op, failWith, removeFails)
				t.Run(name, func(t *testing.T) {
					seam, set, m := maker(t, 0, noMax)
					before := set.Counters()

					rules := []journalv2test.Rule{
						journalv2test.Failing(st.op, st.key, 1, failWith),
					}
					if removeFails {
						rules = append(rules, journalv2test.Failing(journalv2test.OpRemove, "*", 0, syscall.EIO))
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
	if err := seam.Schedule(journalv2test.Failing(journalv2test.OpAllocate, spare, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	h := seam.HoldFirst(t, journalv2test.OpRemove, "*")
	errc := make(chan error, 1)
	go func() { _, err := m.Make(); errc <- err }()
	awaitReached(t, journalv2test.OpRemove, h, errc)

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

// TestFailedMakeWarnsAboutTheLeftover checks the plan's leftover log: a
// spare whose unlink failed is the one case a failed Make leaves in the
// ledger, and the operator reads of it from a Warn naming the id. Deleting
// the Warn would stay green against a nil-loggered Maker alone, so here the
// Maker logs into a capture.
func TestFailedMakeWarnsAboutTheLeftover(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)
	var logs logCapture
	m := segment.NewMaker(seam.FS(), set, noMax, 0, slog.New(&logs))

	spare := format.SpareName(core.SegmentID(1))
	rules := []journalv2test.Rule{
		// The remove fails for every call, so the leftover is left whichever
		// names the cleanup reached.
		journalv2test.Failing(journalv2test.OpAllocate, spare, 1, syscall.EIO),
		journalv2test.Failing(journalv2test.OpRemove, "*", 0, syscall.EIO),
	}
	if err := seam.Schedule(rules...); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	if _, err := m.Make(); err == nil {
		t.Fatal("a Make whose cleanup could not unlink: no error")
	}

	warns := logs.warnings()
	if len(warns) != 1 {
		t.Fatalf("the failed Make logged %d Warn records, want exactly one: a leftover is the one case the operator is told of", len(warns))
	}
	if id, ok := attrOf(warns[0], "id").(uint64); !ok || id != 1 {
		t.Fatalf("the leftover Warn carries the id %v, want 1", attrOf(warns[0], "id"))
	}
	if spareAttr, _ := attrOf(warns[0], "spare").(string); spareAttr != spare {
		t.Fatalf("the leftover Warn names the spare %v, want %s", attrOf(warns[0], "spare"), spare)
	}
}

// TestMakeNeverRemovesANameItDidNotCreate checks the one remove the failure
// path must never make: an existing `<id>.spare.tmp` means the id the Maker
// drew is one the directory already held, so removing it could unlink a live
// file. The Make fails, the file is untouched under its name, the reservation
// is returned — to the counters, not merely out of a retire's reach — and no
// remove is issued at all.
func TestMakeNeverRemovesANameItDidNotCreate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)

	const last = core.SegmentID(41)
	spare := format.SpareName(last + 1)

	// A foreign file under the name the next Make draws, holding a marker
	// byte: what the Maker must not unlink.
	f, err := seam.Create(spare)
	if err != nil {
		t.Fatalf("the pre-existing spare: Create: %v", err)
	}
	if _, err := f.WriteAt([]byte{0x5a}, 0); err != nil {
		t.Fatalf("the pre-existing spare: WriteAt: %v", err)
	}
	f.Close()

	var rec recording
	seam.SetFault(&rec)
	m := segment.NewMaker(seam.FS(), set, noMax, last, nil)

	if id, err := m.Make(); err == nil {
		t.Fatalf("a Make over an existing %s returned %d, nil, want the create's refusal", spare, id)
	}

	// The file is untouched, down to its marker byte.
	g, err := seam.Open(spare)
	if err != nil {
		t.Fatalf("the existing %s after the failed Make: Open: %v, want it still in place", spare, err)
	}
	defer g.Close()
	var one [1]byte
	if _, err := g.ReadAt(one[:], 0); err != nil {
		t.Fatalf("the existing %s: ReadAt: %v", spare, err)
	}
	if one[0] != 0x5a {
		t.Fatalf("the existing %s holds %#x at 0, want the marker byte %#x it was holding before", spare, one[0], 0x5a)
	}

	// The Maker removed nothing and asked the ledger for nothing it kept.
	for _, call := range rec.calls {
		if recOpOf(call) == journalv2test.OpRemove {
			t.Fatalf("the failed Make removed %s: a name it did not create must never be unlinked", spare)
		}
	}
	// The reservation is returned, not merely unretirable: a spare still
	// held would show in the counters, whatever a retire of it says.
	if c := set.Counters(); c != (segment.Counters{}) {
		t.Fatalf("after the failed Make the counters are %+v, want the empty ledger: the reservation was returned", c)
	}
}

// recOpOf splits a recording's call record back into its op half.
func recOpOf(call string) string {
	op, _, _ := strings.Cut(call, " ")
	return op
}

// TestFailedMakeNeverRemovesASegmentNameItDidNotCreate checks the `.seg` half
// of the never-remove rule: a failure at create, allocate, sync or close runs
// before the rename, so under that name the attempt owns nothing — a segment
// file the directory already holds there, with an id above the seed, is a
// live segment exactly as the `.spare.tmp` one in this test's twin is, and
// the cleanup must not unlink it. It catches a `fail` that removes both names
// whatever the failing step was.
func TestFailedMakeNeverRemovesASegmentNameItDidNotCreate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, st := range []struct{ op, step string }{
		{journalv2test.OpCreate, "create"},
		{journalv2test.OpAllocate, "allocate"},
		{journalv2test.OpSync, "sync"},
		{journalv2test.OpClose, "close"},
	} {
		t.Run(st.op, func(t *testing.T) {
			seam := newSeam(t)
			set := segment.NewSet(segCap, nil)

			// A foreign segment under the name the make never reaches unless
			// it renames: the `.seg` name of the id the Maker draws over a
			// zero seed.
			const last = core.SegmentID(0)
			seg := format.SegmentName(last + 1)
			spare := format.SpareName(last + 1)
			f, err := seam.Create(seg)
			if err != nil {
				t.Fatalf("the pre-existing segment: Create: %v", err)
			}
			if _, err := f.WriteAt([]byte{0x5a}, 0); err != nil {
				t.Fatalf("the pre-existing segment: WriteAt: %v", err)
			}
			f.Close()

			// The schedule fails the step on the spare's temporary name —
			// the file the make itself creates — which it shares with no
			// foreign file.
			if err := seam.Schedule(journalv2test.Failing(st.op, spare, 1, syscall.EIO)); err != nil {
				t.Fatalf("Schedule: %v", err)
			}
			m := makerOver(t, seam, set, last)

			if _, err := m.Make(); err == nil {
				t.Fatalf("the Make over the existing %s did not fail at %s, want the failing step", seg, st.op)
			}

			// The segment is untouched: it was never this attempt's to unlink.
			g, err := seam.Open(seg)
			if err != nil {
				t.Fatalf("the existing %s after the failed Make: Open: %v, want it still in place", seg, err)
			}
			defer g.Close()
			var one [1]byte
			if _, err := g.ReadAt(one[:], 0); err != nil {
				t.Fatalf("the existing %s: ReadAt: %v", seg, err)
			}
			if one[0] != 0x5a {
				t.Fatalf("the existing %s holds %#x at 0, want the marker byte %#x it held before", seg, one[0], 0x5a)
			}

			// And the spare this make did create is gone.
			if _, spares := classify(listNames(t, seam)); len(spares) != 0 {
				t.Fatalf("the failed Make left the spares %v, want none: its own file is unlinked", spares)
			}
			if c := set.Counters(); c.UsedBytes != 0 || c.Spares != 0 {
				t.Fatalf("after the failed Make the ledger holds %d bytes in %d spares, want none",
					c.UsedBytes, c.Spares)
			}
		})
	}
}

// makerOver builds a Maker directly on a seam and set the test prepared,
// seeded with the id the directory's names stop at, as open will.
func makerOver(t *testing.T, seam *journalv2test.Seam, set *segment.Set, last core.SegmentID) *segment.Maker {
	t.Helper()
	return segment.NewMaker(seam.FS(), set, noMax, last, nil)
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
