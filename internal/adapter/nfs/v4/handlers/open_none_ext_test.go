package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// delegationArmFromOpenResult skips past OPEN4resok's fixed prefix and returns
// the open_delegation4 discriminant plus whatever follows it.
//
// Layout: status, stateid4, change_info4 (atomic + before + after), rflags,
// attrset bitmap length, then the delegation union.
func delegationArmFromOpenResult(t *testing.T, data []byte) (uint32, []uint32) {
	t.Helper()

	reader := bytes.NewReader(data)
	if _, err := xdr.DecodeUint32(reader); err != nil { // status
		t.Fatalf("decode status: %v", err)
	}
	if _, err := types.DecodeStateid4(reader); err != nil {
		t.Fatalf("decode stateid: %v", err)
	}
	_, _ = xdr.DecodeUint32(reader) // change_info4.atomic
	_, _ = xdr.DecodeUint64(reader) // change_info4.before
	_, _ = xdr.DecodeUint64(reader) // change_info4.after
	_, _ = xdr.DecodeUint32(reader) // rflags

	bitmapLen, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode attrset length: %v", err)
	}
	for range bitmapLen {
		_, _ = xdr.DecodeUint32(reader)
	}

	delegType, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode delegation type: %v", err)
	}

	var rest []uint32
	for {
		word, err := xdr.DecodeUint32(reader)
		if err != nil {
			break
		}
		rest = append(rest, word)
	}
	return delegType, rest
}

// TestOpen_DelegationWantBits checks the open_delegation4 arm an OPEN reply
// carries when the client used share_access to say what it wants.
//
// A v4.1 client that says it wants no delegation gets a reason back
// (OPEN_DELEGATE_NONE_EXT + WND4_NOT_WANTED, RFC 8881 Section 18.16.3) rather
// than a bare refusal it cannot tell apart from the server declining. A v4.0
// client gets a bare OPEN_DELEGATE_NONE whatever bits it set: the extended arm
// does not exist in RFC 7530, so encoding it would hand the client bytes it
// cannot decode, and the want bits themselves are meaningless there.
func TestOpen_DelegationWantBits(t *testing.T) {
	tests := []struct {
		name        string
		minorVer    uint32
		shareAccess uint32
		wantArm     uint32
		wantRest    []uint32
	}{
		{
			name:        "v4.1 want-no-deleg is answered with a reason",
			minorVer:    1,
			shareAccess: types.OPEN4_SHARE_ACCESS_BOTH | types.OPEN4_SHARE_ACCESS_WANT_NO_DELEG,
			wantArm:     types.OPEN_DELEGATE_NONE_EXT,
			wantRest:    []uint32{types.WND4_NOT_WANTED},
		},
		{
			name:        "v4.1 want-cancel is answered with a reason",
			minorVer:    1,
			shareAccess: types.OPEN4_SHARE_ACCESS_BOTH | types.OPEN4_SHARE_ACCESS_WANT_CANCEL,
			wantArm:     types.OPEN_DELEGATE_NONE_EXT,
			wantRest:    []uint32{types.WND4_CANCELLED},
		},
		{
			name:        "v4.1 no want bits falls through to the grant policy",
			minorVer:    1,
			shareAccess: types.OPEN4_SHARE_ACCESS_BOTH,
			wantArm:     types.OPEN_DELEGATE_NONE,
			wantRest:    nil,
		},
		{
			name:        "v4.1 want-read is a preference, not an answer",
			minorVer:    1,
			shareAccess: types.OPEN4_SHARE_ACCESS_BOTH | types.OPEN4_SHARE_ACCESS_WANT_READ_DELEG,
			wantArm:     types.OPEN_DELEGATE_NONE,
			wantRest:    nil,
		},
		{
			name:        "v4.0 never sees the extended arm",
			minorVer:    0,
			shareAccess: types.OPEN4_SHARE_ACCESS_BOTH | types.OPEN4_SHARE_ACCESS_WANT_NO_DELEG,
			wantArm:     types.OPEN_DELEGATE_NONE,
			wantRest:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newIOTestFixture(t, "/export")
			clientID := testClientID(t, fx.handler.StateManager, "want-bits-"+tt.name)
			fx.createRegularFile(t, fx.rootHandle, "want.txt", 0o644, 0, 0)

			ctx := newRealFSContext(0, 0)
			ctx.MinorVersion = tt.minorVer
			ctx.MinorVersionAccepted = true
			ctx.SkipOwnerSeqid = tt.minorVer >= 1
			ctx.CurrentFH = append([]byte(nil), fx.rootHandle...)

			args := encodeOpenArgs(
				1,
				tt.shareAccess,
				types.OPEN4_SHARE_DENY_NONE,
				clientID,
				[]byte("owner-want"),
				types.OPEN4_NOCREATE,
				0,
				types.CLAIM_NULL,
				"want.txt",
			)

			result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
			if result.Status != types.NFS4_OK {
				t.Fatalf("OPEN status = %d, want NFS4_OK", result.Status)
			}

			arm, rest := delegationArmFromOpenResult(t, result.Data)
			if arm != tt.wantArm {
				t.Fatalf("open_delegation4 discriminant = %d, want %d", arm, tt.wantArm)
			}
			if len(rest) != len(tt.wantRest) {
				t.Fatalf("delegation arm carried %d words %v, want %d %v",
					len(rest), rest, len(tt.wantRest), tt.wantRest)
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("arm word %d = %d, want %d", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}
