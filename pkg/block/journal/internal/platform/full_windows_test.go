package platform

import (
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// ops returns the op of each call in log, in order.
func ops(log []call) []string {
	names := make([]string, len(log))

	for i, c := range log {
		names[i] = c.op
	}

	return names
}

// TestWriteAtCalls checks WriteAt writes through the table, on the file's own
// handle inside its Control callback, at the offset its overlapped names with
// the high 32 bits kept: a short write continues where it stopped, a write of
// nothing is io.ErrUnexpectedEOF, a failed write is returned once, and a
// negative offset fails before any call.
func TestWriteAtCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	r := &recorder{}
	f := openOn(t, newOS(dir, r.table()), "a")
	h := handleOf(t, f)

	n, err := f.WriteAt([]byte("record"), 4096)
	log := r.take()

	if n != 6 || err != nil {
		t.Fatalf("WriteAt: %d, %v; want 6, nil", n, err)
	}

	if len(log) != 1 || log[0].op != "writeFile" || log[0].off != 4096 || log[0].n != 6 {
		t.Fatalf("want one writeFile of 6 bytes at 4096, got %v", log)
	}

	expectOnFile(t, log[0], h, log)

	r.shortNext(true)
	n, err = f.WriteAt([]byte("abcdef"), 0)
	log = r.take()

	if n != 6 || err != nil {
		t.Fatalf("WriteAt with a short write: %d, %v; want 6, nil", n, err)
	}

	if len(log) != 2 || log[0].off != 0 || log[0].n != 6 || log[1].off != 3 || log[1].n != 3 {
		t.Fatalf("want a write of 6 at 0, then of the 3 left at 3, got %v", log)
	}

	got := make([]byte, 6)

	if _, err := f.f.ReadAt(got, 0); err != nil || string(got) != "abcdef" {
		t.Fatalf("the file reads %q, %v; want %q", got, err, "abcdef")
	}

	r.shortNext(false)
	n, err = f.WriteAt([]byte("x"), 0)

	if log := r.take(); len(log) != 1 {
		t.Fatalf("want one writeFile, got %v", log)
	}

	if n != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a write of nothing: %d, %v; want 0 and io.ErrUnexpectedEOF", n, err)
	}

	r.failNext("writeFile", windows.ERROR_IO_DEVICE)
	n, err = f.WriteAt([]byte("x"), 0)

	if log := r.take(); len(log) != 1 {
		t.Fatalf("a failed write made %v, want exactly one writeFile", log)
	}

	if n != 0 {
		t.Fatalf("a failed write reports %d bytes, want 0", n)
	}

	expectPathError(t, err, windows.ERROR_IO_DEVICE, path)

	r.shortNext(true)
	r.failNext("writeFile", nil, windows.ERROR_IO_DEVICE)
	n, err = f.WriteAt([]byte("abcdef"), 0)

	if log := r.take(); len(log) != 2 {
		t.Fatalf("want the short write and the failed one, got %v", log)
	}

	if n != 3 {
		t.Fatalf("a write that failed after 3 bytes reports %d, want 3", n)
	}

	expectPathError(t, err, windows.ERROR_IO_DEVICE, path)

	_, err = f.WriteAt([]byte("x"), -1)

	if log := r.take(); len(log) != 0 {
		t.Fatalf("a negative offset made %v, want no call", log)
	}

	expectPathError(t, err, windows.ERROR_INVALID_PARAMETER, path)

	dry := &recorder{
		dry: true,
	}
	g := openOn(t, newOS(dir, dry.table()), "b")

	if _, err := g.WriteAt([]byte("record"), 1<<32+5); err != nil {
		t.Fatalf("WriteAt above 4 GiB: %v", err)
	}

	if log := dry.take(); len(log) != 1 || log[0].off != 1<<32+5 {
		t.Fatalf("want one writeFile at %d, got %v", int64(1<<32+5), log)
	}
}

// TestTruncateCalls checks Truncate sets the end of file through the table,
// on the file's own handle inside its Control callback, that a failed set is
// returned once, and that a negative size fails before any call.
func TestTruncateCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	r := &recorder{}
	f := openOn(t, newOS(dir, r.table()), "a")
	h := handleOf(t, f)

	if _, err := f.f.WriteAt([]byte("record and more"), 0); err != nil {
		t.Fatal(err)
	}

	err := f.Truncate(6)
	log := r.take()

	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	if len(log) != 1 ||
		log[0].op != "setFileInformationByHandle" ||
		log[0].class != windows.FileEndOfFileInfo ||
		log[0].value != 6 {
		t.Fatalf("want one end of file set to 6, got %v", log)
	}

	expectOnFile(t, log[0], h, log)

	fi, err := f.f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	if fi.Size() != 6 {
		t.Fatalf("size %d after Truncate(6)", fi.Size())
	}

	r.failNext("setFileInformationByHandle", windows.ERROR_IO_DEVICE)
	err = f.Truncate(3)

	if log := r.take(); len(log) != 1 {
		t.Fatalf("a failed Truncate made %v, want exactly one call", log)
	}

	expectPathError(t, err, windows.ERROR_IO_DEVICE, path)

	err = f.Truncate(-1)

	if log := r.take(); len(log) != 0 {
		t.Fatalf("a negative size made %v, want no call", log)
	}

	expectPathError(t, err, windows.ERROR_INVALID_PARAMETER, path)
}

