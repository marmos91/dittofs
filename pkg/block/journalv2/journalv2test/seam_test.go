package journalv2test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

func newSeam(t *testing.T) *Seam {
	t.Helper()
	s, err := NewSeam(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// create makes name, durable in the directory, and returns it open.
func create(t *testing.T, s *Seam, name string) storage.File {
	t.Helper()

	f, err := s.Create(name)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	return f
}

func write(t *testing.T, f storage.File, p string, off int64) {
	t.Helper()

	if _, err := f.WriteAt([]byte(p), off); err != nil {
		t.Fatal(err)
	}
}

func syncFile(t *testing.T, f storage.File) {
	t.Helper()

	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

func crash(t *testing.T, s *Seam, opts ...CrashOption) {
	t.Helper()

	if err := s.Crash(opts...); err != nil {
		t.Fatal(err)
	}
}

// onDisk returns what the real file holds, read past the seam.
func onDisk(t *testing.T, s *Seam, name string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func exists(s *Seam, name string) bool {
	_, err := os.Stat(filepath.Join(s.dir, name))
	return err == nil
}

func TestCrashKeepsSyncedWritesAndDropsTheRest(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "aaaa", 0)
	syncFile(t, f)
	write(t, f, "bb", 1)   // overwrites synced bytes
	write(t, f, "cccc", 4) // extends the file

	crash(t, s)

	if got := onDisk(t, s, "seg"); got != "aaaa" {
		t.Fatalf("after crash the file holds %q, want the synced %q", got, "aaaa")
	}
}

func TestCrashUndoesUnsyncedAllocate(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "ab", 0)
	syncFile(t, f)
	if err := f.Allocate(0, 1<<20); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if got := onDisk(t, s, "seg"); got != "ab" {
		t.Fatalf("an allocation never synced left %d bytes, want the 2 synced", len(got))
	}
}

func TestTruncateResizesTheFile(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "abcdef", 0)

	// Before any crash: the crash rebuilds the file from the undo log, which
	// would hide a Truncate that never reached it.
	for _, tc := range []struct {
		size int64
		want string
	}{
		{2, "ab"},
		{4, "ab\x00\x00"},
	} {
		if err := f.Truncate(tc.size); err != nil {
			t.Fatal(err)
		}

		if got, err := f.Size(); err != nil || got != tc.size {
			t.Fatalf("Size after Truncate(%d): %d, %v; want %d", tc.size, got, err, tc.size)
		}

		if got := onDisk(t, s, "seg"); got != tc.want {
			t.Fatalf("after Truncate(%d) the file holds %q, want %q", tc.size, got, tc.want)
		}
	}
}

func TestCrashUndoesUnsyncedTruncate(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "abcdef", 0)
	syncFile(t, f)
	if err := f.Truncate(2); err != nil {
		t.Fatal(err)
	}
	write(t, f, "XYZ", 2) // regrows over the cut bytes
	if err := f.Truncate(8); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if got := onDisk(t, s, "seg"); got != "abcdef" {
		t.Fatalf("after crash the file holds %q, want %q", got, "abcdef")
	}

	// A grow on its own file: above, the undo of the later write cuts the
	// file back anyway, which would hide a grow the seam never recorded.
	g := create(t, s, "grow")
	write(t, g, "ab", 0)
	syncFile(t, g)
	if err := g.Truncate(8); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if got := onDisk(t, s, "grow"); got != "ab" {
		t.Fatalf("after crash the grown file holds %q, want %q", got, "ab")
	}
}

func TestCrashDropsCreateWithoutSyncDir(t *testing.T) {
	s := newSeam(t)
	f, err := s.Create("seg")
	if err != nil {
		t.Fatal(err)
	}

	write(t, f, "data", 0)
	syncFile(t, f) // the data is durable, the directory entry is not

	crash(t, s)

	if exists(s, "seg") {
		t.Fatal("a file created without a SyncDir survived the crash")
	}

	if _, err := s.Open("seg"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open after the crash: %v, want fs.ErrNotExist", err)
	}
}

