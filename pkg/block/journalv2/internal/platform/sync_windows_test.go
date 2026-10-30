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
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// call is one entry of a recorder's log.
type call struct {
	// op is createFile, flushFileBuffers, closeHandle,
	// getFileInformationByHandle, setFileInformationByHandle or writeFile.
	op string

	// handle is the handle acted on, or createFile's result; InvalidHandle
	// for a failed createFile.
	handle windows.Handle

	// inControl is whether the call ran inside an os.File's
	// SyscallConn().Control callback, which holds the file open until it
	// returns.
	inControl bool

	// createFile: the path opened, the access asked for, the sharing
	// granted, the disposition and the flags and attributes.
	path                             string
	access, share, createmode, attrs uint32

	// setFileInformationByHandle: the information class, and the size it
	// sets; -1 if the buffer is not one int64.
	class uint32
	value int64

	// writeFile: the offset its overlapped names, -1 with no overlapped; the
	// bytes asked for; whether every byte asked for is zero.
	off   int64
	n     int
	zeros bool
}

func (c call) String() string {
	switch c.op {
	case "createFile":
		return fmt.Sprintf(
			"createFile(%s, access %#x, share %#x, disposition %d, attrs %#x) = %#x",
			c.path,
			c.access,
			c.share,
			c.createmode,
			c.attrs,
			c.handle,
		)
	case "setFileInformationByHandle":
		return fmt.Sprintf(
			"setFileInformationByHandle(%#x, class %d, %d, in control %t)",
			c.handle,
			c.class,
			c.value,
			c.inControl,
		)
	case "writeFile":
		return fmt.Sprintf(
			"writeFile(%#x, %d bytes, zeros %t, at %d, in control %t)",
			c.handle,
			c.n,
			c.zeros,
			c.off,
			c.inControl,
		)
	default:
		return fmt.Sprintf("%s(%#x, in control %t)", c.op, c.handle, c.inControl)
	}
}

// recorder is a table that logs every call and makes it. An error queued for
// an op is returned by its next call instead, and the call does not run,
// except closeHandle, which runs, so the test leaks no handle. A nil error
// queued lets that call run. A short write queued for writeFile makes its
// next call write only the first half of what it was asked for, or nothing.
//
// A dry recorder reaches no file for the size calls and the writes: the size
// read reports size, the size sets change nothing, and a write writes
// nothing and reports every byte written. It lets a test ask for sizes and
// offsets above 4 GiB without a file that large.
type recorder struct {
	mu   sync.Mutex
	log  []call
	fail map[string][]error // per op, what its next calls return, oldest first

	// short holds what the next writeFile calls write: true for half, false
	// for nothing.
	short []bool

	dry  bool
	size int64
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

	q := r.fail[op]

	return len(q) > 0 && q[0] != nil
}

// add logs c and returns the error queued for its op, if any.
func (r *recorder) add(c call) error {
	r.mu.Lock()

	defer r.mu.Unlock()

	r.log = append(r.log, c)

	q := r.fail[c.op]

	if len(q) == 0 {
		return nil
	}

	r.fail[c.op] = q[1:]

	return q[0]
}

// nextShort pops the next queued short write: whether there is one, and
// whether it writes half rather than nothing.
func (r *recorder) nextShort() (short, half bool) {
	r.mu.Lock()

	defer r.mu.Unlock()

	if len(r.short) == 0 {
		return false, false
	}

	half = r.short[0]
	r.short = r.short[1:]

	return true, half
}

