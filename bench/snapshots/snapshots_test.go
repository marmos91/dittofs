package snapshots_test

import (
	"context"
	"testing"

	snap "github.com/marmos91/dittofs/bench/snapshots"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
)

// TestPipeline exercises seed → backup → manifest → verify end to end at a
// tiny scale so CI (which runs benchmarks under -short, skipping the heavy
// cases) still covers the fixture and every workload helper.
func TestPipeline(t *testing.T) {
	ctx := context.Background()
	const (
		files  = 50
		blocks = 3
	)
	store, unique, cleanup, err := snap.NewStore(ctx, snap.SeedOpts{
		Engine:        snap.EngineMemory,
		Files:         files,
		BlocksPerFile: blocks,
		BlockSize:     4096,
		Dedup:         1,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer cleanup()

	if want := files * blocks; unique != want {
		t.Fatalf("unique hashes = %d, want %d (all-unique)", unique, want)
	}

	backup, err := snap.RunBackup(ctx, store)
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	if backup.ManifestHashes != unique {
		t.Fatalf("backup manifest hashes = %d, want %d", backup.ManifestHashes, unique)
	}
	if backup.DumpBytes == 0 {
		t.Fatal("backup dump bytes = 0, want > 0")
	}

	mBytes, err := snap.RunWriteManifest(backup.HashSet)
	if err != nil {
		t.Fatalf("RunWriteManifest: %v", err)
	}
	// 64 hex chars + LF per hash.
	if want := int64(unique) * 65; mBytes != want {
		t.Fatalf("manifest bytes = %d, want %d", mBytes, want)
	}

	manifest, err := snap.SerializeManifest(backup.HashSet)
	if err != nil {
		t.Fatalf("SerializeManifest: %v", err)
	}
	parsed, err := snap.RunReadManifest(manifest)
	if err != nil {
		t.Fatalf("RunReadManifest: %v", err)
	}
	if parsed != unique {
		t.Fatalf("read-manifest hashes = %d, want %d", parsed, unique)
	}

	rs := remotememory.New()
	defer func() { _ = rs.Close() }()
	seeded, err := snap.SeedRemote(ctx, rs, backup.HashSet)
	if err != nil {
		t.Fatalf("SeedRemote: %v", err)
	}
	if seeded != unique {
		t.Fatalf("seeded remote = %d, want %d", seeded, unique)
	}
	probes, err := snap.RunVerify(ctx, rs, backup.HashSet)
	if err != nil {
		t.Fatalf("RunVerify: %v", err)
	}
	if probes != unique {
		t.Fatalf("verify probes = %d, want %d", probes, unique)
	}
}

// TestDedup confirms the dedup factor collapses the unique-hash count.
func TestDedup(t *testing.T) {
	ctx := context.Background()
	_, unique, cleanup, err := snap.NewStore(ctx, snap.SeedOpts{
		Engine:        snap.EngineMemory,
		Files:         40,
		BlocksPerFile: 1,
		BlockSize:     4096,
		Dedup:         4,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer cleanup()

	// 40 blocks, every 4th sharing a hash → 10 unique.
	if want := 10; unique != want {
		t.Fatalf("unique hashes = %d, want %d", unique, want)
	}
}
