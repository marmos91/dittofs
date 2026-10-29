//go:build !race

package index

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestRebuildCost is the rebuild's sanity cost check, beside the package's
// other cost tests: a hundred thousand records over a thousand files
// rebuild, and the rebuilt index's entry count and per-segment bytes match
// an incremental build of the same history. A rebuild that hit a
// super-linear spot — a whole-file walk per record — disagrees with the
// reference or runs past the sanity bound.
func TestRebuildCost(t *testing.T) {
	if testing.Short() {
		t.Skip("rebuilds 10^5 records over 1000 files")
	}
	// The history: a thousand files, plain interleaved writes of 4 KiB at
	// 4 KiB granularity, a hundred records each.
	const fileCount, perFile = 1000, 100
	ids := make([]core.FileID, fileCount)
	for i := range ids {
		ids[i] = core.NewFileID([16]byte{0: byte(i), 1: byte(i >> 8), 15: 0xff})
	}
	var ctr Counters
	_ = ctr.SetVersionLimit(ver(math.MaxUint64 - 1))
	recs := make([]Record, 0, fileCount*perFile)
	for f := range fileCount {
		id := ids[f]
		for i := range perFile {
			v, err := ctr.Draw()
			if err != nil {
				t.Fatal(err)
			}
			seq, err := ctr.NextSeq()
			if err != nil {
				t.Fatal(err)
			}
			r := recd(RecWrite, id, uint64(seq), v.Counter, core.SegmentID(f%genSegs+1), uint32(i)*8, int64(i)*4096, 4096)
			recs = append(recs, r)
		}
	}

	// The incremental reference: the same records in append order, one
	// update per record, noting the segments as the journal does.
	ref := &files{}
	for _, r := range recs {
		_ = ref.update(r.File, func(f *file) error {
			_ = f.recorded(r.Segment, ext(r.Off, r.Len), r.Numbers, r.Tag, r.File)
			f.write(Placed{Extent: ext(r.Off, r.Len), Record: RecordAt{Segment: r.Segment, Offset: r.At}, Version: r.Numbers.Version})
			return nil
		})
	}

	// The rebuild, in a shuffled order, with the clock on Finish.
	order := slices.Clone(recs)
	rand.New(rand.NewPCG(9, 3375)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	start := time.Now()
	fs := rebuild(t, order, ver(0))
	elapsed := time.Since(start)

	if got, want := fs.extentCount(), ref.extentCount(); got != want {
		t.Fatalf("extentCount() = %d, want %d, the reference's", got, want)
	}
	for i := range genSegs {
		seg := core.SegmentID(i + 1)
		if got, want := fs.segmentBytes(seg), ref.segmentBytes(seg); got != want {
			t.Fatalf("segment %d counts %+v, want the reference's %+v", seg, got, want)
		}
	}
	if n := fs.dropRangeCount(); n <= 0 {
		t.Fatalf("dropRangeCount() = %d, want the notes' ranges: the note pass never ran", n)
	}
	t.Logf("rebuilt %d records over %d files in %v", len(recs), fileCount, elapsed)
	if elapsed > 30*time.Second {
		t.Fatalf("rebuild took %v, over the sanity bound", elapsed)
	}
}
