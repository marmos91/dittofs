// Package prof checks that a test leaves no goroutine, descriptor or heap
// growth behind, and writes goroutine, heap and allocation profiles when it
// does. It uses the standard library only, because the journal's tree may not
// import anything else.
package prof

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	retryFor  = 2 * time.Second
	pollEvery = 10 * time.Millisecond
)

// Options configures Watch.
type Options struct {
	// MaxHeapGrowth is the most HeapAlloc may grow over the test, in bytes,
	// measured after a garbage collection at each end. Zero turns the heap
	// check off, and Watch logs that it did.
	MaxHeapGrowth uint64
}

// afterFailedPoll, when set, runs after each poll of the goroutine check that
// still finds a goroutine the test started, before the next poll.
var afterFailedPoll func()

// Watch registers a cleanup that fails t if, by the end of the test, it left
// a goroutine running, a descriptor open or the heap grown by more than
// opts.MaxHeapGrowth. On any failure it writes goroutine, heap and allocation
// profiles to t.ArtifactDir().
//
// Cleanups run in reverse order, so Watch must be the first call in the test:
// only then does its check run after every other cleanup has released what
// the test started.
//
// ponytail: every check reads process-wide state, so Watch does not work
// with t.Parallel: a parallel test's goroutines, descriptors and heap would
// count as this test's leaks. Scope the checks to the test before allowing it.
func Watch(t testing.TB, opts Options) {
	t.Helper()

	dir := fdDir()
	var fds map[string]string
	if dir != "" {
		var err error
		if fds, err = descriptors(dir); err != nil {
			t.Fatalf("prof: %v", err)
			return
		}
	}

	before := goroutines()
	heap := heapAlloc()

	t.Cleanup(func() {
		// Goroutines first: one still exiting may hold a descriptor or memory.
		leakedG := checkGoroutines(t, before)
		leakedFD := checkDescriptors(t, dir, fds)
		grewHeap := checkHeap(t, opts, heap)

		if leakedG || leakedFD || grewHeap {
			writeProfiles(t)
		}
	})
}

// checkGoroutines reports each goroutine running now that was not running
// when Watch took its snapshot.
//
// decision: the check polls for up to retryFor of real time, the one
// real-time wait in the journal's tests, because goroutines a closed journal
// has joined can still be exiting when the cleanup runs. No goroutine is
// filtered: the runner's goroutines predate the snapshot, and the subtest and
// finalizer goroutines that can appear after it are transient, so the retry
// absorbs them. Add a filter, with a test that fails without it, when a
// goroutine the test did not start outlives the retry.
func checkGoroutines(t testing.TB, before map[string]string) bool {
	deadline := time.Now().Add(retryFor)
	for {
		leaked := goroutines()
		for id := range before {
			delete(leaked, id)
		}

		if len(leaked) == 0 {
			return false
		}

		if time.Now().After(deadline) {
			for _, id := range slices.Sorted(maps.Keys(leaked)) {
				t.Errorf("prof: goroutine left running:\n%s", leaked[id])
			}

			return true
		}

		if afterFailedPoll != nil {
			afterFailedPoll()
		}

		time.Sleep(pollEvery)
	}
}

// goroutines returns the stack of every goroutine, keyed by its ID. The
// runtime never reuses an ID, so one missing from an earlier snapshot is a
// goroutine started since.
func goroutines() map[string]string {
	buf := make([]byte, 64<<10) // 64 KB initial buffer for goroutine stack traces
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}

		buf = make([]byte, 2*len(buf))
	}

	out := make(map[string]string)
	for stack := range strings.SplitSeq(string(buf), "\n\n") {
		id, _, _ := strings.Cut(strings.TrimPrefix(stack, "goroutine "), " ")
		out[id] = stack
	}

	return out
}

// ponytail: untested on macOS, where /dev/fd entries are not symlinks, so a
// leak there is reported by number only and a reused number is not seen.
// Test it on macOS before relying on the check there.
func fdDir() string {
	switch runtime.GOOS {
	case "linux":
		return "/proc/self/fd"
	case "darwin":
		return "/dev/fd"
	default:
		return ""
	}
}

// descriptors returns the open descriptors, each mapped to the file it names,
// or to "" where the platform does not say. The descriptor that reads the
// listing is left out.
func descriptors(dir string) (map[string]string, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()

	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}

	self := strconv.Itoa(int(d.Fd()))
	fds := make(map[string]string, len(names))
	for _, name := range names {
		if name != self {
			fds[name], _ = os.Readlink(filepath.Join(dir, name))
		}
	}

	return fds, nil
}

// checkDescriptors reports each descriptor open now that was not open when
// Watch took its snapshot, or that names another file since.
func checkDescriptors(t testing.TB, dir string, before map[string]string) bool {
	if dir == "" {
		t.Logf("prof: descriptor check did not run on %s", runtime.GOOS)
		return false
	}

	after, err := descriptors(dir)
	if err != nil {
		t.Errorf("prof: %v", err)
		return true
	}

	// ponytail: a descriptor that closes while the listing runs reads as a
	// changed target, because its readlink fails. Nothing closes descriptors
	// then, since the goroutine check has settled first. Treat a failed
	// readlink as closed, with a test, if that ever shows up.
	//
	// ponytail: descriptors the runtime's poller opens during a test would
	// read as leaks. Under go test the poller is already up before Watch, so
	// prime it before the snapshot, with a test, if that ever shows up.
	failed := false
	for _, fd := range slices.Sorted(maps.Keys(after)) {
		if target, ok := before[fd]; !ok || target != after[fd] {
			t.Errorf("prof: descriptor %s left open: %q", fd, after[fd])
			failed = true
		}
	}

	return failed
}

func heapAlloc() uint64 {
	runtime.GC()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return m.HeapAlloc
}

// checkHeap reports a heap that grew by more than opts.MaxHeapGrowth since
// Watch measured it.
func checkHeap(t testing.TB, opts Options, before uint64) bool {
	switch {
	case raceEnabled:
		t.Logf("prof: heap check off under -race")
		return false
	case opts.MaxHeapGrowth == 0:
		t.Logf("prof: heap check off, MaxHeapGrowth is zero")
		return false
	}

	after := heapAlloc()
	if after > before && after-before > opts.MaxHeapGrowth {
		t.Errorf("prof: heap grew by %d bytes, more than %d", after-before, opts.MaxHeapGrowth)
		return true
	}

	return false
}

// writeProfiles writes the goroutine, heap and allocation profiles to the
// test's artifact directory.
// writeProfiles writes the goroutine, heap and allocation profiles to the
// test's artifact directory.
func writeProfiles(t testing.TB) {
	dir := t.ArtifactDir()

	writeProfile(t, filepath.Join(dir, "goroutine.txt"), "goroutine", 2)
	writeProfile(t, filepath.Join(dir, "heap.pb.gz"), "heap", 0)
	writeProfile(t, filepath.Join(dir, "allocs.pb.gz"), "allocs", 0)

	t.Logf("prof: profiles written to %s", dir)
}

// writeProfile writes the runtime profile name to path. debug 0 is the
// compressed format go tool pprof reads; 2 is every goroutine's stack as text.
func writeProfile(t testing.TB, path, name string, debug int) {
	f, err := os.Create(path)
	if err != nil {
		t.Errorf("prof: %v", err)
		return
	}

	if err := pprof.Lookup(name).WriteTo(f, debug); err != nil {
		f.Close()
		t.Errorf("prof: writing %s: %v", path, err)
		return
	}

	if err := f.Close(); err != nil {
		t.Errorf("prof: closing %s: %v", path, err)
	}
}
