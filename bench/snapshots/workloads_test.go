package snapshots_test

import (
	"context"
	"testing"

	snap "github.com/marmos91/dittofs/bench/snapshots"
	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// scaleCase is one (files, blocks-per-file) point on the sweep.
type scaleCase struct {
	name          string
	files         int
	blocksPerFile int
	// large gates the case behind -short: 1e6-file points allocate
	// hundreds of MB and take seconds, too heavy for the default CI run.
	large bool
}

// scales is the shared sweep used by every benchmark below. The small
// points run in CI; the large (1e6) points run only without -short.
var scales = []scaleCase{
	{name: "1e4files_1blk", files: 1e4, blocksPerFile: 1},
	{name: "1e5files_1blk", files: 1e5, blocksPerFile: 1},
	{name: "1e5files_8blk", files: 1e5, blocksPerFile: 8},
	{name: "1e6files_1blk", files: 1e6, blocksPerFile: 1, large: true},
	{name: "1e6files_8blk", files: 1e6, blocksPerFile: 8, large: true},
}

func seedOpts(b *testing.B, sc scaleCase) snap.SeedOpts {
	b.Helper()
	return snap.SeedOpts{
		Engine:        snap.EngineMemory,
		Files:         sc.files,
		BlocksPerFile: sc.blocksPerFile,
		BlockSize:     1 << 20, // 1 MiB logical blocks
		Dedup:         1,       // all-unique: worst case for HashSet + manifest
	}
}

// BenchmarkBackup measures the metadata Backup cost (streamed dump +
// resident HashSet) per scale. Custom metrics: dump bytes and manifest
// hash count, so dump-size and HashSet growth read directly off the row.
func BenchmarkBackup(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			store, _, cleanup, err := snap.NewStore(ctx, seedOpts(b, sc))
			if err != nil {
				b.Fatalf("seed: %v", err)
			}
			defer cleanup()

			var last snap.BackupResult
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				last, err = snap.RunBackup(ctx, store)
				if err != nil {
					b.Fatalf("backup: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(last.DumpBytes), "dump_bytes")
			b.ReportMetric(float64(last.ManifestHashes), "manifest_hashes")
		})
	}
}

// BenchmarkWriteManifest measures WriteManifest (sort + hex encode, fully
// streamed). Custom metric: manifest_bytes (on-disk manifest size).
func BenchmarkWriteManifest(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			hs := buildHashSet(b, ctx, sc)

			var bytesOut int64
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				bytesOut, err = snap.RunWriteManifest(hs)
				if err != nil {
					b.Fatalf("write manifest: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(bytesOut), "manifest_bytes")
		})
	}
}

// BenchmarkReadManifest measures ReadManifest (parse into a resident
// HashSet — the restore pre-verify allocation).
func BenchmarkReadManifest(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			hs := buildHashSet(b, ctx, sc)
			manifest, err := snap.SerializeManifest(hs)
			if err != nil {
				b.Fatalf("serialize manifest: %v", err)
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := snap.RunReadManifest(manifest); err != nil {
					b.Fatalf("read manifest: %v", err)
				}
			}
		})
	}
}

// BenchmarkVerify measures VerifyRemoteDurability against an all-present
// in-memory remote at the production concurrency (16). This is the
// HEAD-probe cost; the in-memory remote has no network latency, so the
// number is a floor — multiply by real per-HEAD RTT for an S3 budget.
// Custom metric: probes (= hashes HEAD-probed).
func BenchmarkVerify(b *testing.B) {
	ctx := context.Background()
	for _, sc := range scales {
		sc := sc
		b.Run(sc.name, func(b *testing.B) {
			if sc.large && testing.Short() {
				b.Skip("large scale skipped under -short")
			}
			hs := buildHashSet(b, ctx, sc)
			rs := remotememory.New()
			defer func() { _ = rs.Close() }()
			if _, err := snap.SeedRemote(ctx, rs, hs); err != nil {
				b.Fatalf("seed remote: %v", err)
			}

			var probes int
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				probes, err = snap.RunVerify(ctx, rs, hs)
				if err != nil {
					b.Fatalf("verify: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(probes), "probes")
		})
	}
}

// buildHashSet seeds a store, runs one Backup, and returns the resulting
// HashSet for the manifest / verify benchmarks (which time a stage
// downstream of Backup and so need its output, not the timed Backup
// itself).
func buildHashSet(b *testing.B, ctx context.Context, sc scaleCase) *block.HashSet {
	b.Helper()
	store, _, cleanup, err := snap.NewStore(ctx, seedOpts(b, sc))
	if err != nil {
		b.Fatalf("seed: %v", err)
	}
	defer cleanup()
	res, err := snap.RunBackup(ctx, store)
	if err != nil {
		b.Fatalf("backup: %v", err)
	}
	return res.HashSet
}

// BenchmarkBackupBadger keeps the on-disk badger engine exercised (the
// sweep above uses the memory engine). Badger streams the dump KV-by-KV, so
// this is the streamed-create reference point.
func BenchmarkBackupBadger(b *testing.B) {
	if testing.Short() {
		b.Skip("badger backup skipped under -short")
	}
	ctx := context.Background()
	opts := snap.SeedOpts{
		Engine:        snap.EngineBadger,
		Files:         1e4,
		BlocksPerFile: 4,
		BlockSize:     1 << 20,
		Dedup:         1,
		DBPath:        b.TempDir(),
	}
	store, _, cleanup, err := snap.NewStore(ctx, opts)
	if err != nil {
		b.Fatalf("seed badger: %v", err)
	}
	defer cleanup()

	var last snap.BackupResult
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		last, err = snap.RunBackup(ctx, store)
		if err != nil {
			b.Fatalf("backup: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(last.DumpBytes), "dump_bytes")
	b.ReportMetric(float64(last.ManifestHashes), "manifest_hashes")
}
