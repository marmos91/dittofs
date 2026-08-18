package runtime

import (
	"context"
	"io"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ---- Fakes ----

// fakeRemoteStore is a minimal remote.RemoteStore for pointer-identity
// assertions in the dedup test.
type fakeRemoteStore struct {
	name string
}

// All methods on remote.RemoteStore. The fake is identity-only — it
// never actually performs I/O; tests assert on pointer identity and
// dedup wiring, not on byte-level behavior.

// remote.RemoteBlockStore
func (f *fakeRemoteStore) PutBlock(_ context.Context, _ string, _ io.Reader) error { return nil }
func (f *fakeRemoteStore) GetBlock(_ context.Context, _ string) ([]byte, error)    { return nil, nil }
func (f *fakeRemoteStore) GetBlockRange(_ context.Context, _ string, _, _ int64) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) DeleteBlock(_ context.Context, _ string) error { return nil }
func (f *fakeRemoteStore) WalkBlocks(_ context.Context, _ func(string, block.Meta) error) error {
	return nil
}

// remote.ChunkReader / remote.ChunkSealer
func (f *fakeRemoteStore) ReadChunk(_ context.Context, _ string, _, _ int64, _ block.ContentHash) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) SealChunk(_ context.Context, _ block.ContentHash, plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

// remote.LegacyCASStore
func (f *fakeRemoteStore) WalkLegacyChunks(_ context.Context, _ func(block.ContentHash, int64) error) error {
	return nil
}
func (f *fakeRemoteStore) ReadLegacyChunkVerified(_ context.Context, _ block.ContentHash) ([]byte, error) {
	return nil, nil
}
func (f *fakeRemoteStore) DeleteLegacyChunk(_ context.Context, _ block.ContentHash) error { return nil }

func (f *fakeRemoteStore) HealthCheck(_ context.Context) error { return nil }
func (f *fakeRemoteStore) Healthcheck(_ context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy}
}
func (f *fakeRemoteStore) Close() error { return nil }

// installCollectGarbageSpy replaces collectGarbageFn with a capturing spy
// and registers automatic restoration via t.Cleanup. Returned slice pointer
// collects every invocation's *engine.Options so tests can assert on the
// DryRun / SharePrefix contract.
func installCollectGarbageSpy(t *testing.T) *[]*engine.Options {
	t.Helper()
	captured := make([]*engine.Options, 0, 4)
	orig := collectGarbageFn
	collectGarbageFn = func(_ context.Context, _ engine.MetadataReconciler, opts *engine.Options) *engine.GCStats {
		captured = append(captured, opts)
		return &engine.GCStats{}
	}
	t.Cleanup(func() { collectGarbageFn = orig })
	return &captured
}

// installCollectGarbageLocalSpy replaces collectGarbageLocalFn with a spy that
// records each invocation's *engine.Options (one per swept share).
func installCollectGarbageLocalSpy(t *testing.T) *[]*engine.Options {
	t.Helper()
	captured := make([]*engine.Options, 0, 4)
	orig := collectGarbageLocalFn
	collectGarbageLocalFn = func(_ context.Context, _ block.Store, _ engine.MetadataReconciler, opts *engine.Options) *engine.GCStats {
		captured = append(captured, opts)
		return &engine.GCStats{IsLocalTier: true}
	}
	t.Cleanup(func() { collectGarbageLocalFn = orig })
	return &captured
}

// TestRunBlockGCLocal_SkipsInMemoryShares asserts RunBlockGCLocal does not
// invoke the local sweep for shares with no persistent gc-state root
// (in-memory backends): their chunks evaporate on restart.
func TestRunBlockGCLocal_SkipsInMemoryShares(t *testing.T) {
	captured := installCollectGarbageLocalSpy(t)
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{"/share-a": &fakeRemoteStore{name: "s3"}})

	stats, err := rt.RunBlockGCLocal(context.Background(), false)
	if err != nil {
		t.Fatalf("RunBlockGCLocal: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	for _, opts := range *captured {
		if opts.GCStateRoot == "" {
			t.Error("local sweep invoked for a share with empty GCStateRoot")
		}
		if len(opts.Shares) != 1 {
			t.Errorf("local sweep Options.Shares = %v, want exactly one share", opts.Shares)
		}
	}
}

