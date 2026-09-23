package engine_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// localTierRecorder captures what the local tier emits through the recorder the
// engine forwards. It implements exactly journal.MetricsRecorder, which is what
// *metrics.Metrics reaches these paths as in production.
type localTierRecorder struct {
	mu        sync.Mutex
	evictions int
	bytes     int64
	stalls    int
	waited    time.Duration
}

func (r *localTierRecorder) RecordEviction(bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictions++
	r.bytes += bytes
}

func (r *localTierRecorder) RecordBackpressure(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stalls++
	r.waited += d
}

func (r *localTierRecorder) evictionsSeen() (int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evictions, r.bytes
}

func (r *localTierRecorder) stallsSeen() (int, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stalls, r.waited
}

// meteredEngine wires an engine over a REAL *journal.Store — the only local tier
// production runs — and installs rec the way the runtime does, through
// engine.Store.SetMetrics. Nothing here asserts the forwarding probe matched:
// the tests below observe what the journal emitted, so a probe that answers
// false (the state this seam shipped in) shows up as a missing observation
// rather than as a passing "recorder installed" check.
func meteredEngine(t *testing.T, cfg journal.Config, rec journal.MetricsRecorder) (*engine.Store, *journal.Store) {
	t.Helper()
	if cfg.SegmentSize == 0 {
		cfg.SegmentSize = 1 << 20
	}
	if cfg.ShardCount == 0 {
		cfg.ShardCount = 1
	}
	local, err := journal.Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          local,
		RemoteSync:     engine.NewRemoteSync(local, nil, ms, engine.DefaultConfig()),
		FileChunkStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	bs.SetMetrics(rec)
	return bs, local
}

// TestLocalTierRecordsEviction pins the eviction counter end to end: a real
// journal store, the recorder installed through the engine seam, and a
// disk-pressure eviction driven to completion. Removing the observation from the
// eviction path fails this on its assertion with "recorded 0 evictions, want 1".
func TestLocalTierRecordsEviction(t *testing.T) {
	var rec localTierRecorder
	bs, _ := meteredEngine(t, journal.Config{MaxLocalBytes: 64 << 20}, &rec)
	ctx := context.Background()

	// Hydrate: the records are born remote-durable, so they satisfy the eviction
	// gate (a dirty record's local copy is its only copy and is never evicted).
	// 1.5 MiB over a 1 MiB segment guarantees a sealed segment to reclaim.
	buf := bytes.Repeat([]byte{0xAB}, 256<<10)
	for i := range 6 {
		if err := bs.Local().Hydrate(ctx, "f", int64(i)*int64(len(buf)), buf, 0); err != nil {
			t.Fatalf("hydrate %d: %v", i, err)
		}
	}

	res, err := bs.Local().Evict(ctx, 0) // 0: reclaim exactly one segment
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 1 {
		t.Fatalf("SegmentsEvicted = %d, want 1 (nothing was reclaimed, so the "+
			"assertion below would pass vacuously)", res.SegmentsEvicted)
	}
	evictions, freed := rec.evictionsSeen()
	if evictions != 1 {
		t.Fatalf("recorded %d evictions, want 1", evictions)
	}
	if freed != res.BytesFreed {
		t.Fatalf("recorded %d evicted bytes, want %d", freed, res.BytesFreed)
	}
}

// TestLocalTierRecordsBackpressure pins the backpressure counter end to end, and
// with it the property that separates an honest counter from the always-zero one
// it replaces: the writes that found room must not appear in it. The capacity
// gate is consulted on every write, so counting checks rather than waits would
// report constant backpressure on a store that never stalled.
//
// The wait is the production backoff, not a sleep in the test: with every record
// dirty nothing is evictable, so the writer that meets the cap waits out
// EvictMaxWait and fails with ErrLocalStoreFull. Removing the observation fails
// this on its assertion with "recorded 0 write stalls, want 1".
func TestLocalTierRecordsBackpressure(t *testing.T) {
	var rec localTierRecorder
	bs, _ := meteredEngine(t, journal.Config{
		MaxLocalBytes: 2 << 20,
		EvictMaxWait:  50 * time.Millisecond,
	}, &rec)
	ctx := context.Background()

	// Plain writes stay dirty, so no segment is ever evictable and the cap is
	// reachable. Each write below the cap clears the gate without waiting.
	buf := bytes.Repeat([]byte{0xCD}, 256<<10)
	var full bool
	for i := range 32 {
		err := bs.Local().WriteAt(ctx, "f", int64(i)*int64(len(buf)), buf)
		if errors.Is(err, journal.ErrLocalStoreFull) {
			full = true
			break
		}
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if stalls, _ := rec.stallsSeen(); stalls != 0 {
			t.Fatalf("write %d cleared the capacity gate but recorded %d stalls, want 0", i, stalls)
		}
	}
	if !full {
		t.Fatal("never reached the local-store cap, so nothing ever backpressured")
	}

	stalls, waited := rec.stallsSeen()
	if stalls != 1 {
		t.Fatalf("recorded %d write stalls, want 1", stalls)
	}
	if waited <= 0 {
		t.Fatalf("recorded a %v wait, want the duration actually slept", waited)
	}
}
