package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ExtractHandlerContext Tests
// ============================================================================

func encodeAuthUnix(uid, gid uint32, gids []uint32) []byte {
	buf := new(bytes.Buffer)

	// Stamp
	_ = binary.Write(buf, binary.BigEndian, uint32(12345))

	// Machine name
	machineName := "testhost"
	nameLen := uint32(len(machineName))
	_ = binary.Write(buf, binary.BigEndian, nameLen)
	buf.WriteString(machineName)
	padding := (4 - (nameLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		buf.WriteByte(0)
	}

	// UID, GID
	_ = binary.Write(buf, binary.BigEndian, uid)
	_ = binary.Write(buf, binary.BigEndian, gid)

	// GIDs
	_ = binary.Write(buf, binary.BigEndian, uint32(len(gids)))
	for _, g := range gids {
		_ = binary.Write(buf, binary.BigEndian, g)
	}

	return buf.Bytes()
}

// TestExtractHandlerContext_AuthUnix tests UID/GID extraction from AUTH_UNIX.
func TestExtractHandlerContext_AuthUnix(t *testing.T) {
	authBody := encodeAuthUnix(1000, 2000, []uint32{2000, 3000})

	call := &rpc.RPCCallMessage{
		Cred: rpc.OpaqueAuth{
			Flavor: rpc.AuthUnix,
			Body:   authBody,
		},
		Verf: rpc.OpaqueAuth{
			Flavor: rpc.AuthNull,
			Body:   []byte{},
		},
	}

	ctx := ExtractHandlerContext(context.Background(), call, "192.168.1.1:12345", "/export", "TEST")

	require.NotNil(t, ctx)
	assert.Equal(t, "192.168.1.1:12345", ctx.ClientAddr)
	assert.Equal(t, "/export", ctx.Share)
	assert.EqualValues(t, rpc.AuthUnix, ctx.AuthFlavor)

	require.NotNil(t, ctx.UID, "UID should be parsed from AUTH_UNIX")
	require.NotNil(t, ctx.GID, "GID should be parsed from AUTH_UNIX")
	assert.EqualValues(t, 1000, *ctx.UID)
	assert.EqualValues(t, 2000, *ctx.GID)
	assert.Contains(t, ctx.GIDs, uint32(2000))
	assert.Contains(t, ctx.GIDs, uint32(3000))
}

// TestExtractHandlerContext_AuthNull tests that AUTH_NULL produces nil credentials.
func TestExtractHandlerContext_AuthNull(t *testing.T) {
	call := &rpc.RPCCallMessage{
		Cred: rpc.OpaqueAuth{
			Flavor: rpc.AuthNull,
			Body:   []byte{},
		},
		Verf: rpc.OpaqueAuth{
			Flavor: rpc.AuthNull,
			Body:   []byte{},
		},
	}

	ctx := ExtractHandlerContext(context.Background(), call, "10.0.0.1:5555", "/data", "NULL")

	require.NotNil(t, ctx)
	assert.EqualValues(t, rpc.AuthNull, ctx.AuthFlavor)
	assert.Nil(t, ctx.UID, "UID should be nil for AUTH_NULL")
	assert.Nil(t, ctx.GID, "GID should be nil for AUTH_NULL")
	assert.Nil(t, ctx.GIDs, "GIDs should be nil for AUTH_NULL")
}

// TestExtractHandlerContext_EmptyAuthBody tests graceful handling of empty AUTH_UNIX body.
func TestExtractHandlerContext_EmptyAuthBody(t *testing.T) {
	call := &rpc.RPCCallMessage{
		Cred: rpc.OpaqueAuth{
			Flavor: rpc.AuthUnix,
			Body:   []byte{}, // Empty body with AUTH_UNIX
		},
		Verf: rpc.OpaqueAuth{
			Flavor: rpc.AuthNull,
			Body:   []byte{},
		},
	}

	// Should not panic, should return context with nil credentials
	ctx := ExtractHandlerContext(context.Background(), call, "10.0.0.1:5555", "/data", "TEST")

	require.NotNil(t, ctx)
	assert.EqualValues(t, rpc.AuthUnix, ctx.AuthFlavor)
	assert.Nil(t, ctx.UID, "UID should be nil when auth body is empty")
	assert.Nil(t, ctx.GID, "GID should be nil when auth body is empty")
}

// ============================================================================
// Dispatch Table Completeness Tests
// ============================================================================

// TestNFSDispatchTable_Completeness verifies all 22 NFSv3 procedures are registered.
func TestNFSDispatchTable_Completeness(t *testing.T) {
	expectedProcs := map[uint32]string{
		types.NFSProcNull:        "NULL",
		types.NFSProcGetAttr:     "GETATTR",
		types.NFSProcSetAttr:     "SETATTR",
		types.NFSProcLookup:      "LOOKUP",
		types.NFSProcAccess:      "ACCESS",
		types.NFSProcReadLink:    "READLINK",
		types.NFSProcRead:        "READ",
		types.NFSProcWrite:       "WRITE",
		types.NFSProcCreate:      "CREATE",
		types.NFSProcMkdir:       "MKDIR",
		types.NFSProcSymlink:     "SYMLINK",
		types.NFSProcMknod:       "MKNOD",
		types.NFSProcRemove:      "REMOVE",
		types.NFSProcRmdir:       "RMDIR",
		types.NFSProcRename:      "RENAME",
		types.NFSProcLink:        "LINK",
		types.NFSProcReadDir:     "READDIR",
		types.NFSProcReadDirPlus: "READDIRPLUS",
		types.NFSProcFsStat:      "FSSTAT",
		types.NFSProcFsInfo:      "FSINFO",
		types.NFSProcPathConf:    "PATHCONF",
		types.NFSProcCommit:      "COMMIT",
	}

	assert.Equal(t, len(expectedProcs), len(NfsDispatchTable),
		"NFS dispatch table should have exactly %d procedures", len(expectedProcs))

	for procNum, expectedName := range expectedProcs {
		entry, ok := NfsDispatchTable[procNum]
		require.True(t, ok, "NFS dispatch table missing procedure %d (%s)", procNum, expectedName)
		assert.Equal(t, expectedName, entry.Name,
			"NFS procedure %d should be named %q, got %q", procNum, expectedName, entry.Name)
		assert.NotNil(t, entry.Handler,
			"NFS procedure %d (%s) handler should not be nil", procNum, expectedName)
	}
}

// TestMountDispatchTable_Completeness verifies all 6 Mount procedures are registered.
func TestMountDispatchTable_Completeness(t *testing.T) {
	expectedProcs := map[uint32]string{
		mount.MountProcNull:    "NULL",
		mount.MountProcMnt:     "MNT",
		mount.MountProcDump:    "DUMP",
		mount.MountProcUmnt:    "UMNT",
		mount.MountProcUmntAll: "UMNTALL",
		mount.MountProcExport:  "EXPORT",
	}

	assert.Equal(t, len(expectedProcs), len(MountDispatchTable),
		"Mount dispatch table should have exactly %d procedures", len(expectedProcs))

	for procNum, expectedName := range expectedProcs {
		entry, ok := MountDispatchTable[procNum]
		require.True(t, ok, "Mount dispatch table missing procedure %d (%s)", procNum, expectedName)
		assert.Equal(t, expectedName, entry.Name,
			"Mount procedure %d should be named %q, got %q", procNum, expectedName, entry.Name)
		assert.NotNil(t, entry.Handler,
			"Mount procedure %d (%s) handler should not be nil", procNum, expectedName)
	}
}

// TestNFSDispatchTable_AuthRequirements verifies NeedsAuth flags.
func TestNFSDispatchTable_AuthRequirements(t *testing.T) {
	// Procedures that do NOT require auth
	noAuthProcs := map[uint32]bool{
		types.NFSProcNull:     true,
		types.NFSProcGetAttr:  true,
		types.NFSProcFsStat:   true,
		types.NFSProcFsInfo:   true,
		types.NFSProcPathConf: true,
		types.NFSProcMknod:    true,
	}

	for procNum, entry := range NfsDispatchTable {
		if noAuthProcs[procNum] {
			assert.False(t, entry.NeedsAuth,
				"NFS procedure %d (%s) should not require auth", procNum, entry.Name)
		} else {
			assert.True(t, entry.NeedsAuth,
				"NFS procedure %d (%s) should require auth", procNum, entry.Name)
		}
	}
}

// ============================================================================
// Version Negotiation Tests
// ============================================================================

// parseRPCReply extracts the AcceptStat and (for PROG_MISMATCH) the low/high
// version range from an RPC reply byte slice. The input includes the 4-byte
// RPC fragment header prefix.
func parseRPCReply(t *testing.T, data []byte) (xid, acceptStat, lowVersion, highVersion uint32) {
	t.Helper()

	require.True(t, len(data) >= 4+24, "reply too short: need at least 28 bytes, got %d", len(data))

	// Skip 4-byte fragment header
	body := data[4:]
	xid = binary.BigEndian.Uint32(body[0:4])
	// body[4:8] = MsgType (1 = REPLY)
	// body[8:12] = ReplyState (0 = MSG_ACCEPTED)
	// body[12:16] = Verf Flavor (0 = AUTH_NULL)
	// body[16:20] = Verf Body Length (0)
	acceptStat = binary.BigEndian.Uint32(body[20:24])

	if acceptStat == rpc.RPCProgMismatch && len(body) >= 32 {
		lowVersion = binary.BigEndian.Uint32(body[24:28])
		highVersion = binary.BigEndian.Uint32(body[28:32])
	}

	return
}

// makeTestCall constructs a minimal RPC call message for dispatch testing.
func makeTestCall(xid, program, version, procedure uint32) *rpc.RPCCallMessage {
	return &rpc.RPCCallMessage{
		XID:       xid,
		Program:   program,
		Version:   version,
		Procedure: procedure,
		Cred: rpc.OpaqueAuth{
			Flavor: rpc.AuthNull,
			Body:   []byte{},
		},
		Verf: rpc.OpaqueAuth{
			Flavor: rpc.AuthNull,
			Body:   []byte{},
		},
	}
}

// TestDispatch_VersionNegotiation tests the consolidated Dispatch entry point
// for correct version negotiation behavior across all supported programs.
func TestDispatch_VersionNegotiation(t *testing.T) {
	ctx := context.Background()
	clientAddr := "10.0.0.1:12345"

	// Minimal deps with nil handlers -- sufficient for version rejection tests
	// since the version check happens before handler dispatch.
	deps := &DispatchDeps{}

	tests := []struct {
		name        string
		program     uint32
		version     uint32
		procedure   uint32
		wantStat    uint32
		wantLow     uint32
		wantHigh    uint32
		expectReply bool // true if rpcReply (second return) should be non-nil
	}{
		{
			name:        "NFS_v2_rejected_with_PROG_MISMATCH",
			program:     rpc.ProgramNFS,
			version:     2,
			procedure:   0,
			wantStat:    rpc.RPCProgMismatch,
			wantLow:     rpc.NFSVersion3,
			wantHigh:    rpc.NFSVersion4,
			expectReply: true,
		},
		{
			name:        "NFS_v5_rejected_with_PROG_MISMATCH",
			program:     rpc.ProgramNFS,
			version:     5,
			procedure:   0,
			wantStat:    rpc.RPCProgMismatch,
			wantLow:     rpc.NFSVersion3,
			wantHigh:    rpc.NFSVersion4,
			expectReply: true,
		},
		{
			name:        "NFS_v1_rejected_with_PROG_MISMATCH",
			program:     rpc.ProgramNFS,
			version:     1,
			procedure:   0,
			wantStat:    rpc.RPCProgMismatch,
			wantLow:     rpc.NFSVersion3,
			wantHigh:    rpc.NFSVersion4,
			expectReply: true,
		},
		{
			name:        "NFS_v4_without_handler_returns_PROG_MISMATCH_v3_only",
			program:     rpc.ProgramNFS,
			version:     rpc.NFSVersion4,
			procedure:   0,
			wantStat:    rpc.RPCProgMismatch,
			wantLow:     rpc.NFSVersion3,
			wantHigh:    rpc.NFSVersion3,
			expectReply: true,
		},
		{
			name:        "NLM_v1_rejected_with_PROG_MISMATCH",
			program:     rpc.ProgramNLM,
			version:     1,
			procedure:   0,
			wantStat:    rpc.RPCProgMismatch,
			wantLow:     rpc.NLMVersion4,
			wantHigh:    rpc.NLMVersion4,
			expectReply: true,
		},
		{
			name:        "NSM_v2_rejected_with_PROG_MISMATCH",
			program:     rpc.ProgramNSM,
			version:     2,
			procedure:   0,
			wantStat:    rpc.RPCProgMismatch,
			wantLow:     rpc.NSMVersion1,
			wantHigh:    rpc.NSMVersion1,
			expectReply: true,
		},
		{
			name:        "Mount_MNT_v1_rejected_with_PROG_MISMATCH",
			program:     rpc.ProgramMount,
			version:     1,
			procedure:   mount.MountProcMnt,
			wantStat:    rpc.RPCProgMismatch,
			wantLow:     rpc.MountVersion3,
			wantHigh:    rpc.MountVersion3,
			expectReply: true,
		},
		{
			name:        "unknown_program_returns_PROG_UNAVAIL",
			program:     999999,
			version:     1,
			procedure:   0,
			wantStat:    rpc.RPCProgUnavail,
			expectReply: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := makeTestCall(0x1234, tt.program, tt.version, tt.procedure)

			replyData, rpcReply, err := Dispatch(ctx, call, []byte{}, clientAddr, deps)
			require.NoError(t, err, "Dispatch should not return system error for version negotiation")

			if tt.expectReply {
				require.NotNil(t, rpcReply, "rpcReply should be non-nil for rejected versions")
				assert.Nil(t, replyData, "replyData should be nil when rpcReply is used")

				xid, acceptStat, low, high := parseRPCReply(t, rpcReply)
				assert.Equal(t, uint32(0x1234), xid, "XID should be echoed back")
				assert.Equal(t, tt.wantStat, acceptStat, "AcceptStat mismatch")

				if tt.wantStat == rpc.RPCProgMismatch {
					assert.Equal(t, tt.wantLow, low, "Low version mismatch")
					assert.Equal(t, tt.wantHigh, high, "High version mismatch")
				}
			}
		})
	}
}

