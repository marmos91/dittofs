package storage_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// target is one implementation of storage.FS the contract runs on.
type target struct {
	name string
	open func(t *testing.T, dir string) storage.FS

	// durable returns what a crash now would leave in the file name, where
	// the implementation can tell; nil where only a power cut would.
	durable func(fs storage.FS, name string) ([]byte, error)
}

// targets are the implementations of storage.FS the contract runs on, each
// on a real directory. The seam runs everywhere; each platform's file adds
// the operating system's storage where it is implemented.
var targets = []target{
	{
		name: "journalv2test.Seam",
		open: func(t *testing.T, dir string) storage.FS {
			s, err := journalv2test.NewSeam(dir)
			if err != nil {
				t.Fatal(err)
			}

			return s
		},
		durable: func(fs storage.FS, name string) ([]byte, error) {
			return fs.(*journalv2test.Seam).Durable(name)
		},
	},
}

// contractCase checks one sentence of storage.go's contract.
type contractCase struct {
	sentence string
	run      func(t *testing.T, c env)
}

// env is what a case runs against: the storage, its directory and its target.
type env struct {
	fs  storage.FS
	dir string
	tgt target
}

// TestContract runs one case per sentence of the contract in storage.go on
// every implementation, each on a real directory of its own.
func TestContract(t *testing.T) {
	// Registered before the watch, so it runs after the watch's check.
	t.Cleanup(func() {
		runtime.KeepAlive(held)
		held = nil
	})
	prof.Watch(t, prof.Options{})

	for _, tgt := range targets {
		for _, cc := range contract {
			t.Run(tgt.name+"/"+cc.sentence, func(t *testing.T) {
				dir := t.TempDir()
				cc.run(t, env{fs: tgt.open(t, dir), dir: dir, tgt: tgt})
			})
		}
	}
}

// held keeps every file a case opened reachable until TestContract's watch
// has checked the descriptors. The watch collects garbage, and the finalizer
// of an *os.File nothing references closes its descriptor, which would hide a
// Close that releases nothing.
var held []storage.File

