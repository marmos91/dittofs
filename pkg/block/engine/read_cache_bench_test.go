package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	localfs "github.com/marmos91/dittofs/pkg/block/local/fs"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metastore "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"lukechampine.com/blake3"
)

// latencyRemote wraps a remotememory.Store and (a) injects a fixed per-Get
// WAN latency into the verified-read path the engine fetch code actually
// calls (dispatchRemoteFetch → ReadBlockVerified), and (b) counts Get and Put
// calls atomically. It models a high-latency remote tier (S3 / object store)
// so a benchmark can show the warm (all-local) read path avoiding the injected
// round-trip, and an invariant test can assert that a read-only workload
// issues zero re-uploads (Put count 0) after the #1362 fix.
//
// The counters and latency are wrapped around ReadBlockVerified rather than
// Get because dispatchRemoteFetch routes every block fetch through the
// verified-read CAS path; Get is wrapped too for completeness.
type latencyRemote struct {
	*remotememory.Store
	getLatency time.Duration
	gets       atomic.Int64
	puts       atomic.Int64
}

func newLatencyRemote(getLatency time.Duration) *latencyRemote {
	return &latencyRemote{Store: remotememory.New(), getLatency: getLatency}
}

func (r *latencyRemote) ReadBlockVerified(ctx context.Context, hash, expected block.ContentHash) ([]byte, error) {
	r.gets.Add(1)
	if r.getLatency > 0 {
		time.Sleep(r.getLatency)
	}
	return r.Store.ReadBlockVerified(ctx, hash, expected)
}

func (r *latencyRemote) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	r.gets.Add(1)
	if r.getLatency > 0 {
		time.Sleep(r.getLatency)
	}
	return r.Store.Get(ctx, hash)
}

func (r *latencyRemote) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	r.puts.Add(1)
	return r.Store.Put(ctx, hash, data)
}

// Compile-time assertion that the latency wrapper still satisfies the full
// remote contract the syncer depends on.
var _ remote.RemoteStore = (*latencyRemote)(nil)

// seedRemoteOnlyBlock seeds a single FileBlock row in BlockStateRemote and the
// matching CAS bytes in the (latency) remote, WITHOUT placing the chunk in any
// local store. A subsequent read of (payloadID, blockIdx 0) therefore misses
// locally and fetches from the remote tier — the exact cold-read path #1362
// bounds. Returns the chunk hash.
func seedRemoteOnlyBlock(t testing.TB, fbs *stubFileBlockStore, rs interface {
	Put(context.Context, block.ContentHash, []byte) error
}, payloadID string, data []byte) block.ContentHash {
	t.Helper()
	hash := block.ContentHash(blake3.Sum256(data))
	if err := rs.Put(context.Background(), hash, data); err != nil {
		t.Fatalf("seed remote Put: %v", err)
	}
	fb := &block.FileBlock{
		ID:       fmt.Sprintf("%s/%d", payloadID, 0),
		Hash:     hash,
		DataSize: uint32(len(data)),
		State:    block.BlockStateRemote,
	}
	if err := fbs.Put(context.Background(), fb); err != nil {
		t.Fatalf("seed FileBlock Put: %v", err)
	}
	return hash
}