// ---- Helpers ----

// newRuntimeForGC builds a Runtime fixture for RunBlockGC tests. Each entry
// in shareRemotes is added as a share with its remote store injected
// post-AddShare via the test-only setShareRemoteForTest helper.
func newRuntimeForGC(t *testing.T, shareRemotes map[string]remote.RemoteStore) *Runtime {
	t.Helper()
	rt := New(nil)
	ctx := context.Background()

	// Real memory metadata store keeps AddShare happy without needing a fake
	// with the full MetadataStore surface.
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	for name, rs := range shareRemotes {
		cfg := &ShareConfig{Name: name, MetadataStore: "meta", Enabled: true}
		if err := rt.AddShare(ctx, cfg); err != nil {
			t.Fatalf("AddShare(%s): %v", name, err)
		}
		rt.setShareRemoteForTest(name, rs)
	}

	return rt
}

// ---- Tests ----

// TestRunBlockGC_DedupesSharedRemoteStores asserts that two shares sharing
// the same underlying remote result in exactly one CollectGarbage call.
func TestRunBlockGC_DedupesSharedRemoteStores(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	sharedRS := &fakeRemoteStore{name: "s3-shared"}
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
		"/share-a": sharedRS,
		"/share-b": sharedRS, // same pointer
	})

	if _, err := rt.RunBlockGC(context.Background(), "", false); err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 deduped GC call, got %d", len(*captured))
	}
}

// TestRunBlockGC_DryRunPropagates asserts dryRun flows into the Options
// struct passed to CollectGarbage. engine.Options.SharePrefix was
// removed because the mark-sweep design has a global live set; the
// historical sharePrefix argument on RunBlockGC is preserved for
// caller compatibility but ignored.
func TestRunBlockGC_DryRunPropagates(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rs := &fakeRemoteStore{name: "s3-a"}
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
		"/share-a": rs,
	})

	if _, err := rt.RunBlockGC(context.Background(), "/prefix", true); err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 GC call, got %d", len(*captured))
	}
	if !(*captured)[0].DryRun {
		t.Fatal("expected DryRun=true on captured Options")
	}
}

// TestRunBlockGC_NoRemoteShares asserts RunBlockGC returns empty stats without
// error when no remote-backed shares are registered.
func TestRunBlockGC_NoRemoteShares(t *testing.T) {
	captured := installCollectGarbageSpy(t)

	rt := newRuntimeForGC(t, nil)

	stats, err := rt.RunBlockGC(context.Background(), "", false)
	if err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if len(*captured) != 0 {
		t.Fatalf("expected 0 GC calls with no remote shares, got %d", len(*captured))
	}
}

// recordingLegacyRemote exposes a fixed legacy cas/ namespace and counts the
// deletes a GC pass performs on it.
type recordingLegacyRemote struct {
	*fakeRemoteStore
	objects []block.ContentHash
	deleted int
}

func (r *recordingLegacyRemote) WalkLegacyChunks(_ context.Context, fn func(block.ContentHash, int64) error) error {
	for _, h := range r.objects {
		if err := fn(h, 16); err != nil {
			return err
		}
	}
	return nil
}

func (r *recordingLegacyRemote) DeleteLegacyChunk(_ context.Context, _ block.ContentHash) error {
	r.deleted++
	return nil
}

