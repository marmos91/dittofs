package engine

import (
	"context"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// openChunkStoreAt opens (or reopens) a badger metadata store at dir and joins
// it to the test's cleanup.
func openChunkStoreAt(t *testing.T, dir string) *metadatabadger.BadgerMetadataStore {
	t.Helper()
	ms, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(context.Background(), dir)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	return ms
}

// clobberChunkRowValue replaces the primary fb:{blockID} value with bytes that
// are not a FileChunk. The store must be closed: badger holds a directory lock.
// This is the shape a partial write, a disk-level corruption or an encoding the
// reader does not understand leaves behind — the row is present, its value is
// not readable.
func clobberChunkRowValue(t *testing.T, dir, blockID string) {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("reopen badger raw: %v", err)
	}
	defer func() { _ = db.Close() }()
	key := []byte("fb:" + blockID)
	if err := db.Update(func(txn *badgerdb.Txn) error {
		if _, gerr := txn.Get(key); gerr != nil {
			return gerr
		}
		return txn.Set(key, []byte("not a file chunk"))
	}); err != nil {
		t.Fatalf("clobber %s: %v", key, err)
	}
}

// TestDataExtents_UndecodableManifestRowIsNotAHole pins the hole-map invariant
// at its consumer: a manifest row whose value will not decode leaves the range
// it claims UNDETERMINED, and an undetermined range must be reported as data,
// never narrowed to a hole.
//
// The direction is what matters. A hole ends the inquiry — READ_PLUS emits it
// straight from the segment list, as zeros, without ever asking the block store
// — so a row dropped on the way out of ListFileChunks turns live bytes into
// zeros on the wire. Data only costs a READ, which refuses with a real error if
// the bytes really are unreadable.
//
// Returning an error instead would be the same narrowing in disguise: both NFS
// callers ignore the error and fall back to the inode's CAS block list, which
// cannot see pre-rollup local bytes at all.
func TestDataExtents_UndecodableManifestRowIsNotAHole(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "db")

	const payloadID = "share/file.bin"
	const rowSize = 4096
	const fileSize = uint64(2 * rowSize)

	var hash block.ContentHash
	hash[0] = 0xAB

	ms := openChunkStoreAt(t, dir)
	row := &metadata.FileChunk{
		ID:       payloadID + "/0",
		Hash:     hash,
		DataSize: rowSize,
		State:    block.BlockStateRemote,
	}
	if err := ms.Put(ctx, row); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ms.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	clobberChunkRowValue(t, dir, row.ID)

	ms = openChunkStoreAt(t, dir)
	t.Cleanup(func() { _ = ms.Close() })

	localStore := memorylocal.New()
	rs := remotememory.New()
	syncer := NewRemoteSync(localStore, rs, ms, DefaultConfig())
	syncer.SetSyncedHashStore(ms)
	syncer.SetRemoteBlockStore(rs)
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          rs,
		RemoteSync:      syncer,
		FileChunkStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	ext, derr := bs.DataExtents(ctx, payloadID, fileSize)
	if derr != nil {
		t.Fatalf("DataExtents: %v; an error is the narrowing answer here — "+
			"both callers drop it and fall back to a strictly narrower map", derr)
	}

	// The row claims [0, rowSize). No segment over that range may be a hole.
	for _, seg := range block.SegmentsExtents(ext, fileSize) {
		if seg.Kind != block.SegmentHole {
			continue
		}
		if seg.Start < rowSize && seg.End > 0 {
			t.Fatalf("DataExtents = %v -> hole [%d, %d) overlaps the undecodable row's range [0, %d); "+
				"the row was dropped from the manifest, so READ_PLUS emits zeros over live bytes",
				ext, seg.Start, seg.End, rowSize)
		}
	}
}
