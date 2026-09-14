package v3

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
