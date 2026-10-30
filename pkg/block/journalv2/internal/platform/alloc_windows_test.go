package platform

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// maxZeroWrite is the most one zero write may ask for, so a segment's fill
// never needs a buffer the size of the segment.
const maxZeroWrite = 1 << 20

// shortNext makes the next writeFile calls short: one per entry, writing half
// of what it was asked for when the entry is true, nothing when it is false.
func (r *recorder) shortNext(half ...bool) {
	r.mu.Lock()

	defer r.mu.Unlock()

	r.short = append(r.short, half...)
}

// expectAllocate fails t unless log is exactly Allocate's calls for the range
// [off, off+n) on a file before bytes long, all on the file's handle h and
// inside its Control callback: the file's size read; then, only when before
// is below the range's end, the allocation size and then the end of file set
// to that end, so no call that could shrink the file runs on a longer one;
// then zero writes, each at the offset its overlapped names, covering the
// range exactly, each asking for 1 MiB or the rest of the range if less, and
// nothing after them: no FlushFileBuffers, since the storage.File contract
// leaves durability to the caller's Sync ("Sync makes every WriteAt, Allocate
// and Truncate that returned before it durable"). A short write is repeated
// from where it stopped, so the writes are checked against what each wrote,
// which written gives per write, in order; nil means each wrote all it asked
// for.
func expectAllocate(
	t *testing.T,
	log []call,
	h windows.Handle,
	before int64,
	off int64,
	n int64,
	written []int,
) {
	t.Helper()

	end := off + n
	head := []string{"getFileInformationByHandle"}

	if before < end {
		head = append(head, "setFileInformationByHandle", "setFileInformationByHandle")
	}

	for _, c := range log {
		if c.op == "flushFileBuffers" {
			t.Fatalf("%v: Allocate flushed; the caller's Sync does that; log %v", c, log)
		}
	}

	if len(log) < len(head)+1 {
		t.Fatalf("want %v and zero writes, got %v", head, log)
	}

	for _, c := range log {
		expectOnFile(t, c, h, log)
	}

	for i, op := range head {
		if log[i].op != op {
			t.Fatalf("call %d is %v, want %s; log %v", i, log[i], op, log)
		}
	}

	if before < end {
		for i, class := range []uint32{windows.FileAllocationInfo, windows.FileEndOfFileInfo} {
			c := log[1+i]

			if c.class != class || c.value != end {
				t.Fatalf("%v: want class %d set to %d; log %v", c, class, end, log)
			}
		}
	}

	writes := log[len(head):]

	if written != nil && len(written) != len(writes) {
		t.Fatalf("want %d writes, got %v", len(written), writes)
	}

	at := off

	for i, c := range writes {
		if c.op != "writeFile" || !c.zeros {
			t.Fatalf("%v: want a zero write; log %v", c, log)
		}

		if c.off != at {
			t.Fatalf("%v: want a write at %d, where the previous one stopped; log %v", c, at, log)
		}

		if want := min(end-at, maxZeroWrite); int64(c.n) != want {
			t.Fatalf(
				"%v: want a write of %d bytes, 1 MiB or the rest of the range; log %v",
				c,
				want,
				log,
			)
		}

		w := c.n

		if written != nil {
			w = written[i]
		}

		at += int64(w)
	}

	if at != end {
		t.Fatalf("the zero writes stop at %d, want %d; log %v", at, end, log)
	}
}

// fill writes old bytes, 0xa5, over the first n bytes of f, as a previous
// life of a segment would leave them.
func fill(t *testing.T, f *file, n int64) {
	t.Helper()

	if n == 0 {
		return
	}

	if _, err := f.f.WriteAt(bytes.Repeat([]byte{0xa5}, int(n)), 0); err != nil {
		t.Fatal(err)
	}
}

// TestAllocateCalls checks the calls Allocate makes, in order, with their
// sizes (RFC 28 Appendix A: "setting the file's allocation size, then its end
// of file"): the size read, the allocation size and the end of file set to
// the end of the range only on a shorter file, then zeros written over every
// byte of the range, and no flush. It never sets the valid data
// length: the zero writes move it.
func TestAllocateCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name    string
		before  int64 // bytes written before Allocate
		off, n  int64
		short   []bool // short writes queued, as recorder.shortNext takes them
		written func(asked []int) []int
	}{
		{
			name: "one small range",
			n:    4096,
		},
		{
			name: "a range over several megabytes, not a whole number of them",
			n:    3<<20 + 5,
		},
		{
			name: "an odd range at an offset",
			off:  4096,
			n:    3*4096 + 5,
		},
		{
			name:   "a range over written bytes",
			before: 8192,
			n:      3 * 4096,
		},
		{
			name:   "a file already at the range's end",
			before: 4096 + 3*4096 + 5,
			off:    4096,
			n:      3*4096 + 5,
		},
		{
			name:   "a file past the range's end",
			before: 4096 + 3*4096 + 5 + 4096,
			off:    4096,
			n:      3*4096 + 5,
		},
		{
			name:  "a short write continues where it stopped",
			n:     3*4096 + 5,
			short: []bool{true},
			written: func(asked []int) []int {
				if len(asked) == 0 {
					return nil
				}

				w := slices.Clone(asked)
				w[0] /= 2

				return w
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := &recorder{}
			f := openOn(t, newOS(dir, r.table()), "a")
			fill(t, f, tc.before)

			r.shortNext(tc.short...)
			err := f.Allocate(tc.off, tc.n)
			log := r.take()

			var written []int

			if tc.written != nil {
				asked := make([]int, 0, len(log))

				for _, c := range log {
					if c.op == "writeFile" {
						asked = append(asked, c.n)
					}
				}

				written = tc.written(asked)
			}

			expectAllocate(t, log, handleOf(t, f), tc.before, tc.off, tc.n, written)

			if err != nil {
				t.Fatalf("Allocate: %v", err)
			}
		})
	}
}

