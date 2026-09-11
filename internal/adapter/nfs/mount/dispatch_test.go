package mount

import (
	"testing"

	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
