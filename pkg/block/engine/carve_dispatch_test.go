package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
)

// carveFanoutLocal is a minimal LocalStore that records per-file Carve calls and
// synchronizes on channels so a test can observe how many carves run at once.
// Only ListFiles + Carve are exercised by carvePass; the rest of the interface
// is embedded (nil) and never called.
type carveFanoutLocal struct {
	local.LocalStore
	files    []string
	started  chan string   // one send per Carve entry
	release  chan struct{} // closed to let every held Carve return
	inFlight atomic.Int32
	mu       sync.Mutex
	carved   map[string]int // FileID -> completed count
}

func (f *carveFanoutLocal) ListFiles(context.Context) []string { return f.files }

func (f *carveFanoutLocal) Carve(_ context.Context, opts journal.CarveOptions) (journal.CarveResult, error) {
	f.inFlight.Add(1)
	f.started <- string(opts.FileID)
	<-f.release
	f.inFlight.Add(-1)
	f.mu.Lock()
	f.carved[string(opts.FileID)]++
	f.mu.Unlock()
	return journal.CarveResult{BytesCarved: 1, BlocksWritten: 1}, nil
}

// TestCarvePass_FansOutBoundedByUploadWindow proves carvePass carves every file
// (with its FileID set), runs them concurrently, and never exceeds the upload
// window — the fix that gives the uploader more than one block in flight.
func TestCarvePass_FansOutBoundedByUploadWindow(t *testing.T) {
	fl := &carveFanoutLocal{
		files:   []string{"a", "b", "c", "d", "e"},
		started: make(chan string, 5),
		release: make(chan struct{}),
		carved:  map[string]int{},
	}
	const window = 3
	m := &Syncer{
		local:         fl,
		uploadLimiter: newDynamicSemaphore(window),
		stopCh:        make(chan struct{}),
		config:        DefaultConfig(),
	}

	done := make(chan struct{})
	go func() { m.carvePass(context.Background()); close(done) }()

	// Exactly `window` carves start; the loop's Acquire blocks the rest.
	seen := map[string]bool{}
	for i := 0; i < window; i++ {
		seen[<-fl.started] = true
	}
	require.Equal(t, int32(window), fl.inFlight.Load(), "in-flight carves should fill the window")

	// A further carve must NOT start until a slot frees.
	select {
	case id := <-fl.started:
		t.Fatalf("carve %q started before the upload window freed", id)
	case <-time.After(50 * time.Millisecond):
	}

	// Let everything drain; the remaining files carve as slots free.
	close(fl.release)
	for i := 0; i < len(fl.files)-window; i++ {
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
	fl := &carveFanoutLocal{started: make(chan string, 1), release: make(chan struct{}), carved: map[string]int{}}
	m := &Syncer{local: fl, uploadLimiter: newDynamicSemaphore(4), stopCh: make(chan struct{}), config: DefaultConfig()}
	m.carvePass(context.Background()) // returns immediately, acquires nothing
	require.Equal(t, int32(0), fl.inFlight.Load())
}
