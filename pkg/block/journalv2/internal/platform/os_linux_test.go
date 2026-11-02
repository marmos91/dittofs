package platform

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"golang.org/x/sys/unix"
)

// TestConcurrentWriteAt writes one file from many goroutines at once, each at
// its own offsets, and reads it back whole. A WriteAt that is not positional,
// a seek then a write, lands bytes at another writer's offset; one that shares
// state between calls is reported by the race detector.
func TestConcurrentWriteAt(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const (
		writers = 32
		rounds  = 32
		chunk   = 512
	)
	f := openOn(t, NewOS(t.TempDir()), "a")

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			p := bytes.Repeat([]byte{byte(w + 1)}, chunk)
			for r := range rounds {
				off := int64((r*writers + w) * chunk)
				if n, err := f.WriteAt(p, off); n != chunk || err != nil {
					t.Errorf("WriteAt at %d: %d, %v", off, n, err)
					return
				}
			}
		})
	}

	wg.Wait()

	got := make([]byte, writers*rounds*chunk)
	if n, err := f.ReadAt(got, 0); n != len(got) || err != nil {
		t.Fatalf("ReadAt: %d, %v", n, err)
	}

	for i := 0; i < len(got); i += chunk {
		if want := byte((i/chunk)%writers + 1); got[i] != want || !bytes.Equal(got[i:i+chunk], bytes.Repeat([]byte{want}, chunk)) {
			t.Fatalf("chunk at %d holds %d, want writer %d's bytes", i, got[i], want)
		}
	}
}

// TestClosedFile checks Allocate on an open file allocates through the table,
// then that every operation on a closed file fails with an *fs.PathError
// matching fs.ErrClosed and naming the file's full path, Allocate's with the
// op fallocate, rather than reaching whatever file has since reused the
// descriptor's number, and Sync and Allocate make no system call. The file
// is kept reachable until the profiler's check, so the *os.File finalizer
// cannot close a leaked descriptor first and hide the leak.
func TestClosedFile(t *testing.T) {
	var f storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(f)
	})
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f, err := newOS(dir, r.table()).Create("a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = f.Allocate(0, 1)
	expectAllocate(t, r.take(), filepath.Join(dir, "a"), 0, 1, 1)

	if err != nil {
		t.Fatalf("Allocate on an open file: %v", err)
	}

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
			t.Errorf("%s after Close: %v (%T), want an *fs.PathError matching fs.ErrClosed", op.name, err, err)
		} else if want := filepath.Join(dir, "a"); pe.Path != want {
			t.Errorf("%s after Close: %v names %s, want the full path %s", op.name, err, pe.Path, want)
		} else if op.name == "Allocate" && pe.Op != "fallocate" {
			t.Errorf("Allocate after Close: %v has op %q, want fallocate", err, pe.Op)
		}
	}

	if log := r.take(); len(log) != 0 {
		t.Fatalf("operations after Close made %v, want no call through the table", log)
	}
}

// TestCreatePermission checks Create makes a file only its owner can read or
// write. The umask can only narrow the mode Create asks for, so the test clears
// it, and the file's mode is exactly the one asked for. The umask is process
// wide: no test in this package runs in parallel.
func TestCreatePermission(t *testing.T) {
	var f storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(f)
	})
	prof.Watch(t, prof.Options{})

	old := unix.Umask(0)
	t.Cleanup(func() {
		unix.Umask(old)
	})

	dir := t.TempDir()
	f, err := NewOS(dir).Create("a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}

	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("Create made mode %#o, want 0o600", got)
	}
}