func TestRenameSurvivesOnlyAfterSyncDir(t *testing.T) {
	s := newSeam(t)
	a := create(t, s, "a")

	write(t, a, "new", 0)
	syncFile(t, a)

	b := create(t, s, "b")
	write(t, b, "old", 0)
	syncFile(t, b)

	if err := s.Rename("a", "b"); err != nil { // replaces b
		t.Fatal(err)
	}
	crash(t, s)

	if got := onDisk(t, s, "a"); got != "new" {
		t.Fatalf("after an unsynced rename, a holds %q, want %q", got, "new")
	}

	if got := onDisk(t, s, "b"); got != "old" {
		t.Fatalf("after an unsynced rename, b holds %q, want the file it replaced, %q", got, "old")
	}

	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if exists(s, "a") {
		t.Fatal("after a synced rename, a still exists")
	}

	if got := onDisk(t, s, "b"); got != "new" {
		t.Fatalf("after a synced rename, b holds %q, want %q", got, "new")
	}
}

func TestFailedRenameKeepsTheDestination(t *testing.T) {
	s := newSeam(t)
	a := create(t, s, "a")
	b := create(t, s, "b")

	write(t, a, "new", 0)
	syncFile(t, a)
	write(t, b, "old", 0)
	syncFile(t, b)

	// a vanishes under the seam, so the real rename fails after the seam has
	// already moved b, the file it would replace, to the stash.
	if err := os.Remove(filepath.Join(s.dir, "a")); err != nil {
		t.Fatal(err)
	}

	if err := s.Rename("a", "b"); err == nil {
		t.Fatal("Rename of a file removed under the seam succeeded, want it to fail")
	}

	if !exists(s, "b") {
		t.Fatal("after a failed rename b is missing, want it back in place")
	}

	if got := onDisk(t, s, "b"); got != "old" {
		t.Fatalf("after a failed rename b holds %q, want its own %q", got, "old")
	}

	crash(t, s)

	if !exists(s, "b") {
		t.Fatal("a crash after a failed rename lost b, want it kept")
	}

	if got := onDisk(t, s, "b"); got != "old" {
		t.Fatalf("a crash after a failed rename left b holding %q, want %q", got, "old")
	}
}

func TestCrashUndoesRemoveWithoutSyncDir(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "keep", 0)
	syncFile(t, f)
	if err := s.Remove("seg"); err != nil {
		t.Fatal(err)
	}

	if exists(s, "seg") {
		t.Fatal("Remove left the file in the directory")
	}

	// A crash now would bring the file back, so the seam must say so.
	if got, err := s.Durable("seg"); err != nil || string(got) != "keep" {
		t.Fatalf("Durable after an unsynced remove: %q, %v; want %q", got, err, "keep")
	}

	crash(t, s)

	if got := onDisk(t, s, "seg"); got != "keep" {
		t.Fatalf("after an unsynced remove, the file holds %q, want %q", got, "keep")
	}

	if names, err := s.List(); err != nil || !slices.Equal(names, []string{"seg"}) {
		t.Fatalf("List after the crash: %v, %v; want [seg]", names, err)
	}

	g, err := s.Open("seg")
	if err != nil {
		t.Fatalf("Open after the crash: %v, want the file back", err)
	}
	defer g.Close()

	p := make([]byte, 4)
	if _, err := g.ReadAt(p, 0); err != nil || string(p) != "keep" {
		t.Fatalf("ReadAt after the crash: %q, %v; want %q", p, err, "keep")
	}
}

