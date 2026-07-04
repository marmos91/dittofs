package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// refcountCoordinator is a MetadataCoordinator fake whose DecrementRefCount
// returns realistic post-decrement counts driven by a seeded per-hash
// map. Required for the refcount-cascade tests because the broader
// fakeCoordinator hardcodes newCount == 0, which would conflate the
// "fired cascade" and "did-not-fire cascade" cases.
type refcountCoordinator struct {
	mu     sync.Mutex
	counts map[block.ContentHash]uint32

	// idHash maps the reap-path key "{payloadID}/{offset}" to the hash whose
	// count it decrements. The reap path is keyed by EXACT ID (never by hash),
	// so the coordinator translates the row identity back to the hash it needs
	// to bookkeep — exactly what the production runtime does by reading the row
	// before decrementing. seedBlock populates this alongside the hash count.
	idHash map[string]block.ContentHash

	// decrementErr, when non-nil and matching hash, is returned on the
	// matching DecrementRefCount call (and the count is NOT mutated).
	decrementErr     error
	decrementErrHash block.ContentHash
}

func newRefcountCoordinator() *refcountCoordinator {
	return &refcountCoordinator{
		counts: make(map[block.ContentHash]uint32),
		idHash: make(map[string]block.ContentHash),
	}
}

// seedBlock seeds a hash count AND binds the reap-path row ID
// "{payloadID}/{offset}" to that hash, so a by-ID reap can resolve and
// decrement the hash's count.
func (c *refcountCoordinator) seedBlock(payloadID string, offset uint64, hash block.ContentHash, count uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[hash] = count
	c.idHash[fmt.Sprintf("%s/%d", payloadID, offset)] = hash
}

func (c *refcountCoordinator) IncrementRefCount(_ context.Context, hash block.ContentHash) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[hash]++
	return nil
}

func (c *refcountCoordinator) DecrementRefCount(_ context.Context, hash block.ContentHash) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decrementErr != nil && c.decrementErrHash == hash {
		err := c.decrementErr
		c.decrementErr = nil
		return 0, err
	}
	cur := c.counts[hash]
	if cur == 0 {
		// Treat as already-zero; mirrors backend semantics where
		// underflow is clamped.
		return 0, nil
	}
	cur--
	c.counts[hash] = cur
	return cur, nil
}

// DecrementRefCountAndReap is keyed by EXACT ID "{payloadID}/{offset}" (the
// production reap-path contract). It resolves the ID to the hash it bookkeeps,
// then mirrors DecrementRefCount (including the single-shot error injection) and
// reaps the map entry when the count hits 0, matching the backend reap semantics
// the engine reclaim path relies on. An ID with no seeded row is a tolerated
// no-op (count 0) — the production coordinator maps ErrFileChunkNotFound the
// same way.
func (c *refcountCoordinator) DecrementRefCountAndReap(_ context.Context, payloadID string, offset uint64) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := fmt.Sprintf("%s/%d", payloadID, offset)
	hash, ok := c.idHash[id]
	if !ok {
		return 0, nil
	}
	if c.decrementErr != nil && c.decrementErrHash == hash {
		err := c.decrementErr
		c.decrementErr = nil
		return 0, err
	}
	cur := c.counts[hash]
	if cur == 0 {
		delete(c.counts, hash)
		delete(c.idHash, id)
		return 0, nil
	}
	cur--
	if cur == 0 {
		delete(c.counts, hash)
		delete(c.idHash, id)
		return 0, nil
	}
	c.counts[hash] = cur
	return cur, nil
}

func (c *refcountCoordinator) PersistFileChunks(_ context.Context, _ string, _ []block.ChunkRef, _ block.ObjectID) error {
	return nil
}

func (c *refcountCoordinator) GetPersistedBlocks(_ context.Context, _ string) ([]block.ChunkRef, error) {
	return nil, nil
}

func (c *refcountCoordinator) FindByObjectID(_ context.Context, _ block.ObjectID) ([]block.ChunkRef, error) {
	return nil, nil
}

func (c *refcountCoordinator) GetFileObjectID(_ context.Context, _ string) (block.ObjectID, error) {
	return block.ObjectID{}, nil
}

