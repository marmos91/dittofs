package platform

import (
	"bytes"
	"cmp"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"golang.org/x/sys/unix"
)

// expectAllocate fails t unless log is exactly what one Allocate(off, n) of
// the file at path makes: fallocates calls of fallocate, the last of which
// succeeded, each on the file's own descriptor, with mode 0 and the range
// asked for; then pwrite calls on the same file, each of zeros and of at most
// one chunk, whose written bytes together cover [off, off+n) exactly, with no
// gap, no overlap and no byte past either end; and nothing else, no sync.
func expectAllocate(
	t *testing.T,
	log []call,
	path string,
	off, n int64,
	fallocates int,
) {
	t.Helper()

	expectOnFile(t, log, path)

	if len(log) < fallocates {
		t.Fatalf("want %d fallocate first, got %v", fallocates, log)
	}

	// Mode 0 allocates and grows the file to cover the range:
	// FALLOC_FL_KEEP_SIZE leaves the file short, and FALLOC_FL_ZERO_RANGE and
	// FALLOC_FL_PUNCH_HOLE are other operations. RFC 28 Appendix A also allows
	// one zeroing allocation where the kernel offers one, which
	// FALLOC_FL_WRITE_ZEROES would be; the zero writes after mode 0 are the
	// path chosen here.
	for _, c := range log[:fallocates] {
		if c.op != "fallocate" || c.mode != 0 || c.off != off || c.n != n {
			t.Fatalf("%v: want fallocate(fd, 0, %d, %d), with mode 0", c, off, n)
		}
	}

	var spans [][2]int64

	for _, c := range log[fallocates:] {
		if c.op != "pwrite" {
			t.Fatalf("%v: want only zero writes after the fallocate, got %v", c, log)
		}

		if !c.zero {
			t.Fatalf("%v: a byte written is not zero", c)
		}

		if c.asked > zeroChunk {
			t.Fatalf("%v: asks for more than one chunk of %d bytes", c, zeroChunk)
		}

		if c.written > 0 {
			spans = append(spans, [2]int64{c.off, c.off + int64(c.written)})
		}
	}

	slices.SortFunc(spans, func(a, b [2]int64) int {
		return cmp.Compare(a[0], b[0])
	})

	at := off

	for _, s := range spans {
		if s[0] != at {
			t.Fatalf(
				"the zero writes cover [%d, %d) next, want them to continue at %d: %v",
				s[0],
				s[1],
				at,
				log,
			)
		}

		at = s[1]
	}

	if at != off+n {
		t.Fatalf("the zero writes end at %d, want %d: %v", at, off+n, log)
	}
}

// expectOnFile fails t unless every call in log acted on a descriptor of the
// file at path.
func expectOnFile(t *testing.T, log []call, path string) {
	t.Helper()

	dev, ino := identity(t, path)

	for i, c := range log {
		if c.dev != dev || c.ino != ino {
			t.Fatalf(
				"call %d, %v: the descriptor is not %s's (dev %d ino %d)",
				i,
				c,
				path,
				dev,
				ino,
			)
		}
	}
}

// expectZeros fails t unless the n bytes of f at off all read as zero.
func expectZeros(t *testing.T, f *file, off, n int64) {
	t.Helper()

	p := make([]byte, n)

	if got, err := f.ReadAt(p, off); int64(got) != n || err != nil {
		t.Fatalf("ReadAt %d at %d: %d, %v", n, off, got, err)
	}

	if i := slices.IndexFunc(p, nonZero); i >= 0 {
		t.Fatalf("byte %d reads %#x, want zero", off+int64(i), p[i])
	}
}

// expectBytes fails t unless f holds want at off.
func expectBytes(t *testing.T, f *file, want []byte, off int64) {
	t.Helper()

	p := make([]byte, len(want))

	if n, err := f.ReadAt(p, off); n != len(want) || err != nil || !bytes.Equal(p, want) {
		t.Fatalf("ReadAt %d at %d: %d, %v; the bytes there changed", len(want), off, n, err)
	}
}

// expectFileSize fails t unless f's size is want.
func expectFileSize(t *testing.T, f *file, want int64) {
	t.Helper()

	if got, err := f.Size(); got != want || err != nil {
		t.Fatalf("Size: %d, %v; want %d", got, err, want)
	}
}

