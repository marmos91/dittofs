package engine

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// stubJournalRemote satisfies journal.RemoteStore for a Store whose cold-read
// path is never exercised (carve drives the injected sink, not this remote).
type stubJournalRemote struct{}

func (stubJournalRemote) PutBlock(context.Context, journal.BlockID, io.Reader, int64) error {
	return errors.New("stub: PutBlock unused")
}
func (stubJournalRemote) GetBlock(context.Context, journal.BlockID) (io.ReadCloser, error) {
	return nil, errors.New("stub: GetBlock unused")
}
func (stubJournalRemote) GetRange(context.Context, journal.BlockID, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("stub: GetRange unused")
}

// seamFixture wires a live journal.Store to the production engineDeduper +
// engineBlockSink, a memory metadata store (committer + synced oracle) and a
// memory block-keyed remote.
type seamFixture struct {
	dir  string
	ms   *metadatamemory.MemoryMetadataStore
	mem  *remotememory.Store
	jrnl *journal.Store
}

func newSeamFixture(t *testing.T, dir string, ms *metadatamemory.MemoryMetadataStore, mem *remotememory.Store, sink journal.BlockSink) *seamFixture {
	t.Helper()
	j, err := journal.Open(dir, journal.Config{CarveBlockSize: 1 << 20}, stubJournalRemote{}, journal.SystemClock())
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	j.SetCarveTargets(engineDeduper{synced: ms}, sink)
	return &seamFixture{dir: dir, ms: ms, mem: mem, jrnl: j}
}

func realSink(ms *metadatamemory.MemoryMetadataStore, mem *remotememory.Store) engineBlockSink {
	return engineBlockSink{sealer: nil, rbs: mem, committer: ms}
}

func countBlocks(t *testing.T, ctx context.Context, mem *remotememory.Store) int {
	t.Helper()
	n := 0
	if err := mem.WalkBlocks(ctx, func(string, block.Meta) error { n++; return nil }); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	return n
}

func seamRandBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestJournalCarveSeam_CommitsBlocksAndFileChunkRows(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	f := newSeamFixture(t, t.TempDir(), ms, mem, realSink(ms, mem))

	data := seamRandBytes(3<<20, 1)
	if err := f.jrnl.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := f.jrnl.Carve(ctx, journal.CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}

	// A block object landed on the remote.
	if got := countBlocks(t, ctx, mem); got < 1 {
		t.Fatalf("remote blocks = %d, want >= 1", got)
	}
	// One FileChunk manifest row per carved chunk; DataSize tiles the whole file.
	rows, err := ms.ListFileChunks(ctx, "f")
	if err != nil {
		t.Fatalf("ListFileChunks: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no FileChunk rows written")
	}
	var total int
	for _, r := range rows {
		if r.State != block.BlockStatePending {
			t.Fatalf("row %s state = %v, want Pending", r.ID, r.State)
		}
		if r.DataSize == 0 || r.Hash == (block.ContentHash{}) {
			t.Fatalf("row %s missing DataSize/Hash", r.ID)
		}
		total += int(r.DataSize)
	}
	if total != len(data) {
		t.Fatalf("FileChunk DataSize sum = %d, want %d", total, len(data))
	}
	// Carve flipped the records synced (block committed => durable frontier moved).
	if u := f.jrnl.UnsyncedBytes(); u != 0 {
		t.Fatalf("post-carve unsynced = %d, want 0 (flip after commit)", u)
	}
}

func TestJournalCarveSeam_DuplicateIsNoOp(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()
	f := newSeamFixture(t, t.TempDir(), ms, mem, realSink(ms, mem))

	data := seamRandBytes(2<<20, 2)
	if err := f.jrnl.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jrnl.Carve(ctx, journal.CarveOptions{Force: true}); err != nil {
		t.Fatalf("carve f: %v", err)
	}
	blocksAfterF := countBlocks(t, ctx, mem)

	// A second file with identical content: every chunk is already remote-durable,
	// so carve dedups to a no-op — no new block object is uploaded.
	if err := f.jrnl.WriteAt(ctx, "g", 0, data); err != nil {
		t.Fatal(err)
	}
	res, err := f.jrnl.Carve(ctx, journal.CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("carve g: %v", err)
	}
	if res.BlocksWritten != 0 || res.BytesCarved != 0 {
		t.Fatalf("duplicate carve was not a no-op: %+v", res)
	}
	if got := countBlocks(t, ctx, mem); got != blocksAfterF {
		t.Fatalf("duplicate carve uploaded new blocks: %d -> %d", blocksAfterF, got)
	}
	// The duplicate's records still flip synced (bytes are provably remote).
	if u := f.jrnl.UnsyncedBytes(); u != 0 {
		t.Fatalf("duplicate carve left dirty bytes: %d", u)
	}
}

// failOnceSink commits for real, then returns an injected error on the first
// call — modeling a crash after the metadata commit but before journal flips its
// records synced. The ms is left fully committed; the journal records stay dirty.
type failOnceSink struct {
	inner  engineBlockSink
	failed bool
}

func (s *failOnceSink) CommitBlock(ctx context.Context, chunks []journal.CarveChunk) error {
	if err := s.inner.CommitBlock(ctx, chunks); err != nil {
		return err
	}
	if !s.failed {
		s.failed = true
		return errors.New("injected crash after commit, before flip")
	}
	return nil
}

func TestJournalCarveSeam_CrashMidCommitReCarveIsNoOp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	mem := remotememory.New()

	// First carve: the sink commits (block + rows + synced markers) then errors,
	// so the journal leaves its records dirty even though the commit is durable.
	fail := &failOnceSink{inner: realSink(ms, mem)}
	f := newSeamFixture(t, dir, ms, mem, fail)
	data := seamRandBytes(1<<20, 3)
	if err := f.jrnl.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.jrnl.Carve(ctx, journal.CarveOptions{Force: true}); err == nil {
		t.Fatalf("expected carve to surface the injected commit failure")
	}
	if f.jrnl.UnsyncedBytes() == 0 {
		t.Fatalf("records flipped despite the failed carve")
	}
	blocksAfter := countBlocks(t, ctx, mem)
	rowsAfter, _ := ms.ListFileChunks(ctx, "f")
	_ = f.jrnl.Close()

	// Reopen: recovery replays the still-dirty records. Re-carve with a healthy
	// sink dedups every chunk (already remote-durable) — no new block, no new
	// rows, and the records finally flip synced.
	f2 := newSeamFixture(t, dir, ms, mem, realSink(ms, mem))
	res, err := f2.jrnl.Carve(ctx, journal.CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("re-carve after reopen: %v", err)
	}
	if res.BlocksWritten != 0 {
		t.Fatalf("re-carve re-uploaded a block: %+v", res)
	}
	if got := countBlocks(t, ctx, mem); got != blocksAfter {
		t.Fatalf("re-carve changed block count %d -> %d", blocksAfter, got)
	}
	if rows, _ := ms.ListFileChunks(ctx, "f"); len(rows) != len(rowsAfter) {
		t.Fatalf("re-carve changed row count %d -> %d", len(rowsAfter), len(rows))
	}
	if u := f2.jrnl.UnsyncedBytes(); u != 0 {
		t.Fatalf("re-carve did not flip records: unsynced = %d", u)
	}
}
