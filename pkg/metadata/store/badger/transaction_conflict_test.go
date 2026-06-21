package badger

import (
	goerrors "errors"
	"path/filepath"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// TestWithTransaction_RetryExhaustedConflictIsWrapped asserts that when the
// SSI optimistic-concurrency retry budget is exhausted, WithTransaction
// returns the conflict WRAPPED into an mderrors.StoreError{Code: ErrConflict}
// (recognizable codebase-wide via mderrors.IsConflictError /
// errors.As(*StoreError)) rather than leaking the raw badgerdb.ErrConflict
// sentinel.
//
// Bug #1245-B: the raw sentinel leaked through, so the rollup persister's
// mapObjectIDConflict (which only recognizes *StoreError{Code:ErrConflict}
// or postgres 23505) could not classify the badger SSI hot-key abort as a
// benign object_id conflict → the rollup ticker re-ran the same payloadID
// forever (151 retries/24h) without converging.
func TestWithTransaction_RetryExhaustedConflictIsWrapped(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := NewBadgerMetadataStoreWithDefaults(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := t.Context()
	hotKey := []byte("itest:hotkey")

	// Seed the contended key so every attempt reads then writes it.
	if uerr := store.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set(hotKey, []byte("v0"))
	}); uerr != nil {
		t.Fatalf("seed hot key: %v", uerr)
	}

	// fn reads the hot key (recording a read-conflict dependency) and writes
	// it. After the read but before the closure returns, an out-of-band
	// committed write mutates the same key, guaranteeing the in-flight txn's
	// commit aborts with ErrConflict on EVERY attempt — so the retry budget is
	// exhausted and WithTransaction must surface a wrapped conflict.
	gen := 0
	txErr := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		bt := tx.(*badgerTransaction)
		// Read to establish the SSI read dependency on hotKey.
		if _, gerr := bt.txn.Get(hotKey); gerr != nil {
			return gerr
		}
		// Write the key inside the txn (so the commit actually conflicts).
		if serr := bt.txn.Set(hotKey, []byte("inflight")); serr != nil {
			return serr
		}
		// Out-of-band committed write to the same key, forcing this attempt's
		// commit to abort. Use a fresh value each attempt.
		gen++
		val := []byte{byte('a' + gen)}
		return store.db.Update(func(txn *badgerdb.Txn) error {
			return txn.Set(hotKey, val)
		})
	})

	if txErr == nil {
		t.Fatalf("expected a retry-exhausted conflict error, got nil")
	}

	// MUST be recognizable as an mderrors conflict (this is what
	// mapObjectIDConflict / isObjectIDConflict rely on).
	if !mderrors.IsConflictError(txErr) {
		t.Fatalf("retry-exhausted conflict not recognized as mderrors conflict: %v", txErr)
	}

	// The raw badger sentinel MUST still be reachable through the unwrap chain
	// for diagnostics, but it MUST NOT be the top-level returned value.
	if txErr == badgerdb.ErrConflict { //nolint:errorlint // intentional identity check: the raw sentinel must not be returned bare
		t.Fatalf("WithTransaction leaked the raw badgerdb.ErrConflict sentinel; want a wrapped *StoreError")
	}
	if !goerrors.Is(txErr, badgerdb.ErrConflict) {
		t.Fatalf("wrapped conflict lost the underlying badgerdb.ErrConflict cause: %v", txErr)
	}
}
