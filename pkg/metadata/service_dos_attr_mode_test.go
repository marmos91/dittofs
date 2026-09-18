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

			// CreateFile hardcodes FileTypeRegular and ignores attr.Type, so a
			// directory case has to go through CreateDirectory to be a
			// directory at all.
			attr := &metadata.FileAttr{Type: tc.fileType, Mode: tc.mode}
			var err error
			if tc.fileType == metadata.FileTypeDirectory {
				_, _, err = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "target", attr)
			} else {
				_, _, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "target", attr)
			}
			require.NoError(t, err)
			handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "target")
			require.NoError(t, err)

			pre, err := fx.service.GetFile(context.Background(), handle)
			require.NoError(t, err)
			require.Equal(t, tc.fileType, pre.Type, "test subject is not the type the case names")
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

// TestSetFileAttributes_DOSAttrMasks_NonOwnerRefusedWithoutTruncate is the
// companion to the bundled-truncate case: a DOS-attribute update on its own is
// ownership-gated, so a non-owner holding write permission is refused. This is
// the premise the bundled case rests on — without it, that test would pass for
// the wrong reason.
func TestSetFileAttributes_DOSAttrMasks_NonOwnerRefusedWithoutTruncate(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	owner := fx.authContext(1000, 1000)
	_, _, err := fx.service.CreateFile(owner, fx.rootHandle, "attronly", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o666,
	})
	require.NoError(t, err)
	handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "attronly")
	require.NoError(t, err)

	other := fx.authContext(2000, 2000)
	or := modeDOSExplicitBit | modeDOSReadonlyBit
	_, err = fx.service.SetFileAttributes(other, handle, &metadata.SetAttrs{ModeOrMask: &or})
	require.Error(t, err, "a DOS attribute change is ownership-gated")

	// And a timestamp bump must not buy the caller past it either — the
	// relaxation for UTIME_NOW is keyed on the same ownership test.
	_, err = fx.service.SetFileAttributes(other, handle, &metadata.SetAttrs{
		ModeOrMask: &or,
		MtimeNow:   true,
	})
	require.Error(t, err, "bundling a timestamp bump must not admit a DOS attribute change")

	got, err := fx.service.GetFile(context.Background(), handle)
	require.NoError(t, err)
	assert.Zero(t, got.Mode&modeDOSReadonlyBit, "modeDOSReadonly must not have been set by a non-owner")

	// The owner is still able to make the same change.
	_, err = fx.service.SetFileAttributes(owner, handle, &metadata.SetAttrs{ModeOrMask: &or})
	require.NoError(t, err, "the owner must still be able to set DOS attributes")
}

// TestSetFileAttributes_AbsoluteMode_PreservesDOSAttrBits pins the mirror of
// the mask contract above: the absolute-Mode path (an NFS chmod) owns the POSIX
// permission bits and must carry the high word across untouched. The two halves
// of the Mode field are written by different protocols through different
// fields, so neither may overwrite what the other manages.
//
// The chmod writes exactly the mode an NFS client observes: GETATTR masks the
// high word off, so a client reading the mode and writing the same value back
// — chmod --reference, cp -p, rsync -p — believes it is a no-op. Clearing the
// high word there would destroy SMB attribute state the client never addressed,
// including the FSCTL-managed COMPRESSED and SPARSE bits and the READONLY bit
// the permission model enforces writes against.
func TestSetFileAttributes_AbsoluteMode_PreservesDOSAttrBits(t *testing.T) {
	t.Parallel()

	// Every DOS attribute bit a chmod must not disturb, FSCTL-managed included.
	allDOSBits := uint32(0x10000 | 0x20000 | 0x40000 | 0x80000 | 0x100000 | 0x200000)

	cases := []struct {
		name     string
		fileType metadata.FileType
		mode     uint32
	}{
		{"private file", metadata.FileTypeRegular, 0o600},
		{"world-writable file", metadata.FileTypeRegular, 0o777},
		{"setuid file", metadata.FileTypeRegular, 0o4700},
		{"directory", metadata.FileTypeDirectory, 0o755},
		{"sticky private directory", metadata.FileTypeDirectory, 0o1700},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTestFixture(t)

			attr := &metadata.FileAttr{Type: tc.fileType, Mode: tc.mode}
			var err error
			if tc.fileType == metadata.FileTypeDirectory {
				_, _, err = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "target", attr)
			} else {
				_, _, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "target", attr)
			}
			require.NoError(t, err)

			handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "target")
			require.NoError(t, err)

			// Set the DOS bits the way the SMB path does — through the masks.
			zero := uint32(0)
			_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{
				ModeOrMask:     &allDOSBits,
				ModeAndNotMask: &zero,
			})
			require.NoError(t, err)

			before, err := fx.store.GetFile(context.Background(), handle)
			require.NoError(t, err)
			require.Equal(t, allDOSBits, before.Mode&allDOSBits, "precondition: DOS bits stored")

			// An absolute-mode write carrying no DOS bits at all.
			newMode := tc.mode ^ 0o111 // flip the execute bits
			_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{Mode: &newMode})
			require.NoError(t, err)

			after, err := fx.store.GetFile(context.Background(), handle)
			require.NoError(t, err)
			assert.Equal(t, newMode&0o7777, after.Mode&0o7777, "the chmod must apply")
			assert.Equal(t, allDOSBits, after.Mode&allDOSBits,
				"an absolute-mode write must not clear the DOS attribute bits")
		})
	}
}
