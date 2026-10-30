package platform

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// pattern returns n bytes whose values do not repeat within 251 bytes, so a
// byte moved, lost or zeroed is seen.
func pattern(n int) []byte {
	p := make([]byte, n)

	for i := range p {
		p[i] = byte(i%251 + 1)
	}

	return p
}

// errOp is the Op of the error Allocate returns when the table entry named
// by the key fails.
var errOp = map[string]string{
	"fcntlFstore": "F_PREALLOCATE",
	"fstat":       "fstat",
	"ftruncate":   "ftruncate",
}

// ops returns the op of each call of log, in order.
func ops(log []call) []string {
	out := make([]string, len(log))

	for i, c := range log {
		out[i] = c.op
	}

	return out
}

// expectAllocate fails t unless log is exactly Allocate's calls for a range
// ending at end, all on one descriptor of path: fcntl with F_PREALLOCATE
// asking for all of end bytes from the physical end of the file, then fstat,
// then, if truncate, ftruncate to end.
func expectAllocate(t *testing.T, log []call, path string, end int64, truncate bool) {
	t.Helper()

	want := []string{"fcntlFstore", "fstat"}

	if truncate {
		want = append(want, "ftruncate")
	}

	if !slices.Equal(ops(log), want) {
		t.Fatalf("want %v on %s, got %v", want, filepath.Base(path), log)
	}

	dev, ino := identity(t, path)

	for _, c := range log {
		expectOn(t, c, dev, ino, path)

		if c.fd != log[0].fd {
			t.Fatalf("%v: want every call on the descriptor %v used", c, log[0])
		}
	}

	pre := log[0]

	if pre.cmd != unix.F_PREALLOCATE {
		t.Fatalf("%v: want the command F_PREALLOCATE (%d)", pre, unix.F_PREALLOCATE)
	}

	// F_ALLOCATEALL alone: all of the space or none, and no request for
	// contiguous space, which RFC 28 Appendix A does not ask for.
	if pre.store.Flags != unix.F_ALLOCATEALL {
		t.Fatalf("%v: want the flags F_ALLOCATEALL (%#x) alone", pre, unix.F_ALLOCATEALL)
	}

	// The kernel refuses any other offset in this mode.
	if pre.store.Posmode != unix.F_PEOFPOSMODE || pre.store.Offset != 0 {
		t.Fatalf("%v: want F_PEOFPOSMODE (%d) with offset 0", pre, unix.F_PEOFPOSMODE)
	}

	if pre.store.Length != end {
		t.Fatalf(
			"%v: want a length of %d, the end of the range, whatever the file holds",
			pre,
			end,
		)
	}

	// What the kernel filled in after the call. The fcntl(2) manual has
	// F_ALLOCATEALL "Allocate all requested space or no space at all", so a
	// success reports at least the length asked for. Checked here, on the
	// filesystem, and never by Allocate itself.
	if pre.store.Bytesalloc < pre.store.Length {
		t.Fatalf(
			"%v: the kernel allocated %d bytes, fewer than the %d asked for",
			pre,
			pre.store.Bytesalloc,
			end,
		)
	}

	if truncate && log[2].size != end {
		t.Fatalf("%v: want the size set to %d, the end of the range", log[2], end)
	}
}

// expectBlocks fails t unless the filesystem holds blocks for at least the
// first end bytes of the file at path.
func expectBlocks(t *testing.T, path string, end int64) {
	t.Helper()

	var st unix.Stat_t

	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}

	if st.Blocks*512 < end {
		t.Fatalf(
			"%s holds %d blocks of 512 bytes, want at least the %d bytes allocated",
			filepath.Base(path),
			st.Blocks,
			end,
		)
	}
}

// expectContent fails t unless the file at path holds want and nothing more.
func expectContent(t *testing.T, path string, want []byte) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(want) {
		t.Fatalf("%s holds %d bytes, want %d", filepath.Base(path), len(got), len(want))
	}

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s holds %#x at %d, want %#x", filepath.Base(path), got[i], i, want[i])
		}
	}
}

