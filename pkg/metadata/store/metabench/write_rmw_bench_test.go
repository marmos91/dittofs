package metabench

import (
	"context"
	"fmt"
	"math/rand"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// nFiles is the seeded population the writes scatter over.
const nFiles = 20000

// BenchmarkWriteRMW drives the data-write metadata op — the RMW a 4k WRITE
// triggers: GetFile then UpdateAttrs in one txn — against Badger with relaxed
// durability (SyncWrites=false), scattered over a populated file set, and
// reports IOPS. Profile it with:
//
//	DITTOFS_LOGGING_LEVEL=ERROR go test -run '^$' -bench WriteRMW \
//	  -benchtime 3s -cpuprofile /tmp/badger.prof ./pkg/metadata/store/metabench/
//	go tool pprof -top /tmp/badger.prof
func BenchmarkWriteRMW(b *testing.B) {
	s, err := badger.NewBadgerMetadataStoreWithDefaultsAndCaches(
		context.Background(), b.TempDir(), 0, 0, true /*relaxedDurability*/)
	if err != nil {
		b.Fatalf("badger open: %v", err)
	}
	benchmarkDataWriteRMW(b, s, "/hot")
}

// benchmarkDataWriteRMW seeds share with nFiles files and then drives the 4k
// WRITE metadata RMW — GetFile then UpdateAttrs in one transaction — scattered
// over them from GOMAXPROCS goroutines, reporting IOPS. It closes store.
//
// The seed is untimed but is NOT sized from b.N, so it does not grow with the
// timed region; it does re-run on each invocation the framework makes while
// growing b.N. Pass -benchtime=<N>x to pin the iteration count.
//
// Concurrency is the point, not incidental: a lever that coalesces concurrent
// commits has nothing to work with in a serial benchmark, so a cell run at
// -cpu=1 can only show such a lever's cost and never its benefit.
func benchmarkDataWriteRMW(b *testing.B, store metadata.Store, share string) {
	ctx := context.Background()
	b.Cleanup(func() { _ = store.Close() })

	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755})
	if err != nil {
		b.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		b.Fatalf("GetRootHandle: %v", err)
	}

	handles := make([]metadata.FileHandle, nFiles)
	for i := 0; i < nFiles; i++ {
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
		f := &metadata.File{
			ShareName: share, Path: fp, ID: id,
			FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000},
		}
		if err := store.UpdateAttrs(ctx, f); err != nil {
			b.Fatalf("UpdateAttrs seed: %v", err)
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
				return tx.UpdateAttrs(ctx, f)
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
