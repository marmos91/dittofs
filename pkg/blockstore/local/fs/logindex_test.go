package fs

import (
	"reflect"
	"testing"
)

// TestLogIndex_Empty verifies the zero state: no entries, fence at the
// log header boundary, no lookup hits.
func TestLogIndex_Empty(t *testing.T) {
	idx := newLogIndex()
	if idx.Len() != 0 {
		t.Fatalf("Len: got %d want 0", idx.Len())
	}
	if idx.Fence() != logHeaderSize {
		t.Fatalf("Fence: got %d want %d", idx.Fence(), logHeaderSize)
	}
	if got := idx.EntriesForInterval(0, 4096); len(got) != 0 {
		t.Fatalf("EntriesForInterval on empty: got %v want []", got)
	}
}

// TestLogIndex_AppendAndLookup_InOrder seeds three records at adjacent
// file offsets and verifies a covering interval returns all three in
// logPos order.
func TestLogIndex_AppendAndLookup_InOrder(t *testing.T) {
	idx := newLogIndex()
	idx.Append(logHeaderSize, 0, 1024)
	idx.Append(logHeaderSize+1024+recordFrameOverhead, 1024, 1024)
	idx.Append(logHeaderSize+2*(1024+recordFrameOverhead), 2048, 1024)

	got := idx.EntriesForInterval(0, 3072)
	if len(got) != 3 {
		t.Fatalf("EntriesForInterval count: got %d want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].logPos <= got[i-1].logPos {
			t.Fatalf("entries not in logPos order: %+v", got)
		}
	}
}

// TestLogIndex_AppendAndLookup_OutOfOrderArrivals is the core regression
// case: macOS NFSv3 parallel writes deliver records in arrival order, not
// file-offset order. The lookup must still find every record covering the
// requested file interval, in arrival order.
func TestLogIndex_AppendAndLookup_OutOfOrderArrivals(t *testing.T) {
	idx := newLogIndex()
	// Arrival order: file offsets 32768, 458752, 0, 1540096
	// (matches the example transcript in the redesign proposal).
	type rec struct {
		fileOff uint64
		length  uint32
	}
	arrivals := []rec{
		{fileOff: 32768, length: 32784},
		{fileOff: 458752, length: 32784},
		{fileOff: 0, length: 32768},
		{fileOff: 1540096, length: 32784},
	}
	pos := uint64(logHeaderSize)
	for _, a := range arrivals {
		idx.Append(pos, a.fileOff, a.length)
		pos += uint64(recordFrameOverhead) + uint64(a.length)
	}

	// Query the [0, 65536) window — must surface the rec#2 arrival even
	// though it sits AFTER higher-offset records in the log.
	got := idx.EntriesForInterval(0, 65536)
	if len(got) != 2 {
		t.Fatalf("EntriesForInterval out-of-order: got %d entries want 2 (%+v)", len(got), got)
	}
	wantFileOffs := []uint64{32768, 0}
	for i, e := range got {
		if e.fileOff != wantFileOffs[i] {
			t.Fatalf("entry %d fileOff: got %d want %d (full result: %+v)", i, e.fileOff, wantFileOffs[i], got)
		}
	}
	// logPos must still be ascending — the arrival-order invariant
	// the rollup relies on for FastCDC stream reconstruction.
	if got[0].logPos >= got[1].logPos {
		t.Fatalf("logPos not ascending across out-of-order arrivals: %+v", got)
	}
}

// TestLogIndex_EntriesForInterval_Boundaries exercises the inclusive/
// exclusive edge cases of the file-offset overlap test.
func TestLogIndex_EntriesForInterval_Boundaries(t *testing.T) {
	idx := newLogIndex()
	// Three records, contiguous in file-offset space: [0, 100), [100, 200), [200, 300).
	idx.Append(logHeaderSize+0, 0, 100)
	idx.Append(logHeaderSize+1*(100+recordFrameOverhead), 100, 100)
	idx.Append(logHeaderSize+2*(100+recordFrameOverhead), 200, 100)

	cases := []struct {
		name        string
		off, length uint64
		want        []uint64 // expected fileOff values, in returned order
	}{
		{"zero length returns none", 50, 0, nil},
		{"no overlap below", 0, 0, nil}, // length 0 short-circuits
		{"left edge inclusive", 0, 1, []uint64{0}},
		{"right edge exclusive at 100", 99, 1, []uint64{0}},
		{"hits second only via boundary", 100, 1, []uint64{100}},
		{"spans first and second", 50, 100, []uint64{0, 100}},
		{"exact match second", 100, 100, []uint64{100}},
		{"covers all three", 0, 300, []uint64{0, 100, 200}},
		{"strictly past last", 300, 50, nil},
		{"touches start of last", 200, 1, []uint64{200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := idx.EntriesForInterval(tc.off, tc.length)
			var gotOffs []uint64
			for _, e := range got {
				gotOffs = append(gotOffs, e.fileOff)
			}
			if !reflect.DeepEqual(gotOffs, tc.want) {
				t.Fatalf("offsets: got %v want %v", gotOffs, tc.want)
			}
		})
	}
}