// TestAllocateAbove4GiB checks Allocate reads a size, sets the sizes and
// writes at offsets above 4 GiB with the high 32 bits kept, on a dry table
// that reports the size and reaches no file, so no 4 GiB file is made. Two
// halves combined wrong, or a high half dropped, read a file past the range
// as shorter, set a wrong size, or write at the wrong offset.
func TestAllocateAbove4GiB(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name   string
		before int64 // the size the dry size read reports
		off, n int64
	}{
		{
			name:   "a file past the range's end",
			before: 1<<32 + 3*4096,
			off:    1<<32 + 4096,
			n:      4096,
		},
		{
			name:   "a shorter file",
			before: 1<<32 + 5,
			off:    1<<32 - 4096,
			n:      3 * 4096,
		},
		{
			name:   "a range over several megabytes past 4 GiB",
			before: 3 << 32,
			off:    1<<33 + 7,
			n:      2<<20 + 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{
				dry:  true,
				size: tc.before,
			}
			f := openOn(t, newOS(t.TempDir(), r.table()), "a")

			err := f.Allocate(tc.off, tc.n)
			expectAllocate(t, r.take(), handleOf(t, f), tc.before, tc.off, tc.n, nil)

			if err != nil {
				t.Fatalf("Allocate: %v", err)
			}
		})
	}
}

// TestAllocateError checks each of Allocate's calls failing in turn: the
// error is returned once, as an *fs.PathError naming the file's full path,
// after exactly the calls before it and that one, and no call follows it: a
// failed call is never repeated.
func TestAllocateError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const (
		get   = "getFileInformationByHandle"
		set   = "setFileInformationByHandle"
		write = "writeFile"
	)

	for _, tc := range []struct {
		name  string
		op    string
		errs  []error // queued for op; a nil one lets that call through
		errno windows.Errno
		calls []string // the ops logged, in order
	}{
		{
			name:  "the size read",
			op:    get,
			errs:  []error{windows.ERROR_ACCESS_DENIED},
			errno: windows.ERROR_ACCESS_DENIED,
			calls: []string{get},
		},
		{
			name:  "the allocation size",
			op:    set,
			errs:  []error{windows.ERROR_DISK_FULL},
			errno: windows.ERROR_DISK_FULL,
			calls: []string{get, set},
		},
		{
			name:  "the end of file",
			op:    set,
			errs:  []error{nil, windows.ERROR_DISK_FULL},
			errno: windows.ERROR_DISK_FULL,
			calls: []string{get, set, set},
		},
		{
			name:  "the zero write",
			op:    write,
			errs:  []error{windows.ERROR_IO_DEVICE},
			errno: windows.ERROR_IO_DEVICE,
			calls: []string{get, set, set, write},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := &recorder{}
			f := openOn(t, newOS(dir, r.table()), "a")

			r.failNext(tc.op, tc.errs...)
			err := f.Allocate(0, 4096)
			log := r.take()

			ops := make([]string, len(log))

			for i, c := range log {
				ops[i] = c.op
			}

			if !slices.Equal(ops, tc.calls) {
				t.Fatalf("calls %v, want exactly %v", log, tc.calls)
			}

			expectPathError(t, err, tc.errno, filepath.Join(dir, "a"))
		})
	}

	// A write that reports no byte written and no error would repeat for
	// ever; it is reported as io.ErrUnexpectedEOF, as the os package reports
	// it, and no write follows it.
	t.Run("a write of nothing", func(t *testing.T) {
		dir := t.TempDir()
		r := &recorder{}
		f := openOn(t, newOS(dir, r.table()), "a")

		r.shortNext(false)
		err := f.Allocate(0, 4096)
		log := r.take()

		if len(log) != 4 || log[3].op != write {
			t.Fatalf("calls %v, want the size read, the two sizes and one write", log)
		}

		var pe *fs.PathError

		if !errors.As(err, &pe) ||
			!errors.Is(err, io.ErrUnexpectedEOF) ||
			pe.Path != filepath.Join(dir, "a") {
			t.Fatalf(
				"got %v, want an *fs.PathError naming the file and wrapping io.ErrUnexpectedEOF",
				err,
			)
		}
	})
}