// expectAllocatedBlocks fails t unless the filesystem has allocated at least
// want bytes to path.
func expectAllocatedBlocks(t *testing.T, path string, want int64) {
	t.Helper()

	var st unix.Stat_t

	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}

	if got := st.Blocks * 512; got < want {
		t.Fatalf("%s has %d bytes allocated, want at least %d", path, got, want)
	}
}

// writeBytes writes p into f at off.
func writeBytes(t *testing.T, f *file, p []byte, off int64) {
	t.Helper()

	if n, err := f.WriteAt(p, off); n != len(p) || err != nil {
		t.Fatalf("WriteAt %d at %d: %d, %v", len(p), off, n, err)
	}
}

// TestAllocateCalls checks Allocate on Linux is fallocate then zeros written
// over the whole range (RFC 28 Appendix A: "on Linux, `fallocate` then writing
// zeros over the whole segment"), with no sync of its own, on a range smaller
// than one chunk, on one that is a whole number of chunks, and on one that is
// not, at an offset. The zeros cover the whole range even where the file
// already did: over bytes a write put there, which then read as zeros, and
// over a range a Truncate stretched the file to cover. The file then covers
// the range, which reads as zeros, and the bytes before it are untouched.
func TestAllocateCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	head := bytes.Repeat([]byte{0xa5}, 100)

	for _, tc := range []struct {
		name    string
		off, n  int64
		prepare func(t *testing.T, f *file)
	}{
		{"short", 0, 4096 + 7, nil},
		{"whole chunks", 0, 3 * zeroChunk, nil},
		{"offset", 12345, 2*zeroChunk + 999, nil},
		{"over bytes", 100, zeroChunk + 7, func(t *testing.T, f *file) {
			writeBytes(t, f, bytes.Repeat([]byte{0xa5}, 100+zeroChunk+7), 0)
		}},
		{"over a truncate", 100, zeroChunk + 7, func(t *testing.T, f *file) {
			writeBytes(t, f, head, 0)

			if err := f.Truncate(100 + zeroChunk + 7); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		f := openOn(t, o, tc.name)
		path := filepath.Join(dir, tc.name)

		if tc.prepare != nil {
			tc.prepare(t, f)
		}

		r.take()

		err := f.Allocate(tc.off, tc.n)
		expectAllocate(t, r.take(), path, tc.off, tc.n, 1)

		if err != nil {
			t.Fatalf("%s: Allocate(%d, %d): %v", tc.name, tc.off, tc.n, err)
		}

		expectFileSize(t, f, tc.off+tc.n)
		expectAllocatedBlocks(t, path, tc.n)
		expectZeros(t, f, tc.off, tc.n)

		if tc.prepare != nil {
			expectBytes(t, f, head, 0)
		}
	}
}

// TestAllocateOnDisk checks Allocate on the operating system's own table:
// past the file's end it grows the file to cover the range, and between two
// written regions it leaves the file's size and both regions as they were.
// Either way the range reads as zeros (storage.File: "Bytes no write has
// reached read as zeros").
func TestAllocateOnDisk(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	o := NewOS(dir)
	const off, n = 4096 + 3, zeroChunk + 5
	head := bytes.Repeat([]byte{0xa5}, off)
	tail := bytes.Repeat([]byte{0x5a}, 4096)

	f := openOn(t, o, "past the end")
	writeBytes(t, f, head, 0)

	if err := f.Allocate(off, n); err != nil {
		t.Fatalf("Allocate past the end: %v", err)
	}

	expectFileSize(t, f, off+n)
	expectAllocatedBlocks(t, filepath.Join(dir, "past the end"), off+n)
	expectBytes(t, f, head, 0)
	expectZeros(t, f, off, n)

	g := openOn(t, o, "between")
	writeBytes(t, g, head, 0)
	writeBytes(t, g, tail, off+n)

	if err := g.Allocate(off, n); err != nil {
		t.Fatalf("Allocate between written bytes: %v", err)
	}

	expectFileSize(t, g, off+n+int64(len(tail)))
	expectBytes(t, g, head, 0)
	expectZeros(t, g, off, n)
	expectBytes(t, g, tail, off+n)
}

// TestAllocateShortWrites checks a zero write that writes fewer bytes than
// asked is continued from where it stopped, and that one that writes nothing
// and reports no error fails the Allocate with io.ErrUnexpectedEOF, as the os
// package's own writes do, with no further write, rather than looping for
// ever.
func TestAllocateShortWrites(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	const off, n = 7, 2*zeroChunk + 3

	f := openOn(t, o, "a")
	r.shortNext(4096, 1)

	err := f.Allocate(off, n)
	log := r.take()
	expectAllocate(t, log, filepath.Join(dir, "a"), off, n, 1)

	if err != nil {
		t.Fatalf("Allocate with short writes: %v", err)
	}

	if log[1].written != 4096 || log[2].off != off+4096 || log[2].written != 1 ||
		log[3].off != off+4097 {
		t.Fatalf(
			"want a write of 4096 at %d, then of 1 at %d, then the rest from %d: %v",
			off,
			off+4096,
			off+4097,
			log,
		)
	}

	g := openOn(t, o, "b")
	r.shortNext(0)

	err = g.Allocate(off, n)
	log = r.take()

	if len(log) != 2 || log[0].op != "fallocate" || log[1].op != "pwrite" {
		t.Fatalf("want the fallocate and the empty write, and no other call, got %v", log)
	}

	var pe *fs.PathError

	if !errors.As(err, &pe) || pe.Err != io.ErrUnexpectedEOF || pe.Op != "pwrite" ||
		pe.Path != filepath.Join(dir, "b") {
		t.Fatalf(
			"got %v, want an *fs.PathError of pwrite naming %s whose error is io.ErrUnexpectedEOF",
			err,
			filepath.Join(dir, "b"),
		)
	}
}

// TestAllocateFallocateError checks a failed fallocate is returned once, as an
// *fs.PathError of op fallocate naming the file's full path, with no zero
// write after it and no fallback to writing zeros alone, and that the next
// Allocate makes a new fallocate.
func TestAllocateFallocateError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := openOn(t, newOS(dir, r.table()), "a")
	path := filepath.Join(dir, "a")

	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ENOSPC, syscall.EOPNOTSUPP} {
		r.failNext("fallocate", errno)

		err := f.Allocate(0, 4096)

		if log := r.take(); len(log) != 1 || log[0].op != "fallocate" {
			t.Fatalf("%v: want the one failed fallocate and nothing else, got %v", errno, log)
		}

		expectPathError(t, err, errno, path)

		if pe := err.(*fs.PathError); pe.Op != "fallocate" {
			t.Fatalf("%v: op %q, want fallocate", err, pe.Op)
		}
	}

	err := f.Allocate(0, 4096)
	expectAllocate(t, r.take(), path, 0, 4096, 1)

	if err != nil {
		t.Fatalf("Allocate after the failures: %v", err)
	}
}

