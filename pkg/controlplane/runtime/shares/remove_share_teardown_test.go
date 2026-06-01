package shares

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLocalEngineStore builds a real local-only *engine.Store for the RemoveShare
// teardown tests. It is intentionally minimal (no remote, no coordinator) — the
// teardown path only needs a live Close.
func newLocalEngineStore(t *testing.T) *engine.Store {
	t.Helper()
	local := memory.New()
	// The in-memory metadata store satisfies EngineFileBlockStore (NewSyncer
	// requires a non-nil one); the teardown path never exercises it.
	fbs := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = fbs.Close() })
	syncer := engine.NewSyncer(local, nil, fbs, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:          local,
		Syncer:         syncer,
		FileBlockStore: fbs,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	return bs
}

// TestRemoveShare_OrderedTeardown_ClosesStoreAndWipesSnapshots verifies the
// REVIEW M4 ordered teardown: the registry entry is dropped, the snapshots
// dir is removed, and the block store is Closed (drain-safe) — all in one
// RemoveShare call returning nil on the happy path.
func TestRemoveShare_OrderedTeardown_ClosesStoreAndWipesSnapshots(t *testing.T) {
	svc := New()
	bs := newLocalEngineStore(t)

	localDir := t.TempDir()
	snapsDir := filepath.Join(localDir, "snapshots")
	if err := os.MkdirAll(filepath.Join(snapsDir, "snap-1"), 0o755); err != nil {
		t.Fatalf("seed snapshots dir: %v", err)
	}

	svc.InjectShareForTesting(&Share{
		Name:          "/export",
		BlockStore:    bs,
		localStoreDir: localDir,
	})

	if err := svc.RemoveShare("/export"); err != nil {
		t.Fatalf("RemoveShare returned error on happy path: %v", err)
	}

	// Registry entry gone.
	if _, err := svc.GetShare("/export"); err == nil {
		t.Fatal("share still present in registry after RemoveShare")
	}
	// Snapshots dir wiped.
	if _, err := os.Stat(snapsDir); !os.IsNotExist(err) {
		t.Fatalf("snapshots dir still present after RemoveShare (stat err=%v)", err)
	}
	// Block store closed (drain-safe Close ran): a subsequent op must fail
	// fast with ErrStoreClosed rather than panic.
	if _, err := bs.GetSize(context.Background(), "any"); !errors.Is(err, engine.ErrStoreClosed) {
		t.Fatalf("block store not closed by RemoveShare; GetSize err=%v", err)
	}
}

// TestRemoveShare_ContinuesPastSnapshotError_AggregatesAndClosesStore verifies
// M4's "best-effort, continue past an individual error, aggregate" property:
// when the snapshots-dir removal fails, RemoveShare still Closes the block
// store and returns a non-nil aggregated error (rather than early-returning
// and leaking the store).
func TestRemoveShare_ContinuesPastSnapshotError_AggregatesAndClosesStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o555 does not block os.RemoveAll on Windows (dir mode bits are not enforced); RemoveAll-failure injection only bites on Unix")
	}
	if os.Geteuid() == 0 {
		t.Skip("chmod-based RemoveAll-failure injection does not bite as root")
	}
	svc := New()
	bs := newLocalEngineStore(t)

	localDir := t.TempDir()
	snapsDir := filepath.Join(localDir, "snapshots")
	// Create snapshots/<child> then make the snapshots dir itself read+exec
	// only (no write): RemoveAll cannot unlink the child, so it errors.
	if err := os.MkdirAll(filepath.Join(snapsDir, "snap-1"), 0o755); err != nil {
		t.Fatalf("seed snapshots dir: %v", err)
	}
	if err := os.Chmod(snapsDir, 0o555); err != nil {
		t.Fatalf("chmod snapshots dir: %v", err)
	}
	// Restore perms so t.TempDir cleanup can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(snapsDir, 0o755) })

	svc.InjectShareForTesting(&Share{
		Name:          "/export",
		BlockStore:    bs,
		localStoreDir: localDir,
	})

	err := svc.RemoveShare("/export")
	if err == nil {
		t.Fatal("expected aggregated error from RemoveShare when snapshots removal fails")
	}

	// Registry entry still dropped.
	if _, gerr := svc.GetShare("/export"); gerr == nil {
		t.Fatal("share still present in registry after RemoveShare")
	}
	// Critically: the block store was still Closed despite the snapshot error
	// (teardown did NOT early-return). This is the M4 fix.
	if _, gerr := bs.GetSize(context.Background(), "any"); !errors.Is(gerr, engine.ErrStoreClosed) {
		t.Fatalf("block store NOT closed after snapshot-removal error (early return regression); GetSize err=%v", gerr)
	}
}
