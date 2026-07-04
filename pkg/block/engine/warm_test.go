package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// listFilesLocal wraps a memory LocalStore and overrides ListFiles to return a
// fixed payload set. The memory store only tracks files that were written
// through its append path; the warm tests seed blocks straight into the CAS +
// FileChunk store, so this lets WarmAll enumerate them without a write loop.
type listFilesLocal struct {
	*memorylocal.MemoryStore
	files []string
}

func (l *listFilesLocal) ListFiles() []string { return append([]string(nil), l.files...) }

// seedFileChunkAt installs a FileChunk row covering [offset, offset+len(data))
// under payloadID and seeds the remote with a packed block holding the
// matching bytes plus a synced marker carrying the block locator. Unlike
// seedFileChunk it lets the test place a row at a non-zero chunk offset so a
// single payload can carry multiple blocks.
func seedFileChunkAt(t *testing.T, fbs *stubFileChunkStore, rs *remotememory.Store, mds *metadatamemory.MemoryMetadataStore, payloadID string, offset uint64, data []byte) block.ContentHash {
	t.Helper()
	return seedSyncedRemoteChunk(t, fbs, rs, mds, payloadID, offset, data)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// warmHarness wires a Syncer over a list-aware memory local store, a memory
// remote, a stub FileChunk store, and a memory metadata store serving as the
// SyncedHashStore the block-locator fetch path resolves from.
func warmHarness(payloads []string) (*Syncer, *listFilesLocal, *remotememory.Store, *stubFileChunkStore, *metadatamemory.MemoryMetadataStore) {
	loc := &listFilesLocal{MemoryStore: memorylocal.New(), files: payloads}
	rs := remotememory.New()
	fbs := newStubFileChunkStore()
	mds := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	m := newFetchSyncer(loc, rs, fbs, mds)
	return m, loc, rs, fbs, mds
}

func TestWarmAll_FetchesMissingSkipsLocal(t *testing.T) {
	ctx := context.Background()
	m, loc, rs, fbs, mds := warmHarness([]string{"payA", "payB"})

	// payA: two remote blocks at offsets 0 and BlockSize.
	dataA0 := []byte("payA-block-zero-bytes")
	hA0 := seedFileChunkAt(t, fbs, rs, mds, "payA", 0, dataA0)
	hA1 := seedFileChunkAt(t, fbs, rs, mds, "payA", uint64(BlockSize), []byte("payA-block-one-bytes"))
	// payB: one remote block at offset 0.
	hB0 := seedFileChunkAt(t, fbs, rs, mds, "payB", 0, []byte("payB-block-zero-bytes"))

	// Pre-place payA/0 locally so WarmAll must skip it.
	if err := loc.Put(ctx, hA0, dataA0); err != nil {
		t.Fatalf("pre-seed local hA0: %v", err)
	}

	// The progress callback is invoked concurrently from each fetch goroutine,
	// so guard the captured counters (mirrors how the shares registry callback
	// takes its mutex).
	var (
		progMu              sync.Mutex
		lastDone, lastTotal int64
	)
	res, err := m.WarmAll(ctx, func(done, total int64) {
		progMu.Lock()
		lastDone, lastTotal = done, total
		progMu.Unlock()
	})
	if err != nil {
		t.Fatalf("WarmAll: %v", err)
	}

	if res.BlocksAlreadyLocal != 1 {
		t.Errorf("BlocksAlreadyLocal = %d; want 1", res.BlocksAlreadyLocal)
	}
	if res.BlocksFetched != 2 {
		t.Errorf("BlocksFetched = %d; want 2", res.BlocksFetched)
	}
	if res.BytesFetched <= 0 {
		t.Errorf("BytesFetched = %d; want > 0", res.BytesFetched)
	}
	if lastTotal != 2 || lastDone != 2 {
		t.Errorf("progress final = (%d/%d); want (2/2)", lastDone, lastTotal)
	}

	// Every block must now be present in the local CAS tier.
	for _, h := range []block.ContentHash{hA0, hA1, hB0} {
		has, err := loc.Has(ctx, h)
		if err != nil {
			t.Fatalf("local Has: %v", err)
		}
		if !has {
			t.Errorf("block %s not local after WarmAll", h.String())
		}
	}
}

// TestWarmAll_ProgressMonotonic guards against the progress callback reporting
// a stale, lower `done` last under concurrency: with the counter snapshot taken
// outside the emit, a goroutine that incremented to N-1 could call back after
// the one that reached N, leaving the final progress below total. With many
// fetch targets and parallel downloads, the reported `done` sequence must be
// non-decreasing and end exactly at total.
func TestWarmAll_ProgressMonotonic(t *testing.T) {
	ctx := context.Background()

	const n = 12
	payloads := make([]string, n)
	for i := range payloads {
		payloads[i] = "pay" + itoa(uint64(i))
	}
	m, _, rs, fbs, mds := warmHarness(payloads)
	for _, p := range payloads {
		seedFileChunkAt(t, fbs, rs, mds, p, 0, []byte(p+"-block-zero"))
	}

	var (
		mu  sync.Mutex
		seq []int64
	)
	res, err := m.WarmAll(ctx, func(done, total int64) {
		mu.Lock()
		seq = append(seq, done)
		mu.Unlock()
		if total != n {
			t.Errorf("progress total = %d; want %d", total, n)
		}
	})
	if err != nil {
		t.Fatalf("WarmAll: %v", err)
	}
	if res.BlocksFetched != n {
		t.Errorf("BlocksFetched = %d; want %d", res.BlocksFetched, n)
	}

	// Initial progress(0, total) plus one tick per fetch: 0,1,2,...,n.
	if len(seq) != n+1 {
		t.Fatalf("progress emitted %d times; want %d (got %v)", len(seq), n+1, seq)
	}
	for i, done := range seq {
		if done != int64(i) {
			t.Fatalf("progress not monotonic: seq[%d] = %d, want %d (full %v)", i, done, i, seq)
		}
	}
}

// TestWarmAll_FetchesNonAlignedChunk guards #1374: FastCDC chunks start at
// arbitrary byte offsets that do NOT align to BlockSize. The old WarmAll
// fetched via fetchBlock(payloadID, blockIdx), and fetchBlock resolves the row
// covering byte blockIdx*BlockSize — so a chunk whose offset is not a multiple
// of BlockSize was never resolved and was silently skipped (WarmAll reported
// success but fetched nothing). WarmAll now fetches the enumerated row directly
// (fetchResolvedBlock), so the non-aligned chunk must land locally.
func TestWarmAll_FetchesNonAlignedChunk(t *testing.T) {
	ctx := context.Background()
	m, _, rs, fbs, mds := warmHarness([]string{"payA"})

	// A remote-only chunk at a deliberately non-BlockSize-aligned offset.
	offset := uint64(BlockSize) + 12345
	hash := seedFileChunkAt(t, fbs, rs, mds, "payA", offset, []byte("non-aligned-fastcdc-chunk-bytes"))

	res, err := m.WarmAll(ctx, nil)
	if err != nil {
		t.Fatalf("WarmAll: %v", err)
	}
	if res.BlocksFetched != 1 {
		t.Errorf("BlocksFetched = %d; want 1 (non-aligned chunk must be fetched)", res.BlocksFetched)
	}
	if res.BytesFetched <= 0 {
		t.Errorf("BytesFetched = %d; want > 0", res.BytesFetched)
	}

	has, err := m.local.Has(ctx, hash)
	if err != nil {
		t.Fatalf("local Has: %v", err)
	}
	if !has {
		t.Errorf("non-aligned chunk %s not present locally after WarmAll", hash.String())
	}
}

func TestWarmAll_NoRemote(t *testing.T) {
	m, _, _, _, _ := warmHarness([]string{"payA"})
	m.remoteStore = nil
	if _, err := m.WarmAll(context.Background(), nil); err == nil {
		t.Fatal("WarmAll with nil remote: want error, got nil")
	}
}

func TestWarmAll_Cancellation(t *testing.T) {
	m, _, rs, fbs, mds := warmHarness([]string{"payA"})
	// Seed several remote blocks so there is work to cancel.
	for i := uint64(0); i < 8; i++ {
		seedFileChunkAt(t, fbs, rs, mds, "payA", i*uint64(BlockSize), []byte("payA-block-bytes-"+itoa(i)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the run starts

	_, err := m.WarmAll(ctx, nil)
	if err == nil {
		t.Fatal("WarmAll with cancelled context: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}
