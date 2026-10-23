package journalv2test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/sim"
)

// faultFunc is a Fault written inline.
type faultFunc func(op string, key []byte) sim.FaultAction

func (f faultFunc) At(op string, key []byte) sim.FaultAction { return f(op, key) }

var eio = sim.FaultAction{
	Kind: sim.Fail,
	Err:  syscall.EIO,
}

// failOn makes every later call of op fail with EIO.
func failOn(t *testing.T, s *Seam, op string) {
	t.Helper()

	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: op, Key: "*", Action: eio})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)
}

func wantEIO(t *testing.T, op string, err error) {
	t.Helper()

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("%s under a failing fault: %v, want EIO", op, err)
	}
}

// waitPending waits, in real time, until something waits on c.
func waitPending(t *testing.T, c *sim.TestClock) {
	t.Helper()

	for deadline := time.Now().Add(2 * time.Second); c.Pending() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("nothing waited on the clock")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFaultSeesEachOpAndItsKey(t *testing.T) {
	s := newSeam(t)

	var got []string
	s.SetFault(faultFunc(func(op string, key []byte) sim.FaultAction {
		got = append(got, op+":"+string(key))
		return sim.FaultAction{}
	}))

	f, err := s.Create("a")
	if err != nil {
		t.Fatal(err)
	}

	write(t, f, "data", 0)
	if _, err := f.ReadAt(make([]byte, 1), 0); err != nil {
		t.Fatal(err)
	}

	if _, err := f.ReadAtDirect(make([]byte, 1), 0); err != nil && !errors.Is(err, io.EOF) { // nothing synced yet
		t.Fatal(err)
	}

	if err := f.Allocate(0, 8); err != nil {
		t.Fatal(err)
	}

	if err := f.Truncate(4); err != nil {
		t.Fatal(err)
	}

	syncFile(t, f)
	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}

	g, err := s.Open("b")
	if err != nil {
		t.Fatal(err)
	}

	if err := g.Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove("b"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"create:a", "write:a", "read:a", "read_direct:a", "allocate:a",
		"truncate:a", "sync:a", "rename:a", "list:", "sync_dir:",
		"open:b", "close:b", "remove:b",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("the fault saw\n%v\nwant\n%v", got, want)
	}
}

func TestFailedOpReturnsItsErrorAndDoesNothing(t *testing.T) {
	cases := map[string]func(t *testing.T, s *Seam){
		OpCreate: func(t *testing.T, s *Seam) {
			failOn(t, s, OpCreate)
			_, err := s.Create("new")
			wantEIO(t, OpCreate, err)

			if exists(s, "new") {
				t.Fatal("a failed Create made the file")
			}
		},
		OpOpen: func(t *testing.T, s *Seam) {
			create(t, s, "seg")
			before := len(s.open)

			failOn(t, s, OpOpen)
			_, err := s.Open("seg")
			wantEIO(t, OpOpen, err)

			if len(s.open) != before {
				t.Fatal("a failed Open left a handle open")
			}
		},
		OpWrite: func(t *testing.T, s *Seam) {
			f := create(t, s, "seg")
			write(t, f, "aaaa", 0)
			syncFile(t, f)

			failOn(t, s, OpWrite)
			_, err := f.WriteAt([]byte("bbbb"), 0)
			wantEIO(t, OpWrite, err)

			if got := onDisk(t, s, "seg"); got != "aaaa" {
				t.Fatalf("after a failed write the file holds %q, want %q", got, "aaaa")
			}
		},
		OpRead: func(t *testing.T, s *Seam) {
			f := create(t, s, "seg")
			write(t, f, "aaaa", 0)

			failOn(t, s, OpRead)
			_, err := f.ReadAt(make([]byte, 4), 0)
			wantEIO(t, OpRead, err)
		},
		OpReadDirect: func(t *testing.T, s *Seam) {
			f := create(t, s, "seg")
			write(t, f, "aaaa", 0)
			syncFile(t, f)

			failOn(t, s, OpReadDirect)
			_, err := f.ReadAtDirect(make([]byte, 4), 0)
			wantEIO(t, OpReadDirect, err)
		},
		OpSync: func(t *testing.T, s *Seam) {
			f := create(t, s, "seg")
			write(t, f, "data", 0)

			failOn(t, s, OpSync)
			wantEIO(t, OpSync, f.Sync())

			crash(t, s)
			if got := onDisk(t, s, "seg"); got != "" {
				t.Fatalf("after a failed sync and a crash the file holds %q, want it empty", got)
			}
		},
		OpTruncate: func(t *testing.T, s *Seam) {
			f := create(t, s, "seg")
			write(t, f, "abcd", 0)

			failOn(t, s, OpTruncate)
			wantEIO(t, OpTruncate, f.Truncate(1))

			if got := onDisk(t, s, "seg"); got != "abcd" {
				t.Fatalf("after a failed truncate the file holds %q, want %q", got, "abcd")
			}
		},
		OpAllocate: func(t *testing.T, s *Seam) {
			f := create(t, s, "seg")
			write(t, f, "abcd", 0)

			failOn(t, s, OpAllocate)
			wantEIO(t, OpAllocate, f.Allocate(0, 1<<20))

			if got := onDisk(t, s, "seg"); got != "abcd" {
				t.Fatalf("after a failed allocate the file holds %d bytes, want 4", len(got))
			}
		},
		OpClose: func(t *testing.T, s *Seam) {
			f := create(t, s, "seg")

			failOn(t, s, OpClose)
			wantEIO(t, OpClose, f.Close())

			// A failed close still releases the handle.
			s.SetFault(nil)
			if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("WriteAt after a failed Close: %v, want fs.ErrClosed", err)
			}
			if len(s.open) != 0 {
				t.Fatal("a failed Close left the handle among the open files")
			}
		},
		OpRename: func(t *testing.T, s *Seam) {
			create(t, s, "a")

			failOn(t, s, OpRename)
			wantEIO(t, OpRename, s.Rename("a", "b"))

			if !exists(s, "a") || exists(s, "b") {
				t.Fatal("a failed Rename moved the file")
			}
		},
		OpRemove: func(t *testing.T, s *Seam) {
			create(t, s, "a")

			failOn(t, s, OpRemove)
			wantEIO(t, OpRemove, s.Remove("a"))

			if !exists(s, "a") {
				t.Fatal("a failed Remove unlinked the file")
			}
		},
		OpList: func(t *testing.T, s *Seam) {
			failOn(t, s, OpList)
			_, err := s.List()
			wantEIO(t, OpList, err)
		},
		OpSyncDir: func(t *testing.T, s *Seam) {
			if _, err := s.Create("new"); err != nil {
				t.Fatal(err)
			}

			failOn(t, s, OpSyncDir)
			wantEIO(t, OpSyncDir, s.SyncDir())

			crash(t, s)
			if exists(s, "new") {
				t.Fatal("a Create covered only by a failed SyncDir survived the crash")
			}
		},
	}

	for op, run := range cases {
		t.Run(op, func(t *testing.T) { run(t, newSeam(t)) })
	}
}

