package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

type warmScopeRemote struct {
	*remotememory.Store
	gateHash block.ContentHash
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	hook     func(context.Context, block.ContentHash) error
	reads    atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
}

func (r *warmScopeRemote) ReadChunk(ctx context.Context, id string, off, length int64, hash block.ContentHash) ([]byte, error) {
	r.reads.Add(1)
	n := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for old := r.peak.Load(); n > old; old = r.peak.Load() {
		if r.peak.CompareAndSwap(old, n) {
			break
		}
	}
	if r.hook != nil {
		if err := r.hook(ctx, hash); err != nil {
			return nil, err
		}
	}
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
	local, err := journal.Open(t.TempDir(), journal.Config{})
	require.NoError(t, err)
	cfg := DefaultConfig()
	cfg.ManualSync = true
	cfg.ParallelDownloads = 2
	rs := NewRemoteSync(local, remote, carve.ms, cfg)
	rs.SetSyncedHashStore(carve.ms)
	rs.SetRemoteBlockStore(remote)
	bs, err := New(BlockStoreConfig{Local: local, Remote: remote, RemoteSync: rs, FileChunkStore: carve.ms, SyncedHashStore: carve.ms})
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
	require.Zero(t, f.bs.local.WriteVersion(), "fresh journal must expose the initial zero version")
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

func TestWarmScopeUnrelatedFileRemainsAvailable(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.remote.gateHash = f.dstRow.Hash
	warmed := make(chan error, 1)
	go func() { _, err := f.bs.WarmAll(ctx, nil); warmed <- err }()
	select {
	case <-f.remote.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warm never reached the gated destination")
	}

	// A replacement of the source must finish while the unrelated destination
	// is still downloading. A whole-share warm permit would keep it queued and
	// also block the following ordinary source write behind that writer.
	replaced := make(chan error, 1)
	go func() {
		replaced <- f.bs.WithPayloadScope(ctx, []string{"warm-src"}, true, func(context.Context) error { return nil })
	}()
	foreground := make(chan error, 1)
	go func() {
		_, err := f.bs.WriteAt(ctx, "warm-src", nil, []byte("new"), 0)
		foreground <- err
	}()
	select {
	case err := <-replaced:
		require.NoError(t, err)
	case <-time.After(time.Second):
		cancel()
		close(f.remote.release)
		_ = waitWarmScope(t, warmed)
		t.Fatal("unrelated replacement waited for a cold file")
	}
	select {
	case err := <-foreground:
		require.NoError(t, err)
	case <-time.After(time.Second):
		cancel()
		close(f.remote.release)
		_ = waitWarmScope(t, warmed)
		t.Fatal("foreground write waited for an unrelated cold file")
	}
	close(f.remote.release)
	require.NoError(t, waitWarmScope(t, warmed))
}

func TestWarmScopeProgressMayReadWhileCloseIsQueued(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	active := make(chan error, 1)
	go func() {
		active <- f.bs.WithPayloadScope(ctx, []string{"unrelated"}, false, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	callback := make(chan struct{})
	readNow := make(chan struct{})
	readResult := make(chan error, 1)
	warmed := make(chan error, 1)
	go func() {
		_, err := f.bs.WarmAll(ctx, func(done, total int64) {
			if done != 0 {
				return
			}
			close(callback)
			<-readNow
			result := make(chan error, 1)
			go func() { _, err := f.bs.GetSize(ctx, "warm-src"); result <- err }()
			select {
			case err := <-result:
				readResult <- err
			case <-time.After(time.Second):
				// Returning releases a regressed whole-run lifecycle pin, so
				// the test can fail without hanging Close or its cleanup.
				readResult <- errors.New("progress callback deadlocked behind Close")
			}
		})
		warmed <- err
	}()
	select {
	case <-callback:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("warm never invoked its initial progress callback")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.bs.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for f.bs.closeMu.TryRLock() {
		f.bs.closeMu.RUnlock()
		if time.Now().After(deadline) {
			close(readNow)
			close(release)
			t.Fatal("Close never queued")
		}
		time.Sleep(time.Millisecond)
	}
	close(readNow)
	close(release)
	require.NoError(t, waitWarmScope(t, active))
	require.NoError(t, waitWarmScope(t, closed))
	require.ErrorIs(t, waitWarmScope(t, readResult), ErrStoreClosed)
	require.ErrorIs(t, waitWarmScope(t, warmed), ErrStoreClosed)
}

func TestWarmScopeReplacementRefreshesPlannedRows(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	var callbackErr error
	var counts [][2]int64
	result, err := f.bs.WarmAll(ctx, func(done, total int64) {
		counts = append(counts, [2]int64{done, total})
		if done == 0 {
			callbackErr = f.replace(ctx)
		}
	})
	require.NoError(t, callbackErr)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.BlocksFetched)
	require.Equal(t, [][2]int64{{0, 2}, {1, 2}, {2, 2}}, counts)
	got := make([]byte, len(f.source))
	_, err = f.bs.ReadAt(ctx, "warm-dst", got, 0)
	require.NoError(t, err)
	require.Equal(t, f.source, got, "a replacement after planning must invalidate the saved manifest")
}

func TestWarmScopeRemovedRowsPreserveTotal(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	var callbackErr error
	var counts [][2]int64
	result, err := f.bs.WarmAll(ctx, func(done, total int64) {
		counts = append(counts, [2]int64{done, total})
		if done == 0 {
			callbackErr = f.bs.WithPayloadScope(ctx, []string{"warm-dst"}, true, func(context.Context) error {
				return f.metadata.Delete(ctx, f.dstRow.ID)
			})
		}
	})
	require.NoError(t, callbackErr)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.BlocksFetched)
	require.EqualValues(t, len(f.source), result.BytesFetched)
	require.Equal(t, [][2]int64{{0, 2}, {1, 2}, {2, 2}}, counts)
	require.EqualValues(t, 1, f.remote.reads.Load(), "a removed row must not fetch obsolete bytes")
}

type warmScopeListingCounts struct {
	metadata.Store
	mu     sync.Mutex
	counts map[string]int
}

func (s *warmScopeListingCounts) ListFileChunks(ctx context.Context, id string) ([]*block.FileChunk, error) {
	s.mu.Lock()
	s.counts[id]++
	s.mu.Unlock()
	return s.Store.ListFileChunks(ctx, id)
}

func TestWarmScopeBoundsDownloadsAndReusesManifest(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const chunks = 12
	for i := 1; i < chunks; i++ {
		row := *f.srcRow
		row.ID = fmt.Sprintf("warm-src/%d", i*4096)
		require.NoError(t, f.metadata.Put(ctx, &row))
	}
	watched := &warmScopeListingCounts{Store: f.metadata, counts: make(map[string]int)}
	f.rs.fileChunkStore = watched
	f.rs.config.ParallelDownloads = 3
	started := make(chan struct{}, chunks+1)
	release := make(chan struct{})
	f.remote.hook = func(ctx context.Context, _ block.ContentHash) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	warmed := make(chan error, 1)
	var result WarmResult
	var counts [][2]int64
	go func() {
		var err error
		result, err = f.bs.WarmAll(ctx, func(done, total int64) { counts = append(counts, [2]int64{done, total}) })
		warmed <- err
	}()
	for range f.rs.config.ParallelDownloads {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			cancel()
			close(release)
			_ = waitWarmScope(t, warmed)
			t.Fatal("warm failed to fill its bounded download pool")
		}
	}
	require.EqualValues(t, 3, f.remote.peak.Load())
	close(release)
	require.NoError(t, waitWarmScope(t, warmed))
	require.EqualValues(t, chunks+1, result.BlocksFetched)
	require.LessOrEqual(t, f.remote.peak.Load(), int64(3))
	require.Len(t, counts, chunks+2)
	for i, pair := range counts {
		require.Equal(t, [2]int64{int64(i), chunks + 1}, pair)
	}
	watched.mu.Lock()
	defer watched.mu.Unlock()
	require.Equal(t, map[string]int{"warm-src": 1, "warm-dst": 1}, watched.counts,
		"unchanged files need only one manifest scan regardless of chunk count")
}

func TestWarmScopeRefreshesOverlapBoundsAndKeepsPlannedTotal(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	watched := &warmScopeListingCounts{Store: f.metadata, counts: make(map[string]int)}
	f.rs.fileChunkStore = watched
	var callbackErr error
	var counts [][2]int64
	result, err := f.bs.WarmAll(ctx, func(done, total int64) {
		counts = append(counts, [2]int64{done, total})
		if done == 0 {
			callbackErr = f.bs.WithPayloadScope(ctx, []string{"warm-src"}, true, func(context.Context) error {
				row := *f.dstRow
				row.ID = "warm-src/2048"
				row.DataSize = 2048
				return f.metadata.Put(ctx, &row)
			})
		}
	})
	require.NoError(t, callbackErr)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.BlocksFetched, "the newly added ID is outside the fixed work list")
	require.Equal(t, [][2]int64{{0, 2}, {1, 2}, {2, 2}}, counts)
	watched.mu.Lock()
	require.Equal(t, map[string]int{"warm-src": 2, "warm-dst": 1}, watched.counts,
		"only the replaced file needs one fresh manifest scan")
	watched.mu.Unlock()
	got := make([]byte, len(f.source))
	_, err = f.bs.ReadAt(ctx, "warm-src", got, 0)
	require.NoError(t, err)
	want := append(bytes.Clone(f.source[:2048]), bytes.Repeat([]byte{0x62}, 2048)...)
	require.Equal(t, want, got, "the old row must give up its claim at the new row's start")
}

