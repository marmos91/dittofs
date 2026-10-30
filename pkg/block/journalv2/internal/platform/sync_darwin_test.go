package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// call is one entry of a recorder's log.
type call struct {
	op   string // open, fcntl, fcntlFstore, fstat, ftruncate or close
	fd   int    // the descriptor acted on, or open's result; -1 for a failed open
	path string // open: the path opened
	mode int    // open: its flags

	cmd   int           // fcntl and fcntlFstore: the command
	arg   int           // fcntl: its argument
	store unix.Fstore_t // fcntlFstore: the structure as the call received it
	size  int64         // ftruncate: the size asked for

	// Every call but open and close: the file the descriptor referred to at
	// the call, so a call on a stale or wrong descriptor is seen.
	dev, ino uint64
}

func (c call) String() string {
	switch c.op {
	case "open":
		return fmt.Sprintf("open(%s, %#x) = %d", c.path, c.mode, c.fd)
	case "close":
		return fmt.Sprintf("close(%d)", c.fd)
	case "fcntl":
		return fmt.Sprintf("fcntl(%d, %d, %d) on dev %d ino %d", c.fd, c.cmd, c.arg, c.dev, c.ino)
	case "fcntlFstore":
		return fmt.Sprintf(
			"fcntl(%d, %d, %+v) on dev %d ino %d",
			c.fd,
			c.cmd,
			c.store,
			c.dev,
			c.ino,
		)
	case "ftruncate":
		return fmt.Sprintf("ftruncate(%d, %d) on dev %d ino %d", c.fd, c.size, c.dev, c.ino)
	default:
		return fmt.Sprintf("%s(%d) on dev %d ino %d", c.op, c.fd, c.dev, c.ino)
	}
}

// recorder is a table that logs every call and makes it. An error queued for
// an op is returned by its next call instead: that call then does not run,
// except close, which runs, as the descriptor is released even when close
// reports an error.
type recorder struct {
	mu   sync.Mutex
	log  []call
	fail map[string][]error // per op, what its next calls return, oldest first
}

// failNext makes the next calls of op return errs, one each, in order.
func (r *recorder) failNext(op string, errs ...error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.fail == nil {
		r.fail = map[string][]error{}
	}

	r.fail[op] = append(r.fail[op], errs...)
}

// take returns the calls logged since the last take.
func (r *recorder) take() []call {
	r.mu.Lock()
	defer r.mu.Unlock()

	log := r.log
	r.log = nil

	return log
}

// queued reports whether an error is queued for op's next call.
func (r *recorder) queued(op string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.fail[op]) > 0
}

// add logs c and returns the error queued for its op, if any.
func (r *recorder) add(c call) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.log = append(r.log, c)

	if q := r.fail[c.op]; len(q) > 0 {
		r.fail[c.op] = q[1:]

		return q[0]
	}

	return nil
}

// on returns a call of op on fd, with the file fd refers to now.
func on(op string, fd int) call {
	c := call{
		op: op,
		fd: fd,
	}

	var st unix.Stat_t

	if unix.Fstat(fd, &st) == nil {
		c.dev, c.ino = uint64(st.Dev), st.Ino
	}

	return c
}

func (r *recorder) table() calls {
	return calls{
		open: func(path string, mode int, perm uint32) (int, error) {
			if r.queued("open") {
				return -1, r.add(call{
					op:   "open",
					fd:   -1,
					path: path,
					mode: mode,
				})
			}

			fd, err := unix.Open(path, mode, perm)
			if err != nil {
				fd = -1
			}

			_ = r.add(call{
				op:   "open",
				fd:   fd,
				path: path,
				mode: mode,
			})

			return fd, err
		},
		fcntl: func(fd uintptr, cmd, arg int) (int, error) {
			c := on("fcntl", int(fd))
			c.cmd, c.arg = cmd, arg

			if err := r.add(c); err != nil {
				return -1, err
			}

			return unix.FcntlInt(fd, cmd, arg)
		},
		fcntlFstore: func(fd uintptr, cmd int, fstore *unix.Fstore_t) error {
			c := on("fcntlFstore", int(fd))
			c.cmd = cmd

			if r.queued("fcntlFstore") {
				c.store = *fstore

				return r.add(c)
			}

			// Copied after the call, so the log holds what the kernel
			// filled in.
			err := unix.FcntlFstore(fd, cmd, fstore)
			c.store = *fstore
			_ = r.add(c)

			return err
		},
		fstat: func(fd int, st *unix.Stat_t) error {
			if err := r.add(on("fstat", fd)); err != nil {
				return err
			}

			return unix.Fstat(fd, st)
		},
		ftruncate: func(fd int, length int64) error {
			c := on("ftruncate", fd)
			c.size = length

			if err := r.add(c); err != nil {
				return err
			}

			return unix.Ftruncate(fd, length)
		},
		close: func(fd int) error {
			queued := r.add(call{
				op: "close",
				fd: fd,
			})
			err := unix.Close(fd)

			if queued != nil {
				return queued
			}

			return err
		},
	}
}

