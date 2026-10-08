package handlers

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

type writeCommitPauseKey struct{}

type writeCommitPauseStore struct {
	metadata.Store
	entered chan struct{}
	resume  chan struct{}
}

func (s *writeCommitPauseStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	if ctx.Value(writeCommitPauseKey{}) != nil {
		close(s.entered)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.WithTransaction(ctx, fn)
}

// The handler must retain admission after the journal write returns, until
// CommitWrite publishes the size. Guarding only engine.WriteAt misses this gap.
func TestWritePayloadScopeIncludesMetadataCommit(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	handle := fx.createRegularFile(t, fx.rootHandle, "scoped-write", 0o666, 1000, 1000)
	file, err := fx.metaSvc.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatal(err)
	}
	gate := &writeCommitPauseStore{Store: fx.store, entered: make(chan struct{}), resume: make(chan struct{})}
	if err := fx.metaSvc.RegisterStoreForShare(fx.shareName, gate); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	resume := func() { once.Do(func() { close(gate.resume) }) }
	t.Cleanup(resume)
	ctx := newRealFSContext(1000, 1000)
	ctx.Context = context.WithValue(t.Context(), writeCommitPauseKey{}, true)
	ctx.CurrentFH = handle
	data := bytes.Repeat([]byte{0x37}, 4096)
	done := make(chan *types.CompoundResult, 1)
	go func() {
		done <- fx.handler.handleWrite(ctx, bytes.NewReader(encodeWriteArgs(anonStateid(), 4096, types.UNSTABLE4, data)))
	}()
	select {
	case <-gate.entered:
	case result := <-done:
		t.Fatalf("WRITE returned before its metadata commit: status=%d", result.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("WRITE did not reach its metadata commit")
	}

	probeCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = fx.blockStore.WithPayloadScope(probeCtx, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replacement entered before WRITE metadata commit: got %v, want deadline exceeded", err)
	}
	resume()
	select {
	case result := <-done:
		if result.Status != types.NFS4_OK {
			t.Fatalf("WRITE status = %d", result.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WRITE did not finish after metadata was released")
	}
	after, err := fx.metaSvc.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(data))
	if _, err := fx.blockStore.ReadAt(t.Context(), string(file.PayloadID), got, 4096); err != nil {
		t.Fatal(err)
	}
	if after.Size != 8192 || !bytes.Equal(got, data) {
		t.Fatalf("WRITE lost its committed size or tail: size=%d, tail matches=%v", after.Size, bytes.Equal(got, data))
	}
}
