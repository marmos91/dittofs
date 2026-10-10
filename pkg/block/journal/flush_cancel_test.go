package journal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFlushCancelsWhileAnotherFileOnShardUploads(t *testing.T) {
	s, _ := seamStore(t, Config{ShardCount: 1})
	ctx := context.Background()
	for _, id := range []FileID{"uploading", "waiting"} {
		if err := s.WriteAt(ctx, id, 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Flush(ctx, "uploading", FlushOptions{Force: true}, func(_ context.Context, run Run) ([]Extent, error) {
			close(entered)
			<-resume
			return []Extent{run.Extent}, nil
		})
	}()
	defer func() { close(resume); <-done }()
	<-entered
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- s.Flush(waitCtx, "waiting", FlushOptions{Force: true}, func(context.Context, Run) ([]Extent, error) {
			t.Error("cancelled flush reached the sink")
			return nil, nil
		})
	}()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("flush error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush ignored its deadline while waiting for the shard")
	}
	if dirty, err := s.HasDirty(ctx, "waiting"); err != nil || !dirty {
		t.Fatalf("waiting file lost dirty state: dirty=%v err=%v", dirty, err)
	}
}
