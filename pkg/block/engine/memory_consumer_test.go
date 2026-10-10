package engine

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
	"github.com/stretchr/testify/require"
)

type memoryConsumerFixture struct {
	bs     *Store
	local  *memorylocal.MemoryStore
	remote *warmScopeRemote
	data   []byte
	hash   block.ContentHash
}

func newMemoryConsumerFixture(t *testing.T) *memoryConsumerFixture {
	t.Helper()
	ctx := context.Background()
	ms := badgertest.NewInMemory(t)
	rem := &warmScopeRemote{Store: remotememory.New(), entered: make(chan struct{}), release: make(chan struct{})}
	data := bytes.Repeat([]byte{'o'}, 4096)
	hash := seedSyncedRemoteChunk(t, ms, rem, ms, "memory-file", 0, data)
	local := memorylocal.New()
	cfg := DefaultConfig()
	cfg.ManualSync = true
	rs := NewRemoteSync(local, rem, ms, cfg)
	rs.SetSyncedHashStore(ms)
	rs.SetRemoteBlockStore(rem)
	bs, err := New(BlockStoreConfig{Local: local, Remote: rem, RemoteSync: rs, FileChunkStore: ms,
		SyncedHashStore: ms, Coordinator: &reapCoordinator{store: ms}})
	require.NoError(t, err)
	require.NoError(t, bs.Start(ctx))
	t.Cleanup(func() { _ = bs.Close() })
	return &memoryConsumerFixture{bs: bs, local: local, remote: rem, data: data, hash: hash}
}

func TestMemoryWarmCannotReplaceNewerContent(t *testing.T) {
	for _, mutation := range []string{"write-after-plan", "dirty-before-plan", "truncate-absent", "delete-recreate"} {
		t.Run(mutation, func(t *testing.T) {
			f := newMemoryConsumerFixture(t)
			ctx := context.Background()
			require.Zero(t, f.local.WriteVersion())
			fresh := bytes.Repeat([]byte{'n'}, len(f.data))
			want := fresh
			if mutation == "dirty-before-plan" {
				_, err := f.bs.WriteAt(ctx, "memory-file", nil, fresh, 0)
				require.NoError(t, err)
			}
			var mutationErr error
			result, err := f.bs.WarmAll(ctx, func(done, total int64) {
				if done != 0 {
					return
				}
				require.EqualValues(t, 1, total)
				switch mutation {
				case "write-after-plan":
					_, mutationErr = f.bs.WriteAt(ctx, "memory-file", nil, fresh, 0)
				case "truncate-absent":
					refs := []block.ChunkRef{{Hash: f.hash, Offset: 0, Size: uint32(len(f.data))}}
					_, mutationErr = f.bs.Truncate(ctx, "memory-file", refs, 0)
					want = make([]byte, len(f.data))
				case "delete-recreate":
					mutationErr = f.bs.Delete(ctx, "memory-file", nil)
					if mutationErr == nil {
						_, mutationErr = f.bs.WriteAt(ctx, "memory-file", nil, fresh, uint64(len(f.data)))
					}
					want = append(make([]byte, len(f.data)), fresh...)
				}
			})
			require.NoError(t, mutationErr)
			require.NoError(t, err)
			require.EqualValues(t, 1, result.BlocksFetched, "warm must actually download the planned row")
			got := make([]byte, len(want))
			_, err = f.bs.ReadAt(ctx, "memory-file", got, 0)
			require.NoError(t, err)
			require.True(t, bytes.Equal(want, got), "planned remote data replaced the file's current bytes")
		})
	}
}

func TestMemoryDemandFetchPreservesConcurrentWrite(t *testing.T) {
	f := newMemoryConsumerFixture(t)
	ctx := context.Background()
	f.remote.gateHash = f.hash
	var once sync.Once
	release := func() { once.Do(func() { close(f.remote.release) }) }
	t.Cleanup(release)
	readDone := make(chan error, 1)
	go func() {
		_, err := f.bs.ReadAt(ctx, "memory-file", make([]byte, len(f.data)), 0)
		readDone <- err
	}()
	select {
	case <-f.remote.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("demand read did not reach remote")
	}
	fresh := bytes.Repeat([]byte{'n'}, len(f.data))
	_, err := f.bs.WriteAt(ctx, "memory-file", nil, fresh, 0)
	require.NoError(t, err)
	release()
	require.NoError(t, waitWarmScope(t, readDone))
	got := make([]byte, len(fresh))
	_, err = f.bs.ReadAt(ctx, "memory-file", got, 0)
	require.NoError(t, err)
	require.True(t, bytes.Equal(fresh, got), "completed remote fetch overwrote the acknowledged write")
}

