package carver

import (
	"context"
	"math/rand"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// Sizing small enough to run the sweeps cheaply, large enough that a chunk can
// reach the Max ceiling — so every branch of the boundary search takes part.
var smallParams = chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10}

func allChunks(t testing.TB, o Options, data []byte) []Chunk {
	t.Helper()
	ctx := context.Background()
	cv := New(o)
	var chunks []Chunk
	for off := 0; off < len(data); {
		end := min(off+chunker.MaxChunkSize, len(data))
		blocks, tiled, err := cv.Box(ctx, data[off:end], int64(off), end == len(data))
		if err != nil {
			t.Fatalf("Box: %v", err)
		}
		if want := int64(end - off); tiled != want {
			t.Fatalf("Box tiled %d bytes of a %d-byte feed", tiled, want)
		}
		off = end
		for _, b := range blocks {
			chunks = append(chunks, b.Chunks...)
		}
	}
	for _, b := range cv.Drain() {
		chunks = append(chunks, b.Chunks...)
	}
	if len(chunks) == 0 {
		t.Fatalf("no chunks cut from %d bytes", len(data))
	}
	// Chunks must tile the stream ascending and contiguous, and the hashes must
	// identify the bytes they cover — dedup and the content-addressed commit key
	// on them.
	want := int64(0)
	for _, ch := range chunks {
		if ch.Offset != want {
			t.Fatalf("chunk at %d, want %d — tiling is not contiguous", ch.Offset, want)
		}
		want += ch.Size
	}
	if want != int64(len(data)) {
		t.Fatalf("chunks tile %d of %d bytes", want, len(data))
	}
	return chunks
}

// TestCarver_BoundaryIndependentOfBufferLength pins the property dedup rests
// on: where a chunk ends is decided by its bytes alone, never by how many bytes
// the caller happened to feed per Box call. Two writers of identical bytes feed
// different buffer lengths (their readers do not deliver in lockstep); if the
// cut moved with the feed, their chunk hashes would differ and dedup would
// silently stop collapsing them.
//
// So: cut one blob with a large feed, then again sweeping every buffer length
// from Min to the whole stream, and require identical chunk hashes.
func TestCarver_BoundaryIndependentOfBufferLength(t *testing.T) {
	data := randomBlob(t, 1<<20, 7)
	want := allChunks(t, Options{Params: smallParams}, data)
	wantHashes := make([]Hash, len(want))
	for i, ch := range want {
		wantHashes[i] = ch.Hash
	}

	for _, feed := range []int{smallParams.Min, smallParams.Min + 1, 3 * smallParams.Min, smallParams.Avg, smallParams.Max, 1 << 20} {
		ctx := context.Background()
		cv := New(Options{Params: smallParams})
		var got []Hash
		for off := 0; off < len(data); {
			end := min(off+feed, len(data))
			blocks, _, err := cv.Box(ctx, data[off:end], int64(off), end == len(data))
			if err != nil {
				t.Fatalf("feed %d: Box: %v", feed, err)
			}
			off = end
			for _, b := range blocks {
				for _, ch := range b.Chunks {
					got = append(got, ch.Hash)
				}
			}
		}
		for _, b := range cv.Drain() {
			for _, ch := range b.Chunks {
				got = append(got, ch.Hash)
			}
		}
		if !slices.Equal(wantHashes, got) {
			t.Fatalf("feed %d cut %d chunks, the full-feed cut has %d, and they differ — "+
				"the boundary moved with the buffer, not the content", feed, len(got), len(wantHashes))
		}
	}
}

// TestCarver_SkipCoversBytesWithoutData pins the dedup contract: a skipped
// chunk still tiles its range (the caller needs the manifest row even when the
// payload is redundant) but carries no bytes, and the block reports only what
// actually needs uploading.
func TestCarver_SkipCoversBytesWithoutData(t *testing.T) {
	data := randomBlob(t, 256<<10, 3)
	cv := New(Options{
		Params:    smallParams,
		BlockSize: 1 << 20,
		Skip: func(_ context.Context, h Hash) (bool, error) {
			// Skip every other chunk deterministically.
			return h[0]%2 == 0, nil
		},
	})
	ctx := context.Background()
	blocks, tiled, err := cv.Box(ctx, data, 0, true)
	if err != nil {
		t.Fatalf("Box: %v", err)
	}
	if tiled != int64(len(data)) {
		t.Fatalf("tiled %d of %d bytes", tiled, len(data))
	}
	blocks = append(blocks, cv.Drain()...)
	covered := int64(0)
	novel := int64(0)
	for _, b := range blocks {
		if b.Bytes != novelBytes(b) {
			t.Fatalf("block reports %d bytes, has %d novel", b.Bytes, novelBytes(b))
		}
		for _, ch := range b.Chunks {
			covered += ch.Size
			if ch.Data != nil {
				novel += ch.Size
			}
		}
	}
	if covered != int64(len(data)) {
		t.Fatalf("skipped chunks dropped bytes: tiled %d of %d", covered, len(data))
	}
	if novel == 0 || novel == covered {
		t.Fatalf("skip oracle did not split the stream: %d novel of %d", novel, covered)
	}
}

