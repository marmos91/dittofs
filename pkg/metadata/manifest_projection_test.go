package metadata

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/stretchr/testify/require"
)

// projTestChunkSpan is the offset grid batches draw from — small enough that a
// run keeps landing on offsets earlier batches already wrote.
const projTestChunkSpan = 1 << 12

// manifestTx models the manifest half of a metadata transaction: FileChunk rows
// keyed by ID (upsert semantics) plus the single File row a projection writes
// onto. ListFileChunks hands rows back in lexicographic ID order, which stops
// matching offset order as soon as a file passes ten chunks, so a projection
// that leans on list order instead of sorting fails here.
type manifestTx struct {
	Transaction
	rows    map[string]*block.FileChunk
	file    *File
	records map[string]struct{}
}

func newManifestTx(payloadID string) *manifestTx {
	return &manifestTx{
		rows:    map[string]*block.FileChunk{},
		file:    &File{FileAttr: FileAttr{PayloadID: PayloadID(payloadID)}},
		records: map[string]struct{}{},
	}
}

func (t *manifestTx) Put(_ context.Context, fc *block.FileChunk) error {
	t.rows[fc.ID] = fc
	return nil
}

func (t *manifestTx) deleteRow(id string) { delete(t.rows, id) }

// Delete follows the FileChunkStore contract: a missing ID is an error, not a
// silent no-op, so a reap that deletes a row twice or deletes one it never
// listed shows up here instead of passing.
func (t *manifestTx) Delete(_ context.Context, id string) error {
	if _, ok := t.rows[id]; !ok {
		return block.ErrFileChunkNotFound
	}
	t.deleteRow(id)
	return nil
}

func (t *manifestTx) ListFileChunks(_ context.Context, payloadID string) ([]*block.FileChunk, error) {
	ids := make([]string, 0, len(t.rows))
	for id := range t.rows {
		if chunkPayloadID(id) == payloadID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]*block.FileChunk, 0, len(ids))
	for _, id := range ids {
		out = append(out, t.rows[id])
	}
	return out, nil
}

// GetFileByPayloadID hands back a copy, as the real backends do, so a projection
// cannot fake coherence by mutating the stored row's slice in place.
func (t *manifestTx) GetFileByPayloadID(_ context.Context, pid PayloadID) (*File, error) {
	if t.file.PayloadID != pid {
		return nil, &StoreError{Code: ErrNotFound, Message: "no file for payload"}
	}
	return copyFileRow(t.file), nil
}

func (t *manifestTx) PutFile(_ context.Context, f *File) error {
	t.file = copyFileRow(f)
	return nil
}

func (t *manifestTx) GetBlockRecord(_ context.Context, blockID string) (block.BlockRecord, bool, error) {
	_, ok := t.records[blockID]
	return block.BlockRecord{BlockID: blockID}, ok, nil
}

func (t *manifestTx) PutBlockRecord(_ context.Context, rec block.BlockRecord) error {
	t.records[rec.BlockID] = struct{}{}
	return nil
}

func (t *manifestTx) WithTransaction(_ context.Context, fn func(tx Transaction) error) error {
	return fn(t)
}

func copyFileRow(f *File) *File {
	cp := *f
	if f.Blocks != nil {
		cp.Blocks = append([]block.ChunkRef(nil), f.Blocks...)
	}
	return &cp
}

// fullProjection is what File.Blocks would hold if it were re-derived from the
// whole manifest — the reference the incremental projection must reproduce.
func (t *manifestTx) fullProjection(tb testing.TB, payloadID string) []block.ChunkRef {
	tb.Helper()
	rows, err := t.ListFileChunks(context.Background(), payloadID)
	require.NoError(tb, err)
	return ManifestToChunkRefs(rows)
}

func testHash(seed int) block.ContentHash {
	var h block.ContentHash
	binary.LittleEndian.PutUint64(h[:], uint64(seed))
	return h
}

func chunkID(payloadID string, offset uint64) string {
	return fmt.Sprintf("%s/%d", payloadID, offset)
}

func chunkRow(payloadID string, offset uint64, seed int, size uint32) *block.FileChunk {
	return &block.FileChunk{
		ID:       chunkID(payloadID, offset),
		Hash:     testHash(seed),
		DataSize: size,
		State:    block.BlockStatePending,
	}
}