// TestAllocateCalls checks the calls each Allocate makes on macOS (RFC 28
// Appendix A: "`fcntl(F_PREALLOCATE)` then setting the size on macOS"): one
// F_PREALLOCATE asking for the whole range with F_ALLOCATEALL, then the size
// set to the range's end where the file ends before it, never shrinking a
// file that already reaches past it. The bytes a write had put in the file
// stay, and every other byte up to the end reads as zeros (RFC 28 §2.2: "An
// allocated region no record has reached reads as zeros.").
func TestAllocateCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	for i, tc := range []struct {
		name     string
		held     int // bytes written from offset 0 before the call
		off, n   int64
		truncate bool
	}{
		{"an empty file", 0, 0, 3*4096 + 5, true},
		{"a range past zero", 0, 4096, 2 * 4096, true},
		{"a file holding bytes, a range past them", 10, 4096, 2 * 4096, true},
		{"a range that starts inside the bytes held", 4096 + 7, 4096, 4096, true},
		{"a range inside the bytes held", 3 * 4096, 0, 4096, false},
		{"a range that ends where the bytes held end", 2 * 4096, 4096, 4096, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("a%d", i)
			path := filepath.Join(dir, name)
			f := openOn(t, o, name)
			held := pattern(tc.held)

			if _, err := f.f.WriteAt(held, 0); err != nil {
				t.Fatal(err)
			}

			r.take()
			err := f.Allocate(tc.off, tc.n)
			expectAllocate(t, r.take(), path, tc.off+tc.n, tc.truncate)

			if err != nil {
				t.Fatalf("Allocate(%d, %d): %v", tc.off, tc.n, err)
			}

			want := make([]byte, max(int64(tc.held), tc.off+tc.n))
			copy(want, held)
			expectContent(t, path, want)
			expectBlocks(t, path, tc.off+tc.n)
		})
	}
}

// TestAllocateError checks a failed call fails the Allocate with its own
// error, as an *fs.PathError naming the file's full path, and that no call
// follows it: a refused F_PREALLOCATE leaves the size alone.
func TestAllocateError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	all := []string{"fcntlFstore", "fstat", "ftruncate"}

	for i, tc := range []struct {
		op    string
		errno syscall.Errno
	}{
		{"fcntlFstore", syscall.ENOSPC},
		{"fcntlFstore", syscall.EIO},
		{"fcntlFstore", syscall.ENOTSUP},
		{"fstat", syscall.EIO},
		{"ftruncate", syscall.EIO},
		{"ftruncate", syscall.EFBIG},
	} {
		t.Run(fmt.Sprintf("%s %v", tc.op, tc.errno), func(t *testing.T) {
			name := fmt.Sprintf("a%d", i)
			path := filepath.Join(dir, name)
			f := openOn(t, o, name)

			r.failNext(tc.op, tc.errno)
			err := f.Allocate(0, 4096)
			log := r.take()

			if want := all[:slices.Index(all, tc.op)+1]; !slices.Equal(ops(log), want) {
				t.Fatalf("want %v, got %v", want, log)
			}

			expectPathError(t, err, errOp[tc.op], tc.errno, path)

			if tc.op != "ftruncate" {
				expectContent(t, path, nil)
			}
		})
	}
}

// TestAllocateInterrupted checks each of Allocate's calls interrupted by a
// signal is made again, twice in a row, and that the repeated call's own
// result is what counts: an error after an interruption is returned, and no
// call follows it.
func TestAllocateInterrupted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	all := []string{"fcntlFstore", "fstat", "ftruncate"}

	for i, op := range all {
		t.Run(op, func(t *testing.T) {
			name := fmt.Sprintf("a%d", i)
			path := filepath.Join(dir, name)
			f := openOn(t, o, name)

			r.failNext(op, syscall.EINTR, syscall.EINTR)
			err := f.Allocate(0, 4096)
			log := r.take()

			want := slices.Insert(slices.Clone(all), i, op, op)

			if !slices.Equal(ops(log), want) {
				t.Fatalf("want %v, got %v", want, log)
			}

			if err != nil {
				t.Fatalf("Allocate with %s interrupted twice: %v", op, err)
			}

			expectContent(t, path, make([]byte, 4096))

			g := openOn(t, o, name+"b")
			r.failNext(op, syscall.EINTR, syscall.EIO)
			err = g.Allocate(0, 4096)
			log = r.take()

			want = slices.Insert(slices.Clone(all[:i+1]), i, op)

			if !slices.Equal(ops(log), want) {
				t.Fatalf("want %v, got %v", want, log)
			}

			expectPathError(t, err, errOp[op], syscall.EIO, path+"b")
		})
	}
}

