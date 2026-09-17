package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The high-word DOS attribute bits an SMB FileAttributes update flips, mirrored
// from internal/adapter/smb/handlers. They are the only bits SetAttrs.ModeOrMask
// and SetAttrs.ModeAndNotMask are allowed to move.
const (
	modeDOSExplicitBit = uint32(0x10000)
	modeDOSArchiveBit  = uint32(0x20000)
	modeDOSSystemBit   = uint32(0x80000)
	modeDOSReadonlyBit = uint32(0x100000)
)

// TestSetFileAttributes_DOSAttrMasks_LeavePermissionBitsIntact pins the contract
// the SMB FileAttributes path relies on: an update expressed through
// ModeOrMask / ModeAndNotMask moves DOS attribute bits only. The POSIX
// permission triple, the setid bits and the sticky bit must read back exactly
// as they were, whatever the file's mode happens to be — including modes that
// are wider and narrower than the 0o644 / 0o755 defaults the SMB layer
// synthesizes for a newly created file.
//
// Both masks deliberately carry permission, setid and sticky bits as well, so
// the whitelist is exercised in the set direction and the clear direction.
func TestSetFileAttributes_DOSAttrMasks_LeavePermissionBitsIntact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		fileType metadata.FileType
		mode     uint32
	}{
		{"private file", metadata.FileTypeRegular, 0o600},
		{"world-writable file", metadata.FileTypeRegular, 0o777},
		{"setuid file", metadata.FileTypeRegular, 0o4700},
		{"default-mode file", metadata.FileTypeRegular, 0o644},
		{"directory", metadata.FileTypeDirectory, 0o755},
		{"sticky private directory", metadata.FileTypeDirectory, 0o1700},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTestFixture(t)

			_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "target", &metadata.FileAttr{
				Type: tc.fileType,
				Mode: tc.mode,
			})
			require.NoError(t, err)
			handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "target")
			require.NoError(t, err)

			pre, err := fx.service.GetFile(context.Background(), handle)
			require.NoError(t, err)
			wantPOSIX := pre.Mode & 0o7777

			// Set ARCHIVE, clear SYSTEM and READONLY — the shape an SMB
			// FileAttributes update takes. The 0o7777 riders on both masks are
			// the smuggling attempt the whitelist must drop.
			or := modeDOSExplicitBit | modeDOSArchiveBit | 0o7777
			andNot := modeDOSSystemBit | modeDOSReadonlyBit | 0o7777
			_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{
				ModeOrMask:     &or,
				ModeAndNotMask: &andNot,
			})
			require.NoError(t, err)

			got, err := fx.service.GetFile(context.Background(), handle)
			require.NoError(t, err)

			assert.Equal(t, wantPOSIX, got.Mode&0o7777,
				"a DOS attribute update must not alter permission, setid or sticky bits")
			// The DOS attribute update itself must still land.
			assert.NotZero(t, got.Mode&modeDOSExplicitBit, "modeDOSExplicit must be set")
			assert.NotZero(t, got.Mode&modeDOSArchiveBit, "modeDOSArchive must be set")
			assert.Zero(t, got.Mode&modeDOSSystemBit, "modeDOSSystem must be cleared")
			assert.Zero(t, got.Mode&modeDOSReadonlyBit, "modeDOSReadonly must be cleared")
		})
	}
}

// TestSetFileAttributes_DOSAttrMasks_WithTruncateStillNeedsOwnership pins the
// boundary of the truncate-by-write-permission relaxation: truncate() takes
// write access rather than ownership, but a SetAttrs that also flips DOS
// attribute bits is not a truncate. A non-owner holding write permission must
// not reach the attribute change by bundling it with a size change — a plain
// DOS-attribute update from that caller is refused, and adding a size to it
// must not buy the caller past the ownership gate.
func TestSetFileAttributes_DOSAttrMasks_WithTruncateStillNeedsOwnership(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	owner := fx.authContext(1000, 1000)
	_, _, err := fx.service.CreateFile(owner, fx.rootHandle, "bundled", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o666,
	})
	require.NoError(t, err)
	handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "bundled")
	require.NoError(t, err)

	// A different user with write permission on the file (0o666).
	other := fx.authContext(2000, 2000)

	zero := uint64(0)
	or := modeDOSExplicitBit | modeDOSReadonlyBit
	_, err = fx.service.SetFileAttributes(other, handle, &metadata.SetAttrs{
		Size:       &zero,
		ModeOrMask: &or,
	})
	require.Error(t, err, "a non-owner must not flip DOS attribute bits by bundling them with a truncate")

	got, err := fx.service.GetFile(context.Background(), handle)
	require.NoError(t, err)
	assert.Zero(t, got.Mode&modeDOSReadonlyBit, "modeDOSReadonly must not have been set by a non-owner")

	// The same truncate without the masks is still permitted by write access.
	_, err = fx.service.SetFileAttributes(other, handle, &metadata.SetAttrs{Size: &zero})
	require.NoError(t, err, "a plain truncate is still authorized by write permission")
}