var _ MetadataCoordinator = (*refcountCoordinator)(nil)

// recordingSyncedHashStore is a SyncedHashStore that wraps an in-memory
// map and records DeleteSynced invocations so tests can assert cascade
// behavior. A seeded markErr is returned from the next DeleteSynced
// call once (single-shot) to exercise the benign-orphan logging path.
type recordingSyncedHashStore struct {
	mu        sync.Mutex
	synced    map[block.ContentHash]time.Time
	deleted   []block.ContentHash
	deleteErr error
}

func newRecordingSyncedHashStore() *recordingSyncedHashStore {
	return &recordingSyncedHashStore{synced: make(map[block.ContentHash]time.Time)}
}

func (s *recordingSyncedHashStore) markSyncedForTest(hash block.ContentHash) {
	s.markSyncedAtForTest(hash, time.Now())
}

// markSyncedAtForTest stamps a marker with an explicit first-mirror time so
// LIST-free sweep tests can exercise the grace window deterministically.
func (s *recordingSyncedHashStore) markSyncedAtForTest(hash block.ContentHash, when time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced[hash] = when
}

func (s *recordingSyncedHashStore) IsSynced(_ context.Context, hash block.ContentHash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.synced[hash]
	return ok, nil
}

func (s *recordingSyncedHashStore) MarkSynced(_ context.Context, hash block.ContentHash, _ block.ChunkLocator) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.synced[hash]; !ok {
		s.synced[hash] = time.Now() // first-write-wins
	}
	return nil
}

// EnumerateSynced satisfies engine.SyncedHashIndex, yielding each marker with
// its recorded first-mirror time (the LIST-free sweep's grace anchor).
func (s *recordingSyncedHashStore) EnumerateSynced(ctx context.Context, fn func(block.ContentHash, time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	snapshot := make(map[block.ContentHash]time.Time, len(s.synced))
	for h, t := range s.synced {
		snapshot[h] = t
	}
	s.mu.Unlock()
	for h, t := range snapshot {
		if err := fn(h, t); err != nil {
			return err
		}
	}
	return nil
}

func (s *recordingSyncedHashStore) GetLocator(_ context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.synced[hash]
	return block.ChunkLocator{}, ok, nil
}

func (s *recordingSyncedHashStore) DeleteSynced(_ context.Context, hash block.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, hash)
	if s.deleteErr != nil {
		err := s.deleteErr
		s.deleteErr = nil
		return err
	}
	delete(s.synced, hash)
	return nil
}

func (s *recordingSyncedHashStore) deletedHashes() []block.ContentHash {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]block.ContentHash, len(s.deleted))
	copy(out, s.deleted)
	return out
}

var (
	_ metadata.SyncedHashStore = (*recordingSyncedHashStore)(nil)
	_ SyncedHashIndex          = (*recordingSyncedHashStore)(nil)
)

// buildCascadeFixture wires a Store with the supplied coordinator
// and (optional) SyncedHashStore. Local store is in-memory so the test
// can focus on engine.Delete's coordinator + syncedHashStore loop
// without filesystem state.
//
// Note on race semantics (in the plan threat model): the
// DecrementRefCount returns the new count BEFORE DeleteSynced fires.
// A parallel mirror-loop pass would have already snapshotted the hash
// at iteration start; if the local CAS chunk is gone by the time it
// runs, local.Get errors and the mirror loop surfaces the error rather
// than re-marking. If the chunk survives momentarily, the marker race
// is benign because the cascade cleans it up post-race.
func buildCascadeFixture(t *testing.T, coord MetadataCoordinator, syncedStore metadata.SyncedHashStore) *Store {
	t.Helper()
	localStore := memory.New()
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())

	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		Coordinator:     coord,
		SyncedHashStore: syncedStore,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestEngine_Delete_PreservesSyncedMarker asserts that engine.Delete NEVER
