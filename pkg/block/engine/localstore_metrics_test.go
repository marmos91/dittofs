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
func meteredEngine(t *testing.T, cfg journal.Config, rec journal.MetricsRecorder) *engine.Store {
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
	return bs
}

// TestLocalTierRecordsEviction pins the eviction counter end to end: a real
// journal store, the recorder installed through the engine seam, and an eviction
// the write path's capacity gate drove — the only reclaim the counter claims to
// describe. ColdExtents is the independent witness that bytes really were demoted,
// so the recorder assertion cannot pass by never evicting.
func TestLocalTierRecordsEviction(t *testing.T) {
	var rec localTierRecorder
	bs := meteredEngine(t, journal.Config{
		MaxLocalBytes: 2 << 20,
		EvictMaxWait:  2 * time.Second,
	}, &rec)
	ctx := context.Background()

	// Hydrate: the records are born remote-durable, so they satisfy the eviction
	// gate (a dirty record's local copy is its only copy and is never evicted).
	// 4 MiB into a 2 MiB cap makes the gate evict rather than merely check.
	buf := bytes.Repeat([]byte{0xAB}, 256<<10)
	for i := range 16 {
		if err := bs.Local().Hydrate(ctx, "f", int64(i)*int64(len(buf)), buf, 0); err != nil {
			t.Fatalf("hydrate %d: %v", i, err)
		}
	}

	cold, _, err := bs.Local().ColdExtents(ctx)
	if err != nil {
		t.Fatalf("ColdExtents: %v", err)
	}
	if cold == 0 {
		t.Fatal("no bytes were demoted, so nothing was evicted and the assertions below would pass vacuously")
	}

	evictions, freed := rec.evictionsSeen()
	if evictions == 0 {
		t.Fatalf("recorded 0 evictions, want 1 or more (%d bytes were demoted)", cold)
	}
	if freed == 0 {
		t.Fatalf("recorded %d evictions but 0 evicted bytes", evictions)
	}

	// The same gate event held this appender across the eviction and then cleared,
	// without ever reaching the dirty-pinned backoff. A stall measured from the
	// backoff rather than from the gate would see none of it — and this is the
	// expensive shape, since the eviction waits out the shard's carve pass.
	if stalls, waited := rec.stallsSeen(); stalls == 0 {
		t.Fatal("recorded 0 stalls for appends held across an eviction, want 1 or more")
	} else if waited <= 0 {
		t.Fatalf("recorded %d stalls totalling a %v wait, want a measured duration", stalls, waited)
	}
}

// TestLocalTierRecordsBackpressure pins the backpressure counter end to end, and
// with it the property that separates an honest counter from the always-zero one
// it replaces: the appends that found room must not appear in it. The capacity
// gate is consulted on every append, so counting checks rather than stalls would
// report constant backpressure on a store that never waited.
//
// The wait is the production backoff, not a sleep in the test: with every record
// dirty nothing is evictable, so the writer that meets the cap waits out
// EvictMaxWait and fails with ErrLocalStoreFull.
func TestLocalTierRecordsBackpressure(t *testing.T) {
	var rec localTierRecorder
	bs := meteredEngine(t, journal.Config{
		MaxLocalBytes: 2 << 20,
		EvictMaxWait:  500 * time.Millisecond,
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
		t.Fatalf("recorded a %v wait, want the duration actually spent held", waited)
	}
}
