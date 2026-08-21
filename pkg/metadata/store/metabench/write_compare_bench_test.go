package metabench

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
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// caps is the minimal filesystem-capability set every backend needs to open.
func caps() metadata.FilesystemCapabilities {
	return metadata.FilesystemCapabilities{
		MaxReadSize: 1 << 20, PreferredReadSize: 1 << 20,
		MaxWriteSize: 1 << 20, PreferredWriteSize: 1 << 20,
		MaxFileSize: 1<<63 - 1, MaxFilenameLen: 255, MaxPathLen: 4096,
		CasePreserving: true, TimestampResolution: 1,
	}
}

// backend names a metadata store the write-path comparison drives. Durability
// is equalized to fsync-free across all three (badger SyncWrites=false, sqlite
// WAL+synchronous=NORMAL, and — for postgres — synchronous_commit=off via the
// relaxed path) so any gap that remains is access-pattern/round-trip/CPU, i.e.
// design, not the disk's fsync cost.
type backend struct {
	name string
	open func(b *testing.B) metadata.Store
}

func backends() []backend {
	return []backend{
		{"badger", func(b *testing.B) metadata.Store {
			s, err := badger.NewBadgerMetadataStoreWithDefaultsAndCaches(
				context.Background(), b.TempDir(), 0, 0, true /*relaxedDurability*/)
			if err != nil {
				b.Fatalf("badger open: %v", err)
			}
			return s
		}},
		{"sqlite", func(b *testing.B) metadata.Store {
			s, err := sqlite.NewSQLiteMetadataStore(context.Background(),
				&sqlite.SQLiteMetadataStoreConfig{Path: filepath.Join(b.TempDir(), "m.db"), AutoMigrate: true},
				caps())
			if err != nil {
				b.Fatalf("sqlite open: %v", err)
			}
			return s
		}},
	}
}

// BenchmarkWriteCompare drives the identical data-write metadata op — the RMW a
// 4k WRITE triggers: GetFile (SELECT) then UpdateAttrs (row UPDATE) in one txn —
// against each backend, scattered over a populated file set, and reports IOPS.
// Run one backend at a time with its own profile to compare flamegraphs:
//
//	DITTOFS_LOGGING_LEVEL=ERROR go test -run '^$' -bench 'WriteCompare/badger' \
//	  -benchtime 3s -cpuprofile /tmp/badger.prof ./pkg/metadata/store/metabench/
//	go tool pprof -top /tmp/badger.prof
func BenchmarkWriteCompare(b *testing.B) {
	const nFiles = 20000
	ctx := context.Background()
	for _, be := range backends() {
		b.Run(be.name, func(b *testing.B) {
			store := be.open(b)
			b.Cleanup(func() { _ = store.Close() })

			const share = "/hot"
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
		})
	}
}
