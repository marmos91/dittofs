//go:build linux && cgo

package fdbkv

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// A commit that landed but answered "outcome unknown" is not applied again:
// the retry finds its mark and reports success without running fn.
func TestUnknownCommitOutcomeNotReapplied(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	prefix := fmt.Appendf(nil, "test/%d-unknown/", time.Now().UnixNano())
	s, err := Open(Options{ClusterFile: cf, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		end, _ := fdb.Strinc(prefix)
		_, _ = s.db.Transact(func(tr fdb.Transaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(end)})
			return nil, nil
		})
	})

	once := true
	afterCommit = func(err error) error {
		if err == nil && once {
			once = false
			return fdb.Error{Code: codeCommitUnknown}
		}
		return err
	}
	t.Cleanup(func() { afterCommit = func(err error) error { return err } })

	ctx := context.Background()
	runs := 0
	if err := s.Update(ctx, func(tx kv.Txn) error {
		runs++
		n := 0
		if v, err := tx.Get([]byte("count")); err == nil {
			n = int(v[0])
		}
		return tx.Set([]byte("count"), []byte{byte(n + 1)})
	}); err != nil {
		t.Fatalf("Update after a landed commit's unknown outcome: %v", err)
	}
	if err := s.View(ctx, func(r kv.Reader) error {
		v, err := r.Get([]byte("count"))
		if err != nil {
			return err
		}
		if v[0] != 1 || runs != 1 {
			return fmt.Errorf("count %d after %d runs of fn, want 1 after 1", v[0], runs)
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
}

// The sweep clears the marks older than their retention and keeps the newer
// ones, which a commit still in doubt may need.
func TestSweepClearsOnlyOldMarks(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	prefix := fmt.Appendf(nil, "test/%d-sweep/", time.Now().UnixNano())
	s, err := Open(Options{ClusterFile: cf, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		end, _ := fdb.Strinc(prefix)
		_, _ = s.db.Transact(func(tr fdb.Transaction) (any, error) {
			tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(end)})
			return nil, nil
		})
	})
	mark := func(at time.Time) []byte {
		return append(binary.BigEndian.AppendUint64(append([]byte{}, keys.Commit...), uint64(at.UnixNano())), 'x')
	}
	old, fresh := mark(time.Now().Add(-markRetention-time.Minute)), mark(time.Now())
	if _, err := s.db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.Set(s.key(old), nil)
		tr.Set(s.key(fresh), nil)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepMarks(); err != nil {
		t.Fatal(err)
	}
	present := func(k []byte) bool {
		v, err := s.db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) { return tr.Get(s.key(k)).Get() })
		if err != nil {
			t.Fatal(err)
		}
		return v.([]byte) != nil
	}
	if present(old) || !present(fresh) {
		t.Errorf("after a sweep: old mark present %v, fresh mark present %v; want false, true", present(old), present(fresh))
	}
}
