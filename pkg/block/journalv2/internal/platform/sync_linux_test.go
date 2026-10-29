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

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"golang.org/x/sys/unix"
)

// call is one entry of a recorder's log.
type call struct {
	op   string // open, fsync, fdatasync or close
	fd   int    // the descriptor acted on, or open's result; -1 for a failed open
	path string // open: the path opened
	mode int    // open: its flags

	// fsync and fdatasync: the file the descriptor referred to at the call,
	// so a sync on a stale or wrong descriptor is seen.
	dev, ino uint64
}

func (c call) String() string {
	switch c.op {
	case "open":
		return fmt.Sprintf("open(%s, %#x) = %d", c.path, c.mode, c.fd)
	case "close":
		return fmt.Sprintf("close(%d)", c.fd)
	default:
		return fmt.Sprintf("%s(%d) on dev %d ino %d", c.op, c.fd, c.dev, c.ino)
	}
}

// recorder is a table that logs every call and makes it. An error queued for
// an op is returned by its next call instead: open, fsync and fdatasync then
// do not run, and close runs, as Linux releases the descriptor even when
// close reports an error.
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

func (r *recorder) table() calls {
	return calls{
		open: func(path string, mode int, perm uint32) (int, error) {
			if r.queued("open") {
				return -1, r.add(call{op: "open", fd: -1, path: path, mode: mode})
			}

			fd, err := unix.Open(path, mode, perm)
			if err != nil {
				fd = -1
			}
			_ = r.add(call{op: "open", fd: fd, path: path, mode: mode})

			return fd, err
		},
		fsync: func(fd int) error {
			return r.sync("fsync", fd, unix.Fsync)
		},
		fdatasync: func(fd int) error {
			return r.sync("fdatasync", fd, unix.Fdatasync)
		},
		close: func(fd int) error {
			queued := r.add(call{op: "close", fd: fd})
			err := unix.Close(fd)
			if queued != nil {
				return queued
			}

			return err
		},
	}
}

func (r *recorder) sync(op string, fd int, real func(int) error) error {
	c := call{op: op, fd: fd}

	var st unix.Stat_t
	if unix.Fstat(fd, &st) == nil {
		c.dev, c.ino = uint64(st.Dev), st.Ino
	}

	if err := r.add(c); err != nil {
		return err
	}

	return real(fd)
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
	t.Cleanup(func() { _ = f.Close() })

	return sf
}

// expectSync fails t unless log is exactly n calls of op, each on a
// descriptor of path.
func expectSync(t *testing.T, log []call, op, path string, n int) {
	t.Helper()

	if len(log) != n {
		t.Fatalf("want exactly %d %s on %s, got %v", n, op, filepath.Base(path), log)
	}

	dev, ino := identity(t, path)
	for _, c := range log {
		if c.op != op {
			t.Fatalf("want exactly %d %s on %s, got %v", n, op, filepath.Base(path), log)
		}

		if c.dev != dev || c.ino != ino {
			t.Fatalf("%v: the descriptor is not %s's (dev %d ino %d)", c, path, dev, ino)
		}
	}
}

// expectSyncDir fails t unless log is exactly: dir opened for reading as a
// directory and not inherited by a child process, then fsyncs fsync calls on
// that descriptor, which refers to dir, then that descriptor closed.
func expectSyncDir(t *testing.T, log []call, dir string, fsyncs int) {
	t.Helper()

	if len(log) != fsyncs+2 || log[0].op != "open" || log[len(log)-1].op != "close" {
		t.Fatalf("want open, %d fsync and close of the directory, got %v", fsyncs, log)
	}

	open, closed := log[0], log[len(log)-1]
	if open.path != dir {
		t.Fatalf("%v: want %s opened", open, dir)
	}

	// O_PATH is refused: fsync on such a descriptor fails with EBADF.
	if open.mode&unix.O_ACCMODE != unix.O_RDONLY || open.mode&unix.O_DIRECTORY == 0 ||
		open.mode&unix.O_CLOEXEC == 0 || open.mode&unix.O_PATH != 0 {
		t.Fatalf("%v: want O_RDONLY with O_DIRECTORY and O_CLOEXEC, and no O_PATH", open)
	}

	if closed.fd != open.fd {
		t.Fatalf("%v: want the descriptor %v returned closed", closed, open)
	}

	dev, ino := identity(t, dir)
	for _, c := range log[1 : len(log)-1] {
		if c.op != "fsync" || c.fd != open.fd {
			t.Fatalf("%v: want fsync on the descriptor %v returned", c, open)
		}

		if c.dev != dev || c.ino != ino {
			t.Fatalf("%v: the descriptor is not the directory's (dev %d ino %d)", c, dev, ino)
		}
	}
}

