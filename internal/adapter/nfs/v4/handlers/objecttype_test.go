package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// The operations below are each defined over one kind of object, and each of
// them used to act on whatever the current filehandle named. The tests pin the
// status RFC 7530 requires for the wrong type, per operation.

// TestCommit_NonRegularTarget covers RFC 7530 Section 16.5.4: COMMIT flushes a
// regular file, so a directory is NFS4ERR_ISDIR and every other type
// NFS4ERR_INVAL. An object with no payload used to answer NFS4_OK, because the
// handler returned early on an empty PayloadID without ever looking at the type.
func TestCommit_NonRegularTarget(t *testing.T) {
	cases := []struct {
		name     string
		child    string
		fileType metadata.FileType
		want     uint32
	}{
		{"directory", "subdir", metadata.FileTypeDirectory, types.NFS4ERR_ISDIR},
		{"symlink", "link", metadata.FileTypeSymlink, types.NFS4ERR_INVAL},
		{"fifo", "fifo", metadata.FileTypeFIFO, types.NFS4ERR_INVAL},
		{"socket", "sock", metadata.FileTypeSocket, types.NFS4ERR_INVAL},
		{"regular", "file.txt", metadata.FileTypeRegular, types.NFS4_OK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			handle := fx.createTestFile(t, fx.rootHandle, tc.child, tc.fileType, 0o755, 1000, 1000)

			ctx := newRealFSContext(1000, 1000)
			ctx.CurrentFH = append([]byte(nil), handle...)

			var args bytes.Buffer
			_ = xdr.WriteUint64(&args, 0) // offset
			_ = xdr.WriteUint32(&args, 0) // count: whole file

			r := fx.handler.handleCommit(ctx, bytes.NewReader(args.Bytes()))
			if r.Status != tc.want {
				t.Fatalf("COMMIT on %s: status = %d, want %d", tc.name, r.Status, tc.want)
			}
		})
	}
}

// TestLockT_NonRegularTarget covers RFC 7530 Section 16.10.4. The lock tables
// are keyed by filehandle bytes alone and never consulted the store, so a probe
// against a directory or a device node found no conflict and answered NFS4_OK.
func TestLockT_NonRegularTarget(t *testing.T) {
	cases := []struct {
		name     string
		child    string
		fileType metadata.FileType
		want     uint32
	}{
		{"directory", "subdir", metadata.FileTypeDirectory, types.NFS4ERR_ISDIR},
		{"symlink", "link", metadata.FileTypeSymlink, types.NFS4ERR_INVAL},
		{"chardev", "chr", metadata.FileTypeCharDevice, types.NFS4ERR_INVAL},
		{"regular", "file.txt", metadata.FileTypeRegular, types.NFS4_OK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			clientID := testClientID(t, fx.handler.StateManager, "lockt-type-client")
			handle := fx.createTestFile(t, fx.rootHandle, tc.child, tc.fileType, 0o644, 1000, 1000)

			ctx := newRealFSContext(1000, 1000)
			ctx.CurrentFH = append([]byte(nil), handle...)

			var args bytes.Buffer
			_ = xdr.WriteUint32(&args, types.WRITE_LT)
			_ = xdr.WriteUint64(&args, 0)
			_ = xdr.WriteUint64(&args, 100)
			_ = xdr.WriteUint64(&args, clientID)
			_ = xdr.WriteXDROpaque(&args, []byte("lockt-type-owner"))

			r := fx.handler.handleLockT(ctx, bytes.NewReader(args.Bytes()))
			if r.Status != tc.want {
				t.Fatalf("LOCKT on %s: status = %d, want %d", tc.name, r.Status, tc.want)
			}
		})
	}
}

// TestLookup_SymlinkCurrentFH covers RFC 7530 Section 16.15.4: a client that
// used a symbolic link as the current filehandle must be told NFS4ERR_SYMLINK,
// so it knows to resolve the link, rather than the NFS4ERR_NOTDIR the store
// reports for every non-directory alike.
func TestLookup_SymlinkCurrentFH(t *testing.T) {
	cases := []struct {
		name     string
		child    string
		fileType metadata.FileType
		want     uint32
	}{
		{"symlink", "link", metadata.FileTypeSymlink, types.NFS4ERR_SYMLINK},
		{"regular", "file.txt", metadata.FileTypeRegular, types.NFS4ERR_NOTDIR},
		{"fifo", "fifo", metadata.FileTypeFIFO, types.NFS4ERR_NOTDIR},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			handle := fx.createTestFile(t, fx.rootHandle, tc.child, tc.fileType, 0o755, 1000, 1000)

			ctx := newRealFSContext(1000, 1000)
			ctx.CurrentFH = append([]byte(nil), handle...)

			var args bytes.Buffer
			_ = xdr.WriteXDRString(&args, "anything")

			r := fx.handler.handleLookup(ctx, bytes.NewReader(args.Bytes()))
			if r.Status != tc.want {
				t.Fatalf("LOOKUP under %s: status = %d, want %d", tc.name, r.Status, tc.want)
			}
		})
	}
}

