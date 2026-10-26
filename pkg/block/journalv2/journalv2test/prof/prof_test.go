package prof

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeTB records what Watch reports instead of failing the real test, and
// keeps the cleanups Watch registers so the test runs them itself. Unlike a
// real TB, its Fatal and Fatalf do not stop the goroutine.
type fakeTB struct {
	testing.TB

	artifacts string
	errors    []string
	logs      []string
	cleanups  []func()
}

func newFakeTB(t *testing.T) *fakeTB {
	return &fakeTB{
		TB:        t,
		artifacts: t.TempDir(),
	}
}

func (f *fakeTB) Error(args ...any) {
	f.errors = append(f.errors, fmt.Sprint(args...))
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Fatal(args ...any) {
	f.Error(args...)
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
}

func (f *fakeTB) Log(args ...any) {
	f.logs = append(f.logs, fmt.Sprint(args...))
}

func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Cleanup(fn func()) {
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeTB) ArtifactDir() string {
	return f.artifacts
}

// end runs the recorded cleanups in reverse order, as the testing package
// does at the end of a test.
func (f *fakeTB) end() {
	for len(f.cleanups) > 0 {
		last := len(f.cleanups) - 1
		fn := f.cleanups[last]

		f.cleanups = f.cleanups[:last]
		fn()
	}
}

// errorsContaining returns the recorded errors that contain s.
func (f *fakeTB) errorsContaining(s string) []string {
	var out []string
	for _, e := range f.errors {
		if strings.Contains(e, s) {
			out = append(out, e)
		}
	}

	return out
}

func (f *fakeTB) logged(s string) bool {
	for _, l := range f.logs {
		if strings.Contains(l, s) {
			return true
		}
	}

	return false
}

var profiles = []string{"goroutine.txt", "heap.pb.gz", "allocs.pb.gz"}

// assertProfiles checks that every profile was written, non-empty, to dir.
func assertProfiles(t *testing.T, dir string) {
	t.Helper()

	for _, name := range profiles {
		fi, err := os.Stat(filepath.Join(dir, name))
		switch {
		case err != nil:
			t.Errorf("profile %s: %v", name, err)
		case fi.Size() == 0:
			t.Errorf("profile %s is empty", name)
		}
	}
}

// assertNoProfiles checks that no profile was written to dir.
func assertNoProfiles(t *testing.T, dir string) {
	t.Helper()

	for _, name := range profiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("profile %s was written, want none", name)
		}
	}
}

// sink keeps a buffer reachable, so the compiler cannot drop the allocation.
var sink []byte

// leakyWait is the planted goroutine; its name is what the report must show.
func leakyWait(release <-chan struct{}) {
	<-release
}

