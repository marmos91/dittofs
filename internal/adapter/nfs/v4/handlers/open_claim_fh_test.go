package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestOpen_ClaimFH_ReopenExistingFile reproduces the intermittent EINVAL a
// Linux NFSv4.1 client hits when it re-opens a file it already holds a
// filehandle for (e.g. reading back a file it just created and closed).
//
// Instead of a fresh CLAIM_NULL lookup, the client sends OPEN with claim type
// CLAIM_FH (RFC 8881 Section 18.16.3): open the current filehandle, no name,
// void args. The server previously had no case for CLAIM_FH and fell through
// to the dispatch default, returning NFS4ERR_INVAL — surfacing to the client
// as EINVAL from open(2). Whether the kernel used CLAIM_FH or CLAIM_NULL
// depended on its cache state, which made the failure look intermittent.
//
// A CLAIM_FH open of an existing, accessible file must succeed with NFS4_OK.
func TestOpen_ClaimFH_ReopenExistingFile(t *testing.T) {
	const clientID = uint64(0x0BADF00D)
	owner := []byte("claim-fh-owner")

	fx := newRealFSTestFixture(t, "/export")

	// Create the file and grab its filehandle, as if the client had just
	// created it and still held the handle.
	fileHandle := fx.createTestFile(t, fx.rootHandle, "reopen.txt",
		metadata.FileTypeRegular, 0o644, 1000, 1000)

	ctx := newRealFSContext(1000, 1000)
	ctx.SkipOwnerSeqid = true // NFSv4.1 session semantics
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// OPEN with CLAIM_FH: NOCREATE, READ, no filename (void claim args).
	args := encodeOpenArgs(
		0, // seqid ignored in v4.1
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		clientID, owner,
		types.OPEN4_NOCREATE, 0, types.CLAIM_FH,
		"", // CLAIM_FH carries no component name
	)

	r := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if r.Status == types.NFS4ERR_INVAL {
		t.Fatalf("CLAIM_FH OPEN returned NFS4ERR_INVAL (regression: server has no " +
			"CLAIM_FH case and rejected the re-open)")
	}
	if r.Status != types.NFS4_OK {
		t.Fatalf("CLAIM_FH OPEN status = %d, want NFS4_OK", r.Status)
	}
}

// TestOpen_ClaimFH_CreateRejected verifies that OPEN4_CREATE combined with
// CLAIM_FH is rejected. CLAIM_FH re-opens an existing filehandle; createhow4 is
// only meaningful for CLAIM_NULL (RFC 8881), so a create claim must not be
// silently downgraded to a plain open.
func TestOpen_ClaimFH_CreateRejected(t *testing.T) {
	const clientID = uint64(0x0BADF00D)
	owner := []byte("claim-fh-create-owner")

	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "reopen.txt",
		metadata.FileTypeRegular, 0o644, 1000, 1000)

	ctx := newRealFSContext(1000, 1000)
	ctx.SkipOwnerSeqid = true
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// UNCHECKED4 createattrs are still encoded so the args are well-formed; the
	// handler must reject on the OPEN4_CREATE opentype before consuming them.
	args := encodeOpenArgs(
		0,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		clientID, owner,
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_FH,
		"",
	)

	r := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if r.Status != types.NFS4ERR_INVAL {
		t.Fatalf("CLAIM_FH OPEN4_CREATE status = %d, want NFS4ERR_INVAL", r.Status)
	}
}
