package engine

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// putProbe counts concurrent PutBlock calls reaching the remote.
type putProbe struct {
	*remotememory.Store
	inFlight atomic.Int64
	peak     atomic.Int64
	calls    atomic.Int64
	// delay is how long each PutBlock is held open. It has to outlast the
	// spread in when workers arrive, or the peak measures arrival jitter
	// instead of the bound under test: workers that come and go one at a time
	// read as low concurrency however wide the bound actually is.
	delay time.Duration
}

func (p *putProbe) PutBlock(ctx context.Context, id string, r io.Reader) error {
	p.calls.Add(1)
	cur := p.inFlight.Add(1)
	for {
		mx := p.peak.Load()
		if cur <= mx || p.peak.CompareAndSwap(mx, cur) {
			break
		}
	}
	defer p.inFlight.Add(-1)
	// Real PUT latency; without overlap an absent bound looks like a working one.
	d := p.delay
	if d == 0 {
		d = 30 * time.Millisecond
	}
	time.Sleep(d)
	return p.Store.PutBlock(ctx, id, r)
}

// newUploadWindowFixture wires a manual-sync syncer over a journal holding one
// file of blockCount distinct blocks, and returns it with the PUT probe. The
// payloads must be distinct or dedup collapses them and there is no upload
// concurrency left to bound.
func newUploadWindowFixture(t *testing.T, blockSize int64, blockCount int) (*RemoteSync, *putProbe) {
	t.Helper()
	ctx := context.Background()

	probe := &putProbe{Store: remotememory.New()}
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := journal.Open(t.TempDir(), journal.Config{CarveBlockSize: blockSize})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.ManualSync = true
	cfg.ChunkParams = chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10}
	m := NewRemoteSync(local, probe, ms, cfg)
	m.SetSyncedHashStore(ms)
	m.SetRemoteBlockStore(probe)
	if !m.carveActive.Load() {
		t.Fatal("carve substrate should be active")
	}

	rng := rand.New(rand.NewSource(11))
	var off int64
	for range blockCount {
		buf := make([]byte, blockSize)
		if _, err := rng.Read(buf); err != nil {
			t.Fatalf("rand: %v", err)
		}
		if err := local.WriteAt(ctx, "share/p1", off, buf); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		off += int64(len(buf))
	}
	return m, probe
}

// TestUploadWindow_BoundsConcurrentPutBlock pins that the upload window is the
// whole bound on concurrent PutBlock calls, not one factor of it. A per-pass
// window nested inside this one makes the PUTs in flight the product of the
// two, so a limit of 1 admits a pass's worth of simultaneous uploads and the
// configured ceiling means nothing.
func TestUploadWindow_BoundsConcurrentPutBlock(t *testing.T) {
	ctx := context.Background()
	const window = 1 // the strictest bound there is: a lone PUT at a time
	m, probe := newUploadWindowFixture(t, 32<<10, 12)

	m.uploadLimiter.SetLimit(window)
	_ = m.uploadLimiter.TakePeak() // reset the high-water mark

	m.carvePass(ctx)

	sampled := m.uploadLimiter.TakePeak()
	t.Logf("uploadLimiter.Limit()=%d  TakePeak()=%d  PutBlock calls=%d  peak concurrent PutBlock=%d",
		m.uploadLimiter.Limit(), sampled, probe.calls.Load(), probe.peak.Load())

	if got := probe.calls.Load(); got <= window {
		t.Fatalf("only %d PutBlock calls; need > window (%d) for the bound to mean anything", got, window)
	}
	if peak := probe.peak.Load(); peak > int64(window) {
		t.Errorf("peak concurrent PutBlock = %d, want <= upload window %d", peak, window)
	}
	if sampled > window {
		t.Errorf("TakePeak sampled %d > limit %d", sampled, window)
	}
}