// TestCloseCrashAndCreateRunConcurrently guards against data races only when
// run under -race.
func TestCloseCrashAndCreateRunConcurrently(t *testing.T) {
	s := newSeam(t)
	files := make([]storage.File, 16)
	for i := range files {
		files[i] = create(t, s, fmt.Sprint("seg-", i))
	}

	// Half the closes fail, so both of Close's paths run.
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: OpClose, Key: "*", Prob: 0.5, Action: eio})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)

	var wg sync.WaitGroup
	for _, f := range files {
		wg.Go(func() { f.Close() }) // fs.ErrClosed when Crash got there first
	}
	wg.Go(func() {
		if err := s.Crash(); err != nil {
			t.Error(err)
		}
	})
	wg.Go(func() {
		for i := range 16 {
			// fs.ErrClosed when the Crash ran while the Create waited.
			if _, err := s.Create(fmt.Sprint("new-", i)); err != nil && !errors.Is(err, fs.ErrClosed) {
				t.Error(err)
			}
		}
	})
	wg.Wait()
}

func TestCallOnAClosedFileIsNotCountedByTheFault(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "a")
	g := create(t, s, "b")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// The first write the fault sees fails.
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: OpWrite, Key: "*", Count: 1, Action: eio})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)

	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("WriteAt on a closed file: %v, want fs.ErrClosed", err)
	}

	_, err = g.WriteAt([]byte("x"), 0)
	wantEIO(t, "the first write on an open file", err)
}

func TestDelayStallsOnlyItsOwnCall(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "a")
	g := create(t, s, "b")

	c := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, c, sim.Rule{Op: OpSync, Key: "a", Action: sim.FaultAction{Kind: sim.Delay, Delay: 5 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)

	synced := make(chan error)
	go func() { synced <- f.Sync() }()
	waitPending(t, c)

	wrote := make(chan error)
	go func() {
		_, err := g.WriteAt([]byte("x"), 0)
		wrote <- err
	}()

	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a write on another file waited for a delayed sync")
	}

	c.Advance(5 * time.Second)
	select {
	case err := <-synced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the delayed sync did not return once its delay had elapsed")
	}
}

func TestCrashDuringADelayedSyncLeavesTheNewHandlesWritesUnsynced(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "aaaa", 0)
	syncFile(t, f)

	c := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, c, sim.Rule{Op: OpSync, Key: "seg", Count: 1, Action: sim.FaultAction{Kind: sim.Delay, Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)

	synced := make(chan error)
	go func() { synced <- f.Sync() }()
	waitPending(t, c)

	crash(t, s)
	g, err := s.Open("seg")
	if err != nil {
		t.Fatal(err)
	}
	write(t, g, "bbbb", 0)

	c.Advance(time.Second)
	if err := <-synced; !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("a sync delayed across a crash returned %v, want fs.ErrClosed", err)
	}

	if got, err := s.Durable("seg"); err != nil || string(got) != "aaaa" {
		t.Fatalf("Durable: %q, %v; want %q: the old handle's sync covered the new handle's write", got, err, "aaaa")
	}
}

