package storetest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// RunBlockLayoutSuite asserts that a metadata.MetadataStore round-trips
// ShareOptions.BlockLayout across CreateShare / GetShareOptions /
// UpdateShareOptions. Backends invoke this from their per-backend test
// file. Conformance gate for (D-A6) — every metadata backend
// MUST persist the per-share block_layout flag so the dual-read shim
// can route per-share during the v0.13/v0.14 → v0.15 migration window.
//
// Scenarios:
//
//   - RoundTripCASOnly         — explicit cas-only survives Create+Get.
//   - RoundTripLegacy          — explicit legacy survives Create+Get.
//   - DefaultLegacyOnEmpty     — zero-value coerces to legacy on read
//
// (D-A6 safe default rows pass this path).
//   - UpdateLegacyToCASOnly    — UpdateShareOptions flips the flag,
//     observable via the next GetShareOptions call (this is how the
//     migration tool's auto-cutover works, D-A7).
//   - UpdateCASOnlyToLegacy    — symmetric round-trip; defensive
//     coverage even though production migration is forward-only.
func RunBlockLayoutSuite(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("RoundTripCASOnly", func(t *testing.T) {
		store := factory(t)
		_ = createBlockLayoutShare(t, store, "/cas-only-share", metadata.BlockLayoutCASOnly)

		got, err := store.GetShareOptions(t.Context(), "/cas-only-share")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, metadata.BlockLayoutCASOnly, got.BlockLayout)
	})

	t.Run("RoundTripLegacy", func(t *testing.T) {
		store := factory(t)
		_ = createBlockLayoutShare(t, store, "/legacy-share", metadata.BlockLayoutLegacy)

		got, err := store.GetShareOptions(t.Context(), "/legacy-share")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, metadata.BlockLayoutLegacy, got.BlockLayout)
	})

	t.Run("DefaultLegacyOnEmpty", func(t *testing.T) {
		store := factory(t)
		// Zero-value BlockLayout (i.e. caller never set the field) —
		// mirrors a share row that lacks the column.
		_ = createBlockLayoutShare(t, store, "/empty-share", "")

		got, err := store.GetShareOptions(t.Context(), "/empty-share")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, metadata.BlockLayoutLegacy, got.BlockLayout,
			"empty BlockLayout must coerce to legacy (D-A6)")
	})

	t.Run("UpdateLegacyToCASOnly", func(t *testing.T) {
		store := factory(t)
		_ = createBlockLayoutShare(t, store, "/upgrade-share", metadata.BlockLayoutLegacy)

		// Flip to cas-only — same code path the migration tool uses
		// after the integrity check passes (D-A7).
		updated := &metadata.ShareOptions{BlockLayout: metadata.BlockLayoutCASOnly}
		require.NoError(t, store.UpdateShareOptions(t.Context(), "/upgrade-share", updated))

		got, err := store.GetShareOptions(t.Context(), "/upgrade-share")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, metadata.BlockLayoutCASOnly, got.BlockLayout)
	})

	t.Run("UpdateCASOnlyToLegacy", func(t *testing.T) {
		store := factory(t)
		_ = createBlockLayoutShare(t, store, "/downgrade-share", metadata.BlockLayoutCASOnly)

		// Symmetric round-trip — production migration is forward-only
		// (D-A8 no-rollback) but the field itself must be symmetric so
		// the metadata layer doesn't impose a one-way constraint that
		// the test/dev tooling can't undo.
		updated := &metadata.ShareOptions{BlockLayout: metadata.BlockLayoutLegacy}
		require.NoError(t, store.UpdateShareOptions(t.Context(), "/downgrade-share", updated))

		got, err := store.GetShareOptions(t.Context(), "/downgrade-share")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, metadata.BlockLayoutLegacy, got.BlockLayout)
	})
}

// createBlockLayoutShare creates a share with the supplied BlockLayout
// and (for backends that require it — Postgres) the matching root
// directory so subsequent GetShareOptions / UpdateShareOptions land on
// a real row. Returns the root handle in case a future scenario needs
// it; current scenarios discard it.
func createBlockLayoutShare(
	t *testing.T,
	store metadata.MetadataStore,
	shareName string,
	layout metadata.BlockLayout,
) metadata.FileHandle {
	t.Helper()

	ctx := t.Context()

	// Order mirrors storetest.createTestShare: CreateShare first
	// (writes ShareOptions including BlockLayout), then
	// CreateRootDirectory (materializes / wires the root inode).
	// Backends differ in how this composes:
	//   - Memory writes Options on CreateShare and never touches the
	//     share row again on CreateRootDirectory.
	//   - Badger writes Options on CreateShare; CreateRootDirectory's
	//     createNewRoot now READS the existing share row and
	//     preserves Options before re-writing the row with the new
	// RootHandle (fix; pre-fix it wiped Options).
	//   - Postgres CreateShare INSERTs share_name+options+block_layout
	//     into the shares table; CreateRootDirectory ON CONFLICT
	//     UPDATEs root_file_id only, leaving block_layout intact.
	share := &metadata.Share{
		Name: shareName,
		Options: metadata.ShareOptions{
			BlockLayout: layout,
		},
	}
	require.NoError(t, store.CreateShare(ctx, share))

	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	require.NoError(t, err)

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)
	return rootHandle
}
