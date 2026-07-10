package metadata_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

// TestConcurrentMkdirNoParentLinkConflict reproduces #1571: concurrent mkdirs in
// one parent each read+write the parent's link-count key (the ".." bump), which
// is the single shared key in an otherwise disjoint create transaction. BadgerDB
// SSI aborts the losers as write conflicts; under sustained same-parent mkdir
// load the retry budget exhausts and the abort escapes as StoreError{ErrConflict}
// — surfaced to an SMB client as a hard "mkdir failed" (StatusInternalError).
//
// The retry budget is pinned to 1 so the conflict is deterministic: with the
// shared-key read+write still present, some mkdirs escape as ErrConflict; with
// the parent link-count bump serialized per-parent, none do. The final nlink
// (2 + subdir count) guards that the fix keeps the counter exact.
//
// Run: go test ./pkg/metadata -run TestConcurrentMkdirNoParentLinkConflict -v
func TestConcurrentMkdirNoParentLinkConflict(t *testing.T) {
	const (
		concurrency = 16
		perWorker   = 25
	)
	total := concurrency * perWorker

	// Single attempt: no retry safety net, so the hot-key SSI conflict surfaces
	// deterministically instead of being masked by the 20-attempt backoff loop.
	restore := badger.SetMaxTransactionRetriesForTest(1)
	defer restore()

	ctx := context.Background()
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const share = "/test"
	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(share, svc0store(store)))
	auth := &metadata.AuthContext{
		Context: ctx, AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		ClientAddr: "127.0.0.1",
	}
	dirAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777}

	var ok, conflict int64
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_, _, err := svc.CreateDirectory(auth, rootHandle, fmt.Sprintf("d-%d-%d", w, i), dirAttr)
				switch {
				case err == nil:
					atomic.AddInt64(&ok, 1)
				case metadata.IsConflictError(err):
					atomic.AddInt64(&conflict, 1)
				default:
					t.Errorf("unexpected mkdir error: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	t.Logf("mkdir: %d ok, %d conflict-escaped (of %d)", ok, conflict, total)
	require.Zero(t, conflict,
		"concurrent same-parent mkdirs escaped as ErrConflict — the parent link-count read+write is still contended (#1571)")
	require.Equal(t, int64(total), ok)

	// Parent nlink must be exactly 2 (self + ".") + one per child directory.
	got, err := store.GetFile(ctx, rootHandle)
	require.NoError(t, err)
	require.Equal(t, uint32(2+total), got.Nlink, "parent directory link count drifted under concurrent mkdir")
}

// TestConcurrentDirRenameNoParentLinkConflict is the Move-path sibling of #1571:
// renaming a directory across parents decrements the source parent's link-count
// key and increments the destination's, the same shared counter keys mkdir/rmdir
// bump. Without serialization, concurrent cross-parent dir renames race the
// destination's key (and any concurrent mkdir/rmdir there) and BadgerDB SSI
// aborts them — escaping as ErrConflict under a tight retry budget.
func TestConcurrentDirRenameNoParentLinkConflict(t *testing.T) {
	const n = 200

	restore := badger.SetMaxTransactionRetriesForTest(1)
	defer restore()

	ctx := context.Background()
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const share = "/test"
	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(share, svc0store(store)))
	auth := &metadata.AuthContext{
		Context: ctx, AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		ClientAddr: "127.0.0.1",
	}
	dirAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777}

	// Two parents A and B, and n subdirectories under A to move into B concurrently.
	srcDir, _, err := svc.CreateDirectory(auth, rootHandle, "A", dirAttr)
	require.NoError(t, err)
	dstDir, _, err := svc.CreateDirectory(auth, rootHandle, "B", dirAttr)
	require.NoError(t, err)
	fromHandle, err := metadata.EncodeShareHandle(share, srcDir.ID)
	require.NoError(t, err)
	toHandle, err := metadata.EncodeShareHandle(share, dstDir.ID)
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		_, _, err := svc.CreateDirectory(auth, fromHandle, fmt.Sprintf("c-%d", i), dirAttr)
		require.NoError(t, err)
	}

	var ok, conflict int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("c-%d", i)
			_, err := svc.Move(auth, fromHandle, name, toHandle, name)
			switch {
			case err == nil:
				atomic.AddInt64(&ok, 1)
			case metadata.IsConflictError(err):
				atomic.AddInt64(&conflict, 1)
			default:
				t.Errorf("unexpected rename error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	t.Logf("rename: %d ok, %d conflict-escaped (of %d)", ok, conflict, n)
	require.Zero(t, conflict,
		"concurrent cross-parent dir renames escaped as ErrConflict — parent link-count RMW still contended (#1571)")
	require.Equal(t, int64(n), ok)

	// Every child moved out of A into B: A back to empty (2), B holds all n.
	gotA, err := store.GetFile(ctx, fromHandle)
	require.NoError(t, err)
	require.Equal(t, uint32(2), gotA.Nlink, "source parent link count drifted under concurrent rename")
	gotB, err := store.GetFile(ctx, toHandle)
	require.NoError(t, err)
	require.Equal(t, uint32(2+n), gotB.Nlink, "destination parent link count drifted under concurrent rename")
}