// randomBatch mimics one carved block object's manifest rows. Offsets are drawn
// from a window that widens with the run, so a batch mixes fresh appends with
// rewrites of offsets earlier batches wrote, in arbitrary order. Every fifth
// batch also carries an entry the commit path must tolerate: a nil, a second row
// for an offset already in the batch, a row whose offset does not parse, and a
// row belonging to another payload.
func randomBatch(rng *rand.Rand, payloadID string, batchNo int) []*block.FileChunk {
	window := 8 + batchNo*2
	// The first row must be well-formed: the payload the batch projects onto is
	// read off it.
	rows := make([]*block.FileChunk, 0, 9)
	for i := range 1 + rng.Intn(6) {
		rows = append(rows, chunkRow(payloadID, uint64(rng.Intn(window))*projTestChunkSpan, batchNo*100+i, uint32(1+rng.Intn(1<<20))))
	}
	switch batchNo % 5 {
	case 1:
		rows = append(rows, nil)
	case 2:
		dup := *rows[0]
		dup.Hash = testHash(batchNo*100 + 99)
		dup.DataSize = dup.DataSize + 1
		rows = append(rows, &dup)
	case 3:
		rows = append(rows, &block.FileChunk{ID: payloadID + "/not-an-offset", Hash: testHash(batchNo)})
	case 4:
		rows = append(rows, chunkRow("other-payload", uint64(batchNo)*projTestChunkSpan, batchNo, 4096))
	}
	return rows
}

// TestCommitBlockProjectionMatchesFullReprojection drives a long, adversarial
// commit sequence through the carve-commit path and asserts File.Blocks is, after
// every batch, exactly what re-deriving the projection from the whole manifest
// would produce.
func TestCommitBlockProjectionMatchesFullReprojection(t *testing.T) {
	const payloadID = "payload-1"
	ctx := context.Background()
	tx := newManifestTx(payloadID)
	rng := rand.New(rand.NewSource(20260805))

	for batchNo := range 300 {
		rows := randomBatch(rng, payloadID, batchNo)
		rec := block.BlockRecord{BlockID: fmt.Sprintf("block-%d", batchNo), LiveChunkCount: uint32(len(rows))}
		require.NoError(t, DefaultCommitBlock(ctx, tx, rec, nil, rows))

		want := tx.fullProjection(t, payloadID)
		require.Equal(t, want, tx.file.Blocks, "projection after batch %d", batchNo)
		require.True(t, tx.file.BlocksDirty, "batch %d must mark the block list dirty", batchNo)

		// Re-committing the same block object is a no-op, projection included.
		require.NoError(t, DefaultCommitBlock(ctx, tx, rec, nil, rows))
		require.Equal(t, want, tx.file.Blocks, "re-commit of batch %d", batchNo)
	}
	require.Greater(t, len(tx.file.Blocks), 100, "the run must have built a substantial manifest")
}

