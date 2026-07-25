package sqlite_test

import (
	"context"
	"fmt"
	"math/rand"
	"path"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// BenchmarkWriteThroughput models the production metadata-write topology behind
// a 4k random write: ONE SQLiteMetadataStore (MaxOpenConns(1)) driven by many
// concurrent protocol writers. Each op is the metadata cost the runtime pays
// per data write — a read-modify-write of the inode row: GetFile (SELECT) then
// PutFile (full-row UPDATE), inside one WithTransaction (BeginTx + Commit, a WAL
// frame, no fsync under WAL+synchronous=NORMAL).
//
// With a single connection the concurrent writers do not collide at the SQLite
// level; Go's pool serializes them by blocking to acquire the one connection.
// So this measures the true single-connection per-op ceiling — the number the
// ~19x-below-badger field result must be explained by. Run with -cpuprofile to
// see where the ~ms/op goes (VM per-statement vs BeginTx/Commit vs marshal).
//
//	go test -run '^$' -bench BenchmarkWriteThroughput -benchmem -cpu 1,4,8 ./pkg/metadata/store/sqlite/
func BenchmarkWriteThroughput(b *testing.B) {
	ctx := context.Background()
	cfg := &sqlite.SQLiteMetadataStoreConfig{
		Path:        filepath.Join(b.TempDir(), "wthr.db"),
		AutoMigrate: true,
	}
	store, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
	if err != nil {
		b.Fatalf("NewSQLiteMetadataStore: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	const share = "/hot"
	if _, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Mode: 0o755}); err != nil {
		b.Fatalf("CreateRootDirectory: %v", err)
	}
	root, err := store.GetRootHandle(ctx, share)
	if err != nil {
		b.Fatalf("GetRootHandle: %v", err)
	}

	var ops int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
				f, err := tx.GetFile(ctx, root)
				if err != nil {
					return err
				}
				f.Mtime = time.Now()
				return tx.PutFile(ctx, f)
			}); err != nil {
				b.Fatalf("write txn: %v", err)
			}
			atomic.AddInt64(&ops, 1)
		}
	})
	b.StopTimer()

	// IOPS the way the bench rig reports it, so the number is directly
	// comparable to the ~340 field figure.
	secs := b.Elapsed().Seconds()
	if secs > 0 {
		b.ReportMetric(float64(atomic.LoadInt64(&ops))/secs, "iops")
	}
}

// BenchmarkWriteThroughputBatched simulates the ceiling a commit-coalescer
// would unlock: it folds batch-many inode RMWs into ONE WithTransaction, so the
// BeginTx + Commit + WAL-frame write is paid once per batch instead of per op.
// If per-op IOPS climbs with batch size, the per-commit write is the dominant
// cost and a coalescer is worth building; if it stays flat, it is not.
func BenchmarkWriteThroughputBatched(b *testing.B) {
	for _, batch := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			ctx := context.Background()
			cfg := &sqlite.SQLiteMetadataStoreConfig{
				Path:        filepath.Join(b.TempDir(), "wthr_batch.db"),
				AutoMigrate: true,
			}
			store, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
			if err != nil {
				b.Fatalf("NewSQLiteMetadataStore: %v", err)
			}
			b.Cleanup(func() { _ = store.Close() })
			const share = "/hot"
			if _, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Mode: 0o755}); err != nil {
				b.Fatalf("CreateRootDirectory: %v", err)
			}
			root, err := store.GetRootHandle(ctx, share)
			if err != nil {
				b.Fatalf("GetRootHandle: %v", err)
			}

			var ops int64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
					for j := 0; j < batch; j++ {
						f, err := tx.GetFile(ctx, root)
						if err != nil {
							return err
						}
						f.Mtime = time.Now()
						if err := tx.PutFile(ctx, f); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatalf("batched txn: %v", err)
				}
				atomic.AddInt64(&ops, int64(batch))
			}
			b.StopTimer()
			if secs := b.Elapsed().Seconds(); secs > 0 {
				b.ReportMetric(float64(atomic.LoadInt64(&ops))/secs, "iops")
			}
		})
	}
}

// BenchmarkWriteThroughputScattered is the faithful field shape: instead of
// rewriting one hot inode, it pre-creates a large file set and each op does the
// RMW against a RANDOM file. This scatters writes across the inodes B-tree,
// grows the WAL, and drives the periodic checkpoint (which fsyncs the main DB)
// — the sqlite-specific amplification a single hot row never exercises. If the
// ~7000-IOPS hot-row ceiling collapses toward the ~340 field number here, the
// wall is scattered-write/checkpoint cost, not single-writer serialization.
func BenchmarkWriteThroughputScattered(b *testing.B) {
	ctx := context.Background()
	cfg := &sqlite.SQLiteMetadataStoreConfig{
		Path:        filepath.Join(b.TempDir(), "wthr_scatter.db"),
		AutoMigrate: true,
	}
	store, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
	if err != nil {
		b.Fatalf("NewSQLiteMetadataStore: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	const share = "/hot"
	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		b.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		b.Fatalf("GetRootHandle: %v", err)
	}

	const nFiles = 20000 // large enough that the inodes B-tree spills many pages
	handles := make([]metadata.FileHandle, nFiles)
	for i := 0; i < nFiles; i++ {
		name := fmt.Sprintf("f%06d", i)
		fullPath := path.Join(root.Path, name)
		h, err := store.GenerateHandle(ctx, share, fullPath)
		if err != nil {
			b.Fatalf("GenerateHandle: %v", err)
		}
		_, id, err := metadata.DecodeFileHandle(h)
		if err != nil {
			b.Fatalf("DecodeFileHandle: %v", err)
		}
		f := &metadata.File{
			ShareName: share, Path: fullPath, ID: id,
			FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000},
		}
		if err := store.PutFile(ctx, f); err != nil {
			b.Fatalf("PutFile seed: %v", err)
		}
		if err := store.SetParent(ctx, h, rootHandle); err != nil {
			b.Fatalf("SetParent: %v", err)
		}
		if err := store.SetChild(ctx, rootHandle, name, h); err != nil {
			b.Fatalf("SetChild: %v", err)
		}
		handles[i] = h
	}

	var ops int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(rand.Int63()))
		for pb.Next() {
			h := handles[rng.Intn(nFiles)]
			if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
				f, err := tx.GetFile(ctx, h)
				if err != nil {
					return err
				}
				f.Mtime = time.Now()
				return tx.PutFile(ctx, f)
			}); err != nil {
				b.Fatalf("write txn: %v", err)
			}
			atomic.AddInt64(&ops, 1)
		}
	})
	b.StopTimer()

	secs := b.Elapsed().Seconds()
	if secs > 0 {
		b.ReportMetric(float64(atomic.LoadInt64(&ops))/secs, "iops")
	}
}
