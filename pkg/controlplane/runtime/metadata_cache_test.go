package runtime

import (
	"encoding/json"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestConfigInt64 covers the per-store config-map integer extraction used to
// read the optional badger cache overrides (block_cache_mb / index_cache_mb).
// Config maps come from json.Unmarshal (numbers -> float64) but may also carry
// int/int64/json.Number, all of which must be accepted; everything else -> 0.
func TestConfigInt64(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		key  string
		want int64
	}{
		{"float64 (json-decoded)", map[string]any{"block_cache_mb": float64(2048)}, "block_cache_mb", 2048},
		{"int", map[string]any{"block_cache_mb": 1024}, "block_cache_mb", 1024},
		{"int64", map[string]any{"block_cache_mb": int64(512)}, "block_cache_mb", 512},
		{"json.Number", map[string]any{"block_cache_mb": json.Number("777")}, "block_cache_mb", 777},
		{"absent key -> 0", map[string]any{}, "block_cache_mb", 0},
		{"wrong type -> 0", map[string]any{"block_cache_mb": "nope"}, "block_cache_mb", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configInt64(tc.m, tc.key); got != tc.want {
				t.Fatalf("configInt64: got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCreateMetadataStoreFromConfig_BadgerCacheOverride asserts that the per-
// store config-map cache keys are threaded into the opened badger store's live
// options (#1245 Bug D).
func TestCreateMetadataStoreFromConfig_BadgerCacheOverride(t *testing.T) {
	// Ensure global defaults don't mask the per-store override.
	badger.SetGlobalBadgerCacheDefaults(0, 0)

	cfg := &fakeStoreConfig{cfg: map[string]any{
		"path":           t.TempDir(),
		"block_cache_mb": float64(640),
		"index_cache_mb": float64(320),
	}}

	store, err := CreateMetadataStoreFromConfig(t.Context(), "badger", cfg)
	if err != nil {
		t.Fatalf("CreateMetadataStoreFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bs, ok := store.(*badger.BadgerMetadataStore)
	if !ok {
		t.Fatalf("expected *badger.BadgerMetadataStore, got %T", store)
	}
	const mib = int64(1) << 20
	if got, want := bs.BadgerOptions().BlockCacheSize, 640*mib; got != want {
		t.Errorf("BlockCacheSize: got %d, want %d (640 MiB)", got, want)
	}
	if got, want := bs.BadgerOptions().IndexCacheSize, 320*mib; got != want {
		t.Errorf("IndexCacheSize: got %d, want %d (320 MiB)", got, want)
	}
}

// TestCreateMetadataStoreFromConfig_BadgerNegativeCacheRejected asserts that a
// negative per-store cache override is a config error rather than silently
// falling through to auto-sizing. 0 = unset/auto, positive = explicit size;
// negative is invalid (#1245 Bug D).
func TestCreateMetadataStoreFromConfig_BadgerNegativeCacheRejected(t *testing.T) {
	badger.SetGlobalBadgerCacheDefaults(0, 0)

	cases := []struct {
		name string
		key  string
	}{
		{"negative block_cache_mb", "block_cache_mb"},
		{"negative index_cache_mb", "index_cache_mb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &fakeStoreConfig{cfg: map[string]any{
				"path": t.TempDir(),
				tc.key: float64(-1),
			}}

			store, err := CreateMetadataStoreFromConfig(t.Context(), "badger", cfg)
			if err == nil {
				if store != nil {
					_ = store.Close()
				}
				t.Fatalf("expected error for negative %s, got nil", tc.key)
			}
		})
	}
}

// fakeStoreConfig satisfies the GetConfig interface CreateMetadataStoreFromConfig expects.
type fakeStoreConfig struct{ cfg map[string]any }

func (f *fakeStoreConfig) GetConfig() (map[string]any, error) { return f.cfg, nil }
