package engine

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	gosync "sync"
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

	peak := m.takePutPeak()
	windowLimited := peak >= m.uploadLimiter.Limit() // the exact expression in adaptiveUploadTick
	t.Logf("limit=%d TakePeak=%d windowLimited=%v | PutBlock calls=%d peak concurrent PutBlock=%d",
		m.uploadLimiter.Limit(), peak, windowLimited, probe.calls.Load(), probe.peak.Load())

	if probe.calls.Load() <= window {
		t.Fatalf("only %d PutBlock calls; need > window (%d) to be able to fill it", probe.calls.Load(), window)
	}
	if probe.peak.Load() < 2 {
		t.Fatalf("only %d concurrent PUTs; nothing was overlapping", probe.peak.Load())
	}
	// The sample must account for every PUT in flight.
	if int64(peak) < probe.peak.Load() {
		t.Errorf("sampled peak %d while %d PutBlock calls were concurrent: the sample is not PUT concurrency",
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

// TestAdaptiveUploadTick_HeldSlotsWithoutUploadsDoNotRampTheWindow pins where
// the controller's saturation signal comes from, with no timing involved.
//
// An upload slot is held across PutBlock *and* the per-file-serialized metadata
// commit, so a slow commit keeps slots occupied long after the uploads have
// drained. Sampling the semaphore's own peak reported that as a full window —
// "window-limited", meaning the uplink is saturated, so ramp up — when the
// uplink was idle and the metadata store was the bottleneck. Resizing an upload
// window cannot relieve a commit bottleneck, so the controller must not see one
// as saturation.
//
// The fixture holds every slot while issuing no uploads at all, which is the
// steady state a slow commit produces, and asserts the window is held rather
// than ramped. Reading the semaphore instead grows it.
func TestAdaptiveUploadTick_HeldSlotsWithoutUploadsDoNotRampTheWindow(t *testing.T) {
	ctx := context.Background()
	m, _ := newUploadWindowFixture(t, 32<<10, 1)
	if m.uploadController == nil {
		t.Fatal("fixture must be in adaptive mode for the controller to run")
	}

	// The window must match the controller's own starting point, or the tick
	// snaps the limiter back to that instead and the move says nothing about
	// the saturation signal.
	window := AdaptiveUploadFloor
	startWindow := m.uploadLimiter.Limit()
	if startWindow != window {
		t.Fatalf("fixture starts at window %d, want the adaptive floor %d", startWindow, window)
	}

	// Occupy every slot without a single PutBlock — commits holding slots while
	// the uplink is idle.
	for range window {
		if err := m.uploadLimiter.Acquire(ctx); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}
	if got := m.uploadLimiter.InFlight(); got != window {
		t.Fatalf("held %d slots, want %d", got, window)
	}
	// Bytes delivered earlier in the interval, so the tick is not skipped as idle.
	m.uploadedBytesWindow.Store(8 << 20)

	m.adaptiveUploadTick(0.5)

	got := m.uploadLimiter.Limit()
	t.Logf("slots held=%d (no PutBlock in flight) | window %d -> %d",
		window, startWindow, got)

	if got > startWindow {
		t.Errorf("window ramped %d -> %d on slots that carried no uploads: the controller read commit backpressure as uplink saturation",
			startWindow, got)
	}
}

// TestPutSample_AccountingAndResetBaseline pins the contract takePutPeak owes
// the controller: the peak covers the interval just ended, and the reset
// baseline is the count still in flight, so a long-running upload keeps
// counting into the next interval instead of vanishing from it.
func TestPutSample_AccountingAndResetBaseline(t *testing.T) {
	m := &RemoteSync{}

	if got := m.takePutPeak(); got != 0 {
		t.Fatalf("idle peak = %d, want 0", got)
	}

	for range 5 {
		m.notePutInFlight(1)
	}
	if got := m.takePutPeak(); got != 5 {
		t.Errorf("peak with 5 in flight = %d, want 5", got)
	}
	// All five are still uploading, so the next interval starts at five rather
	// than at zero — the baseline is the live count, not a clean slate.
	if got := m.takePutPeak(); got != 5 {
		t.Errorf("baseline peak = %d, want the 5 still in flight", got)
	}

	for range 5 {
		m.notePutInFlight(-1)
	}
	// The interval that just ended still had five in flight at its start.
	if got := m.takePutPeak(); got != 5 {
		t.Errorf("peak over the draining interval = %d, want 5", got)
	}
	if got := m.takePutPeak(); got != 0 {
		t.Errorf("peak after the drain = %d, want 0", got)
	}

	// An unbalanced release must not wrap the counter into a huge in-flight
	// count, which would pin windowLimited true forever.
	m.notePutInFlight(-1)
	if got := m.takePutPeak(); got != 0 {
		t.Errorf("peak after an unbalanced release = %d, want 0", got)
	}
	// The baseline matters more than that first read: a counter allowed to go
	// negative reports a negative in-flight count from here on, and a peak that
	// can never reach the limit pins windowLimited false for good.
	if got := m.takePutPeak(); got != 0 {
		t.Errorf("baseline after an unbalanced release = %d, want 0 (the count went negative)", got)
	}
}

// TestPutSample_ConcurrentBracketsDrainToZero hammers the bracket from many
// goroutines and pins that the paired counters stay consistent: every +1 is
// matched by its -1, so the sample drains to exactly zero. A lost or
// double-counted update inside the compare-and-swap leaves a non-zero residue
// that would bias windowLimited for the rest of the process's life.
func TestPutSample_ConcurrentBracketsDrainToZero(t *testing.T) {
	m := &RemoteSync{}

	const workers = 16
	const perWorker = 2000

	// A sampler racing the brackets, because the reset is the half that reads
	// and writes both fields in one step.
	stop := make(chan struct{})
	var sampler gosync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if got := m.takePutPeak(); got > workers {
					t.Errorf("sampled peak %d exceeds the %d goroutines that can be in flight", got, workers)
					return
				}
			}
		}
	}()

	var work gosync.WaitGroup
	for range workers {
		work.Add(1)
		go func() {
			defer work.Done()
			for range perWorker {
				m.notePutInFlight(1)
				m.notePutInFlight(-1)
			}
		}()
	}
	work.Wait()
	close(stop)
	sampler.Wait()

	// Drain the interval, then confirm nothing is left in flight.
	_ = m.takePutPeak()
	if got := m.takePutPeak(); got != 0 {
		t.Errorf("after %d balanced brackets the sample reads %d in flight, want 0", workers*perWorker, got)
	}
}