func TestWarmScopeCloseFromProgress(t *testing.T) {
	f := newWarmScopeFixture(t)
	var closeErr error
	_, err := f.bs.WarmAll(context.Background(), func(done, total int64) {
		if done == 0 {
			closeErr = f.bs.Close()
		}
	})
	require.NoError(t, closeErr)
	require.ErrorIs(t, err, ErrStoreClosed)
	f.bs.admission.mu.Lock()
	defer f.bs.admission.mu.Unlock()
	require.Empty(t, f.bs.admission.entries, "closing from progress must still release observers")
}

func TestWarmScopeProgressPanicJoinsWorkers(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan struct{})
	var once sync.Once
	var calls, cancelled atomic.Int64
	f.remote.hook = func(ctx context.Context, _ block.ContentHash) error {
		if calls.Add(1) == 1 {
			return nil
		}
		once.Do(func() { close(blocked) })
		<-ctx.Done()
		cancelled.Add(1)
		return ctx.Err()
	}

	const panicValue = "warm progress failed"
	require.PanicsWithValue(t, panicValue, func() {
		_, _ = f.bs.WarmAll(ctx, func(done, total int64) {
			if done == 1 {
				select {
				case <-blocked:
				case <-time.After(5 * time.Second):
					t.Fatal("second warm download never started")
				}
				panic(panicValue)
			}
		})
	})
	// The caller has recovered without cancelling its own context. WarmAll
	// must already have cancelled and joined the remaining remote operation.
	require.NoError(t, ctx.Err())
	require.Zero(t, f.remote.inFlight.Load(), "remote work survived the callback panic")
	require.EqualValues(t, 1, cancelled.Load())
	require.EqualValues(t, 2, f.remote.reads.Load(), "no extra remote work may outlive the caller")
	f.bs.admission.mu.Lock()
	defer f.bs.admission.mu.Unlock()
	require.Empty(t, f.bs.admission.entries, "workers must exit before observers are released")
}