// TestProjectCommittedChunksEdgeCases covers the boundaries the random run only
// reaches by luck.
func TestProjectCommittedChunksEdgeCases(t *testing.T) {
	const payloadID = "payload-1"
	ctx := context.Background()

	t.Run("first commit bootstraps from the manifest", func(t *testing.T) {
		tx := newManifestTx(payloadID)
		rows := []*block.FileChunk{
			chunkRow(payloadID, 8192, 2, 4096),
			chunkRow(payloadID, 0, 1, 4096),
		}
		for _, r := range rows {
			require.NoError(t, tx.Put(ctx, r))
		}
		require.NoError(t, ProjectCommittedChunks(ctx, tx, payloadID, rows))
		require.Equal(t, tx.fullProjection(t, payloadID), tx.file.Blocks)
	})

	t.Run("no rows for this payload leaves the projection alone", func(t *testing.T) {
		tx := newManifestTx(payloadID)
		seed := []*block.FileChunk{chunkRow(payloadID, 0, 1, 4096), chunkRow(payloadID, 4096, 2, 4096)}
		for _, r := range seed {
			require.NoError(t, tx.Put(ctx, r))
		}
		require.NoError(t, ProjectManifestToBlocks(ctx, tx, payloadID))
		before := tx.file.Blocks

		foreign := []*block.FileChunk{chunkRow("other-payload", 0, 9, 4096)}
		require.NoError(t, tx.Put(ctx, foreign[0]))
		require.NoError(t, ProjectCommittedChunks(ctx, tx, payloadID, foreign))
		require.Equal(t, before, tx.file.Blocks)
		require.Equal(t, tx.fullProjection(t, payloadID), tx.file.Blocks)
	})

	// The payload prefix is taken without validating the offset behind it, so a
	// row with an empty offset still names the file its batch projects onto. The
	// row itself is dropped, exactly as the full projection drops it.
	t.Run("a row with an empty offset names the file but is not projected", func(t *testing.T) {
		tx := newManifestTx(payloadID)
		for _, r := range []*block.FileChunk{chunkRow(payloadID, 0, 1, 4096), chunkRow(payloadID, 4096, 2, 4096)} {
			require.NoError(t, tx.Put(ctx, r))
		}
		require.NoError(t, ProjectManifestToBlocks(ctx, tx, payloadID))
		before := tx.file.Blocks

		rows := []*block.FileChunk{{ID: payloadID + "/", Hash: testHash(9), DataSize: 4096}}
		require.NoError(t, tx.Put(ctx, rows[0]))
		require.Equal(t, payloadID, payloadIDFromChunks(rows))
		require.NoError(t, ProjectCommittedChunks(ctx, tx, payloadIDFromChunks(rows), rows))
		require.Equal(t, before, tx.file.Blocks)
		require.Equal(t, tx.fullProjection(t, payloadID), tx.file.Blocks)
	})

	t.Run("empty payload is a no-op", func(t *testing.T) {
		tx := newManifestTx(payloadID)
		require.NoError(t, ProjectCommittedChunks(ctx, tx, "", nil))
		require.Nil(t, tx.file.Blocks)
	})

	t.Run("reap and re-commit stay coherent", func(t *testing.T) {
		tx := newManifestTx(payloadID)
		var rows []*block.FileChunk
		for i := range 6 {
			rows = append(rows, chunkRow(payloadID, uint64(i)*projTestChunkSpan, i, 4096))
		}
		require.NoError(t, DefaultCommitBlock(ctx, tx, block.BlockRecord{BlockID: "b0"}, nil, rows))

		// A run-end reap drops the rows a re-carve superseded and re-derives.
		tx.deleteRow(chunkID(payloadID, 2*projTestChunkSpan))
		tx.deleteRow(chunkID(payloadID, 3*projTestChunkSpan))
		require.NoError(t, ProjectManifestToBlocks(ctx, tx, payloadID))
		require.Equal(t, tx.fullProjection(t, payloadID), tx.file.Blocks)

		// The next batch merges into the reaped projection, not the pre-reap one.
		next := []*block.FileChunk{chunkRow(payloadID, 2*projTestChunkSpan, 42, 8192)}
		require.NoError(t, DefaultCommitBlock(ctx, tx, block.BlockRecord{BlockID: "b1"}, nil, next))
		require.Equal(t, tx.fullProjection(t, payloadID), tx.file.Blocks)
	})
}

// BenchmarkManifestProjectionOnCommit measures one block object's commit against
// an existing manifest: the full re-derivation lists and sorts every row, the
// incremental one touches only the batch. The in-memory manifest here has no I/O
// or row decode, so a real backend's list is dearer than this shows.
func BenchmarkManifestProjectionOnCommit(b *testing.B) {
	for _, manifestRows := range []int{1024, 8192, 65536} {
		b.Run(fmt.Sprintf("rows=%d/full", manifestRows), func(b *testing.B) {
			benchProjection(b, manifestRows, false)
		})
		b.Run(fmt.Sprintf("rows=%d/incremental", manifestRows), func(b *testing.B) {
			benchProjection(b, manifestRows, true)
		})
	}
}

func benchProjection(b *testing.B, manifestRows int, incremental bool) {
	const payloadID = "payload-1"
	ctx := context.Background()
	tx := newManifestTx(payloadID)
	for i := range manifestRows {
		require.NoError(b, tx.Put(ctx, chunkRow(payloadID, uint64(i)*projTestChunkSpan, i, projTestChunkSpan)))
	}
	require.NoError(b, ProjectManifestToBlocks(ctx, tx, payloadID))

	// One carved block object's worth of rows, spread across the manifest.
	const batchSize = 128
	batch := make([]*block.FileChunk, 0, batchSize)
	for i := range batchSize {
		off := uint64(i*(manifestRows/batchSize)) * projTestChunkSpan
		batch = append(batch, chunkRow(payloadID, off, i+1, projTestChunkSpan))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for _, fc := range batch {
			if err := tx.Put(ctx, fc); err != nil {
				b.Fatal(err)
			}
		}
		var err error
		if incremental {
			err = ProjectCommittedChunks(ctx, tx, payloadID, batch)
		} else {
			err = ProjectManifestToBlocks(ctx, tx, payloadID)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}
