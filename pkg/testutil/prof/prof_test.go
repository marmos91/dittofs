package prof

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a testing.TB whose failures, logs and cleanups are kept rather
// than acted on, so a test can plant a leak, run the watch's cleanup and
// assert that it failed without failing itself. Every method it does not
// override reaches the real test.
type recorder struct {
	testing.TB
	dir      string
	errors   []string
	logs     []string
	cleanups []func()
}

func newRecorder(t *testing.T) *recorder {
	return &recorder{TB: t, dir: t.TempDir()}
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recorder) ArtifactDir() string { return r.dir }

func (r *recorder) Failed() bool { return len(r.errors) > 0 }

// finish runs the recorded cleanups last-in first-out, as the testing package
// would at the end of the test.
func (r *recorder) finish() {
	for _, f := range slices.Backward(r.cleanups) {
		f()
	}
	r.cleanups = nil
}

// failure returns every recorded failure as one string.
func (r *recorder) failure() string { return strings.Join(r.errors, "\n") }

func TestWatchCleanBodyPasses(t *testing.T) {
	rec := newRecorder(t)

	Watch(rec, Options{})
	rec.finish()

	if rec.Failed() {
		t.Fatalf("a body that leaks nothing failed the watch:\n%s", rec.failure())
	}
}

// shortSettle shortens the watch's retry window for a test that plants a
// leak, so the test waits a fraction of the real window before the watch
// gives up on the leak going away.
func shortSettle(t *testing.T) {
	t.Helper()

	prev := settle
	settle = 200 * time.Millisecond
	t.Cleanup(func() { settle = prev })
}

// plantedLeak blocks until release is closed. It is a named function so the
// failure can be checked for naming it.
func plantedLeak(release <-chan struct{}) { <-release }

func TestWatchFailsOnLeakedGoroutine(t *testing.T) {
	shortSettle(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rec := newRecorder(t)

	Watch(rec, Options{})
	go plantedLeak(release)
	rec.finish()

	if !rec.Failed() {
		t.Fatal("a goroutine still running at the end of the test did not fail the watch")
	}
	if !strings.Contains(rec.failure(), "plantedLeak") {
		t.Errorf("the failure does not name the leaked goroutine's function:\n%s", rec.failure())
	}
}

func TestWatchWaitsForJoinedGoroutinesToReturn(t *testing.T) {
	rec := newRecorder(t)

	Watch(rec, Options{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			wg.Done()
			// Joined, but still running when the test ends: the shape of a
			// component whose Close has waited for its workers to say done.
			time.Sleep(50 * time.Millisecond)
		}()
	}
	wg.Wait()
	rec.finish()

	if rec.Failed() {
		t.Fatalf("goroutines that return shortly after the test ends failed the watch:\n%s", rec.failure())
	}
}

func TestWatchIgnoresGoroutinesStartedBeforeIt(t *testing.T) {
	shortSettle(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go plantedLeak(release)
	rec := newRecorder(t)

	Watch(rec, Options{})
	rec.finish()

	if rec.Failed() {
		t.Fatalf("a goroutine running before the watch began was blamed on the test:\n%s", rec.failure())
	}
}

// artifacts returns the names of the files in dir.
func artifacts(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

func TestWatchWritesProfilesOnFailure(t *testing.T) {
	shortSettle(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rec := newRecorder(t)

	Watch(rec, Options{})
	go plantedLeak(release)
	rec.finish()

	got := artifacts(t, rec.dir)
	for _, want := range []string{"goroutines.txt", "heap.pb.gz", "allocs.pb.gz"} {
		if !slices.Contains(got, want) {
			t.Errorf("the artifact directory holds %v, missing %s", got, want)
		}
	}
	dump, err := os.ReadFile(filepath.Join(rec.dir, "goroutines.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dump), "plantedLeak") {
		t.Errorf("goroutines.txt does not hold the leaked goroutine:\n%s", dump)
	}
	if !strings.Contains(rec.failure(), rec.dir) {
		t.Errorf("the failure does not say where the profiles are:\n%s", rec.failure())
	}
}

func TestWatchWritesNothingOnSuccess(t *testing.T) {
	rec := newRecorder(t)

	Watch(rec, Options{})
	rec.finish()

	if got := artifacts(t, rec.dir); len(got) != 0 {
		t.Errorf("a passing watch wrote %v", got)
	}
}

func TestWatchFailsBothOfTwoOverlappingWatches(t *testing.T) {
	a, b := newRecorder(t), newRecorder(t)

	Watch(a, Options{})
	Watch(b, Options{})
	b.finish()
	a.finish()

	for name, rec := range map[string]*recorder{"first": a, "second": b} {
		if !strings.Contains(rec.failure(), "overlapping") {
			t.Errorf("the %s of two overlapping watches did not fail on the overlap:\n%s", name, rec.failure())
		}
	}
}

// TestWatchCleanRepeatedly runs a body that does real work and leaks nothing
// under a hundred watches in a row: goroutines spawned and joined, files and a
// pipe opened and closed, a large buffer allocated and dropped. One false
// positive fails it, so the watch cannot flake on a clean test.
func TestWatchCleanRepeatedly(t *testing.T) {
	dir := t.TempDir()
	for i := range 100 {
		rec := newRecorder(t)

		Watch(rec, Options{})
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { time.Sleep(time.Millisecond) })
		}
		wg.Wait()
		path := filepath.Join(dir, fmt.Sprint(i))
		if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		_ = w.Close()
		buf := make([]byte, 64<<20)
		buf[len(buf)-1] = 1
		rec.finish()

		if rec.Failed() {
			t.Fatalf("run %d of a body that leaks nothing failed the watch:\n%s", i, rec.failure())
		}
	}
}

// TestWatchOnARealTest watches this test itself, through the testing
// package's own Cleanup and ArtifactDir.
func TestWatchOnARealTest(t *testing.T) {
	Watch(t, Options{})

	done := make(chan struct{})
	go func() { close(done) }()
	<-done
}

func TestWatchSaysHowToKeepProfilesWhenTheyAreNotKept(t *testing.T) {
	shortSettle(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rec := newRecorder(t)

	Watch(rec, Options{})
	go plantedLeak(release)
	rec.finish()

	kept := flag.Lookup("test.artifacts").Value.String() == "true"
	hinted := strings.Contains(rec.failure(), "rerun with -artifacts")
	if hinted == kept {
		t.Errorf("with -artifacts=%v, the failure hints at it: %v; want the hint exactly when the profiles are not kept:\n%s", kept, hinted, rec.failure())
	}
}