// BenchmarkReadThroughCache_ColdVsWarm quantifies the benefit of the
// read-through cache by comparing two regimes against a latency-injecting
// remote:
//
//   - cold: every iteration reads a fresh, remote-only working set. Each block
//     misses locally and pays the injected WAN round-trip (fetchBlock →
//     ReadBlockVerified → time.Sleep).
//   - warm: the working set is fetched ONCE up front, so every benchmarked read
//     is served from the local store with no remote latency and no Get.
//
// The benchmark exercises the syncer's fetch+persist path (fetchBlock) — the
// exact code that persists fetched chunks into the local CAS store and, after
// #1362, marks them synced + bounds them. Reported MB/s (via b.SetBytes) shows
// the warm path's throughput is dominated by local I/O, not the WAN latency.
func BenchmarkReadThroughCache_ColdVsWarm(b *testing.B) {
	const (
		chunkSize  = 64 * 1024            // 64 KiB working-set chunk
		wanLatency = 1 * time.Millisecond // injected per-Get round-trip
	)
	ctx := context.Background()

	b.Run("cold", func(b *testing.B) {
		b.SetBytes(chunkSize)
		b.ReportAllocs()
		for i := 0; b.Loop(); i++ {
			// Fresh remote-only working set every iteration so each read is a
			// genuine miss that pays the WAN latency.
			loc := memorylocal.New()
			rs := newLatencyRemote(wanLatency)
			fbs := newStubFileBlockStore()
			payloadID := fmt.Sprintf("cold-%d", i)
			data := makeChunk(chunkSize, byte(i))
			seedRemoteOnlyBlock(b, fbs, rs, payloadID, data)

			m := newFetchSyncer(loc, rs.Store, fbs)
			m.remoteStore = rs // route through the latency wrapper

			got, err := m.fetchBlock(ctx, payloadID, 0)
			if err != nil {
				b.Fatalf("cold fetchBlock: %v", err)
			}
			if len(got) != chunkSize {
				b.Fatalf("cold read len=%d, want %d", len(got), chunkSize)
			}
		}
	})

	b.Run("warm", func(b *testing.B) {
		// Build the working set ONCE and prime the local store so every
		// benchmarked read is served from disk with no remote latency.
		loc := memorylocal.New()
		rs := newLatencyRemote(wanLatency)
		fbs := newStubFileBlockStore()
		payloadID := "warm"
		data := makeChunk(chunkSize, 0x5A)
		hash := seedRemoteOnlyBlock(b, fbs, rs, payloadID, data)

		m := newFetchSyncer(loc, rs.Store, fbs)
		m.remoteStore = rs

		// Prime: one cold fetch persists the chunk locally.
		if _, err := m.fetchBlock(ctx, payloadID, 0); err != nil {
			b.Fatalf("warm prime fetchBlock: %v", err)
		}
		if has, _ := loc.Has(ctx, hash); !has {
			b.Fatalf("warm prime did not persist chunk locally")
		}
		getsAfterPrime := rs.gets.Load()

		b.SetBytes(chunkSize)
		b.ReportAllocs()
		for b.Loop() {
			// Local-served read: ReadChunk hits the warm local store; no Get,
			// no WAN latency. This is the read the cache exists to accelerate.
			got, err := loc.Get(ctx, hash)
			if err != nil {
				b.Fatalf("warm local read: %v", err)
			}
			if len(got) != chunkSize {
				b.Fatalf("warm read len=%d, want %d", len(got), chunkSize)
			}
		}
		// Sanity: the warm loop issued no further remote Gets.
		if extra := rs.gets.Load() - getsAfterPrime; extra != 0 {
			b.Fatalf("warm loop issued %d remote Gets; want 0 (served from cache)", extra)
		}
	})
}

// makeChunk returns a deterministic chunkSize-byte slice seeded by fill so
// distinct working-set members hash to distinct CAS keys.
func makeChunk(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill ^ byte(i)
	}
	return b
}

// TestReadThroughCache_NoReuploadOnReadOnlyWorkload pins the #1362 read→write
// de-amplification: fetching verbatim remote content on a read miss must NOT
// schedule that content for re-upload. The chunk is already durable on the
// remote, so re-uploading it turns a read-only workload into a write-heavy one
// (read-amplification → write-amplification).
//
// The test seeds a remote-only working set, reads the whole set through the
// real syncer fetch path (fetchBlock, which persists via local.Put then calls
// markFetchedSynced), and asserts the mock remote's Put count is 0. It also
// asserts the pending-upload set is fully drained (unsyncedBytes == 0) — the
// precondition that makes the mirror loop a guaranteed no-op, so the assertion
// does not depend on mirror-loop timing.
func TestReadThroughCache_NoReuploadOnReadOnlyWorkload(t *testing.T) {
	const (
		setSize   = 8
		chunkSize = 4 * 1024
	)
	ctx := context.Background()

	loc := memorylocal.New()
	rs := newLatencyRemote(0)
	fbs := newStubFileBlockStore()
	mds := metastore.NewMemoryMetadataStoreWithDefaults()

	m := newFetchSyncer(loc, rs.Store, fbs)
	m.remoteStore = rs      // count Puts through the wrapper
	m.syncedHashStore = mds // so markFetchedSynced can mark synced
	hashes := make([]block.ContentHash, 0, setSize)

	for i := 0; i < setSize; i++ {
		payloadID := fmt.Sprintf("ro-%d", i)
		data := makeChunk(chunkSize, byte(i))
		h := seedRemoteOnlyBlock(t, fbs, rs, payloadID, data)
		hashes = append(hashes, h)

		// Model the real read-fetch path: StoreChunk's onChunkComplete callback
		// (wired by engine.New in production) registers the freshly-persisted
		// chunk as pending upload. The memory local store does not fire that
		// callback, so register it explicitly; markFetchedSynced inside
		// fetchBlock must then cancel it.
		m.addPendingHash(h, int64(chunkSize))
	}
	if got := m.UnsyncedBytes(); got != int64(setSize*chunkSize) {
		t.Fatalf("precondition unsyncedBytes=%d, want %d", got, setSize*chunkSize)
	}

	// Reset the Put counter: the remote-seed Puts above are fixture setup, not
	// re-uploads. We only care about Puts the READ path issues.
	rs.puts.Store(0)

	// Read the entire working set through the fetch+persist path.
	for i := 0; i < setSize; i++ {
		payloadID := fmt.Sprintf("ro-%d", i)
		got, err := m.fetchBlock(ctx, payloadID, 0)
		if err != nil {
			t.Fatalf("fetchBlock %s: %v", payloadID, err)
		}
		if len(got) != chunkSize {
			t.Fatalf("read %s len=%d, want %d", payloadID, len(got), chunkSize)
		}
	}

	// Invariant 1: the read-only workload issued zero re-uploads.
	if got := rs.puts.Load(); got != 0 {
		t.Errorf("remote Put count=%d after read-only workload; want 0 (#1362 read→write amplification eliminated)", got)
	}

	// Invariant 2: the pending-upload set is fully drained, so the mirror loop
	// is a guaranteed no-op regardless of its timing.
	if got := m.UnsyncedBytes(); got != 0 {
		t.Errorf("unsyncedBytes=%d after reads; want 0 (every fetched chunk canceled its pending upload)", got)
	}
	m.pendingMu.Lock()
	pending := len(m.pendingHashes)
	m.pendingMu.Unlock()
	if pending != 0 {
		t.Errorf("pendingHashes has %d entries; want 0", pending)
	}

	// And each fetched chunk is marked synced so eviction can reclaim it on a
	// read-only workload.
	for _, h := range hashes {
		synced, err := mds.IsSynced(ctx, h)
		if err != nil {
			t.Fatalf("IsSynced: %v", err)
		}
		if !synced {
			t.Errorf("chunk %s not marked synced; eviction would stall on a read-only workload", h.String())
		}
	}
}