// TestAllocateWriteError checks a zero write that fails part way through the
// range returns its error, as an *fs.PathError of op pwrite naming the file's
// full path, and is followed by no further write.
func TestAllocateWriteError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	table := r.table()
	pwrite := table.pwrite
	var writes int
	table.pwrite = func(fd int, p []byte, off int64) (int, error) {
		n, err := pwrite(fd, p, off)

		writes++

		if writes == 1 {
			r.failNext("pwrite", syscall.EIO)
		}

		return n, err
	}

	f := openOn(t, newOS(dir, table), "a")
	path := filepath.Join(dir, "a")

	err := f.Allocate(0, 3*zeroChunk)
	log := r.take()

	if len(log) != 3 || log[0].op != "fallocate" || log[1].op != "pwrite" || log[1].written == 0 ||
		log[2].op != "pwrite" || log[2].written != 0 {
		t.Fatalf("want fallocate, a good write, the failed write and nothing more, got %v", log)
	}

	expectPathError(t, err, syscall.EIO, path)

	if pe := err.(*fs.PathError); pe.Op != "pwrite" {
		t.Fatalf("%v: op %q, want pwrite", err, pe.Op)
	}
}

// TestAllocateInterrupted checks a fallocate or a zero write interrupted by a
// signal is made again, twice in a row, as the os package does for its own
// writes, and that an interrupted fallocate followed by a failure returns the
// failure after two calls.
func TestAllocateInterrupted(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())
	const n = 2*zeroChunk + 1

	f := openOn(t, o, "a")
	r.failNext("fallocate", syscall.EINTR, syscall.EINTR)

	err := f.Allocate(0, n)
	expectAllocate(t, r.take(), filepath.Join(dir, "a"), 0, n, 3)

	if err != nil {
		t.Fatalf("Allocate with its fallocate interrupted twice: %v", err)
	}

	g := openOn(t, o, "b")
	r.failNext("pwrite", syscall.EINTR, syscall.EINTR)

	err = g.Allocate(0, n)
	log := r.take()
	expectAllocate(t, log, filepath.Join(dir, "b"), 0, n, 1)

	if err != nil {
		t.Fatalf("Allocate with a write interrupted twice: %v", err)
	}

	if len(log) < 4 || log[1].written != 0 || log[2].written != 0 || log[3].off != 0 {
		t.Fatalf("want two interrupted writes at 0 and the write made again at 0, got %v", log)
	}

	h := openOn(t, o, "c")
	r.failNext("fallocate", syscall.EINTR, syscall.EIO)

	err = h.Allocate(0, n)

	if log := r.take(); len(log) != 2 || log[0].op != "fallocate" || log[1].op != "fallocate" {
		t.Fatalf("want two fallocate and no write, got %v", log)
	}

	expectPathError(t, err, syscall.EIO, filepath.Join(dir, "c"))
}