func TestUnsupportedActionPanicsNamingTheKindAndTheOp(t *testing.T) {
	cases := []struct {
		op   string
		kind sim.FaultKind
		call func(s *Seam, f storage.File)
	}{
		{OpRename, sim.Drop, func(s *Seam, f storage.File) { s.Rename("seg", "other") }},
		{OpRemove, sim.Drop, func(s *Seam, f storage.File) { s.Remove("seg") }},
		{OpCreate, sim.Drop, func(s *Seam, f storage.File) { s.Create("new") }},
		{OpClose, sim.Drop, func(s *Seam, f storage.File) { f.Close() }},
		{OpAllocate, sim.Drop, func(s *Seam, f storage.File) { f.Allocate(0, 8) }},
		{OpTruncate, sim.Drop, func(s *Seam, f storage.File) { f.Truncate(0) }},
		{OpRead, sim.Drop, func(s *Seam, f storage.File) { f.ReadAt(make([]byte, 1), 0) }},
		{OpSync, sim.Tear, func(s *Seam, f storage.File) { f.Sync() }},
		{OpReadDirect, sim.Tear, func(s *Seam, f storage.File) { f.ReadAtDirect(make([]byte, 1), 0) }},
		{OpSyncDir, sim.Tear, func(s *Seam, f storage.File) { s.SyncDir() }},
		{OpRead, sim.Reorder, func(s *Seam, f storage.File) { f.ReadAt(make([]byte, 1), 0) }},
		{OpSync, sim.Reorder, func(s *Seam, f storage.File) { f.Sync() }},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%v on %s", tc.kind, tc.op)
		t.Run(name, func(t *testing.T) {
			s := newSeam(t)
			f := create(t, s, "seg")
			s.SetFault(faultFunc(func(string, []byte) sim.FaultAction {
				return sim.FaultAction{Kind: tc.kind}
			}))

			defer func() {
				msg := fmt.Sprint(recover())
				if !strings.Contains(msg, tc.kind.String()) || !strings.Contains(msg, tc.op) {
					t.Fatalf("panic %q, want one naming %v and %s", msg, tc.kind, tc.op)
				}
			}()
			tc.call(s, f)
		})
	}
}

// failNext makes the next call of op fail with EIO and every later call pass.
func failNext(t *testing.T, s *Seam, op string) {
	t.Helper()

	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: op, Key: "*", Count: 1, Action: eio})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)
}

func failNextSync(t *testing.T, s *Seam) {
	t.Helper()
	failNext(t, s, OpSync)
}

func TestFailedSyncLosesThePendingWritesForGood(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "data", 0)

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())
	syncFile(t, f) // succeeds, and must not stand in for the failed one

	p := make([]byte, 4)
	if _, err := f.ReadAt(p, 0); err != nil || string(p) != "data" {
		t.Fatalf("ReadAt after the failed sync: %q, %v; want the page cache's %q", p, err, "data")
	}

	if got, err := s.Durable("seg"); err != nil || len(got) != 0 {
		t.Fatalf("Durable: %q, %v; want it empty", got, err)
	}

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "" {
		t.Fatalf("after the crash the file holds %q, want it empty", got)
	}
}

func TestFailedSyncLosesAnAllocation(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	if err := f.Allocate(0, 1<<20); err != nil {
		t.Fatal(err)
	}

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())
	syncFile(t, f)

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "" {
		t.Fatalf("an allocation a failed sync covered left %d bytes after the crash, want 0", len(got))
	}
}

func TestWritesAfterAFailedSyncBecomeDurable(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "aaaa", 0)

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())

	write(t, f, "bb", 1) // over the lost write
	syncFile(t, f)
	write(t, f, "cc", 4) // after a sync that lost nothing
	syncFile(t, f)

	want := "\x00bb\x00cc" // the lost bytes read as zeros: they never existed durably
	if got, err := s.Durable("seg"); err != nil || string(got) != want {
		t.Fatalf("Durable: %q, %v; want %q", got, err, want)
	}

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != want {
		t.Fatalf("after the crash the file holds %q, want %q", got, want)
	}
}

func TestDurableReadsServeTheImageAfterALostSync(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "ab", 0)
	syncFile(t, f)
	write(t, f, "XYZW", 0)

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())
	syncFile(t, f) // nothing pending is left to undo: only the image knows

	p := make([]byte, 4)
	if n, err := f.ReadAtDirect(p, 0); n != 2 || !errors.Is(err, io.EOF) || string(p[:n]) != "ab" {
		t.Fatalf("ReadAtDirect: %q, %v; want %q and io.EOF", p[:n], err, "ab")
	}

	if _, err := f.ReadAtDirect(p, 2); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAtDirect at the durable end: %v, want io.EOF", err)
	}

	if _, err := f.ReadAtDirect(p, -1); err == nil {
		t.Fatal("ReadAtDirect at a negative offset succeeded")
	}

	s.StaleReads("seg")
	if n, _ := f.ReadAt(p, 0); string(p[:n]) != "ab" {
		t.Fatalf("ReadAt with stale reads: %q, want %q", p[:n], "ab")
	}
}