func TestMemoryTruncateAndDiscardClearReportedDirtyBytes(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()
	_, err := bs.WriteAt(ctx, "file", nil, []byte("original"), 0)
	require.NoError(t, err)
	_, err = bs.WriteAt(ctx, "file", nil, []byte("NEW!"), 2)
	require.NoError(t, err)
	require.EqualValues(t, 8, bs.GetStatsLite().UnsyncedBytes)
	_, err = bs.Truncate(ctx, "file", nil, 4)
	require.NoError(t, err)
	require.EqualValues(t, 4, bs.GetStatsLite().UnsyncedBytes)
	require.NoError(t, bs.DiscardLocalContent(ctx, "file"))
	require.Zero(t, bs.GetStatsLite().UnsyncedBytes, "discard must not leave phantom pending upload bytes")
	dirty, err := bs.HasDirty(ctx, "file")
	require.NoError(t, err)
	require.False(t, dirty)
}

func TestMemoryDemandFetchKeepsTruncatedPrefix(t *testing.T) {
	f := newMemoryConsumerFixture(t)
	ctx := context.Background()
	f.remote.gateHash = f.hash
	var once sync.Once
	release := func() { once.Do(func() { close(f.remote.release) }) }
	t.Cleanup(release)
	got := make([]byte, len(f.data))
	done := make(chan error, 1)
	go func() { _, err := f.bs.ReadAt(ctx, "memory-file", got, 0); done <- err }()
	select {
	case <-f.remote.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("demand read did not reach remote")
	}
	refs := []block.ChunkRef{{Hash: f.hash, Offset: 0, Size: uint32(len(f.data))}}
	_, err := f.bs.Truncate(ctx, "memory-file", refs, uint64(len(f.data)/2))
	require.NoError(t, err)
	release()
	require.NoError(t, waitWarmScope(t, done))
	want := append(bytes.Clone(f.data[:len(f.data)/2]), make([]byte, len(f.data)/2)...)
	require.True(t, bytes.Equal(want, got), "straddling stale fetch lost the surviving prefix or restored the tail")
}

func TestMemoryDemandFetchRefusesExpiredFenceHistoryAndRetriesFresh(t *testing.T) {
	f := newMemoryConsumerFixture(t)
	ctx := context.Background()
	f.remote.gateHash = f.hash
	var once sync.Once
	release := func() { once.Do(func() { close(f.remote.release) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := f.bs.ReadAt(ctx, "memory-file", make([]byte, len(f.data)), 0)
		done <- err
	}()
	select {
	case <-f.remote.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("demand read did not reach remote")
	}
	// More invalidations than the bounded history retains, while this read
	// still carries its initial version. The conservative floor must refuse
	// explicitly rather than returning absent local bytes as successful zeros.
	for i := 0; i < 2048; i++ {
		require.NoError(t, f.bs.DiscardLocalContent(ctx, fmt.Sprintf("unrelated-%d", i)))
	}
	release()
	require.ErrorIs(t, waitWarmScope(t, done), memorylocal.ErrHydrateHistoryExpired)
	got := make([]byte, len(f.data))
	_, err := f.bs.ReadAt(ctx, "memory-file", got, 0)
	require.NoError(t, err)
	require.Equal(t, f.data, got, "fresh request must recover the legitimate remote data")
}

func TestMemoryDemandReadAfterDiscardUsesFreshVersion(t *testing.T) {
	f := newMemoryConsumerFixture(t)
	ctx := context.Background()
	_, err := f.bs.WarmAll(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, f.bs.DiscardLocalContent(ctx, "memory-file"))
	got := make([]byte, len(f.data))
	_, err = f.bs.ReadAt(ctx, "memory-file", got, 0)
	require.NoError(t, err)
	require.Equal(t, f.data, got, "fresh request must cross the completed discard's version fence")
}
