package shares

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeLocalBlockStoreProvider returns a fixed local fs block-store config.
// createBlockStoreForShare resolves the config via this provider; the test
// fixture points it at a t.TempDir-backed fs store so the engine can build
// without external dependencies.
type fakeLocalBlockStoreProvider struct {
	cfg *models.BlockStoreConfig
}

func (f *fakeLocalBlockStoreProvider) GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error) {
	return f.cfg, nil
}

// makeBlockLayoutShare seeds a memory metadata store with a share whose
// ShareOptions.BlockLayout matches `layout`, then returns the populated
// store. Mirrors the storetest.createBlockLayoutShare helper from Plan
// 14-01 but lives in this package so the test stays self-contained.
func makeBlockLayoutShare(t *testing.T, layout metadata.BlockLayout, shareName string) *metamem.MemoryMetadataStore {
	t.Helper()
	ctx := context.Background()
	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	share := &metadata.Share{
		Name: shareName,
		Options: metadata.ShareOptions{
			BlockLayout: layout,
		},
	}
	if err := mds.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare(%q, %q): %v", shareName, layout, err)
	}
	// CreateRootDirectory materializes the root file row. Plan 14-01's
	// Badger fix ensured Options is preserved across this step; the
	// memory backend has always preserved Options, but mirroring the
	// storetest order keeps the wire-up faithful to production.
	if _, err := mds.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
	}); err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", shareName, err)
	}
	return mds
}

// runCreateBlockStoreForShare wires the createBlockStoreForShare path
// against the in-memory metadata store + a tmp-dir fs block store and
// returns the resulting *Share so tests can inspect its BlockStore's
// per-share BlockLayout.
func runCreateBlockStoreForShare(t *testing.T, mds *metamem.MemoryMetadataStore, shareName string) *Share {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	// Build a local fs block-store config pointing at tmp.
	rawCfg, err := json.Marshal(map[string]any{
		"path": tmp,
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	bsCfg := &models.BlockStoreConfig{
		ID:     "local-test",
		Name:   "local-test",
		Kind:   models.BlockStoreKindLocal,
		Type:   "fs",
		Config: string(rawCfg),
	}
	provider := &fakeLocalBlockStoreProvider{cfg: bsCfg}

	share := &Share{
		Name:          shareName,
		MetadataStore: "memory",
		Enabled:       true,
	}
	cfg := &ShareConfig{
		Name:              shareName,
		MetadataStore:     "memory",
		LocalBlockStoreID: "local-test",
	}

	svc := New()
	if err := svc.createBlockStoreForShare(ctx, share, cfg, provider, mds, nil, nil); err != nil {
		t.Fatalf("createBlockStoreForShare: %v", err)
	}
	t.Cleanup(func() {
		if share.BlockStore != nil {
			_ = share.BlockStore.Close()
		}
	})
	if share.BlockStore == nil {
		t.Fatalf("createBlockStoreForShare: share.BlockStore is nil")
	}
	return share
}

// TestCreateBlockStoreForShare_BlockLayoutCASOnly (Plan 14-02 task 2):
// when the share's ShareOptions.BlockLayout is cas-only, the engine
// BlockStore created by createBlockStoreForShare reports cas-only
// through its BlockLayout() getter.
func TestCreateBlockStoreForShare_BlockLayoutCASOnly(t *testing.T) {
	const shareName = "/cas-only-share"
	mds := makeBlockLayoutShare(t, metadata.BlockLayoutCASOnly, shareName)

	share := runCreateBlockStoreForShare(t, mds, shareName)

	got := share.BlockStore.BlockLayout()
	if got != metadata.BlockLayoutCASOnly {
		t.Errorf("BlockLayout() = %q, want cas-only", got)
	}
}

// TestCreateBlockStoreForShare_BlockLayoutLegacy (Plan 14-02 task 2):
// explicit legacy BlockLayout round-trips through the engine seam.
func TestCreateBlockStoreForShare_BlockLayoutLegacy(t *testing.T) {
	const shareName = "/legacy-share"
	mds := makeBlockLayoutShare(t, metadata.BlockLayoutLegacy, shareName)

	share := runCreateBlockStoreForShare(t, mds, shareName)

	got := share.BlockStore.BlockLayout()
	if got != metadata.BlockLayoutLegacy {
		t.Errorf("BlockLayout() = %q, want legacy", got)
	}
}

// TestCreateBlockStoreForShare_BlockLayoutDefaultsLegacy (Plan 14-02 task 2):
// a share whose ShareOptions has the zero-value BlockLayout coerces to
// legacy through the engine seam (D-A6 forward-compat). Mirrors
// metadata.ParseBlockLayout's empty-string-coerces-to-legacy semantics
// at the engine layer.
func TestCreateBlockStoreForShare_BlockLayoutDefaultsLegacy(t *testing.T) {
	const shareName = "/empty-share"
	// metadata.BlockLayout("") is the zero value — explicitly used to
	// document the forward-compat path that pre-Phase-14 rows hit.
	mds := makeBlockLayoutShare(t, metadata.BlockLayout(""), shareName)

	share := runCreateBlockStoreForShare(t, mds, shareName)

	got := share.BlockStore.BlockLayout()
	if got != metadata.BlockLayoutLegacy {
		t.Errorf("BlockLayout() = %q, want legacy (zero-value coercion)", got)
	}
}
