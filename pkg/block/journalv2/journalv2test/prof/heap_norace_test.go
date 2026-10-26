//go:build !race

package prof

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sink holds a planted buffer past the end of the test body, as a component
// that never drops what it allocated would.
var sink []byte

// plantRetained allocates n bytes, touches every page so they are really
// committed, and keeps them in sink until the test ends.
func plantRetained(t *testing.T, n int) {
	t.Helper()

	sink = make([]byte, n)
	for i := 0; i < n; i += 4096 {
		sink[i] = 1
	}
	t.Cleanup(func() { sink = nil })
}

func TestWatchFailsOnRetainedHeap(t *testing.T) {
	rec := newRecorder(t)

	Watch(rec, Options{})
	plantRetained(t, 64<<20)
	rec.finish()

	if !rec.Failed() {
		t.Fatal("64 MiB still reachable at the end of the test did not fail the watch")
	}
	if !strings.Contains(rec.failure(), "heap") {
		t.Errorf("the failure does not say the heap grew:\n%s", rec.failure())
	}
	if _, err := os.Stat(filepath.Join(rec.dir, "heap.pb.gz")); err != nil {
		t.Errorf("no heap profile written: %v", err)
	}
}

func TestWatchFailsOnRetainedHeapAfterEarlierGarbage(t *testing.T) {
	garbage := make([]byte, 64<<20)
	for i := 0; i < len(garbage); i += 4096 {
		garbage[i] = 1
	}
	garbage = nil
	_ = garbage
	rec := newRecorder(t)

	Watch(rec, Options{})
	plantRetained(t, 64<<20)
	rec.finish()

	if !rec.Failed() {
		t.Fatal("64 MiB retained by the test did not fail the watch when 64 MiB of garbage preceded it")
	}
}

func TestWatchHeapSlackAllowsGrowthWithinIt(t *testing.T) {
	rec := newRecorder(t)

	Watch(rec, Options{HeapSlack: 128 << 20})
	plantRetained(t, 64<<20)
	rec.finish()

	if rec.Failed() {
		t.Fatalf("64 MiB retained under a 128 MiB slack failed the watch:\n%s", rec.failure())
	}
}
