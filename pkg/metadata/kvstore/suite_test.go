package kvstore_test

import (
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

func badger(opts badgerkv.Options) kvstoretest.Opener {
	return func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(opts)
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { db.Close() })
		return db
	}
}

func TestStoreConformance(t *testing.T) {
	kvstoretest.Run(t, badger(badgerkv.Options{}))
}

// The suite on disk, every commit synced: the backend as it ships.
func TestStoreConformanceOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("syncs every commit")
	}
	kvstoretest.Run(t, func(tb testing.TB) kv.KV {
		return badger(badgerkv.Options{Dir: tb.TempDir()})(tb)
	})
}

// BenchmarkStore runs the store's benchmarks in memory, and on disk under
// KV_BENCH_DIR when it is set: point it at a real device, since a tmpfs
// syncs in no time and hides what a commit costs.
func BenchmarkStore(b *testing.B) {
	b.Run("memory", func(b *testing.B) { kvstoretest.Bench(b, badger(badgerkv.Options{})) })
	if dir := os.Getenv("KV_BENCH_DIR"); dir != "" {
		b.Run("disk", func(b *testing.B) {
			kvstoretest.Bench(b, func(tb testing.TB) kv.KV {
				d, err := os.MkdirTemp(dir, "kvstore-bench-")
				if err != nil {
					tb.Fatal(err)
				}
				tb.Cleanup(func() { os.RemoveAll(d) })
				return badger(badgerkv.Options{Dir: d})(tb)
			})
		})
	}
}
