package engine

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// The memory local tier deliberately has no journal version fence. It makes an
// obsolete warm hydrate visible even when the destination's index began empty.
type warmScopeRemote struct {
	*remotememory.Store
	gateHash block.ContentHash
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (r *warmScopeRemote) ReadChunk(ctx context.Context, id string, off, length int64, hash block.ContentHash) ([]byte, error) {
	if hash == r.gateHash {
		r.once.Do(func() { close(r.entered) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.Store.ReadChunk(ctx, id, off, length, hash)
}

type warmScopeFixture struct {
	bs       *Store
	rs       *RemoteSync
	metadata metadata.Store
	remote   *warmScopeRemote
	source   []byte
	srcRow   *block.FileChunk
	dstRow   *block.FileChunk
}

func newWarmScopeFixture(t *testing.T) *warmScopeFixture {
	t.Helper()
	ctx := context.Background()
	remote := &warmScopeRemote{Store: remotememory.New(), entered: make(chan struct{}), release: make(chan struct{})}
	carve := newCarveFixture(t, remote, defaultTestCarveBlockSize)
	source := bytes.Repeat([]byte{0x51}, 4096)
	require.NoError(t, carve.local.WriteAt(ctx, "warm-src", 0, source))
	require.NoError(t, carve.local.WriteAt(ctx, "warm-dst", 0, bytes.Repeat([]byte{0x62}, 4096)))
	require.NoError(t, carve.syncer.SyncNow(ctx))
	src, err := carve.ms.ListFileChunks(ctx, "warm-src")
	require.NoError(t, err)
	require.Len(t, src, 1)
	dst, err := carve.ms.ListFileChunks(ctx, "warm-dst")
	require.NoError(t, err)
	require.Len(t, dst, 1)
	mem := localmemory.New()
	cfg := DefaultConfig()
	cfg.ManualSync = true
	cfg.ParallelDownloads = 2
	rs := NewRemoteSync(mem, remote, carve.ms, cfg)
	rs.SetSyncedHashStore(carve.ms)
	rs.SetRemoteBlockStore(remote)
	bs, err := New(BlockStoreConfig{Local: mem, Remote: remote, RemoteSync: rs, FileChunkStore: carve.ms, SyncedHashStore: carve.ms})
	require.NoError(t, err)
	require.NoError(t, bs.Start(ctx))
	t.Cleanup(func() { _ = bs.Close() })
	return &warmScopeFixture{bs: bs, rs: rs, metadata: carve.ms, remote: remote, source: source, srcRow: src[0], dstRow: dst[0]}
}

func (f *warmScopeFixture) replace(ctx context.Context) error {
	return f.bs.WithPayloadScope(ctx, []string{"warm-src", "warm-dst"}, true, func(ctx context.Context) error {
		row := *f.srcRow
		row.ID = f.dstRow.ID
		if err := f.metadata.Put(ctx, &row); err != nil {
			return err
		}
		return f.bs.DiscardLocalContent(ctx, "warm-dst")
	})
}

func waitWarmScope(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("warm operation did not finish")
		return nil
	}
}

func TestWarmScopeRetainsAdmissionThroughHydration(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	require.Zero(t, f.bs.local.WriteVersion(), "fixture must expose the unfenced local tier")
	require.Empty(t, f.bs.local.ListFiles(ctx), "destination starts without local ranges")
	f.remote.gateHash = f.dstRow.Hash
	warmed := make(chan error, 1)
	go func() { _, err := f.bs.WarmAll(ctx, nil); warmed <- err }()
	select {
	case <-f.remote.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warm never fetched the old destination")
	}
	replaced := make(chan error, 1)
	go func() { replaced <- f.replace(ctx) }()
	finished := false
	select {
	case err := <-replaced:
		finished = true
		require.NoError(t, err)
	case <-time.After(20 * time.Millisecond):
	}
	close(f.remote.release)
	require.NoError(t, waitWarmScope(t, warmed))
	if !finished {
		require.NoError(t, waitWarmScope(t, replaced))
	}
	got := make([]byte, len(f.source))
	_, err := f.bs.ReadAt(ctx, "warm-dst", got, 0)
	require.NoError(t, err)
	require.Equal(t, f.source, got, "an old warm target must not restore replaced destination bytes")
}

type warmScopeEnumeration struct {
	metadata.Store
	enumerated chan struct{}
	listed     chan struct{}
}

func (s *warmScopeEnumeration) ListFileChunks(ctx context.Context, id string) ([]*block.FileChunk, error) {
	select {
	case s.listed <- struct{}{}:
	default:
	}
	return s.Store.ListFileChunks(ctx, id)
}

func (s *warmScopeEnumeration) EnumeratePayloads(ctx context.Context, fn func(string) error) error {
	err := s.Store.EnumeratePayloads(ctx, fn)
	close(s.enumerated)
	return err
}

func TestWarmScopeResolvesRowsAfterAdmission(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	watched := &warmScopeEnumeration{Store: f.metadata, enumerated: make(chan struct{}), listed: make(chan struct{}, 1)}
	f.rs.fileChunkStore = watched
	warmed := make(chan error, 1)
	require.NoError(t, f.bs.WithPayloadScope(ctx, []string{"warm-src", "warm-dst"}, true, func(ctx context.Context) error {
		go func() { _, err := f.bs.WarmAll(context.Background(), nil); warmed <- err }()
		select {
		case <-watched.enumerated:
		case <-time.After(5 * time.Second):
			t.Fatal("warm never enumerated payloads")
		}
		// The old rows must remain unresolved until this replacement completes.
		// A guard acquired only by download workers would already hold stale rows.
		select {
		case <-watched.listed:
			t.Error("warm resolved a manifest before obtaining payload admission")
		case <-time.After(20 * time.Millisecond):
		}
		return f.replace(ctx)
	}))
	require.NoError(t, waitWarmScope(t, warmed))
	got := make([]byte, len(f.source))
	_, err := f.bs.ReadAt(ctx, "warm-dst", got, 0)
	require.NoError(t, err)
	require.Equal(t, f.source, got)
}

func TestWarmScopeProgressCallbackMayAcquireExclusiveAdmission(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	warmed := make(chan error, 1)
	var counts [][2]int64
	go func() {
		_, err := f.bs.WarmAll(ctx, func(done, total int64) {
			counts = append(counts, [2]int64{done, total})
			if done == 0 {
				err := f.bs.WithPayloadScope(ctx, []string{"warm-dst"}, true, func(ctx context.Context) error {
					_, err := f.bs.ReadAt(ctx, "warm-dst", make([]byte, 4096), 0)
					return err
				})
				if err != nil {
					panic(err)
				}
			}
		})
		warmed <- err
	}()
	require.NoError(t, waitWarmScope(t, warmed))
	require.Equal(t, [][2]int64{{0, 2}, {1, 2}, {2, 2}}, counts, "progress must remain ordered and finish before return")
}

func TestWarmScopeCancellationReleasesAdmission(t *testing.T) {
	f := newWarmScopeFixture(t)
	f.remote.gateHash = f.dstRow.Hash
	ctx, cancel := context.WithCancel(context.Background())
	warmed := make(chan error, 1)
	go func() { _, err := f.bs.WarmAll(ctx, nil); warmed <- err }()
	select {
	case <-f.remote.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warm never started")
	}
	cancel()
	require.ErrorIs(t, waitWarmScope(t, warmed), context.Canceled)
	require.NoError(t, f.replace(context.Background()))
	f.bs.admission.mu.Lock()
	defer f.bs.admission.mu.Unlock()
	require.Empty(t, f.bs.admission.entries)
}
