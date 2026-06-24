package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"lukechampine.com/blake3"
)

// listFilesLocal wraps a memory LocalStore and overrides ListFiles to return a
// fixed payload set. The memory store only tracks files that were written
// through its append path; the warm tests seed blocks straight into the CAS +
// FileBlock store, so this lets WarmAll enumerate them without a write loop.
type listFilesLocal struct {
	*memorylocal.MemoryStore
	files []string
}

func (l *listFilesLocal) ListFiles() []string { return append([]string(nil), l.files...) }

// seedFileBlockAt installs a FileBlock row covering [offset, offset+len(data))
// under payloadID and seeds the remote CAS with the matching bytes. Unlike
// seedFileBlock it lets the test place a row at a non-zero chunk offset so a
// single payload can carry multiple blocks.
func seedFileBlockAt(t *testing.T, fbs *stubFileBlockStore, rs *remotememory.Store, payloadID string, offset uint64, data []byte) block.ContentHash {
	t.Helper()
	hash := block.ContentHash(blake3.Sum256(data))
	if err := rs.Put(context.Background(), hash, data); err != nil {
		t.Fatalf("seed remote Put: %v", err)
	}
	fb := &block.FileBlock{
		ID:       payloadIDChunkID(payloadID, offset),
		Hash:     hash,
		DataSize: uint32(len(data)),
		State:    block.BlockStateRemote,
	}
	if err := fbs.Put(context.Background(), fb); err != nil {
		t.Fatalf("seed FileBlock at offset %d: %v", offset, err)
	}
	return hash
}

func payloadIDChunkID(payloadID string, offset uint64) string {
	return payloadID + "/" + itoa(offset)
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
// remote, and a stub FileBlock store.
func warmHarness(payloads []string) (*Syncer, *listFilesLocal, *remotememory.Store, *stubFileBlockStore) {
	loc := &listFilesLocal{MemoryStore: memorylocal.New(), files: payloads}
	rs := remotememory.New()
	fbs := newStubFileBlockStore()
	m := newFetchSyncer(loc, rs, fbs)
	return m, loc, rs, fbs
}

func TestWarmAll_FetchesMissingSkipsLocal(t *testing.T) {
	ctx := context.Background()
	m, loc, rs, fbs := warmHarness([]string{"payA", "payB"})

	// payA: two remote blocks at offsets 0 and BlockSize.
	hA0 := seedFileBlockAt(t, fbs, rs, "payA", 0, []byte("payA-block-zero-bytes"))
	hA1 := seedFileBlockAt(t, fbs, rs, "payA", uint64(BlockSize), []byte("payA-block-one-bytes"))
	// payB: one remote block at offset 0.
	hB0 := seedFileBlockAt(t, fbs, rs, "payB", 0, []byte("payB-block-zero-bytes"))

	// Pre-place payA/0 locally so WarmAll must skip it.
	dataA0, err := rs.Get(ctx, hA0)
	if err != nil {
		t.Fatalf("remote Get hA0: %v", err)
	}
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

// TestWarmAll_FetchesNonAlignedChunk guards #1374: FastCDC chunks start at
// arbitrary byte offsets that do NOT align to BlockSize. The old WarmAll
// fetched via fetchBlock(payloadID, blockIdx), and fetchBlock resolves the row
// covering byte blockIdx*BlockSize — so a chunk whose offset is not a multiple
// of BlockSize was never resolved and was silently skipped (WarmAll reported
// success but fetched nothing). WarmAll now fetches the enumerated row directly
// (fetchResolvedBlock), so the non-aligned chunk must land locally.
func TestWarmAll_FetchesNonAlignedChunk(t *testing.T) {
	ctx := context.Background()
	m, _, rs, fbs := warmHarness([]string{"payA"})

	// A remote-only chunk at a deliberately non-BlockSize-aligned offset.
	offset := uint64(BlockSize) + 12345
	hash := seedFileBlockAt(t, fbs, rs, "payA", offset, []byte("non-aligned-fastcdc-chunk-bytes"))

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
	m, _, _, _ := warmHarness([]string{"payA"})
	m.remoteStore = nil
	if _, err := m.WarmAll(context.Background(), nil); err == nil {
		t.Fatal("WarmAll with nil remote: want error, got nil")
	}
}

func TestWarmAll_Cancellation(t *testing.T) {
	m, _, rs, fbs := warmHarness([]string{"payA"})
	// Seed several remote blocks so there is work to cancel.
	for i := uint64(0); i < 8; i++ {
		seedFileBlockAt(t, fbs, rs, "payA", i*uint64(BlockSize), []byte("payA-block-bytes-"+itoa(i)))
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
