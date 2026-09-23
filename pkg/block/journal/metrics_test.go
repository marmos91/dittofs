package journal

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// countingRecorder counts what the store emits. Guarded: eviction and the append
// path can both emit concurrently.
type countingRecorder struct {
	mu        sync.Mutex
	evictions int
}

func (r *countingRecorder) RecordEviction(int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictions++
}

func (r *countingRecorder) RecordBackpressure(time.Duration) {}

func (r *countingRecorder) evictionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evictions
}

// TestRepackRecordsNoEviction is one negative half of the eviction counter. A
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
	if got := rec.evictionCount(); got != 0 {
		t.Fatalf("recorded %d evictions for a repack, want 0", got)
	}
}

// TestForceDrainRecordsNoEviction is the other negative half, and the one the
// counter's meaning rests on. Store.Evict is a second entry point into the same
// eviction loop the capacity gate uses, reached by the shares evict admin and the
// benchmark harness — on an idle store, on request. Counting it steps
// evictions_total by the whole sealed set while nothing was ever short of space,
// so a rate() alert on the counter fires on an operator command.
func TestForceDrainRecordsNoEviction(t *testing.T) {
	s, _ := evictStore(t, Config{}) // MaxLocalBytes unset: no pressure anywhere
	var rec countingRecorder
	s.SetMetrics(&rec)

	fillUntilSealed(t, s, "f", true, 2)
	res, err := s.Evict(context.Background(), 1<<62) // what DrainLocalSynced passes
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted == 0 {
		t.Fatal("the drain reclaimed nothing, so the assertion below would pass vacuously")
	}
	if got := rec.evictionCount(); got != 0 {
		t.Fatalf("recorded %d evictions for an operator drain of %d segments, want 0",
			got, res.SegmentsEvicted)
	}
}

// TestCancelledAppendRecordsNoStall pins the one way the gate can be entered
// without the appender waiting for anything: the context is already cancelled, so
// ensureSpace turns around immediately. Stamping the stall before that check
// records a ~0ns backpressure event for a wait that never happened, which is a
// step in the counter an operator cannot tell from a real one.
func TestCancelledAppendRecordsNoStall(t *testing.T) {
	s, _ := evictStore(t, Config{MaxLocalBytes: 1 << 20, EvictMaxWait: time.Second})
	var rec stallRecorder
	s.SetMetrics(&rec)

	// Put the store over its cap so the gate is genuinely met, then enter with a
	// context that is already done.
	s.diskBytes.Store(s.cfg.MaxLocalBytes + 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ensureSpace(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ensureSpace = %v, want context.Canceled (the gate was not entered, "+
			"so the assertion below would pass vacuously)", err)
	}
	if got := rec.stallCount(); got != 0 {
		t.Fatalf("recorded %d stalls for an append that waited for nothing, want 0", got)
	}
}

// stallRecorder counts only what the append path emits.
type stallRecorder struct {
	mu     sync.Mutex
	stalls int
}

func (r *stallRecorder) RecordEviction(int64) {}

func (r *stallRecorder) RecordBackpressure(time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stalls++
}

func (r *stallRecorder) stallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stalls
}

// nilHandleRecorder is the production nil shape: *metrics.Metrics is installed
// as a non-nil interface that may hold a nil pointer, and every method on it
// early-returns on the nil receiver.
type nilHandleRecorder struct{}

func (r *nilHandleRecorder) RecordEviction(int64)             {}
func (r *nilHandleRecorder) RecordBackpressure(time.Duration) {}

// TestNilRecorderIsSafe drives both recording paths under each of the three ways
// "no recorder" reaches them. The third is the one that would panic rather than
// no-op if a guard were dropped, and is exactly what a server started without
// metrics installs.
func TestNilRecorderIsSafe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		install func(*Store)
	}{
		{"never installed", func(*Store) {}},
		{"nil interface", func(s *Store) { s.SetMetrics(nil) }},
		{"nil handle in a non-nil interface", func(s *Store) {
			var handle *nilHandleRecorder
			s.SetMetrics(handle)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, _ := evictStore(t, Config{
				MaxLocalBytes: 2 << 20,
				EvictMaxWait:  200 * time.Millisecond,
			})
			tc.install(s)

			// Synced fill then a pressure eviction: reaches recordEviction.
			fillUntilSealed(t, s, "f", true, 1)
			buf := bytes.Repeat([]byte{0xAB}, chunk256)
			var off int64
			for range 16 {
				if err := s.Hydrate(ctx, "f2", off, buf, 0); err != nil {
					t.Fatalf("hydrate at %d: %v", off, err)
				}
				off += chunk256
			}
			if cold, _, err := s.ColdExtents(ctx); err != nil {
				t.Fatalf("ColdExtents: %v", err)
			} else if cold == 0 {
				t.Fatal("nothing was evicted under pressure, so recordEviction was never reached")
			}

			// Dirty fill until the cap is pinned: reaches recordBackpressure.
			full := false
			for i := range 32 {
				err := s.WriteAt(ctx, "f3", int64(i)*chunk256, buf)
				if errors.Is(err, ErrLocalStoreFull) {
					full = true
					break
				}
				if err != nil {
					t.Fatalf("write %d: %v", i, err)
				}
			}
			if !full {
				t.Fatal("never backpressured, so recordBackpressure was never reached")
			}
		})
	}
}
