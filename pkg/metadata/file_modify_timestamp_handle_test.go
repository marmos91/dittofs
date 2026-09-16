package metadata_test

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// requireErrorCode asserts err is a StoreError carrying the given code.
func requireErrorCode(t *testing.T, err error, want metadata.ErrorCode) {
	t.Helper()
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, want, storeErr.Code)
}

// requirePermissionDenied asserts err is a StoreError carrying
// ErrPermissionDenied.
func requirePermissionDenied(t *testing.T, err error) {
	t.Helper()
	requireErrorCode(t, err, metadata.ErrPermissionDenied)
}

// stampTime is an arbitrary fixed explicit timestamp used by the
// TimestampAuthorizedByHandle tests.
var stampTime = time.Unix(1_000_000, 0).UTC()

// newForeignOwnedFile creates a regular file owned by UID 2002 under the
// fixture root and returns its handle. Mode 0o600 gives a non-owner neither
// POSIX write nor read, so nothing but the handle grant can authorize a
// timestamp write on it.
func newForeignOwnedFile(t *testing.T, f *testFixture, name string, mode uint32) metadata.FileHandle {
	t.Helper()
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, name,
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: mode,
			UID:  2002,
			GID:  2002,
		})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)
	return handle
}

// TestSetFileAttributes_TimestampAuthorizedByHandleGrantsNonOwner asserts the
// headline case: SMB authorizes a timestamp write by FILE_WRITE_ATTRIBUTES on
// the open handle, not by ownership of the file, so a non-owner holding such a
// handle can set an explicit timestamp the POSIX ownership gate would refuse.
func TestSetFileAttributes_TimestampAuthorizedByHandleGrantsNonOwner(t *testing.T) {
	f := newTestFixture(t)
	handle := newForeignOwnedFile(t, f, "handle_ts.txt", 0o600)

	// Precondition: without the handle grant the non-owner is refused.
	denyCtx := f.authContext(1001, 1001)
	_, err := f.service.SetFileAttributes(denyCtx, handle, &metadata.SetAttrs{Mtime: &stampTime})
	require.Error(t, err, "precondition: non-owner explicit timestamp must be denied without the handle grant")
	requirePermissionDenied(t, err)

	// With the grant the same write lands.
	authCtx := f.authContext(1001, 1001)
	authCtx.TimestampAuthorizedByHandle = true
	_, err = f.service.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{Mtime: &stampTime})
	require.NoError(t, err, "FILE_WRITE_ATTRIBUTES on the handle must authorize an explicit timestamp write")

	file, err := f.service.GetFile(authCtx.Context, handle)
	require.NoError(t, err)
	require.True(t, file.Mtime.Equal(stampTime), "stored Mtime = %v, want %v", file.Mtime, stampTime)
}

// TestSetFileAttributes_TimestampHandleGrantCoversAllFourStamps asserts the
// relaxation covers every timestamp FILE_BASIC_INFORMATION can carry, not just
// Mtime.
func TestSetFileAttributes_TimestampHandleGrantCoversAllFourStamps(t *testing.T) {
	f := newTestFixture(t)

	for _, tc := range []struct {
		name  string
		attrs *metadata.SetAttrs
	}{
		{"Atime", &metadata.SetAttrs{Atime: &stampTime}},
		{"Mtime", &metadata.SetAttrs{Mtime: &stampTime}},
		{"Ctime", &metadata.SetAttrs{Ctime: &stampTime}},
		{"CreationTime", &metadata.SetAttrs{CreationTime: &stampTime}},
		{"All", &metadata.SetAttrs{
			Atime: &stampTime, Mtime: &stampTime,
			Ctime: &stampTime, CreationTime: &stampTime,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle := newForeignOwnedFile(t, f, "stamps_"+tc.name+".txt", 0o600)
			authCtx := f.authContext(1001, 1001)
			authCtx.TimestampAuthorizedByHandle = true
			_, err := f.service.SetFileAttributes(authCtx, handle, tc.attrs)
			require.NoError(t, err)
		})
	}
}

