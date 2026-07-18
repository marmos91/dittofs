package metadata_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

// TestCreateFile_ConcurrentSameName_NoOrphan is the constraint-A guard for the
// #1735 dirent cache: the negative dirent cache accelerates only the OUTER
// pre-transaction existence check; the authoritative in-transaction recheck
// still hits the real badger txn and joins Badger's SSI conflict read-set. So N
// concurrent creates of the SAME (parent,name) must resolve to exactly ONE
// winner, the rest AlreadyExists, and — critically — NO orphaned inode (a
// double-create would PutFile a second inode whose dirent lost, inflating the
// file count).
//
// If the dirent cache ever served the in-txn recheck, both racers would see
// ABSENT, both would commit, and UsedFiles would exceed the single-winner count.
func TestCreateFile_ConcurrentSameName_NoOrphan(t *testing.T) {
	const concurrency = 24

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
	require.NoError(t, svc.RegisterStoreForShare(share, metadata.Store(store)))
	auth := &metadata.AuthContext{
		Context: ctx, AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		ClientAddr: "127.0.0.1",
	}
	attr := &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}

	var okCount, existsCount int64
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, cErr := svc.CreateFile(auth, rootHandle, "collide", attr)
			switch {
			case cErr == nil:
				atomic.AddInt64(&okCount, 1)
			case isAlreadyExists(cErr):
				atomic.AddInt64(&existsCount, 1)
			default:
				t.Errorf("unexpected create error: %v", cErr)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, int64(1), okCount, "exactly one concurrent create must win")
	require.Equal(t, int64(concurrency-1), existsCount, "all losers must get AlreadyExists")

	// Orphan check: UsedFiles counts every inode. root + the single "collide"
	// child = 2. A double-create would leave an extra orphaned inode (3+).
	stats, err := store.GetFilesystemStatistics(ctx, rootHandle)
	require.NoError(t, err)
	require.Equal(t, uint64(2), stats.UsedFiles,
		"orphaned inode detected: a concurrent create double-committed under one name")

	// The surviving name resolves to a single, live regular-file inode.
	h, err := store.GetChild(ctx, rootHandle, "collide")
	require.NoError(t, err)
	f, err := store.GetFile(ctx, h)
	require.NoError(t, err)
	require.Equal(t, metadata.FileTypeRegular, f.Type)
}

func isAlreadyExists(err error) bool {
	var se *metadata.StoreError
	return errors.As(err, &se) && se.Code == metadata.ErrAlreadyExists
}