// TestLookupP_NonDirectoryCurrentFH covers RFC 7530 Section 16.16.4. Every
// object carries a parent, so LOOKUPP used to walk out of a file or a device
// node just as happily as out of a directory.
func TestLookupP_NonDirectoryCurrentFH(t *testing.T) {
	cases := []struct {
		name     string
		child    string
		fileType metadata.FileType
		want     uint32
	}{
		{"regular", "file.txt", metadata.FileTypeRegular, types.NFS4ERR_NOTDIR},
		{"symlink", "link", metadata.FileTypeSymlink, types.NFS4ERR_SYMLINK},
		{"fifo", "fifo", metadata.FileTypeFIFO, types.NFS4ERR_NOTDIR},
		{"socket", "sock", metadata.FileTypeSocket, types.NFS4ERR_NOTDIR},
		{"directory", "subdir", metadata.FileTypeDirectory, types.NFS4_OK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			handle := fx.createTestFile(t, fx.rootHandle, tc.child, tc.fileType, 0o755, 1000, 1000)

			ctx := newRealFSContext(1000, 1000)
			ctx.CurrentFH = append([]byte(nil), handle...)

			r := fx.handler.handleLookupP(ctx, bytes.NewReader(nil))
			if r.Status != tc.want {
				t.Fatalf("LOOKUPP from %s: status = %d, want %d", tc.name, r.Status, tc.want)
			}
		})
	}
}

// TestPutFH_UndecodableHandle covers RFC 7530 Section 16.21.4. A filehandle is
// either a pseudo-fs handle or the "<share>:<uuid>" form this server mints;
// anything else was never issued here. Accepting it deferred the error to
// whichever later operation first tried to resolve the handle, which reported
// the wrong status against the wrong operation.
func TestPutFH_UndecodableHandle(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	cases := []struct {
		name   string
		handle []byte
		want   uint32
	}{
		{"garbage", []byte("abc"), types.NFS4ERR_BADHANDLE},
		{"no uuid", []byte("/export:not-a-uuid"), types.NFS4ERR_BADHANDLE},
		{"empty share", []byte(":00000000-0000-0000-0000-000000000001"), types.NFS4ERR_BADHANDLE},
		{"real handle", fx.rootHandle, types.NFS4_OK},
		{"pseudo-fs root", fx.handler.PseudoFS.GetRootHandle(), types.NFS4_OK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newRealFSContext(1000, 1000)

			var args bytes.Buffer
			_ = xdr.WriteXDROpaque(&args, tc.handle)

			r := fx.handler.handlePutFH(ctx, bytes.NewReader(args.Bytes()))
			if r.Status != tc.want {
				t.Fatalf("PUTFH %s: status = %d, want %d", tc.name, r.Status, tc.want)
			}
		})
	}
}

// TestLookupP_ShareRootCrossesToJunctionParent pins the step out of a share.
//
// The pseudo-fs junction and the share root are two views of one directory: a
// share exported at /export is reached either as the pseudo-fs node /export or
// as the real filesystem's root. Answering LOOKUPP at the share root with the
// junction therefore left the client in the directory it started in, and a walk
// upwards stalled there instead of reaching the pseudo-fs root.
func TestLookupP_ShareRootCrossesToJunctionParent(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(1000, 1000)
	ctx.CurrentFH = append([]byte(nil), fx.rootHandle...)

	r := fx.handler.handleLookupP(ctx, bytes.NewReader(nil))
	if r.Status != types.NFS4_OK {
		t.Fatalf("LOOKUPP at the share root: status = %d, want NFS4_OK", r.Status)
	}

	pseudoRoot := fx.handler.PseudoFS.GetRootHandle()
	if !bytes.Equal(ctx.CurrentFH, pseudoRoot) {
		t.Fatalf("LOOKUPP at the share root landed on %q, want the pseudo-fs root %q",
			ctx.CurrentFH, pseudoRoot)
	}
}