// TestCarver_SkipErrorStopsCall pins the first-error behaviour: an error from
// Skip ends the Box call with the blocks cut so far, and nothing after it.
func TestCarver_SkipErrorStopsCall(t *testing.T) {
	data := randomBlob(t, 512<<10, 5)
	sentinel := context.DeadlineExceeded
	calls := 0
	cv := New(Options{
		Params:    smallParams,
		BlockSize: 1 << 20,
		Skip: func(_ context.Context, _ Hash) (bool, error) {
			calls++
			if calls > 3 {
				return false, sentinel
			}
			return false, nil
		},
	})
	ctx := context.Background()
	_, _, err := cv.Box(ctx, data, 0, true)
	if err != sentinel {
		t.Fatalf("Box err = %v, want the Skip error", err)
	}
}

// TestCarver_DrainEmitsTrailingPartial pins the two-call contract: a batch
// below BlockSize is not emitted by Box (final included), only by Drain — the
// trailing partial block is the caller's, once its last stream is over.
func TestCarver_DrainEmitsTrailingPartial(t *testing.T) {
	data := randomBlob(t, 100<<10, 9) // below BlockSize: one trailing block only
	cv := New(Options{Params: smallParams, BlockSize: 1 << 20})
	ctx := context.Background()
	blocks, tiled, err := cv.Box(ctx, data, 0, true)
	if err != nil {
		t.Fatalf("Box: %v", err)
	}
	if tiled != int64(len(data)) {
		t.Fatalf("tiled %d of %d bytes", tiled, len(data))
	}
	if len(blocks) != 0 {
		t.Fatalf("Box emitted %d blocks below BlockSize", len(blocks))
	}
	blocks = cv.Drain()
	if len(blocks) != 1 || len(blocks[0].Chunks) == 0 {
		t.Fatalf("Drain returned %d blocks, want the one trailing partial", len(blocks))
	}
	if cv.Drain() != nil {
		t.Fatal("second Drain emitted something — batch state did not reset")
	}
}

// TestCarver_ChunkNeverSpansStreams pins the gap rule: final=true resets the
// boundary search, so the bytes before the gap and after it are chunked
// independently — feeding the two halves separately cuts identically to one
// contiguous feed.
func TestCarver_ChunkNeverSpansStreams(t *testing.T) {
	data := randomBlob(t, 512<<10, 13)
	want := allChunks(t, Options{Params: smallParams}, data)
	wantHashes := make([]Hash, len(want))
	for i, ch := range want {
		wantHashes[i] = ch.Hash
	}

	// Split at a point that would otherwise be mid-chunk: two streams, a final
	// Box on each, one Drain at the end.
	split := 200<<10 + 123
	ctx := context.Background()
	cv := New(Options{Params: smallParams})
	var got []Hash
	for i, parts := range [][]byte{data[:split], data[split:]} {
		blocks, _, err := cv.Box(ctx, parts, int64(0), i == 1)
		if err != nil {
			t.Fatalf("Box: %v", err)
		}
		for _, b := range blocks {
			for _, ch := range b.Chunks {
				got = append(got, ch.Hash)
			}
		}
	}
	for _, b := range cv.Drain() {
		for _, ch := range b.Chunks {
			got = append(got, ch.Hash)
		}
	}
	if !slices.Equal(wantHashes, got) {
		t.Fatalf("split feed cut %d chunks vs %d — a chunk spanned the stream gap", len(got), len(wantHashes))
	}
}

func novelBytes(b Block) int64 {
	var n int64
	for _, ch := range b.Chunks {
		if ch.Data != nil {
			n += ch.Size
		}
	}
	return n
}

func randomBlob(t testing.TB, n int, seed int64) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	rng.Read(b)
	return b
}