func TestWarmScopeProgressPanicDrainsCompletions(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 1; i < 12; i++ {
		row := *f.srcRow
		row.ID = fmt.Sprintf("warm-src/%d", i*4096)
		require.NoError(t, f.metadata.Put(ctx, &row))
	}

	// After the first completion is consumed, two workers can fill the two
	// event slots and each finish one more fetch before blocking on its send.
	const blockedAfter = 5
	const panicValue = "warm progress failed with a full event buffer"
	require.PanicsWithValue(t, panicValue, func() {
		_, _ = f.bs.WarmAll(ctx, func(done, total int64) {
			if done == 1 {
				require.Eventually(t, func() bool {
					return f.remote.reads.Load() == blockedAfter && f.remote.inFlight.Load() == 0
				}, 5*time.Second, time.Millisecond, "workers never filled the completion buffer")
				panic(panicValue)
			}
		})
	})
	require.NoError(t, ctx.Err())
	require.EqualValues(t, blockedAfter, f.remote.reads.Load(), "panic cleanup must stop scheduling new work")
	// Both senders and their join goroutine must be gone when the caller
	// recovers. Counting only active downloads would miss blocked senders.
	stacks := make([]byte, 1<<20)
	require.Eventually(t, func() bool {
		n := runtime.Stack(stacks, true)
		return !bytes.Contains(stacks[:n], []byte("engine.(*RemoteSync).warmAll.func"))
	}, time.Second, time.Millisecond, "warm workers survived their caller")
	f.bs.admission.mu.Lock()
	defer f.bs.admission.mu.Unlock()
	require.Empty(t, f.bs.admission.entries)
}

