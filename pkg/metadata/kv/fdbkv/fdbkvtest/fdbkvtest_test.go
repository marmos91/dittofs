//go:build linux && cgo

package fdbkvtest

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
)

// Two stores Open makes see none of each other's keys, and a store's keys are
// gone once its test ends, so tests sharing one cluster never meet.
func TestOpenIsolatesAndClears(t *testing.T) {
	if os.Getenv("DITTOFS_FDB_CLUSTER_FILE") == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	ctx := context.Background()
	pa, pb := Prefix(), Prefix()
	stored := func(prefix []byte) int {
		db, err := fdb.OpenDatabase(os.Getenv("DITTOFS_FDB_CLUSTER_FILE"))
		if err != nil {
			t.Fatal(err)
		}
		end, _ := fdb.Strinc(prefix)
		kvs, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			return tr.GetRange(fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(end)}, fdb.RangeOptions{}).GetSliceWithError()
		})
		if err != nil {
			t.Fatal(err)
		}
		return len(kvs.([]fdb.KeyValue))
	}
	t.Run("stores", func(t *testing.T) {
		a, b := Open(t, fdbkv.Options{Prefix: pa}), Open(t, fdbkv.Options{Prefix: pb})
		if err := a.Update(ctx, func(tx kv.Txn) error { return tx.Set([]byte("k"), []byte("a")) }); err != nil {
			t.Fatal(err)
		}
		if err := b.View(ctx, func(r kv.Reader) error {
			_, err := r.Get([]byte("k"))
			return err
		}); !errors.Is(err, kv.ErrNotFound) {
			t.Errorf("one store read another's key: %v", err)
		}
		if stored(pa) == 0 {
			t.Fatal("the store wrote nothing under its prefix")
		}
	})
	if n := stored(pa) + stored(pb); n != 0 {
		t.Errorf("%d keys left under the stores' prefixes after their test", n)
	}
}