func TestTornNeverPicksALostWrite(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	g := create(t, s, "other")

	write(t, g, "bbbb", 0) // pending, and older than the lost write
	write(t, f, "aaaa", 0)

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())

	crash(t, s, Torn(1))

	if got := onDisk(t, s, "seg"); got != "" {
		t.Fatalf("seg holds %q after the crash: a lost write was torn", got)
	}
	if got := onDisk(t, s, "other"); got != "b" {
		t.Fatalf("other holds %q, want the last write that was not lost, torn to %q", got, "b")
	}
}

func TestReorderedNeverKeepsALostWrite(t *testing.T) {
	for seed := range uint64(64) {
		s := newSeam(t)
		f := create(t, s, "seg")
		write(t, f, "lost", 0)

		failNextSync(t, s)
		wantEIO(t, OpSync, f.Sync())

		crash(t, s, Reordered(seed))
		if got := onDisk(t, s, "seg"); got != "" {
			t.Fatalf("seed %d kept the lost write: %q", seed, got)
		}
	}
}

func TestResizesAfterAFailedSyncBecomeDurable(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "abcd", 0)

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())

	if err := f.Truncate(6); err != nil {
		t.Fatal(err)
	}
	syncFile(t, f)
	if got, _ := s.Durable("seg"); string(got) != "\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("after a synced Truncate(6) the durable image is %q, want 6 zeros", got)
	}

	if err := f.Allocate(0, 8); err != nil {
		t.Fatal(err)
	}
	syncFile(t, f)
	if got, _ := s.Durable("seg"); len(got) != 8 {
		t.Fatalf("after a synced Allocate(0, 8) the durable image has %d bytes, want 8", len(got))
	}
}

func TestDurableReturnsACopy(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "ab", 0)
	syncFile(t, f)
	write(t, f, "XY", 0)

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())
	syncFile(t, f)

	got, err := s.Durable("seg")
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 'Z'

	if again, _ := s.Durable("seg"); string(again) != "ab" {
		t.Fatalf("changing the bytes Durable returned changed the durable image to %q", again)
	}
}

func TestSyncAfterAFailedSyncOnAnUnlinkedFile(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "seg")
	write(t, f, "ab", 0)
	syncFile(t, f)
	write(t, f, "XY", 0)

	if err := s.Remove("seg"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncDir(); err != nil { // unlinks the file: only the handle reaches it
		t.Fatal(err)
	}

	failNextSync(t, s)
	wantEIO(t, OpSync, f.Sync())
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync on an open, unlinked file after a failed one: %v", err)
	}

	p := make([]byte, 2)
	if _, err := f.ReadAtDirect(p, 0); err != nil || string(p) != "ab" {
		t.Fatalf("ReadAtDirect: %q, %v; want the durable %q", p, err, "ab")
	}
}

// failSyncDir runs a SyncDir that fails, then lets every later call pass.
func failSyncDir(t *testing.T, s *Seam) {
	t.Helper()

	failNext(t, s, OpSyncDir)
	wantEIO(t, OpSyncDir, s.SyncDir())
}

func syncDir(t *testing.T, s *Seam) {
	t.Helper()

	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}
}

// wantDurable checks what a crash now would leave in name: want, or no file
// when want is nil.
func wantDurable(t *testing.T, s *Seam, name string, want *string) {
	t.Helper()

	got, err := s.Durable(name)
	switch {
	case want == nil && !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("Durable(%q): %q, %v; want fs.ErrNotExist", name, got, err)
	case want != nil && (err != nil || string(got) != *want):
		t.Fatalf("Durable(%q): %q, %v; want %q", name, got, err, *want)
	}
}

// wantNames checks the directory's names, read past the seam.
func wantNames(t *testing.T, s *Seam, want ...string) {
	t.Helper()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the directory holds %v, want %v", got, want)
	}
}

func TestFailedSyncDirLosesItsChangesForGood(t *testing.T) {
	s := newSeam(t)
	if _, err := s.Create("a"); err != nil {
		t.Fatal(err)
	}

	failSyncDir(t, s)
	syncDir(t, s) // succeeds, and must not stand in for the failed one
	wantDurable(t, s, "a", nil)

	// A second failure drops what was logged since, and keeps what the first
	// one left durable.
	if _, err := s.Create("b"); err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, s)
	syncDir(t, s)
	wantDurable(t, s, "b", nil)

	crash(t, s)
	wantNames(t, s)
}

// durableFile creates name holding data, durable in the directory and on
// disk.
func durableFile(t *testing.T, s *Seam, name, data string) storage.File {
	t.Helper()

	f := create(t, s, name)
	write(t, f, data, 0)
	syncFile(t, f)
	return f
}

func TestARenameAfterAFailedSyncDirIsDurable(t *testing.T) {
	s := newSeam(t)
	f, err := s.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	write(t, f, "x", 0)
	syncFile(t, f)

	failSyncDir(t, s) // loses the create
	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	syncDir(t, s)

	wantDurable(t, s, "a", nil)
	wantDurable(t, s, "b", new("x"))

	crash(t, s)
	wantNames(t, s, "b")
}