// TestDispatch_NFS_v3_NULL_accepted verifies that NFSv3 NULL procedure is dispatched.
// The NULL procedure requires no special setup and returns an empty response.
func TestDispatch_NFS_v3_NULL_accepted(t *testing.T) {
	ctx := context.Background()
	clientAddr := "10.0.0.1:12345"

	// NULL procedure needs a v3 handler. We test that the dispatch reaches
	// the v3 handler path (no PROG_MISMATCH), even though the handler will
	// return nil since we don't provide a full Handler.
	deps := &DispatchDeps{}

	call := makeTestCall(0xABCD, rpc.ProgramNFS, rpc.NFSVersion3, types.NFSProcNull)

	replyData, rpcReply, err := Dispatch(ctx, call, []byte{}, clientAddr, deps)
	require.NoError(t, err)

	// v3 dispatch should NOT return an RPC error reply for a valid version.
	// It routes to dispatchNFSv3Procedure which looks up the procedure table.
	// NULL is in the table but the handler may fail without a real Handler;
	// the key assertion is that we did NOT get a version mismatch.
	assert.Nil(t, rpcReply, "NFSv3 NULL should not produce an RPC error reply (PROG_MISMATCH)")

	// replyData may be nil (handler panics without real deps) or non-nil;
	// the important thing is no PROG_MISMATCH was returned.
	_ = replyData
}