// TestRunBlockGC_LegacyCASPurgeWaitsForEveryShare asserts the cas/ namespace of
// a shared remote is reclaimed only once no share on that remote still carries
// a standalone (un-migrated) locator — the namespace is content-addressed and
// not scoped per share, so an early purge would delete another share's chunks.
func TestRunBlockGC_LegacyCASPurgeWaitsForEveryShare(t *testing.T) {
	installCollectGarbageSpy(t)
	installCollectGarbageLocalSpy(t)
	ctx := context.Background()

	rs := &recordingLegacyRemote{
		fakeRemoteStore: &fakeRemoteStore{name: "s3-shared"},
		objects:         []block.ContentHash{{1}, {2}},
	}
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
		"/share-a": rs,
		"/share-b": rs, // same remote config
	})

	mds, err := rt.GetMetadataStoreForShare("/share-a")
	if err != nil {
		t.Fatalf("GetMetadataStoreForShare: %v", err)
	}
	// A share still resolving a chunk through the legacy namespace.
	standalone := block.ContentHash{1}
	if err := mds.MarkSynced(ctx, standalone, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	if _, err := rt.RunBlockGC(ctx, "", false); err != nil {
		t.Fatalf("RunBlockGC: %v", err)
	}
	if rs.deleted != 0 {
		t.Fatalf("purged %d cas objects while a share was still un-migrated", rs.deleted)
	}

	// Once every share is migrated (locator rewritten onto a packed block) the
	// namespace is unreferenced. MarkSynced keeps the first locator, so the
	// stale marker has to go before the migrated one can be recorded.
	if err := mds.DeleteSynced(ctx, standalone); err != nil {
		t.Fatalf("DeleteSynced: %v", err)
	}
	if err := mds.MarkSynced(ctx, standalone, block.ChunkLocator{BlockID: "blk-1"}); err != nil {
		t.Fatalf("MarkSynced (migrated): %v", err)
	}
	if _, err := rt.RunBlockGC(ctx, "", false); err != nil {
		t.Fatalf("RunBlockGC (post-migration): %v", err)
	}
	if rs.deleted != len(rs.objects) {
		t.Fatalf("purged %d cas objects, want %d", rs.deleted, len(rs.objects))
	}
}

