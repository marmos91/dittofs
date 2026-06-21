package metadata_test

import (
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// TestServiceXattrRoundTrip exercises the metadata Service xattr accessors
// (the scaffolding backing the NFSv4.2 / RFC 8276 handlers) against a real
// memory store: set, get, list, remove. EA storage is shared with SMB, so names
// round-trip case-insensitively.
func TestServiceXattrRoundTrip(t *testing.T) {
	fx := newTestFixture(t)
	ctx := fx.rootContext()

	file, _, err := fx.service.CreateFile(ctx, fx.rootHandle, "x.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	require.NoError(t, err)
	handle, err := metadata.EncodeFileHandle(file)
	require.NoError(t, err)

	// Missing xattr.
	_, found, err := fx.service.GetXattr(ctx, handle, "user.foo")
	require.NoError(t, err)
	require.False(t, found)

	// Set + get.
	require.NoError(t, fx.service.SetXattr(ctx, handle, "user.foo", []byte("bar")))
	val, found, err := fx.service.GetXattr(ctx, handle, "user.foo")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "bar", string(val))

	// Returned value is a copy (mutating it must not corrupt the store).
	val[0] = 'X'
	val2, _, err := fx.service.GetXattr(ctx, handle, "user.foo")
	require.NoError(t, err)
	require.Equal(t, "bar", string(val2))

	// Second name + list.
	require.NoError(t, fx.service.SetXattr(ctx, handle, "user.baz", []byte("qux")))
	names, err := fx.service.ListXattr(ctx, handle)
	require.NoError(t, err)
	sort.Strings(names)
	require.Equal(t, []string{"user.baz", "user.foo"}, names)

	// Remove.
	require.NoError(t, fx.service.RemoveXattr(ctx, handle, "user.foo"))
	_, found, err = fx.service.GetXattr(ctx, handle, "user.foo")
	require.NoError(t, err)
	require.False(t, found)

	names, err = fx.service.ListXattr(ctx, handle)
	require.NoError(t, err)
	require.Equal(t, []string{"user.baz"}, names)
}

// TestServiceXattrCaseInsensitive verifies the EA-name case-insensitivity
// (MS-FSCC §2.4.15) shared with SMB carries over to the xattr accessors.
func TestServiceXattrCaseInsensitive(t *testing.T) {
	fx := newTestFixture(t)
	ctx := fx.rootContext()

	file, _, err := fx.service.CreateFile(ctx, fx.rootHandle, "ci.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	require.NoError(t, err)
	handle, err := metadata.EncodeFileHandle(file)
	require.NoError(t, err)

	require.NoError(t, fx.service.SetXattr(ctx, handle, "user.Foo", []byte("v")))
	_, found, err := fx.service.GetXattr(ctx, handle, "user.foo")
	require.NoError(t, err)
	require.True(t, found, "GetXattr must resolve case-insensitively")
}