func TestSyncDirCoversEveryPendingChange(t *testing.T) {
	s := newSeam(t)
	create(t, s, "c")
	create(t, s, "d")

	// Four directory changes pending at once, made durable by one SyncDir.
	for _, name := range []string{"a", "b"} {
		if _, err := s.Create(name); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Remove("c"); err != nil {
		t.Fatal(err)
	}

	if err := s.Rename("d", "e"); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	for _, name := range []string{"a", "b"} {
		if !exists(s, name) {
			t.Fatalf("%s, created before a SyncDir, is missing after the crash", name)
		}
	}

	if exists(s, "c") {
		t.Fatal("c, removed before a SyncDir, came back after the crash")
	}

	if !exists(s, "e") || exists(s, "d") {
		t.Fatal("the rename d to e, made before a SyncDir, was undone by the crash")
	}
}

func TestSyncDirEmptiesTheStash(t *testing.T) {
	s := newSeam(t)
	create(t, s, "gone")
	create(t, s, "a")
	create(t, s, "b")

	if err := s.Remove("gone"); err != nil {
		t.Fatal(err)
	}

	if err := s.Rename("a", "b"); err != nil { // replaces b
		t.Fatal(err)
	}

	stashed := func() int {
		t.Helper()

		entries, err := os.ReadDir(s.stash)
		if err != nil {
			t.Fatal(err)
		}

		return len(entries)
	}

	// check if the stash had the files before the SyncDir
	if n := stashed(); n != 2 {
		t.Fatalf("before the SyncDir the stash holds %d files, want the removed and the replaced one", n)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	// No crash first: a crash empties the stash on its own.
	if n := stashed(); n != 0 {
		t.Fatalf("after a SyncDir the stash holds %d files, want none", n)
	}
}

func TestSyncDirSucceedsWhenTheStashCannotBeEmptied(t *testing.T) {
	s := newSeam(t)
	create(t, s, "seg")

	if err := s.Remove("seg"); err != nil {
		t.Fatal(err)
	}

	// A non-empty directory in the stash: os.Remove refuses it, even as root.
	blocker := filepath.Join(s.stash, "blocker")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(blocker, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatalf("SyncDir with a stash it cannot empty: %v, want nil", err)
	}

	// The cleanup failure passes; the next Crash removes what it left.
	if err := os.Remove(filepath.Join(blocker, "f")); err != nil {
		t.Fatal(err)
	}

	crash(t, s)

	if exists(s, "seg") {
		t.Fatal("seg, removed before a SyncDir that returned nil, came back after the crash")
	}

	entries, err := os.ReadDir(s.stash)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Fatalf("after the crash the stash holds %d entries, want none", len(entries))
	}
}

func TestTornKeepsExactlyNBytesOfTheLastWrite(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "aaaa", 0)
	syncFile(t, f)

	g := create(t, s, "other")
	write(t, f, "bbbb", 4) // lost: not the last write
	write(t, g, "cccc", 0) // the last write, torn

	crash(t, s, Torn(3))

	if got := onDisk(t, s, "seg"); got != "aaaa" {
		t.Fatalf("seg holds %q, want only its synced %q", got, "aaaa")
	}

	if got := onDisk(t, s, "other"); got != "ccc" {
		t.Fatalf("the torn write left %q, want its first 3 bytes %q", got, "ccc")
	}
}

func TestTornDropsEarlierWritesOfTheTornFile(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "aaaa", 0)
	syncFile(t, f)
	write(t, f, "bb", 0)   // lost: an earlier write to the torn file
	write(t, f, "cccc", 4) // the last write, torn

	crash(t, s, Torn(2))

	if got := onDisk(t, s, "seg"); got != "aaaacc" {
		t.Fatalf("seg holds %q, want the synced %q and the torn %q only", got, "aaaa", "cc")
	}
}

func TestReorderedNeverPutsAnOlderWriteOverANewerOne(t *testing.T) {
	// Two overlapping writes: older "aaaaaaaa" at 0, newer "bbbbbbbb" at 4.
	// A device may keep either, both or neither, but where both are kept the
	// newer must win in the overlap.
	allowed := map[string]string{
		"":                         "neither",
		"aaaaaaaa":                 "older only",
		"\x00\x00\x00\x00bbbbbbbb": "newer only",
		"aaaabbbbbbbb":             "both",
	}
	seen := map[string]bool{}
	for seed := uint64(0); seed < 64; seed++ {
		s := newSeam(t)
		f := create(t, s, "seg")

		syncFile(t, f)
		write(t, f, "aaaaaaaa", 0)
		write(t, f, "bbbbbbbb", 4)

		crash(t, s, Reordered(seed))

		got := onDisk(t, s, "seg")
		if _, ok := allowed[got]; !ok {
			t.Fatalf("seed %d left %q, an order no device produces", seed, got)
		}

		seen[allowed[got]] = true
	}
	if !seen["newer only"] {
		t.Fatal("no seed kept the newer write without the older one, so nothing was reordered")
	}
}

