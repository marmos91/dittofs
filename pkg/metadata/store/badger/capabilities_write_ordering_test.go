package badger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetFilesystemCapabilities_FailedPersistLeavesInMemoryValue pins the write
// ordering of BadgerMetadataStore.SetFilesystemCapabilities: the in-memory copy
// that GetFilesystemMeta reports must only advance once the write has actually
// committed. Closing the store makes every subsequent write fail, standing in
// for any persist failure.
func TestSetFilesystemCapabilities_FailedPersistLeavesInMemoryValue(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	store, err := NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)

	const committed = uint64(1 << 20)
	caps := store.loadCapabilities()
	caps.MaxFileSize = committed
	store.SetFilesystemCapabilities(caps)
	require.Equal(t, committed, store.loadCapabilities().MaxFileSize,
		"a capability write that commits must update the in-memory copy")

	require.NoError(t, store.Close())

	caps.MaxFileSize = 1 << 30
	store.SetFilesystemCapabilities(caps)

	require.Equal(t, committed, store.loadCapabilities().MaxFileSize,
		"a capability write that never reached the store must not update the in-memory copy")
}