func TestWarmScopeOrdinaryOverwriteAfterPlan(t *testing.T) {
	for _, initialNonzero := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonzero_%t", initialNonzero), func(t *testing.T) {
			f := newWarmScopeFixture(t)
			ctx := context.Background()
			if initialNonzero {
				_, err := f.bs.ReadAt(ctx, "warm-src", make([]byte, len(f.source)), 0)
				require.NoError(t, err)
				_, err = f.bs.DrainLocalSynced(ctx)
				require.NoError(t, err)
				require.NotZero(t, f.bs.local.WriteVersion())
			} else {
				require.Zero(t, f.bs.local.WriteVersion())
			}
			fresh := bytes.Repeat([]byte{0x7f}, len(f.source))
			var mutationErr error
			_, err := f.bs.WarmAll(ctx, func(done, total int64) {
				if done != 0 {
					return
				}
				_, mutationErr = f.bs.WriteAt(ctx, "warm-src", nil, fresh, 0)
				if mutationErr != nil {
					return
				}
				mutationErr = f.rs.SyncNow(ctx)
				if mutationErr != nil {
					return
				}
				row, rowErr := f.metadata.GetFileChunk(ctx, f.srcRow.ID)
				require.NoError(t, rowErr)
				require.NotEqual(t, f.srcRow.Hash, row.Hash, "the overwrite must have replaced the remote manifest")
				evicted, evictErr := f.bs.DrainLocalSynced(ctx)
				mutationErr = evictErr
				require.Greater(t, evicted.SegmentsEvicted, 0)
				resident, residentErr := f.bs.local.IsRangeResident(ctx, "warm-src", 0, int64(len(fresh)))
				require.NoError(t, residentErr)
				require.False(t, resident)
			})
			require.NoError(t, mutationErr)
			require.NoError(t, err)
			got := make([]byte, len(fresh))
			_, err = f.bs.ReadAt(ctx, "warm-src", got, 0)
			require.NoError(t, err)
			require.True(t, bytes.Equal(fresh, got), "planned stale row replaced a newer carved cold interval: got %#x, want %#x", got[0], fresh[0])
		})
	}
}