// TestAllocateInvalidRange checks a range no file can hold fails with EINVAL
// before any system call, and that an empty range is nothing to allocate.
func TestAllocateInvalidRange(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := openOn(t, newOS(dir, r.table()), "a")

	for _, tc := range []struct {
		off, n int64
	}{
		{-1, 1},
		{0, -1},
		{math.MaxInt64, 1},
		{1, math.MaxInt64},
	} {
		err := f.Allocate(tc.off, tc.n)

		if log := r.take(); len(log) != 0 {
			t.Fatalf("Allocate(%d, %d) made %v, want no call", tc.off, tc.n, log)
		}

		expectPathError(t, err, syscall.EINVAL, filepath.Join(dir, "a"))
	}

	err := f.Allocate(4096, 0)

	if log := r.take(); len(log) != 0 {
		t.Fatalf("Allocate(4096, 0) made %v, want no call", log)
	}

	if err != nil {
		t.Fatalf("Allocate(4096, 0): %v, want nil", err)
	}

	expectFileSize(t, f, 0)
}

// TestAllocateCloseDuringAllocate checks a Close made while Allocate is under
// way, from inside its fallocate or inside its first zero write, does not
// release the descriptor under it: with the file closed and another opened,
// every call still acts on the first file. Allocate may then finish, or stop
// with an error matching fs.ErrClosed.
func TestAllocateCloseDuringAllocate(t *testing.T) {
	for _, during := range []string{"fallocate", "pwrite"} {
		t.Run(during, func(t *testing.T) {
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
			var closed bool
			closeAndReopen := func(op string) {
				if op != during || closed {
					return
				}

				closed = true

				if err := a.Close(); err != nil {
					t.Errorf("Close during Allocate: %v", err)
				}

				var err error

				if other, err = os.Create(filepath.Join(dir, "b")); err != nil {
					t.Fatal(err)
				}
			}

			fallocate, pwrite := table.fallocate, table.pwrite
			table.fallocate = func(fd int, mode uint32, off, n int64) error {
				closeAndReopen("fallocate")

				return fallocate(fd, mode, off, n)
			}
			table.pwrite = func(fd int, p []byte, off int64) (int, error) {
				closeAndReopen("pwrite")

				return pwrite(fd, p, off)
			}

			a = openOn(t, newOS(dir, table), "a")
			path := filepath.Join(dir, "a")
			const n = 2*zeroChunk + 1

			err := a.Allocate(0, n)
			log := r.take()

			if !closed {
				t.Fatalf("no %s was made, so no Close happened during it: %v", during, log)
			}

			if err == nil {
				expectAllocate(t, log, path, 0, n, 1)

				return
			}

			var pe *fs.PathError

			if !errors.As(err, &pe) || !errors.Is(err, fs.ErrClosed) || pe.Path != path {
				t.Fatalf(
					"Allocate: %v, want nil or an *fs.PathError naming %s matching fs.ErrClosed",
					err,
					path,
				)
			}

			expectOnFile(t, log, path)
		})
	}
}
