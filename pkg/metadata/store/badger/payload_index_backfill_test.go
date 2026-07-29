package badger

import (
	"context"
	"errors"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// openRawBadger gives a bare DB so a store can be seeded with rows that predate
// the pl: index, which the normal constructor would index on the way in.
func openRawBadger(t *testing.T) *badgerdb.DB {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedLegacyFile writes a file row with no pl: entry — the shape of every row
// written before the index existed.
func seedLegacyFile(t *testing.T, db *badgerdb.DB, payload metadata.PayloadID) *metadata.File {
	t.Helper()
	f := &metadata.File{
		ID: uuid.New(),
		FileAttr: metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			PayloadID: payload,
			Size:      4096,
		},
	}
	enc, err := encodeFile(f)
	if err != nil {
		t.Fatalf("encode file: %v", err)
	}
	if err := db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(keyFile(f.ID), enc)
	}); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	return f
}

func hasPayloadIndex(t *testing.T, db *badgerdb.DB, payload metadata.PayloadID) bool {
	t.Helper()
	var found bool
	err := db.View(func(txn *badgerdb.Txn) error {
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
	})
	if err != nil {
		t.Fatalf("probe index: %v", err)
	}
	return found
}

// Rows written before the index existed must get an entry, or every lookup for
// them falls back to a full keyspace scan — which a caller resolving payloads in
// a loop turns into quadratic work before any listener binds.
func TestBackfillPayloadIndex_IndexesLegacyRows(t *testing.T) {
	db := openRawBadger(t)
	ctx := context.Background()

	want := []metadata.PayloadID{"payload-a", "payload-b", "payload-c"}
	for _, p := range want {
		seedLegacyFile(t, db, p)
		if hasPayloadIndex(t, db, p) {
			t.Fatalf("%s indexed before backfill; the fixture is not reproducing a legacy row", p)
		}
	}

	if err := backfillPayloadIndex(ctx, db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	for _, p := range want {
		if !hasPayloadIndex(t, db, p) {
			t.Errorf("%s still unindexed after backfill", p)
		}
	}
}

// The entry must point at the row it was derived from, otherwise the index
// resolves to the wrong file and the lookup silently returns another file's
// metadata.
func TestBackfillPayloadIndex_EntryResolvesToItsFile(t *testing.T) {
	db := openRawBadger(t)

	f := seedLegacyFile(t, db, "payload-a")
	seedLegacyFile(t, db, "payload-b")

	if err := backfillPayloadIndex(context.Background(), db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var got []byte
	if err := db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyPayloadID("payload-a"))
		if err != nil {
			return err
		}
		got, err = item.ValueCopy(nil)
		return err
	}); err != nil {
		t.Fatalf("read index: %v", err)
	}

	wantID, err := f.ID.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal id: %v", err)
	}
	if string(got) != string(wantID) {
		t.Fatalf("index for payload-a points at %x, want %x", got, wantID)
	}
}

// A second open must not re-scan. Without the marker the cost is paid on every
// restart, which for a large store is the whole problem this is fixing.
func TestBackfillPayloadIndex_SkipsSecondRun(t *testing.T) {
	db := openRawBadger(t)
	ctx := context.Background()

	seedLegacyFile(t, db, "payload-a")
	if err := backfillPayloadIndex(ctx, db); err != nil {
		t.Fatalf("first backfill: %v", err)
	}

	// A row seeded after the marker is written stays unindexed, which is how the
	// test observes that the second call did not scan.
	seedLegacyFile(t, db, "payload-late")
	if err := backfillPayloadIndex(ctx, db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if hasPayloadIndex(t, db, "payload-late") {
		t.Fatal("second backfill re-scanned; the completion marker is not being honoured")
	}
}

// A row with no PayloadID has nothing to index, and must not produce an entry
// under the empty key — which would then be returned for every payload-less
// lookup.
func TestBackfillPayloadIndex_SkipsRowsWithoutPayload(t *testing.T) {
	db := openRawBadger(t)

	seedLegacyFile(t, db, "")

	if err := backfillPayloadIndex(context.Background(), db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if hasPayloadIndex(t, db, "") {
		t.Fatal("wrote an index entry for an empty PayloadID")
	}
}
