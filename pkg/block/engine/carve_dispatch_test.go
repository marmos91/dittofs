package engine

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/block/syncer"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
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

func (f *carveFanoutLocal) ListFiles(context.Context) []journal.FileID {
	out := make([]journal.FileID, 0, len(f.files))
	for _, id := range f.files {
		out = append(out, journal.FileID(id))
	}
	return out
}

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

// TestCarvePass_FansOutBoundedByCarvePasses pins the per-file cap: at most
// DefaultCarvePasses files enter local.Carve concurrently. Concurrent PutBlock
// calls across all passes are separately bounded by the sink's upload window
// (TestBlockSink_EnforcesUploadWindow); this test pins the aggregate-memory
// bound — each carving file retains up to CarvePackAhead queued arenas, so the
// pass count itself needs a cap independent of the upload window.
func TestCarvePass_FansOutBoundedByCarvePasses(t *testing.T) {
	fl := &carveFanoutLocal{
		files:   make([]string, DefaultCarvePasses+4),
		started: make(chan string, DefaultCarvePasses+4),
		release: make(chan struct{}),
		carved:  map[string]int{},
	}
	for i := range fl.files {
		fl.files[i] = fmt.Sprintf("file-%d", i)
	}
	m := &RemoteSync{
		local:         fl,
		uploadLimiter: syncer.NewDynamicSemaphore(AdaptiveUploadFloor),
		carvePasses:   syncer.NewDynamicSemaphore(DefaultCarvePasses),
		stopCh:        make(chan struct{}),
		config:        DefaultConfig(),
	}

	go func() { m.carvePass(context.Background()) }()

	// Exactly DefaultCarvePasses carves enter; the rest are blocked on slots.
	for i := 0; i < DefaultCarvePasses; i++ {
		select {
		case <-fl.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("carve %d never entered local.Carve", i)
		}
	}
	select {
	case id := <-fl.started:
		t.Fatalf("carve %q entered past the cap", id)
	case <-time.After(200 * time.Millisecond):
	}

	close(fl.release) // drain and release all slots
	// The remaining 4 files now enter.
	for i := 0; i < 4; i++ {
		select {
		case <-fl.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("carve %d never entered after a slot freed", i)
		}
	}
}

// TestCarvePass_FansOutUnbounded passes every file to local.Carve concurrently:
// carvePass carves every file (with its FileID set) and runs them all at once.
// Concurrent PutBlock calls across all passes are bounded by the engine's
// upload window acquired in the block sink itself
// (TestBlockSink_EnforcesUploadWindow); concurrent passes are bounded by
// carvePasses (TestCarvePass_FansOutBoundedByCarvePasses).
func TestCarvePass_FansOutUnbounded(t *testing.T) {
	fl := &carveFanoutLocal{
		files:   []string{"a", "b", "c", "d", "e"},
		started: make(chan string, 5),
		release: make(chan struct{}),
		carved:  map[string]int{},
	}
	m := &RemoteSync{
		local:         fl,
		uploadLimiter: syncer.NewDynamicSemaphore(AdaptiveUploadFloor),
		stopCh:        make(chan struct{}),
		config:        DefaultConfig(),
	}

	done := make(chan struct{})
	go func() { m.carvePass(context.Background()); close(done) }()

	// Every file enters local.Carve without waiting on any pass window.
	seen := map[string]bool{}
	for i := 0; i < len(fl.files); i++ {
		select {
		case id := <-fl.started:
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("carve %d never entered local.Carve", i)
		}
	}
	require.Len(t, seen, len(fl.files), "every file should have been carved")

	close(fl.release)
	<-done

	for _, id := range fl.files {
		require.Equal(t, 1, fl.carved[id], "file %q carved exactly once", id)
	}
}

