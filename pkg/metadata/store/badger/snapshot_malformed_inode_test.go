package badger_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestWriteSnapshot_AbortsOnMalformedInode pins Backup's policy on an f: entry
// it cannot decode. The raw record is dumped verbatim before the hashes are
// extracted, so skipping the decode ships a snapshot whose restored store holds
// a file the snapshot's own HashSet never claimed — the durability verify then
// passes over chunks nothing is keeping alive.
//
// It is the same failure loadManifest three lines below already aborts for, and
// the two must agree: an f: entry that cannot be read makes the snapshot's hash
// claim incomplete, and an incomplete claim cannot be told apart from a wrong
// one.
func TestWriteSnapshot_AbortsOnMalformedInode(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "db")

	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	const shareName = "share"
	h, err := store.GenerateHandle(ctx, shareName, "/data.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(h)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	if err := store.UpdateAttrs(ctx, &metadata.File{
		ID: id, ShareName: shareName, Path: "/data.bin",
		FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644},
	}); err != nil {
		t.Fatalf("UpdateAttrs: %v", err)
	}

	// A healthy snapshot first, so a failure below is the corruption and not
	// the fixture.
	var ok bytes.Buffer
	if _, err := store.WriteSnapshot(ctx, &ok); err != nil {
		t.Fatalf("WriteSnapshot on a clean store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	clobberFirstInodeRow(t, dir)

	store, err = badger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store.Close() }()

	var buf bytes.Buffer
	if _, err := store.WriteSnapshot(ctx, &buf); !errors.Is(err, metadata.ErrSnapshotAborted) {
		t.Fatalf("WriteSnapshot err = %v, want ErrSnapshotAborted: the malformed inode was dumped verbatim but its hashes never reached the snapshot's claim", err)
	}
}

// clobberFirstInodeRow makes one f: value undecodable. The store must be closed.
func clobberFirstInodeRow(t *testing.T, dir string) {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("reopen badger raw: %v", err)
	}
	defer func() { _ = db.Close() }()

	var key []byte
	if err := db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = []byte("f:")
		it := txn.NewIterator(opts)
		defer it.Close()
		it.Seek(opts.Prefix)
		if !it.ValidForPrefix(opts.Prefix) {
			return errors.New("fixture seeded no inode rows")
		}
		key = it.Item().KeyCopy(nil)
		return nil
	}); err != nil {
		t.Fatalf("scan inode keys: %v", err)
	}
	if err := db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(key, []byte("not an inode"))
	}); err != nil {
		t.Fatalf("clobber %s: %v", key, err)
	}
}