// expectPathError fails t unless err is, or joins, an *fs.PathError wrapping
// errno, the shape the platform's own errors take.
func expectPathError(t *testing.T, err error, errno syscall.Errno) {
	t.Helper()

	if !errors.Is(err, errno) || !hasPathError(err, errno) {
		t.Fatalf("got %v (%T), want an *fs.PathError wrapping %v", err, err, errno)
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

// TestSyncCalls checks the call each sync makes on Linux (RFC 28 Appendix A:
// "`fdatasync` on the file", "and `fsync` on the directory's descriptor"): one
// fdatasync on the file's own descriptor per Sync, never fsync or a range
// sync (RFC 1 §5.2), and per SyncDir one fsync on a descriptor of the
// directory that SyncDir opens and closes itself.
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
		expectSync(t, r.take(), "fdatasync", filepath.Join(dir, step.name), 1)
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

// TestSyncError checks a failed sync is reported and not retried (RFC 1 §5.2:
// "a failure to sync **MUST** be reported to the caller, and **MUST NOT** be
// recorded as success and retried silently"): the error is returned once,
// after exactly one call, and the next Sync makes exactly one new call.
func TestSyncError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	a := openOn(t, newOS(dir, r.table()), "a")

	r.failNext("fdatasync", syscall.EIO)
	err := a.Sync()
	expectSync(t, r.take(), "fdatasync", filepath.Join(dir, "a"), 1)
	expectPathError(t, err, syscall.EIO)

	err = a.Sync()
	expectSync(t, r.take(), "fdatasync", filepath.Join(dir, "a"), 1)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
}

// TestSyncInterrupted checks an fdatasync interrupted before it completed is
// made again, and that Sync returns the repeated call's own result.
func TestSyncInterrupted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	a := openOn(t, newOS(dir, r.table()), "a")

	r.failNext("fdatasync", syscall.EINTR)
	err := a.Sync()
	expectSync(t, r.take(), "fdatasync", filepath.Join(dir, "a"), 2)
	if err != nil {
		t.Fatalf("Sync interrupted once: %v", err)
	}

	r.failNext("fdatasync", syscall.EINTR, syscall.EIO)
	err = a.Sync()
	expectSync(t, r.take(), "fdatasync", filepath.Join(dir, "a"), 2)
	expectPathError(t, err, syscall.EIO)
}

// TestSyncAfterClose checks Sync on a closed file fails before any system
// call, so it never syncs whatever file has since reused the descriptor's
// number.
func TestSyncAfterClose(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := &recorder{}
	a := openOn(t, newOS(t.TempDir(), r.table()), "a")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	err := a.Sync()
	if log := r.take(); len(log) != 0 {
		t.Fatalf("Sync after Close made %v, want no call", log)
	}

	if err == nil {
		t.Fatal("Sync after Close returned nil")
	}
}

// TestSyncDirError is TestSyncError for SyncDir. A failed fsync still closes
// the descriptor it opened, and a failed open makes no further call.
func TestSyncDirError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	r.failNext("fsync", syscall.EIO)
	err := o.SyncDir()
	expectSyncDir(t, r.take(), dir, 1)
	expectPathError(t, err, syscall.EIO)

	err = o.SyncDir()
	expectSyncDir(t, r.take(), dir, 1)
	if err != nil {
		t.Fatalf("second SyncDir: %v", err)
	}

	r.failNext("open", syscall.EMFILE)
	err = o.SyncDir()
	if log := r.take(); len(log) != 1 || log[0].op != "open" {
		t.Fatalf("want only the failed open, got %v", log)
	}
	expectPathError(t, err, syscall.EMFILE)
}

// TestSyncDirCloseError checks that when both the fsync and the close of the
// directory fail, SyncDir returns both errors and makes no further call.
func TestSyncDirCloseError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	r.failNext("fsync", syscall.EIO)
	r.failNext("close", syscall.ENOSPC)
	err := o.SyncDir()
	expectSyncDir(t, r.take(), dir, 1)
	expectPathError(t, err, syscall.EIO)
	expectPathError(t, err, syscall.ENOSPC)
}

// TestSyncDirInterrupted is TestSyncInterrupted for SyncDir's fsync. The
// close is never repeated: Linux releases the descriptor even when close
// reports EINTR.
func TestSyncDirInterrupted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	r.failNext("fsync", syscall.EINTR)
	err := o.SyncDir()
	expectSyncDir(t, r.take(), dir, 2)
	if err != nil {
		t.Fatalf("SyncDir interrupted once: %v", err)
	}

	r.failNext("fsync", syscall.EINTR, syscall.EIO)
	err = o.SyncDir()
	expectSyncDir(t, r.take(), dir, 2)
	expectPathError(t, err, syscall.EIO)

	r.failNext("close", syscall.EINTR)
	err = o.SyncDir()
	expectSyncDir(t, r.take(), dir, 1)
	expectPathError(t, err, syscall.EINTR)
}
