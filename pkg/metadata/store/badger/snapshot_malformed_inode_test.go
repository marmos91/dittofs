package badger_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
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

	store, _ := seedSnapshotStore(t, ctx, dir)

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

	store, rerr := badger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if rerr != nil {
		t.Fatalf("reopen store: %v", rerr)
	}
	defer func() { _ = store.Close() }()

	var buf bytes.Buffer
	if _, err := store.WriteSnapshot(ctx, &buf); !errors.Is(err, metadata.ErrSnapshotAborted) {
		t.Fatalf("WriteSnapshot err = %v, want ErrSnapshotAborted: the malformed inode was dumped verbatim but its hashes never reached the snapshot's claim", err)
	}
}

// TestWriteSnapshotDegraded_CompletesAndReports is the opt-in half. The same
// store that WriteSnapshot refuses must produce a snapshot through
// WriteSnapshotDegraded — and that snapshot must carry, in its report, both how
// many entries it left out and which ones, so a caller can label it rather than
// ship a silently short manifest.
//
// It also pins the other direction: over a clean store the degraded entrypoint
// reports nothing, so a nil report genuinely means complete.
func TestWriteSnapshotDegraded_CompletesAndReports(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "db")

	store, id := seedSnapshotStore(t, ctx, dir)

	var clean bytes.Buffer
	hs, degraded, err := store.WriteSnapshotDegraded(ctx, &clean)
	if err != nil {
		t.Fatalf("WriteSnapshotDegraded on a clean store: %v", err)
	}
	if degraded != nil {
		t.Fatalf("clean store reported degradation %+v; a nil report is what lets a caller trust a snapshot is complete", degraded)
	}
	if hs == nil {
		t.Fatal("clean store returned a nil HashSet")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wantKey := "f:" + id.String()
	clobberInodeKey(t, dir, wantKey)

	store, err = badger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store.Close() }()

	var buf bytes.Buffer
	_, degraded, err = store.WriteSnapshotDegraded(ctx, &buf)
	if err != nil {
		t.Fatalf("WriteSnapshotDegraded over an undecodable row: %v; the opt-in path must complete, not refuse", err)
	}
	if degraded == nil {
		t.Fatal("no degradation reported for an undecodable row; an unreported gap is the whole defect")
	}
	if degraded.Entries != 1 {
		t.Errorf("Entries = %d, want 1", degraded.Entries)
	}
	if len(degraded.Keys) != 1 || degraded.Keys[0] != wantKey {
		t.Errorf("Keys = %v, want [%s] — the keys are what makes the gap findable", degraded.Keys, wantKey)
	}
	if buf.Len() == 0 {
		t.Error("degraded snapshot wrote no stream")
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

// seedSnapshotStore opens a store at dir holding one regular file and returns
// the store plus that file's ID.
func seedSnapshotStore(t *testing.T, ctx context.Context, dir string) (*badger.BadgerMetadataStore, uuid.UUID) {
	t.Helper()
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
	return store, id
}

// clobberInodeKey makes one named f: value undecodable. The store must be closed.
func clobberInodeKey(t *testing.T, dir, key string) {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("reopen badger raw: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Update(func(txn *badgerdb.Txn) error {
		if _, gerr := txn.Get([]byte(key)); gerr != nil {
			return gerr
		}
		return txn.Set([]byte(key), []byte("not an inode"))
	}); err != nil {
		t.Fatalf("clobber %s: %v", key, err)
	}
}
