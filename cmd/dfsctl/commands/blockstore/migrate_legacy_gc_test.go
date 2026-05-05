package blockstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
)

// failingDeleteRemoteStore wraps a stubRemoteStore and lets a test
// substitute DeleteBlock to return an error for selected keys.
type failingDeleteRemoteStore struct {
	*stubRemoteStore
	deleteFn func(ctx context.Context, key string) error
	deletes  atomic.Int64
}

func (f *failingDeleteRemoteStore) DeleteBlock(ctx context.Context, key string) error {
	f.deletes.Add(1)
	if f.deleteFn != nil {
		return f.deleteFn(ctx, key)
	}
	return f.stubRemoteStore.DeleteBlock(ctx, key)
}

var _ remote.RemoteStore = (*failingDeleteRemoteStore)(nil)

// TestDeleteLegacyKeys_Happy covers behavior 4: 100 legacy keys exist;
// after deleteLegacyKeys, the store reports zero matching keys.
func TestDeleteLegacyKeys_Happy(t *testing.T) {
	f := newIntegrityFixture(t)
	ctx := t.Context()

	// Seed 100 legacy keys across 10 payloadIDs.
	const totalKeys = 100
	for i := 0; i < totalKeys; i++ {
		payloadID := fmt.Sprintf("share/file-%d", i/10)
		key := blockstore.FormatStoreKey(payloadID, uint64(i%10))
		if err := f.stub.WriteBlock(ctx, key, []byte("legacy-data")); err != nil {
			t.Fatalf("seed WriteBlock(%s): %v", key, err)
		}
	}

	// Also seed a CAS object so we can assert it survives the sweep.
	dataCAS := []byte("cas-survivor")
	hashCAS := blockstore.ContentHash{}
	hashCAS[0] = 0xff
	casKey := blockstore.FormatCASKey(hashCAS)
	if err := f.stub.WriteBlockWithHash(ctx, casKey, hashCAS, dataCAS); err != nil {
		t.Fatalf("seed WriteBlockWithHash: %v", err)
	}

	count, err := deleteLegacyKeys(ctx, f.rt, migrateOptions{share: f.share, parallel: 4})
	if err != nil {
		t.Fatalf("deleteLegacyKeys: %v", err)
	}
	if count != totalKeys {
		t.Errorf("deleted = %d, want %d", count, totalKeys)
	}

	// Post-state: zero legacy keys, one CAS key.
	allKeys, _ := f.stub.ListByPrefix(ctx, "")
	for _, k := range allKeys {
		if !strings.HasPrefix(k, "cas/") {
			t.Errorf("legacy key %q survived sweep", k)
		}
	}
	if got, _ := f.stub.ListByPrefix(ctx, "cas/"); len(got) != 1 {
		t.Errorf("cas keys after sweep = %d, want 1 (survivor)", len(got))
	}
}

// TestDeleteLegacyKeys_PartialFailure covers behavior 5: a per-key
// DeleteBlock error is logged but does not abort the sweep; the final
// return is non-nil with the failure aggregated.
func TestDeleteLegacyKeys_PartialFailure(t *testing.T) {
	f := newIntegrityFixture(t)
	ctx := t.Context()

	// Seed 5 legacy keys; configure DeleteBlock to fail for one of them.
	const total = 5
	failingKey := blockstore.FormatStoreKey("share/file-1", 2)
	for i := 0; i < total; i++ {
		key := blockstore.FormatStoreKey("share/file-1", uint64(i))
		if err := f.stub.WriteBlock(ctx, key, []byte("data")); err != nil {
			t.Fatalf("seed WriteBlock(%s): %v", key, err)
		}
	}

	// Wrap the underlying store so the failure is observable. Use a
	// fresh failingDeleteRemoteStore composed from the existing stub.
	failing := &failingDeleteRemoteStore{stubRemoteStore: f.stub}
	failing.deleteFn = func(ctx context.Context, key string) error {
		if key == failingKey {
			return errors.New("simulated S3 access denied")
		}
		return failing.stubRemoteStore.DeleteBlock(ctx, key)
	}
	rt := newTestOfflineRuntime(f.share, f.mds, f.mds, failing, f.dataDir)

	count, err := deleteLegacyKeys(ctx, rt, migrateOptions{share: f.share, parallel: 2})
	if err == nil {
		t.Fatal("deleteLegacyKeys: expected aggregated error, got nil")
	}
	if count != total-1 {
		t.Errorf("deleted = %d, want %d", count, total-1)
	}
	if !strings.Contains(err.Error(), "1 of") {
		t.Errorf("error %q does not summarize 1-of-N failure", err.Error())
	}
	if !strings.Contains(err.Error(), failingKey) {
		t.Errorf("error %q does not identify the failing key", err.Error())
	}
}

// TestDeleteLegacyKeys_SkipsCASKeys covers the filter invariant: cas/
// objects MUST NOT be considered for deletion even if they exist
// alongside legacy keys (e.g., during the migration window).
func TestDeleteLegacyKeys_SkipsCASKeys(t *testing.T) {
	f := newIntegrityFixture(t)
	ctx := t.Context()

	// One legacy + one CAS.
	legacyKey := blockstore.FormatStoreKey("share/x", 0)
	if err := f.stub.WriteBlock(ctx, legacyKey, []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	hashCAS := blockstore.ContentHash{}
	hashCAS[0] = 0xab
	casKey := blockstore.FormatCASKey(hashCAS)
	if err := f.stub.WriteBlockWithHash(ctx, casKey, hashCAS, []byte("cas")); err != nil {
		t.Fatal(err)
	}

	count, err := deleteLegacyKeys(ctx, f.rt, migrateOptions{share: f.share, parallel: 2})
	if err != nil {
		t.Fatalf("deleteLegacyKeys: %v", err)
	}
	if count != 1 {
		t.Errorf("deleted = %d, want 1 (legacy only)", count)
	}
	// CAS key must still be readable.
	if _, err := f.stub.ReadBlock(ctx, casKey); err != nil {
		t.Errorf("CAS key was deleted by sweep: %v", err)
	}
}

// TestDeleteLegacyKeys_EmptyStore covers the no-keys-to-delete edge:
// nil error, zero count.
func TestDeleteLegacyKeys_EmptyStore(t *testing.T) {
	f := newIntegrityFixture(t)
	count, err := deleteLegacyKeys(t.Context(), f.rt, migrateOptions{share: f.share, parallel: 2})
	if err != nil {
		t.Fatalf("deleteLegacyKeys empty: %v", err)
	}
	if count != 0 {
		t.Errorf("deleted = %d, want 0", count)
	}
}

// TestDeleteLegacyKeys_NilRuntime sanity check.
func TestDeleteLegacyKeys_NilRuntime(t *testing.T) {
	_, err := deleteLegacyKeys(t.Context(), nil, migrateOptions{})
	if err == nil {
		t.Fatal("expected error for nil offlineRuntime")
	}
}
