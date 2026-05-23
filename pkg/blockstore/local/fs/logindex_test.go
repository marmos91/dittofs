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

// TestLogIndex_AdvanceFence_Contiguous verifies that marking each entry's
// file extent consumed advances the fence past each full record extent
// when entries are non-overlapping.
func TestLogIndex_AdvanceFence_Contiguous(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(256)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)
	type entExt struct {
		logPos  uint64
		fileOff uint64
	}
	ents := make([]entExt, 0, 3)
	for i := 0; i < 3; i++ {
		fileOff := uint64(i * 4096)
		idx.Append(pos, fileOff, payload)
		ents = append(ents, entExt{logPos: pos, fileOff: fileOff})
		pos += step
	}

	// No consumption → fence stays at logHeaderSize.
	if got := idx.AdvanceFence(); got != logHeaderSize {
		t.Fatalf("fence with no consumption: got %d want %d", got, logHeaderSize)
	}

	// Consume the first entry's file extent. Fence advances past record 0.
	idx.MarkConsumed(ents[0].fileOff, payload)
	want := ents[0].logPos + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after consume[0]: got %d want %d", got, want)
	}

	// Consume the second entry's file extent.
	idx.MarkConsumed(ents[1].fileOff, payload)
	want = ents[1].logPos + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after consume[1]: got %d want %d", got, want)
	}

	// Consume the third entry's file extent.
	idx.MarkConsumed(ents[2].fileOff, payload)
	want = ents[2].logPos + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after consume[2]: got %d want %d", got, want)
	}
}

// TestLogIndex_AdvanceFence_HoleBlocks verifies that a consumed entry
// preceded by a head entry whose file region is NOT yet covered must NOT
// advance the fence. With non-overlapping file regions (records at
// distinct extents), consuming the trailing entries' extents does not
// cover the head entry's extent, so the fence stays anchored at the head
// until the head's own extent is consumed.
func TestLogIndex_AdvanceFence_HoleBlocks(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(256)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)
	type entExt struct {
		logPos  uint64
		fileOff uint64
	}
	ents := []entExt{}
	for i := 0; i < 3; i++ {
		fileOff := uint64(i * 4096)
		idx.Append(pos, fileOff, payload)
		ents = append(ents, entExt{logPos: pos, fileOff: fileOff})
		pos += step
	}

	// Consume entries 1 and 2 (skipping head). Fence MUST stay at the
	// header boundary because record 0's file region [0, 256) is not
	// covered by either trailing record's extent.
	idx.MarkConsumed(ents[1].fileOff, payload)
	idx.MarkConsumed(ents[2].fileOff, payload)
	if got := idx.AdvanceFence(); got != logHeaderSize {
		t.Fatalf("hole at head: fence got %d want %d", got, logHeaderSize)
	}

	// Now consume record 0's extent. The fence walks through all three
	// because the head is now covered and the trailing extents were
	// already covered by their own MarkConsumed calls above.
	idx.MarkConsumed(ents[0].fileOff, payload)
	want := ents[2].logPos + step
	if got := idx.AdvanceFence(); got != want {
		t.Fatalf("fence after head-consume: got %d want %d", got, want)
	}
}