func TestReorderedKeepsTheSameWritesForTheSameSeed(t *testing.T) {
	// Several files, so a draw that followed map order would differ between
	// runs of one seed.
	names := []string{"a", "b", "c", "d"}
	run := func(seed uint64) string {
		s := newSeam(t)
		files := make([]storage.File, len(names))
		for i, name := range names {
			files[i] = create(t, s, name)
		}
		for off := range int64(8) {
			for _, f := range files {
				write(t, f, "x", off)
			}
		}

		crash(t, s, Reordered(seed))

		var got string
		for _, name := range names {
			got += name + "=" + onDisk(t, s, name) + ";"
		}
		return got
	}

	for seed := range uint64(8) {
		want := run(seed)
		for range 10 {
			if got := run(seed); got != want {
				t.Fatalf("seed %d kept %q, then %q", seed, want, got)
			}
		}
	}
}

func TestReorderedNeverLosesASyncedWrite(t *testing.T) {
	for seed := range uint64(64) {
		s := newSeam(t)
		f := create(t, s, "seg")
		g := create(t, s, "other")

		write(t, f, "synced", 0)
		syncFile(t, f)
		write(t, f, "after", 6)
		write(t, g, "elsewhere", 0)

		crash(t, s, Reordered(seed))

		if got := onDisk(t, s, "seg"); len(got) < 6 || got[:6] != "synced" {
			t.Fatalf("seed %d left %q, want it to start with the synced %q", seed, got, "synced")
		}
	}
}

func TestDurableAndStaleReads(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "synced", 0)
	syncFile(t, f)
	write(t, f, "UNSYNC", 0)

	got, err := s.Durable("seg")
	if err != nil || string(got) != "synced" {
		t.Fatalf("Durable: %q, %v; want %q", got, err, "synced")
	}

	p := make([]byte, 6)
	if _, err := f.ReadAtDirect(p, 0); err != nil || string(p) != "synced" {
		t.Fatalf("ReadAtDirect: %q, %v; want the durable %q", p, err, "synced")
	}

	if _, err := f.ReadAt(p, 0); err != nil || string(p) != "UNSYNC" {
		t.Fatalf("ReadAt: %q, %v; want the written %q", p, err, "UNSYNC")
	}

	s.StaleReads("seg")
	if _, err := f.ReadAt(p, 0); err != nil || string(p) != "synced" {
		t.Fatalf("ReadAt with stale reads: %q, %v; want the durable %q", p, err, "synced")
	}

	if _, err := s.Create("new"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Durable("new"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Durable of a file created without a SyncDir: %v, want fs.ErrNotExist", err)
	}
}

func TestStaleReadsCoversHandlesOpenedLater(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "synced", 0)
	syncFile(t, f)
	write(t, f, "UNSYNC", 0)

	s.StaleReads("seg")

	g, err := s.Open("seg")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	p := make([]byte, 6)
	if _, err := g.ReadAt(p, 0); err != nil || string(p) != "synced" {
		t.Fatalf("ReadAt on a handle opened after StaleReads: %q, %v; want the durable %q", p, err, "synced")
	}
}

