package fs

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// fakeMetricsRecorder is a test double satisfying local.MetricsRecorder. It
// counts eviction/backpressure callbacks so the call-site wiring can be
// asserted without pulling in pkg/metrics (and its prometheus dependency).
type fakeMetricsRecorder struct {
	evictions    atomic.Int64
	evictedBytes atomic.Int64
	backpressure atomic.Int64
}

func (f *fakeMetricsRecorder) RecordBackpressure(time.Duration) { f.backpressure.Add(1) }
func (f *fakeMetricsRecorder) RecordEviction(bytes int64) {
	f.evictions.Add(1)
	f.evictedBytes.Add(bytes)
}

// TestEvictionMetrics_CounterMovesAfterForcedEviction asserts the eviction
// counter + reclaimed-bytes counter advance when ensureSpace evicts a chunk,
// and that the recorder is reached via SetMetrics.
func TestEvictionMetrics_CounterMovesAfterForcedEviction(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 600)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(block.RetentionLRU, 0)

	rec := &fakeMetricsRecorder{}
	bc.SetMetrics(rec)

	// Seed a 500B chunk, then force an eviction by needing more space.
	_ = storeChunk(t, bc, bytes.Repeat([]byte{0x42}, 500))
	if err := bc.ensureSpace(context.Background(), 200); err != nil {
		t.Fatalf("ensureSpace: %v", err)
	}

	if got := rec.evictions.Load(); got != 1 {
		t.Fatalf("expected 1 eviction recorded, got %d", got)
	}
	if got := rec.evictedBytes.Load(); got != 500 {
		t.Fatalf("expected 500 evicted bytes recorded, got %d", got)
	}
}

// TestEvictionMetrics_NilRecorderSafe asserts the hot path does not panic when
// no recorder has been wired (the startup-before-metrics window).
func TestEvictionMetrics_NilRecorderSafe(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 600)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(block.RetentionLRU, 0)

	_ = storeChunk(t, bc, bytes.Repeat([]byte{0x43}, 500))
	if err := bc.ensureSpace(context.Background(), 200); err != nil {
		t.Fatalf("ensureSpace: %v", err)
	}
}