// TestDiskFull checks a full volume reaches the caller as the core's
// NoSpaceError decision says a full filesystem does: an *fs.PathError naming
// the file or the directory that errors.Is matches with syscall.ENOSPC, for
// both codes Windows reports it with, from every call the platform makes
// through its table: Create, Open, WriteAt, Truncate, Size, Sync, every call
// of Allocate and of SyncDir. The error still matches its own Windows code,
// and the call is not made again. Another error is not taken for a full
// volume.
func TestDiskFull(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const (
		create = "createFile"
		flush  = "flushFileBuffers"
		shut   = "closeHandle"
		get    = "getFileInformationByHandle"
		set    = "setFileInformationByHandle"
		write  = "writeFile"
	)

	for _, code := range []syscall.Errno{
		windows.ERROR_DISK_FULL,
		windows.ERROR_HANDLE_DISK_FULL,
	} {
		for _, tc := range []struct {
			name  string
			op    string
			errs  []error // queued for op; a nil one lets that call through
			call  func(o *OS, f *file) error
			path  string   // the name the error names; empty for the directory
			calls []string // the ops logged, in order
		}{
			{
				name: "Create",
				op:   create,
				errs: []error{code},
				call: func(o *OS, _ *file) error {
					_, err := o.Create("b")

					return err
				},
				path:  "b",
				calls: []string{create},
			},
			{
				name: "Open",
				op:   create,
				errs: []error{code},
				call: func(o *OS, _ *file) error {
					_, err := o.Open("a")

					return err
				},
				path:  "a",
				calls: []string{create},
			},
			{
				name: "WriteAt",
				op:   write,
				errs: []error{code},
				call: func(_ *OS, f *file) error {
					_, err := f.WriteAt([]byte("record"), 0)

					return err
				},
				path:  "a",
				calls: []string{write},
			},
			{
				name: "Truncate",
				op:   set,
				errs: []error{code},
				call: func(_ *OS, f *file) error {
					return f.Truncate(4096)
				},
				path:  "a",
				calls: []string{set},
			},
			{
				name: "Size",
				op:   get,
				errs: []error{code},
				call: func(_ *OS, f *file) error {
					_, err := f.Size()

					return err
				},
				path:  "a",
				calls: []string{get},
			},
			{
				name: "Sync",
				op:   flush,
				errs: []error{code},
				call: func(_ *OS, f *file) error {
					return f.Sync()
				},
				path:  "a",
				calls: []string{flush},
			},
			{
				name: "Allocate's size read",
				op:   get,
				errs: []error{code},
				call: func(_ *OS, f *file) error {
					return f.Allocate(0, 4096)
				},
				path:  "a",
				calls: []string{get},
			},
			{
				name: "Allocate's allocation size",
				op:   set,
				errs: []error{code},
				call: func(_ *OS, f *file) error {
					return f.Allocate(0, 4096)
				},
				path:  "a",
				calls: []string{get, set},
			},
			{
				name: "Allocate's end of file",
				op:   set,
				errs: []error{nil, code},
				call: func(_ *OS, f *file) error {
					return f.Allocate(0, 4096)
				},
				path:  "a",
				calls: []string{get, set, set},
			},
			{
				name: "Allocate's zero write",
				op:   write,
				errs: []error{code},
				call: func(_ *OS, f *file) error {
					return f.Allocate(0, 4096)
				},
				path:  "a",
				calls: []string{get, set, set, write},
			},
			{
				name: "SyncDir's open",
				op:   create,
				errs: []error{code},
				call: func(o *OS, _ *file) error {
					return o.SyncDir()
				},
				calls: []string{create},
			},
			{
				name: "SyncDir's flush",
				op:   flush,
				errs: []error{code},
				call: func(o *OS, _ *file) error {
					return o.SyncDir()
				},
				calls: []string{create, flush, shut},
			},
			{
				name: "SyncDir's close",
				op:   shut,
				errs: []error{code},
				call: func(o *OS, _ *file) error {
					return o.SyncDir()
				},
				calls: []string{create, flush, shut},
			},
		} {
			t.Run(code.Error()+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				r := &recorder{}
				o := newOS(dir, r.table())
				f := openOn(t, o, "a")

				r.failNext(tc.op, tc.errs...)
				err := tc.call(o, f)
				log := r.take()

				if !slices.Equal(ops(log), tc.calls) {
					t.Fatalf("calls %v, want exactly %v", log, tc.calls)
				}

				want := dir

				if tc.path != "" {
					want = filepath.Join(dir, tc.path)
				}

				pe, ok := err.(*fs.PathError)

				if !ok || pe.Path != want {
					t.Fatalf("got %v (%T), want an *fs.PathError naming %s", err, err, want)
				}

				if !errors.Is(err, syscall.ENOSPC) {
					t.Fatalf("%v does not match syscall.ENOSPC", err)
				}

				if !errors.Is(err, code) {
					t.Fatalf(
						"%v no longer matches %d, the code Windows reported",
						err,
						uint32(code),
					)
				}
			})
		}
	}

	r := &recorder{}
	f := openOn(t, newOS(t.TempDir(), r.table()), "b")

	r.failNext("writeFile", windows.ERROR_IO_DEVICE)
	_, err := f.WriteAt([]byte("record"), 0)

	if errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("%v matches syscall.ENOSPC, but the volume is not full", err)
	}
}