func TestARemoveLostByAFailedSyncDirIsUndoneByACrash(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "keep")

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, s)
	syncDir(t, s) // must not unlink the removed file: a crash brings it back

	wantDurable(t, s, "a", new("keep"))

	crash(t, s)
	if got := onDisk(t, s, "a"); got != "keep" {
		t.Fatalf("after the crash a holds %q, want %q", got, "keep")
	}
}

func TestAReplacingRenameLostByAFailedSyncDirIsUndoneByACrash(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "x", "new")
	durableFile(t, s, "a", "old")

	if err := s.Rename("x", "a"); err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, s)
	syncDir(t, s)

	wantDurable(t, s, "x", new("new"))
	wantDurable(t, s, "a", new("old"))

	crash(t, s)
	if got := onDisk(t, s, "x") + "," + onDisk(t, s, "a"); got != "new,old" {
		t.Fatalf("after the crash x,a hold %q, want %q", got, "new,old")
	}
}

func TestAFileReappendedAfterAFailedSyncDirIsDurable(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "old")

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, s)

	f, err := s.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	write(t, f, "new", 0)
	syncFile(t, f)
	syncDir(t, s)

	wantDurable(t, s, "a", new("new"))

	crash(t, s)
	if got := onDisk(t, s, "a"); got != "new" {
		t.Fatalf("after the crash a holds %q, want %q", got, "new")
	}
}

func TestARenameAfterALostRenameTakesTheFileFromItsDurableName(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "data")

	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, s)
	if err := s.Rename("b", "c"); err != nil {
		t.Fatal(err)
	}
	syncDir(t, s)

	wantDurable(t, s, "a", nil)
	wantDurable(t, s, "b", nil)
	wantDurable(t, s, "c", new("data"))

	crash(t, s)
	wantNames(t, s, "c")
}

func TestARemoveAfterAFailedSyncDirDeletesTheDurableName(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "old")

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("a"); err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, s) // loses the remove and the create

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	syncDir(t, s)

	wantDurable(t, s, "a", nil)

	crash(t, s)
	wantNames(t, s)
}

func TestARenameLostByAFailedSyncDirSurvivesWhenItsNameIsReused(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "orig")

	if err := s.Rename("a", "c"); err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, s) // loses the rename

	f, err := s.Create("a") // takes the name the lost rename freed
	if err != nil {
		t.Fatal(err)
	}
	write(t, f, "new", 0)
	syncFile(t, f)
	if err := s.Rename("a", "d"); err != nil {
		t.Fatal(err)
	}
	syncDir(t, s)

	// An ordered filesystem cannot commit the create without the rename
	// that freed its name: the original survives as c, never under no name.
	wantDurable(t, s, "a", nil)
	wantDurable(t, s, "c", new("orig"))
	wantDurable(t, s, "d", new("new"))

	crash(t, s)
	wantNames(t, s, "c", "d")
	if got := onDisk(t, s, "c") + "," + onDisk(t, s, "d"); got != "orig,new" {
		t.Fatalf("after the crash c,d hold %q, want %q", got, "orig,new")
	}
}

func TestLostRenamesAReusedNameDependsOnSurviveInAChain(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "A")
	durableFile(t, s, "c", "C")

	for _, r := range [][2]string{{"c", "e"}, {"a", "c"}} {
		if err := s.Rename(r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	failSyncDir(t, s) // loses both renames

	if _, err := s.Create("a"); err != nil { // needs a free, so a->c, so c->e
		t.Fatal(err)
	}
	syncDir(t, s)

	wantDurable(t, s, "a", new(""))
	wantDurable(t, s, "c", new("A"))
	wantDurable(t, s, "e", new("C"))

	crash(t, s)
	wantNames(t, s, "a", "c", "e")
}

func TestARemoveAfterAFailedSyncDirKeepsTheFileALostRenameMoved(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "X")
	durableFile(t, s, "b", "Y")

	for _, r := range [][2]string{{"a", "c"}, {"b", "a"}} {
		if err := s.Rename(r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	failSyncDir(t, s) // loses both renames: a is still X, durably

	if err := s.Remove("a"); err != nil { // removes Y, which a now names
		t.Fatal(err)
	}
	syncDir(t, s)

	// The remove could only remove Y after b->a put it at a, and a was free
	// for that only after a->c: committing the remove commits both renames.
	// Y is gone from every name, and X keeps c rather than vanishing with a.
	wantDurable(t, s, "a", nil)
	wantDurable(t, s, "b", nil)
	wantDurable(t, s, "c", new("X"))

	crash(t, s)
	wantNames(t, s, "c")
	if got := onDisk(t, s, "c"); got != "X" {
		t.Fatalf("after the crash c holds %q, want %q", got, "X")
	}
}

func TestTornAfterAFailedSyncDirPicksOnlyDurableFiles(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "a", "aaaa")
	write(t, f, "bbbb", 4)

	g, err := s.Create("c")
	if err != nil {
		t.Fatal(err)
	}
	write(t, g, "cc", 0) // the last write, on a file whose create is lost
	failSyncDir(t, s)

	crash(t, s, Torn(1))

	if got := onDisk(t, s, "a"); got != "aaaab" {
		t.Fatalf("a holds %q, want its last write torn to %q", got, "aaaab")
	}
	wantNames(t, s, "a")
}

func TestARenameAfterAFailedSyncDirDeletesTheDurableOldName(t *testing.T) {
	s := newSeam(t)
	durableFile(t, s, "a", "old")

	if err := s.Remove("a"); err != nil {
		t.Fatal(err)
	}
	f, err := s.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	write(t, f, "new", 0)
	syncFile(t, f)
	failSyncDir(t, s) // loses the remove and the create: a is still "old"

	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	syncDir(t, s)

	wantDurable(t, s, "a", nil)
	wantDurable(t, s, "b", new("new"))

	crash(t, s)
	wantNames(t, s, "b")
}

// actOnce makes the next call of op take the action a, and every later call
// pass.
func actOnce(t *testing.T, s *Seam, op string, a sim.FaultAction) {
	t.Helper()

	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: op, Key: "*", Count: 1, Action: a})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)
}