// TestReadThroughCache_BoundedByMaxDisk is the headline #1362 invariant: a
// read-only workload over a remote tier must NOT grow the local CAS store
// without bound. It drives many distinct fetched chunks through the real
// syncer fetch path into an FSStore configured with a small maxDisk, reading a
// working set several times larger than the cap, and asserts diskUsed stays
// <= maxDisk throughout and at the end.
//
// Before the fix, FSStore.Put (the read-through cache's write entry) delegated
// straight to StoreChunk and never reserved capacity, so the cache grew
// unbounded on a read-only workload (eviction only ran on the write/append
// path). The fetch path here persists through FSStore.Put → ensureSpace, which
// now evicts to honor the bound. The FSStore is wired with no SyncedHashStore,
// so every chunk is evictable and the bound is enforced purely by Put's
// capacity reservation.
func TestReadThroughCache_BoundedByMaxDisk(t *testing.T) {
	const (
		chunkSize = 4 * 1024
		// Cap at 5 chunks; the working set is 40 chunks (8x the cap).
		maxDisk = 5 * chunkSize
		setSize = 40
	)
	ctx := context.Background()

	dir := t.TempDir()
	// No SyncedHashStore: every fetched chunk is immediately evictable, so the
	// bound is enforced by Put's ensureSpace alone.
	loc, err := localfs.NewWithOptions(dir, maxDisk, newStubFileBlockStore(), localfs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	startCtx, cancel := context.WithCancel(ctx)
	loc.Start(startCtx)
	t.Cleanup(func() {
		cancel()
		_ = loc.Close()
	})

	rs := newLatencyRemote(0)
	fbs := newStubFileBlockStore()
	m := newFetchSyncer(loc, rs.Store, fbs)
	m.remoteStore = rs

	for i := 0; i < setSize; i++ {
		payloadID := fmt.Sprintf("bound-%d", i)
		data := makeChunk(chunkSize, byte(i))
		seedRemoteOnlyBlock(t, fbs, rs, payloadID, data)

		got, err := m.fetchBlock(ctx, payloadID, 0)
		if err != nil {
			t.Fatalf("fetchBlock %s: %v", payloadID, err)
		}
		if len(got) != chunkSize {
			t.Fatalf("read %s len=%d, want %d", payloadID, len(got), chunkSize)
		}

		// Invariant: after every fetched chunk lands, the local CAS store stays
		// within its disk budget.
		if used := loc.Stats().DiskUsed; used > maxDisk {
			t.Fatalf("after fetch %d diskUsed=%d exceeds maxDisk=%d — read-through cache unbounded (#1362)", i, used, maxDisk)
		}
	}

	// Read the set a second time (several times larger than the cap, total) to
	// confirm the bound holds steady across re-fetches of evicted chunks.
	for i := 0; i < setSize; i++ {
		payloadID := fmt.Sprintf("bound-%d", i)
		if _, err := m.fetchBlock(ctx, payloadID, 0); err != nil {
			t.Fatalf("re-read fetchBlock %s: %v", payloadID, err)
		}
		if used := loc.Stats().DiskUsed; used > maxDisk {
			t.Fatalf("re-read fetch %d diskUsed=%d exceeds maxDisk=%d (#1362)", i, used, maxDisk)
		}
	}

	if used := loc.Stats().DiskUsed; used > maxDisk {
		t.Fatalf("final diskUsed=%d exceeds maxDisk=%d after reading %d chunks into a %d-chunk cache (#1362)",
			used, maxDisk, setSize, maxDisk/chunkSize)
	}
}
