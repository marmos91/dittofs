package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ClientRecoveryStoreProvider is implemented by metadata stores that expose a
// ClientRecoveryStore. The conformance suite type-asserts the store to this
// interface and skips if unimplemented.
type ClientRecoveryStoreProvider interface {
	ClientRecoveryStore() lock.ClientRecoveryStore
}

// recoveryTimeTolerance bounds ConfirmedAt round-trip drift. Postgres stores
// TIMESTAMPTZ at microsecond resolution, so values must be chosen at that
// granularity; the tolerance absorbs any residual rounding.
const recoveryTimeTolerance = time.Millisecond

// RunClientRecoveryStoreTests runs the cross-backend conformance suite for
// ClientRecoveryStore. The factory creates a fresh MetadataStore per subtest.
func RunClientRecoveryStoreTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	store := factory(t)
	provider, ok := store.(ClientRecoveryStoreProvider)
	if !ok {
		t.Skip("store does not implement ClientRecoveryStoreProvider")
		return
	}
	_ = provider.ClientRecoveryStore()

	t.Run("PutListRoundTrip", func(t *testing.T) {
		testRecoveryPutListRoundTrip(t, factory)
	})
	t.Run("PutUpsert", func(t *testing.T) {
		testRecoveryPutUpsert(t, factory)
	})
	t.Run("Delete", func(t *testing.T) {
		testRecoveryDelete(t, factory)
	})
	t.Run("RecordReclaimComplete", func(t *testing.T) {
		testRecoveryRecordReclaimComplete(t, factory)
	})
	t.Run("ListEmpty", func(t *testing.T) {
		testRecoveryListEmpty(t, factory)
	})
	t.Run("DeleteNonExistent", func(t *testing.T) {
		testRecoveryDeleteNonExistent(t, factory)
	})
	t.Run("ReclaimCompleteNonExistent", func(t *testing.T) {
		testRecoveryReclaimCompleteNonExistent(t, factory)
	})
}

// recoveryStoreFromFactory builds a store and returns its ClientRecoveryStore.
func recoveryStoreFromFactory(t *testing.T, factory StoreFactory) lock.ClientRecoveryStore {
	t.Helper()
	store := factory(t)
	provider, ok := store.(ClientRecoveryStoreProvider)
	if !ok {
		t.Skip("store does not implement ClientRecoveryStoreProvider")
	}
	return provider.ClientRecoveryStore()
}

// assertRecoveryEqual asserts every field of a recovery record matches, with a
// tolerance on the ConfirmedAt timestamp.
func assertRecoveryEqual(t *testing.T, want, got *lock.V4ClientRecoveryRecord) {
	t.Helper()
	if want.ClientID != got.ClientID {
		t.Errorf("ClientID: want %d, got %d", want.ClientID, got.ClientID)
	}
	if want.ClientIDString != got.ClientIDString {
		t.Errorf("ClientIDString: want %q, got %q", want.ClientIDString, got.ClientIDString)
	}
	if want.BootVerifier != got.BootVerifier {
		t.Errorf("BootVerifier: want %v, got %v", want.BootVerifier, got.BootVerifier)
	}
	if want.Principal != got.Principal {
		t.Errorf("Principal: want %q, got %q", want.Principal, got.Principal)
	}
	if want.ServerEpoch != got.ServerEpoch {
		t.Errorf("ServerEpoch: want %d, got %d", want.ServerEpoch, got.ServerEpoch)
	}
	if want.ReclaimComplete != got.ReclaimComplete {
		t.Errorf("ReclaimComplete: want %v, got %v", want.ReclaimComplete, got.ReclaimComplete)
	}
	diff := want.ConfirmedAt.Sub(got.ConfirmedAt)
	if diff < 0 {
		diff = -diff
	}
	if diff > recoveryTimeTolerance {
		t.Errorf("ConfirmedAt: want %v, got %v (diff %v)", want.ConfirmedAt, got.ConfirmedAt, diff)
	}
}

// findRecovery returns the record with the given clientIDString, or nil.
func findRecovery(recs []*lock.V4ClientRecoveryRecord, clientIDString string) *lock.V4ClientRecoveryRecord {
	for _, r := range recs {
		if r.ClientIDString == clientIDString {
			return r
		}
	}
	return nil
}

func testRecoveryPutListRoundTrip(t *testing.T, factory StoreFactory) {
	store := recoveryStoreFromFactory(t, factory)
	ctx := context.Background()

	want := &lock.V4ClientRecoveryRecord{
		ClientID:        0x0000000100000007,
		ClientIDString:  "linux-client-aabbccdd",
		BootVerifier:    [8]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04},
		Principal:       "user@EXAMPLE.COM",
		ConfirmedAt:     time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC),
		ServerEpoch:     42,
		ReclaimComplete: false,
	}
	if err := store.PutClientRecovery(ctx, want); err != nil {
		t.Fatalf("PutClientRecovery() failed: %v", err)
	}

	recs, err := store.ListClientRecovery(ctx)
	if err != nil {
		t.Fatalf("ListClientRecovery() failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("ListClientRecovery() returned %d records, want 1", len(recs))
	}
	assertRecoveryEqual(t, want, recs[0])
}