var drop = sim.FaultAction{Kind: sim.Drop}

func readAt(t *testing.T, f storage.File, n int) string {
	t.Helper()

	p := make([]byte, n)
	got, err := f.ReadAt(p, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(p[:got])
}

func readDirect(t *testing.T, f storage.File, n int) string {
	t.Helper()

	p := make([]byte, n)
	got, err := f.ReadAtDirect(p, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(p[:got])
}

func TestADroppedWriteIsReadButNeverDurable(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxxxx")

	actOnce(t, s, OpWrite, drop)
	if n, err := f.WriteAt([]byte("BB"), 2); n != 2 || err != nil {
		t.Fatalf("a dropped write returned %d, %v; want 2, nil", n, err)
	}
	syncFile(t, f) // succeeds, and must not make the dropped write durable

	if got := readAt(t, f, 16); got != "xxBBxxxx" {
		t.Fatalf("ReadAt: %q, want the page cache's %q", got, "xxBBxxxx")
	}
	if got := readDirect(t, f, 16); got != "xxxxxxxx" {
		t.Fatalf("ReadAtDirect: %q, want the durable %q", got, "xxxxxxxx")
	}
	wantDurable(t, s, "seg", new("xxxxxxxx"))

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "xxxxxxxx" {
		t.Fatalf("after the crash the file holds %q, want %q", got, "xxxxxxxx")
	}
}

func TestASyncedWriteOverADroppedOneIsDurable(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxxxx")

	actOnce(t, s, OpWrite, drop)
	write(t, f, "BB", 2)
	write(t, f, "C", 3) // over half of the dropped write
	syncFile(t, f)

	wantDurable(t, s, "seg", new("xxxCxxxx"))

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "xxxCxxxx" {
		t.Fatalf("after the crash the file holds %q, want %q", got, "xxxCxxxx")
	}
}

func TestADroppedWriteThenAnExtendingWrite(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxxxx")

	actOnce(t, s, OpWrite, drop)
	write(t, f, "BBBB", 8) // extends the file, dropped
	syncFile(t, f)
	wantDurable(t, s, "seg", new("xxxxxxxx"))

	write(t, f, "Z", 12) // past the dropped bytes
	syncFile(t, f)

	want := "xxxxxxxx\x00\x00\x00\x00Z" // the dropped bytes are a hole
	wantDurable(t, s, "seg", &want)

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != want {
		t.Fatalf("after the crash the file holds %q, want %q", got, want)
	}
}

func TestADroppedWriteThenATruncate(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxxxx")

	actOnce(t, s, OpWrite, drop)
	write(t, f, "BB", 2)
	if err := f.Truncate(4); err != nil {
		t.Fatal(err)
	}
	syncFile(t, f)

	wantDurable(t, s, "seg", new("xxxx"))

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "xxxx" {
		t.Fatalf("after the crash the file holds %q, want %q", got, "xxxx")
	}
}

func TestADroppedSyncLeavesTheWritesPending(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "aaaa")
	write(t, f, "bb", 4)

	actOnce(t, s, OpSync, drop)
	if err := f.Sync(); err != nil {
		t.Fatalf("a dropped sync returned %v, want nil", err)
	}
	wantDurable(t, s, "seg", new("aaaa"))

	syncFile(t, f) // a later sync still covers the writes
	wantDurable(t, s, "seg", new("aaaabb"))
}

func TestADroppedSyncDirLeavesTheCreatePending(t *testing.T) {
	s := newSeam(t)
	if _, err := s.Create("a"); err != nil {
		t.Fatal(err)
	}

	actOnce(t, s, OpSyncDir, drop)
	if err := s.SyncDir(); err != nil {
		t.Fatalf("a dropped SyncDir returned %v, want nil", err)
	}
	wantDurable(t, s, "a", nil)

	crash(t, s)
	wantNames(t, s)
}

func TestADroppedSyncDirLosesNothing(t *testing.T) {
	s := newSeam(t)
	if _, err := s.Create("a"); err != nil {
		t.Fatal(err)
	}

	actOnce(t, s, OpSyncDir, drop)
	if err := s.SyncDir(); err != nil {
		t.Fatal(err)
	}
	syncDir(t, s) // unlike after a failure, covers the create

	wantDurable(t, s, "a", new(""))
}

func TestTearWritesOnlyTheFirstNBytes(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxxxx")

	actOnce(t, s, OpWrite, sim.FaultAction{Kind: sim.Tear, N: 3})
	if n, err := f.WriteAt([]byte("ABCDE"), 2); n != 5 || err != nil {
		t.Fatalf("a write torn at 3 returned %d, %v; want the whole 5, nil", n, err)
	}

	if got := readAt(t, f, 16); got != "xxABCxxx" {
		t.Fatalf("ReadAt: %q, want %q", got, "xxABCxxx")
	}

	syncFile(t, f)
	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "xxABCxxx" {
		t.Fatalf("after a sync and a crash the file holds %q, want %q", got, "xxABCxxx")
	}
}

func TestTearPastTheWriteWritesItWhole(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxx")

	actOnce(t, s, OpWrite, sim.FaultAction{Kind: sim.Tear, N: 4096})
	if n, err := f.WriteAt([]byte("AB"), 1); n != 2 || err != nil {
		t.Fatalf("a write shorter than its tear returned %d, %v; want 2, nil", n, err)
	}

	syncFile(t, f)
	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "xABx" {
		t.Fatalf("after a sync and a crash the file holds %q, want %q", got, "xABx")
	}
}

func TestTornWriteRecordsOnlyTheBytesWritten(t *testing.T) {
	// Reordered replays the bytes a write recorded; a torn write must offer
	// only the ones that reached the file.
	for seed := range uint64(64) {
		s := newSeam(t)
		f := durableFile(t, s, "seg", "")

		actOnce(t, s, OpWrite, sim.FaultAction{Kind: sim.Tear, N: 2})
		f.WriteAt([]byte("ABCD"), 0)

		crash(t, s, Reordered(seed))
		if got := onDisk(t, s, "seg"); got != "" && got != "AB" {
			t.Fatalf("seed %d left %q, want nothing or the 2 bytes written", seed, got)
		}
	}
}

var reorder = sim.FaultAction{Kind: sim.Reorder}

// markWrites makes every write of a file whose name matches key take the
// Reorder action, and every other call pass.
func markWrites(t *testing.T, s *Seam, key string) {
	t.Helper()

	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: OpWrite, Key: key, Action: reorder})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)
}