// The contract sentences no case checks, and why:
//
//   - "Every call is one a crash-simulating implementation can record and
//     discard", for FS and for File: a crash. The seam's own tests check it;
//     on the operating system it takes a power cut.
//   - "The rename is atomic, but a crash before SyncDir may undo it": the
//     atomicity is the kernel's rename; the crash half is as above.
//   - ReadAtDirect's "bypassing the page cache, so it observes what reached
//     the device": not provided yet, the read goes through the page cache.
//   - Allocate's two sentences: the operating system's allocation is not
//     written yet.
//   - "A later success does not stand in for a Sync that failed", and the
//     same for SyncDir: it needs a failure injected beneath the storage.
//     platform's TestSyncError and TestSyncDirError check it there, and the
//     seam's own tests on the seam.
var contract = []contractCase{
	{"FS is one journal's directory. Names are of files directly inside it", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		write(t, f, "record", 0)
		closeFile(t, f)
		got, err := os.ReadFile(filepath.Join(c.dir, "a"))
		if err != nil || string(got) != "record" {
			t.Fatalf("the directory's file a holds %q, %v; want %q", got, err, "record")
		}
	}},
	{"Create creates the file name, empty, and opens it for reading and writing", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		defer closeFile(t, f)
		expectSize(t, f, 0)
		write(t, f, "record", 0)
		expectRead(t, f, "record", 0)
	}},
	{"Create fails with an error matching fs.ErrExist if name exists", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		write(t, f, "record", 0)
		closeFile(t, f)
		if g, err := c.fs.Create("a"); !errors.Is(err, fs.ErrExist) {
			if g != nil {
				_ = g.Close()
			}
			t.Fatalf("second Create: %v, want fs.ErrExist", err)
		}
		f = open(t, c.fs, "a")
		defer closeFile(t, f)
		expectRead(t, f, "record", 0)
	}},
	{"Open opens the existing file name for reading and writing", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		write(t, f, "record", 0)
		closeFile(t, f)
		f = open(t, c.fs, "a")
		defer closeFile(t, f)
		expectRead(t, f, "record", 0)
		write(t, f, "RE", 0)
		expectRead(t, f, "REcord", 0)
	}},
	{"Open fails with an error matching fs.ErrNotExist if name does not exist", func(t *testing.T, c env) {
		if f, err := c.fs.Open("a"); !errors.Is(err, fs.ErrNotExist) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("Open: %v, want fs.ErrNotExist", err)
		}
	}},
	{"A file is opened with sharing that permits it to be removed while open", func(t *testing.T, c env) {
		closeFile(t, create(t, c.fs, "a"))
		closeFile(t, create(t, c.fs, "b"))
		f := open(t, c.fs, "a")
		defer closeFile(t, f)
		g := open(t, c.fs, "b")
		defer closeFile(t, g)
		write(t, f, "record", 0)
		if err := c.fs.Rename("a", "c"); err != nil {
			t.Fatalf("Rename of an open file: %v", err)
		}
		if err := c.fs.Remove("b"); err != nil {
			t.Fatalf("Remove of an open file: %v", err)
		}
		expectRead(t, f, "record", 0)
		expectNames(t, c.fs, "c")
	}},
	{"Rename renames oldName to newName, replacing newName if it exists", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		write(t, f, "new", 0)
		closeFile(t, f)
		g := create(t, c.fs, "b")
		write(t, g, "old bytes", 0)
		closeFile(t, g)
		if err := c.fs.Rename("a", "b"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		expectNames(t, c.fs, "b")
		f = open(t, c.fs, "b")
		defer closeFile(t, f)
		expectSize(t, f, 3)
		expectRead(t, f, "new", 0)
	}},
	{"Remove unlinks name", func(t *testing.T, c env) {
		closeFile(t, create(t, c.fs, "a"))
		closeFile(t, create(t, c.fs, "b"))
		if err := c.fs.Remove("a"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		expectNames(t, c.fs, "b")
		if _, err := os.Stat(filepath.Join(c.dir, "a")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a is still in the directory: %v", err)
		}
	}},
	{"List returns the names of the files in the directory, including those the journal does not recognise", func(t *testing.T, c env) {
		closeFile(t, create(t, c.fs, "00000000000000000001.seg"))
		if err := os.WriteFile(filepath.Join(c.dir, "not the journal's"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		expectNames(t, c.fs, "00000000000000000001.seg", "not the journal's")
	}},
	{"SyncDir makes every Create, Rename and Remove that returned before it durable", func(t *testing.T, c env) {
		closeFile(t, create(t, c.fs, "a"))
		closeFile(t, create(t, c.fs, "b"))
		closeFile(t, create(t, c.fs, "c"))
		if err := c.fs.Rename("b", "d"); err != nil {
			t.Fatal(err)
		}
		if err := c.fs.Remove("c"); err != nil {
			t.Fatal(err)
		}
		if err := c.fs.SyncDir(); err != nil {
			t.Fatalf("SyncDir: %v", err)
		}
		expectNames(t, c.fs, "a", "d")
		if c.tgt.durable == nil {
			return // platform's TestSyncCalls checks the call; survival takes a power cut
		}
		for name, want := range map[string]bool{"a": true, "b": false, "c": false, "d": true} {
			if _, err := c.tgt.durable(c.fs, name); (err == nil) != want {
				t.Errorf("%s durable: %v, want %v", name, err, want)
			}
		}
	}},
	{"ReadAt reads len(p) bytes at offset off, with the semantics of io.ReaderAt", func(t *testing.T, c env) {
		expectReaderAt(t, c, storage.File.ReadAt)
	}},
	{"ReadAtDirect reads as ReadAt does", func(t *testing.T, c env) {
		expectReaderAt(t, c, storage.File.ReadAtDirect)
	}},
	{"WriteAt writes len(p) bytes at offset off, with the semantics of io.WriterAt", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		defer closeFile(t, f)
		write(t, f, "head", 0)
		write(t, f, "tail", 8)
		write(t, f, "EA", 1)
		expectSize(t, f, 12)
		expectRead(t, f, "hEAd\x00\x00\x00\x00tail", 0)
	}},
	{"Sync makes every WriteAt, Allocate and Truncate that returned before it durable", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		defer closeFile(t, f)
		if err := c.fs.SyncDir(); err != nil {
			t.Fatal(err)
		}
		write(t, f, "record and more", 0)
		if err := f.Truncate(6); err != nil {
			t.Fatal(err)
		}
		if c.tgt.durable != nil {
			if got, _ := c.tgt.durable(c.fs, "a"); len(got) != 0 {
				t.Fatalf("durable before Sync: %q, want nothing", got)
			}
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		expectSize(t, f, 6)
		expectRead(t, f, "record", 0)
		if c.tgt.durable == nil {
			return // platform's TestSyncCalls checks the call; survival takes a power cut
		}
		if got, err := c.tgt.durable(c.fs, "a"); err != nil || string(got) != "record" {
			t.Fatalf("durable after Sync: %q, %v; want %q", got, err, "record")
		}
	}},
	{"Truncate sets the file's size to size", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		defer closeFile(t, f)
		write(t, f, "record", 0)
		if err := f.Truncate(3); err != nil {
			t.Fatal(err)
		}
		expectSize(t, f, 3)
		expectRead(t, f, "rec", 0)
		if err := f.Truncate(5); err != nil {
			t.Fatal(err)
		}
		expectSize(t, f, 5)
		expectRead(t, f, "rec\x00\x00", 0)
	}},
	{"Size returns the file's size in bytes", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		defer closeFile(t, f)
		write(t, f, "x", 4095)
		expectSize(t, f, 4096)
		g := open(t, c.fs, "a")
		defer closeFile(t, g)
		expectSize(t, g, 4096)
	}},
	{"Close releases the file", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// TestContract's watch fails a descriptor Close left open.
	}},
	{"Name returns the name the file was created or opened under", func(t *testing.T, c env) {
		f := create(t, c.fs, "a")
		defer closeFile(t, f)
		if err := c.fs.Rename("a", "b"); err != nil {
			t.Fatal(err)
		}
		g := open(t, c.fs, "b")
		defer closeFile(t, g)
		if f.Name() != "a" || g.Name() != "b" {
			t.Fatalf("names %q and %q, want %q and %q", f.Name(), g.Name(), "a", "b")
		}
	}},
}

