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
