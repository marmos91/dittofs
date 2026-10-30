package index

import (
	"math"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// rebuildCounting plants, before Finish, an entry for id whose held tree
// counts key comparisons — the same way the standalone sets' cost tests do —
// so the replay's own puts and the passes around them are counted. The
// entry is the one the replay then fills.
func rebuildCounting(t *testing.T, fs *files, id core.FileID) *atomic.Uint64 {
	t.Helper()
	f := newFile(&fs.segs)
	f.held.tree.cmps = new(atomic.Uint64)
	s := fs.shard(id)
	s.mu.Lock()
	if s.m == nil {
		s.m = make(map[core.FileID]*file)
	}
	s.m[id] = f
	s.mu.Unlock()
	return f.held.tree.cmps
}

// rebuildCost rebuilds n records, all of one file, fed in a shuffled order,
// and returns the tree comparisons the replay made and the rebuilt index.
func rebuildCost(t *testing.T, n int) (uint64, *files, time.Duration) {
	t.Helper()
	fs := &files{}
	id := fid(1)
	recs := make([]Record, 0, n)
	for i := range n {
		recs = append(recs, recd(RecWrite, id, uint64(i+1), uint64(i+1),
			core.SegmentID(1+i%genSegs), uint32(i%60_000)*4096, int64(i)*4096, 4096))
	}
	shuffled := slices.Clone(recs)
	rand.New(rand.NewPCG(uint64(n), 3)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	b := newRebuilder(fs, &Counters{})
	for _, r := range shuffled {
		if err := b.Feed(r); err != nil {
			t.Fatalf("Feed: %v", err)
		}
	}
	cmps := rebuildCounting(t, fs, id)
	start := time.Now()
	if err := b.Finish(ver(0)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return cmps.Load(), fs, time.Since(start)
}

// TestRebuildCost: the rebuild's costs grow at most logarithmically, and
// the rebuilt count and per-segment counters are exact. Every record is one
// file's. The comparison count catches a super-linear search or insert;
// it cannot see a whole-file walk, which visits nodes without comparing
// keys, so the time ratio of the two runs catches that: 8x the records
// costs about 9x the time, a walk per record about 64x, with or without
// the race detector.
func TestRebuildCost(t *testing.T) {
	if testing.Short() {
		t.Skip("rebuilds 2^17 records")
	}
	small, fsSmall, smallTime := rebuildCost(t, 1<<14)
	large, fsLarge, largeTime := rebuildCost(t, 1<<17)

	// 8x the records costs about 8x the log ratio, about 9.4x the time; a
	// walk per record costs about 64x. The ratio holds with and without
	// the race detector.
	if r := float64(largeTime) / float64(smallTime); r > 24 {
		t.Fatalf("2^17 rebuild took %.1fx the 2^14 one, want at most 24x: a step of the replay is not logarithmic", r)
	}

	if got := fsLarge.extentCount(); got != 1<<17 {
		t.Fatalf("extentCount() = %d, want %d", got, 1<<17)
	}
	if got := fsSmall.extentCount(); got != 1<<14 {
		t.Fatalf("extentCount() = %d, want %d", got, 1<<14)
	}
	segs := make([]core.SegmentID, 0, genSegs)
	for i := range genSegs {
		segs = append(segs, core.SegmentID(i+1))
	}
	checkSegments(t, 0, fsSmall, segs...)
	checkSegments(t, 1, fsLarge, segs...)

	smallPer, largePer := float64(small)/float64(1<<14), float64(large)/float64(1<<17)
	t.Logf("comparisons per record: %.1f at 2^14, %.1f at 2^17", smallPer, largePer)
	// log2 grows by 2.33 between the sizes; a whole-file walk per record
	// grows per-record comparisons by a factor of 131 072 / 16 384 = 8.
	if largePer > 2.5*smallPer {
		t.Fatalf("%.1f comparisons per record at 2^17, over 2.5× the %.1f at 2^14", largePer, smallPer)
	}
	// A put costs a few descents — cutting, setting, and the two ends'
	// merge checks — each about log2(n) key comparisons; 64 log2(n) holds
	// every pass' account with room.
	if cap := 64 * (math.Log2(float64(1<<17)) + 1); largePer > cap {
		t.Fatalf("%.1f comparisons per record at 2^17, over the %.1f bound", largePer, cap)
	}
	if largePer < 1 {
		t.Fatalf("%.1f comparisons per record: the counter never ran", largePer)
	}
}
