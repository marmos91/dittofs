package badger

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// countRetiredObjectIDKeys returns how many keys of the retired ObjectID index
// the database still holds.
func countRetiredObjectIDKeys(t *testing.T, db *badgerdb.DB) int {
	t.Helper()
	prefix := []byte(prefixRetiredObjectID)
	n := 0
	require.NoError(t, db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			n++
		}
		return nil
	}))
	return n
}

// writeLegacyObjectIDState makes a store look like an older binary wrote it:
// one obj: key pointing at the file, and the file's record carrying field 21.
func writeLegacyObjectIDState(t *testing.T, store *BadgerMetadataStore, id uuid.UUID) {
	t.Helper()
	require.NoError(t, store.db.Update(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyFile(id))
		if err != nil {
			return err
		}
		val, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		oid := make([]byte, 32)
		oid[0] = 0x2a
		if err := txn.Set(keyFile(id), putField(val, fObjectID, oid)); err != nil {
			return err
		}
		idBin, err := id.MarshalBinary()
		if err != nil {
			return err
		}
		return txn.Set([]byte(fmt.Sprintf("%s%064x", prefixRetiredObjectID, 0x2a)), idBin)
	}))
	require.Equal(t, 1, countRetiredObjectIDKeys(t, store.db))
}

// The cleanup deletes every leftover obj: key, across more than one batch, and
// touches no other key. A second run finds nothing.
func TestDropRetiredObjectIDIndex(t *testing.T) {
	db, err := badgerdb.Open(badgerdb.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	const leftover = 2*retiredObjectIDDeleteBatch + 500
	others := map[string]string{"f:keep": "file", "pl:keep": "payload", "objects:keep": "not the obj: prefix"}
	require.NoError(t, db.Update(func(txn *badgerdb.Txn) error {
		for i := 0; i < leftover; i++ {
			if err := txn.Set([]byte(fmt.Sprintf("%s%064x", prefixRetiredObjectID, i)), []byte("id")); err != nil {
				return err
			}
		}
		for k, v := range others {
			if err := txn.Set([]byte(k), []byte(v)); err != nil {
				return err
			}
		}
		return nil
	}))

	n, err := dropRetiredObjectIDIndex(db)
	require.NoError(t, err)
	assert.Equal(t, leftover, n)
	assert.Zero(t, countRetiredObjectIDKeys(t, db), "every obj: key must be gone")

	require.NoError(t, db.View(func(txn *badgerdb.Txn) error {
		for k, want := range others {
			item, err := txn.Get([]byte(k))
			require.NoError(t, err, "key %q must survive", k)
			got, err := item.ValueCopy(nil)
			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		}
		return nil
	}))

	n, err = dropRetiredObjectIDIndex(db)
	require.NoError(t, err)
	assert.Zero(t, n, "a store with no obj: key left needs no work")
}

// A store written by an older binary holds obj: keys and file records that
// carry the ObjectID field. Opening it with this binary drops the keys and still reads
// every file.
func TestOpenDropsRetiredObjectIDIndex(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	const share = "/legacy"

	store, err := NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)

	writeLegacyObjectIDState(t, store, root.ID)
	require.NoError(t, store.Close())

	store, err = NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	assert.Zero(t, countRetiredObjectIDKeys(t, store.db), "open must drop the retired index")

	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)
	got, err := store.GetFile(ctx, rootHandle)
	require.NoError(t, err, "a record carrying field 21 must still be readable")
	assert.Equal(t, root.ID, got.ID)
	assert.Equal(t, metadata.FileTypeDirectory, got.Type)
}

// A dump taken by an older binary carries obj: keys and file records with the
// ObjectID field. Restoring it drops the keys right away, without waiting for
// the next open, and every file still reads.
func TestRestoreDropsRetiredObjectIDIndex(t *testing.T) {
	ctx := context.Background()
	const share = "/legacy"

	src, err := NewBadgerMetadataStoreWithDefaults(ctx, filepath.Join(t.TempDir(), "metadata.db"))
	require.NoError(t, err)
	root, err := src.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	writeLegacyObjectIDState(t, src, root.ID)

	var dump bytes.Buffer
	_, err = src.WriteSnapshot(ctx, &dump)
	require.NoError(t, err)
	require.NoError(t, src.Close())

	dst, err := NewBadgerMetadataStoreWithDefaults(ctx, filepath.Join(t.TempDir(), "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = dst.Close() })

	require.NoError(t, dst.RestoreSnapshot(ctx, bytes.NewReader(dump.Bytes())))

	assert.Zero(t, countRetiredObjectIDKeys(t, dst.db), "restore must drop the retired index")

	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)
	got, err := dst.GetFile(ctx, rootHandle)
	require.NoError(t, err, "a restored record carrying field 21 must still be readable")
	assert.Equal(t, root.ID, got.ID)
	assert.Equal(t, metadata.FileTypeDirectory, got.Type)
}