// TestLogIndex_AdvanceFence_Contiguous verifies that consuming records in
// arrival order advances the fence past each full record extent.
func TestLogIndex_AdvanceFence_Contiguous(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(256)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)
	positions := make([]uint64, 0, 3)
	for i := 0; i < 3; i++ {
		idx.Append(pos, uint64(i*4096), payload)
		positions = append(positions, pos)
		pos += step
	}

	// No consumption → fence stays at logHeaderSize.
	if got := idx.AdvanceFence(); got != logHeaderSize {
		t.Fatalf("fence with no consumption: got %d want %d", got, logHeaderSize)
	}

	// Consume the first entry. Fence advances to end of record 0.
	idx.MarkConsumed(positions[0])
	want := positions[0] + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after consume[0]: got %d want %d", got, want)
	}

	// Consume the second entry. Fence advances to end of record 1.
	idx.MarkConsumed(positions[1])
	want = positions[1] + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after consume[1]: got %d want %d", got, want)
	}

	// Consume the third entry. Fence advances to end of record 2 (== pos).
	idx.MarkConsumed(positions[2])
	want = positions[2] + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after consume[2]: got %d want %d", got, want)
	}
}

// TestLogIndex_AdvanceFence_HoleBlocks verifies the load-bearing
// invariant for R-7: a consumed entry preceded by an unconsumed
// predecessor must NOT advance the fence.
func TestLogIndex_AdvanceFence_HoleBlocks(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(256)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)
	positions := []uint64{}
	for i := 0; i < 3; i++ {
		idx.Append(pos, uint64(i*4096), payload)
		positions = append(positions, pos)
		pos += step
	}

	// Consume entries 1 and 2 (skipping 0). Fence MUST stay at the
	// header boundary because record 0 is still in-flight.
	idx.MarkConsumed(positions[1])
	idx.MarkConsumed(positions[2])
	if got := idx.AdvanceFence(); got != logHeaderSize {
		t.Fatalf("hole at head: fence got %d want %d", got, logHeaderSize)
	}

	// Now consume record 0. The fence walks through all three because
	// 1 and 2 are already marked.
	idx.MarkConsumed(positions[0])
	want := positions[2] + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after head-consume: got %d want %d", got, want)
	}
}

// TestLogIndex_MarkConsumed_Idempotent guards against repeated rollup
// calls for the same entry inflating any internal accounting. (None
// today, but cheap to enforce.)
func TestLogIndex_MarkConsumed_Idempotent(t *testing.T) {
	idx := newLogIndex()
	idx.Append(logHeaderSize, 0, 100)
	idx.MarkConsumed(logHeaderSize)
	idx.MarkConsumed(logHeaderSize)
	idx.MarkConsumed(logHeaderSize)
	want := uint64(logHeaderSize) + uint64(recordFrameOverhead) + 100
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence: got %d want %d", got, want)
	}
}

// TestLogIndex_AdvanceFence_NoConsumed_NoMove verifies AdvanceFence is
// idempotent when called repeatedly with no new consumption.
func TestLogIndex_AdvanceFence_NoConsumed_NoMove(t *testing.T) {
	idx := newLogIndex()
	idx.Append(logHeaderSize, 0, 100)
	for i := 0; i < 3; i++ {
		if got := idx.AdvanceFence(); got != logHeaderSize {
			t.Fatalf("AdvanceFence call %d: got %d want %d", i, got, logHeaderSize)
		}
	}
}

