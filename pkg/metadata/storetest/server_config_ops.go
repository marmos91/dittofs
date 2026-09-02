package storetest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runServerConfigOps pins the store-wide server configuration round-trip.
//
// The settings deliberately carry a string, a number and a nested object: a
// backend that stores the config as opaque bytes and one that stores it as a
// structured column disagree about what survives, and only a value with shape
// can tell them apart. Numbers come back as float64 because that is what
// encoding/json produces for an untyped number, which every backend must match
// whether or not it round-trips through JSON internally.
func runServerConfigOps(t *testing.T, factory StoreFactory) {
	t.Helper()

	want := map[string]any{
		"feature": "on",
		"n":       float64(7),
		"nested":  map[string]any{"deep": "value"},
	}

	t.Run("PoolRoundTrip", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		require.NoError(t, store.SetServerConfig(ctx, metadata.MetadataServerConfig{CustomSettings: want}))

		got, err := store.GetServerConfig(ctx)
		require.NoError(t, err)
		require.Equal(t, want, got.CustomSettings)
	})

	t.Run("TxWriteIsVisibleToPool", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			return tx.SetServerConfig(ctx, metadata.MetadataServerConfig{CustomSettings: want})
		}))

		var inTx metadata.MetadataServerConfig
		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			var err error
			inTx, err = tx.GetServerConfig(ctx)
			return err
		}))
		require.Equal(t, want, inTx.CustomSettings)

		got, err := store.GetServerConfig(ctx)
		require.NoError(t, err)
		require.Equal(t, want, got.CustomSettings)
	})

	t.Run("PoolWriteIsVisibleInTx", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		// The mirror of the case above, and the one that matters for a backend
		// whose two paths encode the column differently: a pool write that the
		// transaction path cannot decode would pass every assertion that only
		// ever reads back what the same path wrote.
		require.NoError(t, store.SetServerConfig(ctx, metadata.MetadataServerConfig{CustomSettings: want}))

		var inTx metadata.MetadataServerConfig
		require.NoError(t, store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			var err error
			inTx, err = tx.GetServerConfig(ctx)
			return err
		}))
		require.Equal(t, want, inTx.CustomSettings)
	})

	t.Run("ReadIsNotAliasedToStoreState", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		// A backend that decodes the config per read hands back a fresh map; an
		// in-memory one can hand back the live one, and then a caller that reads
		// the config, sets a key and writes it back — which the block-GC
		// reconcile marker does — mutates store state before the write and
		// outside whatever lock the read held.
		require.NoError(t, store.SetServerConfig(ctx, metadata.MetadataServerConfig{
			CustomSettings: map[string]any{"kept": "yes"},
		}))

		got, err := store.GetServerConfig(ctx)
		require.NoError(t, err)
		got.CustomSettings["scribbled"] = true

		again, err := store.GetServerConfig(ctx)
		require.NoError(t, err)
		require.NotContains(t, again.CustomSettings, "scribbled",
			"mutating a read config changed store state without a write")
	})

	t.Run("UnwrittenConfigIsEmptyNotMissing", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		got, err := store.GetServerConfig(ctx)
		require.NoError(t, err, "a store that has never had a config written must not report an error")
		require.NotNil(t, got.CustomSettings, "settings must be an empty map, not nil")
		require.Empty(t, got.CustomSettings)
	})
}
