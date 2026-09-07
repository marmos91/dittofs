package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// decodeWriteResok pulls count and committed out of a successful WRITE4res.
func decodeWriteResok(t *testing.T, result *types.CompoundResult) (count, committed uint32) {
	t.Helper()
	if result.Status != types.NFS4_OK {
		t.Fatalf("WRITE status = %d, want NFS4_OK", result.Status)
	}
	reader := bytes.NewReader(result.Data)
	if _, err := xdr.DecodeUint32(reader); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	count, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode count: %v", err)
	}
	committed, err = xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode committed: %v", err)
	}
	return count, committed
}

// TestWrite_CommittedMatchesRequestedStability pins RFC 7530 Section 16.36.4:
// a WRITE that asked for FILE_SYNC4 must be answered with FILE_SYNC4, and one
// that asked for DATA_SYNC4 with at least DATA_SYNC4. Reporting UNSTABLE4 for
// either is a protocol violation, and reporting the stronger level without
// having flushed would be a durability lie.
func TestWrite_CommittedMatchesRequestedStability(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested uint32
		wantMin   uint32
	}{
		{"unstable", types.UNSTABLE4, types.UNSTABLE4},
		{"data_sync", types.DATA_SYNC4, types.DATA_SYNC4},
		{"file_sync", types.FILE_SYNC4, types.FILE_SYNC4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newIOTestFixture(t, "/export")
			fileHandle, stateid := openFileAndGetStateid(t, fx, "stable-"+tc.name,
				types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE)

			ctx := newRealFSContext(0, 0)
			setCurrentFH(ctx, fileHandle)

			args := encodeWriteArgs(stateid, 0, tc.requested, []byte("payload"))
			count, committed := decodeWriteResok(t, fx.handler.handleWrite(ctx, bytes.NewReader(args)))

			if count != uint32(len("payload")) {
				t.Errorf("count = %d, want %d", count, len("payload"))
			}
			if committed < tc.wantMin {
				t.Errorf("committed = %d, want at least %d", committed, tc.wantMin)
			}
		})
	}
}

// TestWrite_ZeroCountLeavesTimeModify pins RFC 7530 Section 16.36.5: "a WRITE
// request with count set to 0 should not cause the time_modify attribute of the
// file to be updated".
func TestWrite_ZeroCountLeavesTimeModify(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	fileHandle, stateid := openFileAndGetStateid(t, fx, "zerocount",
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE)

	metaSvc, err := getMetadataServiceForCtx(fx.handler)
	if err != nil {
		t.Fatalf("metadata service: %v", err)
	}
	before, err := metaSvc.GetFileForRead(t.Context(), fileHandle)
	if err != nil {
		t.Fatalf("GetFileForRead: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeWriteArgs(stateid, 5, types.UNSTABLE4, nil)
	count, _ := decodeWriteResok(t, fx.handler.handleWrite(ctx, bytes.NewReader(args)))
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}

	after, err := metaSvc.GetFileForRead(t.Context(), fileHandle)
	if err != nil {
		t.Fatalf("GetFileForRead: %v", err)
	}
	if !after.Mtime.Equal(before.Mtime) {
		t.Errorf("time_modify moved on a zero-count WRITE: %v -> %v", before.Mtime, after.Mtime)
	}
	if after.Size != before.Size {
		t.Errorf("size moved on a zero-count WRITE: %d -> %d", before.Size, after.Size)
	}
}

// TestWrite_RejectsOutOfRangeStableHow pins that an undefined stable_how4 is
// refused rather than echoed back. The reply's committed field is drawn from
// the same three-value enum (RFC 7530 Section 16.36.2), so accepting the write
// and reporting what was asked for would put an invalid enum on the wire.
func TestWrite_RejectsOutOfRangeStableHow(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	fileHandle, stateid := openFileAndGetStateid(t, fx, "badstable",
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE)

	ctx := newRealFSContext(0, 0)
	setCurrentFH(ctx, fileHandle)

	args := encodeWriteArgs(stateid, 0, types.FILE_SYNC4+1, []byte("payload"))
	result := fx.handler.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADXDR {
		t.Errorf("WRITE with stable_how4=%d status = %d, want NFS4ERR_BADXDR (%d)",
			types.FILE_SYNC4+1, result.Status, types.NFS4ERR_BADXDR)
	}
}