func TestDurableReadsOfAnUnlinkedFile(t *testing.T) {
	// Both ways a file leaves the directory while open: Remove, and being
	// replaced by a Rename. SyncDir then unlinks it for good.
	for _, tc := range []struct {
		how    string
		unlink func(s *Seam) error
	}{
		{"removed", func(s *Seam) error { return s.Remove("seg") }},
		{"replaced by a rename", func(s *Seam) error {
			create(t, s, "other")
			return s.Rename("other", "seg")
		}},
	} {
		s := newSeam(t)
		f := create(t, s, "seg")

		write(t, f, "synced", 0)
		syncFile(t, f)
		write(t, f, "UNSYNC", 0)
		s.StaleReads("seg")

		if err := tc.unlink(s); err != nil {
			t.Fatal(err)
		}

		if err := s.SyncDir(); err != nil {
			t.Fatal(err)
		}

		p := make([]byte, 6)
		if _, err := f.ReadAtDirect(p, 0); err != nil || string(p) != "synced" {
			t.Fatalf("ReadAtDirect on an open file %s: %q, %v; want the durable %q", tc.how, p, err, "synced")
		}

		if _, err := f.ReadAt(p, 0); err != nil || string(p) != "synced" {
			t.Fatalf("stale ReadAt on an open file %s: %q, %v; want the durable %q", tc.how, p, err, "synced")
		}
	}
}

func TestDurableReadsRejectANegativeOffset(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "ab", 0)
	syncFile(t, f)
	s.StaleReads("seg")

	for _, tc := range []struct {
		how  string
		read func(p []byte, off int64) (int, error)
	}{
		{"ReadAtDirect", f.ReadAtDirect},
		{"stale ReadAt", f.ReadAt},
	} {
		// A panic is caught so it fails here, as this test's own failure.
		var err error
		panicked := func() (p any) {
			defer func() { p = recover() }()
			_, err = tc.read(make([]byte, 1), -1)
			return nil
		}()

		if panicked != nil {
			t.Fatalf("%s at offset -1 panicked: %v; want the error os.File.ReadAt returns", tc.how, panicked)
		}

		// os.File.ReadAt's own error for a negative offset.
		var pe *fs.PathError
		if !errors.As(err, &pe) || pe.Op != "readat" || pe.Err.Error() != "negative offset" {
			t.Fatalf("%s at offset -1: %v; want a readat PathError for a negative offset", tc.how, err)
		}
	}
}

func TestCrashClosesEveryOpenFile(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	crash(t, s)

	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("WriteAt on a file opened before the crash: %v, want fs.ErrClosed", err)
	}
	g, err := s.Open("seg")
	if err != nil {
		t.Fatalf("reopening after the crash: %v", err)
	}
	write(t, g, "ok", 0)
}

func TestCallsAreCountedPerOperationAndFile(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "a")
	g := create(t, s, "b")
	write(t, f, "x", 0)
	write(t, f, "y", 1)
	write(t, g, "z", 0)
	syncFile(t, f)

	if got := s.Calls(OpWrite); got != 3 {
		t.Fatalf("Calls(write) = %d, want 3", got)
	}

	if got := s.CallsFor(OpWrite, "a"); got != 2 {
		t.Fatalf("CallsFor(write, a) = %d, want 2", got)
	}

	if got := s.CallsFor(OpSync, "b"); got != 0 {
		t.Fatalf("CallsFor(sync, b) = %d, want 0", got)
	}

	if got := s.Calls(OpSyncDir); got != 2 {
		t.Fatalf("Calls(sync_dir) = %d, want 2", got)
	}
}

func TestRefusedCrashChangesNothing(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	syncFile(t, f)

	if err := s.Crash(Torn(1)); err == nil {
		t.Fatal("Crash(Torn) with no unsynced write succeeded")
	}

	// The refused crash must leave the file open and its writes unsynced.
	write(t, f, "after", 0)
	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "" {
		t.Fatalf("after a refused crash and a real one, seg holds %q; want it empty", got)
	}
}

func TestCrashRefusesTornWithReordered(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "aaaa", 0)
	syncFile(t, f)
	write(t, f, "bbbb", 0)

	if err := s.Crash(Torn(1), Reordered(0)); err == nil {
		t.Fatal("Crash(Torn, Reordered) succeeded, want it refused")
	}

	// The refused crash must leave the file open and its write in place.
	if got := onDisk(t, s, "seg"); got != "bbbb" {
		t.Fatalf("after a refused crash seg holds %q, want the unsynced %q", got, "bbbb")
	}

	if _, err := f.WriteAt([]byte("c"), 0); err != nil {
		t.Fatalf("WriteAt after a refused crash: %v, want the file still open", err)
	}
}

