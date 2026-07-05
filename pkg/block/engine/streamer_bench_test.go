package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

const (
	streamerChunkSize = 1 * 1024 * 1024 // 1 MiB — FastCDC MinChunkSize floor
	streamerPerBlock  = 4               // 4 chunks × 1 MiB ≈ 4 MiB target block
)

// streamerFixture holds pre-hashed pseudo-random chunks and block IDs for
// blockCount blocks of streamerPerBlock chunks each. Building it is kept out of
// every timed / measured region so only the carve→codec→PutBlock cost is seen.
type streamerFixture struct {
	chunks   [][]byte
	hashes   []block.ContentHash
	blockIDs []string
}

func newStreamerFixture(blockCount int) streamerFixture {
	chunkCount := blockCount * streamerPerBlock
	rng := rand.New(rand.NewSource(1))
	f := streamerFixture{
		chunks:   make([][]byte, chunkCount),
		hashes:   make([]block.ContentHash, chunkCount),
		blockIDs: make([]string, blockCount),
	}
	for i := range f.chunks {
		c := make([]byte, streamerChunkSize)
		_, _ = rng.Read(c) // distinct bytes so hashes/codec bodies don't dedup
		f.chunks[i] = c
		f.hashes[i] = block.ContentHash(blake3.Sum256(c))
	}
	for i := range f.blockIDs {
		f.blockIDs[i] = fmt.Sprintf("blk-%d", i)
	}
	return f
}

// streamBlocks runs the carve→codec→PutBlock inner loop for every block in the
// fixture: frame streamerPerBlock chunks into one block via blockcodec and
// upload it. This is the stage under profile — no Syncer orchestration, no WAN.
func (f streamerFixture) streamBlocks(ctx context.Context, rbs *remotememory.Store) error {
	for blk := range f.blockIDs {
		var buf bytes.Buffer
		builder, err := blockcodec.NewBuilder(&buf, f.blockIDs[blk], nil)
		if err != nil {
			return err
		}
		for j := range streamerPerBlock {
			idx := blk*streamerPerBlock + j
			if _, err := builder.Add(f.hashes[idx], f.chunks[idx]); err != nil {
				return err
			}
		}
		if _, err := builder.Finish(); err != nil {
			return err
		}
		if err := rbs.PutBlock(ctx, f.blockIDs[blk], &buf); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkStreamer_CarveCodecPutBlock profiles the upload streamer stage in
// isolation: frame pre-hashed chunks into target-sized blocks via blockcodec
// (the carveAndCommitBlock inner loop, which reuses each chunk's write-time hash
// rather than rehashing) and PutBlock each assembled block to an in-memory
// remote. It deliberately drops the Syncer orchestration (carve queue, retries,
// dispatcher, dynsem) and real WAN — those are latency/concurrency-bound and
// covered by BenchmarkReadThroughCache_ColdVsWarm and the #1432 remote lane.
// BLAKE3 hashing is its own stage (BenchmarkBLAKE3_256MiB) and is precomputed
// in the fixture here, matching production carve. This is the in-memory
// CPU+alloc floor of carve → codec → PutBlock, so a -cpuprofile / -memprofile
// run attributes cost between codec framing and the remote body copy.
//
//	go test -bench=BenchmarkStreamer_CarveCodecPutBlock -benchmem -run=^$ \
//	    -cpuprofile=cpu.out -memprofile=mem.out ./pkg/block/engine/
func BenchmarkStreamer_CarveCodecPutBlock(b *testing.B) {
	const blockCount = 16 // 16 × 4 MiB ≈ 64 MiB working set
	f := newStreamerFixture(blockCount)
	ctx := context.Background()

	// One remote reused across iterations: same block IDs overwrite in place, so
	// the fake's retained bytes are not re-charged each op — leaving the timed
	// allocations to the codec framing and PutBlock body copy, not fixture churn.
	rbs := remotememory.New()
	b.SetBytes(int64(blockCount * streamerPerBlock * streamerChunkSize))
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if err := f.streamBlocks(ctx, rbs); err != nil {
			b.Fatal(err)
		}
	}
}

// TestStreamer_AllocationTracksBlockCount pins the streaming data-plane memory
// invariant (#1555 Refinement 2): the streamer's allocation grows linearly with
// the number of blocks, never super-linearly with total file size. A regression
// that buffers the whole file instead of one block at a time would inflate
// bytes-per-block as the file grows. We stream two file sizes and assert
// per-block allocation stays flat.
func TestStreamer_AllocationTracksBlockCount(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~80 MiB of chunk fixtures across the two runs; skip under -short")
	}
	ctx := context.Background()

	// bytesPerBlock streams blockCount blocks and returns the cumulative bytes
	// allocated by streamBlocks alone (fixture build is excluded), divided by the
	// block count. TotalAlloc is monotonic-cumulative, so the delta captures
	// allocation pressure independent of GC timing.
	bytesPerBlock := func(blockCount int) float64 {
		f := newStreamerFixture(blockCount)
		rbs := remotememory.New()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if err := f.streamBlocks(ctx, rbs); err != nil {
			t.Fatalf("streamBlocks(%d): %v", blockCount, err)
		}
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc-before.TotalAlloc) / float64(blockCount)
	}

	small := bytesPerBlock(4)
	large := bytesPerBlock(16)

	// Linear streaming keeps per-block bytes flat; allow 50% slack for heap
	// bookkeeping noise. A whole-file-buffering regression blows well past this.
	if ratio := large / small; ratio > 1.5 {
		t.Fatalf("per-block allocation grew %.2fx from 4→16 blocks (small=%.0f B, large=%.0f B); "+
			"streamer must carve one block at a time, not buffer the whole file", ratio, small, large)
	}
}