// TestDispatch_Mount_Non_MNT_v1_not_version_rejected verifies that Mount
// procedures other than MNT accept v1/v2 (macOS sends UMNT with mount v1).
// The handler may fail on data parsing, but the version should not be rejected.
func TestDispatch_Mount_Non_MNT_v1_not_version_rejected(t *testing.T) {
	ctx := context.Background()
	clientAddr := "10.0.0.1:12345"
	deps := &DispatchDeps{}

	// UMNT with v1 should NOT be rejected at the version check level.
	// The handler may fail on parsing the empty data, but that is a handler
	// error, not a version rejection.
	call := makeTestCall(0x5678, rpc.ProgramMount, 1, mount.MountProcUmnt)

	_, rpcReply, _ := Dispatch(ctx, call, []byte{}, clientAddr, deps)

	// The critical assertion: no PROG_MISMATCH reply for UMNT with v1.
	// A handler error is acceptable, but a version rejection is not.
	assert.Nil(t, rpcReply, "Mount UMNT with v1 should not produce PROG_MISMATCH")
}

// TestDispatch_Mount_NULL_v1_accepted verifies that Mount NULL with v1 works.
// NULL procedures typically require no data and succeed with minimal deps.
func TestDispatch_Mount_NULL_v1_accepted(t *testing.T) {
	ctx := context.Background()
	clientAddr := "10.0.0.1:12345"
	deps := &DispatchDeps{}

	call := makeTestCall(0x9ABC, rpc.ProgramMount, 1, mount.MountProcNull)

	replyData, rpcReply, err := Dispatch(ctx, call, []byte{}, clientAddr, deps)
	require.NoError(t, err)

	// No version rejection for NULL with v1
	assert.Nil(t, rpcReply, "Mount NULL with v1 should not produce PROG_MISMATCH")
	// NULL procedure returns empty success
	_ = replyData
}
