package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/syncer"
)

// carveFanoutLocal is a LocalStore that records per-file Flush calls and
// synchronizes on channels so a test can observe how many flush passes run at
// once. It overrides ListFiles + Flush; everything else is delegated to a real
// in-memory store rather than to a nil embed, so a carve path that reaches for
// another part of the interface gets that store's honest answer instead of a
// nil dereference.
type carveFanoutLocal struct {
	journal.LocalStore
	files    []string
	started  chan string   // one send per Flush entry
	release  chan struct{} // closed to let every held Flush return
	inFlight atomic.Int32
	mu       sync.Mutex
	carved   map[string]int // FileID -> completed count
}

func (f *carveFanoutLocal) ListFiles(context.Context) []journal.FileID {
	out := make([]journal.FileID, 0, len(f.files))
	for _, id := range f.files {
		out = append(out, journal.FileID(id))
	}
	return out
}

func (f *carveFanoutLocal) Flush(_ context.Context, id journal.FileID, _ journal.FlushOptions, _ journal.FlushFunc) error {
	f.inFlight.Add(1)
	f.started <- string(id)
	<-f.release
	f.inFlight.Add(-1)
	f.mu.Lock()
	f.carved[string(id)]++
	f.mu.Unlock()
	return nil
}

// TestCarvePass_FanOutIsCappedIndependentlyOfUploadWindow proves carvePass
// carves every file exactly once, runs them concurrently, and does not let a
// narrow upload window narrow the fan-out with it: the window is pinned as
// small as it goes and carveFanOut workers still start.
//
// carveFanOut is the FLOOR, not a fixed cap — carvePass sizes the fan-out as
// max(carveFanOut, window), so a wide window widens it. This test exercises the
// floor end only; TestUploadWindow_FanOutDoesNotThrottleBelowTheWindow covers
// the other, where a fan-out frozen at carveFanOut collides with the shard
// count and throttles uploads.
//
// The two were the same semaphore once, and that is what made upload
// concurrency the product of two windows: a pass held an upload slot for its
// whole carve while the blocks inside it opened a second window on the PUTs.
// Re-coupling them would also deadlock — a pass cannot upload through a slot
// its own loop is holding.
func TestCarvePass_FanOutIsCappedIndependentlyOfUploadWindow(t *testing.T) {
	files := make([]string, 0, carveFanOut+2)
	for i := range cap(files) {
		files = append(files, fmt.Sprintf("f%02d", i))
	}
	fl := &carveFanoutLocal{
		LocalStore: memory.New(),
		files:      files,
		started:    make(chan string, len(files)), // never blocks a Flush on send
		release:    make(chan struct{}),
		carved:     map[string]int{},
	}
	m := &RemoteSync{
		local: fl,
		// One slot: on a build where the loop acquires this, only a single
		// carve ever starts and the wait below reports it.
		uploadLimiter: syncer.NewDynamicSemaphore(1),
		stopCh:        make(chan struct{}),
		config:        DefaultConfig(),
	}

	done := make(chan struct{})
	go func() { m.carvePass(context.Background()); close(done) }()

	// Exactly carveFanOut carves start; the loop's Acquire blocks the rest.
	seen := map[string]bool{}
	for i := range carveFanOut {
		select {
		case id := <-fl.started:
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d carves started, want %d: the fan-out is gated by something narrower than carveFanOut", i, carveFanOut)
		}
	}
	require.Equal(t, int32(carveFanOut), fl.inFlight.Load(), "in-flight carves should fill the fan-out")

	// A further carve must NOT start until a slot frees.
	select {
	case id := <-fl.started:
		t.Fatalf("carve %q started beyond the fan-out cap", id)
	case <-time.After(50 * time.Millisecond):
	}

	// Let everything drain; the remaining files carve as slots free.
	close(fl.release)
	for range len(fl.files) - carveFanOut {
		seen[<-fl.started] = true
	}
	<-done

	require.Len(t, seen, len(fl.files), "every file should have been carved")
	for _, id := range fl.files {
		require.Equal(t, 1, fl.carved[id], "file %q carved exactly once", id)
	}
}

// TestCarvePass_NoFilesIsNoop guards the empty working-set path.
func TestCarvePass_NoFilesIsNoop(t *testing.T) {
	fl := &carveFanoutLocal{LocalStore: memory.New(), started: make(chan string, 1), release: make(chan struct{}), carved: map[string]int{}}
	m := &RemoteSync{local: fl, uploadLimiter: syncer.NewDynamicSemaphore(4), stopCh: make(chan struct{}), config: DefaultConfig()}
	m.carvePass(context.Background()) // returns immediately, acquires nothing
	require.Equal(t, int32(0), fl.inFlight.Load())
}

// TestCarvePass_NilUploadLimiterDoesNotPanic pins that a RemoteSync built
// without an upload limiter still carves. NewRemoteSync always sets one, but
// the type is also built as a bare struct literal here and in several other
// tests in this package, so nil is a representable state that reaches
// carvePass.
//
// It is a plain read (sizing the fan-out) rather than an acquire, which is
// exactly why it needs the guard: the acquire it replaced was itself nil
// checked, so sizing from the limiter moved the access earlier and lost the
// check with it. Without the guard this panics rather than falling back to the
// fan-out floor.
func TestCarvePass_NilUploadLimiterDoesNotPanic(t *testing.T) {
	fl := &carveFanoutLocal{
		LocalStore: memory.New(),
		files:      []string{"a", "b", "c"},
		started:    make(chan string, 3),
		release:    make(chan struct{}),
		carved:     map[string]int{},
	}
	close(fl.release) // let every Flush return immediately
	m := &RemoteSync{
		local: fl,
		// uploadLimiter deliberately left nil.
		stopCh: make(chan struct{}),
		config: DefaultConfig(),
	}
	require.Nil(t, m.uploadLimiter, "fixture must exercise the nil window")

	m.carvePass(context.Background())

	for _, id := range fl.files {
		require.Equal(t, 1, fl.carved[id], "file %q carved exactly once", id)
	}
}