func TestAnUnmarkedWriteNeverSurvivesAReorderedCrashOnceOneIsMarked(t *testing.T) {
	for seed := range uint64(64) {
		s := newSeam(t)
		f := durableFile(t, s, "marked", "")
		g := durableFile(t, s, "plain", "")

		markWrites(t, s, "marked")
		write(t, g, "uu", 0) // unmarked
		write(t, f, "mm", 0) // marked

		crash(t, s, Reordered(seed))
		if got := onDisk(t, s, "plain"); got != "" {
			t.Fatalf("seed %d kept the unmarked write %q while a write was marked", seed, got)
		}
	}
}

func TestAMarkedWriteSurvivesAReorderedCrashForSomeSeed(t *testing.T) {
	kept, lost := false, false
	for seed := range uint64(64) {
		s := newSeam(t)
		f := durableFile(t, s, "seg", "")

		markWrites(t, s, "seg")
		write(t, f, "mm", 0)

		crash(t, s, Reordered(seed))
		switch onDisk(t, s, "seg") {
		case "mm":
			kept = true
		case "":
			lost = true
		}
	}

	if !kept || !lost {
		t.Fatalf("over 64 seeds a marked write was kept: %v, lost: %v; want both", kept, lost)
	}
}

func TestMarksHaveNoEffectUnderCrashOrTorn(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "")
	markWrites(t, s, "seg")
	write(t, f, "mm", 0)

	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "" {
		t.Fatalf("a plain crash kept the marked write: %q", got)
	}

	s = newSeam(t)
	f = durableFile(t, s, "seg", "")
	g := durableFile(t, s, "plain", "")
	markWrites(t, s, "seg")
	write(t, f, "mm", 0) // marked, older
	write(t, g, "uu", 0) // unmarked, the last write

	crash(t, s, Torn(1))
	if got := onDisk(t, s, "seg") + "," + onDisk(t, s, "plain"); got != ",u" {
		t.Fatalf("Torn left seg,plain = %q, want the last write torn: %q", got, ",u")
	}
}

