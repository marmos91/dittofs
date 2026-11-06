package index

import (
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestDeltasStayWithTheirFile runs two goroutines, one file each in different
// shards, over one index. Every delta a goroutine's update returns must name
// its own file, and each goroutine's running sums must equal its file's
// read-out at the end: a delta read from a shared segment or store-wide
// counter would pick up the other goroutine's bytes.
func TestDeltasStayWithTheirFile(t *testing.T) {
	var fs files
	a, b := fid(1), fid(2)
	for fs.shard(a) == fs.shard(b) {
		b = fid(b.Bytes()[15] + 1)
	}
	kinds := []int{opWrite, opWrite, opWriteRetain, opMark, opMark, opTruncate}
	var wg sync.WaitGroup
	for i, id := range []core.FileID{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The goroutines' records share the segment table, so each takes a
			// range of segments of its own.
			w := &deltaWorld{t: t, fs: &fs, rng: rand.New(rand.NewPCG(uint64(i), 3796)), seed: uint64(i), base: core.SegmentID(i) << 20}
			w.fail = func(msg string) { t.Error(msg); runtime.Goexit() }
			f := &deltaFile{id: id, tag: core.ShareTag(1 + i)}
			for w.step = 0; w.step < 2000; w.step++ {
				w.run(f, kinds[w.rng.IntN(len(kinds))])
			}
		}()
	}
	wg.Wait()
}
