package badger

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
)

// stampFormatVersion writes fmt:store directly, standing in for the database a
// different build would have left behind. The store must be closed first —
// Badger holds a directory lock.
func stampFormatVersion(t *testing.T, dbPath string, version uint32) {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(dbPath).WithLogger(nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], version)
	require.NoError(t, db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set([]byte(formatVersionKey), buf[:])
	}))
}

// readFormatVersion reads fmt:store directly, reporting whether it exists.
func readFormatVersion(t *testing.T, dbPath string) (uint32, bool) {
	t.Helper()
	db, err := badgerdb.Open(badgerdb.DefaultOptions(dbPath).WithLogger(nil))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	var stored uint32
	var found bool
	require.NoError(t, db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get([]byte(formatVersionKey))
		if errors.Is(err, badgerdb.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return item.Value(func(v []byte) error {
			stored = binary.BigEndian.Uint32(v)
			return nil
		})
	}))
	return stored, found
}

// TestFormatVersion_UnstampedOpensAndStamps pins that a database written before
// stamping existed still opens, and comes out of that open guarded.
func TestFormatVersion_UnstampedOpensAndStamps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	stored, found := readFormatVersion(t, dbPath)
	require.True(t, found, "open must stamp the format version")
	require.Equal(t, storeFormatVersion, stored)
}

// TestFormatVersion_CurrentOpens pins that reopening a database this build
// itself stamped is not mistaken for a future format.
func TestFormatVersion_CurrentOpens(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	reopened, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

// TestFormatVersion_FutureRefusesOpen is the downgrade case: a database stamped
// by a newer release must fail the open with block.ErrFutureFormat rather than
// serve whatever this build can still decode.
func TestFormatVersion_FutureRefusesOpen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	stampFormatVersion(t, dbPath, storeFormatVersion+1)

	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	require.Error(t, err)
	require.True(t, errors.Is(err, block.ErrFutureFormat), "got %v", err)
	require.Nil(t, store)
}