// clears a hash's synced marker on reap — not even when the coordinator reaps
// the last reference (newCount == 0).
//
// The synced marker means "these bytes are on the remote", which remains true
// after the last reference is reaped: the remote object survives until the GC
// sweep physically deletes it. Since #1458 the steady-state remote sweep finds
// orphan candidates from (synced − live), so the marker MUST outlive unlink —
// otherwise the just-orphaned remote object drops out of the candidate set and
// leaks forever (only a full-Walk reconcile could find it). The sweep clears
// the marker itself, right after it deletes the remote object (#1433).
func TestEngine_Delete_PreservesSyncedMarker(t *testing.T) {
	ctx := context.Background()

	t.Run("RefcountZero_KeepsSyncedMarker", func(t *testing.T) {
		var hash block.ContentHash
		hash[0] = 0xC0

		coord := newRefcountCoordinator()
		coord.seedBlock("pid-keep-zero", 0, hash, 1) // last reference → Delete reaps to 0

		syncedStore := newRecordingSyncedHashStore()
		syncedStore.markSyncedForTest(hash)

		bs := buildCascadeFixture(t, coord, syncedStore)

		blocks := []block.ChunkRef{{Hash: hash, Offset: 0, Size: 4096}}
		if err := bs.Delete(ctx, "pid-keep-zero", blocks); err != nil {
			t.Fatalf("Delete returned error: %v", err)
		}

		// The remote object still exists, so the marker MUST survive so the
		// steady-state GC sweep can still find it as an orphan candidate.
		got, err := syncedStore.IsSynced(ctx, hash)
		if err != nil {
			t.Fatalf("IsSynced: %v", err)
		}
		if !got {
			t.Errorf("IsSynced=false after refcount→0 Delete; want true (marker must outlive unlink so GC can reclaim the remote orphan)")
		}
		if dels := syncedStore.deletedHashes(); len(dels) != 0 {
			t.Errorf("DeleteSynced invocations=%v; want none (unlink must not clear the marker)", dels)
		}

		// The block row itself is still reaped.
		coord.mu.Lock()
		count := coord.counts[hash]
		coord.mu.Unlock()
		if count != 0 {
			t.Errorf("coordinator refcount=%d after Delete; want 0 (row still reaped)", count)
		}
	})

	t.Run("RefcountNonZero_KeepsSyncedMarker", func(t *testing.T) {
		var hash block.ContentHash
		hash[0] = 0xC1

		coord := newRefcountCoordinator()
		coord.seedBlock("pid-keep-nonzero", 0, hash, 2) // two refs; Delete drops one → newCount == 1

		syncedStore := newRecordingSyncedHashStore()
		syncedStore.markSyncedForTest(hash)

		bs := buildCascadeFixture(t, coord, syncedStore)

		blocks := []block.ChunkRef{{Hash: hash, Offset: 0, Size: 4096}}
		if err := bs.Delete(ctx, "pid-keep-nonzero", blocks); err != nil {
			t.Fatalf("Delete returned error: %v", err)
		}

		got, err := syncedStore.IsSynced(ctx, hash)
		if err != nil {
			t.Fatalf("IsSynced: %v", err)
		}
		if !got {
			t.Errorf("IsSynced=false after refcount→1 Delete; want true")
		}
		if dels := syncedStore.deletedHashes(); len(dels) != 0 {
			t.Errorf("DeleteSynced invocations=%v; want none", dels)
		}
	})

	t.Run("NilSyncedStore_NoOps", func(t *testing.T) {
		var hash block.ContentHash
		hash[0] = 0xC2

		coord := newRefcountCoordinator()
		coord.seedBlock("pid-keep-nil", 0, hash, 1)

		// SyncedHashStore intentionally nil — Delete must not panic and must
		// still drive the coordinator decrement.
		bs := buildCascadeFixture(t, coord, nil)

		blocks := []block.ChunkRef{{Hash: hash, Offset: 0, Size: 4096}}
		if err := bs.Delete(ctx, "pid-keep-nil", blocks); err != nil {
			t.Fatalf("Delete returned error: %v", err)
		}

		coord.mu.Lock()
		got := coord.counts[hash]
		coord.mu.Unlock()
		if got != 0 {
			t.Errorf("coordinator refcount=%d after Delete with nil syncedStore; want 0", got)
		}
	})
}
