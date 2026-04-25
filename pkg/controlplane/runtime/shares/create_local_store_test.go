package shares

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	metamem "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeBlockStoreConfig implements the cfg.GetConfig interface that
// CreateLocalStoreFromConfig consumes. Config values mirror what the
// server would load from the control-plane DB — floats for numeric keys
// (per the plan 10-08 config contract mirroring JSON numeric decoding)
// and bools for flag keys.
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

// TestCreateLocalStoreFromConfig_AppendLogEnabled exercises the Phase 10
// config wiring: with use_append_log=true and a metadata backend that
// satisfies metadata.RollupStore, CreateLocalStoreFromConfig must build an
// FSStore via NewWithOptions, start the rollup pool, and return a store
// whose AppendWrite path is ENABLED (no ErrAppendLogDisabled).
func TestCreateLocalStoreFromConfig_AppendLogEnabled(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{
			"path":                       tmp,
			"use_append_log":             true,
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

	// AppendWrite must not return ErrAppendLogDisabled when the flag is on.
	// A successful write proves: flag parsed, opts threaded to NewWithOptions,
	// and StartRollup did not short-circuit the append path.
	err = fsStore.AppendWrite(ctx, "test-payload", []byte("hello phase 10"), 0)
	if errors.Is(err, fs.ErrAppendLogDisabled) {
		t.Fatalf("AppendWrite returned ErrAppendLogDisabled; config wiring failed")
	}
	if err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Sanity check: the block dir was created at the expected location
	// under the sanitized share name.
	expectedBlockDir := filepath.Join(tmp, "shares", "test-share", "blocks")
	if _, statErr := statDir(expectedBlockDir); statErr != nil {
		t.Errorf("block dir %q not created: %v", expectedBlockDir, statErr)
	}
}

// TestCreateLocalStoreFromConfig_AppendLogDefaultDisabled verifies the
// D-03 contract: absent use_append_log, the returned store takes the
// legacy path — AppendWrite yields ErrAppendLogDisabled. This is the
// safety net that keeps Phase 10 zero-behavior-change when the flag is
// off.
func TestCreateLocalStoreFromConfig_AppendLogDefaultDisabled(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{"path": tmp},
	}

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	store, err := CreateLocalStoreFromConfig(ctx, "fs", cfg, "legacy", nil, mds)
	if err != nil {
		t.Fatalf("CreateLocalStoreFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fsStore, ok := store.(*fs.FSStore)
	if !ok {
		t.Fatalf("returned store type = %T, want *fs.FSStore", store)
	}

	err = fsStore.AppendWrite(ctx, "test-payload", []byte("x"), 0)
	if !errors.Is(err, fs.ErrAppendLogDisabled) {
		t.Fatalf("legacy path: want ErrAppendLogDisabled, got %v", err)
	}
}

// TestCreateLocalStoreFromConfig_AppendLogInvalidTypes proves T-10-08-01
// mitigation: invalid types for the new config keys do NOT panic — they
// are warn-logged and ignored (matches the max_size idiom).
func TestCreateLocalStoreFromConfig_AppendLogInvalidTypes(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{
			"path":                       tmp,
			"use_append_log":             "not-a-bool",
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

	// Invalid use_append_log => flag stays false (default). Legacy path.
	fsStore := store.(*fs.FSStore)
	if err := fsStore.AppendWrite(ctx, "p", []byte("x"), 0); !errors.Is(err, fs.ErrAppendLogDisabled) {
		t.Fatalf("invalid use_append_log must leave flag false: got %v", err)
	}
}

// statDir wraps os.Stat so tests can assert the block directory was
// created without importing os at every call site.
func statDir(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
