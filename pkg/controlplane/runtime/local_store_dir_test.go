package runtime

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// TestRuntime_LocalStoreDir_Found exercises the happy path: a share
// registered with a configured per-share local data dir round-trips
// through Runtime.LocalStoreDir.
//
// We use the test-only InjectShareForTesting hook to bypass the full
// AddShare composition path — which requires DB-backed BlockStoreConfig,
// metadata service registration, and a real local fs store. The handler
// only cares about the final string the shares service hands it.
func TestRuntime_LocalStoreDir_Found(t *testing.T) {
	rt := New(nil)
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "myshare",
		MetadataStore: "memory",
	})
	// Drive the per-share local-store dir directly. The accessor used by
	// the migration status handler does not require the BlockStore field
	// to be populated.
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting("myshare", "/var/lib/dittofs/shares/myshare"))

	dir, err := rt.LocalStoreDir("myshare")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/dittofs/shares/myshare", dir)
}

// TestRuntime_LocalStoreDir_NotFound asserts the unknown-share case
// surfaces an ErrShareNotFound-wrapped error so the REST handler can map
// it to 404 deterministically.
func TestRuntime_LocalStoreDir_NotFound(t *testing.T) {
	rt := New(nil)

	_, err := rt.LocalStoreDir("nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, shares.ErrShareNotFound),
		"expected ErrShareNotFound, got %v", err)
}

// TestRuntime_LocalStoreDir_EmptyForMemoryBackend asserts that a share
// with no configured local-store path (e.g., memory backend) yields an
// empty string + nil error. The handler treats "" as "no journal
// available" and proceeds without crashing.
func TestRuntime_LocalStoreDir_EmptyForMemoryBackend(t *testing.T) {
	rt := New(nil)
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "memshare",
		MetadataStore: "memory",
	})
	// No SetLocalStoreDirForTesting call → field stays empty.

	dir, err := rt.LocalStoreDir("memshare")
	require.NoError(t, err)
	assert.Empty(t, dir, "memory-backed share must produce empty data dir")
}
