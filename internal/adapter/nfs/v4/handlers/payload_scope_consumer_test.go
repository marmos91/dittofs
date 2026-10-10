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
	"github.com/stretchr/testify/require"
)

type v4ScopePauseKey struct{}

type v4ScopePauseStore struct {
	metadata.Store
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
}

func (s *v4ScopePauseStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	s.pause(ctx)
	return s.Store.WithTransaction(ctx, fn)
}

func (s *v4ScopePauseStore) UpdateAttrs(ctx context.Context, file *metadata.File) error {
	s.pause(ctx)
	return s.Store.UpdateAttrs(ctx, file)
}

func (s *v4ScopePauseStore) SetManifest(ctx context.Context, file *metadata.File) error {
	s.pause(ctx)
	return s.Store.SetManifest(ctx, file)
}

func (s *v4ScopePauseStore) pause(ctx context.Context) {
	if ctx.Value(v4ScopePauseKey{}) != nil {
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.resume:
			case <-ctx.Done():
			}
		})
	}
}

// Size changes and hole punches must own admission while publishing metadata,
// including the gap before engine.Truncate or engine.PunchHole takes its scope.
func TestV4PayloadScopeCoversMetadataPublication(t *testing.T) {
	for _, operation := range []string{"SETATTR", "ALLOCATE", "DEALLOCATE"} {
		t.Run(operation, func(t *testing.T) {
			fx := newCloneRangeFixture(t)
			t.Cleanup(func() { _ = fx.blockStore.Close() })
			handle := fx.createRegularFile(t, fx.rootHandle, "scope.bin", 0o666, 1000, 1000)
			original := bytes.Repeat([]byte{0x39}, 8192)
			fx.writeContent(t, handle, original)
			file, err := fx.metaSvc.GetFile(t.Context(), handle)
			require.NoError(t, err)
			if operation == "DEALLOCATE" {
				// Publish a real manifest so DEALLOCATE exercises metadata
				// pruning as well as the journal's zero overwrite.
				require.NoError(t, fx.blockStore.DrainPayload(t.Context(), string(file.PayloadID)))
				file, err = fx.metaSvc.GetFile(t.Context(), handle)
				require.NoError(t, err)
				require.NotEmpty(t, file.Blocks)
			}
			gate := &v4ScopePauseStore{Store: fx.store, entered: make(chan struct{}), resume: make(chan struct{})}
			require.NoError(t, fx.metaSvc.RegisterStoreForShare(fx.shareName, gate))
			var once sync.Once
			resume := func() { once.Do(func() { close(gate.resume) }) }
			t.Cleanup(resume)
			ctx := newRealFSContext(1000, 1000)
			ctx.Context = context.WithValue(t.Context(), v4ScopePauseKey{}, true)
			ctx.CurrentFH = handle
			done := make(chan *types.CompoundResult, 1)
			go func() {
				switch operation {
				case "SETATTR":
					done <- fx.handler.handleSetAttr(ctx, bytes.NewReader(encodeSetAttrSizeArgs(t, anonStateid(), 4096)))
				case "ALLOCATE":
					done <- fx.handler.handleAllocate(ctx, encAllocArgs(anonStateid(), 8192, 4096))
				case "DEALLOCATE":
					done <- fx.handler.handleDeallocate(ctx, encAllocArgs(anonStateid(), 2048, 4096))
				}
			}()
			select {
			case <-gate.entered:
			case got := <-done:
				t.Fatalf("operation missed metadata publication: status=%d", got.Status)
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not reach metadata publication")
			}
			probe, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			err = fx.blockStore.WithPayloadScope(probe, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil })
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("replacement entered during %s metadata publication: %v", operation, err)
			}
			resume()
			select {
			case got := <-done:
				require.Equal(t, uint32(types.NFS4_OK), got.Status)
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not finish after metadata resumed")
			}
			want := append([]byte(nil), original...)
			switch operation {
			case "SETATTR":
				want = want[:4096]
			case "ALLOCATE":
				want = append(want, make([]byte, 4096)...)
			case "DEALLOCATE":
				clear(want[2048:6144])
			}
			after, err := fx.metaSvc.GetFile(t.Context(), handle)
			require.NoError(t, err)
			require.Equal(t, uint64(len(want)), after.Size)
			got := make([]byte, len(want))
			_, err = fx.blockStore.ReadAt(t.Context(), string(file.PayloadID), got, 0)
			require.NoError(t, err)
			require.Equal(t, want, got)
			released, releaseCancel := context.WithTimeout(t.Context(), time.Second)
			defer releaseCancel()
			require.NoError(t, fx.blockStore.WithPayloadScope(released, []string{string(file.PayloadID)}, true, func(context.Context) error { return nil }))
		})
	}
}