// TestAllocateInvalidRange checks a range no file can hold fails before any call,
// with ERROR_INVALID_PARAMETER in an *fs.PathError naming the file, and that
// an empty range makes no call and succeeds.
func TestAllocateInvalidRange(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := openOn(t, newOS(dir, r.table()), "a")

	for _, rg := range []struct {
		off, n int64
	}{
		{-1, 4096},
		{0, -1},
		{math.MaxInt64, 1},
		{1, math.MaxInt64},
	} {
		err := f.Allocate(rg.off, rg.n)

		if log := r.take(); len(log) != 0 {
			t.Fatalf("Allocate(%d, %d) made %v, want no call", rg.off, rg.n, log)
		}

		expectPathError(t, err, windows.ERROR_INVALID_PARAMETER, filepath.Join(dir, "a"))
	}

	err := f.Allocate(4096, 0)

	if log := r.take(); len(log) != 0 {
		t.Fatalf("Allocate of an empty range made %v, want no call", log)
	}

	if err != nil {
		t.Fatalf("Allocate of an empty range: %v", err)
	}
}

// TestAllocateOnDisk allocates a range on the real filesystem over bytes
// a previous life left in the file, and reads the whole file back: every byte
// of the range reads as zero (RFC 28 §2.2: "An allocated region no record has
// reached reads as zeros."), the bytes before it are untouched, a shorter file
// grows to the range's end, and a file already at or past that end keeps its
// size and its bytes after the range. NTFS reads zeros past the valid data
// length on its own, so the old bytes are what show a zero write missing.
func TestAllocateOnDisk(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const (
		off = 4
		end = 2<<20 + 4096 + 5
	)

	for _, tc := range []struct {
		name   string
		before int64
	}{
		{
			name:   "a shorter file",
			before: 1 << 20,
		},
		{
			name:   "a file at the range's end",
			before: end,
		},
		{
			name:   "a file past the range's end",
			before: end + 4096,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := openOn(t, NewOS(t.TempDir()), "a")
			fill(t, f, tc.before)

			if err := f.Allocate(off, end-off); err != nil {
				t.Fatalf("Allocate: %v", err)
			}

			// The storage.File contract leaves durability to the caller:
			// "Sync makes every WriteAt, Allocate and Truncate that returned
			// before it durable".
			if err := f.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}

			size, err := f.Size()
			if err != nil {
				t.Fatal(err)
			}

			if want := max(tc.before, end); size != want {
				t.Fatalf("size %d after Allocate, want %d", size, want)
			}

			got := make([]byte, size)

			if n, err := f.ReadAt(got, 0); int64(n) != size || err != nil {
				t.Fatalf("ReadAt of the whole file: %d, %v", n, err)
			}

			for i, b := range got {
				want := byte(0xa5)

				if i >= off && i < end {
					want = 0
				}

				if b != want {
					t.Fatalf(
						"byte %d is %#x after Allocate(%d, %d), want %#x",
						i,
						b,
						off,
						end-off,
						want,
					)
				}
			}
		})
	}
}

// TestAllocateWriteReopen runs a segment's life on the real filesystem
// through the storage's own Create and Open: create, allocate, sync, write a
// record in the middle, sync, close, reopen and read the whole file back. The file
// is the allocated length and only the record's bytes are not zero.
func TestAllocateWriteReopen(t *testing.T) {
	var held []*file
	t.Cleanup(func() {
		runtime.KeepAlive(held)
	})
	prof.Watch(t, prof.Options{})

	const (
		name   = "00000000000000000001.seg"
		end    = 2<<20 + 5
		at     = 1<<20 + 3
		record = "record"
	)

	o := NewOS(t.TempDir())

	f, err := o.Create(name)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	held = append(held, f.(*file))

	if err := f.Allocate(0, end); err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	// Allocate does not sync: the storage.File contract leaves it to the
	// caller, as here.
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync after Allocate: %v", err)
	}

	if _, err := f.WriteAt([]byte(record), at); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	g, err := o.Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	held = append(held, g.(*file))

	size, err := g.Size()
	if err != nil {
		t.Fatal(err)
	}

	if size != end {
		t.Fatalf("size %d after the reopen, want %d", size, end)
	}

	got := make([]byte, end)

	if n, err := g.ReadAt(got, 0); n != end || err != nil {
		t.Fatalf("ReadAt of the whole file: %d, %v", n, err)
	}

	want := make([]byte, end)
	copy(want[at:], record)

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf(
				"byte %d is %#x, want %#x: only %q at %d is not zero",
				i,
				got[i],
				want[i],
				record,
				at,
			)
		}
	}

	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
