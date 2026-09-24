package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// mkdirAt creates a directory under parent and returns its handle.
func mkdirAt(t *testing.T, fx *testFixture, parent metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()
	dir, _, err := fx.service.CreateDirectory(fx.rootContext(), parent, name, &metadata.FileAttr{Mode: 0o755})
	require.NoError(t, err)
	handle, err := metadata.EncodeFileHandle(dir)
	require.NoError(t, err)
	return handle
}

// requireInvalidArgument asserts a refusal carrying ErrInvalidArgument, which is
// the EINVAL POSIX rename(2) returns for a directory loop.
func requireInvalidArgument(t *testing.T, err error, msg string) {
	t.Helper()
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr, msg)
	require.Equal(t, metadata.ErrInvalidArgument, storeErr.Code, msg)
}

// TestMove_DirectoryUnderOwnDescendantRefused is the sequential loop check: no
// concurrency at all, so nothing but an ancestor test can refuse it. Moving /a
// under /a/b detaches the whole subtree from the share root — /a's parent
// becomes /a/b, which is only reachable through /a — and nothing ever collects
// the cycle because every inode in it keeps a positive link count.
func TestMove_DirectoryUnderOwnDescendantRefused(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	aHandle := mkdirAt(t, fx, fx.rootHandle, "a")
	bHandle := mkdirAt(t, fx, aHandle, "b")

	_, _, err := fx.service.Move(fx.rootContext(), fx.rootHandle, "a", bHandle, "a")
	requireInvalidArgument(t, err, "moving /a under its own descendant /a/b must be refused")

	// The namespace must be untouched: /a still hangs off the share root, by
	// both its parent edge and the root's entry for it.
	parent, err := fx.store.GetParent(ctx, aHandle)
	require.NoError(t, err)
	require.Equal(t, string(fx.rootHandle), string(parent), "/a must still be a child of the share root")
	still, err := fx.store.GetChild(ctx, fx.rootHandle, "a")
	require.NoError(t, err)
	require.Equal(t, string(aHandle), string(still))
	_, err = fx.store.GetChild(ctx, bHandle, "a")
	require.Error(t, err, "/a/b must not have gained an entry")
}

// TestMove_DirectoryIntoItselfRefused covers the degenerate case where the
// destination parent is the source itself rather than one of its descendants.
func TestMove_DirectoryIntoItselfRefused(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	aHandle := mkdirAt(t, fx, fx.rootHandle, "a")

	_, _, err := fx.service.Move(fx.rootContext(), fx.rootHandle, "a", aHandle, "a")
	requireInvalidArgument(t, err, "moving /a into /a must be refused")
}

// reachesRoot walks parent edges from handle and reports whether the share root
// is reached. A cycle never reaches it; the bound stops the walk instead of
// hanging the test.
func reachesRoot(t *testing.T, fx *testFixture, handle metadata.FileHandle) bool {
	t.Helper()
	ctx := context.Background()
	for range 64 {
		if string(handle) == string(fx.rootHandle) {
			return true
		}
		parent, err := fx.store.GetParent(ctx, handle)
		if err != nil {
			return false
		}
		handle = parent
	}
	return false
}

// TestMove_ConcurrentReciprocalDirectoryMoves runs the two halves of a cycle at
// once: /a moves under /b while /b moves under /a. Exactly one may commit, and
// whichever loses must leave every directory reachable from the share root.
//
// It still cannot stand in for the sequential case above. On this backend both
// renames re-resolve different names in one parent, so neither re-resolution
// collides and a build with no loop check commits both — which this does catch.
// On a backend that refuses one of them for an unrelated reason, ErrConflict
// from the entry re-resolution satisfies the count on its own, and the test
// passes without the loop check ever running.
func TestMove_ConcurrentReciprocalDirectoryMoves(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)

	aHandle := mkdirAt(t, fx, fx.rootHandle, "a")
	bHandle := mkdirAt(t, fx, fx.rootHandle, "b")

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, move := range []struct {
		name string
		dst  metadata.FileHandle
	}{{"a", bHandle}, {"b", aHandle}} {
		go func() {
			<-start
			_, _, err := fx.service.Move(fx.rootContext(), fx.rootHandle, move.name, move.dst, move.name)
			errs <- err
		}()
	}
	close(start)

	failures := 0
	for range 2 {
		if err := <-errs; err != nil {
			failures++
		}
	}
	require.GreaterOrEqual(t, failures, 1, "at least one reciprocal move must be refused")

	require.True(t, reachesRoot(t, fx, aHandle), "/a must stay reachable from the share root")
	require.True(t, reachesRoot(t, fx, bHandle), "/b must stay reachable from the share root")
}
