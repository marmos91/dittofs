package badger

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// newCreateCacheStore builds a store with a share + root directory so the dirent
// and parent caches operate against a realistic keyspace (derivePath needs the
// parent edges).
func newCreateCacheStore(t *testing.T) (*BadgerMetadataStore, metadata.FileHandle) {
	t.Helper()
	ctx := context.Background()
	s, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const share = "/s"
	if err := s.CreateShare(ctx, &metadata.Share{Name: share}); err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateRootDirectory(ctx, share, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	if err != nil {
		t.Fatal(err)
	}
	rootHandle, err := metadata.EncodeFileHandle(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, rootHandle
}

// putDir writes a directory inode under parent linked as name, returning its
// handle. Mirrors what createEntry persists (file row + parent + child edges).
func putDir(t *testing.T, s *BadgerMetadataStore, share string, parent metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	h, err := s.GenerateHandle(ctx, share, "/"+name)
	if err != nil {
		t.Fatal(err)
	}
	_, id, err := metadata.DecodeFileHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile(ctx, &metadata.File{
		ID: id, ShareName: share,
		FileAttr: metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParent(ctx, h, parent); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChild(ctx, parent, name, h); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestWarmFileReadCache_HitAndPathless proves WarmFileReadCache populates the
// shared read cache (so the trailing read hits warm) and NEVER bakes File.Path
// into the shared cache (constraint D / #1166): the cached entry is path-less.
func TestWarmFileReadCache_HitAndPathless(t *testing.T) {
	s, _ := newCreateCacheStore(t)
	ctx := context.Background()

	// A just-created file carries a Path; warming must strip it.
	f := bigFile(2)
	f.ShareName = "/s"
	f.Path = "/dir/created.txt"
	f.Mode = 0o644
	if err := s.PutFile(ctx, f); err != nil { // persist so a real read would succeed
		t.Fatal(err)
	}
	handle, err := metadata.EncodeShareHandle("/s", f.ID)
	if err != nil {
		t.Fatal(err)
	}

	s.WarmFileReadCache(f)

	cached, ok := s.readCache.get(f.ID.String())
	if !ok {
		t.Fatal("WarmFileReadCache did not populate the read cache")
	}
	if cached.Path != "" {
		t.Fatalf("read cache holds a path %q; shared cache must be path-less (#1166)", cached.Path)
	}

	// The warm read returns the correct file (Path is derived at the service
	// layer, so the store cache being path-less is correct).
	got, err := s.GetFileForRead(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != 0o644 {
		t.Fatalf("warm read mode=%o, want 0644", got.Mode)
	}

	// Caller mutation of the warmed value must not corrupt the shared entry.
	got.Mode = 0
	again, ok := s.readCache.get(f.ID.String())
	if !ok || again.Mode != 0o644 {
		t.Fatal("warm cache entry aliased or evicted by caller mutation")
	}
}

// TestDirentCache_NegativeThenInvalidate proves the negative dirent cache
// (constraint C): a miss caches ABSENT, a subsequent SetChild for the same name
// invalidates it, and the next lookup returns the child — never a stale ENOENT.
func TestDirentCache_NegativeThenInvalidate(t *testing.T) {
	s, root := newCreateCacheStore(t)
	ctx := context.Background()
	_, rootID, _ := metadata.DecodeFileHandle(root)
	key := direntKey(rootID.String(), "foo")

	// Miss -> caches ABSENT.
	if _, err := s.GetChildForCreate(ctx, root, "foo"); !metadata.IsNotFoundError(err) {
		t.Fatalf("first lookup err=%v, want NotFound", err)
	}
	if e, ok := s.direntCache.get(key); !ok || e.present {
		t.Fatalf("expected a cached ABSENT entry, got ok=%v entry=%+v", ok, e)
	}

	// Create "foo": SetChild must invalidate the negative entry after commit.
	child := putDir(t, s, "/s", root, "foo")
	if _, ok := s.direntCache.get(key); ok {
		t.Fatal("SetChild did not invalidate the negative dirent entry")
	}

	// The next lookup must find the child, not serve a stale ENOENT.
	got, err := s.GetChildForCreate(ctx, root, "foo")
	if err != nil {
		t.Fatalf("post-create lookup err=%v; stale negative entry would give ENOENT", err)
	}
	if string(got) != string(child) {
		t.Fatal("lookup returned the wrong child handle")
	}

	// Deleting the child must invalidate the now-positive entry.
	if err := s.DeleteChild(ctx, root, "foo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.direntCache.get(key); ok {
		t.Fatal("DeleteChild did not invalidate the positive dirent entry")
	}
	if _, err := s.GetChildForCreate(ctx, root, "foo"); !metadata.IsNotFoundError(err) {
		t.Fatalf("post-delete lookup err=%v, want NotFound", err)
	}
}

// TestDirentCache_NegativeGenGuard is the constraint-C race guard: a populate
// that snapshots an OLD generation (as if it read badger just before a
// concurrent create committed) must be rejected, so no permanently-stale ABSENT
// entry is pinned.
func TestDirentCache_NegativeGenGuard(t *testing.T) {
	var c direntCache
	key := direntKey("parent", "foo")

	genBefore := c.generation()            // reader snapshots gen, then "reads" badger (absent)
	c.invalidate(key)                      // concurrent create commits + invalidates
	c.store(key, direntEntry{}, genBefore) // stale populate must be dropped

	if _, ok := c.get(key); ok {
		t.Fatal("stale ABSENT entry pinned despite a racing invalidation (gen guard failed)")
	}
}

// TestGetFileForCreate_PathFreshAfterRename proves the parent cache never serves
// a stale path across the parent's own rename (constraint D): renaming the
// directory PutFiles it, which invalidates the parent cache, so the next
// create-path parent read derives the current path.
func TestGetFileForCreate_PathFreshAfterRename(t *testing.T) {
	s, root := newCreateCacheStore(t)
	ctx := context.Background()

	dir := putDir(t, s, "/s", root, "dirA")

	got, err := s.GetFileForCreate(ctx, dir) // caches path /dirA
	if err != nil || got.Path != "/dirA" {
		t.Fatalf("initial parent read: path=%q err=%v", got.Path, err)
	}

	// Rename /dirA -> /dirB exactly as Move does: repoint the edges AND PutFile
	// the moved directory (its ctime changes), which drives the cache invalidation.
	if err := s.DeleteChild(ctx, root, "dirA"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChild(ctx, root, "dirB", dir); err != nil {
		t.Fatal(err)
	}
	moved, err := s.GetFile(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile(ctx, moved); err != nil { // ctime bump -> dirtyFiles -> invalidate
		t.Fatal(err)
	}

	got, err = s.GetFileForCreate(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/dirB" {
		t.Fatalf("STALE PARENT PATH: got %q after rename to /dirB", got.Path)
	}
}

// TestGetChildForCreate_Concurrent is a race-detector smoke test: concurrent
// lookups + writes on the dirent cache must not race (run under -race).
func TestGetChildForCreate_Concurrent(t *testing.T) {
	s, root := newCreateCacheStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.GetChildForCreate(ctx, root, "foo")
				_ = s.SetChild(ctx, root, "foo", root)
				_ = s.DeleteChild(ctx, root, "foo")
			}
		}()
	}
	wg.Wait()
}
