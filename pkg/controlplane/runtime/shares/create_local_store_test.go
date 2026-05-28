package shares

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeBlockStoreConfig implements the cfg.GetConfig interface that
// CreateLocalStoreFromConfig consumes. Config values mirror what the
// server would load from the control-plane DB — floats for numeric keys
// (mirroring JSON numeric decoding).
type fakeBlockStoreConfig struct {
	cfg map[string]any
}

func (f *fakeBlockStoreConfig) GetConfig() (map[string]any, error) {
	// Return a defensive copy to match the real config source semantics.
	cp := make(map[string]any, len(f.cfg))
	for k, v := range f.cfg {
		cp[k] = v
	}
	return cp, nil
}

// TestCreateLocalStoreFromConfig_AppendLogMandatory exercises the config
// wiring: append is mandatory on the local tier. With a metadata backend
// that satisfies metadata.RollupStore, CreateLocalStoreFromConfig must
// build an FSStore via NewWithOptions, start the rollup pool, and return
// a store whose AppendWrite path succeeds.
func TestCreateLocalStoreFromConfig_AppendLogMandatory(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{
			"path":                       tmp,
			"max_log_bytes":              float64(2_000_000_000), // 2 GB
			"rollup_workers":             float64(4),
			"stabilization_ms":           float64(500),
			"orphan_log_min_age_seconds": float64(7200),
		},
	}

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	store, err := CreateLocalStoreFromConfig(ctx, "fs", cfg, "test-share", nil, mds)
	if err != nil {
		t.Fatalf("CreateLocalStoreFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fsStore, ok := store.(*fs.FSStore)
	if !ok {
		t.Fatalf("returned store type = %T, want *fs.FSStore", store)
	}

	// AppendWrite must succeed unconditionally — append is mandatory.
	if err := fsStore.AppendWrite(ctx, "test-payload", []byte("hello phase 17"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Sanity check: the share root was created at the expected location.
	// baseDir is shareDir, not shareDir/blocks; the FSStore creates
	// `blocks/` (CAS) + `logs/` (append log) inside.
	expectedShareDir := filepath.Join(tmp, "shares", "test-share")
	if _, statErr := statDir(expectedShareDir); statErr != nil {
		t.Errorf("share dir %q not created: %v", expectedShareDir, statErr)
	}
}

// TestCreateLocalStoreFromConfig_InvalidTypesIgnored proves invalid
// types for the optional config keys do NOT panic — they are
// warn-logged and ignored (matches the max_size idiom).
func TestCreateLocalStoreFromConfig_InvalidTypesIgnored(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{
			"path":                       tmp,
			"use_append_log":             "not-a-bool", // accepted then warned (no-op)
			"max_log_bytes":              "not-a-number",
			"rollup_workers":             float64(-1),
			"stabilization_ms":           float64(0),
			"orphan_log_min_age_seconds": "nope",
		},
	}

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	store, err := CreateLocalStoreFromConfig(ctx, "fs", cfg, "bad-types", nil, mds)
	if err != nil {
		t.Fatalf("CreateLocalStoreFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Defaults still produce a working append path.
	fsStore := store.(*fs.FSStore)
	if err := fsStore.AppendWrite(ctx, "p", []byte("x"), 0); err != nil {
		t.Fatalf("AppendWrite under invalid-types config: %v", err)
	}
}

// statDir wraps os.Stat so tests can assert the block directory was
// created without importing os at every call site.
func statDir(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