// identity returns the device and inode of path.
func identity(t *testing.T, path string) (dev, ino uint64) {
	t.Helper()

	var st unix.Stat_t

	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return uint64(st.Dev), st.Ino
}

// openOn creates the file name in o's directory and returns it as o's file.
func openOn(t *testing.T, o *OS, name string) *file {
	t.Helper()

	f, err := os.OpenFile(filepath.Join(o.dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	sf := o.newFile(f, name)
	t.Cleanup(func() {
		_ = f.Close()
	})

	return sf
}

// expectOn fails t unless c acted on a descriptor of the file whose device
// and inode are dev and ino.
func expectOn(t *testing.T, c call, dev, ino uint64, path string) {
	t.Helper()

	if c.dev != dev || c.ino != ino {
		t.Fatalf("%v: the descriptor is not %s's (dev %d ino %d)", c, path, dev, ino)
	}
}

// isFullSync reports whether c is fcntl with F_FULLFSYNC, whose argument
// is ignored and passed as 0.
func isFullSync(c call) bool {
	return c.op == "fcntl" && c.cmd == unix.F_FULLFSYNC && c.arg == 0
}

// expectFullSync fails t unless log is exactly n calls of fcntl with
// F_FULLFSYNC, each on a descriptor of path.
func expectFullSync(t *testing.T, log []call, path string, n int) {
	t.Helper()

	if len(log) != n {
		t.Fatalf("want exactly %d fcntl(F_FULLFSYNC) on %s, got %v", n, filepath.Base(path), log)
	}

	dev, ino := identity(t, path)

	for _, c := range log {
		if !isFullSync(c) {
			t.Fatalf(
				"want exactly %d fcntl(F_FULLFSYNC) on %s, got %v",
				n,
				filepath.Base(path),
				log,
			)
		}

		expectOn(t, c, dev, ino, path)
	}
}

// expectSyncDir fails t unless log is exactly: dir opened for reading as a
// directory and not inherited by a child process, then syncs calls of fcntl
// with F_FULLFSYNC on that descriptor, which refers to dir, then that
// descriptor closed.
func expectSyncDir(t *testing.T, log []call, dir string, syncs int) {
	t.Helper()

	if len(log) != syncs+2 || log[0].op != "open" || log[len(log)-1].op != "close" {
		t.Fatalf("want open, %d fcntl(F_FULLFSYNC) and close of the directory, got %v", syncs, log)
	}

	open, closed := log[0], log[len(log)-1]

	if open.path != dir {
		t.Fatalf("%v: want %s opened", open, dir)
	}

	if open.mode&unix.O_ACCMODE != unix.O_RDONLY || open.mode&unix.O_DIRECTORY == 0 ||
		open.mode&unix.O_CLOEXEC == 0 {
		t.Fatalf("%v: want O_RDONLY with O_DIRECTORY and O_CLOEXEC", open)
	}

	if closed.fd != open.fd {
		t.Fatalf("%v: want the descriptor %v returned closed", closed, open)
	}

	dev, ino := identity(t, dir)

	for _, c := range log[1 : len(log)-1] {
		if !isFullSync(c) || c.fd != open.fd {
			t.Fatalf("%v: want fcntl(F_FULLFSYNC) on the descriptor %v returned", c, open)
		}

		expectOn(t, c, dev, ino, dir)
	}
}

// expectPathError fails t unless err is itself an *fs.PathError of op,
// wrapping errno and naming path, the shape the platform's own errors take
// when one thing failed.
func expectPathError(t *testing.T, err error, op string, errno syscall.Errno, path string) {
	t.Helper()

	pe, ok := err.(*fs.PathError)
	if !ok || !errors.Is(pe.Err, errno) {
		t.Fatalf("got %v (%T), want an *fs.PathError wrapping %v", err, err, errno)
	}

	if pe.Path != path {
		t.Fatalf("%v: names %s, want %s", err, pe.Path, path)
	}

	if pe.Op != op {
		t.Fatalf("%v: has op %q, want %q", err, pe.Op, op)
	}
}

// hasPathError reports whether err is, or joins, an *fs.PathError wrapping
// errno.
func hasPathError(err error, errno syscall.Errno) bool {
	if pe, ok := err.(*fs.PathError); ok {
		return errors.Is(pe.Err, errno)
	}

	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			if hasPathError(e, errno) {
				return true
			}
		}
	}

	return false
}

