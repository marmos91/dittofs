package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// TestOpen_CreateGuarded_AfterCloseSameOwner reproduces the pjdfstest
// open/07.t / open/22.t / rename/04.t NFSv4.0 regression.
//
// A Linux v4.0 client reuses a single open-owner across files. After it
// creates+confirms+closes one file, the FIRST GUARDED create of a DIFFERENT,
// freshly-named file must succeed (NFS4_OK) regardless of the open-owner seqid
// the client uses for it. The open-owner seqid is per-owner state; once the
// owner has no remaining open state it must NOT keep gating new OPENs against
// the (now stale) seqid advanced by the prior CLOSE — doing so either rejects
// the new create as NFS4ERR_BAD_SEQID or, when the seqid happens to equal the
// retained owner's last seqid, mis-classifies the brand-new OPEN as a REPLAY
// and returns the cached CLOSE reply, which downstream surfaces as the spurious
// NFS4ERR_EXIST the test suite observed.
//
// This is the behavior PR #890's open-owner retention broke and that this fix
// restores: after the last CLOSE the owner is dropped from the live owner table
// (so new OPENs are unconstrained, as before), while still being retained for
// CLOSE replay via closedOwnerByOther.
func TestOpen_CreateGuarded_AfterCloseSameOwner(t *testing.T) {
	const clientID = uint64(0x12345678)
	owner := []byte("pjdfstest-open-owner")

	decodeStateid := func(t *testing.T, data []byte) *types.Stateid4 {
		t.Helper()
		r := bytes.NewReader(data)
		_, _ = xdr.DecodeUint32(r) // status
		sid, err := types.DecodeStateid4(r)
		if err != nil {
			t.Fatalf("decode stateid: %v", err)
		}
		return sid
	}

	// Drive create+confirm+close of file1 on a shared owner, then attempt the
	// first GUARDED create of file2 at file2Seq. Returns file2's OPEN status.
	run := func(t *testing.T, file2Seq uint32) uint32 {
		t.Helper()
		fx := newIOTestFixture(t, "/export")
		ctx := newRealFSContext(65534, 65534)

		openCreateGuarded := func(seqid uint32, filename string) *types.CompoundResult {
			ctx.CurrentFH = make([]byte, len(fx.rootHandle))
			copy(ctx.CurrentFH, fx.rootHandle)
			args := encodeOpenArgs(
				seqid,
				types.OPEN4_SHARE_ACCESS_BOTH,
				types.OPEN4_SHARE_DENY_NONE,
				clientID, owner,
				types.OPEN4_CREATE, types.GUARDED4, types.CLAIM_NULL,
				filename,
			)
			return fx.handler.handleOpen(ctx, bytes.NewReader(args))
		}

		r1 := openCreateGuarded(1, "file1.txt")
		if r1.Status != types.NFS4_OK {
			t.Fatalf("file1 OPEN CREATE status = %d, want NFS4_OK", r1.Status)
		}
		sid1 := decodeStateid(t, r1.Data)

		cr := fx.handler.handleOpenConfirm(ctx, bytes.NewReader(encodeOpenConfirmArgs(sid1, 2)))
		if cr.Status != types.NFS4_OK {
			t.Fatalf("file1 OPEN_CONFIRM status = %d, want NFS4_OK", cr.Status)
		}
		confirmedSid := decodeStateid(t, cr.Data)

		clr := fx.handler.handleClose(ctx, bytes.NewReader(encodeCloseArgs(3, confirmedSid)))
		if clr.Status != types.NFS4_OK {
			t.Fatalf("file1 CLOSE status = %d, want NFS4_OK", clr.Status)
		}

		// First GUARDED create of a brand-new name on the SAME owner.
		return openCreateGuarded(file2Seq, "file2.txt").Status
	}

	// After CLOSE advances the owner seqid to 3, a fresh create must succeed no
	// matter what seqid the client picks for it. Pre-fix #890 returned
	// NFS4ERR_BAD_SEQID for 1/2/5 and a stale cached CLOSE reply for 3.
	for _, file2Seq := range []uint32{1, 2, 3, 4, 5} {
		file2Seq := file2Seq
		t.Run("file2Seq", func(t *testing.T) {
			status := run(t, file2Seq)
			if status == types.NFS4ERR_EXIST {
				t.Fatalf("first GUARDED create of file2 (seqid=%d) returned NFS4ERR_EXIST "+
					"(regression: new OPEN mis-classified as replay of retained owner)", file2Seq)
			}
			if status != types.NFS4_OK {
				t.Fatalf("first GUARDED create of file2 (seqid=%d) status = %d, want NFS4_OK",
					file2Seq, status)
			}
		})
	}
}
