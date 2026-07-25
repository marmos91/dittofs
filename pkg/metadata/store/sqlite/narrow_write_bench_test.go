package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// The hot data-write only needs to bump size + timestamps. This is that write as
// a single narrow statement against the real inodes table — no prior SELECT, no
// 20-column rewrite, no explicit BEGIN/COMMIT (autocommit = one implicit txn).
const narrowUpdate = `UPDATE inodes SET size = ?1, mtime = ?2, ctime = ?2 WHERE id = ?3 AND share_name = ?4`

// seedFiles builds a populated inode table and returns the per-file ids.
func seedFiles(b *testing.B, store metadata.Store, share string, n int) []string {
	b.Helper()
	ctx := context.Background()
	root, err := store.CreateRootDirectory(ctx, share, &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755})
	if err != nil {
		b.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		b.Fatalf("GetRootHandle: %v", err)
	}
	ids := make([]string, n)
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
		f := &metadata.File{ShareName: share, Path: fp, ID: id,
			FileAttr: metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000}}
		if err := store.PutFile(ctx, f); err != nil {
			b.Fatalf("PutFile seed: %v", err)
		}
		if err := store.SetParent(ctx, h, rootHandle); err != nil {
			b.Fatalf("SetParent: %v", err)
		}
		if err := store.SetChild(ctx, rootHandle, name, h); err != nil {
			b.Fatalf("SetChild: %v", err)
		}
		ids[i] = id.String()
	}
	return ids
}

// BenchmarkNarrowWrite isolates the two SQL write-path levers against the same
// populated table the GetFile+PutFile path runs at ~5k IOPS:
//
//	raw      — narrow single UPDATE, re-parsed each op (isolates: drop SELECT +
//	           20-col rewrite + explicit txn, but keep per-op SQL compilation)
//	prepared — same UPDATE via a cached *sql.Stmt (adds: kill per-op compilation)
//
// The delta raw→prepared is the prepared-statement win; current→raw is the
// narrow-single-statement win.
func BenchmarkNarrowWrite(b *testing.B) {
	const share = "/hot"
	const nFiles = 20000
	newStore := func(b *testing.B) (metadata.Store, *sql.DB) {
		s, err := sqlite.NewSQLiteMetadataStore(context.Background(),
			&sqlite.SQLiteMetadataStoreConfig{Path: b.TempDir() + "/m.db", AutoMigrate: true}, sqliteTestCapabilities())
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		return s, s.DBForBench()
	}

	b.Run("raw", func(b *testing.B) {
		store, db := newStore(b)
		b.Cleanup(func() { _ = store.Close() })
		ids := seedFiles(b, store, share, nFiles)
		ctx := context.Background()
		var ops int64
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			rng := rand.New(rand.NewSource(rand.Int63()))
			for pb.Next() {
				if _, err := db.ExecContext(ctx, narrowUpdate, int64(4096), int64(1_700_000_000), ids[rng.Intn(nFiles)], share); err != nil {
					b.Fatalf("exec: %v", err)
				}
				atomic.AddInt64(&ops, 1)
			}
		})
		b.StopTimer()
		if s := b.Elapsed().Seconds(); s > 0 {
			b.ReportMetric(float64(atomic.LoadInt64(&ops))/s, "iops")
		}
	})

	b.Run("prepared", func(b *testing.B) {
		store, db := newStore(b)
		b.Cleanup(func() { _ = store.Close() })
		ids := seedFiles(b, store, share, nFiles)
		ctx := context.Background()
		stmt, err := db.PrepareContext(ctx, narrowUpdate)
		if err != nil {
			b.Fatalf("prepare: %v", err)
		}
		defer func() { _ = stmt.Close() }()
		var ops int64
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			rng := rand.New(rand.NewSource(rand.Int63()))
			for pb.Next() {
				if _, err := stmt.ExecContext(ctx, int64(4096), int64(1_700_000_000), ids[rng.Intn(nFiles)], share); err != nil {
					b.Fatalf("exec: %v", err)
				}
				atomic.AddInt64(&ops, 1)
			}
		})
		b.StopTimer()
		if s := b.Elapsed().Seconds(); s > 0 {
			b.ReportMetric(float64(atomic.LoadInt64(&ops))/s, "iops")
		}
	})
}
