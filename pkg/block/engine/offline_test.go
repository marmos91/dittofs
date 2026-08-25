package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// fakeColdReporter stands in for the local tier's residency surface so the
// gating rules can be checked without building a journal.
type fakeColdReporter struct {
	seeded  bool
	bytes   int64
	extents int64
	err     error
}

func (f *fakeColdReporter) ColdExtents(ctx context.Context) (int64, int64, error) {
	if f.err != nil {
		return 0, 0, f.err
	}
	return f.bytes, f.extents, ctx.Err()
}
func (f *fakeColdReporter) ColdSeeded() bool { return f.seeded }

func (f *fakeColdReporter) DataExtents(context.Context, string, int64) ([][2]uint64, error) {
	return nil, nil
}

// noShortfall stands in for a manifest that accounts for every range the index
// describes, so the gating rules below are exercised on their own.
func noShortfall(context.Context, coldRangeReporter) (int64, int64, error) { return 0, 0, nil }

// TestOfflineReadiness_Safe pins the two ways a readiness answer can be wrong
// in the dangerous direction: reporting zero remote-only bytes for a share
// whose residency is not actually known reads as "provably offline safe" for
// exactly the shares most likely not to be.
func TestOfflineReadiness_Safe(t *testing.T) {
	tests := []struct {
		name     string
		r        OfflineReadiness
		wantSafe bool
	}{
		{"known and fully local", OfflineReadiness{Known: true}, true},
		{"known with remote-only bytes", OfflineReadiness{Known: true, RemoteOnlyBytes: 1}, false},
		{"unknown reads as unsafe", OfflineReadiness{Reason: "local tier has not been seeded from the manifest"}, false},
		{"unknown is never safe even at zero", OfflineReadiness{Reason: "block store is closed", RemoteOnlyBytes: 0}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Safe(); got != tc.wantSafe {
				t.Errorf("Safe() = %v, want %v", got, tc.wantSafe)
			}
		})
	}
}

// TestOfflineReadinessOf_Gating pins when the measurement refuses to answer.
// Each refusal replaces a zero that would have read as "provably offline
// safe", which is the wrong answer in the dangerous direction.
func TestOfflineReadinessOf_Gating(t *testing.T) {
	tests := []struct {
		name      string
		localTier any
		hasRemote bool
		shortfall shortfallFunc
		wantKnown bool
		wantBytes int64
	}{
		{"no remote, tier confirms nothing evicted", &fakeColdReporter{}, false, noShortfall, true, 0},
		{"tier that cannot report residency", struct{}{}, true, noShortfall, false, 0},
		{"no remote and no residency tracking", struct{}{}, false, noShortfall, true, 0},
		{"unseeded tier cannot see remote-only ranges", &fakeColdReporter{seeded: false, bytes: 0}, true, noShortfall, false, 0},
		{"seeded and fully local", &fakeColdReporter{seeded: true}, true, noShortfall, true, 0},
		{"seeded with evicted ranges", &fakeColdReporter{seeded: true, bytes: 4096, extents: 2}, true, noShortfall, true, 4096},
		// Unbinding a remote from a share that had already evicted leaves the
		// cold intervals behind, and the journal replays them from its cold log
		// on the next open. Reads reconcile on the cold flag whether or not a
		// remote exists, so those ranges never serve — reporting this share
		// safe would be the worst answer the measurement can give.
		{"no remote but cold ranges remain", &fakeColdReporter{seeded: false, bytes: 8192, extents: 3}, false, noShortfall, true, 8192},
		// A range the manifest places that no interval describes was lost, not
		// evicted: it adds nothing to the cold tally, so without this the
		// answer below would be a confident zero.
		{"manifest places bytes the index has forgotten", &fakeColdReporter{seeded: true}, true,
			func(context.Context, coldRangeReporter) (int64, int64, error) { return 4096, 1, nil }, false, 0},
		// A share with no manifest reaches this the same way: the store's
		// cross-check reports errNoManifest rather than a zero shortfall.
		{"cross-check could not run", &fakeColdReporter{seeded: true}, true,
			func(context.Context, coldRangeReporter) (int64, int64, error) { return 0, 0, errNoManifest }, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := offlineReadinessOf(context.Background(), tc.localTier, tc.hasRemote, tc.shortfall)
			if got.Known != tc.wantKnown {
				t.Errorf("Known = %v, want %v (reason %q)", got.Known, tc.wantKnown, got.Reason)
			}
			if got.RemoteOnlyBytes != tc.wantBytes {
				t.Errorf("RemoteOnlyBytes = %d, want %d", got.RemoteOnlyBytes, tc.wantBytes)
			}
			if !got.Known && got.Reason == "" {
				t.Error("refused to answer without saying why")
			}
			if got.Known && got.Reason != "" {
				t.Errorf("answered but still gave a reason: %q", got.Reason)
			}
			if got.Safe() && got.RemoteOnlyBytes != 0 {
				t.Errorf("reported safe with %d remote-only bytes", got.RemoteOnlyBytes)
			}
		})
	}
}

