package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestOpen_NonRegularTarget checks the type gate OPEN applies to an object that
// already exists. RFC 7530 Section 16.16.6: "If the component provided to OPEN
// resolves to something other than a regular file (or a named attribute), an
// error will be returned to the client. If it is a directory, NFS4ERR_ISDIR is
// returned; otherwise, NFS4ERR_SYMLINK is returned."
//
// Without the gate the handler resolved the name, found the mode bits allowed
// the requested share_access, and opened a directory with NFS4_OK.
func TestOpen_NonRegularTarget(t *testing.T) {
	cases := []struct {
		name     string
		child    string
		fileType metadata.FileType
		want     uint32
	}{
		{"directory", "subdir", metadata.FileTypeDirectory, types.NFS4ERR_ISDIR},
		{"symlink", "link", metadata.FileTypeSymlink, types.NFS4ERR_SYMLINK},
		{"regular", "file.txt", metadata.FileTypeRegular, types.NFS4_OK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			clientID := testClientID(t, fx.handler.StateManager, "open-type-client")
			fx.createTestFile(t, fx.rootHandle, tc.child, tc.fileType, 0o755, 1000, 1000)

			ctx := newRealFSContext(1000, 1000)
			ctx.CurrentFH = make([]byte, len(fx.rootHandle))
			copy(ctx.CurrentFH, fx.rootHandle)

			args := encodeOpenArgs(
				1,
				types.OPEN4_SHARE_ACCESS_READ,
				types.OPEN4_SHARE_DENY_NONE,
				clientID, []byte("open-type-owner"),
				types.OPEN4_NOCREATE, 0, types.CLAIM_NULL,
				tc.child,
			)

			r := fx.handler.handleOpen(ctx, bytes.NewReader(args))
			if r.Status != tc.want {
				t.Fatalf("OPEN of %s: status = %d, want %d", tc.name, r.Status, tc.want)
			}
		})
	}
}

// TestOpen_ClaimFH_NonRegularTarget covers the same gate on the CLAIM_FH path,
// which re-opens a filehandle the client already holds and so never performs a
// lookup of its own.
func TestOpen_ClaimFH_NonRegularTarget(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	clientID := testClientID(t, fx.handler.StateManager, "open-type-fh-client")
	dirHandle := fx.createTestFile(t, fx.rootHandle, "subdir", metadata.FileTypeDirectory, 0o755, 1000, 1000)

	ctx := newRealFSContext(1000, 1000)
	ctx.SkipOwnerSeqid = true
	ctx.CurrentFH = make([]byte, len(dirHandle))
	copy(ctx.CurrentFH, dirHandle)

	args := encodeOpenArgs(
		0,
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		clientID, []byte("open-type-fh-owner"),
		types.OPEN4_NOCREATE, 0, types.CLAIM_FH,
		"",
	)

	r := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if r.Status != types.NFS4ERR_ISDIR {
		t.Fatalf("CLAIM_FH OPEN of a directory: status = %d, want NFS4ERR_ISDIR", r.Status)
	}
}
