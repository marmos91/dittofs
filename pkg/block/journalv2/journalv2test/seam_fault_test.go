package journalv2test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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
			if _, err := s.Create(fmt.Sprint("new-", i)); err != nil {
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
		{OpClose, sim.Drop, func(s *Seam, f storage.File) { f.Close() }},
		{OpSync, sim.Tear, func(s *Seam, f storage.File) { f.Sync() }},
		{OpRead, sim.Reorder, func(s *Seam, f storage.File) { f.ReadAt(make([]byte, 1), 0) }},
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

// failNextSync makes the next sync fail with EIO and every later call pass.
func failNextSync(t *testing.T, s *Seam) {
	t.Helper()

	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: OpSync, Key: "*", Count: 1, Action: eio})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)
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
