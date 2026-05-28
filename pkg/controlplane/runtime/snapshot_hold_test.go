package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// newSnapshotHoldRuntime returns a runtime backed by an in-memory SQLite
// store, with no shares registered. Callers are responsible for share
// injection + local-store-dir wiring per test.
func newSnapshotHoldRuntime(t *testing.T) *Runtime {
	t.Helper()
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return New(s)
}

// snapshotHoldWriteManifest builds a manifest with the supplied hashes
// and atomically writes it to path, creating parent directories as needed.
func snapshotHoldWriteManifest(t *testing.T, path string, hashes []blockstore.ContentHash) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	hs := blockstore.NewHashSet(len(hashes))
	for _, h := range hashes {
		hs.Add(h)
	}
	require.NoError(t, snapshot.WriteManifestAtomic(path, hs))
}

// snapshotHoldCreateReady persists a snapshot row in state=ready by
// going through the legitimate creating -> ready transition. Returns
// the generated ID.
func snapshotHoldCreateReady(t *testing.T, rt *Runtime, shareName string) string {
	t.Helper()
	ctx := context.Background()
	id, err := rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName:      shareName,
		State:          models.StateCreating,
		MetadataEngine: "sqlite",
	})
	require.NoError(t, err)
	require.NoError(t, rt.store.UpdateSnapshotState(ctx, shareName, id, models.StateReady))
	return id
}

// snapshotHoldCreateState persists a snapshot in the supplied state.
// Only "creating", "ready", and "failed" are accepted — "ready" is
// reached via the creating -> ready transition; "failed" via
// creating -> failed.
func snapshotHoldCreateState(t *testing.T, rt *Runtime, shareName, state string) string {
	t.Helper()
	ctx := context.Background()
	id, err := rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName:      shareName,
		State:          models.StateCreating,
		MetadataEngine: "sqlite",
	})
	require.NoError(t, err)
	switch state {
	case models.StateCreating:
		// nothing — already in creating
	case models.StateReady:
		require.NoError(t, rt.store.UpdateSnapshotState(ctx, shareName, id, models.StateReady))
	case models.StateFailed:
		require.NoError(t, rt.store.UpdateSnapshotState(ctx, shareName, id, models.StateFailed))
	default:
		t.Fatalf("unsupported state %q", state)
	}
	return id
}

// TestSnapshotHoldProvider_NilStore_NoOp asserts an unconfigured runtime
// (store == nil) returns nil from HeldHashes and emits zero callbacks.
func TestSnapshotHoldProvider_NilStore_NoOp(t *testing.T) {
	rt := New(nil)
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "alpha",
		MetadataStore: "memory",
	})

	provider := rt.snapshotHoldForRemote([]string{"alpha"})

	called := 0
	err := provider.HeldHashes(context.Background(), "remote-1", []string{"alpha"},
		func(h blockstore.ContentHash) error {
			called++
			return nil
		})
	require.NoError(t, err)
	assert.Zero(t, called, "no callbacks expected when store is nil")
}

// TestSnapshotHoldProvider_FiltersByReadyState asserts only state=ready
// snapshots contribute to the held set; creating/failed are skipped
// without opening any manifest file (so their missing manifests do not
// surface as errors).
func TestSnapshotHoldProvider_FiltersByReadyState(t *testing.T) {
	rt := newSnapshotHoldRuntime(t)
	localStoreDir := t.TempDir()

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "alpha",
		MetadataStore: "memory",
	})
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting("alpha", localStoreDir))

	// Three rows for the same share: ready, failed, creating. The unique
	// partial index on state='creating' allows at most one in-flight row
	// per share, so insert the terminal-state rows first by transitioning
	// out of creating, then leave the final creating row untouched.
	readyID := snapshotHoldCreateState(t, rt, "alpha", models.StateReady)
	_ = snapshotHoldCreateState(t, rt, "alpha", models.StateFailed)
	_ = snapshotHoldCreateState(t, rt, "alpha", models.StateCreating)

	want := []blockstore.ContentHash{hashAll(0x11), hashAll(0x22)}
	// Build a Snapshot value to reuse ManifestPath (state field irrelevant).
	manifestPath := (&models.Snapshot{ID: readyID}).ManifestPath(localStoreDir)
	snapshotHoldWriteManifest(t, manifestPath, want)

	provider := rt.snapshotHoldForRemote([]string{"alpha"})

	var got []blockstore.ContentHash
	err := provider.HeldHashes(context.Background(), "remote-1", []string{"alpha"},
		func(h blockstore.ContentHash) error {
			got = append(got, h)
			return nil
		})
	require.NoError(t, err)
	require.Len(t, got, 2, "exactly the two ready-row hashes")
	sortHashes(got)
	sortHashes(want)
	assert.Equal(t, want, got)
}

