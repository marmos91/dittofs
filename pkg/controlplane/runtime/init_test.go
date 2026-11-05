package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/controlplane/models"
	cpstore "github.com/cubbit/dittofs/pkg/controlplane/store"
	"github.com/cubbit/dittofs/pkg/metadata"

	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// setupTestRuntime creates a runtime with SQLite in-memory store and a registered metadata store.
func setupTestRuntime(t *testing.T) (*Runtime, cpstore.Store) {
	t.Helper()
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	rt := New(s)
	setJournalRoot(t, rt)
	ctx := context.Background()

	// Register a metadata store in the DB and runtime.
	metaStoreCfg := &models.MetadataStoreConfig{
		Name: "test-meta",
		Type: "badger", Config: `{"in_memory":true}`,
	}
	if _, err := s.CreateMetadataStore(ctx, metaStoreCfg); err != nil {
		t.Fatalf("failed to create metadata store config: %v", err)
	}
	metaStore := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("failed to register metadata store: %v", err)
	}

	return rt, s
}

// createBlockStoreConfig creates a block store config in the DB with memory type.
func createBlockStoreConfig(t *testing.T, s cpstore.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	cfg := &models.BlockStoreConfig{
		Name: name,
		Type: "memory",
	}
	id, err := s.CreateBlockStore(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create local block store config: %v", err)
	}
	return id
}

// createRemoteBlockStoreConfig creates a remote block store config in the DB with memory type.
func createRemoteBlockStoreConfig(t *testing.T, s cpstore.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	cfg := &models.BlockStoreConfig{
		Name: name,
		Type: "memory",
	}
	id, err := s.CreateBlockStore(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create remote block store config: %v", err)
	}
	return id
}

// TestLoadSharesResolvesBlockStoreByName reproduces #1312: a share whose
// local_block_store_id (and remote_block_store_id) column holds the block
// store *name* rather than its UUID — the shape persisted by the
// `dfsctl share edit --local/--remote <name>` REST UpdateShare path, which
// (unlike CreateShare) wrote the raw name into the DB. On a fresh runtime
// over that persisted DB the share must still load and its per-share
// BlockStore (local + remote) must resolve. Before the fix, resolution went
// strictly through GetBlockStoreByID(name) and failed with "store not found",
// so AddShare warn-skipped the share and every mount was denied.
func TestLoadSharesResolvesBlockStoreByName(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	// Block store configs created via the API carry a UUID id and a
	// human-friendly name. We deliberately ignore the returned UUIDs and
	// reference the stores by NAME from the share, mirroring the corrupted
	// rows produced by `share edit`.
	const localName = "local-fs"
	const remoteName = "remote-s3"
	_ = createBlockStoreConfig(t, s, localName)
	_ = createRemoteBlockStoreConfig(t, s, remoteName)

	metaStores, _ := s.ListMetadataStores(ctx)
	metaID := metaStores[0].ID

	remoteRef := remoteName
	share := &models.Share{
		Name:            "/demo",
		MetadataStoreID: metaID,
		BlockStoreID:    remoteRef, // NAME, not UUID (#1312 repro)
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("failed to create share: %v", err)
	}

	// Simulate a fresh server startup over the persisted DB.
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore failed: %v", err)
	}

	if !rt.ShareExists("/demo") {
		t.Fatal("share /demo did not load after restart (block store not resolved by name)")
	}
	if entries := rt.sharesSvc.DistinctRemoteStores(); len(entries) != 1 {
		t.Fatalf("expected the name-resolved remote store to be bound, got %d", len(entries))
	}
}

// TestRebindShareBlockStore_Live exercises the hot-reload: changing a running
// share's block-store binding moves it onto the new remote store live, and a
// binding that cannot be resolved is refused with the old one kept.
func TestRebindShareBlockStore_Live(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	storeA := createRemoteBlockStoreConfig(t, s, "reb-a")
	storeB := createRemoteBlockStoreConfig(t, s, "reb-b")

	metaStores, _ := s.ListMetadataStores(ctx)
	share := &models.Share{
		Name:            "/reb",
		MetadataStoreID: metaStores[0].ID,
		BlockStoreID:    storeA,
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}

	// bind persists a new binding to the DB, mirroring the PUT handler before
	// the runtime rebind is invoked.
	bind := func(blockStoreID string) {
		dbShare, err := s.GetShare(ctx, "/reb")
		if err != nil {
			t.Fatalf("GetShare(db): %v", err)
		}
		dbShare.BlockStoreID = blockStoreID
		if err := s.UpdateShare(ctx, dbShare); err != nil {
			t.Fatalf("UpdateShare: %v", err)
		}
	}
	bound := func(want string) {
		t.Helper()
		entries := rt.sharesSvc.DistinctRemoteStores()
		if len(entries) != 1 || entries[0].ConfigID != want {
			t.Fatalf("bound remotes = %+v, want exactly %s", entries, want)
		}
	}

	bound(storeA)

	bind("no-such-store")
	if err := rt.RebindShareBlockStore(ctx, "/reb"); err == nil {
		t.Fatal("rebind to an unknown block store must be refused")
	}
	bound(storeA)

	bind(storeB)
	if err := rt.RebindShareBlockStore(ctx, "/reb"); err != nil {
		t.Fatalf("rebind A->B: %v", err)
	}
	bound(storeB)
}

