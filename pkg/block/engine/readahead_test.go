package engine

import (
	"strconv"
	"testing"
)

func newRAsyncer(prefetch int) *Syncer {
	return &Syncer{
		config:    SyncerConfig{PrefetchBlocks: prefetch},
		readahead: make(map[string]*raState),
	}
}

func TestPlanReadahead_SequentialRamps(t *testing.T) {
	m := newRAsyncer(64)
	// First read has no history: depth 0 (Linux-style cold start).
	if d := m.planReadahead("p", 0, 0); d != 0 {
		t.Fatalf("first read: got depth %d, want 0", d)
	}
	// Contiguous reads ramp 1 -> 2 -> 4 -> ... capped at PrefetchBlocks.
	want := []int{1, 2, 4, 8, 16, 32, 64, 64}
	for i, exp := range want {
		start := uint64(i + 1)
		if d := m.planReadahead("p", start, start); d != exp {
			t.Fatalf("sequential read %d (block %d): got depth %d, want %d", i, start, d, exp)
		}
	}
}

func TestPlanReadahead_RandomResets(t *testing.T) {
	m := newRAsyncer(64)
	// Ramp up on a sequential run.
	for b := uint64(0); b <= 5; b++ {
		m.planReadahead("p", b, b)
	}
	if got := m.readahead["p"].depth; got == 0 {
		t.Fatalf("expected ramped depth after sequential run, got 0")
	}
	// A random jump backs off to 0 — no wasted prefetch of blocks a random
	// reader will not touch.
	if d := m.planReadahead("p", 1000, 1000); d != 0 {
		t.Fatalf("random jump: got depth %d, want 0", d)
	}
	// Re-establishing sequentiality from the new position ramps again.
	if d := m.planReadahead("p", 1001, 1001); d != 1 {
		t.Fatalf("resume after random: got depth %d, want 1", d)
	}
}

func TestPlanReadahead_RereadSameBlockIsSequential(t *testing.T) {
	m := newRAsyncer(64)
	m.planReadahead("p", 0, 0)                   // depth 0, frontier 0
	if d := m.planReadahead("p", 0, 0); d != 1 { // start==lastEnd tolerated as sequential
		t.Fatalf("re-read same block: got depth %d, want 1", d)
	}
}

func TestPlanReadahead_DisabledWhenZero(t *testing.T) {
	m := newRAsyncer(0)
	for b := uint64(0); b <= 5; b++ {
		if d := m.planReadahead("p", b, b); d != 0 {
			t.Fatalf("prefetch disabled: got depth %d, want 0", d)
		}
	}
}

func TestPlanReadahead_MapBounded(t *testing.T) {
	m := newRAsyncer(64)
	for i := 0; i < maxReadaheadEntries*2; i++ {
		m.planReadahead("payload-"+strconv.Itoa(i), 0, 0)
	}
	if got := len(m.readahead); got > maxReadaheadEntries {
		t.Fatalf("readahead map unbounded: %d entries, cap %d", got, maxReadaheadEntries)
	}
}
