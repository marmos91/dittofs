// Package prof fails a test that leaves goroutines, open descriptors or live
// heap behind it.
//
// It uses the standard library only, and must keep to it: the journal's tree,
// which forbids every third-party package, reaches it through its test
// components.
package prof

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options tunes a watch. The zero value is the default.
type Options struct {
	// HeapSlack is how far the live heap may grow over the test before the
	// watch fails it. Zero means DefaultHeapSlack.
	HeapSlack uint64
}

// DefaultHeapSlack is the heap growth a test may leave behind when its
// Options name none: room for the runtime's and the testing package's own
// allocations, well under any buffer a component would plausibly leak.
const DefaultHeapSlack = 16 << 20

// settle is how long, in real time, the check waits for leaks to go away.
//
// decision: the check retries in real time rather than on a test clock,
// because a goroutine a closed component has joined can still be returning
// when the test ends, and nothing on a test clock tells the check when it has.
// The wait is spent only on a test that leaks or is slow to wind down; a clean
// test passes on the first look. Shorten it if failing runs make the suite's
// duration matter.
var settle = 2 * time.Second

// watch is one active Watch.
type watch struct {
	// overlapped is set once another watch is active at the same time.
	overlapped bool
}

var (
	activeMu sync.Mutex
	active   = make(map[*watch]struct{})
)

// begin registers w as active, marking it and every watch already active as
// overlapped if there is one.
func (w *watch) begin() {
	activeMu.Lock()
	defer activeMu.Unlock()

	for other := range active {
		other.overlapped = true
		w.overlapped = true
	}
	active[w] = struct{}{}
}

// end deregisters w and reports whether another watch was active at any point
// during it.
func (w *watch) end() bool {
	activeMu.Lock()
	defer activeMu.Unlock()

	delete(active, w)

	return w.overlapped
}

// poll is how long the check sleeps between two looks.
const poll = 10 * time.Millisecond

// Watch snapshots the process's goroutines, open descriptors and live heap
// now, and registers a cleanup that fails tb if, when the test ends,
// goroutines started since are still running, descriptors opened since are
// still open, or the live heap has grown by more than opts.HeapSlack. Where
// the platform cannot list descriptors, the descriptor check does not run;
// under the race detector, the heap check does not run. The watch logs each
// check it skips.
//
// Watches must not overlap: a watched test must not run in parallel with
// another, and a watched test's subtests must not be watched. Two watches
// active at once both fail.
//
// Cleanups run last-in first-out, so call Watch before anything it should
// watch registers its own cleanup: the check then runs after they have closed.
func Watch(tb testing.TB, opts Options) {
	tb.Helper()

	w := &watch{}
	w.begin()
	warmPoller()
	before := goroutines()
	fdsBefore, fdsChecked := descriptors()
	if !fdsChecked {
		tb.Logf("prof: this platform lists no descriptors; the descriptor check does not run")
	}
	slack := opts.HeapSlack
	if slack == 0 {
		slack = DefaultHeapSlack
	}
	var heapBefore uint64
	if raceEnabled {
		tb.Logf("prof: the race detector changes the heap; the heap check does not run")
	} else {
		heapBefore = liveHeap()
	}

	tb.Cleanup(func() {
		// decision: two watches active at once fail rather than wait for each
		// other. Goroutines, descriptors and the heap are the process's, so
		// each watch would see the other test's work as its own leak; and a
		// lock would deadlock a watched test that calls t.Parallel, or a
		// watched parent whose subtests are watched too.
		if w.end() {
			tb.Errorf("prof: overlapping watches: another watch was active during this one, and the checks are process-wide, so neither can be trusted; do not run watched tests in parallel")
			return
		}

		// decision: a collection runs before the checks, so a file the test
		// dropped without closing is closed by its cleanup and not reported.
		// Without it, the same leak would fail or pass depending on whether
		// the test happened to allocate enough to trigger a collection; a file
		// a live object still holds, the leak that matters, is reported either
		// way.
		runtime.GC()

		var leaked []string
		var fdsLeaked []string
		for deadline := time.Now().Add(settle); ; time.Sleep(poll) {
			leaked = leakedGoroutines(before)
			if fdsChecked {
				fdsLeaked = leakedDescriptors(fdsBefore)
			}
			if len(leaked)+len(fdsLeaked) == 0 || time.Now().After(deadline) {
				break
			}
		}

		var failures []string
		if len(leaked) > 0 {
			failures = append(failures, fmt.Sprintf("%d goroutine(s) leaked:\n\n%s", len(leaked), strings.Join(leaked, "\n\n")))
		}
		if len(fdsLeaked) > 0 {
			failures = append(failures, fmt.Sprintf("%d descriptor(s) leaked: %s", len(fdsLeaked), strings.Join(fdsLeaked, ", ")))
		}
		if !raceEnabled {
			if after := liveHeap(); after > heapBefore && after-heapBefore > slack {
				failures = append(failures, fmt.Sprintf("live heap grew by %d bytes, over the %d-byte slack", after-heapBefore, slack))
			}
		}
		if len(failures) == 0 {
			return
		}

		dir := tb.ArtifactDir()
		where := "profiles written to " + dir
		if err := writeProfiles(dir); err != nil {
			where = "profiles not all written to " + dir + ": " + err.Error()
		}
		tb.Errorf("prof: %s\n\n%s", strings.Join(failures, "\n\n"), where)
	})
}

// warmPoller makes sure the runtime's network poller exists before the
// snapshot.
//
// decision: the poller holds descriptors of its own (an epoll or kqueue
// descriptor and a wakeup pipe or eventfd), created on the first file the
// process opens through it and kept for the life of the process. Without this,
// a test that happened to be that first user would be blamed for them.
func warmPoller() {
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	_ = r.Close()
	_ = w.Close()
}

// leakedDescriptors returns the descriptors open now that were not in before,
// each as its number and, where the platform names it, its file.
//
// ponytail: descriptors are compared by number only, so a test that closes a
// descriptor opened before the watch and leaks one that reuses its number goes
// unseen. Compare the Linux targets too if such a leak ever slips through.
func leakedDescriptors(before map[int]string) []string {
	now, _ := descriptors()
	var leaked []int
	for fd := range now {
		if _, ok := before[fd]; !ok {
			leaked = append(leaked, fd)
		}
	}
	slices.Sort(leaked)

	out := make([]string, len(leaked))
	for i, fd := range leaked {
		out[i] = fmt.Sprint(fd)
		if name := now[fd]; name != "" {
			out[i] += " (" + name + ")"
		}
	}

	return out
}
