package common

import (
	"context"
	"errors"
	"testing"
	"time"

	nfs4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

func TestCloneWholeFileAddsRequestDeadline(t *testing.T) {
	ms := badgertest.NewInMemory(t)
	bs := newCloneTestEngineWithMS(t, &fakeCoordinator{}, ms)
	src := putTestFile(t, ms, "/src", "src", nil, 0)
	dst := putTestFile(t, ms, "/dst", "dst", nil, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	shortRequestDeadline(t, 100*time.Millisecond)
	err := bs.WithPayloadScope(ctx, []string{"dst"}, false, func(context.Context) error {
		done := make(chan error, 1)
		go func() { done <- CloneWholeFile(ctx, bs, ms, nil, src, dst, "dst", 0, nil) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("clone without an incoming deadline: %v", err)
			}
			if status := nfs4types.StatusForErr(err); status != nfs4types.NFS4ERR_IO {
				t.Fatalf("clone timeout maps to NFS status %d, want NFS4ERR_IO", status)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("clone without an incoming deadline waited indefinitely")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCloneWholeFilePreservesRangeErrorStatus(t *testing.T) {
	ms := badgertest.NewInMemory(t)
	bs := newCloneTestEngineWithMS(t, &fakeCoordinator{}, ms)
	src := putTestFile(t, ms, "/src", "src", nil, 0)
	dst := putTestFile(t, ms, "/dst", "dst", nil, 4096)
	err := CloneWholeFile(context.Background(), bs, ms, nil, src, dst, "dst", 0, nil)
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrNotSupported {
		t.Fatalf("clone replaced its metadata range error: %v", err)
	}
	if status := nfs4types.StatusForErr(err); status != nfs4types.NFS4ERR_NOTSUPP {
		t.Fatalf("clone range error maps to NFS status %d, want NFS4ERR_NOTSUPP", status)
	}
}
