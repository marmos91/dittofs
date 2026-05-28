package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// TestSnapshotHoldProvider_FilterByManifestOnDisk asserts the D-23-02
// filter: any snapshot whose manifest.hashes exists on disk contributes
// hashes, regardless of state. Snapshots whose manifest is absent (because
// the orchestrator has not written it yet, or an operator removed it) are
// short-circuited via os.IsNotExist with no error.
//
// The six rows mirror the plan's enumerated behaviors:
//
//  1. ready + manifest      → contributes (Phase 22 regression)
//  2. creating + manifest   → contributes (D-23-02 window: post-manifest, pre-flip)
//  3. failed + manifest     → contributes (D-23-02 window: retained for retry)
//  4. creating + no manifest → no contribution (pre-manifest-write, no panic)
//  5. ready + no manifest   → no contribution (operator-deleted manifest)
//  6. failed + no manifest  → no contribution (failed before manifest)
//
// The partial unique index idx_share_creating allows at most one
// creating row per share, so each row gets its own share.
func TestSnapshotHoldProvider_FilterByManifestOnDisk(t *testing.T) {
	type row struct {
		state          string
		writeManifest  bool
		wantContribute bool
		seed           byte
	}
	cases := []struct {
		name string
		row  row
	}{
		{name: "ready + manifest contributes", row: row{models.StateReady, true, true, 0x11}},
		{name: "creating + manifest contributes", row: row{models.StateCreating, true, true, 0x22}},
		{name: "failed + manifest contributes", row: row{models.StateFailed, true, true, 0x33}},
		{name: "creating + no manifest skipped", row: row{models.StateCreating, false, false, 0x44}},
		{name: "ready + no manifest skipped", row: row{models.StateReady, false, false, 0x55}},
		{name: "failed + no manifest skipped", row: row{models.StateFailed, false, false, 0x66}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newSnapshotHoldRuntime(t)
			localStoreDir := t.TempDir()

			shareName := "alpha"
			rt.sharesSvc.InjectShareForTesting(&shares.Share{
				Name:          shareName,
				MetadataStore: "memory",
			})
			require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir))

			id := snapshotHoldCreateState(t, rt, shareName, tc.row.state)

			hash := hashAll(tc.row.seed)
			if tc.row.writeManifest {
				manifestPath := (&models.Snapshot{ID: id}).ManifestPath(localStoreDir)
				snapshotHoldWriteManifest(t, manifestPath, []blockstore.ContentHash{hash})
			}

			provider := rt.snapshotHoldForRemote([]string{shareName})

			var got []blockstore.ContentHash
			err := provider.HeldHashes(context.Background(), "remote-1", []string{shareName},
				func(h blockstore.ContentHash) error {
					got = append(got, h)
					return nil
				})
			require.NoError(t, err, "missing manifest must not error (os.IsNotExist short-circuit)")

			if tc.row.wantContribute {
				require.Len(t, got, 1)
				assert.Equal(t, hash, got[0])
			} else {
				assert.Empty(t, got, "no manifest on disk must contribute zero hashes")
			}
		})
	}
}

// TestSnapshotHoldProvider_FailClosed_OnManifestStatError asserts that a
// non-IsNotExist error from os.Stat (e.g., permission denied) propagates
// as a wrapped error so the GC mark phase aborts (INV-04 fail-closed).
// Only os.IsNotExist is the no-hold short-circuit.
func TestSnapshotHoldProvider_FailClosed_OnManifestStatError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores Unix-style chmod for read access; os.Stat succeeds despite 0o000")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission-denied scenario does not apply when running as root")
	}

	rt := newSnapshotHoldRuntime(t)
	localStoreDir := t.TempDir()

	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          "alpha",
		MetadataStore: "memory",
	})
	require.NoError(t, rt.sharesSvc.SetLocalStoreDirForTesting("alpha", localStoreDir))

	id := snapshotHoldCreateReady(t, rt, "alpha")

	// Create the snapshot dir, then chmod it 0o000 so os.Stat on the
	// manifest path returns EACCES (not ENOENT). Cleanup restores perms
	// so t.TempDir teardown can recurse.
	snapDir := (&models.Snapshot{ID: id}).SnapshotDir(localStoreDir)
	require.NoError(t, os.MkdirAll(snapDir, 0o755))
	require.NoError(t, os.Chmod(snapDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(snapDir, 0o755) })

	provider := rt.snapshotHoldForRemote([]string{"alpha"})

	err := provider.HeldHashes(context.Background(), "remote-1", []string{"alpha"},
		func(h blockstore.ContentHash) error {
			t.Fatalf("callback must not be invoked when stat errors out")
			return nil
		})
	require.Error(t, err)
	assert.False(t, errors.Is(err, os.ErrNotExist),
		"non-IsNotExist error must NOT be confused with the short-circuit path")
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
