package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// OPENATTR Tests
// ============================================================================

func TestHandleOpenAttr_NotSupp(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  pfs.GetRootHandle(),
	}

	// Encode createdir = false (uint32 = 0)
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, 0) // createdir = false

	result := h.handleOpenAttr(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_NOTSUPP {
		t.Errorf("OPENATTR status = %d, want NFS4ERR_NOTSUPP (%d)",
			result.Status, types.NFS4ERR_NOTSUPP)
	}
	if result.OpCode != types.OP_OPENATTR {
		t.Errorf("OPENATTR opCode = %d, want OP_OPENATTR (%d)",
			result.OpCode, types.OP_OPENATTR)
	}
}

// ============================================================================
// OPEN_DOWNGRADE Tests
// ============================================================================

func TestHandleOpenDowngrade_BadStateid(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  pfs.GetRootHandle(),
	}

	// Encode OPEN_DOWNGRADE args with an unknown stateid (no open state exists)
	var args bytes.Buffer
	sid := &types.Stateid4{Seqid: 1}
	types.EncodeStateid4(&args, sid)
	_ = xdr.WriteUint32(&args, 2)                             // seqid
	_ = xdr.WriteUint32(&args, types.OPEN4_SHARE_ACCESS_READ) // share_access
	_ = xdr.WriteUint32(&args, types.OPEN4_SHARE_DENY_NONE)   // share_deny

	result := h.handleOpenDowngrade(ctx, bytes.NewReader(args.Bytes()))

	// Should return BAD_STATEID since the stateid is not tracked
	if result.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("OPEN_DOWNGRADE with unknown stateid status = %d, want NFS4ERR_BAD_STATEID (%d)",
			result.Status, types.NFS4ERR_BAD_STATEID)
	}
	if result.OpCode != types.OP_OPEN_DOWNGRADE {
		t.Errorf("OPEN_DOWNGRADE opCode = %d, want OP_OPEN_DOWNGRADE (%d)",
			result.OpCode, types.OP_OPEN_DOWNGRADE)
	}
}

func TestHandleOpenDowngrade_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  nil,
	}

	// Encode args (they won't be read since FH check happens first)
	var args bytes.Buffer
	sid := &types.Stateid4{Seqid: 1}
	types.EncodeStateid4(&args, sid)
	_ = xdr.WriteUint32(&args, 2)
	_ = xdr.WriteUint32(&args, types.OPEN4_SHARE_ACCESS_READ)
	_ = xdr.WriteUint32(&args, types.OPEN4_SHARE_DENY_NONE)

	result := h.handleOpenDowngrade(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("OPEN_DOWNGRADE without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// RELEASE_LOCKOWNER Tests
// ============================================================================

func TestHandleReleaseLockOwner_Success(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	// Encode lock_owner4: clientid (uint64) + owner (opaque)
	var args bytes.Buffer
	_ = xdr.WriteUint64(&args, 12345)                   // clientid
	_ = xdr.WriteXDROpaque(&args, []byte("test-owner")) // owner

	result := h.handleReleaseLockOwner(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Errorf("RELEASE_LOCKOWNER status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}
	if result.OpCode != types.OP_RELEASE_LOCKOWNER {
		t.Errorf("RELEASE_LOCKOWNER opCode = %d, want OP_RELEASE_LOCKOWNER (%d)",
			result.OpCode, types.OP_RELEASE_LOCKOWNER)
	}
}

// SECINFO tests moved to secinfo_test.go