// TestPurgeLegacyCAS_IgnoresStaleShareSnapshot asserts the gate does not trust
// the caller's share snapshot. That snapshot is taken before the per-remote GC
// lock, so a share that registers in the window is missing from it — and here
// that missing share is the one still carrying un-migrated standalone chunks.
func TestPurgeLegacyCAS_IgnoresStaleShareSnapshot(t *testing.T) {
	ctx := context.Background()
	rt := New(nil)

	// Separate metadata stores: the stale snapshot must be unable to see the
	// second share's markers through the first share's store.
	stores := map[string]*metadatamemory.MemoryMetadataStore{
		"meta-a": metadatamemory.NewMemoryMetadataStoreWithDefaults(),
		"meta-b": metadatamemory.NewMemoryMetadataStoreWithDefaults(),
	}
	for name, ms := range stores {
		if err := rt.RegisterMetadataStore(name, ms); err != nil {
			t.Fatalf("RegisterMetadataStore(%s): %v", name, err)
		}
	}

	rs := &recordingLegacyRemote{
		fakeRemoteStore: &fakeRemoteStore{name: "s3-shared"},
		objects:         []block.ContentHash{{1}, {2}},
	}
	for _, sh := range []struct{ share, meta string }{{"/share-a", "meta-a"}, {"/share-b", "meta-b"}} {
		if err := rt.AddShare(ctx, &ShareConfig{Name: sh.share, MetadataStore: sh.meta, Enabled: true}); err != nil {
			t.Fatalf("AddShare(%s): %v", sh.share, err)
		}
		rt.setShareRemoteForTest(sh.share, rs)
	}

	// The late-registering share has not migrated yet.
	if err := stores["meta-b"].MarkSynced(ctx, block.ContentHash{1}, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	entries := rt.sharesSvc.DistinctRemoteStores()
	if len(entries) != 1 {
		t.Fatalf("expected one remote entry, got %d", len(entries))
	}
	stale := shares.RemoteStoreEntry{
		Store:    entries[0].Store,
		ConfigID: entries[0].ConfigID,
		Shares:   []string{"/share-a"}, // snapshot predating share-b's registration
	}

	rt.purgeLegacyCASForEntry(ctx, stale, false, &engine.GCStats{})
	if rs.deleted != 0 {
		t.Fatalf("purged %d cas objects using a stale share snapshot", rs.deleted)
	}
}

// TestPurgeLegacyCAS_ConfiguredShareNotRegistered asserts the gate refuses to
// purge while a share configured against the remote is absent from the
// registry. Such a share never ran its cas→blocks migration, so the standalone
// objects the purge would delete are exactly its live data.
func TestPurgeLegacyCAS_ConfiguredShareNotRegistered(t *testing.T) {
	ctx := context.Background()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)

	metaID, err := cp.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "meta", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore("meta", metadatamemory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	remoteID, err := cp.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "shared-remote", Kind: models.BlockStoreKindRemote, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore(remote): %v", err)
	}

	// Both shares are configured against that one remote.
	locals := map[string]string{
		"/share-a": createFSLocalBlockStore(t, cp, "fs-a"),
		"/share-b": createFSLocalBlockStore(t, cp, "fs-b"),
	}
	// Registered AFTER the local stores so t.Cleanup's LIFO order closes each
	// share's block store — releasing its journal fds — before those stores'
	// t.TempDir() is removed. On Windows an open handle blocks the unlink.
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})
	for name, local := range locals {
		if _, err := cp.CreateShare(ctx, &models.Share{
			Name:               name,
			MetadataStoreID:    metaID,
			LocalBlockStoreID:  local,
			RemoteBlockStoreID: &remoteID,
			Enabled:            true,
		}); err != nil {
			t.Fatalf("CreateShare(%s): %v", name, err)
		}
	}

	register := func(name string) {
		t.Helper()
		if err := rt.AddShare(ctx, &ShareConfig{
			Name:               name,
			MetadataStore:      "meta",
			LocalBlockStoreID:  locals[name],
			RemoteBlockStoreID: remoteID,
			Enabled:            true,
		}); err != nil {
			t.Fatalf("AddShare(%s): %v", name, err)
		}
	}
	// Only the first share registers — the second stands for one whose
	// AddShare failed and was warn-and-skipped.
	register("/share-a")

	rs := &recordingLegacyRemote{
		fakeRemoteStore: &fakeRemoteStore{name: "s3-shared"},
		objects:         []block.ContentHash{{1}, {2}},
	}
	entry := shares.RemoteStoreEntry{Store: rs, ConfigID: remoteID, Shares: []string{"/share-a"}}

	rt.purgeLegacyCASForEntry(ctx, entry, false, &engine.GCStats{})
	if rs.deleted != 0 {
		t.Fatalf("purged %d cas objects while a configured share was unregistered", rs.deleted)
	}

	// A row whose remote reference resolves to no configured store blocks the
	// purge too: it is most likely a stale name for this since-renamed remote.
	rows, err := cp.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	for _, row := range rows {
		if row.Name != "/share-b" {
			continue
		}
		stale := "renamed-away"
		row.RemoteBlockStoreID = &stale
		if err := cp.UpdateShare(ctx, row); err != nil {
			t.Fatalf("UpdateShare: %v", err)
		}
	}
	rt.purgeLegacyCASForEntry(ctx, entry, false, &engine.GCStats{})
	if rs.deleted != 0 {
		t.Fatalf("purged %d cas objects while a share held an unresolvable remote reference", rs.deleted)
	}

	// With every configured share registered and migrated, the purge proceeds.
	register("/share-b")
	rt.purgeLegacyCASForEntry(ctx, entry, false, &engine.GCStats{})
	if rs.deleted != len(rs.objects) {
		t.Fatalf("purged %d cas objects, want %d", rs.deleted, len(rs.objects))
	}
}
