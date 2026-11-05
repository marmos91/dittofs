package shares

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// addShareWithSnapshots adds /export bound to a remote and seeds its
// snapshots directory, returning the directory.
func addShareWithSnapshots(t *testing.T, svc *Service) string {
	t.Helper()
	store := badgertest.NewInMemory(t)
	t.Cleanup(func() { _ = store.Close() })
	cfg := &ShareConfig{Name: "/export", MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID}
	if err := svc.AddShare(context.Background(), cfg, fixedStoreProvider{store: store},
		metadata.New(), memBlockStoreProvider{}, journalDefaults(t, svc)); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	svc.mu.RLock()
	dir := svc.registry["/export"].localStoreDir
	svc.mu.RUnlock()
	if dir == "" {
		t.Fatal("share has no state directory")
	}
	snaps := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(filepath.Join(snaps, "snap-1"), 0o755); err != nil {
		t.Fatalf("seed snapshots dir: %v", err)
	}
	return snaps
}

func remotesHeld(svc *Service) int {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return len(svc.remoteStores)
}

// TestRemoveShare_WipesSnapshotsAndReleasesRemote: RemoveShare drops the
// registry entry, removes <stateDir>/snapshots and releases the remote.
func TestRemoveShare_WipesSnapshotsAndReleasesRemote(t *testing.T) {
	svc := New()
	snaps := addShareWithSnapshots(t, svc)

	if err := svc.RemoveShare("/export"); err != nil {
		t.Fatalf("RemoveShare: %v", err)
	}
	if _, err := svc.GetShare("/export"); err == nil {
		t.Fatal("share still registered after RemoveShare")
	}
	if _, err := os.Stat(snaps); !os.IsNotExist(err) {
		t.Fatalf("snapshots dir still present after RemoveShare (stat err=%v)", err)
	}
	if n := remotesHeld(svc); n != 0 {
		t.Fatalf("%d remote(s) still held after RemoveShare", n)
	}
}

// TestRemoveShare_ContinuesPastSnapshotError: a snapshots directory that
// cannot be removed fails RemoveShare, but the share is still unregistered
// and its remote still released.
func TestRemoveShare_ContinuesPastSnapshotError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory mode bits do not block os.RemoveAll on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("chmod does not block os.RemoveAll as root")
	}
	svc := New()
	snaps := addShareWithSnapshots(t, svc)
	if err := os.Chmod(snaps, 0o555); err != nil {
		t.Fatalf("chmod snapshots dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(snaps, 0o755) })

	if err := svc.RemoveShare("/export"); err == nil {
		t.Fatal("RemoveShare succeeded with an unremovable snapshots dir, want its error")
	}
	if _, err := svc.GetShare("/export"); err == nil {
		t.Fatal("share still registered after RemoveShare")
	}
	if n := remotesHeld(svc); n != 0 {
		t.Fatalf("%d remote(s) still held after a failed snapshots removal", n)
	}
}