// A share whose block store reference resolves to nothing is reported by name,
// with the reference that could not be resolved, rather than being folded into
// the generic add-failure warning that leaves an operator guessing which
// binding is broken.
func TestLoadSharesFromStore_ReportsUnresolvableBlockStore(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	metaStores, err := s.ListMetadataStores(ctx)
	if err != nil || len(metaStores) == 0 {
		t.Fatalf("ListMetadataStores: %v", err)
	}

	if _, err := s.CreateShare(ctx, &models.Share{
		Name:            "/orphaned",
		MetadataStoreID: metaStores[0].ID,
		BlockStoreID:    "deleted-blocks",
	}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}

	reason, ok := rt.SkippedShares()["/orphaned"]
	if !ok {
		t.Fatal("share with an unresolvable block store was not recorded as skipped")
	}
	if want := "block store deleted-blocks is not configured"; reason != want {
		t.Errorf("skip reason = %q, want %q", reason, want)
	}
}

// TestLoadSharesFromStore_LeadingSlashIsOneShare drives the persisted-state
// path: two share rows whose names differ only in a leading slash. Both
// sanitize to one journal directory, so loading them must register one share
// and skip the other — registering both puts two shares in one journal and
// interleaves their writes.
func TestLoadSharesFromStore_LeadingSlashIsOneShare(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	blockID := createBlockStoreConfig(t, s, "collide-blocks")
	metaStores, err := s.ListMetadataStores(ctx)
	if err != nil {
		t.Fatalf("ListMetadataStores: %v", err)
	}
	for _, name := range []string{"alpha", "/alpha"} {
		if _, err := s.CreateShare(ctx, &models.Share{
			Name:            name,
			MetadataStoreID: metaStores[0].ID,
			BlockStoreID:    blockID,
		}); err != nil {
			t.Fatalf("CreateShare(%q): %v", name, err)
		}
	}

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}

	if got := rt.CountShares(); got != 1 {
		t.Fatalf("two spellings of one journal directory registered %d shares, want 1", got)
	}
	if _, err := rt.GetShare("/alpha"); err != nil {
		t.Fatalf(`the surviving share must be registered as "/alpha": %v`, err)
	}
	if skipped := rt.SkippedShares(); len(skipped) != 1 {
		t.Fatalf("want the colliding share reported as skipped, got %v", skipped)
	}
}

// A share whose metadata is keyed by the unfolded spelling of its name must not
// be served from a fresh root under the folded one: that would leave every file
// already under it addressable by nothing. The refusal is decided by what the
// metadata store holds, so correcting the persisted name does not bypass it.
func TestLoadSharesFromStore_RefusesToShadowAnUnfoldedNamespace(t *testing.T) {
	for _, persisted := range []string{"alpha", "/alpha"} {
		t.Run(persisted, func(t *testing.T) {
			rt, s := setupTestRuntime(t)
			ctx := context.Background()

			meta, err := rt.GetMetadataStore("test-meta")
			if err != nil {
				t.Fatalf("GetMetadataStore: %v", err)
			}

			// Durable state of a share whose metadata was keyed by the name
			// without its leading slash: a root, and one file under it.
			const unfolded = "alpha"
			root, err := meta.CreateRootDirectory(ctx, unfolded,
				&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755})
			if err != nil {
				t.Fatalf("CreateRootDirectory: %v", err)
			}
			rootHandle, err := metadata.EncodeFileHandle(root)
			if err != nil {
				t.Fatalf("EncodeFileHandle: %v", err)
			}
			file := &metadata.File{
				ID:        uuid.New(),
				ShareName: unfolded,
				Path:      "/legacy.txt",
				FileAttr:  metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, Size: 7},
			}
			fileHandle, err := metadata.EncodeFileHandle(file)
			if err != nil {
				t.Fatalf("EncodeFileHandle: %v", err)
			}
			if err := meta.UpdateAttrs(ctx, file); err != nil {
				t.Fatalf("UpdateAttrs: %v", err)
			}
			if err := meta.SetChild(ctx, rootHandle, "legacy.txt", fileHandle); err != nil {
				t.Fatalf("SetChild: %v", err)
			}

			blockID := createBlockStoreConfig(t, s, "unfolded-blocks")
			metaStores, err := s.ListMetadataStores(ctx)
			if err != nil {
				t.Fatalf("ListMetadataStores: %v", err)
			}
			if _, err := s.CreateShare(ctx, &models.Share{
				Name:            persisted,
				MetadataStoreID: metaStores[0].ID,
				BlockStoreID:    blockID,
			}); err != nil {
				t.Fatalf("CreateShare: %v", err)
			}

			if err := LoadSharesFromStore(ctx, rt, s); err != nil {
				t.Fatalf("LoadSharesFromStore: %v", err)
			}

			reason, ok := rt.SkippedShares()[persisted]
			if !ok {
				t.Fatalf("a share whose metadata is keyed by %q must be reported as skipped, got %v",
					unfolded, rt.SkippedShares())
			}
			if !strings.Contains(reason, unfolded) || !strings.Contains(reason, "/"+unfolded) {
				t.Errorf("skip reason must name both spellings, got %q", reason)
			}

			// No second namespace: the folded name must not have acquired a root
			// of its own, which is what would strand the files below.
			if handle, err := meta.GetRootHandle(ctx, "/"+unfolded); err == nil {
				t.Fatalf("a second root %q was minted; the files under %q are now unreachable",
					string(handle), unfolded)
			}

			// The original root still holds the file.
			entries, _, err := meta.ListChildren(ctx, rootHandle, "", 16, metadata.NamesOnly)
			if err != nil {
				t.Fatalf("ListChildren: %v", err)
			}
			if len(entries) != 1 || entries[0].Name != "legacy.txt" {
				t.Fatalf("the persisted root lost its files: %v", entries)
			}
		})
	}
}
