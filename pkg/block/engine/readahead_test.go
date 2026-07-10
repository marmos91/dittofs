package engine

import (
	"strconv"
	"testing"
)

// newRAsyncer builds a Syncer exercising only planWindow (no queue/remote).
func newRAsyncer(prefetch int) *Syncer {
	return &Syncer{
		config:    SyncerConfig{PrefetchBlocks: prefetch},
		readahead: make(map[string]*raState),
	}
}

// window is a test helper: the block indices planWindow schedules for a read
// spanning [start, end], or nil when it fires nothing.
func (m *Syncer) window(payloadID string, start, end uint64) []uint64 {
	from, to, fire := m.planWindow(payloadID, start, end)
	if !fire {
		return nil
	}
	var out []uint64
	for b := from + 1; b <= to; b++ {
		out = append(out, b)
	}
	return out
}

func eq(t *testing.T, got, want []uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestPlanWindow_FirstReadEstablishesFrontier(t *testing.T) {
	m := newRAsyncer(4)
	// A single read is not a pattern — schedule nothing, just anchor the frontier.
	eq(t, m.window("p", 0, 0), nil)
}

func TestPlanWindow_SequentialSchedulesFullWindowThenSlides(t *testing.T) {
	m := newRAsyncer(4)
	m.window("p", 0, 0)                              // establish frontier at block 0
	eq(t, m.window("p", 0, 0), []uint64{1, 2, 3, 4}) // full window ahead
	eq(t, m.window("p", 1, 1), []uint64{5})          // frontier advanced → slide by one
	eq(t, m.window("p", 2, 2), []uint64{6})          // slides again
}

func TestPlanWindow_ScheduleOnce(t *testing.T) {
	m := newRAsyncer(4)
	m.window("p", 0, 0)
	m.window("p", 0, 0) // schedules 1..4
	// Re-reading the same block does not re-schedule already-queued blocks.
	eq(t, m.window("p", 0, 0), nil)
}

func TestPlanWindow_RandomResetsThenResumes(t *testing.T) {
	m := newRAsyncer(4)
	for b := uint64(0); b <= 3; b++ {
		m.window("p", b, b) // ramp up a sequential run
	}
	// A random jump prefetches nothing (a random reader won't touch the window).
	eq(t, m.window("p", 1000, 1000), nil)
	// Re-establishing sequentiality from the new position schedules a fresh
	// window anchored at the new frontier.
	eq(t, m.window("p", 1001, 1001), []uint64{1002, 1003, 1004, 1005})
}

func TestPlanWindow_DisabledWhenZero(t *testing.T) {
	m := newRAsyncer(0)
	for b := uint64(0); b <= 5; b++ {
		if got := m.window("p", b, b); got != nil {
			t.Fatalf("prefetch disabled: scheduled %v, want nothing", got)
		}
	}
}

func TestPlanWindow_MapBounded(t *testing.T) {
	m := newRAsyncer(4)
	for i := 0; i < maxReadaheadEntries*2; i++ {
		m.window("payload-"+strconv.Itoa(i), 0, 0)
	}
	if got := len(m.readahead); got > maxReadaheadEntries {
		t.Fatalf("readahead map unbounded: %d entries, cap %d", got, maxReadaheadEntries)
	}
}

// newSchedSyncer builds a Syncer with a real (unstarted) SyncQueue so
// scheduleReadahead enqueues into an inspectable prefetch channel.
func newSchedSyncer(prefetch int) *Syncer {
	m := &Syncer{
		config:    SyncerConfig{PrefetchBlocks: prefetch},
		readahead: make(map[string]*raState),
	}
	m.hasRemote.Store(true) // IsRemoteHealthy() is true with no health monitor
	m.queue = NewSyncQueue(m, SyncQueueConfig{QueueSize: 1000, DownloadWorkers: 4})
	return m
}

func drainPrefetch(q *SyncQueue) []uint64 {
	var out []uint64
	for {
		select {
		case req := <-q.prefetch:
			out = append(out, req.BlockIndex)
		default:
			return out
		}
	}
}

// TestScheduleReadahead_SlidesOnEveryRead is the core Step-1 guard: the window is
// computed purely from read offsets and slides forward on EVERY read — it never
// probes whether a block is local. This is exactly what the two prior refuted
// attempts lacked (they only advanced on a local MISS, so a sequential reader
// serving from the local tier stalled). Fails on develop, where readahead was
// driven from the on-miss path in EnsureAvailableAndRead.
//
// NOTE: a structural guard is necessary but NOT sufficient — the throughput win
// must be confirmed by a real-VM cold-read A/B (see the design doc's method).
func TestScheduleReadahead_SlidesOnEveryRead(t *testing.T) {
	m := newSchedSyncer(4)
	bs := uint64(BlockSize)

	// First read anchors the frontier only.
	m.scheduleReadahead("p", 0, 1)
	eq(t, drainPrefetch(m.queue), nil)

	// Second sequential read (still block 0) schedules the full window ahead.
	m.scheduleReadahead("p", 0, 1)
	eq(t, drainPrefetch(m.queue), []uint64{1, 2, 3, 4})

	// Advancing into block 1 slides the window by one — even though block 1
	// would be served locally; readahead never consults local state.
	m.scheduleReadahead("p", bs, 1)
	eq(t, drainPrefetch(m.queue), []uint64{5})

	// Re-reading within block 1 schedules nothing (schedule-once frontier).
	m.scheduleReadahead("p", bs, 1)
	eq(t, drainPrefetch(m.queue), nil)
}

func TestScheduleReadahead_NoRemoteIsNoOp(t *testing.T) {
	m := newSchedSyncer(4)
	m.hasRemote.Store(false)
	m.scheduleReadahead("p", 0, 1)
	m.scheduleReadahead("p", 0, 1)
	eq(t, drainPrefetch(m.queue), nil)
}
