package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	mount "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
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
