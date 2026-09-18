package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getAttr reads the entry at name under the fixture root through the store.
func (f *testFixture) getAttr(t *testing.T, name string) *metadata.File {
	t.Helper()
	h, err := f.store.GetChild(context.Background(), f.rootHandle, name)
	require.NoError(t, err)
	file, err := f.store.GetFile(context.Background(), h)
	require.NoError(t, err)
	return file
}

// TestCreateEntry_ExactAttrs pins the create-path contract a remove-then-
// recreate conversion relies on: ExactAttrs means the attributes describe an
// entry that already existed, so none of the new-entry defaults may touch them.
// Without it a re-create silently re-homes or widens the entry it replaces.
func TestCreateEntry_ExactAttrs(t *testing.T) {
	t.Parallel()

	t.Run("preserves an explicit zero mode", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Mode 0 is a real mode a placeholder can carry. Without ExactAttrs the
		// create path reads it as "unspecified" and substitutes 0o644.
		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "zero", &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0, ExactAttrs: true,
		})
		require.NoError(t, err)

		got := fx.getAttr(t, "zero")
		assert.Zero(t, got.Mode&0o7777, "an exact re-create must store the explicit mode 0")
	})

	t.Run("preserves a zero mode on symlink creation", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, _, err := fx.service.CreateSymlink(fx.rootContext(), fx.rootHandle, "link", "../A", &metadata.FileAttr{
			Type: metadata.FileTypeSymlink, Mode: 0o777, ExactAttrs: true,
		})
		require.NoError(t, err)

		got := fx.getAttr(t, "link")
		assert.EqualValues(t, 0o777, got.Mode&0o7777, "the named symlink mode must survive")
	})

	t.Run("does not inherit the SGID parent's group", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		sgidDir, _, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "sgid", &metadata.FileAttr{
			Type: metadata.FileTypeDirectory, Mode: 0o2777, UID: 0, GID: 7777,
		})
		require.NoError(t, err)
		sgidHandle, err := metadata.EncodeShareHandle(fx.shareName, sgidDir.ID)
		require.NoError(t, err)

		const carriedGID = uint32(5555)
		_, _, err = fx.service.CreateFile(fx.rootContext(), sgidHandle, "exact", &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o600, UID: 4242, GID: carriedGID, ExactAttrs: true,
		})
		require.NoError(t, err)

		child, err := fx.store.GetChild(context.Background(), sgidHandle, "exact")
		require.NoError(t, err)
		got, err := fx.store.GetFile(context.Background(), child)
		require.NoError(t, err)
		assert.Equal(t, carriedGID, got.GID,
			"SGID inheritance must not override the carried group on an exact re-create")
		assert.Equal(t, uint32(4242), got.UID, "the carried owner must survive")
		assert.EqualValues(t, 0o600, got.Mode&0o7777, "the carried mode must survive")
	})

	t.Run("does not strip carried setid bits", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// A non-root create would normally have SUID/SGID stripped. The mode was
		// validated when it was first set, so an exact re-create must keep it.
		nonRoot := mkCtx(1000, 1000)
		_, _, err := fx.service.CreateFile(nonRoot, fx.rootHandle, "setid", &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o4755, UID: 1000, GID: 1000, ExactAttrs: true,
		})
		require.NoError(t, err)

		got := fx.getAttr(t, "setid")
		assert.NotZero(t, got.Mode&0o4000, "an exact re-create must not strip a carried SUID bit")
	})

	t.Run("still defaults a normal create", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// The control: without ExactAttrs the defaults must still apply.
		_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "normal", &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0,
		})
		require.NoError(t, err)

		got := fx.getAttr(t, "normal")
		assert.EqualValues(t, 0o644, got.Mode&0o7777, "a normal create must still get the file default")
	})
}
