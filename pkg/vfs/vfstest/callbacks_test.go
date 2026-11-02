package vfstest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
	"github.com/marmos91/dittofs/pkg/vfs/vfstest"
)

func TestRecordingCallbacksAnswerAsSet(t *testing.T) {
	ctx := context.Background()
	failed := errors.New("no answer")
	cb := &vfstest.RecordingCallbacks{
		Answer: func(_ context.Context, c vfstest.Callback) error {
			if c.Method == "RecallAny" {
				return failed
			}
			return nil
		},
		Refuse: true,
	}
	w := vfs.LockWaiter{Wait: vfs.WaitCallback}
	if refused, err := cb.LockWaitOver(ctx, w, true); !refused || err != nil {
		t.Fatalf("LockWaitOver = %v, %v; want refused, nil", refused, err)
	}
	if err := cb.RecallAny(ctx, 2); err != failed {
		t.Fatalf("RecallAny = %v, want %v", err, failed)
	}
	want := []vfstest.Callback{{"LockWaitOver", []any{w, true}}, {"RecallAny", []any{2}}}
	if got := cb.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
}
