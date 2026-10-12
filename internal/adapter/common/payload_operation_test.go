package common

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// A read or write queued behind an exclusive payload scope, the scope a CLONE
// holds across its source's upload, must give up at the request deadline
// instead of waiting for as long as the CLONE holds the scope.
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
