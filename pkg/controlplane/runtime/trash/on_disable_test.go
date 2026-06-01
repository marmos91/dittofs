package trash

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnDisableAutoEmpties verifies that disabling trash permanently purges
// every recycled entry (freeing its blocks) and removes the #recycle root dir
// from the share root entirely.
func TestOnDisableAutoEmpties(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	tt.recycle("a.txt")
	tt.recycle("b.txt")

	require.NoError(t, tt.svc.OnDisable(tt.ctx, tt.deps.shareName))

	// The #recycle root no longer exists under the share root.
	_, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, metadata.RecycleDirName)
	assert.True(t, metadata.IsNotFoundError(err), "#recycle dir must be removed on disable, got %v", err)

	// Both recycled files had their CAS blocks freed.
	assert.Len(t, tt.deps.freed, 2, "each recycled file should free its blocks on disable")
}

// TestOnDisableNoBinIsNoop verifies that disabling trash on a share that never
// recycled anything (no #recycle dir) is a no-op returning nil.
func TestOnDisableNoBinIsNoop(t *testing.T) {
	t.Parallel()
	tt := newTestTrash(t)

	require.NoError(t, tt.svc.OnDisable(tt.ctx, tt.deps.shareName))

	// Still no bin (none was ever created).
	_, err := tt.deps.svc.GetChild(tt.ctx.Context, tt.deps.rootHandle, metadata.RecycleDirName)
	assert.True(t, metadata.IsNotFoundError(err))
}
