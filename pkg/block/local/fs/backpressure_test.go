package fs

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeBackpressureSource is a controllable stand-in for the engine syncer's
// read-only backpressure accessor. It lets the fs-internal tests drive the
// ensureSpace remote-cache stall path deterministically — without an actual
// async upload loop — by toggling remote health and the unsynced-byte count.
type fakeBackpressureSource struct {
	healthy  atomic.Bool
	unsynced atomic.Int64
}

func newFakeBackpressureSource(healthy bool) *fakeBackpressureSource {
	f := &fakeBackpressureSource{}
	f.healthy.Store(healthy)
	return f
}

func (f *fakeBackpressureSource) IsRemoteHealthy() bool { return f.healthy.Load() }
func (f *fakeBackpressureSource) UnsyncedBytes() int64  { return f.unsynced.Load() }

// newBackpressureStore builds an FSStore with a small disk ceiling and a
// SyncedHashStore wired, so an unsynced-only LRU forces ensureSpace down the
// backpressure path.
func newBackpressureStore(t *testing.T, maxDisk int64) (*FSStore, *memory.MemoryMetadataStore) {
	t.Helper()
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(dir, maxDisk, 256*1024*1024, mds, FSStoreOptions{
		SyncedHashStore: mds,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(block.RetentionLRU, 0)
	t.Cleanup(func() { _ = bc.Close() })
	return bc, mds
}

// TestBackpressure_RemoteUnhealthy_FailsFast asserts that when every cached
// chunk is unsynced and the remote is UNHEALTHY (the syncer cannot drain),
// ensureSpace returns ErrDiskFull without waiting out the (long) backpressure
// window — it does not stall a writer that cannot make progress.
func TestBackpressure_RemoteUnhealthy_FailsFast(t *testing.T) {
	bc, _ := newBackpressureStore(t, 600)
	// Long backpressure window: the test must NOT wait it out. The
	// short-circuit on remote-unhealthy is what returns promptly.
	bc.backpressureMaxWait = 30 * time.Second
	bc.evictMaxWait = 30 * time.Second
	bc.SetBackpressureSource(newFakeBackpressureSource(false)) // remote unhealthy
	ctx := context.Background()

	storeChunk(t, bc, bytes.Repeat([]byte{0x01}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x02}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x03}, 200))

	start := time.Now()
	err := bc.ensureSpace(ctx, 200)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("ensureSpace with unhealthy remote: got %v, want ErrDiskFull", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ensureSpace stalled %v with unhealthy remote; expected fast fail (no backpressure wait)", elapsed)
	}
}

// TestBackpressure_HealthyRemote_ReleasesWhenDrained asserts the graceful
// path: with a HEALTHY remote and an unsynced-only LRU, ensureSpace stalls
// (engages backpressure) rather than failing, and RELEASES — succeeding —
// once the syncer drains (chunks marked synced + evicted) to free space.
func TestBackpressure_HealthyRemote_ReleasesWhenDrained(t *testing.T) {
	bc, mds := newBackpressureStore(t, 600)
	bc.backpressureMaxWait = 10 * time.Second // ample; the drain frees space well before
	src := newFakeBackpressureSource(true)    // remote healthy
	bc.SetBackpressureSource(src)
	ctx := context.Background()

	hA := storeChunk(t, bc, bytes.Repeat([]byte{0x01}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x02}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x03}, 200))
	src.unsynced.Store(600)

	// Simulate the syncer draining one chunk shortly after the writer stalls:
	// mark hA synced so it becomes evictable, freeing 200 bytes.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = mds.MarkSynced(ctx, hA)
		src.unsynced.Add(-200)
	}()

	start := time.Now()
	if err := bc.ensureSpace(ctx, 200); err != nil {
		t.Fatalf("ensureSpace with healthy remote that drains: got %v, want nil (backpressure released)", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("ensureSpace returned in %v; expected it to stall until the drain freed space", elapsed)
	}

	// Backpressure must have engaged exactly once and accounted some stall.
	engage, stall := bc.BackpressureStats()
	if engage != 1 {
		t.Errorf("engage count = %d, want 1", engage)
	}
	if stall <= 0 {
		t.Errorf("total stall = %v, want > 0", stall)
	}
}

// TestBackpressure_HealthyRemote_WindowExceeded asserts that a healthy remote
// that never drains eventually trips the backpressure window and returns
// ErrDiskFull — the bounded stall, not an indefinite hang.
func TestBackpressure_HealthyRemote_WindowExceeded(t *testing.T) {
	bc, _ := newBackpressureStore(t, 600)
	bc.backpressureMaxWait = 200 * time.Millisecond // short window for the test
	src := newFakeBackpressureSource(true)          // healthy but never drains
	src.unsynced.Store(600)
	bc.SetBackpressureSource(src)
	ctx := context.Background()

	storeChunk(t, bc, bytes.Repeat([]byte{0x01}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x02}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x03}, 200))

	start := time.Now()
	err := bc.ensureSpace(ctx, 200)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("ensureSpace over a never-draining healthy remote: got %v, want ErrDiskFull", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("ensureSpace returned in %v; expected it to honor the %v backpressure window", elapsed, bc.backpressureMaxWait)
	}
	if engage, _ := bc.BackpressureStats(); engage != 1 {
		t.Errorf("engage count = %d, want 1", engage)
	}
}

// TestBackpressure_NoSource_LocalOnlyUnaffected asserts that with no
// backpressure source wired (local-only share: no remote), ensureSpace falls
// back to the short evictMaxWait deadline and does NOT engage the remote-cache
// backpressure window. This guards the invariant that local-only writes never
// incur the remote stall.
func TestBackpressure_NoSource_LocalOnlyUnaffected(t *testing.T) {
	bc, _ := newBackpressureStore(t, 600)
	bc.evictMaxWait = 100 * time.Millisecond
	bc.backpressureMaxWait = 30 * time.Second // must NOT be used (no source)
	// No SetBackpressureSource — bpSource is nil (local-only).
	ctx := context.Background()

	storeChunk(t, bc, bytes.Repeat([]byte{0x01}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x02}, 200))
	storeChunk(t, bc, bytes.Repeat([]byte{0x03}, 200))

	start := time.Now()
	err := bc.ensureSpace(ctx, 200)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("local-only ensureSpace over unsynced LRU: got %v, want ErrDiskFull", err)
	}
	// Should honor the short evictMaxWait, never the long backpressure window.
	if elapsed > 5*time.Second {
		t.Fatalf("local-only ensureSpace stalled %v; expected fast fail on evictMaxWait", elapsed)
	}
	if engage, _ := bc.BackpressureStats(); engage != 0 {
		t.Errorf("engage count = %d, want 0 (no backpressure for local-only)", engage)
	}
}
