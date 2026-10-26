//go:build linux || darwin

package prof

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWatchFailsOnLeakedDescriptor(t *testing.T) {
	shortSettle(t)
	path := filepath.Join(t.TempDir(), "planted")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder(t)

	Watch(rec, Options{})
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	rec.finish()

	if !rec.Failed() {
		t.Fatal("a descriptor still open at the end of the test did not fail the watch")
	}
	if want := fmt.Sprintf("1 descriptor(s) leaked: %d", f.Fd()); !strings.Contains(rec.failure(), want) {
		t.Errorf("the failure does not report the leaked descriptor alone, as %q:\n%s", want, rec.failure())
	}
	// Only Linux can name a descriptor's target from the standard library.
	if runtime.GOOS == "linux" && !strings.Contains(rec.failure(), "planted") {
		t.Errorf("the failure does not name the leaked descriptor's file:\n%s", rec.failure())
	}
}

func TestWatchPassesDescriptorsClosedBeforeTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed")
	rec := newRecorder(t)

	Watch(rec, Options{})
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	_ = w.Close()
	rec.finish()

	if rec.Failed() {
		t.Fatalf("descriptors closed before the test ended failed the watch:\n%s", rec.failure())
	}
}

// dropOpen opens path and drops the file without closing it, leaving the
// descriptor for the garbage collector to close.
func dropOpen(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Open(path); err != nil {
		t.Fatal(err)
	}
}

func TestWatchPassesDescriptorsOnlyTheCollectorHolds(t *testing.T) {
	shortSettle(t)
	path := filepath.Join(t.TempDir(), "dropped")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder(t)

	Watch(rec, Options{})
	dropOpen(t, path)
	rec.finish()

	if rec.Failed() {
		t.Fatalf("a dropped file the collector closes failed the watch:\n%s", rec.failure())
	}
}

func TestWatchPassesADescriptorClosedThatPredatesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "earlier")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	earlier, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecorder(t)

	Watch(rec, Options{})
	// Closing a descriptor older than the watch frees a low number, so
	// whatever the check opens to list descriptors lands on a number of its
	// own; none of it may be counted as the test's.
	_ = earlier.Close()
	rec.finish()

	if rec.Failed() {
		t.Fatalf("closing a descriptor opened before the watch failed it:\n%s", rec.failure())
	}
}