// TestWatchClean checks that a test which joins what it starts passes. It
// passes on a Watch that checks nothing too: what it guards against is a
// false failure, not a missed leak.
func TestWatchClean(t *testing.T) {
	f := newFakeTB(t)
	Watch(f, Options{
		MaxHeapGrowth: 16 << 20, // 16 MB
	})

	// Goroutines released and not waited for, so some are still exiting
	// when the check runs.
	done := make(chan struct{})
	for range 8 {
		go leakyWait(done)
	}
	close(done)

	file, err := os.Create(filepath.Join(t.TempDir(), "clean"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	sink = make([]byte, 64<<20) // 64 MB to simulate heap usage
	sink = nil

	f.end()

	if len(f.errors) != 0 {
		t.Errorf("Watch reported a leak on a clean test: %q", f.errors)
	}

	assertNoProfiles(t, f.artifacts)
}

// closeOnRelease holds f open until release is closed, then closes it on its
// way out.
func closeOnRelease(release <-chan struct{}, f *os.File) {
	<-release
	f.Close()
}

// TestWatchChecksGoroutinesFirst checks that descriptors are checked only
// once the goroutine check has settled, so a descriptor that a goroutine
// closes on its way out is not reported.
func TestWatchChecksGoroutinesFirst(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once

	t.Cleanup(func() {
		once.Do(func() { close(release) })
	})

	afterFailedPoll = func() {
		once.Do(func() { close(release) })
	}
	t.Cleanup(func() { afterFailedPoll = nil })

	f := newFakeTB(t)
	Watch(f, Options{})

	file, err := os.Create(filepath.Join(t.TempDir(), "held"))
	if err != nil {
		t.Fatal(err)
	}
	go closeOnRelease(release, file)

	f.end()

	if len(f.errors) != 0 {
		t.Errorf("Watch reported a descriptor its goroutine closed on exit: %q", f.errors)
	}
}

// TestWatchReportsEveryLeak checks that one failing check does not hide the
// others, nor one leaked goroutine the next.
func TestWatchReportsEveryLeak(t *testing.T) {
	release := make(chan struct{})
	var leaked *os.File
	var retained []byte

	t.Cleanup(func() {
		close(release)
		if leaked != nil {
			leaked.Close()
		}
		runtime.KeepAlive(retained)
	})

	f := newFakeTB(t)
	Watch(f, Options{
		MaxHeapGrowth: 16 << 20, // 16 MB
	})

	go leakyWait(release)
	go leakyWait(release)

	leaked, err := os.Create(filepath.Join(t.TempDir(), "leaked"))
	if err != nil {
		t.Fatal(err)
	}

	retained = make([]byte, 64<<20) // 64 MB to exceed the MaxHeapGrowth threshold

	f.end()

	if got := len(f.errorsContaining("leakyWait")); got != 2 {
		t.Errorf("goroutine errors = %d, want 2: %q", got, f.errors)
	}

	wantFDs := 0
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		wantFDs = 1
	}
	if got := len(f.errorsContaining("prof: descriptor")); got != wantFDs {
		t.Errorf("descriptor errors = %d, want %d: %q", got, wantFDs, f.errors)
	}

	wantHeap := 1
	if raceEnabled {
		wantHeap = 0
	}
	if got := len(f.errorsContaining("prof: heap grew")); got != wantHeap {
		t.Errorf("heap errors = %d, want %d: %q", got, wantHeap, f.errors)
	}
}

// TestWatchRetries checks that the goroutine check polls again rather than
// failing at the first poll that still sees a goroutine the test started.
func TestWatchRetries(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once

	t.Cleanup(func() {
		once.Do(func() { close(release) })
	})

	polls := 0
	afterFailedPoll = func() {
		polls++
		once.Do(func() { close(release) })
	}
	t.Cleanup(func() { afterFailedPoll = nil })

	f := newFakeTB(t)
	Watch(f, Options{})

	go leakyWait(release)

	f.end()

	if polls == 0 {
		t.Errorf("no poll found the goroutine, want at least one before it was released")
	}
	if len(f.errors) != 0 {
		t.Errorf("Watch reported a goroutine released after its first poll: %q", f.errors)
	}
}

func TestWatchFailsOnLeakedGoroutine(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	f := newFakeTB(t)
	Watch(f, Options{})

	go leakyWait(release)

	f.end()

	if got := f.errorsContaining("leakyWait"); len(got) != 1 || len(f.errors) != 1 {
		t.Errorf("errors = %q, want one naming leakyWait", f.errors)
	}
	assertProfiles(t, f.artifacts)

	stacks, err := os.ReadFile(filepath.Join(f.artifacts, "goroutine.txt"))
	if err == nil && !strings.Contains(string(stacks), "leakyWait") {
		t.Errorf("goroutine profile does not show leakyWait")
	}
}

func TestWatchFailsOnLeakedDescriptor(t *testing.T) {
	var leaked *os.File
	t.Cleanup(func() {
		if leaked != nil {
			leaked.Close()
		}
	})

	f := newFakeTB(t)
	Watch(f, Options{})

	path := filepath.Join(t.TempDir(), "leaked")
	leaked, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	f.end()

	switch runtime.GOOS {
	case "linux":
		if got := f.errorsContaining(path); len(got) != 1 || len(f.errors) != 1 {
			t.Errorf("errors = %q, want one naming %s", f.errors, path)
		}

		assertProfiles(t, f.artifacts)
	case "darwin":
		if len(f.errors) != 1 {
			t.Errorf("errors = %q, want one", f.errors)
		}

		assertProfiles(t, f.artifacts)
	default:
		if !f.logged("descriptor check did not run") {
			t.Errorf("logs = %q, want one saying the descriptor check did not run", f.logs)
		}
	}
}

func TestWatchFailsOnRetainedHeap(t *testing.T) {
	var retained []byte
	t.Cleanup(func() { runtime.KeepAlive(retained) })

	f := newFakeTB(t)
	Watch(f, Options{
		MaxHeapGrowth: 16 << 20, // 16 MB
	})

	retained = make([]byte, 64<<20) // 64 MB to exceed the MaxHeapGrowth threshold

	f.end()

	if raceEnabled {
		if !f.logged("heap check off") {
			t.Errorf("logs = %q, want one saying the heap check is off under -race", f.logs)
		}
		if len(f.errors) != 0 {
			t.Errorf("errors = %q, want none with the heap check off", f.errors)
		}
		assertNoProfiles(t, f.artifacts)
		return
	}

	if got := f.errorsContaining("heap"); len(got) != 1 || len(f.errors) != 1 {
		t.Errorf("errors = %q, want one about the heap", f.errors)
	}

	assertProfiles(t, f.artifacts)
}

func TestWatchZeroHeapThresholdIsOff(t *testing.T) {
	var retained []byte
	t.Cleanup(func() { runtime.KeepAlive(retained) })

	f := newFakeTB(t)
	Watch(f, Options{})

	retained = make([]byte, 64<<20) // 64 MB to simulate heap usage

	f.end()

	if !f.logged("heap check off") {
		t.Errorf("logs = %q, want one saying the heap check is off", f.logs)
	}

	if len(f.errors) != 0 {
		t.Errorf("errors = %q, want none with the heap check off", f.errors)
	}
}
