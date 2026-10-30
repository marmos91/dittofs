package platform

import (
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// expectOpen fails t unless log is exactly one createFile of path, with read
// and write access, sharing reads, writes and deletes, with the disposition
// createmode, whose handle is h.
func expectOpen(t *testing.T, log []call, path string, createmode uint32, h windows.Handle) {
	t.Helper()

	if len(log) != 1 || log[0].op != "createFile" {
		t.Fatalf("want exactly one createFile of %s, got %v", path, log)
	}

	c := log[0]

	if c.path != path {
		t.Fatalf("%v: want %s opened", c, path)
	}

	if c.createmode != createmode {
		t.Fatalf("%v: want the disposition %d", c, createmode)
	}

	if c.handle != h {
		t.Fatalf("%v: the file holds the handle %#x, not the one opened", c, h)
	}

	for _, bit := range []struct {
		name      string
		got, want uint32
	}{
		{"GENERIC_READ", c.access, windows.GENERIC_READ},
		{"GENERIC_WRITE", c.access, windows.GENERIC_WRITE},
		{"FILE_SHARE_READ", c.share, windows.FILE_SHARE_READ},
		{"FILE_SHARE_WRITE", c.share, windows.FILE_SHARE_WRITE},
		{"FILE_SHARE_DELETE", c.share, windows.FILE_SHARE_DELETE},
	} {
		if bit.got&bit.want == 0 {
			t.Errorf("%v: want %s", c, bit.name)
		}
	}
}

// TestOpenSharing checks Create and Open each open their file through the
// table with read and write access and with sharing that permits deletion
// (RFC 28 §2.2: "on platforms where an open file cannot be unlinked, segments
// **MUST** be opened with sharing that permits deletion"; Appendix A:
// "segments are opened with `FILE_SHARE_DELETE`"), as well as reads and
// writes, so a second open of a file already open is not refused.
func TestOpenSharing(t *testing.T) {
	var files []storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(files)
	})
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	o := newOS(dir, r.table())

	f, err := o.Create("a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	files = append(files, f)
	expectOpen(t, r.take(), filepath.Join(dir, "a"), windows.CREATE_NEW, handleOf(t, f.(*file)))

	g, err := o.Open("a")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	files = append(files, g)
	expectOpen(t, r.take(), filepath.Join(dir, "a"), windows.OPEN_EXISTING, handleOf(t, g.(*file)))

	for _, h := range files {
		if err := h.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// TestRenameAndRemoveWhileOpen renames a file a reader holds open, then
// removes it, both while the reader still holds it, on the real filesystem.
// Windows refuses both unless every handle open on the file shares deletion,
// which is why segments are opened with FILE_SHARE_DELETE (RFC 28 Appendix A:
// "so one can be deleted while a reader still holds it"). The reader still
// reads its bytes after both.
func TestRenameAndRemoveWhileOpen(t *testing.T) {
	var files []storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(files)
	})
	prof.Watch(t, prof.Options{})

	o := NewOS(t.TempDir())

	w, err := o.Create("00000000000000000001.seg")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	files = append(files, w)

	if _, err := w.WriteAt([]byte("record"), 0); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := o.Open("00000000000000000001.seg")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	files = append(files, reader)

	if err := o.Rename("00000000000000000001.seg", "00000000000000000002.seg"); err != nil {
		t.Fatalf("Rename of a file a reader holds open: %v", err)
	}

	if err := o.Remove("00000000000000000002.seg"); err != nil {
		t.Fatalf("Remove of a file a reader holds open: %v", err)
	}

	p := make([]byte, 6)

	if n, err := reader.ReadAt(p, 0); n != 6 || err != nil || string(p) != "record" {
		t.Fatalf(
			"the reader read %d, %v, %q after the rename and the remove; want 6, nil, %q",
			n,
			err,
			p,
			"record",
		)
	}

	names, err := o.List()
	if err != nil {
		t.Fatal(err)
	}

	if len(names) != 0 {
		t.Fatalf("List after the remove: %v, want no name", names)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestClosedFile checks every operation on a closed file fails with an
// *fs.PathError matching fs.ErrClosed and naming the file's full path, rather
// than reaching whatever file has since reused the handle's value, and that
// Sync and Allocate make no call through the table. The profiler does not
// list handles on Windows, so the file is kept reachable only so its
// finalizer cannot close it first.
func TestClosedFile(t *testing.T) {
	var f *file
	t.Cleanup(func() {
		runtime.KeepAlive(f)
	})
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f = openOn(t, newOS(dir, r.table()), "a")

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, op := range []struct {
		name string
		call func() error
	}{
		{"ReadAt", func() error {
			_, err := f.ReadAt(make([]byte, 1), 0)

			return err
		}},
		{"WriteAt", func() error {
			_, err := f.WriteAt([]byte{1}, 0)

			return err
		}},
		{"Truncate", func() error {
			return f.Truncate(1)
		}},
		{"Size", func() error {
			_, err := f.Size()

			return err
		}},
		{"Allocate", func() error {
			return f.Allocate(0, 1)
		}},
		{"Sync", f.Sync},
	} {
		err := op.call()

		var pe *fs.PathError

		if !errors.As(err, &pe) || !errors.Is(err, fs.ErrClosed) {
			t.Errorf(
				"%s after Close: %v (%T), want an *fs.PathError matching fs.ErrClosed",
				op.name,
				err,
				err,
			)
		} else if want := filepath.Join(dir, "a"); pe.Path != want {
			t.Errorf(
				"%s after Close: %v names %s, want the full path %s",
				op.name,
				err,
				pe.Path,
				want,
			)
		}
	}

	if log := r.take(); len(log) != 0 {
		t.Fatalf("operations after Close made %v, want no call through the table", log)
	}
}

// TestSizeCalls checks Size reads the file's size through the table, on the
// file's own handle and inside its Control callback, so a Close made
// meanwhile cannot hand the handle to another file, and that a failed read is
// returned once as an *fs.PathError naming the file's full path.
func TestSizeCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f := openOn(t, newOS(dir, r.table()), "a")
	h := handleOf(t, f)

	if _, err := f.f.WriteAt([]byte("x"), 4096); err != nil {
		t.Fatal(err)
	}

	size, err := f.Size()
	log := r.take()

	if len(log) != 1 || log[0].op != "getFileInformationByHandle" {
		t.Fatalf("want exactly one getFileInformationByHandle, got %v", log)
	}

	expectOnFile(t, log[0], h, log)

	if err != nil || size != 4097 {
		t.Fatalf("Size: %d, %v; want 4097, nil", size, err)
	}

	r.failNext("getFileInformationByHandle", windows.ERROR_IO_DEVICE)
	_, err = f.Size()
	log = r.take()

	if len(log) != 1 || log[0].op != "getFileInformationByHandle" {
		t.Fatalf("want exactly one getFileInformationByHandle, got %v", log)
	}

	expectPathError(t, err, windows.ERROR_IO_DEVICE, filepath.Join(dir, "a"))
}

// TestSizeAbove4GiB checks Size combines the two halves of the size the
// table reports, on a dry table, so no 4 GiB file is made: a size read that
// keeps only the low 32 bits, or swaps the halves, fails it.
func TestSizeAbove4GiB(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, want := range []int64{
		1<<32 + 5,
		3<<32 + 1<<31,
		1 << 40,
	} {
		r := &recorder{
			dry:  true,
			size: want,
		}
		f := openOn(t, newOS(t.TempDir(), r.table()), "a")

		got, err := f.Size()
		if err != nil || got != want {
			t.Fatalf("Size: %d, %v; want %d, nil", got, err, want)
		}
	}
}

// createWith creates name through o, writes content into it and closes it.
func createWith(t *testing.T, o *OS, name, content string) {
	t.Helper()

	f, err := o.Create(name)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}

	if _, err := f.WriteAt([]byte(content), 0); err != nil {
		t.Fatalf("WriteAt %s: %v", name, err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close %s: %v", name, err)
	}
}

// expectContent fails t unless f holds exactly want.
func expectContent(t *testing.T, f storage.File, want string) {
	t.Helper()

	size, err := f.Size()
	if err != nil {
		t.Fatalf("Size of %s: %v", f.Name(), err)
	}

	if size != int64(len(want)) {
		t.Fatalf("%s is %d bytes, want %d holding %q", f.Name(), size, len(want), want)
	}

	if size == 0 {
		return
	}

	p := make([]byte, size)

	if n, err := f.ReadAt(p, 0); n != len(p) || err != nil || string(p) != want {
		t.Fatalf("%s reads %d, %v, %q; want %q", f.Name(), n, err, p, want)
	}
}

// TestCreateOverRemovedNameWhileOpen removes a file a reader still holds,
// then creates a new file under the same name while the reader holds the old
// one, on the real filesystem. A removed name can come back: a new segment's
// id need only be above every id still in the directory (RFC 28 §2.2), so
// once the segment with the highest id is unlinked, the next one can take
// its name. A Windows that keeps the removed name in a delete-pending state
// until the last handle closes refuses the create, and fails this test. The
// new file is empty, and the reader still reads the old bytes.
func TestCreateOverRemovedNameWhileOpen(t *testing.T) {
	var files []storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(files)
	})
	prof.Watch(t, prof.Options{})

	const name = "00000000000000000001.seg"

	o := NewOS(t.TempDir())
	createWith(t, o, name, "old record")

	reader, err := o.Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	files = append(files, reader)

	if err := o.Remove(name); err != nil {
		t.Fatalf("Remove of a file a reader holds open: %v", err)
	}

	f, err := o.Create(name)
	if err != nil {
		t.Fatalf("Create under a removed name a reader still holds: %v", err)
	}

	files = append(files, f)
	expectContent(t, f, "")
	expectContent(t, reader, "old record")

	for _, h := range files {
		if err := h.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// TestRenameOverOpenName renames a file over a name a reader still holds
// open, on the real filesystem: the replace that puts a new format file in
// place, written to format.tmp and renamed over format (RFC 28 §2.1). It is
// the only rename over an existing name; recycling renames a segment to a
// fresh name. The rename succeeds, the name then holds the new bytes, only
// that name is left, and the reader still reads the old bytes.
func TestRenameOverOpenName(t *testing.T) {
	var files []storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(files)
	})
	prof.Watch(t, prof.Options{})

	o := NewOS(t.TempDir())
	createWith(t, o, "format", "old format")

	reader, err := o.Open("format")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	files = append(files, reader)
	createWith(t, o, "format.tmp", "the new format")

	if err := o.Rename("format.tmp", "format"); err != nil {
		t.Fatalf("Rename over a name a reader holds open: %v", err)
	}

	names, err := o.List()
	if err != nil {
		t.Fatal(err)
	}

	if len(names) != 1 || names[0] != "format" {
		t.Fatalf("the directory holds %v after the rename, want only format", names)
	}

	f, err := o.Open("format")
	if err != nil {
		t.Fatalf("Open after the rename: %v", err)
	}

	files = append(files, f)
	expectContent(t, f, "the new format")
	expectContent(t, reader, "old format")

	for _, h := range files {
		if err := h.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}