func TestCrashRefusesTornOutOfRange(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "aaaa", 0)
	syncFile(t, f)
	write(t, f, "bbbb", 0)

	for _, n := range []int{5, -1} {
		// Catch a panic so this test reports it, instead of the whole test binary crashing.
		var err error
		panicked := func() (p any) {
			defer func() {
				p = recover()
			}()

			err = s.Crash(Torn(n))
			return nil
		}()

		if panicked != nil {
			t.Fatalf("Crash(Torn(%d)) of a 4-byte write panicked: %v; want it refused", n, panicked)
		}

		if err == nil {
			t.Fatalf("Crash(Torn(%d)) of a 4-byte write succeeded; want it refused", n)
		}
	}

	// The refused crashes must leave the file open and its write in place.
	if got := onDisk(t, s, "seg"); got != "bbbb" {
		t.Fatalf("after a refused crash seg holds %q, want the unsynced %q", got, "bbbb")
	}

	if _, err := f.WriteAt([]byte("c"), 0); err != nil {
		t.Fatalf("WriteAt after a refused crash: %v, want the file still open", err)
	}
}

func TestEmptyWritePastTheEndIsNotRecorded(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "ab", 0)
	syncFile(t, f)

	if n, err := f.WriteAt(nil, 16); n != 0 || err != nil {
		t.Fatalf("WriteAt(nil, 16): %d, %v; want 0, nil", n, err)
	}

	// A panic is caught so it fails here, as this test's own failure.
	recovered := func(fn func()) (p any) {
		defer func() { p = recover() }()
		fn()
		return nil
	}

	var got []byte
	var err error
	if p := recovered(func() { got, err = s.Durable("seg") }); p != nil {
		t.Fatalf("Durable after an empty write past the end panicked: %v; want %q", p, "ab")
	}

	if err != nil || string(got) != "ab" {
		t.Fatalf("Durable after an empty write past the end: %q, %v; want %q", got, err, "ab")
	}

	if p := recovered(func() { err = s.Crash() }); p != nil {
		t.Fatalf("Crash after an empty write past the end panicked: %v; want it to succeed", p)
	}

	if err != nil {
		t.Fatalf("Crash after an empty write past the end: %v; want it to succeed", err)
	}

	if got := onDisk(t, s, "seg"); got != "ab" {
		t.Fatalf("after the crash seg holds %q (%d bytes), want the synced %q", got, len(got), "ab")
	}
}

func TestTornZeroLeavesTheSizeAlone(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")

	write(t, f, "ab", 0)
	syncFile(t, f)
	write(t, f, "cccc", 8) // past the end: keeping none of it must not grow the file

	crash(t, s, Torn(0))

	if got := onDisk(t, s, "seg"); got != "ab" {
		t.Fatalf("Torn(0) of a write past the end left %q (%d bytes), want the synced %q", got, len(got), "ab")
	}
}

func TestTornAcceptsTheWholeRange(t *testing.T) {
	// Torn(n) is valid from 0 to the write's length: 0 keeps none of the last
	// write, its length keeps all of it.
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "aaaa"},
		{4, "bbbb"},
	} {
		s := newSeam(t)
		f := create(t, s, "seg")

		write(t, f, "aaaa", 0)
		syncFile(t, f)
		write(t, f, "bbbb", 0)

		if err := s.Crash(Torn(tc.n)); err != nil {
			t.Fatalf("Crash(Torn(%d)) of a 4-byte write: %v, want it accepted", tc.n, err)
		}

		if got := onDisk(t, s, "seg"); got != tc.want {
			t.Fatalf("Crash(Torn(%d)) of a 4-byte write left %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFailedWriteLeavesNoUndo(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "ab", 0)
	syncFile(t, f)

	if _, err := f.WriteAt([]byte("xy"), 1<<63-1); err == nil {
		t.Fatal("a write at the largest offset succeeded")
	}

	got := make([]byte, 2)
	if _, err := f.ReadAtDirect(got, 0); err != nil || string(got) != "ab" {
		t.Fatalf("ReadAtDirect after a failed write: %q, %v; want %q", got, err, "ab")
	}
}
