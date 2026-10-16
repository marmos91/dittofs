//go:build fdb

// Package fdbkvtest opens fdbkv stores for tests, each under a prefix of its
// own on a local cluster.
package fdbkvtest

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/marmos91/dittofs/pkg/metadata/kv/fdbkv"
)

var run atomic.Int64

// Open is a store under a prefix of its own on the cluster
// DITTOFS_FDB_CLUSTER_FILE names, cleared when tb ends: test/fdb/cluster.sh
// starts one and sets it. Without it the test is skipped.
func Open(tb testing.TB, opts fdbkv.Options) *fdbkv.Store {
	tb.Helper()
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		tb.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	opts.ClusterFile = cf
	opts.Prefix = fmt.Appendf(nil, "test/%d-%d/", time.Now().UnixNano(), run.Add(1))
	s, err := fdbkv.Open(opts)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
		clearPrefix(tb, opts)
	})
	return s
}

// clearPrefix removes everything under the test's prefix.
func clearPrefix(tb testing.TB, opts fdbkv.Options) {
	db, err := fdb.OpenDatabase(opts.ClusterFile)
	if err != nil {
		tb.Error(err)
		return
	}
	// The client's handle for this cluster file is shared; it stays open.
	end, _ := fdb.Strinc(opts.Prefix)
	if _, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(opts.Prefix), End: fdb.Key(end)})
		return nil, nil
	}); err != nil {
		tb.Error(err)
	}
}
