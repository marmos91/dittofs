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

func TestWithFilePayloadScopeCancelledRoutingMapsToIO(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := WithFilePayloadScope(&metadata.AuthContext{Context: ctx}, metadata.New(), newTestEngine(t), nil, func(*metadata.AuthContext) (struct{}, error) {
		t.Fatal("cancelled routing entered the payload operation")
		return struct{}{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation cause: %v", err)
	}
	if got := nfs4types.StatusForErr(err); got != nfs4types.NFS4ERR_IO {
		t.Fatalf("cancelled routing status = %d, want IO", got)
	}
}

// A read or write queued behind an exclusive payload scope must give up at the
// request deadline instead of waiting for a local copy or metadata commit.
func TestWithFilePayloadScopeWaitEndsAtRequestDeadline(t *testing.T) {
	shortRequestDeadline(t, 200*time.Millisecond)

	ms := badgertest.NewInMemory(t)
	bs := newCloneTestEngineWithMS(t, &fakeCoordinator{}, ms)
	const payloadID = metadata.PayloadID("scope-wait-deadline")
	handle := putTestFile(t, ms, "/scope-wait", payloadID, nil, 0)
	metaSvc := metadata.New()
	if err := metaSvc.RegisterStoreForShare("test-share", ms); err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		_ = bs.WithPayloadScope(context.Background(), []string{string(payloadID)}, true, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("exclusive scope was not taken")
	}

	type outcome struct {
		entered bool
		err     error
		took    time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		entered := false
		// A request context without a deadline, as the protocol handlers pass.
		authCtx := &metadata.AuthContext{Context: context.Background()}
		_, err := WithFilePayloadScope(authCtx, metaSvc, bs, handle, func(*metadata.AuthContext) (struct{}, error) {
			entered = true
			return struct{}{}, nil
		})
		done <- outcome{entered: entered, err: err, took: time.Since(start)}
	}()

	select {
	case got := <-done:
		if got.entered {
			t.Fatal("the callback ran while another holder had the scope exclusively")
		}
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("wait ended with %v, want the request deadline", got.err)
		}
		if got.took < 150*time.Millisecond {
			t.Fatalf("wait ended after %v, before the %v deadline", got.took, 200*time.Millisecond)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a scope wait behind an exclusive holder did not end at the request deadline")
	}
}