func TestASyncedMarkedWriteIsNeverDrawn(t *testing.T) {
	// Once its mark is synced away, nothing is marked, so the draw covers
	// every pending write again.
	kept := false
	for seed := range uint64(64) {
		s := newSeam(t)
		f := durableFile(t, s, "seg", "")
		g := durableFile(t, s, "plain", "")

		markWrites(t, s, "seg")
		write(t, f, "mm", 0)
		syncFile(t, f)
		write(t, g, "uu", 0)

		crash(t, s, Reordered(seed))
		if got := onDisk(t, s, "seg"); got != "mm" {
			t.Fatalf("seed %d: the synced marked write left %q", seed, got)
		}
		if onDisk(t, s, "plain") == "uu" {
			kept = true
		}
	}

	if !kept {
		t.Fatal("no seed kept the unmarked write: a synced mark still narrowed the draw")
	}
}

func TestTearWithAnErrorIsAShortWrite(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxxxxxx")

	actOnce(t, s, OpWrite, sim.FaultAction{Kind: sim.Tear, N: 3, Err: syscall.EIO})
	n, err := f.WriteAt([]byte("ABCDE"), 2)

	var pe *fs.PathError
	if n != 3 || !errors.Is(err, syscall.EIO) || !errors.As(err, &pe) || pe.Path != "seg" {
		t.Fatalf("a write torn at 3 with EIO returned %d, %v; want 3 and EIO in a PathError on seg", n, err)
	}

	if got := readAt(t, f, 16); got != "xxABCxxx" {
		t.Fatalf("ReadAt: %q, want the part written, %q", got, "xxABCxxx")
	}
	wantDurable(t, s, "seg", new("xxxxxxxx")) // written, not yet synced

	syncFile(t, f)
	crash(t, s)
	if got := onDisk(t, s, "seg"); got != "xxABCxxx" {
		t.Fatalf("after a sync and a crash the file holds %q, want %q", got, "xxABCxxx")
	}
}

func TestTearWithAnErrorPastTheWriteWritesItWhole(t *testing.T) {
	s := newSeam(t)
	f := durableFile(t, s, "seg", "xxxx")

	actOnce(t, s, OpWrite, sim.FaultAction{Kind: sim.Tear, N: 4096, Err: syscall.EIO})
	if n, err := f.WriteAt([]byte("AB"), 1); n != 2 || err != nil {
		t.Fatalf("a write shorter than its tear returned %d, %v; want 2, nil: nothing was torn", n, err)
	}

	if got := readAt(t, f, 16); got != "xABx" {
		t.Fatalf("ReadAt: %q, want %q", got, "xABx")
	}
}

func goSyncDir(s *Seam) <-chan error {
	done := make(chan error, 1)
	go func() { done <- s.SyncDir() }()
	return done
}

func TestASyncDirDelayedAcrossACrashLeavesTheNewLogPending(t *testing.T) {
	s := newSeam(t)

	c := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, c, sim.Rule{Op: OpSyncDir, Key: "*", Count: 1, Action: sim.FaultAction{Kind: sim.Delay, Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)

	done := goSyncDir(s)
	waitPending(t, c)

	crash(t, s)
	if _, err := s.Create("a"); err != nil { // after the crash
		t.Fatal(err)
	}

	c.Advance(time.Second)
	if err := within(t, done, "the delayed SyncDir returning"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("a SyncDir delayed across a crash returned %v, want fs.ErrClosed", err)
	}
	wantDurable(t, s, "a", nil)

	crash(t, s)
	wantNames(t, s)
}

func TestAFailedSyncDirHeldAcrossACrashLeavesTheNewLogIntact(t *testing.T) {
	s := newSeam(t)
	failNext(t, s, OpSyncDir)

	h := s.Hold(OpSyncDir, "*")
	done := goSyncDir(s)
	within(t, h.Reached(), "the SyncDir reaching the hold")

	crash(t, s)
	if _, err := s.Create("a"); err != nil { // after the crash
		t.Fatal(err)
	}

	h.Release()
	if err := within(t, done, "the released SyncDir returning"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("a failing SyncDir held across a crash returned %v, want fs.ErrClosed", err)
	}

	syncDir(t, s) // must cover the create: nothing was lost
	wantDurable(t, s, "a", new(""))
}

func TestACreateHeldAcrossACrashDoesNotCreateTheFile(t *testing.T) {
	s := newSeam(t)

	h := s.Hold(OpCreate, "a")
	done := make(chan error, 1)
	go func() { _, err := s.Create("a"); done <- err }()
	within(t, h.Reached(), "the Create reaching the hold")

	crash(t, s)
	h.Release()
	if err := within(t, done, "the released Create returning"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("a Create held across a crash returned %v, want fs.ErrClosed", err)
	}
	wantNames(t, s)
}

func TestACallHeldAcrossARefusedCrashGoesAhead(t *testing.T) {
	s := newSeam(t)

	h := s.Hold(OpCreate, "a")
	done := make(chan error, 1)
	go func() { _, err := s.Create("a"); done <- err }()
	within(t, h.Reached(), "the Create reaching the hold")

	if err := s.Crash(Torn(1)); err == nil { // no unsynced write to tear
		t.Fatal("Crash(Torn(1)) with nothing to tear succeeded")
	}

	h.Release()
	if err := within(t, done, "the released Create returning"); err != nil {
		t.Fatalf("a Create held across a refused crash returned %v, want nil", err)
	}
	wantNames(t, s, "a")
}