// TestSnapshotHoldProvider_FailClosed_OnMissingManifest asserts that a
// state=ready row with no on-disk manifest aborts the run (fail-closed,
// orphan-not-deleted preferred over live-data-deleted).
func TestSnapshotHoldProvider_FailClosed_OnMissingManifest(t *testing.T) {
	rt := newSnapshotHoldRuntime(t)
	localStoreDir := t.TempDir()

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "alpha",
		MetadataStore: "memory",
	})
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting("alpha", localStoreDir))

	// Ready row, but deliberately no WriteManifestAtomic call → file absent.
	_ = snapshotHoldCreateReady(t, rt, "alpha")

	provider := rt.snapshotHoldForRemote([]string{"alpha"})

	err := provider.HeldHashes(context.Background(), "remote-1", []string{"alpha"},
		func(h blockstore.ContentHash) error {
			t.Fatalf("callback must not be invoked when manifest is missing")
			return nil
		})
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"expected os.ErrNotExist in error chain, got %v", err)
}

// TestSnapshotHoldProvider_MemoryShare_Skipped asserts a share with no
// persistent local-store dir (memory backend) is skipped — even when DB
// rows in state=ready exist, no callbacks fire and no error surfaces.
func TestSnapshotHoldProvider_MemoryShare_Skipped(t *testing.T) {
	rt := newSnapshotHoldRuntime(t)

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "memshare",
		MetadataStore: "memory",
	})
	// No SetLocalStoreDirForTesting → localStoreDir stays "".

	_ = snapshotHoldCreateReady(t, rt, "memshare")

	provider := rt.snapshotHoldForRemote([]string{"memshare"})

	called := 0
	err := provider.HeldHashes(context.Background(), "remote-1", []string{"memshare"},
		func(h blockstore.ContentHash) error {
			called++
			return nil
		})
	require.NoError(t, err)
	assert.Zero(t, called, "memory-backed share contributes zero held hashes")
}

// TestSnapshotHoldProvider_MultipleSharesUnion asserts the provider
// streams the union of held hashes across every captured share. Ordering
// across shares is not asserted.
func TestSnapshotHoldProvider_MultipleSharesUnion(t *testing.T) {
	rt := newSnapshotHoldRuntime(t)

	dirA := t.TempDir()
	dirB := t.TempDir()

	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: "alpha", MetadataStore: "memory"})
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: "beta", MetadataStore: "memory"})
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting("alpha", dirA))
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting("beta", dirB))

	idA := snapshotHoldCreateReady(t, rt, "alpha")
	idB := snapshotHoldCreateReady(t, rt, "beta")

	wantA := []blockstore.ContentHash{hashAll(0xAA), hashAll(0xAB)}
	wantB := []blockstore.ContentHash{hashAll(0xBA), hashAll(0xBB)}
	snapshotHoldWriteManifest(t, (&models.Snapshot{ID: idA}).ManifestPath(dirA), wantA)
	snapshotHoldWriteManifest(t, (&models.Snapshot{ID: idB}).ManifestPath(dirB), wantB)

	provider := rt.snapshotHoldForRemote([]string{"alpha", "beta"})

	var got []blockstore.ContentHash
	err := provider.HeldHashes(context.Background(), "remote-1", []string{"alpha", "beta"},
		func(h blockstore.ContentHash) error {
			got = append(got, h)
			return nil
		})
	require.NoError(t, err)

	want := append([]blockstore.ContentHash{}, wantA...)
	want = append(want, wantB...)
	sortHashes(got)
	sortHashes(want)
	assert.Equal(t, want, got, "union of held hashes across both shares")
}

// sortHashes sorts a ContentHash slice in lexicographic byte order so
// per-share ordering does not destabilize assertions.
func sortHashes(hs []blockstore.ContentHash) {
	sort.Slice(hs, func(i, j int) bool {
		for k := range hs[i] {
			if hs[i][k] != hs[j][k] {
				return hs[i][k] < hs[j][k]
			}
		}
		return false
	})
}
