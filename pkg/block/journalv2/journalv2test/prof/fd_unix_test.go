//go:build linux || darwin

package prof

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

func TestWatchFailsWhenTheDescriptorsCannotBeListedAtTheEnd(t *testing.T) {
	var prev syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &prev); err != nil {
		t.Fatal(err)
	}
	open, _ := descriptors()
	lim := prev
	lim.Cur = uint64(len(open) + 16)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &prev) })
	shortSettle(t)
	rec := newRecorder(t)

	Watch(rec, Options{})
	var leaked []*os.File
	t.Cleanup(func() {
		for _, f := range leaked {
			_ = f.Close()
		}
	})
	for {
		f, err := os.Open(os.DevNull)
		if err != nil {
			break
		}
		leaked = append(leaked, f)
	}
	rec.finish()

	if !strings.Contains(rec.failure(), "could not be listed") {
		t.Errorf("a test that leaked descriptors until none could be opened did not fail the watch on the listing:\n%s", rec.failure())
	}
}

func TestWatchReportsEveryLeak(t *testing.T) {
	shortSettle(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	path := filepath.Join(t.TempDir(), "planted")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var retained []byte
	t.Cleanup(func() { runtime.KeepAlive(retained) })
	rec := newRecorder(t)

	Watch(rec, Options{})
	go plantedLeak(release)
	go plantedLeak(release)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	retained = make([]byte, 64<<20)
	for i := 0; i < len(retained); i += 4096 {
		retained[i] = 1
	}
	rec.finish()

	want := []string{"2 goroutine(s) leaked", "1 descriptor(s) leaked"}
	if !raceEnabled {
		want = append(want, "live heap grew")
	}
	for _, w := range want {
		if got := strings.Count(rec.failure(), w); got != 1 {
			t.Errorf("the failure reports %q %d times, want once:\n%s", w, got, rec.failure())
		}
	}
}
