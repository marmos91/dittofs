package handlers_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

type v3ScopePauseKey struct{}

type v3ScopePauseStore struct {
	metadata.Store
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
}

func (s *v3ScopePauseStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	if ctx.Value(v3ScopePauseKey{}) != nil {
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.resume:
			case <-ctx.Done():
			}
		})
	}
	return s.Store.WithTransaction(ctx, fn)
}

// These operations pair metadata with journal mutations. In particular, WRITE
// must still exclude replacement after the engine's WriteAt scope has ended.
func TestV3PayloadScopeCoversMetadataTransaction(t *testing.T) {
	for _, operation := range []string{"WRITE", "CREATE_truncate"} {
		t.Run(operation, func(t *testing.T) {
			fx := handlertesting.NewHandlerFixture(t)
			fx.MetadataService.SetDeferredCommit(false)
			original := bytes.Repeat([]byte{0x31}, 8192)
			handle := fx.CreateFile("scope.bin", original)
			file, err := fx.MetadataService.GetFile(t.Context(), handle)
			require.NoError(t, err)
			gate := &v3ScopePauseStore{Store: fx.MetaStore, entered: make(chan struct{}), resume: make(chan struct{})}
			require.NoError(t, fx.MetadataService.RegisterStoreForShare(fx.ShareName, gate))
			var once sync.Once
			resume := func() { once.Do(func() { close(gate.resume) }) }
			t.Cleanup(resume)
			ctx := fx.ContextWithUID(0, 0)
			ctx.Context = context.WithValue(t.Context(), v3ScopePauseKey{}, true)
			type result struct {
				status uint32
				err    error
			}
			done := make(chan result, 1)
			tail := bytes.Repeat([]byte{0x72}, 4096)
			go func() {
				if operation == "WRITE" {
					resp, err := fx.Handler.Write(ctx, &handlers.WriteRequest{Handle: handle, Offset: 8192, Count: uint32(len(tail)), Data: tail})
					done <- result{resp.Status, err}
					return
				}
				size := uint64(4096)
				resp, err := fx.Handler.Create(ctx, &handlers.CreateRequest{DirHandle: fx.RootHandle, Filename: "scope.bin", Mode: 0, Attr: &metadata.SetAttrs{Size: &size}})
				done <- result{resp.Status, err}
			}()
			select {
			case <-gate.entered:
			case got := <-done:
				t.Fatalf("operation missed metadata transaction: %+v", got)
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not reach metadata transaction")
			}
			if operation == "WRITE" {
				got := make([]byte, len(tail))
				_, err := fx.BlockStore.ReadAt(t.Context(), string(file.PayloadID), got, 8192)
				require.NoError(t, err)
				require.Equal(t, tail, got, "the engine write must have finished before probing the outer scope")
			}
			probe, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			err = fx.BlockStore.WithPayloadScope(probe, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil })
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("replacement entered during %s metadata transaction: %v", operation, err)
			}
			resume()
			select {
			case got := <-done:
				require.NoError(t, got.err)
				require.Equal(t, uint32(types.NFS3OK), got.status)
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not finish after metadata resumed")
			}
			want := original[:4096]
			if operation == "WRITE" {
				want = append(append([]byte(nil), original...), tail...)
			}
			read, err := fx.Handler.Read(fx.ContextWithUID(0, 0), &handlers.ReadRequest{Handle: handle, Count: uint32(len(original) + len(tail))})
			require.NoError(t, err)
			require.Equal(t, uint32(types.NFS3OK), read.Status)
			require.Equal(t, want, read.Data)
			after, err := fx.MetadataService.GetFile(t.Context(), handle)
			require.NoError(t, err)
			require.Equal(t, uint64(len(want)), after.Size)
			released, releaseCancel := context.WithTimeout(t.Context(), time.Second)
			defer releaseCancel()
			require.NoError(t, fx.BlockStore.WithPayloadScope(released, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil }))
		})
	}
}

type v3WarmPauseStore struct {
	metadata.Store
	reads   int
	entered chan *metadata.File
	resume  chan struct{}
}

func (s *v3WarmPauseStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	file, err := s.Store.GetFile(ctx, handle)
	if err == nil && file.Type == metadata.FileTypeRegular && ctx.Value(v3ScopePauseKey{}) != nil {
		s.reads++
		// CREATE already knows the new payload's identity. Its first regular
		// file read obtains the fresh snapshot about to enter the write cache.
		if s.reads == 1 {
			s.entered <- file
			select {
			case <-s.resume:
			case <-ctx.Done():
			}
		}
	}
	return file, err
}

func TestV3PayloadScopeCoversCreateCachePrewarm(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	gate := &v3WarmPauseStore{Store: fx.MetaStore, entered: make(chan *metadata.File, 1), resume: make(chan struct{})}
	require.NoError(t, fx.MetadataService.RegisterStoreForShare(fx.ShareName, gate))
	var once sync.Once
	resume := func() { once.Do(func() { close(gate.resume) }) }
	t.Cleanup(resume)
	ctx := fx.ContextWithUID(0, 0)
	ctx.Context = context.WithValue(t.Context(), v3ScopePauseKey{}, true)
	type result struct {
		response *handlers.CreateResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := fx.Handler.Create(ctx, &handlers.CreateRequest{DirHandle: fx.RootHandle, Filename: "prewarm.bin", Mode: 0})
		done <- result{resp, err}
	}()
	var file *metadata.File
	select {
	case file = <-gate.entered:
	case got := <-done:
		t.Fatalf("CREATE missed its cache-prewarm read: %+v", got)
	case <-time.After(5 * time.Second):
		t.Fatal("CREATE did not reach cache-prewarm read")
	}
	probe, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err := fx.BlockStore.WithPayloadScope(probe, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil })
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("replacement entered before CREATE published its write-cache snapshot: %v", err)
	}
	resume()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, uint32(types.NFS3OK), got.response.Status)
		handle, err := metadata.EncodeFileHandle(file)
		require.NoError(t, err)
		require.Equal(t, []byte(handle), got.response.FileHandle)
		after, err := fx.MetadataService.GetFile(t.Context(), handle)
		require.NoError(t, err)
		require.Zero(t, after.Size)
		require.Equal(t, file.PayloadID, after.PayloadID)
	case <-time.After(5 * time.Second):
		t.Fatal("CREATE did not finish after prewarming resumed")
	}
}
