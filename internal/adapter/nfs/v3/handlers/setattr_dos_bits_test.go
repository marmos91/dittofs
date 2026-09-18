package handlers_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rootAuthContext is the fixture's own root setup context, which is unexported
// there. DOS attribute bits are set the way the SMB path sets them, and the
// fixture offers no exported hook for it.
func rootAuthContext() *metadata.AuthContext {
	uid := uint32(0)
	gid := uint32(0)
	return &metadata.AuthContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:12345",
		AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: &uid, GID: &gid, GIDs: []uint32{gid}},
	}
}

// dosAttributeModeBits are the high-word bits that carry DOS attributes in the
// same Mode field as the POSIX permission triple.
const dosAttributeModeBits = uint32(0x10000 | 0x20000 | 0x40000 | 0x80000 | 0x100000 | 0x200000)

// setDOSAttributes stores the given DOS attribute bits on a file the way the
// SMB attribute path does — through the mode masks, which the store whitelists
// down to the DOS bits.
func setDOSAttributes(t *testing.T, fx *handlertesting.HandlerTestFixture, handle metadata.FileHandle, bits uint32) {
	t.Helper()
	zero := uint32(0)
	_, err := fx.MetadataService.SetFileAttributes(rootAuthContext(), handle, &metadata.SetAttrs{
		ModeOrMask:     &bits,
		ModeAndNotMask: &zero,
	})
	require.NoError(t, err, "setting DOS attributes must succeed")
}

// TestSetAttr_ModePreservesDOSAttributes pins that an NFS SETATTR mode change
// leaves the DOS attribute bits standing.
//
// The NFS client cannot see those bits: GETATTR masks the high word off, so a
// file whose stored mode is 0x3501a4 is reported as 0o644. A client that reads
// the mode and writes the same value back — chmod --reference, cp -p, rsync -p,
// or a plain chmod that changes nothing — believes it is a no-op, so a mode
// write that cleared the high word would destroy SMB attribute state the client
// never addressed. The FSCTL-managed COMPRESSED and SPARSE bits are the worst
// of them: they are owned by FSCTL_SET_COMPRESSION / FSCTL_SET_SPARSE, and
// READONLY is what the permission model enforces writes against.
func TestSetAttr_ModePreservesDOSAttributes(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("dosbits.txt", []byte("content"))

	// Explicit + Archive + Compressed + Readonly + Sparse.
	const dosBits = uint32(0x10000 | 0x20000 | 0x40000 | 0x100000 | 0x200000)
	setDOSAttributes(t, fx, fileHandle, dosBits)

	before := fx.GetFile("dosbits.txt")
	require.Equal(t, dosBits, before.Mode&dosAttributeModeBits,
		"precondition: the DOS attributes must be stored")

	// A chmod writing back exactly the mode NFS GETATTR reported.
	newMode := uint32(0o644)
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), &handlers.SetAttrRequest{
		Handle:  fileHandle,
		NewAttr: metadata.SetAttrs{Mode: &newMode},
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status, "SETATTR should return NFS3OK")

	after := fx.GetFile("dosbits.txt")
	assert.EqualValues(t, uint32(0o644), after.Mode&0o7777, "the mode change must apply")
	assert.Equal(t, dosBits, after.Mode&dosAttributeModeBits,
		"a SETATTR mode change must not clear the DOS attribute bits")
}

// TestSetAttr_ModeStillChangesPermissions is the control: preserving the high
// word must not stop the mode write from doing its actual job, including on a
// file that carries no DOS attributes at all.
func TestSetAttr_ModeStillChangesPermissions(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("plainmode.txt", []byte("content"))

	newMode := uint32(0o600)
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), &handlers.SetAttrRequest{
		Handle:  fileHandle,
		NewAttr: metadata.SetAttrs{Mode: &newMode},
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status)

	after := fx.GetFile("plainmode.txt")
	assert.EqualValues(t, uint32(0o600), after.Mode&0o7777, "the mode change must apply")
	assert.Zero(t, after.Mode&dosAttributeModeBits, "no DOS attributes were ever set")
}