// TestLogIndex_AdvanceFence_CursorSkipsConsumed exercises the
// fenceCursor optimization: after a partial advance, repeated
// AdvanceFence calls must not rescan entries that were already
// fence-passed. We can't observe big-O directly, but we can prove
// correctness by appending more entries AFTER a partial advance and
// verifying the cursor picks them up without rewalking the prefix.
func TestLogIndex_AdvanceFence_CursorSkipsConsumed(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(64)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)
	for i := 0; i < 3; i++ {
		idx.Append(pos, uint64(i*4096), payload)
		idx.MarkConsumed(pos)
		pos += step
	}
	wantAfterThree := uint64(logHeaderSize) + 3*step
	if got := idx.AdvanceFence(); got != wantAfterThree {
		t.Fatalf("after 3 consumed: got %d want %d", got, wantAfterThree)
	}
	// Append two more entries, mark both consumed, AdvanceFence again.
	for i := 3; i < 5; i++ {
		idx.Append(pos, uint64(i*4096), payload)
		idx.MarkConsumed(pos)
		pos += step
	}
	wantAfterFive := uint64(logHeaderSize) + 5*step
	if got := idx.AdvanceFence(); got != wantAfterFive {
		t.Fatalf("after 5 consumed: got %d want %d", got, wantAfterFive)
	}
}

// TestLogIndex_AdvanceFence_TrimsConsumedPrefix verifies the R-2 (#581)
// invariant: after AdvanceFence walks past a prefix of consumed entries,
// those entries are dropped from idx.entries and their logPos keys are
// removed from idx.consumed. The fence value itself is preserved.
func TestLogIndex_AdvanceFence_TrimsConsumedPrefix(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(128)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)
	positions := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		idx.Append(pos, uint64(i*4096), payload)
		positions = append(positions, pos)
		pos += step
	}
	if idx.Len() != 4 {
		t.Fatalf("pre-advance Len: got %d want 4", idx.Len())
	}

	// Consume the first two entries. Fence walks past 0,1; trim drops
	// entries[0:2] and shifts entries[2:] to the head.
	idx.MarkConsumed(positions[0])
	idx.MarkConsumed(positions[1])
	wantFence := positions[1] + step
	if got := idx.AdvanceFence(); got != wantFence {
		t.Fatalf("fence after consume 0,1: got %d want %d", got, wantFence)
	}
	if got := idx.Len(); got != 2 {
		t.Fatalf("Len after trim: got %d want 2", got)
	}

	// Surviving entries must be the original positions[2] and positions[3]
	// — verifiable via EntriesForInterval over the file-offset window
	// they cover.
	hits := idx.EntriesForInterval(0, 4*4096)
	if len(hits) != 2 {
		t.Fatalf("EntriesForInterval after trim: got %d want 2", len(hits))
	}
	if hits[0].logPos != positions[2] || hits[1].logPos != positions[3] {
		t.Fatalf("entries after trim: got logPos %d,%d want %d,%d",
			hits[0].logPos, hits[1].logPos, positions[2], positions[3])
	}

	// Trim invariant: every surviving entry has logPos >= compactionFence.
	for _, e := range hits {
		if e.logPos < wantFence {
			t.Fatalf("trim invariant violated: entry logPos=%d < fence=%d", e.logPos, wantFence)
		}
	}

	// `consumed` map keys for the trimmed entries are gone. Re-consuming
	// the now-trailing entry (entries[1] in the trimmed slice) must work
	// from the new head without help from the stale keys.
	idx.MarkConsumed(positions[2])
	wantFence = positions[2] + step
	if got := idx.AdvanceFence(); got != wantFence {
		t.Fatalf("fence after consume positions[2]: got %d want %d", got, wantFence)
	}
	if got := idx.Len(); got != 1 {
		t.Fatalf("Len after second trim: got %d want 1", got)
	}
}

// TestLogIndex_AdvanceFence_TrimDoesNotCrossHole pins the load-bearing
// safety property: a "hole" entry (unconsumed) at the head of the slice
// blocks both the fence advance AND the trim, even when later entries
// are already consumed. Otherwise we'd lose the trailing consumed
// entries from the consumed map and AdvanceFence would never finish
// the walk.
func TestLogIndex_AdvanceFence_TrimDoesNotCrossHole(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(128)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)
	positions := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		idx.Append(pos, uint64(i*4096), payload)
		positions = append(positions, pos)
		pos += step
	}

	// Consume positions 1, 2, 3 but NOT 0. Fence cannot advance; trim
	// must not run.
	idx.MarkConsumed(positions[1])
	idx.MarkConsumed(positions[2])
	idx.MarkConsumed(positions[3])
	if got := idx.AdvanceFence(); got != logHeaderSize {
		t.Fatalf("fence with hole at head: got %d want %d", got, logHeaderSize)
	}
	if got := idx.Len(); got != 4 {
		t.Fatalf("Len with hole at head: got %d want 4 (trim must not run with unconsumed head)", got)
	}

	// Filling the hole should now cascade — all four entries advance and
	// then the entire slice is trimmed.
	idx.MarkConsumed(positions[0])
	wantFence := positions[3] + step
	if got := idx.AdvanceFence(); got != wantFence {
		t.Fatalf("fence after head-fill: got %d want %d", got, wantFence)
	}
	if got := idx.Len(); got != 0 {
		t.Fatalf("Len after full drain: got %d want 0", got)
	}
}

