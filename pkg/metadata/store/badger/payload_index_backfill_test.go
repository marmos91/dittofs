package badger

import (
	"context"
	"errors"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// seedLegacyFiles writes file rows straight through a raw Badger handle at dir,
// with no pl: entry — the shape of every row written before that index existed.
// The handle is closed before returning so the store can open the same
// directory.
// legacyShare is the share the seeded legacy rows belong to; the usage cache is
// keyed by share, so the assertions read that share's bucket.
const legacyShare = "/legacy"

func seedLegacyFiles(t *testing.T, dir string, payloads ...metadata.PayloadID) []*metadata.File {
	t.Helper()

	db, err := badgerdb.Open(badgerdb.DefaultOptions(dir).WithLogger(nil))
	if err != nil {
		t.Fatalf("open raw badger: %v", err)
	}
	defer func() {
		if cErr := db.Close(); cErr != nil {
			t.Fatalf("close raw badger: %v", cErr)
		}
	}()

	files := make([]*metadata.File, 0, len(payloads))
	for _, p := range payloads {
		f := &metadata.File{
			ID:        uuid.New(),
			ShareName: legacyShare,
			FileAttr: metadata.FileAttr{
				Type:      metadata.FileTypeRegular,
				PayloadID: p,
				Size:      4096,
			},
		}
		enc, encErr := encodeFile(f)
		if encErr != nil {
			t.Fatalf("encode file: %v", encErr)
		}
		if uErr := db.Update(func(txn *badgerdb.Txn) error {
			return txn.Set(keyFile(f.ID), enc)
		}); uErr != nil {
			t.Fatalf("seed file: %v", uErr)
		}
		if hasPayloadIndexAt(t, db, p) {
			t.Fatalf("%q indexed before open; the fixture is not reproducing a legacy row", p)
		}
		files = append(files, f)
	}
	return files
}

func hasPayloadIndexAt(t *testing.T, db *badgerdb.DB, payload metadata.PayloadID) bool {
	t.Helper()
	var found bool
	if err := db.View(func(txn *badgerdb.Txn) error {
		_, err := txn.Get(keyPayloadID(payload))
		switch {
		case err == nil:
			found = true
			return nil
		case errors.Is(err, badgerdb.ErrKeyNotFound):
			return nil
		default:
			return err
		}
	}); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	return found
}

func openStore(t *testing.T, dir string) *BadgerMetadataStore {
	t.Helper()
	s, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// Rows written before the index existed must be indexed at open. Without it
// every lookup for them falls back to a full keyspace scan, which a caller
// resolving payloads in a loop turns into quadratic work before any listener
// binds.
func TestOpen_IndexesLegacyRowsByPayload(t *testing.T) {
	dir := t.TempDir()
	want := seedLegacyFiles(t, dir, "payload-a", "payload-b", "payload-c")

	store := openStore(t, dir)
	defer func() { _ = store.Close() }()

	for _, f := range want {
		got, err := store.GetFileByPayloadID(context.Background(), f.PayloadID)
		if err != nil {
			t.Fatalf("GetFileByPayloadID(%q): %v", f.PayloadID, err)
		}
		if got == nil || got.ID != f.ID {
			t.Errorf("payload %q resolved to %v, want file %v", f.PayloadID, got, f.ID)
		}
		if !hasPayloadIndexAt(t, store.db, f.PayloadID) {
			t.Errorf("payload %q still unindexed after open", f.PayloadID)
		}
	}
}

// A second open must not repeat the scan. Without the marker the cost is paid on
// every restart, which for a large store is the whole problem being fixed.
func TestOpen_SkipsPayloadIndexingOnSecondOpen(t *testing.T) {
	dir := t.TempDir()
	seedLegacyFiles(t, dir, "payload-a")

	first := openStore(t, dir)
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	// Seeded after the marker was written, so it stays unindexed only if the
	// second open genuinely skipped the pass.
	seedLegacyFiles(t, dir, "payload-late")

	second := openStore(t, dir)
	defer func() { _ = second.Close() }()

	if hasPayloadIndexAt(t, second.db, "payload-late") {
		t.Fatal("second open re-indexed; the completion marker is not being honoured")
	}
}

// A row with no PayloadID has nothing to index, and must not produce an entry
// under the empty key — which would then be returned for payload-less lookups.
func TestOpen_SkipsRowsWithoutPayload(t *testing.T) {
	dir := t.TempDir()
	seedLegacyFiles(t, dir, "")

	store := openStore(t, dir)
	defer func() { _ = store.Close() }()

	if hasPayloadIndexAt(t, store.db, "") {
		t.Fatal("wrote an index entry for an empty PayloadID")
	}
}

// The usage cache shares the scan the indexing rides on, so it must still be
// seeded correctly when that indexing runs.
func TestOpen_UsedBytesStillSeededWhileIndexing(t *testing.T) {
	dir := t.TempDir()
	files := seedLegacyFiles(t, dir, "payload-a", "payload-b")

	store := openStore(t, dir)
	defer func() { _ = store.Close() }()

	var want int64
	for _, f := range files {
		want += int64(f.Size)
	}
	got, err := store.GetUsedBytesForShare(t.Context(), legacyShare)
	if err != nil {
		t.Fatalf("GetUsedBytesForShare(%q): %v", legacyShare, err)
	}
	if got != want {
		t.Fatalf("GetUsedBytesForShare(%q) = %d, want %d", legacyShare, got, want)
	}
}