// TestLogIndex_MarkConsumed_Idempotent guards against repeated rollup
// calls for the same file extent inflating any internal accounting. The
// coverageSet add operation is set-union, so repeated identical calls
// are no-ops.
func TestLogIndex_MarkConsumed_Idempotent(t *testing.T) {
	idx := newLogIndex()
	idx.Append(logHeaderSize, 0, 100)
	idx.MarkConsumed(0, 100)
	idx.MarkConsumed(0, 100)
	idx.MarkConsumed(0, 100)
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
		fileOff := uint64(i * 4096)
		idx.Append(pos, fileOff, payload)
		idx.MarkConsumed(fileOff, payload)
		pos += step
	}
	wantAfterThree := uint64(logHeaderSize) + 3*step
	if got := idx.AdvanceFence(); got != wantAfterThree {
		t.Fatalf("after 3 consumed: got %d want %d", got, wantAfterThree)
	}
	// Append two more entries, mark both consumed, AdvanceFence again.
	for i := 3; i < 5; i++ {
		fileOff := uint64(i * 4096)
		idx.Append(pos, fileOff, payload)
		idx.MarkConsumed(fileOff, payload)
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

	// Consume the first two entries (by file extent — R-7 semantics).
	// Fence walks past 0,1; trim drops entries[0:2] and shifts
	// entries[2:] to the head.
	idx.MarkConsumed(0, payload)
	idx.MarkConsumed(4096, payload)
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

	// Re-consuming the now-trailing entry (entries[1] in the trimmed
	// slice) must work from the new head.
	idx.MarkConsumed(2*4096, payload)
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

	// Mark file regions 1,2,3 consumed but NOT region 0. Fence cannot
	// advance past entry 0; trim must not run.
	idx.MarkConsumed(4096, payload)
	idx.MarkConsumed(8192, payload)
	idx.MarkConsumed(12288, payload)
	if got := idx.AdvanceFence(); got != logHeaderSize {
		t.Fatalf("fence with hole at head: got %d want %d", got, logHeaderSize)
	}
	if got := idx.Len(); got != 4 {
		t.Fatalf("Len with hole at head: got %d want 4 (trim must not run with unconsumed head)", got)
	}

	// Marking region 0 consumed should cascade — all four entries advance
	// and the entire slice is trimmed.
	idx.MarkConsumed(0, payload)
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
// rollup cycles must not retain unbounded entries. Drives K cycles, then
// asserts that (a) Len()==0 after each fully-consumed cycle and (b) the
// backing array itself is released to the GC at the end of the run by
// inspecting `idx.entries == nil` directly (internal-package test).
func TestLogIndex_AdvanceFence_TrimFullDrainReleasesBackingArray(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(64)
	const cycles = 1024 // enough that pre-trim behavior would clearly diverge
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)

	for c := 0; c < cycles; c++ {
		// One AppendWrite per cycle, fully consumed immediately.
		fileOff := uint64(c * 4096)
		idx.Append(pos, fileOff, payload)
		idx.MarkConsumed(fileOff, payload)
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
	// Backing array MUST be released on full drain — internal access.
	idx.mu.Lock()
	entriesNil := idx.entries == nil
	idx.mu.Unlock()
	if !entriesNil {
		t.Fatalf("full drain did not release entries backing array (still non-nil)")
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
	// state. Track the file-offset of each in-flight entry — R-7
	// MarkConsumed is file-offset-keyed.
	inflight := make([]uint64, 0, inflightAhead)
	for i := 0; i < inflightAhead; i++ {
		fileOff := uint64(i * 4096)
		idx.Append(pos, fileOff, payload)
		inflight = append(inflight, fileOff)
		pos += step
	}

	for c := 0; c < cycles; c++ {
		// Add a new arrival.
		newFileOff := uint64((inflightAhead + c) * 4096)
		idx.Append(pos, newFileOff, payload)
		// Consume the OLDEST in-flight entry's file extent.
		oldest := inflight[0]
		inflight = append(inflight[1:], newFileOff)
		idx.MarkConsumed(oldest, payload)
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

// TestLogIndex_AdvanceFence_OverwriteUnsticksHead is the load-bearing
// R-7 (#580) acceptance: a head record whose file region is fully
// covered by a LATER consumed overwrite becomes dead immediately, even
// though the head record itself was never marked consumed. The fence
// walks straight through it. Pre-R-7 this would have stalled at the
// head record's frame.
func TestLogIndex_AdvanceFence_OverwriteUnsticksHead(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(4096)
	step := uint64(recordFrameOverhead) + uint64(payload)
	pos := uint64(logHeaderSize)

	// Head: write at [0, 4K). Logically alive until something covers it.
	headLogPos := pos
	idx.Append(headLogPos, 0, payload)
	pos += step
	// Overwrite at the same file extent [0, 4K).
	overwriteLogPos := pos
	idx.Append(overwriteLogPos, 0, payload)

	// Mark ONLY the overwrite's extent consumed. The head was never
	// processed (analog of: still in the dirty interval tree, awaiting
	// stabilization, or held by a long-lived stable interval pass).
	idx.MarkConsumed(0, payload)

	// Fence MUST advance past BOTH frames — the head's [0, 4K) is
	// covered by the overwrite's coverage even without the head being
	// individually marked.
	wantFence := overwriteLogPos + step
	if got := idx.AdvanceFence(); got != wantFence {
		t.Fatalf("R-7 overwrite-unsticks-head: fence got %d want %d", got, wantFence)
	}
}

// TestLogIndex_AdvanceFence_PartialOverlapBlocks asserts that a partial
// overlap does NOT release the head record's bytes. The fence may only
// walk past an entry whose FULL extent is covered — partial coverage
// keeps the entry alive (the uncovered tail bytes are still authoritative
// chunked-data input).
func TestLogIndex_AdvanceFence_PartialOverlapBlocks(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(4096)
	headLogPos := uint64(logHeaderSize)
	idx.Append(headLogPos, 0, payload) // head: [0, 4096)
	idx.Append(headLogPos+uint64(recordFrameOverhead)+uint64(payload), 2048, payload)

	// Mark consumed only [2048, 6144). Head's [0, 4096) is only partially
	// covered (the [2048, 4096) tail). Head must stay alive.
	idx.MarkConsumed(2048, payload)
	if got := idx.AdvanceFence(); got != uint64(logHeaderSize) {
		t.Fatalf("partial overlap should not unstick head: got %d want %d", got, logHeaderSize)
	}

	// Now cover [0, 2048) too. Head's extent is now fully covered.
	idx.MarkConsumed(0, 2048)
	wantFence := headLogPos + 2*(uint64(recordFrameOverhead)+uint64(payload))
	if got := idx.AdvanceFence(); got != wantFence {
		t.Fatalf("after full coverage: got %d want %d", got, wantFence)
	}
}

// TestLogIndex_AdvanceFence_CoverageMerge verifies that two MarkConsumed
// calls at adjacent file extents merge so a record straddling the
// boundary still reads as fully covered. This exercises coverageSet's
// adjacency-merge behavior: [0, 1024) + [1024, 2048) must cover an entry
// at [0, 2048).
func TestLogIndex_AdvanceFence_CoverageMerge(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(2048)
	headLogPos := uint64(logHeaderSize)
	idx.Append(headLogPos, 0, payload)

	idx.MarkConsumed(0, 1024)
	idx.MarkConsumed(1024, 1024)

	wantFence := headLogPos + uint64(recordFrameOverhead) + uint64(payload)
	if got := idx.AdvanceFence(); got != wantFence {
		t.Fatalf("adjacency merge failed: got %d want %d", got, wantFence)
	}
}

// TestCoverageSet exercises the coverage-set primitive in isolation —
// add merges adjacent and overlapping intervals; covers returns the
// strict containment predicate.
func TestCoverageSet(t *testing.T) {
	cases := []struct {
		name string
		ops  func(*coverageSet)
		// (query start, end) → expected covers result
		queries []struct {
			start, end uint64
			want       bool
		}
	}{
		{
			name: "single-interval",
			ops:  func(c *coverageSet) { c.add(10, 20) },
			queries: []struct {
				start, end uint64
				want       bool
			}{
				{10, 20, true},
				{11, 19, true},
				{10, 21, false},
				{9, 20, false},
				{20, 30, false},
				{0, 5, false},
			},
		},
		{
			name: "adjacency-merge",
			ops: func(c *coverageSet) {
				c.add(0, 10)
				c.add(10, 20)
				c.add(20, 30)
			},
			queries: []struct {
				start, end uint64
				want       bool
			}{
				{0, 30, true},
				{5, 25, true},
				{29, 31, false},
			},
		},
		{
			name: "overlap-merge",
			ops: func(c *coverageSet) {
				c.add(0, 10)
				c.add(5, 15)
				c.add(12, 25)
			},
			queries: []struct {
				start, end uint64
				want       bool
			}{
				{0, 25, true},
				{0, 26, false},
			},
		},
		{
			name: "gap-stays-gap",
			ops: func(c *coverageSet) {
				c.add(0, 10)
				c.add(20, 30)
			},
			queries: []struct {
				start, end uint64
				want       bool
			}{
				{0, 10, true},
				{20, 30, true},
				{0, 30, false},
				{9, 21, false},
			},
		},
		{
			name: "subsume-existing",
			ops: func(c *coverageSet) {
				c.add(10, 20)
				c.add(30, 40)
				c.add(50, 60)
				// One big add subsumes all three.
				c.add(0, 100)
			},
			queries: []struct {
				start, end uint64
				want       bool
			}{
				{0, 100, true},
				{50, 99, true},
			},
		},
		{
			name: "empty-query-vacuously-true",
			ops:  func(_ *coverageSet) {},
			queries: []struct {
				start, end uint64
				want       bool
			}{
				{0, 0, true},
				{100, 100, true},
			},
		},
		{
			name: "empty-add-ignored",
			ops: func(c *coverageSet) {
				c.add(5, 5)
				c.add(10, 5)
			},
			queries: []struct {
				start, end uint64
				want       bool
			}{
				{5, 6, false},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cs coverageSet
			tc.ops(&cs)
			for _, q := range tc.queries {
				if got := cs.covers(q.start, q.end); got != q.want {
					t.Errorf("covers(%d,%d): got %v want %v (intervals=%+v)",
						q.start, q.end, got, q.want, cs.intervals)
				}
			}
		})
	}
}

// TestCoverageSet_MergeKeepsSortedInvariant adds intervals in mixed
// order and asserts that the underlying slice stays sorted and
// non-overlapping — the precondition for covers's binary search.
func TestCoverageSet_MergeKeepsSortedInvariant(t *testing.T) {
	var cs coverageSet
	cs.add(100, 200)
	cs.add(0, 50)
	cs.add(300, 400)
	cs.add(150, 350) // bridges 100-200 and 300-400 via 150-350.
	cs.add(60, 90)
	cs.add(50, 60) // adjacency-bridge 0-50, 50-60, 60-90 → 0-90.

	for i := 1; i < len(cs.intervals); i++ {
		prev := cs.intervals[i-1]
		cur := cs.intervals[i]
		if prev.end > cur.start {
			t.Fatalf("invariant broken: %+v then %+v", prev, cur)
		}
		if prev.start >= prev.end {
			t.Fatalf("empty interval at %d: %+v", i-1, prev)
		}
	}
	if len(cs.intervals) != 2 {
		t.Fatalf("expected 2 merged intervals (0-90, 100-400), got %d: %+v", len(cs.intervals), cs.intervals)
	}
	if cs.intervals[0] != (coverageInterval{start: 0, end: 90}) {
		t.Errorf("intervals[0]: got %+v want {0,90}", cs.intervals[0])
	}
	if cs.intervals[1] != (coverageInterval{start: 100, end: 400}) {
		t.Errorf("intervals[1]: got %+v want {100,400}", cs.intervals[1])
	}
}
