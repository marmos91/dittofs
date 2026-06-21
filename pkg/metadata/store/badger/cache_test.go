package badger

import (
	"testing"

	badger "github.com/dgraph-io/badger/v4"
)

const mib = int64(1) << 20

// TestAutoSizeCacheMB_Bounds asserts the RAM-relative auto-sizing function
// (#1245 Bug D): the caches scale with available memory and are clamped to the
// documented [min, max] bounds. In particular a 4 GiB host must get a usefully
// large cache (well above Badger's tiny default), not the floor.
func TestAutoSizeCacheMB_Bounds(t *testing.T) {
	gib := uint64(1) << 30

	cases := []struct {
		name        string
		availMem    uint64
		wantBlockMB int64
		wantIndexMB int64
	}{
		{
			// Tiny host: both dimensions clamp to the floor.
			name:        "1GiB clamps to floor",
			availMem:    1 * gib,
			wantBlockMB: minBlockCacheMB, // 1024*0.15=153 -> floor 512
			wantIndexMB: minIndexCacheMB, // 1024*0.075=76 -> floor 256
		},
		{
			// 4 GiB host (the bug report's host): fractions exceed the floor,
			// so the host gets a genuinely larger cache.
			name:        "4GiB scales above floor",
			availMem:    4 * gib,
			wantBlockMB: 614, // 4096*0.15 = 614.4 -> 614
			wantIndexMB: 307, // 4096*0.075 = 307.2 -> 307
		},
		{
			// Huge host: both dimensions clamp to the ceiling.
			name:        "256GiB clamps to ceiling",
			availMem:    256 * gib,
			wantBlockMB: maxBlockCacheMB,
			wantIndexMB: maxIndexCacheMB,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBlock, gotIndex := autoSizeCacheMB(tc.availMem)
			if gotBlock != tc.wantBlockMB {
				t.Errorf("block cache: got %d MiB, want %d MiB", gotBlock, tc.wantBlockMB)
			}
			if gotIndex != tc.wantIndexMB {
				t.Errorf("index cache: got %d MiB, want %d MiB", gotIndex, tc.wantIndexMB)
			}
			// Invariants: both dimensions are always within bounds and > 0.
			if gotBlock < minBlockCacheMB || gotBlock > maxBlockCacheMB {
				t.Errorf("block cache %d MiB out of [%d,%d]", gotBlock, minBlockCacheMB, maxBlockCacheMB)
			}
			if gotIndex < minIndexCacheMB || gotIndex > maxIndexCacheMB {
				t.Errorf("index cache %d MiB out of [%d,%d]", gotIndex, minIndexCacheMB, maxIndexCacheMB)
			}
		})
	}
}

// TestAutoSizeCacheMB_BlockNonZero pins Badger's hard requirement that the
// block cache be > 0 when compression/encryption is enabled — the auto-sizer
// must never return zero regardless of how little memory is reported.
func TestAutoSizeCacheMB_BlockNonZero(t *testing.T) {
	gotBlock, gotIndex := autoSizeCacheMB(0)
	if gotBlock <= 0 {
		t.Fatalf("block cache must be > 0, got %d", gotBlock)
	}
	if gotIndex <= 0 {
		t.Fatalf("index cache must be > 0, got %d", gotIndex)
	}
}

// TestResolveCacheSizesMB_Precedence asserts the resolution precedence:
// explicit per-store config > global config > RAM-relative auto-sizing, with
// each dimension resolved independently.
func TestResolveCacheSizesMB_Precedence(t *testing.T) {
	// Pin a deterministic memory figure so auto-sizing is predictable.
	availMem := uint64(4) << 30 // 4 GiB -> auto block 614, index 307
	autoBlock, autoIndex := autoSizeCacheMB(availMem)

	t.Run("explicit per-store wins", func(t *testing.T) {
		SetGlobalBadgerCacheDefaults(2000, 1000)
		t.Cleanup(func() { SetGlobalBadgerCacheDefaults(0, 0) })

		block, index := resolveCacheSizesMB(BadgerMetadataStoreConfig{
			BlockCacheSizeMB: 777,
			IndexCacheSizeMB: 333,
		}, availMem)
		if block != 777 || index != 333 {
			t.Fatalf("explicit config should win: got (%d,%d), want (777,333)", block, index)
		}
	})

	t.Run("global config used when per-store unset", func(t *testing.T) {
		SetGlobalBadgerCacheDefaults(2000, 1000)
		t.Cleanup(func() { SetGlobalBadgerCacheDefaults(0, 0) })

		block, index := resolveCacheSizesMB(BadgerMetadataStoreConfig{}, availMem)
		if block != 2000 || index != 1000 {
			t.Fatalf("global config should be used: got (%d,%d), want (2000,1000)", block, index)
		}
	})

	t.Run("auto-size when nothing configured", func(t *testing.T) {
		SetGlobalBadgerCacheDefaults(0, 0)

		block, index := resolveCacheSizesMB(BadgerMetadataStoreConfig{}, availMem)
		if block != autoBlock || index != autoIndex {
			t.Fatalf("should auto-size: got (%d,%d), want (%d,%d)", block, index, autoBlock, autoIndex)
		}
	})

	t.Run("independent dimensions: block explicit, index auto", func(t *testing.T) {
		SetGlobalBadgerCacheDefaults(0, 0)

		block, index := resolveCacheSizesMB(BadgerMetadataStoreConfig{
			BlockCacheSizeMB: 1234,
		}, availMem)
		if block != 1234 {
			t.Fatalf("explicit block should win: got %d, want 1234", block)
		}
		if index != autoIndex {
			t.Fatalf("index should auto-size: got %d, want %d", index, autoIndex)
		}
	})
}

