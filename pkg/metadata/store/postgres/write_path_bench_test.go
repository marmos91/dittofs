//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// benchSeed creates a share holding n regular files and returns their handles.
// The share name is unique per call so repeated runs against the same database
// never collide.
func benchSeed(b *testing.B, store metadata.Store, n int) (string, []metadata.FileHandle) {
	b.Helper()
	ctx := context.Background()
	share := uniqueShare("bench")
	root, err := store.CreateRootDirectory(ctx, share,
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755})
	if err != nil {
		b.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		b.Fatalf("GetRootHandle: %v", err)
	}
	hs := make([]metadata.FileHandle, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%06d", i)
		fp := path.Join(root.Path, name)
		h, err := store.GenerateHandle(ctx, share, fp)
		if err != nil {
			b.Fatalf("GenerateHandle: %v", err)
		}
		_, id, err := metadata.DecodeFileHandle(h)
		if err != nil {
			b.Fatalf("DecodeFileHandle: %v", err)
		}
		if err := store.PutFile(ctx, &metadata.File{ShareName: share, Path: fp, ID: id,
			FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000}}); err != nil {
			b.Fatalf("PutFile seed: %v", err)
		}
		if err := store.SetParent(ctx, h, rootHandle); err != nil {
			b.Fatalf("SetParent: %v", err)
		}
		if err := store.SetChild(ctx, rootHandle, name, h); err != nil {
			b.Fatalf("SetChild: %v", err)
		}
		hs[i] = h
	}
	return share, hs
}

// BenchmarkPostgresWritePath measures the metadata cost of one data WRITE
// against a scattered file set, in the two shapes the runtime can take:
//
//	getfile_putfile — the fallback: GetFile (row + aggregate block-refs read)
//	                  then PutFile (full-row UPDATE), two statements per op
//	applydatawrite  — the DataWriteApplier fast path: one CTE statement that
//	                  reads the old size/owner and updates size/mtime/ctime in
//	                  a single round-trip
//
// Both run the same concurrency against the same seeded share, so the delta is
// the per-op SQL access pattern rather than disk or durability.
//
//	DITTOFS_TEST_POSTGRES_DSN=1 go test -tags integration -run '^$' \
//	  -bench BenchmarkPostgresWritePath ./pkg/metadata/store/postgres/
func BenchmarkPostgresWritePath(b *testing.B) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		b.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL write-path benchmark")
	}
	const nFiles = 5000
	ctx := context.Background()

	run := func(b *testing.B, op func(tx metadata.Transaction, h metadata.FileHandle) error) {
		store := pgStore(b)
		_, handles := benchSeed(b, store, nFiles)
		var ops int64
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			rng := rand.New(rand.NewSource(rand.Int63()))
			for pb.Next() {
				h := handles[rng.Intn(nFiles)]
				if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
					return op(tx, h)
				}); err != nil {
					b.Fatalf("write txn: %v", err)
				}
				atomic.AddInt64(&ops, 1)
			}
		})
		b.StopTimer()
		if secs := b.Elapsed().Seconds(); secs > 0 {
			b.ReportMetric(float64(atomic.LoadInt64(&ops))/secs, "iops")
		}
	}

	b.Run("getfile_putfile", func(b *testing.B) {
		run(b, func(tx metadata.Transaction, h metadata.FileHandle) error {
			f, err := tx.GetFile(ctx, h)
			if err != nil {
				return err
			}
			f.Size = 4096
			f.Mtime = time.Now()
			f.Ctime = f.Mtime
			return tx.PutFile(ctx, f)
		})
	})

	b.Run("applydatawrite", func(b *testing.B) {
		run(b, func(tx metadata.Transaction, h metadata.FileHandle) error {
			_, err := tx.(metadata.DataWriteApplier).ApplyDataWrite(ctx, h, 4096, time.Now(), false)
			return err
		})
	})
}