func TestWarmScopeAlreadyDirtyOverwriteAtPlan(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	fresh := bytes.Repeat([]byte{0x7f}, len(f.source))
	_, err := f.bs.WriteAt(ctx, "warm-src", nil, fresh, 0)
	require.NoError(t, err)
	before, err := f.metadata.GetFileChunk(ctx, f.srcRow.ID)
	require.NoError(t, err)
	require.Equal(t, f.srcRow.Hash, before.Hash)
	require.NotZero(t, f.bs.local.WriteVersion())
	var mutationErr error
	var counts [][2]int64
	result, err := f.bs.WarmAll(ctx, func(done, total int64) {
		counts = append(counts, [2]int64{done, total})
		if done != 0 {
			return
		}
		mutationErr = f.rs.SyncNow(ctx)
		if mutationErr != nil {
			return
		}
		row, rowErr := f.metadata.GetFileChunk(ctx, f.srcRow.ID)
		require.NoError(t, rowErr)
		require.NotEqual(t, f.srcRow.Hash, row.Hash)
		evicted, evictErr := f.bs.DrainLocalSynced(ctx)
		mutationErr = evictErr
		require.Greater(t, evicted.SegmentsEvicted, 0)
		resident, residentErr := f.bs.local.IsRangeResident(ctx, "warm-src", 0, int64(len(fresh)))
		require.NoError(t, residentErr)
		require.False(t, resident)
	})
	require.NoError(t, mutationErr)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.BlocksFetched, "the conservative bound must preserve download accounting")
	require.Equal(t, [][2]int64{{0, 2}, {1, 2}, {2, 2}}, counts)
	got := make([]byte, len(fresh))
	_, err = f.bs.ReadAt(ctx, "warm-src", got, 0)
	require.NoError(t, err)
	require.True(t, bytes.Equal(fresh, got), "overwrite dirty before plan came back stale: got %#x, want %#x", got[0], fresh[0])
}

func TestWarmScopePlanningDuringManifestReap(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	stale := *f.dstRow
	stale.ID = "warm-src/2048"
	stale.DataSize = 2048
	require.NoError(t, f.metadata.Put(ctx, &stale))
	fresh := bytes.Repeat([]byte{0x7f}, len(f.source))
	_, err := f.bs.WriteAt(ctx, "warm-src", nil, fresh, 0)
	require.NoError(t, err)
	afterFile := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	unpause := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(unpause)
	flushed := make(chan error, 1)
	go func() {
		flushed <- f.bs.WithPayloadScope(ctx, []string{"warm-src"}, false, func(ctx context.Context) error {
			fn, reap := f.rs.flushFn()
			return f.bs.local.Flush(ctx, "warm-src", journal.FlushOptions{Force: true, AfterFile: func(ctx context.Context, id journal.FileID) error {
				close(afterFile)
				<-resume
				return reap(ctx, id)
			}}, fn)
		})
	}()
	select {
	case <-afterFile:
	case <-time.After(5 * time.Second):
		t.Fatal("flush never reached AfterFile")
	}
	require.Positive(t, f.bs.local.UnsyncedBytes(), "bytes must stay dirty until manifest reap succeeds")
	rows, err := f.metadata.ListFileChunks(ctx, "warm-src")
	require.NoError(t, err)
	require.Len(t, rows, 2, "the obsolete interior row must still be awaiting reap")
	row, err := f.metadata.GetFileChunk(ctx, stale.ID)
	require.NoError(t, err)
	require.Equal(t, stale.Hash, row.Hash)
	var phaseErr error
	_, err = f.bs.WarmAll(ctx, func(done, total int64) {
		if done != 0 {
			return
		}
		require.EqualValues(t, 3, total)
		unpause()
		phaseErr = waitWarmScope(t, flushed)
		if phaseErr != nil {
			return
		}
		rows, phaseErr = f.metadata.ListFileChunks(ctx, "warm-src")
		if phaseErr != nil {
			return
		}
		require.Len(t, rows, 1, "reap should remove the obsolete interior row")
		evicted, evictErr := f.bs.DrainLocalSynced(ctx)
		phaseErr = evictErr
		require.Greater(t, evicted.SegmentsEvicted, 0)
	})
	require.NoError(t, phaseErr)
	require.NoError(t, err)
	got := make([]byte, len(fresh))
	_, err = f.bs.ReadAt(ctx, "warm-src", got, 0)
	require.NoError(t, err)
	require.True(t, bytes.Equal(fresh, got), "interior row awaiting reap restored stale bytes: got %#x at2048, want %#x", got[2048], fresh[2048])
}
