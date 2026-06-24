package shares

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/local/fs"
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

// TestCreateLocalStoreFromConfig_RollupSurvivesCallerCancel is the #1405
// regression guard. A share created live via the REST API passes the HTTP
// request context all the way down to FSStore.StartRollup. That context is
// cancelled the instant the create-share response is sent. If the rollup
// worker pool inherits the caller's cancellation, the ticker exits
// immediately and append-log data is NEVER chunked into CAS (and therefore
// never mirrored to a remote) — the exact bug a contributor hit (files on
// the mount, empty S3 bucket, block stats all zero).
//
// This test reproduces that lifecycle: it cancels the context right after
// CreateLocalStoreFromConfig returns, then writes through the append log and
// asserts the rollup ticker still converts it to CAS. With the bug
// (StartRollup(ctx)) the cancellation kills the pool and no FileBlock rows
// ever appear; the fix (StartRollup(context.WithoutCancel(ctx))) keeps the
// pool alive — shutdown is driven by store.Close instead.
func TestCreateLocalStoreFromConfig_RollupSurvivesCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{
			"path":             tmp,
			"max_log_bytes":    float64(2_000_000_000),
			"rollup_workers":   float64(2),
			"stabilization_ms": float64(50), // roll up quickly once idle
		},
	}

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	store, err := CreateLocalStoreFromConfig(ctx, "fs", cfg, "cancel-share", nil, mds)
	if err != nil {
		t.Fatalf("CreateLocalStoreFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Simulate the HTTP handler returning: the request context is cancelled
	// while the store lives on. The rollup pool must NOT die with it.
	cancel()

	fsStore := store.(*fs.FSStore)
	const payloadID = "cancel-payload"
	// 256 KiB of dedup-resistant content — comfortably past the chunker's
	// minimum so the rollup produces at least one CAS block.
	data := make([]byte, 256*1024)
	for i := range data {
		data[i] = byte(i*7 + 1)
	}
	if err := fsStore.AppendWrite(context.Background(), payloadID, data, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Poll for the rollup ticker (50ms stabilization) to fold the append log
	// into CAS chunks on disk. A live pool converts within a few ticks; a
	// pool killed by the cancelled caller context never does. We assert on
	// physical CAS chunk files (the bare FSStore, with no engine wired, still
	// writes chunks and advances the rollup offset — the FileBlock manifest is
	// the engine's job, not the store's).
	casDir := filepath.Join(tmp, "shares", "cancel-share", "blocks")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := countFiles(casDir); n > 0 {
			return // rollup ran despite the cancelled caller context — fixed.
		}
		if time.Now().After(deadline) {
			t.Fatalf("rollup never produced CAS chunks after caller-context cancel (#1405): "+
				"0 chunk files under %s", casDir)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// countFiles returns the number of regular files anywhere under root.
func countFiles(root string) int {
	n := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

// statDir wraps os.Stat so tests can assert the block directory was
// created without importing os at every call site.
func statDir(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
