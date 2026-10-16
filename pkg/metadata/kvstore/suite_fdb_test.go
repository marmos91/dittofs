//go:build fdb

package kvstore_test

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

func fdbStore(opts fdbkv.Options) kvstoretest.Opener {
	return func(tb testing.TB) kv.KV { return fdbkvtest.Open(tb, opts) }
}

// The suite on FoundationDB, the replicated backend.
func TestStoreConformanceFDB(t *testing.T) {
	kvstoretest.Run(t, fdbStore(fdbkv.Options{}))
}

// The suite on FoundationDB under a clock that never moves.
func TestStoreConformanceFDBFrozenClock(t *testing.T) {
	frozen := time.Unix(1_800_000_000, 0)
	kvstoretest.Run(t, fdbStore(fdbkv.Options{Clock: func() time.Time { return frozen }}))
}

func FuzzNamespaceFDB(f *testing.F) { kvstoretest.FuzzNamespace(f, fdbStore(fdbkv.Options{})) }

// BenchmarkStoreFDB runs the store's benchmarks on FoundationDB.
func BenchmarkStoreFDB(b *testing.B) { kvstoretest.Bench(b, fdbStore(fdbkv.Options{})) }
