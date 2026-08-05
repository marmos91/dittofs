package handlers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// countingDurableStore counts the per-file lookups the purge scans perform, so
// tests can assert that a file with no disconnected handle never reaches the
// store at all. lookupDelay stands in for the metadata round trip a real
// durable store costs on that lookup.
type countingDurableStore struct {
	*mockDurableStore
	lookups     atomic.Int32
	lookupDelay time.Duration
}

func newCountingDurableStore() *countingDurableStore {
	return &countingDurableStore{mockDurableStore: newMockDurableStore()}
}

func (s *countingDurableStore) GetDurableHandlesByFileHandle(
	ctx context.Context, metaHandle []byte,
) ([]*lock.PersistedDurableHandle, error) {
	s.lookups.Add(1)
	if s.lookupDelay > 0 {
		time.Sleep(s.lookupDelay)
	}
	return s.mockDurableStore.GetDurableHandlesByFileHandle(ctx, metaHandle)
}

// dataChangePurge runs the scan a WRITE performs, from a lease key that
// conflicts with every handle disconnectedHandle builds.
func dataChangePurge(h *Handler, metaHandle []byte) int {
	return h.purgeConflictingDisconnectedHandlesForDataChange(
		context.Background(), metaHandle, [16]byte{0x01}, true)
}

// persistOnDisconnect mirrors the ordering the close path uses: the count is
// published under durablePurgeMu before the row becomes visible in the store.
// It returns the store error instead of failing the test itself, because it is
// also called from goroutines, where FailNow does not stop the test.
func persistOnDisconnect(h *Handler, d *lock.PersistedDurableHandle) error {
	h.durablePurgeMu.Lock()
	h.noteDisconnectedHandle(d.MetadataHandle)
	err := h.DurableStore.PutDurableHandle(context.Background(), d)
	h.durablePurgeMu.Unlock()
	return err
}

func disconnectedHandle(id string, metaHandle []byte, leaseKey [16]byte) *lock.PersistedDurableHandle {
	return &lock.PersistedDurableHandle{
		ID:             id,
		MetadataHandle: metaHandle,
		LeaseKey:       leaseKey,
		LeaseState:     smbLeaseRead | smbLeaseHandle,
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
	}
}

// TestDataChangePurgeSkipsStoreWithoutDisconnectedHandles is the fast path the
// whole change exists for: a WRITE against a file nobody disconnected from
// must not reach the durable store.
func TestDataChangePurgeSkipsStoreWithoutDisconnectedHandles(t *testing.T) {
	store := newCountingDurableStore()
	h := &Handler{DurableStore: store}

	for i := 0; i < 100; i++ {
		if purged := dataChangePurge(h, []byte("quiet-file")); purged != 0 {
			t.Fatalf("purged = %d, want 0", purged)
		}
	}
	if got := store.lookups.Load(); got != 0 {
		t.Errorf("store lookups = %d, want 0 (fast path must skip the lookup)", got)
	}
}

// TestDataChangePurgeSeesHandleFromDisconnect proves the fast path does not
// skip a live case: once the close path has persisted a disconnected handle,
// a conflicting data change still purges it.
func TestDataChangePurgeSeesHandleFromDisconnect(t *testing.T) {
	metaHandle := []byte("busy-file")
	store := newCountingDurableStore()
	h := &Handler{DurableStore: store}

	if err := persistOnDisconnect(h, disconnectedHandle("foreign", metaHandle, [16]byte{0x05})); err != nil {
		t.Fatalf("persistOnDisconnect: %v", err)
	}

	if !h.hasDisconnectedHandles(metaHandle) {
		t.Fatal("hasDisconnectedHandles = false while a disconnected row exists")
	}
	if purged := dataChangePurge(h, metaHandle); purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if store.count() != 0 {
		t.Errorf("store still holds %d handles, want 0", store.count())
	}

	// The scan reconciled the count back to zero, so the file returns to the
	// fast path without waiting for the scavenger.
	before := store.lookups.Load()
	dataChangePurge(h, metaHandle)
	if store.lookups.Load() != before {
		t.Error("purge scan did not reconcile the count; file stayed on the slow path")
	}
}

// TestDataChangePurgeSeesHandleFromPreviousProcess covers the handle a restart
// knows only from the durable store: seeding restores the count, so a data
// change still purges it instead of taking the fast path over it.
func TestDataChangePurgeSeesHandleFromPreviousProcess(t *testing.T) {
	metaHandle := []byte("survivor-file")
	store := newCountingDurableStore()
	// Written by the process that died — no in-memory state accompanies it.
	if err := store.PutDurableHandle(context.Background(),
		disconnectedHandle("from-previous-process", metaHandle, [16]byte{0x05})); err != nil {
		t.Fatalf("PutDurableHandle: %v", err)
	}

	h := &Handler{DurableStore: store}
	h.SeedFromDurableHandles(context.Background(), store)

	if purged := dataChangePurge(h, metaHandle); purged != 1 {
		t.Fatalf("purged = %d, want 1 (seeded handle was skipped)", purged)
	}
}