// expectReaderAt checks read has io.ReaderAt's semantics: a whole read
// returns len(p) and nil, and a read the end cuts short returns what is there
// and io.EOF. The bytes are synced first, so a read that observes only what
// reached the device sees them too.
func expectReaderAt(t *testing.T, c env, read func(storage.File, []byte, int64) (int, error)) {
	t.Helper()
	f := create(t, c.fs, "a")
	defer closeFile(t, f)
	write(t, f, "record", 0)
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	p := make([]byte, 3)
	if n, err := read(f, p, 2); n != 3 || err != nil || string(p) != "cor" {
		t.Fatalf("read of 3 at 2: %d, %v, %q; want 3, nil, %q", n, err, p, "cor")
	}
	p = make([]byte, 4)
	if n, err := read(f, p, 4); n != 2 || !errors.Is(err, io.EOF) || string(p[:n]) != "rd" {
		t.Fatalf("read of 4 at 4: %d, %v, %q; want 2, io.EOF, %q", n, err, p[:n], "rd")
	}
	if n, err := read(f, p, 6); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at the end: %d, %v; want 0, io.EOF", n, err)
	}
}

func create(t *testing.T, s storage.FS, name string) storage.File {
	t.Helper()
	f, err := s.Create(name)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	held = append(held, f)
	return f
}

func open(t *testing.T, s storage.FS, name string) storage.File {
	t.Helper()
	f, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	held = append(held, f)
	return f
}

func closeFile(t *testing.T, f storage.File) {
	t.Helper()
	if err := f.Close(); err != nil {
		t.Fatalf("Close %s: %v", f.Name(), err)
	}
}

func write(t *testing.T, f storage.File, s string, off int64) {
	t.Helper()
	if n, err := f.WriteAt([]byte(s), off); n != len(s) || err != nil {
		t.Fatalf("WriteAt %q at %d: %d, %v", s, off, n, err)
	}
}

func expectRead(t *testing.T, f storage.File, want string, off int64) {
	t.Helper()
	p := make([]byte, len(want))
	if n, err := f.ReadAt(p, off); n != len(want) || err != nil || !bytes.Equal(p, []byte(want)) {
		t.Fatalf("ReadAt %d at %d: %d, %v, %q; want %q", len(want), off, n, err, p[:n], want)
	}
}

func expectSize(t *testing.T, f storage.File, want int64) {
	t.Helper()
	if got, err := f.Size(); got != want || err != nil {
		t.Fatalf("Size: %d, %v; want %d", got, err, want)
	}
}

func expectNames(t *testing.T, s storage.FS, want ...string) {
	t.Helper()
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("List: %q, want %q", got, want)
	}
}