func testRecoveryPutUpsert(t *testing.T, factory StoreFactory) {
	store := recoveryStoreFromFactory(t, factory)
	ctx := context.Background()

	const id = "client-upsert"
	first := &lock.V4ClientRecoveryRecord{
		ClientID:       1,
		ClientIDString: id,
		BootVerifier:   [8]byte{1, 1, 1, 1, 1, 1, 1, 1},
		Principal:      "first@REALM",
		ConfirmedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ServerEpoch:    1,
	}
	if err := store.PutClientRecovery(ctx, first); err != nil {
		t.Fatalf("PutClientRecovery(first) failed: %v", err)
	}

	second := &lock.V4ClientRecoveryRecord{
		ClientID:       2,
		ClientIDString: id, // same key
		BootVerifier:   [8]byte{2, 2, 2, 2, 2, 2, 2, 2},
		Principal:      "second@REALM",
		ConfirmedAt:    time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC),
		ServerEpoch:    2,
	}
	if err := store.PutClientRecovery(ctx, second); err != nil {
		t.Fatalf("PutClientRecovery(second) failed: %v", err)
	}

	recs, err := store.ListClientRecovery(ctx)
	if err != nil {
		t.Fatalf("ListClientRecovery() failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("upsert produced %d rows, want 1 (latest wins)", len(recs))
	}
	assertRecoveryEqual(t, second, recs[0])
}

func testRecoveryDelete(t *testing.T, factory StoreFactory) {
	store := recoveryStoreFromFactory(t, factory)
	ctx := context.Background()

	a := &lock.V4ClientRecoveryRecord{
		ClientID: 1, ClientIDString: "del-a", BootVerifier: [8]byte{9},
		Principal: "a", ConfirmedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ServerEpoch: 1,
	}
	b := &lock.V4ClientRecoveryRecord{
		ClientID: 2, ClientIDString: "del-b", BootVerifier: [8]byte{8},
		Principal: "b", ConfirmedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ServerEpoch: 1,
	}
	if err := store.PutClientRecovery(ctx, a); err != nil {
		t.Fatalf("Put(a) failed: %v", err)
	}
	if err := store.PutClientRecovery(ctx, b); err != nil {
		t.Fatalf("Put(b) failed: %v", err)
	}

	if err := store.DeleteClientRecovery(ctx, "del-a"); err != nil {
		t.Fatalf("DeleteClientRecovery() failed: %v", err)
	}

	recs, err := store.ListClientRecovery(ctx)
	if err != nil {
		t.Fatalf("ListClientRecovery() failed: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("after delete got %d records, want 1", len(recs))
	}
	if findRecovery(recs, "del-a") != nil {
		t.Errorf("deleted record del-a still present")
	}
	if findRecovery(recs, "del-b") == nil {
		t.Errorf("record del-b unexpectedly removed")
	}
}

func testRecoveryRecordReclaimComplete(t *testing.T, factory StoreFactory) {
	store := recoveryStoreFromFactory(t, factory)
	ctx := context.Background()

	rec := &lock.V4ClientRecoveryRecord{
		ClientID: 7, ClientIDString: "reclaim-client", BootVerifier: [8]byte{5, 5, 5},
		Principal: "p@R", ConfirmedAt: time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC), ServerEpoch: 3,
	}
	if err := store.PutClientRecovery(ctx, rec); err != nil {
		t.Fatalf("PutClientRecovery() failed: %v", err)
	}

	// Before marking: ReclaimComplete is false.
	recs, err := store.ListClientRecovery(ctx)
	if err != nil {
		t.Fatalf("ListClientRecovery() failed: %v", err)
	}
	got := findRecovery(recs, "reclaim-client")
	if got == nil {
		t.Fatalf("record not found before reclaim")
	}
	if got.ReclaimComplete {
		t.Errorf("ReclaimComplete true before RecordReclaimComplete")
	}

	if err := store.RecordReclaimComplete(ctx, "reclaim-client"); err != nil {
		t.Fatalf("RecordReclaimComplete() failed: %v", err)
	}

	recs, err = store.ListClientRecovery(ctx)
	if err != nil {
		t.Fatalf("ListClientRecovery() after reclaim failed: %v", err)
	}
	got = findRecovery(recs, "reclaim-client")
	if got == nil {
		t.Fatalf("record not found after reclaim")
	}
	if !got.ReclaimComplete {
		t.Errorf("ReclaimComplete not reflected in List after RecordReclaimComplete")
	}
	// Other fields must be intact after the in-place mark.
	if got.Principal != rec.Principal || got.ServerEpoch != rec.ServerEpoch ||
		got.BootVerifier != rec.BootVerifier || got.ClientID != rec.ClientID {
		t.Errorf("RecordReclaimComplete mutated unrelated fields: got %+v", got)
	}
}

func testRecoveryListEmpty(t *testing.T, factory StoreFactory) {
	store := recoveryStoreFromFactory(t, factory)
	ctx := context.Background()

	recs, err := store.ListClientRecovery(ctx)
	if err != nil {
		t.Fatalf("ListClientRecovery() on empty store errored: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("empty store returned %d records, want 0", len(recs))
	}
}

func testRecoveryDeleteNonExistent(t *testing.T, factory StoreFactory) {
	store := recoveryStoreFromFactory(t, factory)
	ctx := context.Background()

	if err := store.DeleteClientRecovery(ctx, "does-not-exist"); err != nil {
		t.Fatalf("DeleteClientRecovery(missing) errored: %v", err)
	}
}

func testRecoveryReclaimCompleteNonExistent(t *testing.T, factory StoreFactory) {
	store := recoveryStoreFromFactory(t, factory)
	ctx := context.Background()

	if err := store.RecordReclaimComplete(ctx, "does-not-exist"); err != nil {
		t.Fatalf("RecordReclaimComplete(missing) errored: %v", err)
	}
	recs, err := store.ListClientRecovery(ctx)
	if err != nil {
		t.Fatalf("ListClientRecovery() failed: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("RecordReclaimComplete on missing client created a row: got %d", len(recs))
	}
}