func (r *recorder) table() calls {
	return calls{
		createFile: func(
			name *uint16,
			access uint32,
			mode uint32,
			sa *windows.SecurityAttributes,
			createmode uint32,
			attrs uint32,
			templatefile windows.Handle,
		) (windows.Handle, error) {
			c := call{
				op:         "createFile",
				handle:     windows.InvalidHandle,
				inControl:  inControl(),
				path:       windows.UTF16PtrToString(name),
				access:     access,
				share:      mode,
				createmode: createmode,
				attrs:      attrs,
			}

			if r.queued(c.op) {
				return windows.InvalidHandle, r.add(c)
			}

			h, err := windows.CreateFile(name, access, mode, sa, createmode, attrs, templatefile)
			if err == nil {
				c.handle = h
			}

			_ = r.add(c)

			return h, err
		},
		flushFileBuffers: func(h windows.Handle) error {
			if err := r.add(call{
				op:        "flushFileBuffers",
				handle:    h,
				inControl: inControl(),
			}); err != nil {
				return err
			}

			return windows.FlushFileBuffers(h)
		},
		closeHandle: func(h windows.Handle) error {
			queued := r.add(call{
				op:        "closeHandle",
				handle:    h,
				inControl: inControl(),
			})
			err := windows.CloseHandle(h)

			if queued != nil {
				return queued
			}

			return err
		},
		getFileInformationByHandle: func(
			h windows.Handle,
			data *windows.ByHandleFileInformation,
		) error {
			if err := r.add(call{
				op:        "getFileInformationByHandle",
				handle:    h,
				inControl: inControl(),
			}); err != nil {
				return err
			}

			if r.dry {
				data.FileSizeHigh = uint32(r.size >> 32)
				data.FileSizeLow = uint32(r.size)

				return nil
			}

			return windows.GetFileInformationByHandle(h, data)
		},
		setFileInformationByHandle: func(
			h windows.Handle,
			class uint32,
			buf *byte,
			n uint32,
		) error {
			value := int64(-1)

			if buf != nil && n == 8 {
				value = *(*int64)(unsafe.Pointer(buf))
			}

			if err := r.add(call{
				op:        "setFileInformationByHandle",
				handle:    h,
				inControl: inControl(),
				class:     class,
				value:     value,
			}); err != nil {
				return err
			}

			if r.dry {
				return nil
			}

			return windows.SetFileInformationByHandle(h, class, buf, n)
		},
		writeFile: func(h windows.Handle, p []byte, done *uint32, o *windows.Overlapped) error {
			c := call{
				op:        "writeFile",
				handle:    h,
				inControl: inControl(),
				off:       -1,
				n:         len(p),
				zeros:     true,
			}

			if o != nil {
				c.off = int64(o.OffsetHigh)<<32 | int64(o.Offset)
			}

			for _, b := range p {
				if b != 0 {
					c.zeros = false

					break
				}
			}

			if done == nil {
				return fmt.Errorf("writeFile: no done count for %v", c)
			}

			if err := r.add(c); err != nil {
				*done = 0

				return err
			}

			if short, half := r.nextShort(); short {
				if !half {
					*done = 0

					return nil
				}

				p = p[:len(p)/2]
			}

			if r.dry {
				*done = uint32(len(p))

				return nil
			}

			return windows.WriteFile(h, p, done, o)
		},
	}
}

// openOn creates the file name in o's directory and returns it as o's file.
// It opens through the os package, so a sync or allocation test does not
// depend on how Create opens.
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

// handleOf returns the handle f holds.
func handleOf(t *testing.T, f *file) windows.Handle {
	t.Helper()

	rc, err := f.f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}

	var h windows.Handle

	if err := rc.Control(func(fd uintptr) {
		h = windows.Handle(fd)
	}); err != nil {
		t.Fatal(err)
	}

	return h
}

// expectOnFile fails t unless c acts on the handle h from inside the file's
// Control callback, where a Close made meanwhile cannot release h.
func expectOnFile(t *testing.T, c call, h windows.Handle, log []call) {
	t.Helper()

	if c.handle != h {
		t.Fatalf("%v: not on the file's own handle %#x; log %v", c, h, log)
	}

	if !c.inControl {
		t.Fatalf(
			"%v: made outside the file's Control callback, where a Close frees the handle; log %v",
			c,
			log,
		)
	}
}

// expectFlush fails t unless log is exactly n flushFileBuffers calls, each on
// the file's handle h from inside its Control callback.
func expectFlush(t *testing.T, log []call, h windows.Handle, n int) {
	t.Helper()

	if len(log) != n {
		t.Fatalf("want exactly %d flushFileBuffers on %#x, got %v", n, h, log)
	}

	for _, c := range log {
		if c.op != "flushFileBuffers" {
			t.Fatalf("%v: want flushFileBuffers; log %v", c, log)
		}

		expectOnFile(t, c, h, log)
	}
}

// expectPathError fails t unless err is itself an *fs.PathError wrapping
// errno and naming path, the shape the platform's own errors take when one
// thing failed.
func expectPathError(t *testing.T, err error, errno syscall.Errno, path string) {
	t.Helper()

	pe, ok := err.(*fs.PathError)

	if !ok || !errors.Is(pe.Err, errno) {
		t.Fatalf("got %v (%T), want an *fs.PathError wrapping %v", err, err, errno)
	}

	if pe.Path != path {
		t.Fatalf("%v: names %s, want %s", err, pe.Path, path)
	}
}

// TestSyncCalls checks the call each Sync makes on Windows (RFC 28 Appendix
// A: "`FlushFileBuffers` is the sync"): one FlushFileBuffers on the file's own
// handle per Sync, made inside the file's Control callback.
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
	}{
		{b, "b"},
		{a, "a"},
		{a, "a"},
	} {
		if _, err := step.f.f.WriteAt([]byte("record"), 0); err != nil {
			t.Fatal(err)
		}

		err := step.f.Sync()
		expectFlush(t, r.take(), handleOf(t, step.f), 1)

		if err != nil {
			t.Fatalf("Sync %s: %v", step.name, err)
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
	h := handleOf(t, a)

	r.failNext("flushFileBuffers", windows.ERROR_IO_DEVICE)
	err := a.Sync()
	expectFlush(t, r.take(), h, 1)
	expectPathError(t, err, windows.ERROR_IO_DEVICE, filepath.Join(dir, "a"))

	err = a.Sync()
	expectFlush(t, r.take(), h, 1)

	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
}