// TestSeedSkipsConnectedHandles keeps the seed honest: rows that were never
// disconnected must not put their file on the slow path.
func TestSeedSkipsConnectedHandles(t *testing.T) {
	metaHandle := []byte("connected-file")
	store := newCountingDurableStore()
	connected := disconnectedHandle("still-connected", metaHandle, [16]byte{0x05})
	connected.DisconnectedAt = time.Time{}
	if err := store.PutDurableHandle(context.Background(), connected); err != nil {
		t.Fatalf("PutDurableHandle: %v", err)
	}

	h := &Handler{DurableStore: store}
	h.SeedFromDurableHandles(context.Background(), store)

	if h.hasDisconnectedHandles(metaHandle) {
		t.Error("hasDisconnectedHandles = true for a handle that never disconnected")
	}
}

// TestScavengerExpiryClearsDisconnectedCount covers the terminal state of a
// client that never comes back: expiry removes the row, so the file must fall
// back to the fast path even though no purge scan ever runs on it.
func TestScavengerExpiryClearsDisconnectedCount(t *testing.T) {
	metaHandle := []byte("abandoned-file")
	store := newCountingDurableStore()
	h := &Handler{DurableStore: store}

	expired := disconnectedHandle("abandoned", metaHandle, [16]byte{0x05})
	expired.DisconnectedAt = time.Now().Add(-2 * time.Minute)
	expired.TimeoutMs = 1000
	if err := persistOnDisconnect(h, expired); err != nil {
		t.Fatalf("persistOnDisconnect: %v", err)
	}

	s := NewDurableHandleScavenger(store, h, time.Minute, 60000, time.Now())
	s.cleanupAndDelete(context.Background(), expired)

	if h.hasDisconnectedHandles(metaHandle) {
		t.Error("hasDisconnectedHandles = true after the scavenger removed the row")
	}
}

// TestScavengerExpiryKeepsCountForUncountedRow covers the row the store may
// transiently hold without a DisconnectedAt: it looks infinitely expired, so
// the scavenger reaches it, but it was never counted. Dropping a count for it
// would steal the count of a genuinely disconnected handle on the same file
// and let a later data change skip purging that one.
func TestScavengerExpiryKeepsCountForUncountedRow(t *testing.T) {
	metaHandle := []byte("mixed-file")
	store := newCountingDurableStore()
	h := &Handler{DurableStore: store}

	// A real disconnected handle, counted the way the close path counts it.
	if err := persistOnDisconnect(h, disconnectedHandle("real", metaHandle, [16]byte{0x05})); err != nil {
		t.Fatalf("persistOnDisconnect: %v", err)
	}
	// A pre-disconnect row on the same file, never counted.
	preDisconnect := disconnectedHandle("pre-disconnect", metaHandle, [16]byte{0x06})
	preDisconnect.DisconnectedAt = time.Time{}
	if err := store.PutDurableHandle(context.Background(), preDisconnect); err != nil {
		t.Fatalf("PutDurableHandle: %v", err)
	}

	s := NewDurableHandleScavenger(store, h, time.Minute, 60000, time.Now())
	s.cleanupAndDelete(context.Background(), preDisconnect)

	if !h.hasDisconnectedHandles(metaHandle) {
		t.Fatal("expiring an uncounted row cleared the count of a live disconnected handle")
	}
	if purged := dataChangePurge(h, metaHandle); purged != 1 {
		t.Errorf("purged = %d, want 1", purged)
	}
}

// TestDisconnectAndDataChangeRace runs the two sides against each other: one
// half persists disconnected handles the way close does, the other half runs
// the data-change scan. Under -race this covers the count's own locking; the
// closing assertions cover convergence — every persisted row is either purged
// by a scan that saw it, or still present and still counted, never present and
// uncounted (which is what would let a later write skip it).
func TestDisconnectAndDataChangeRace(t *testing.T) {
	metaHandle := []byte("contended-file")
	store := newCountingDurableStore()
	h := &Handler{DurableStore: store}

	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := persistOnDisconnect(h, disconnectedHandle(
				string(rune('a'+i)), metaHandle, [16]byte{byte(0x10 + i)})); err != nil {
				errs <- err
			}
		}(i)
		go func() {
			defer wg.Done()
			dataChangePurge(h, metaHandle)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("persistOnDisconnect: %v", err)
	}

	// Whatever survived the interleaving is still counted, so this final scan
	// is guaranteed to run rather than take the fast path.
	if remaining := store.count(); remaining > 0 && !h.hasDisconnectedHandles(metaHandle) {
		t.Fatalf("%d disconnected rows left but the file is on the fast path", remaining)
	}
	dataChangePurge(h, metaHandle)
	if store.count() != 0 {
		t.Errorf("store still holds %d handles after the final scan", store.count())
	}
	if h.hasDisconnectedHandles(metaHandle) {
		t.Error("count did not reconcile to empty after the final scan")
	}
}

// BenchmarkDataChangePurge measures what every WRITE pays for the durable-handle
// conflict scan on a file nobody has disconnected from — the steady state. The
// slow-store case gives the lookup the cost of a metadata round trip, which is
// what the scan talks to in a deployment.
func BenchmarkDataChangePurge(b *testing.B) {
	for _, tc := range []struct {
		name        string
		lookupDelay time.Duration
	}{
		{"fast-store", 0},
		{"slow-store", 50 * time.Microsecond},
	} {
		b.Run(tc.name, func(b *testing.B) {
			store := newCountingDurableStore()
			store.lookupDelay = tc.lookupDelay
			h := &Handler{DurableStore: store}
			metaHandle := []byte("bench-file")

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					dataChangePurge(h, metaHandle)
				}
			})
		})
	}
}
