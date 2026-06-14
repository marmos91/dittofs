package badger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// newLockTestStore opens a fresh BadgerMetadataStore for lock-transaction tests.
func newLockTestStore(t *testing.T) *BadgerMetadataStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// txLockStore asserts the transaction satisfies the lock.LockStore surface so a
// test can drive lock operations inside the caller's transaction.
func txLockStore(t *testing.T, tx metadata.Transaction) lock.LockStore {
	t.Helper()
	ls, ok := tx.(lock.LockStore)
	if !ok {
		t.Fatalf("badgerTransaction does not implement lock.LockStore")
	}
	return ls
}

// TestTxDeleteLocksByClient_RolledBackWithOuterTx asserts that lock deletions
// issued through the transactional DeleteLocksByClient are discarded when the
// enclosing WithTransaction returns an error. Before the fix the method opened
// its own db.Update and committed the deletions out-of-band, so a rollback (or
// an OCC retry) lost the locks permanently.
func TestTxDeleteLocksByClient_RolledBackWithOuterTx(t *testing.T) {
	ctx := context.Background()
	store := newLockTestStore(t)
	store.initLockStore()

	if err := store.lockStore.PutLock(ctx, &lock.PersistedLock{
		ID: "lk1", FileID: "f1", OwnerID: "o1", ClientID: "c1",
	}); err != nil {
		t.Fatalf("PutLock: %v", err)
	}

	sentinel := errors.New("force rollback")
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		n, derr := txLockStore(t, tx).DeleteLocksByClient(ctx, "c1")
		if derr != nil {
			return derr
		}
		if n != 1 {
			t.Fatalf("in-tx DeleteLocksByClient deleted %d; want 1", n)
		}
		return sentinel // roll the transaction back
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTransaction err = %v; want sentinel", err)
	}

	// The lock must survive because the outer transaction was rolled back.
	got, err := store.lockStore.GetLock(ctx, "lk1")
	if err != nil {
		t.Fatalf("GetLock after rollback: %v", err)
	}
	if got == nil {
		t.Fatalf("lock lk1 was deleted despite transaction rollback (out-of-band commit regression)")
	}
}

// TestTxIncrementServerEpoch_RolledBackWithOuterTx asserts that an epoch
// increment issued inside a transaction is discarded on rollback — so a
// retried transaction increments the epoch exactly once per commit rather than
// once per attempt (the double-increment bug).
func TestTxIncrementServerEpoch_RolledBackWithOuterTx(t *testing.T) {
	ctx := context.Background()
	store := newLockTestStore(t)
	store.initLockStore()

	before, err := store.lockStore.GetServerEpoch(ctx)
	if err != nil {
		t.Fatalf("GetServerEpoch: %v", err)
	}

	sentinel := errors.New("force rollback")
	err = store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		if _, ierr := txLockStore(t, tx).IncrementServerEpoch(ctx); ierr != nil {
			return ierr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTransaction err = %v; want sentinel", err)
	}

	after, err := store.lockStore.GetServerEpoch(ctx)
	if err != nil {
		t.Fatalf("GetServerEpoch after rollback: %v", err)
	}
	if after != before {
		t.Fatalf("server epoch advanced %d -> %d despite rollback (out-of-band commit regression)", before, after)
	}
}

// TestTxIncrementServerEpoch_CommitsOnce confirms the committed-path behavior:
// a successful transaction advances the epoch by exactly one.
func TestTxIncrementServerEpoch_CommitsOnce(t *testing.T) {
	ctx := context.Background()
	store := newLockTestStore(t)
	store.initLockStore()

	before, err := store.lockStore.GetServerEpoch(ctx)
	if err != nil {
		t.Fatalf("GetServerEpoch: %v", err)
	}

	var inTx uint64
	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var ierr error
		inTx, ierr = txLockStore(t, tx).IncrementServerEpoch(ctx)
		return ierr
	}); err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}

	after, err := store.lockStore.GetServerEpoch(ctx)
	if err != nil {
		t.Fatalf("GetServerEpoch after commit: %v", err)
	}
	if after != before+1 {
		t.Fatalf("server epoch = %d after commit; want %d", after, before+1)
	}
	if inTx != after {
		t.Fatalf("in-tx returned epoch %d; persisted epoch %d", inTx, after)
	}
}
