package shares

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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
// wiring: append is mandatory on the local tier. CreateLocalStoreFromConfig
// must build an FSStore via NewWithOptions and return a store whose write
// path succeeds.
func TestCreateLocalStoreFromConfig_AppendLogMandatory(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{
			"path":          tmp,
			"max_log_bytes": float64(2_000_000_000), // 2 GB
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

	// WriteAt must succeed — the journal-backed store accepts writes immediately.
	if err := fsStore.WriteAt(ctx, "test-payload", 0, []byte("hello phase 17")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Sanity check: the share root was created at the expected location.
	// The FSStore opens the journal under `journal/` inside the share dir.
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
			"path":          tmp,
			"max_log_bytes": "not-a-number",
		},
	}

	mds := metamem.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = mds.Close() })

	store, err := CreateLocalStoreFromConfig(ctx, "fs", cfg, "bad-types", nil, mds)
	if err != nil {
		t.Fatalf("CreateLocalStoreFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Defaults still produce a working write path.
	fsStore := store.(*fs.FSStore)
	if err := fsStore.WriteAt(ctx, "p", 0, []byte("x")); err != nil {
		t.Fatalf("WriteAt under invalid-types config: %v", err)
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
// Under the journal switchover the local store owns no background goroutine
// (Start is a no-op; carve is engine-driven with a background context), so the
// caller-ctx-capture bug is structurally gone at this layer. This test now guards
// the surviving kernel: it cancels the context right after
// CreateLocalStoreFromConfig returns, then writes and reads back through the
// store, asserting it stays fully operational rather than dying with the caller's
// context. See the inline note at the assertion for the full rationale.
func TestCreateLocalStoreFromConfig_RollupSurvivesCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tmp := t.TempDir()

	cfg := &fakeBlockStoreConfig{
		cfg: map[string]any{
			"path":          tmp,
			"max_log_bytes": float64(2_000_000_000),
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

	fsStore, ok := store.(*fs.FSStore)
	if !ok {
		t.Fatalf("CreateLocalStoreFromConfig returned %T, want *fs.FSStore", store)
	}
	const payloadID = "cancel-payload"
	// 256 KiB of dedup-resistant content — comfortably past the chunker's
	// minimum so the rollup produces at least one CAS block.
	data := make([]byte, 256*1024)
	for i := range data {
		data[i] = byte(i*7 + 1)
	}
	if err := fsStore.WriteAt(context.Background(), payloadID, 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// #1405 originally guarded that the rollup pool did not bind the caller's
	// request context (which the HTTP handler cancels on return), killing an
	// in-flight rollup. Under the journal switchover the local store owns no
	// background goroutine at all — Start is a no-op and carve is driven by the
	// engine layer with a background context, so the caller-ctx-capture class of
	// bug is structurally gone here. The surviving invariant this test protects is
	// that a store BUILT with the now-cancelled ctx stays fully operational: the
	// post-cancel WriteAt above succeeded, and the bytes read back intact. A store
	// that had bound the caller ctx into its lifetime would fail one of these.
	got := make([]byte, len(data))
	n, _, err := fsStore.ReadAt(context.Background(), payloadID, 0, got)
	if err != nil {
		t.Fatalf("ReadAt after caller-context cancel (#1405): %v", err)
	}
	if n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("read-back after caller-context cancel (#1405): got %d/%d bytes, content match=%v",
			n, len(data), bytes.Equal(got, data))
	}
}

// statDir wraps os.Stat so tests can assert the block directory was
// created without importing os at every call site.
func statDir(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