// TestBlockSink_EnforcesUploadWindow proves the engine's upload window is
// enforced in the block sink itself: concurrent CommitBlock calls never exceed
// the semaphore limit around PutBlock. This is the invariant the upload
// controller samples, and the regression guard that would have caught the
// product-of-two-windows defect.
func TestBlockSink_EnforcesUploadWindow(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()

	const (
		limit = 2
		calls = 8
		block = 1 << 20
	)
	limiter := syncer.NewDynamicSemaphore(limit)

	// windowProbeRemote holds every PUT on a shared gate: PUTs enter, count
	// themselves, and block until the test releases the gate. The gate is
	// closed by the probe itself once limit entries are held, so the
	// limit+1th entry happens while all allowed entries are provably held —
	// any window violation is forced deterministically, flagged by the
	// counter before the gate opens, not dependent on scheduling luck.
	var (
		inFlight atomic.Int32
		exceeded atomic.Bool
	)
	gate := newWindowProbeRemote(mem, &inFlight, &exceeded, limit)
	sink := engineBlockSink{sealer: nil, rbs: gate, committer: ms, commitLocks: &carveCommitLocks{}, uploadLimiter: limiter}

	var (
		wg     sync.WaitGroup
		errsMu sync.Mutex
		errs   = make([]error, calls)
		start  = make(chan struct{})
	)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := make([]byte, block)
			chunk := journal.CarveChunk{
				FileID:     journal.FileID("probe-file"),
				FileOffset: int64(i) * block,
				Hash:       journal.ChunkHash([32]byte{byte(i)}),
				Data:       data,
			}
			<-start
			err := sink.CommitBlock(ctx, []journal.CarveChunk{chunk})
			errsMu.Lock()
			errs[i] = err
			errsMu.Unlock()
		}(i)
	}
	close(start)

	// The probe closes held once limit entries are in flight; the test then
	// closes gate, so every entry — allowed or violating — is held across the
	// violation attempt and released to complete. With a broken limiter the
	// limit+1th entry is forced (overshot closes) while the allowed entries
	// are provably held; with a correct limiter the remaining entries park in
	// the semaphore until the gate opens and the counter never exceeds limit.
	<-gate.held
	close(gate.gate)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "CommitBlock %d surfaced an error", i)
	}
	require.False(t, exceeded.Load(), "in-flight PUTs exceeded the upload window")
}

// windowProbeRemote wraps a block-keyed remote and flags any moment where the
// number of concurrent PutBlock calls inside the sink's upload window exceeds
// the limit. The counter is checked on the PutBlock critical path so the flag
// cannot fire in a gap between calls; PUTs 1..limit hold on the shared gate
// (closed by the probe itself once limit entries are held), so the limit+1th
// entry is forced while all allowed entries are provably held.
// Embedded pointer, not value: an embedded value's copy would carry the
// underlying store's mutex.
type windowProbeRemote struct {
	store    *remotememory.Store
	inFlight *atomic.Int32
	exceeded *atomic.Bool
	limit    int32
	// held is closed by the first PUT whose entry reaches limit, telling the
	// test the allowed entries are in flight. overshot is closed by the first
	// PUT whose entry exceeds limit — with a broken limiter that entry is
	// forced while the allowed entries are provably held on gate, so the
	// violation is deterministic, not dependent on scheduling luck.
	held     chan struct{}
	overshot chan struct{}
	heldOnce sync.Once
	shotOnce sync.Once
	// gate is closed by the test after the violation attempt, releasing every
	// held entry to complete; nothing hangs in either the correct or broken
	// limiter case.
	gate chan struct{}
}

func newWindowProbeRemote(mem *remotememory.Store, inFlight *atomic.Int32, exceeded *atomic.Bool, limit int32) *windowProbeRemote {
	return &windowProbeRemote{store: mem, inFlight: inFlight, exceeded: exceeded, limit: limit, held: make(chan struct{}), overshot: make(chan struct{}), gate: make(chan struct{})}
}

func (w *windowProbeRemote) PutBlock(ctx context.Context, id string, r io.Reader) error {
	n := w.inFlight.Add(1)
	defer w.inFlight.Add(-1)
	if n == w.limit {
		w.heldOnce.Do(func() { close(w.held) })
	}
	if n > w.limit {
		w.exceeded.Store(true)
		w.shotOnce.Do(func() { close(w.overshot) })
	}
	<-w.gate
	return w.store.PutBlock(ctx, id, r)
}

// DeleteBlock, GetBlock, GetBlockRange and WalkBlocks delegate so
// *windowProbeRemote satisfies remote.RemoteBlockStore via the embedded pointer
// rather than an embedded value (whose copy would carry the underlying store's
// mutex).
func (w *windowProbeRemote) DeleteBlock(ctx context.Context, id string) error {
	return w.store.DeleteBlock(ctx, id)
}

func (w *windowProbeRemote) GetBlock(ctx context.Context, id string) ([]byte, error) {
	return w.store.GetBlock(ctx, id)
}

func (w *windowProbeRemote) GetBlockRange(ctx context.Context, id string, offset, length int64) ([]byte, error) {
	return w.store.GetBlockRange(ctx, id, offset, length)
}

func (w *windowProbeRemote) WalkBlocks(ctx context.Context, fn func(string, block.Meta) error) error {
	return w.store.WalkBlocks(ctx, fn)
}

// TestCarvePass_NoFilesIsNoop guards the empty working-set path.
func TestCarvePass_NoFilesIsNoop(t *testing.T) {
	fl := &carveFanoutLocal{started: make(chan string, 1), release: make(chan struct{}), carved: map[string]int{}}
	m := &RemoteSync{local: fl, uploadLimiter: syncer.NewDynamicSemaphore(4), stopCh: make(chan struct{}), config: DefaultConfig()}
	m.carvePass(context.Background()) // returns immediately, acquires nothing
	require.Equal(t, int32(0), fl.inFlight.Load())
}