// TestLogIndex_AdvanceFence_TrimFullDrainReleasesBackingArray verifies
// the R-2 acceptance criterion: a payload that takes many AppendWrite +
// rollup cycles must not retain unbounded entries. Drives K cycles and
// asserts Len() stays at zero when the workload fully consumes each
// batch, instead of growing linearly with arrival count.
func TestLogIndex_AdvanceFence_TrimFullDrainReleasesBackingArray(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(64)
	const cycles = 1024 // enough that pre-trim behavior would clearly diverge
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)

	for c := 0; c < cycles; c++ {
		// One AppendWrite per cycle, fully consumed immediately.
		idx.Append(pos, uint64(c*4096), payload)
		idx.MarkConsumed(pos)
		idx.AdvanceFence()
		pos += step
		// After every cycle the slice should be empty — there are no
		// in-flight unconsumed records.
		if got := idx.Len(); got != 0 {
			t.Fatalf("cycle %d: Len got %d want 0 — trim is not running", c, got)
		}
	}
	// Fence must reflect the total bytes consumed across all cycles.
	wantFence := uint64(logHeaderSize) + uint64(cycles)*step
	if got := idx.Fence(); got != wantFence {
		t.Fatalf("post-drain fence: got %d want %d", got, wantFence)
	}
}

// TestLogIndex_AdvanceFence_TrimBoundedBySteadyState exercises the
// realistic shape: a payload with a small unconsumed working set but
// many lifetime arrivals. After K AppendWrite+rollup cycles, Len() must
// stay proportional to the working-set size, not to K.
func TestLogIndex_AdvanceFence_TrimBoundedBySteadyState(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(64)
	const cycles = 2048
	const inflightAhead = 4 // every batch leaves 4 newest entries unconsumed
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)

	// Pre-fill the in-flight window so each cycle below stays in steady
	// state.
	inflight := make([]uint64, 0, inflightAhead)
	for i := 0; i < inflightAhead; i++ {
		idx.Append(pos, uint64(i*4096), payload)
		inflight = append(inflight, pos)
		pos += step
	}

	for c := 0; c < cycles; c++ {
		// Add a new arrival.
		idx.Append(pos, uint64((inflightAhead+c)*4096), payload)
		// Consume the OLDEST in-flight entry.
		oldest := inflight[0]
		inflight = append(inflight[:0], inflight[1:]...)
		inflight = append(inflight, pos)
		idx.MarkConsumed(oldest)
		idx.AdvanceFence()
		pos += step

		// Len must stay bounded by the in-flight window — never grow
		// linearly with c. Allow a small slack (1) for the just-appended
		// entry; the working-set guard is 2*inflightAhead which is
		// generous but still O(1) in c.
		if got := idx.Len(); got > 2*inflightAhead {
			t.Fatalf("cycle %d: Len got %d, exceeds steady-state bound %d — trim is leaking",
				c, got, 2*inflightAhead)
		}
	}
}

// TestLogIndex_EntriesForInterval_OverflowGuard verifies that a query
// whose fileOff + length sum wraps past MaxUint64 still returns the
// expected records — the saturating end calculation prevents the
// overlap predicate from misclassifying high-offset entries.
func TestLogIndex_EntriesForInterval_OverflowGuard(t *testing.T) {
	idx := newLogIndex()
	// Three entries, the last one near (but not at) MaxUint64.
	idx.Append(logHeaderSize, 0, 100)
	idx.Append(logHeaderSize+100+recordFrameOverhead, 1024, 100)
	idx.Append(logHeaderSize+2*(100+recordFrameOverhead), ^uint64(0)-200, 100)

	// Query [^uint64(0)-300, ^uint64(0)-50): sum overflows but should
	// still surface the third record.
	hits := idx.EntriesForInterval(^uint64(0)-300, 250)
	if len(hits) != 1 || hits[0].fileOff != ^uint64(0)-200 {
		t.Fatalf("overflow query: got %+v want one entry at fileOff %d", hits, ^uint64(0)-200)
	}

	// Query at fileOff = ^uint64(0) - 50 with length = 100 (definite
	// overflow). End saturates to MaxUint64; no entries should match
	// since the high record's extent ends at ^uint64(0) - 100.
	hits = idx.EntriesForInterval(^uint64(0)-50, 100)
	if len(hits) != 0 {
		t.Fatalf("post-extent query: got %+v want []", hits)
	}
}