// TestUploadWindow_ControllerSamplesPutConcurrency pins what the adaptive
// controller reads. It resizes the upload window, so its sample has to be the
// concurrency that window governs — PutBlock calls. Sampling a window that
// counts carve passes instead reports one lone pass draining a file as
// app-limited while a pass's worth of PUTs is in the air, and the controller
// then holds a window that is in fact the binding constraint.
func TestUploadWindow_ControllerSamplesPutConcurrency(t *testing.T) {
	ctx := context.Background()
	// Below what one file's blocks can offer, so the window is genuinely what
	// binds and an honest sample has to say so.
	const window = 4
	m, probe := newUploadWindowFixture(t, 32<<10, 12)

	m.uploadLimiter.SetLimit(window)
	_ = m.uploadLimiter.TakePeak()

	m.carvePass(ctx)

	peak := m.uploadLimiter.TakePeak()
	windowLimited := peak >= m.uploadLimiter.Limit() // the exact expression in adaptiveUploadTick
	t.Logf("limit=%d TakePeak=%d windowLimited=%v | PutBlock calls=%d peak concurrent PutBlock=%d",
		m.uploadLimiter.Limit(), peak, windowLimited, probe.calls.Load(), probe.peak.Load())

	if probe.calls.Load() <= window {
		t.Fatalf("only %d PutBlock calls; need > window (%d) to be able to fill it", probe.calls.Load(), window)
	}
	if probe.peak.Load() < 2 {
		t.Fatalf("only %d concurrent PUTs; nothing was overlapping", probe.peak.Load())
	}
	// The sample must account for every PUT in flight. A slot spans PutBlock
	// plus the commit that follows, so it may exceed the probe's count — but it
	// can never fall short of it.
	if int64(peak) < probe.peak.Load() {
		t.Errorf("TakePeak sampled %d while %d PutBlock calls were concurrent: the sample is not the PUT window",
			peak, probe.peak.Load())
	}
	if !windowLimited {
		t.Errorf("controller read the drain as app-limited (TakePeak=%d < limit=%d) while %d PUTs were in flight",
			peak, m.uploadLimiter.Limit(), probe.peak.Load())
	}
}

// newManyFileFixture wires a manual-sync syncer over a journal of shardCount
// shards holding fileCount single-block files. Carve serializes per shard
// (journal flushMu is shard-scoped), so the files must spread across shards for
// any of them to upload at once.
func newManyFileFixture(t *testing.T, blockSize int64, shardCount, fileCount int) (*RemoteSync, *putProbe) {
	t.Helper()
	ctx := context.Background()

	// Wide fan-out means a wide spread in worker start times; hold each PUT
	// open well past that spread so the peak reflects the bound, not the jitter.
	probe := &putProbe{Store: remotememory.New(), delay: 250 * time.Millisecond}
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := journal.Open(t.TempDir(), journal.Config{
		CarveBlockSize: blockSize,
		ShardCount:     shardCount,
	})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cfg := DefaultConfig()
	cfg.ManualSync = true
	cfg.ChunkParams = chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10}
	m := NewRemoteSync(local, probe, ms, cfg)
	m.SetSyncedHashStore(ms)
	m.SetRemoteBlockStore(probe)

	rng := rand.New(rand.NewSource(11))
	for i := range fileCount {
		buf := make([]byte, blockSize)
		if _, err := rng.Read(buf); err != nil {
			t.Fatalf("rand: %v", err)
		}
		if err := local.WriteAt(ctx, journal.FileID(fmt.Sprintf("share/p%03d", i)), 0, buf); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	}
	return m, probe
}

// TestUploadWindow_FanOutDoesNotThrottleBelowTheWindow pins that the carve
// fan-out never becomes the narrow bound when there is a wide window and plenty
// of files to fill it.
//
// A fan-out fixed at carveFanOut collides with the journal's shard count:
// carve serializes per shard, so N workers scattered balls-in-bins over N
// shards leave roughly a third of them idle, and PUT concurrency lands well
// under the window. That relocates exactly the defect this file exists to pin —
// a window that does not bound what it claims to — from one large file onto
// many small ones.
//
// The fixture gives every file its own shard to compete for (64 shards, 64
// single-block files) so the only thing that can hold concurrency down to
// carveFanOut is the fan-out itself.
func TestUploadWindow_FanOutDoesNotThrottleBelowTheWindow(t *testing.T) {
	ctx := context.Background()
	const files = 64
	const shards = 64
	const window = 64 // wide: the window is explicitly not the constraint here
	m, probe := newManyFileFixture(t, 32<<10, shards, files)

	m.uploadLimiter.SetLimit(window)
	_ = m.uploadLimiter.TakePeak()

	m.carvePass(ctx)

	peak := m.uploadLimiter.TakePeak()
	t.Logf("files=%d shards=%d limit=%d | TakePeak=%d windowLimited=%v | PutBlock calls=%d peak concurrent PutBlock=%d",
		files, shards, m.uploadLimiter.Limit(), peak, peak >= m.uploadLimiter.Limit(),
		probe.calls.Load(), probe.peak.Load())

	if got := probe.calls.Load(); got != files {
		t.Fatalf("PutBlock called %d times, want one per file (%d)", got, files)
	}
	// The fan-out cap must not be the ceiling: with this many files on this many
	// shards and a window of 64, concurrency has to climb past the fixed cap.
	if got := probe.peak.Load(); got <= int64(carveFanOut) {
		t.Errorf("peak concurrent PutBlock = %d, capped at or below carveFanOut (%d) while the window allowed %d: the fan-out is the narrow bound",
			got, carveFanOut, window)
	}
}