// TestSetFileAttributes_TimestampHandleGrantDoesNotWiden asserts the flag
// relaxes the ownership gate for a timestamp-only change and nothing else. A
// SetAttrs that also carries a mode, ownership, size, DOS-attribute or EA
// change still requires ownership, so the flag cannot be used to smuggle a
// chmod or a truncate past the gate by pairing it with a timestamp.
func TestSetFileAttributes_TimestampHandleGrantDoesNotWiden(t *testing.T) {
	f := newTestFixture(t)

	mode := uint32(0o777)
	uid := uint32(1001)
	size := uint64(4096)
	mask := uint32(0o6000)
	hidden := true

	for _, tc := range []struct {
		name  string
		attrs *metadata.SetAttrs
	}{
		{"mode", &metadata.SetAttrs{Mtime: &stampTime, Mode: &mode}},
		{"uid", &metadata.SetAttrs{Mtime: &stampTime, UID: &uid}},
		{"gid", &metadata.SetAttrs{Mtime: &stampTime, GID: &uid}},
		{"size", &metadata.SetAttrs{Mtime: &stampTime, Size: &size}},
		{"modeOrMask", &metadata.SetAttrs{Mtime: &stampTime, ModeOrMask: &mask}},
		{"modeAndNotMask", &metadata.SetAttrs{Mtime: &stampTime, ModeAndNotMask: &mask}},
		{"hidden", &metadata.SetAttrs{Mtime: &stampTime, Hidden: &hidden}},
		{"ea", &metadata.SetAttrs{
			Mtime:       &stampTime,
			EAMutations: []metadata.EAMutation{{Name: "user.x", Value: []byte("v")}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle := newForeignOwnedFile(t, f, "widen_"+tc.name+".txt", 0o600)
			authCtx := f.authContext(1001, 1001)
			authCtx.TimestampAuthorizedByHandle = true
			_, err := f.service.SetFileAttributes(authCtx, handle, tc.attrs)
			// The denial code varies by field — a size change reaches the
			// POSIX truncate branch and is refused as access-denied rather
			// than not-permitted. What matters is that none of these are
			// authorized by the timestamp handle grant.
			require.Error(t, err, "handle timestamp grant must not authorize a %s change", tc.name)
		})
	}
}

// TestSetFileAttributes_TimestampAndWriteHandleGrantsAreDistinct asserts the
// two handle grants stay separate, as MS-FSCC 2.6 requires: FILE_WRITE_DATA
// does not confer the right to set timestamps, and FILE_WRITE_ATTRIBUTES does
// not confer data write.
func TestSetFileAttributes_TimestampAndWriteHandleGrantsAreDistinct(t *testing.T) {
	f := newTestFixture(t)
	handle := newForeignOwnedFile(t, f, "distinct.txt", 0o600)

	// Data-write handle only: the timestamp write stays denied.
	writeOnly := f.authContext(1001, 1001)
	writeOnly.WriteAuthorizedByHandle = true
	_, err := f.service.SetFileAttributes(writeOnly, handle, &metadata.SetAttrs{Mtime: &stampTime})
	require.Error(t, err, "FILE_WRITE_DATA must not confer the right to set timestamps")
	requirePermissionDenied(t, err)

	// Attribute-write handle only: data write stays denied.
	tsOnly := f.authContext(1001, 1001)
	tsOnly.TimestampAuthorizedByHandle = true
	got, err := f.service.CheckPermissions(tsOnly, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	require.Zero(t, got&metadata.PermissionWrite,
		"FILE_WRITE_ATTRIBUTES must not confer data write")
}

// TestSetFileAttributes_NFSTimestampSemanticsUnchanged pins that the NFS path
// is untouched by the bridge. NFS is path-based, has no handle, and never sets
// TimestampAuthorizedByHandle, so POSIX ownership remains the rule for an
// explicit timestamp write and write permission alone remains insufficient —
// while UTIME_NOW continues to be satisfied by write permission.
func TestSetFileAttributes_NFSTimestampSemanticsUnchanged(t *testing.T) {
	f := newTestFixture(t)
	// Mode 0o666: a non-owner has POSIX write, which POSIX says is NOT enough
	// for utimensat() with explicit times but IS enough for UTIME_NOW.
	handle := newForeignOwnedFile(t, f, "nfs.txt", 0o666)

	nfsCtx := f.authContext(1001, 1001)
	require.False(t, nfsCtx.TimestampAuthorizedByHandle,
		"an NFS AuthContext must never carry the SMB handle grant")

	got, err := f.service.CheckPermissions(nfsCtx, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	require.NotZero(t, got&metadata.PermissionWrite, "precondition: non-owner has POSIX write on 0o666")

	_, err = f.service.SetFileAttributes(nfsCtx, handle, &metadata.SetAttrs{Mtime: &stampTime})
	require.Error(t, err, "POSIX requires ownership for an explicit timestamp write")
	requirePermissionDenied(t, err)

	// UTIME_NOW is unchanged: write permission is sufficient.
	_, err = f.service.SetFileAttributes(nfsCtx, handle, &metadata.SetAttrs{MtimeNow: true})
	require.NoError(t, err, "UTIME_NOW must remain satisfied by write permission")
}

// TestSetFileAttributes_TimestampHandleGrantOwnerUnchanged asserts
// the owner and root paths are unchanged: the flag is additive, so an owner who
// never had a handle grant keeps working exactly as before.
func TestSetFileAttributes_TimestampHandleGrantOwnerUnchanged(t *testing.T) {
	f := newTestFixture(t)
	handle := newForeignOwnedFile(t, f, "owner.txt", 0o600)

	owner := f.authContext(2002, 2002)
	_, err := f.service.SetFileAttributes(owner, handle, &metadata.SetAttrs{Mtime: &stampTime})
	require.NoError(t, err, "owner must still be able to set an explicit timestamp without any handle grant")
}

// TestSetFileAttributes_EAOnlyMutationGrantsNonOwnerWithWrite pins the EA-only
// carve-out in the SetAttrs authorization, which the SMB-side gate tests cannot
// reach: they authenticate as UID 0, so the root bypass grants the write whether
// or not the carve-out exists, and the existing metadata EA case pairs the
// mutations with an explicit Mtime, which deliberately falls outside the branch.
//
// The rule: a caller with POSIX write permission but no ownership may mutate
// extended attributes alone. SMB authorizes the open handle's FILE_WRITE_EA bit
// at the handler layer, so requiring ownership here would refuse a write the
// protocol has already granted.
//
// Mode 0o666 is what makes the case meaningful — the non-owner has write but
// not ownership, so only the carve-out can authorize this.
func TestSetFileAttributes_EAOnlyMutationGrantsNonOwnerWithWrite(t *testing.T) {
	f := newTestFixture(t)
	handle := newForeignOwnedFile(t, f, "ea_only.txt", 0o666)

	nonOwner := f.authContext(1001, 1001)
	_, err := f.service.SetFileAttributes(nonOwner, handle, &metadata.SetAttrs{
		EAMutations: []metadata.EAMutation{{Name: "$KERNEL.PURGE.ESBCACHE", Value: []byte("v")}},
	})
	require.NoError(t, err, "a non-owner with POSIX write must be able to mutate EAs alone")

	// The carve-out is EA-only: pairing the same mutation with an ownership
	// attribute must still be refused, or it would be a general write bypass.
	_, err = f.service.SetFileAttributes(nonOwner, handle, &metadata.SetAttrs{
		EAMutations: []metadata.EAMutation{{Name: "$KERNEL.PURGE.ESBCACHE", Value: []byte("v")}},
		Mode:        func() *uint32 { m := uint32(0o600); return &m }(),
	})
	requirePermissionDenied(t, err)
}

// TestSetFileAttributes_EAAuthorizedByHandleGrantsNonOwnerWithoutWrite covers
// the right SMB actually names for extended attributes. FILE_WRITE_EA and
// FILE_WRITE_DATA are distinct in MS-FSCC 2.6, so a handle opened to mutate EAs
// carries no claim on the file's data and cannot borrow the write bypass: the
// flag it sets has to be its own, and it has to be enough on its own, because a
// non-owner with no POSIX write permission is exactly the caller the protocol
// expects to succeed here.
func TestSetFileAttributes_EAAuthorizedByHandleGrantsNonOwnerWithoutWrite(t *testing.T) {
	f := newTestFixture(t)
	// 0o644: the non-owner can read but not write, so POSIX alone refuses and
	// only the handle grant can authorize the mutation.
	handle := newForeignOwnedFile(t, f, "handle_ea.txt", 0o644)

	mutation := []metadata.EAMutation{{Name: "$KERNEL.PURGE.ESBCACHE", Value: []byte("v")}}

	denyCtx := f.authContext(1001, 1001)
	_, err := f.service.SetFileAttributes(denyCtx, handle, &metadata.SetAttrs{EAMutations: mutation})
	require.Error(t, err, "precondition: a non-owner without POSIX write must be refused without the handle grant")
	// EACCES: an EA-only SetAttrs is one of the cases POSIX lets write permission
	// answer for instead of ownership, so without the handle grant it reaches the
	// write check and is refused there on the file's mode.
	requireErrorCode(t, err, metadata.ErrAccessDenied)

	authCtx := f.authContext(1001, 1001)
	authCtx.EAAuthorizedByHandle = true
	_, err = f.service.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{EAMutations: mutation})
	require.NoError(t, err, "FILE_WRITE_EA on the handle must authorize an EA-only mutation")

	// Scoped to EA-only: the same grant paired with anything else is refused,
	// or it would be the general write bypass under another name.
	mode := uint32(0o600)
	scopeCtx := f.authContext(1001, 1001)
	scopeCtx.EAAuthorizedByHandle = true
	_, err = f.service.SetFileAttributes(scopeCtx, handle, &metadata.SetAttrs{
		EAMutations: mutation,
		Mode:        &mode,
	})
	require.Error(t, err, "the EA grant must not authorize a mode change riding alongside")
	requirePermissionDenied(t, err)

	// And it must not confer write on the file's data.
	size := uint64(0)
	dataCtx := f.authContext(1001, 1001)
	dataCtx.EAAuthorizedByHandle = true
	_, err = f.service.SetFileAttributes(dataCtx, handle, &metadata.SetAttrs{Size: &size})
	require.Error(t, err, "the EA grant must not authorize a truncate")
	// EACCES rather than EPERM, and the difference is the point: the EA grant did
	// not stand in for anything here, so the request fell through to the ordinary
	// POSIX write check, which refused it on the file's mode.
	requireErrorCode(t, err, metadata.ErrAccessDenied)
}
