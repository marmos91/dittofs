package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// TestDispatchV40_CacheReply pins which v4.0 COMPOUNDs ask the adapter to keep
// their reply for a retransmission.
//
// NFSv4.0 has no SEQUENCE. Sequenced operations are covered by the open- and
// lock-owner seqid replay caches, but CREATE, REMOVE, RENAME and LINK carry no
// seqid: re-executing a retransmitted one reports NFS4ERR_EXIST or
// NFS4ERR_NOENT for an operation that in fact succeeded. Everything else is
// either seqid-protected or safe to repeat, and caching a READ reply would pin
// megabytes to protect nothing.
func TestDispatchV40_CacheReply(t *testing.T) {
	cases := []struct {
		name string
		ops  []compoundOp
		want bool
	}{
		{
			name: "create",
			ops:  []compoundOp{{opCode: types.OP_CREATE, data: encodeCreateDirArgs("subdir")}},
			want: true,
		},
		{
			name: "remove",
			ops:  []compoundOp{{opCode: types.OP_REMOVE, data: encodeRemoveArgs("victim")}},
			want: true,
		},
		{
			name: "read-only",
			ops:  []compoundOp{{opCode: types.OP_GETFH}},
			want: false,
		},
		{
			name: "no operations",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			fx.createTestFile(t, fx.rootHandle, "victim", 0, 0o644, 0, 0)

			ctx := newRealFSContext(0, 0)
			ops := append([]compoundOp{{opCode: types.OP_PUTFH, data: encodePutFHArgs(fx.rootHandle)}}, tc.ops...)
			data := buildCompoundArgsWithOps(nil, types.NFS4_MINOR_VERSION_0, ops)

			if _, err := fx.handler.ProcessCompound(ctx, data); err != nil {
				t.Fatalf("ProcessCompound error: %v", err)
			}
			if ctx.CacheReply != tc.want {
				t.Fatalf("CacheReply = %v, want %v", ctx.CacheReply, tc.want)
			}
		})
	}
}

// TestDispatchV41_NeverCachesReply checks the v4.1 path leaves CacheReply
// alone: a v4.1 retransmission is recognised by the session slot table, and a
// second cache answering it would be a second source of truth.
func TestDispatchV41_NeverCachesReply(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	data := buildCompoundArgsWithOps(nil, types.NFS4_MINOR_VERSION_1, []compoundOp{
		{opCode: types.OP_PUTFH, data: encodePutFHArgs(fx.rootHandle)},
		{opCode: types.OP_CREATE, data: encodeCreateDirArgs("subdir")},
	})

	if _, err := fx.handler.ProcessCompound(ctx, data); err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}
	if ctx.CacheReply {
		t.Fatal("v4.1 COMPOUND asked for its reply to be cached")
	}
}