// TestOfflineReadinessOf_CancelledScanIsUnknown asserts a walk that ran out of
// time reports unknown rather than the partial total it had reached. A status
// request and a metrics scrape both carry a deadline, and a truncated count
// would read as a share holding less remote-only data than it does.
func TestOfflineReadinessOf_CancelledScanIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := offlineReadinessOf(ctx, &fakeColdReporter{seeded: true, bytes: 4096, extents: 2}, true, noShortfall)
	if got.Known {
		t.Error("a cancelled scan reported a known answer")
	}
	if got.Safe() {
		t.Error("a cancelled scan reported the share offline-safe")
	}
	if got.RemoteOnlyBytes != 0 {
		t.Errorf("RemoteOnlyBytes = %d after cancellation, want 0 (no partial totals)", got.RemoteOnlyBytes)
	}
	if got.Reason == "" {
		t.Error("refused to answer without saying why")
	}

	// A scan that fails for its own reasons is the same kind of non-answer.
	failed := offlineReadinessOf(context.Background(),
		&fakeColdReporter{seeded: true, err: errors.New("store is closed")}, true, noShortfall)
	if failed.Known || failed.Safe() {
		t.Errorf("a failed scan reported known=%v safe=%v", failed.Known, failed.Safe())
	}
}

// stubIndex reports a canned set of described ranges per file, standing in for
// the local tier's interval index.
type stubIndex struct {
	described map[string][][2]uint64
	seeded    bool
}

func (s *stubIndex) ColdExtents(context.Context) (int64, int64, error) { return 0, 0, nil }
func (s *stubIndex) ColdSeeded() bool                                  { return s.seeded }
func (s *stubIndex) DataExtents(_ context.Context, id string, size int64) ([][2]uint64, error) {
	var out [][2]uint64
	for _, e := range s.described[id] {
		if e[0] >= uint64(size) {
			continue
		}
		if e[1] > uint64(size) {
			e[1] = uint64(size)
		}
		out = append(out, e)
	}
	return out, nil
}

// chunkRow builds one manifest row placing [off, off+size). A non-zero hash is
// what makes a row committed; zeroHash rows hold no bytes yet.
func chunkRow(payloadID string, off uint64, size uint32, committed bool) *block.FileChunk {
	row := &block.FileChunk{ID: fmt.Sprintf("%s/%d", payloadID, off), DataSize: size}
	if committed {
		row.Hash = block.ContentHash{1}
	}
	return row
}

// stubManifest is a fixed set of manifest rows per payload.
type stubManifest map[string][]*block.FileChunk

