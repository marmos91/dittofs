package journal

import (
	"context"
	"sync"
	"testing"
	"time"
)

// countingRecorder is the MetricsRecorder a test installs to see what the store
// emits. Guarded because eviction and the write path can both emit concurrently.
type countingRecorder struct {
	mu        sync.Mutex
	evictions int
	bytes     int64
	stalls    int
	waited    time.Duration
}

func (r *countingRecorder) RecordEviction(bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictions++
	r.bytes += bytes
}

func (r *countingRecorder) RecordBackpressure(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stalls++
	r.waited += d
}

func (r *countingRecorder) snapshot() (evictions int, bytes int64, stalls int, waited time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evictions, r.bytes, r.stalls, r.waited
}

// TestRepackRecordsNoEviction is the negative half of the eviction counter. A
// repack unlinks a segment through the same retirement tail eviction ends on, so
// an observation placed in that tail would count it — and then the eviction rate
// reads high on a store under no disk pressure at all, which is the same lie as
// a counter stuck at zero, only louder.
func TestRepackRecordsNoEviction(t *testing.T) {
	s := testStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	var rec countingRecorder
	s.SetMetrics(&rec)

	seedRepackable(t, s, true)
	res, err := s.gc(context.Background(), gcOptions{Force: true})
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if res.SegmentsRepacked != 1 {
		t.Fatalf("SegmentsRepacked = %d, want 1 (nothing was repacked, so the "+
			"assertion below would pass vacuously)", res.SegmentsRepacked)
	}
	if evictions, _, _, _ := rec.snapshot(); evictions != 0 {
		t.Fatalf("recorded %d evictions for a repack, want 0", evictions)
	}
}

// TestSetMetricsNilRecorderIsSafe covers both ways "no recorder" reaches the
// hot path: never installed (nil cell) and installed as nil. The production
// recorder is a *metrics.Metrics that may itself be nil inside a non-nil
// interface, so the call sites must tolerate a nil at either level.
func TestSetMetricsNilRecorderIsSafe(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	fillUntilSealed(t, s, "f", true, 1) // no recorder installed at all
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatalf("Evict with no recorder: %v", err)
	}
	s.SetMetrics(nil)
	fillUntilSealed(t, s, "f", true, 1)
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatalf("Evict with a nil recorder: %v", err)
	}
}
