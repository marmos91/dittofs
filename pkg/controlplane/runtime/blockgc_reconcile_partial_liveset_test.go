package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestReapStrandedRows_RefusesUnreadableLinkCount covers the sibling row. The
// link count lives in l:<uuid>, not in the inode record, and an l: value that
// will not decode says nothing about whether the file is linked. Reading it as
// zero would classify a live file as unlinked — dead — which drops its payload
// from the live set just as surely as an undecodable inode does, and the reaper
// deletes what the live set omits.
//
// Unlike the inode case there is no error to raise here: alive is a safe answer
// for every consumer, so the scan simply must not conclude dead.
func TestReapStrandedRows_RefusesUnreadableLinkCount(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "db")
	old := time.Now().Add(-2 * time.Hour)

	store, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}

	const livePID = "live"
	h, err := store.GenerateHandle(ctx, "share", "/live.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(h)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	if err := store.UpdateAttrs(ctx, &metadata.File{
		ID:        id,
		ShareName: "share",
		Path:      "/live.bin",
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			PayloadID: metadata.PayloadID(livePID),
		},
	}); err != nil {
		t.Fatalf("UpdateAttrs: %v", err)
	}
	if err := store.SetLinkCount(ctx, h, 1); err != nil {
		t.Fatalf("SetLinkCount: %v", err)
	}

	seedReconcileFB(t, ctx, store, livePID, 2, old)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	clobberRows(t, dir, "l:")

	store, err = metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rt := &Runtime{}
	reaped, err := rt.reapStrandedRows(ctx, "share", store, time.Now().Add(-time.Hour), false)
	if err != nil {
		t.Fatalf("reapStrandedRows: %v", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0", reaped)
	}
	rows, lerr := store.ListFileChunks(ctx, livePID)
	if lerr != nil {
		t.Fatalf("ListFileChunks(%s): %v", livePID, lerr)
	}
	if len(rows) != 2 {
		t.Fatalf("live payload rows = %d, want 2; an undecodable l: row was read as nlink=0, so a linked file was reaped as unlinked", len(rows))
	}
}

// clobberRows replaces every value under prefix with bytes no codec can decode,
// the shape a partial write or a disk-level corruption leaves behind. The store
// must be closed first: badger holds a directory lock.
func clobberRows(t *testing.T, dir, prefix string) int {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("reopen badger raw: %v", err)
	}
	defer func() { _ = db.Close() }()

	var keys [][]byte
	if err := db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = []byte(prefix)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(opts.Prefix); it.ValidForPrefix(opts.Prefix); it.Next() {
			keys = append(keys, it.Item().KeyCopy(nil))
		}
		return nil
	}); err != nil {
		t.Fatalf("scan %q keys: %v", prefix, err)
	}
	if len(keys) == 0 {
		t.Fatalf("fixture seeded no %q rows", prefix)
	}
	if err := db.Update(func(txn *badgerdb.Txn) error {
		for _, k := range keys {
			if err := txn.Set(k, []byte("undecodable")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("clobber %q rows: %v", prefix, err)
	}
	return len(keys)
}

// TestReapStrandedRows_RefusesPartialLiveSet pins the rule at the destructive
// consumer: the live set is built by a scan that skips namespace rows it cannot
// decode, and the reaper deletes exactly what that set omits. A payload whose
// only inode is unreadable is therefore absent from the set through no fault of
// its own, and reaping on it destroys a live file's manifest.
//
// The scan's tolerance is deliberate and stays — one bad row must not block
// reclaiming everything else. What must not follow from it is authority to
// delete. This runs with dryRun=false in a goroutine detached from shutdown at
// every boot, so the refusal is the only thing standing in the way.
func TestReapStrandedRows_RefusesPartialLiveSet(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "db")
	old := time.Now().Add(-2 * time.Hour)

	store, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}

	const livePID = "live"
	h, err := store.GenerateHandle(ctx, "share", "/live.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(h)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	if err := store.UpdateAttrs(ctx, &metadata.File{
		ID:        id,
		ShareName: "share",
		Path:      "/live.bin",
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0o644,
			PayloadID: metadata.PayloadID(livePID),
		},
	}); err != nil {
		t.Fatalf("UpdateAttrs: %v", err)
	}

	seedReconcileFB(t, ctx, store, livePID, 2, old)
	seedReconcileFB(t, ctx, store, "stranded", 3, old)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	clobberRows(t, dir, "f:")

	store, err = metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rt := &Runtime{}
	reaped, err := rt.reapStrandedRows(ctx, "share", store, time.Now().Add(-time.Hour), false)
	if !errors.Is(err, metadata.ErrLiveSetIncomplete) {
		t.Errorf("reapStrandedRows err = %v, want ErrLiveSetIncomplete: the live set omits every payload whose inode could not be decoded, so it cannot authorize a reap", err)
	}
	if reaped != 0 {
		t.Errorf("reaped = %d, want 0", reaped)
	}

	// The live payload's rows are the ones the incomplete set would have
	// destroyed: its inode exists, it is simply unreadable.
	rows, lerr := store.ListFileChunks(ctx, livePID)
	if lerr != nil {
		t.Fatalf("ListFileChunks(%s): %v", livePID, lerr)
	}
	if len(rows) != 2 {
		t.Fatalf("live payload rows = %d, want 2; the reconcile reaped a live file's manifest because its inode would not decode", len(rows))
	}
}