func (m stubManifest) EnumeratePayloads(_ context.Context, fn func(string) error) error {
	for id := range m {
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

func (m stubManifest) ListFileChunks(_ context.Context, id string) ([]*block.FileChunk, error) {
	return m[id], nil
}

// TestManifestShortfall pins what the cross-check counts as a range the index
// has forgotten. The dangerous direction is under-reporting — a shortfall read
// as zero is what lets a lost range pass as offline-safe — but a check that
// fires on a healthy share is just as useless, so both are asserted.
func TestManifestShortfall(t *testing.T) {
	tests := []struct {
		name       string
		rows       []*block.FileChunk
		described  [][2]uint64
		wantBytes  int64
		wantRanges int64
	}{
		{
			name:      "index describes everything the manifest places",
			rows:      []*block.FileChunk{chunkRow("p", 0, 1024, true), chunkRow("p", 1024, 1024, true)},
			described: [][2]uint64{{0, 2048}},
		},
		{
			name:       "index describes nothing at all",
			rows:       []*block.FileChunk{chunkRow("p", 0, 1024, true)},
			wantBytes:  1024,
			wantRanges: 1,
		},
		{
			name:       "a hole in the middle of what the index describes",
			rows:       []*block.FileChunk{chunkRow("p", 0, 3072, true)},
			described:  [][2]uint64{{0, 1024}, {2048, 3072}},
			wantBytes:  1024,
			wantRanges: 1,
		},
		{
			name:       "holes at both ends",
			rows:       []*block.FileChunk{chunkRow("p", 0, 4096, true)},
			described:  [][2]uint64{{1024, 3072}},
			wantBytes:  2048,
			wantRanges: 2,
		},
		{
			name:      "overlapping rows are one span, not two claims",
			rows:      []*block.FileChunk{chunkRow("p", 0, 2048, true), chunkRow("p", 1024, 1024, true)},
			described: [][2]uint64{{0, 2048}},
		},
		{
			// A row reaching past a later row's whole span is a legitimate
			// manifest shape, not damage: the straddler starts first, so the
			// later row outranks it where they meet, and the straddler is what
			// keeps the bytes on either side of that row readable. Coverage is the
			// union of what rows claim, so the straddler's tail counts as placed
			// and the byte the later row also covers is not counted twice.
			name:      "a straddling row keeps its tail",
			rows:      []*block.FileChunk{chunkRow("p", 0, 3072, true), chunkRow("p", 1024, 1024, true)},
			described: [][2]uint64{{0, 3072}},
		},
		{
			name:       "a straddling row's tail is missed like any other range",
			rows:       []*block.FileChunk{chunkRow("p", 0, 3072, true), chunkRow("p", 1024, 1024, true)},
			described:  [][2]uint64{{0, 2048}},
			wantBytes:  1024,
			wantRanges: 1,
		},
		{
			// Rows arrive in whatever order the backend returns them, and an
			// out-of-order pair must union the same way a sorted one does.
			name:      "rows out of offset order still union",
			rows:      []*block.FileChunk{chunkRow("p", 2048, 1024, true), chunkRow("p", 0, 2048, true)},
			described: [][2]uint64{{0, 3072}},
		},
		{
			// A row with no committed bytes has nothing on the remote to place,
			// so the index having no interval for it is not a loss.
			name:      "a pending row places nothing",
			rows:      []*block.FileChunk{chunkRow("p", 0, 1024, false)},
			described: nil,
		},
		{
			// Where an unplaceable row's bytes belong is unknowable here, so it
			// is CheckManifests' finding, not this one's.
			name:      "an unplaceable row places nothing",
			rows:      []*block.FileChunk{{ID: "p/not-an-offset", DataSize: 1024, Hash: block.ContentHash{1}}},
			described: nil,
		},
		{
			// A parsed offset is bounded to int64, so a row near the top of the
			// range can name an end the index has no way to express: it reports
			// its side over an int64 span. Counting that unreachable tail would
			// be a shortfall invented by the arithmetic rather than found in the
			// data — the one thing this check must never do.
			name: "a row reaching past int64 is clamped, not counted",
			rows: []*block.FileChunk{
				chunkRow("p", math.MaxInt64-1024, 4096, true),
			},
			described: [][2]uint64{{math.MaxInt64 - 1024, math.MaxInt64}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index := &stubIndex{described: map[string][][2]uint64{"p": tc.described}, seeded: true}
			bytes, ranges, err := manifestShortfall(context.Background(), index, stubManifest{"p": tc.rows})
			if err != nil {
				t.Fatalf("manifestShortfall: %v", err)
			}
			if bytes != tc.wantBytes || ranges != tc.wantRanges {
				t.Errorf("shortfall = %d bytes in %d ranges, want %d in %d",
					bytes, ranges, tc.wantBytes, tc.wantRanges)
			}
		})
	}
}

// TestSubtractExtents covers the arithmetic the shortfall is measured with,
// including the case that matters most: an empty b, where every byte of a is
// missing rather than none of it.
func TestSubtractExtents(t *testing.T) {
	tests := []struct {
		name string
		a, b [][2]uint64
		want [][2]uint64
	}{
		{name: "nothing covers anything", a: [][2]uint64{{0, 10}}, want: [][2]uint64{{0, 10}}},
		{name: "fully covered", a: [][2]uint64{{0, 10}}, b: [][2]uint64{{0, 10}}},
		{name: "covered by a wider span", a: [][2]uint64{{2, 8}}, b: [][2]uint64{{0, 10}}},
		{name: "gap in the middle", a: [][2]uint64{{0, 10}}, b: [][2]uint64{{0, 3}, {7, 10}}, want: [][2]uint64{{3, 7}}},
		{name: "b entirely before a", a: [][2]uint64{{10, 20}}, b: [][2]uint64{{0, 5}}, want: [][2]uint64{{10, 20}}},
		{name: "b entirely after a", a: [][2]uint64{{0, 5}}, b: [][2]uint64{{10, 20}}, want: [][2]uint64{{0, 5}}},
		{
			name: "several spans share one covering list",
			a:    [][2]uint64{{0, 10}, {20, 30}},
			b:    [][2]uint64{{5, 25}},
			want: [][2]uint64{{0, 5}, {25, 30}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := subtractExtents(tc.a, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("subtractExtents = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("subtractExtents = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestShortfallMemoReusesTheWalk asserts the metrics scrape path does not pay
// for a manifest walk on every call, and that a walk which failed is not
// remembered as a verdict.
func TestShortfallMemoReusesTheWalk(t *testing.T) {
	var walks int
	manifest := &countingManifest{stubManifest{"p": {chunkRow("p", 0, 1024, true)}}, &walks}
	index := &stubIndex{described: map[string][][2]uint64{"p": {{0, 1024}}}, seeded: true}

	var memo shortfallMemo
	for i := 0; i < 3; i++ {
		if _, _, err := memo.get(context.Background(), index, manifest); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	if walks != 1 {
		t.Errorf("walked the manifest %d times over 3 calls, want 1", walks)
	}

	// A cancelled walk is a non-answer, so the next caller must get its own
	// attempt rather than inherit this one's failure.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var fresh shortfallMemo
	if _, _, err := fresh.get(ctx, index, manifest); err == nil {
		t.Fatal("a cancelled walk reported a verdict")
	}
	if !fresh.at.IsZero() {
		t.Error("a failed walk was remembered as a verdict")
	}
}

type countingManifest struct {
	stubManifest
	walks *int
}

func (c *countingManifest) EnumeratePayloads(ctx context.Context, fn func(string) error) error {
	*c.walks++
	return c.stubManifest.EnumeratePayloads(ctx, fn)
}

// vanishingManifest serves one set of rows to the first read of a payload and
// another to every read after it, standing in for a delete or truncate that
// lands between the walk's two looks at the same file.
type vanishingManifest struct {
	first, then []*block.FileChunk
	reads       int
}

func (m *vanishingManifest) EnumeratePayloads(_ context.Context, fn func(string) error) error {
	return fn("p")
}

func (m *vanishingManifest) ListFileChunks(context.Context, string) ([]*block.FileChunk, error) {
	m.reads++
	if m.reads == 1 {
		return m.first, nil
	}
	return m.then, nil
}

// TestManifestShortfallConfirmsBeforeReporting pins the re-read. The index and
// the manifest narrow in opposite orders and neither operation is atomic with
// the other, so a walk that looked once would call a file being deleted a lost
// range and hold that verdict for the life of the memo.
//
// The second case is the half that makes the first mean something: a manifest
// that still places the bytes on the re-read is a real shortfall and must
// survive the confirmation.
func TestManifestShortfallConfirmsBeforeReporting(t *testing.T) {
	wide := []*block.FileChunk{chunkRow("p", 0, 4096, true)}
	// The index describes nothing, which is what a payload whose local entry
	// has already been emptied looks like.
	index := &stubIndex{described: map[string][][2]uint64{}, seeded: true}

	tests := []struct {
		name       string
		manifest   *vanishingManifest
		wantBytes  int64
		wantRanges int64
		wantReads  int
	}{
		{
			name:      "rows reaped between the two reads",
			manifest:  &vanishingManifest{first: wide},
			wantReads: 2,
		},
		{
			name:       "rows still place the bytes on the re-read",
			manifest:   &vanishingManifest{first: wide, then: wide},
			wantBytes:  4096,
			wantRanges: 1,
			wantReads:  2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bytes, ranges, err := manifestShortfall(context.Background(), index, tc.manifest)
			if err != nil {
				t.Fatalf("manifestShortfall: %v", err)
			}
			if bytes != tc.wantBytes || ranges != tc.wantRanges {
				t.Errorf("shortfall = %d bytes in %d ranges, want %d in %d",
					bytes, ranges, tc.wantBytes, tc.wantRanges)
			}
			if tc.manifest.reads != tc.wantReads {
				t.Errorf("read the manifest %d times, want %d", tc.manifest.reads, tc.wantReads)
			}
		})
	}

	// A payload that never looked short is not re-read at all.
	covered := &vanishingManifest{first: wide, then: wide}
	full := &stubIndex{described: map[string][][2]uint64{"p": {{0, 4096}}}, seeded: true}
	if _, _, err := manifestShortfall(context.Background(), full, covered); err != nil {
		t.Fatalf("manifestShortfall: %v", err)
	}
	if covered.reads != 1 {
		t.Errorf("read the manifest %d times for a fully described payload, want 1", covered.reads)
	}
}

// TestIntersectExtents covers the arithmetic the confirmation narrows with.
func TestIntersectExtents(t *testing.T) {
	tests := []struct {
		name string
		a, b [][2]uint64
		want [][2]uint64
	}{
		{name: "one side empty", a: [][2]uint64{{0, 10}}},
		{name: "identical", a: [][2]uint64{{0, 10}}, b: [][2]uint64{{0, 10}}, want: [][2]uint64{{0, 10}}},
		{name: "partial overlap", a: [][2]uint64{{0, 10}}, b: [][2]uint64{{5, 20}}, want: [][2]uint64{{5, 10}}},
		{name: "disjoint", a: [][2]uint64{{0, 5}}, b: [][2]uint64{{5, 10}}},
		{
			name: "one span against several",
			a:    [][2]uint64{{0, 30}},
			b:    [][2]uint64{{2, 4}, {10, 12}, {40, 50}},
			want: [][2]uint64{{2, 4}, {10, 12}},
		},
		{
			name: "several against several",
			a:    [][2]uint64{{0, 10}, {20, 30}},
			b:    [][2]uint64{{5, 25}},
			want: [][2]uint64{{5, 10}, {20, 25}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := intersectExtents(tc.a, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("intersectExtents = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("intersectExtents = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
