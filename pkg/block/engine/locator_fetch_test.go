package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLocatorFetchSyncer builds a Syncer over an in-memory local store, remote
// store, and synced-hash store — the minimal wiring dispatchRemoteFetch needs to
// resolve a chunk locator and route the read.
func newLocatorFetchSyncer(t *testing.T) (*Syncer, *remotememory.Store, *metadatamemory.MemoryMetadataStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = ms.Close() })
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = localStore.Close() })

	rem := remotememory.New()
	syncer := NewSyncer(localStore, rem, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	return syncer, rem, ms
}

// TestDispatchRemoteFetch_StandaloneRoundTrip covers the live PR3a path: a chunk
// written and marked synced with a standalone locator reads back via the direct
// CAS object (ReadBlockVerified), and the recorded locator resolves as
// standalone.
func TestDispatchRemoteFetch_StandaloneRoundTrip(t *testing.T) {
	ctx := context.Background()
	syncer, rem, ms := newLocatorFetchSyncer(t)

	data := bytes.Repeat([]byte{0xAB}, 4096)
	hash := block.ContentHash(blake3.Sum256(data))
	if err := rem.Put(ctx, hash, data); err != nil {
		t.Fatalf("remote Put: %v", err)
	}
	// The write path records a standalone locator on MarkSynced.
	if err := ms.MarkSynced(ctx, hash, block.ChunkLocator{Length: int64(len(data))}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	loc, ok, err := ms.GetLocator(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("GetLocator: ok=%v err=%v", ok, err)
	}
	if !loc.IsStandalone() {
		t.Fatalf("standalone write resolved to block: %+v", loc)
	}

	key, got, err := syncer.dispatchRemoteFetch(ctx, &block.FileBlock{Hash: hash})
	if err != nil {
		t.Fatalf("dispatchRemoteFetch: %v", err)
	}
	if key != block.FormatCASKey(hash) {
		t.Fatalf("standalone key = %q, want %q", key, block.FormatCASKey(hash))
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("standalone data mismatch")
	}
}

// TestDispatchRemoteFetch_LegacyNoLocator covers backward compatibility: a hash
// with NO recorded synced marker (a pre-#1414 fixture, or drift) still reads via
// the standalone CAS object — resolveLocator defaults to standalone.
func TestDispatchRemoteFetch_LegacyNoLocator(t *testing.T) {
	ctx := context.Background()
	syncer, rem, _ := newLocatorFetchSyncer(t)

	data := bytes.Repeat([]byte{0xCD}, 2048)
	hash := block.ContentHash(blake3.Sum256(data))
	if err := rem.Put(ctx, hash, data); err != nil {
		t.Fatalf("remote Put: %v", err)
	}
	// Deliberately NOT MarkSynced — GetLocator returns (false).

	key, got, err := syncer.dispatchRemoteFetch(ctx, &block.FileBlock{Hash: hash})
	if err != nil {
		t.Fatalf("dispatchRemoteFetch (legacy): %v", err)
	}
	if key != block.FormatCASKey(hash) {
		t.Fatalf("legacy key = %q, want standalone CAS key", key)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("legacy data mismatch")
	}
}

// TestDispatchRemoteFetch_BlockLocator covers the indirection PR3b will exploit: a
// synthetic block locator routes a ranged read into the enclosing block object and
// verifies the chunk's BLAKE3. This branch is never taken on the live PR3a path.
func TestDispatchRemoteFetch_BlockLocator(t *testing.T) {
	ctx := context.Background()
	syncer, rem, ms := newLocatorFetchSyncer(t)

	// A block with a leading filler chunk so the target sits at a non-zero
	// offset, exercising the range request.
	filler := bytes.Repeat([]byte{0x11}, 100)
	target := bytes.Repeat([]byte{0x22}, 4096)
	blockData := append(append([]byte{}, filler...), target...)
	const blockID = "block-test-0001"
	if err := rem.PutBlock(blockID, blockData); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	hash := block.ContentHash(blake3.Sum256(target))
	loc := block.ChunkLocator{BlockID: blockID, Offset: int64(len(filler)), Length: int64(len(target))}
	if err := ms.MarkSynced(ctx, hash, loc); err != nil {
		t.Fatalf("MarkSynced block: %v", err)
	}

	key, got, err := syncer.dispatchRemoteFetch(ctx, &block.FileBlock{Hash: hash})
	if err != nil {
		t.Fatalf("dispatchRemoteFetch (block): %v", err)
	}
	if key != block.FormatBlockKey(blockID) {
		t.Fatalf("block key = %q, want %q", key, block.FormatBlockKey(blockID))
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("block chunk data mismatch")
	}
}

// TestDispatchRemoteFetch_BlockLocatorVerifyMismatch proves the block read path
// fails closed when the bytes at the located range do not hash to the expected
// chunk (corruption / wrong offset).
func TestDispatchRemoteFetch_BlockLocatorVerifyMismatch(t *testing.T) {
	ctx := context.Background()
	syncer, rem, ms := newLocatorFetchSyncer(t)

	target := bytes.Repeat([]byte{0x33}, 4096)
	hash := block.ContentHash(blake3.Sum256(target))
	// Store a block whose bytes do NOT match the claimed hash.
	corrupt := bytes.Repeat([]byte{0x34}, 4096)
	const blockID = "block-corrupt"
	if err := rem.PutBlock(blockID, corrupt); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	loc := block.ChunkLocator{BlockID: blockID, Offset: 0, Length: int64(len(target))}
	if err := ms.MarkSynced(ctx, hash, loc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	_, _, err := syncer.dispatchRemoteFetch(ctx, &block.FileBlock{Hash: hash})
	if !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("block verify mismatch: got %v, want ErrCASContentMismatch", err)
	}
}