// TestSyncCalls checks the call each sync makes on macOS (RFC 28 Appendix A:
// "the journal uses `fcntl(F_FULLFSYNC)` for every sync, the directory's
// included"): one fcntl with F_FULLFSYNC on the file's own descriptor per
// Sync, and per SyncDir one on a descriptor of the directory that SyncDir
// opens and closes itself. A plain fsync has no entry in the table, and a
// call made beside it is TestNoCallBesideTheTable's to find.
func TestSyncCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	a := openOn(t, o, "a")
	b := openOn(t, o, "b")

	for _, step := range []struct {
		f    *file
		name string
	}{{b, "b"}, {a, "a"}, {a, "a"}} {
		if _, err := step.f.f.WriteAt([]byte("record"), 0); err != nil {
			t.Fatal(err)
		}

		err := step.f.Sync()
		expectFullSync(t, r.take(), filepath.Join(dir, step.name), 1)

		if err != nil {
			t.Fatalf("Sync %s: %v", step.name, err)
		}
	}

	for range 2 {
		err := o.SyncDir()
		expectSyncDir(t, r.take(), dir, 1)

		if err != nil {
			t.Fatalf("SyncDir: %v", err)
		}
	}
}

// TestSyncError checks a failed full sync is reported and not retried
// (RFC 1 §5.2: "a failure to sync **MUST** be reported to the caller, and
// **MUST NOT** be recorded as success and retried silently"): the error is
// returned once, after exactly one call and no other, and the next Sync makes
// exactly one new call. ENOTSUP is the case the os package's own File.Sync
// answers with a plain fsync, which leaves the record in the device's cache.
func TestSyncError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	a := openOn(t, newOS(dir, r.table()), "a")
	path := filepath.Join(dir, "a")

	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ENOTSUP} {
		r.failNext("fcntl", errno)
		err := a.Sync()
		expectFullSync(t, r.take(), path, 1)
		expectPathError(t, err, "F_FULLFSYNC", errno, path)

		err = a.Sync()
		expectFullSync(t, r.take(), path, 1)

		if err != nil {
			t.Fatalf("Sync after %v: %v", errno, err)
		}
	}
}

// TestSyncInterrupted checks a full sync interrupted before it completed is
// made again, and that Sync returns the repeated call's own result.
func TestSyncInterrupted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	a := openOn(t, newOS(dir, r.table()), "a")
	path := filepath.Join(dir, "a")

	r.failNext("fcntl", syscall.EINTR, syscall.EINTR)
	err := a.Sync()
	expectFullSync(t, r.take(), path, 3)

	if err != nil {
		t.Fatalf("Sync interrupted twice: %v", err)
	}

	r.failNext("fcntl", syscall.EINTR, syscall.EIO)
	err = a.Sync()
	expectFullSync(t, r.take(), path, 2)
	expectPathError(t, err, "F_FULLFSYNC", syscall.EIO, path)
}