// TestBuildBadgerOptions_ThreadsCacheSizes asserts that the resolved cache
// sizes are actually threaded into the badger.Options produced by the
// option-builder — the core of the #1245 fix. We assert on the Options struct
// directly (no DB open needed).
func TestBuildBadgerOptions_ThreadsExplicitSizes(t *testing.T) {
	SetGlobalBadgerCacheDefaults(0, 0)

	opts := buildBadgerOptions(BadgerMetadataStoreConfig{
		DBPath:           t.TempDir(),
		BlockCacheSizeMB: 800,
		IndexCacheSizeMB: 400,
	}, 4<<30)

	if got, want := opts.BlockCacheSize, 800*mib; got != want {
		t.Errorf("BlockCacheSize: got %d bytes, want %d bytes (800 MiB)", got, want)
	}
	if got, want := opts.IndexCacheSize, 400*mib; got != want {
		t.Errorf("IndexCacheSize: got %d bytes, want %d bytes (400 MiB)", got, want)
	}
}

// TestBuildBadgerOptions_ThreadsAutoSizes asserts that, with nothing
// configured, the RAM-relative auto-sized values land in the Options.
func TestBuildBadgerOptions_ThreadsAutoSizes(t *testing.T) {
	SetGlobalBadgerCacheDefaults(0, 0)

	availMem := uint64(4) << 30
	autoBlock, autoIndex := autoSizeCacheMB(availMem)

	opts := buildBadgerOptions(BadgerMetadataStoreConfig{DBPath: t.TempDir()}, availMem)

	if got, want := opts.BlockCacheSize, autoBlock*mib; got != want {
		t.Errorf("BlockCacheSize: got %d bytes, want %d bytes (%d MiB auto)", got, want, autoBlock)
	}
	if got, want := opts.IndexCacheSize, autoIndex*mib; got != want {
		t.Errorf("IndexCacheSize: got %d bytes, want %d bytes (%d MiB auto)", got, want, autoIndex)
	}
}

// TestBuildBadgerOptions_CustomOptionsPassthrough asserts that an operator who
// supplies BadgerOptions takes full control: the helper returns them verbatim
// and does not override the caches.
func TestBuildBadgerOptions_CustomOptionsPassthrough(t *testing.T) {
	custom := badger.DefaultOptions(t.TempDir()).
		WithBlockCacheSize(123 * mib).
		WithIndexCacheSize(45 * mib)

	opts := buildBadgerOptions(BadgerMetadataStoreConfig{BadgerOptions: &custom}, 4<<30)

	if opts.BlockCacheSize != 123*mib {
		t.Errorf("custom BlockCacheSize not preserved: got %d", opts.BlockCacheSize)
	}
	if opts.IndexCacheSize != 45*mib {
		t.Errorf("custom IndexCacheSize not preserved: got %d", opts.IndexCacheSize)
	}
}

// TestNewBadgerStore_AppliesResolvedCacheSizes is an end-to-end assertion that
// an opened store's live badger.DB carries the resolved (here: explicit) cache
// sizes — i.e. the option-builder output actually reaches badger.Open.
func TestNewBadgerStore_AppliesResolvedCacheSizes(t *testing.T) {
	SetGlobalBadgerCacheDefaults(0, 0)

	store, err := NewBadgerMetadataStore(t.Context(), BadgerMetadataStoreConfig{
		DBPath:           t.TempDir(),
		BlockCacheSizeMB: 600,
		IndexCacheSizeMB: 300,
	})
	if err != nil {
		t.Fatalf("NewBadgerMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	opts := store.db.Opts()
	if got, want := opts.BlockCacheSize, 600*mib; got != want {
		t.Errorf("live BlockCacheSize: got %d, want %d", got, want)
	}
	if got, want := opts.IndexCacheSize, 300*mib; got != want {
		t.Errorf("live IndexCacheSize: got %d, want %d", got, want)
	}
}
