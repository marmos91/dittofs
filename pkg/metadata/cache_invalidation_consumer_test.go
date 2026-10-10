package metadata_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

type cacheInvalidationCommitKey struct{}

type cacheInvalidationCommitStore struct {
	metadata.Store
	once      sync.Once
	committed chan struct{}
}

func (s *cacheInvalidationCommitStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	err := s.Store.WithTransaction(ctx, fn)
	if err == nil && ctx.Value(cacheInvalidationCommitKey{}) != nil {
		s.once.Do(func() { close(s.committed) })
	}
	return err
}

// Attribute mutation and deferred setid clearing must join a cache-only flush
// before returning, or its older snapshot can overwrite their invalidation.
// PrepareWrite consumes that snapshot even though its permission check re-reads
// the current inode; asserting only the persisted mode misses this regression.
func TestWriteCacheInvalidationConsumersWaitForFlush(t *testing.T) {
	for _, operation := range []string{"SetFileAttributes", "CommitWrite_setid"} {
		t.Run(operation, func(t *testing.T) {
			fx := newTestFixture(t)
			fx.service.SetDeferredCommit(true)
			initialMode, wantMode := uint32(0o666), uint32(0o640)
			if operation == "CommitWrite_setid" {
				initialMode, wantMode = 0o6666, 0o666
			}
			file, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "cache.bin", &metadata.FileAttr{Mode: initialMode, UID: 1000, GID: 1000})
			require.NoError(t, err)
			handle, err := metadata.EncodeFileHandle(file)
			require.NoError(t, err)
			size := uint64(64)
			_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{Size: &size})
			require.NoError(t, err)
			intent, err := fx.service.PrepareWrite(fx.userContext(), handle, size)
			require.NoError(t, err)
			require.Equal(t, initialMode, intent.PreWriteAttr.Mode)
			require.Equal(t, size, intent.PreWriteAttr.Size)

			resume := metadata.PauseCacheOnlyFlushForTest(t, fx.service, handle)
			store := &cacheInvalidationCommitStore{Store: fx.store, committed: make(chan struct{})}
			require.NoError(t, fx.service.RegisterStoreForShare(fx.shareName, store))
			auth := fx.userContext()
			auth.Context = context.WithValue(t.Context(), cacheInvalidationCommitKey{}, true)
			done := make(chan error, 1)
			go func() {
				if operation == "SetFileAttributes" {
					_, err := fx.service.SetFileAttributes(auth, handle, &metadata.SetAttrs{Mode: &wantMode})
					done <- err
					return
				}
				_, err := fx.service.CommitWrite(auth, intent)
				done <- err
			}()
			select {
			case <-store.committed:
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not commit its metadata")
			}
			persisted, err := fx.store.GetFile(t.Context(), handle)
			require.NoError(t, err)
			require.Equal(t, wantMode, persisted.Mode, "the mutation must have reached Badger before invalidation waits")
			require.Equal(t, size, persisted.Size)
			finished := false
			select {
			case err := <-done:
				finished = true
				require.NoError(t, err)
				t.Error("attribute mutation returned while a cache-only flush could still republish stale attributes")
			case <-time.After(100 * time.Millisecond):
			}
			resume()
			if !finished {
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("attribute mutation did not finish after the cache-only flush")
				}
			}
			next, err := fx.service.PrepareWrite(fx.userContext(), handle, size)
			require.NoError(t, err)
			require.Equal(t, wantMode, next.PreWriteAttr.Mode, "PrepareWrite must consume current attributes after invalidation, not the stale flush snapshot")
			require.Equal(t, size, next.PreWriteAttr.Size)
			require.Equal(t, file.PayloadID, next.PayloadID)
		})
	}
}