// TestSyncCloseDuringSync checks a Close made while Sync's fcntl is under way
// does not release the descriptor under it: the file closed and another
// opened from inside the call, the full sync still acts on the first file.
func TestSyncCloseDuringSync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var other *os.File
	t.Cleanup(func() {
		if other != nil {
			_ = other.Close()
		}
	})

	dir := t.TempDir()
	r := &recorder{}
	table := r.table()
	var a *file
	fcntl := table.fcntl
	table.fcntl = func(fd uintptr, cmd, arg int) (int, error) {
		if err := a.Close(); err != nil {
			t.Errorf("Close during Sync: %v", err)
		}

		var err error

		if other, err = os.Create(filepath.Join(dir, "b")); err != nil {
			t.Fatal(err)
		}

		return fcntl(fd, cmd, arg)
	}

	a = openOn(t, newOS(dir, table), "a")

	err := a.Sync()
	expectFullSync(t, r.take(), filepath.Join(dir, "a"), 1)

	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// TestSyncDirError is TestSyncError for SyncDir. A failed full sync still
// closes the descriptor it opened, and a failed open makes no further call.
func TestSyncDirError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ENOTSUP} {
		r.failNext("fcntl", errno)
		err := o.SyncDir()
		expectSyncDir(t, r.take(), dir, 1)
		expectPathError(t, err, "F_FULLFSYNC", errno, dir)

		err = o.SyncDir()
		expectSyncDir(t, r.take(), dir, 1)

		if err != nil {
			t.Fatalf("SyncDir after %v: %v", errno, err)
		}
	}

	r.failNext("open", syscall.EMFILE)
	err := o.SyncDir()

	if log := r.take(); len(log) != 1 || log[0].op != "open" {
		t.Fatalf("want only the failed open, got %v", log)
	}

	expectPathError(t, err, "open", syscall.EMFILE, dir)
}

// TestSyncDirCloseError checks a close error after a good full sync fails the
// SyncDir with the close's error alone, and that when both the full sync and
// the close of the directory fail, SyncDir returns both errors and makes no
// further call.
func TestSyncDirCloseError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	r.failNext("close", syscall.EBADF)
	err := o.SyncDir()
	expectSyncDir(t, r.take(), dir, 1)
	expectPathError(t, err, "close", syscall.EBADF, dir)

	r.failNext("fcntl", syscall.EIO)
	r.failNext("close", syscall.ENOSPC)
	err = o.SyncDir()
	expectSyncDir(t, r.take(), dir, 1)

	if !hasPathError(err, syscall.EIO) || !hasPathError(err, syscall.ENOSPC) {
		t.Fatalf(
			"got %v, want the full sync's EIO and the close's ENOSPC, each an *fs.PathError",
			err,
		)
	}

	// A caller that asks whether the sync failed must find it, whatever
	// else failed beside it.
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("got %v, want the full sync's EIO reachable with errors.Is", err)
	}
}

// TestSyncDirInterrupted is TestSyncInterrupted for SyncDir's full sync, and
// checks an interrupted open of the directory is made again. The close is
// never repeated: a close that reported EINTR may have released the
// descriptor already.
func TestSyncDirInterrupted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	r.failNext("fcntl", syscall.EINTR, syscall.EINTR)
	err := o.SyncDir()
	expectSyncDir(t, r.take(), dir, 3)

	if err != nil {
		t.Fatalf("SyncDir interrupted twice: %v", err)
	}

	r.failNext("fcntl", syscall.EINTR, syscall.EIO)
	err = o.SyncDir()
	expectSyncDir(t, r.take(), dir, 2)
	expectPathError(t, err, "F_FULLFSYNC", syscall.EIO, dir)

	r.failNext("close", syscall.EINTR)
	err = o.SyncDir()
	expectSyncDir(t, r.take(), dir, 1)
	expectPathError(t, err, "close", syscall.EINTR, dir)

	r.failNext("open", syscall.EINTR, syscall.EINTR)
	err = o.SyncDir()
	log := r.take()

	if len(log) < 2 ||
		log[0].op != "open" || log[0].fd != -1 ||
		log[1].op != "open" || log[1].fd != -1 {
		t.Fatalf("want the two interrupted opens first, got %v", log)
	}

	expectSyncDir(t, log[2:], dir, 1)

	if err != nil {
		t.Fatalf("SyncDir with its open interrupted twice: %v", err)
	}
}
