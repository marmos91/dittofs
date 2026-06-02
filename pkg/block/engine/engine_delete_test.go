package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

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
// no-op (count 0) — the production coordinator maps ErrFileBlockNotFound the
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

func (c *refcountCoordinator) PersistFileBlocks(_ context.Context, _ string, _ []block.BlockRef, _ block.ObjectID) error {
	return nil
}

func (c *refcountCoordinator) GetPersistedBlocks(_ context.Context, _ string) ([]block.BlockRef, error) {
	return nil, nil
}

func (c *refcountCoordinator) FindByObjectID(_ context.Context, _ block.ObjectID) ([]block.BlockRef, error) {
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
	synced    map[block.ContentHash]struct{}
	deleted   []block.ContentHash
	deleteErr error
}

func newRecordingSyncedHashStore() *recordingSyncedHashStore {
	return &recordingSyncedHashStore{synced: make(map[block.ContentHash]struct{})}
}

func (s *recordingSyncedHashStore) markSyncedForTest(hash block.ContentHash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced[hash] = struct{}{}
}

func (s *recordingSyncedHashStore) IsSynced(_ context.Context, hash block.ContentHash) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.synced[hash]
	return ok, nil
}

func (s *recordingSyncedHashStore) MarkSynced(_ context.Context, hash block.ContentHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced[hash] = struct{}{}
	return nil
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

var _ metadata.SyncedHashStore = (*recordingSyncedHashStore)(nil)

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
	fbs := newStubFileBlockStore()
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

// TestEngine_Delete_CascadesDeleteSynced asserts that engine.Delete
// fires syncedHashStore.DeleteSynced exactly when the coordinator's
// DecrementRefCount returns newCount == 0, and never otherwise. The
// cascade keeps the synced set a strict subset of local CAS contents
// a stale marker would cause the next mirror pass on a re-Put of the
// same hash to skip the upload, leaving the remote out of sync with
// local.
func TestEngine_Delete_CascadesDeleteSynced(t *testing.T) {
	ctx := context.Background()

	t.Run("RefcountZero_CascadesDeleteSynced", func(t *testing.T) {
		var hash block.ContentHash
		hash[0] = 0xC0

		coord := newRefcountCoordinator()
		coord.seedBlock("pid-cascade-zero", 0, hash, 1) // last reference → Delete reaps to 0

		syncedStore := newRecordingSyncedHashStore()
		syncedStore.markSyncedForTest(hash)

		bs := buildCascadeFixture(t, coord, syncedStore)

		blocks := []block.BlockRef{{Hash: hash, Offset: 0, Size: 4096}}
		if err := bs.Delete(ctx, "pid-cascade-zero", blocks); err != nil {
			t.Fatalf("Delete returned error: %v", err)
		}

		got, err := syncedStore.IsSynced(ctx, hash)
		if err != nil {
			t.Fatalf("IsSynced: %v", err)
		}
		if got {
			t.Errorf("IsSynced=true after refcount→0 Delete; want false (cascade should have fired)")
		}
		if dels := syncedStore.deletedHashes(); len(dels) != 1 || dels[0] != hash {
			t.Errorf("DeleteSynced invocations=%v; want [%x] exactly once", dels, hash[:4])
		}
	})

	t.Run("RefcountNonZero_DoesNotCascade", func(t *testing.T) {
		var hash block.ContentHash
		hash[0] = 0xC1

		coord := newRefcountCoordinator()
		coord.seedBlock("pid-cascade-nonzero", 0, hash, 2) // two refs; Delete drops one → newCount == 1

		syncedStore := newRecordingSyncedHashStore()
		syncedStore.markSyncedForTest(hash)

		bs := buildCascadeFixture(t, coord, syncedStore)

		blocks := []block.BlockRef{{Hash: hash, Offset: 0, Size: 4096}}
		if err := bs.Delete(ctx, "pid-cascade-nonzero", blocks); err != nil {
			t.Fatalf("Delete returned error: %v", err)
		}

		got, err := syncedStore.IsSynced(ctx, hash)
		if err != nil {
			t.Fatalf("IsSynced: %v", err)
		}
		if !got {
			t.Errorf("IsSynced=false after refcount→1 Delete; want true (cascade must NOT fire)")
		}
		if dels := syncedStore.deletedHashes(); len(dels) != 0 {
			t.Errorf("DeleteSynced invocations=%v; want none (newCount != 0)", dels)
		}
	})

	t.Run("NilSyncedStore_NoOps", func(t *testing.T) {
		var hash block.ContentHash
		hash[0] = 0xC2

		coord := newRefcountCoordinator()
		coord.seedBlock("pid-cascade-nil", 0, hash, 1)

		// SyncedHashStore intentionally nil — exercises the bs.syncedHashStore
		// nil-guard. Delete must not panic and must still drive the
		// coordinator decrement.
		bs := buildCascadeFixture(t, coord, nil)

		blocks := []block.BlockRef{{Hash: hash, Offset: 0, Size: 4096}}
		if err := bs.Delete(ctx, "pid-cascade-nil", blocks); err != nil {
			t.Fatalf("Delete returned error: %v", err)
		}

		// DecrementRefCount fired — the seeded count is now zero.
		coord.mu.Lock()
		got := coord.counts[hash]
		coord.mu.Unlock()
		if got != 0 {
			t.Errorf("coordinator refcount=%d after Delete with nil syncedStore; want 0", got)
		}
	})

	t.Run("DeleteSyncedFailure_IsBenign", func(t *testing.T) {
		var hash block.ContentHash
		hash[0] = 0xC3

		coord := newRefcountCoordinator()
		coord.seedBlock("pid-cascade-benign", 0, hash, 1)

		syncedStore := newRecordingSyncedHashStore()
		syncedStore.markSyncedForTest(hash)
		syncedStore.deleteErr = errors.New("induced DeleteSynced failure")

		bs := buildCascadeFixture(t, coord, syncedStore)

		blocks := []block.BlockRef{{Hash: hash, Offset: 0, Size: 4096}}
		// Delete must NOT propagate the DeleteSynced failure — orphan
		// marker is benign per the plan's threat-register disposition.
		if err := bs.Delete(ctx, "pid-cascade-benign", blocks); err != nil {
			t.Fatalf("Delete returned error on DeleteSynced failure (want nil — orphan is benign): %v", err)
		}
		if dels := syncedStore.deletedHashes(); len(dels) != 1 {
			t.Errorf("DeleteSynced invocation count=%d; want 1 (cascade attempted)", len(dels))
		}
	})
}