// TestAllocateInvalid checks a range no file can hold fails with EINVAL, and
// an empty range does nothing, both before any system call.
func TestAllocateInvalid(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := openOn(t, newOS(dir, r.table()), "a")
	path := filepath.Join(dir, "a")

	for _, rng := range [][2]int64{
		{-1, 1},
		{0, -1},
		{1, math.MaxInt64},
		{math.MaxInt64, 1},
	} {
		err := f.Allocate(rng[0], rng[1])
		expectPathError(t, err, "F_PREALLOCATE", syscall.EINVAL, path)

		if log := r.take(); len(log) != 0 {
			t.Fatalf("Allocate(%d, %d) made %v, want no call", rng[0], rng[1], log)
		}
	}

	if err := f.Allocate(5, 0); err != nil {
		t.Fatalf("Allocate of an empty range: %v", err)
	}

	if log := r.take(); len(log) != 0 {
		t.Fatalf("Allocate of an empty range made %v, want no call", log)
	}

	expectContent(t, path, nil)
}

// TestAllocateCloseDuringAllocate checks a Close made while Allocate's
// F_PREALLOCATE is under way does not release the descriptor under it: the
// file closed and another opened from inside the call, every later call still
// acts on the first file.
func TestAllocateCloseDuringAllocate(t *testing.T) {
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
	fcntlFstore := table.fcntlFstore
	table.fcntlFstore = func(fd uintptr, cmd int, fstore *unix.Fstore_t) error {
		if err := a.Close(); err != nil {
			t.Errorf("Close during Allocate: %v", err)
		}

		var err error

		if other, err = os.Create(filepath.Join(dir, "b")); err != nil {
			t.Fatal(err)
		}

		return fcntlFstore(fd, cmd, fstore)
	}

	a = openOn(t, newOS(dir, table), "a")

	err := a.Allocate(0, 4096)
	expectAllocate(t, r.take(), filepath.Join(dir, "a"), 4096, true)

	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	expectContent(t, filepath.Join(dir, "b"), nil)
}

// TestAllocateZeros checks Allocate on the operating system's own storage,
// with no table swapped: the size reaches the range's end, every byte no write
// has reached reads as zeros (RFC 28 §2.2), the filesystem holds blocks for
// the whole range, and a write into the range does not grow the file. On
// macOS F_PREALLOCATE allocates and zero-fills nothing itself: the zeros are
// the filesystem's, from setting the size (RFC 28 Appendix A asks for no fill
// there).
func TestAllocateZeros(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	o := NewOS(dir)

	for _, tc := range []struct {
		name   string
		held   string
		off, n int64
	}{
		{"a", "record", 6, 2*4096 + 3},
		{"b", "", 4096, 3*4096 + 5},
	} {
		f, err := o.Create(tc.name)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		t.Cleanup(func() {
			_ = f.Close()
		})

		if _, err := f.WriteAt([]byte(tc.held), 0); err != nil {
			t.Fatal(err)
		}

		if err := f.Allocate(tc.off, tc.n); err != nil {
			t.Fatalf("Allocate(%d, %d): %v", tc.off, tc.n, err)
		}

		end := tc.off + tc.n
		path := filepath.Join(dir, tc.name)
		want := make([]byte, end)
		copy(want, tc.held)
		expectContent(t, path, want)

		expectBlocks(t, path, end)

		if _, err := f.WriteAt([]byte("tail"), end-4); err != nil {
			t.Fatal(err)
		}

		copy(want[end-4:], "tail")
		expectContent(t, path, want)
	}
}
