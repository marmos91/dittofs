package metadata_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

type parentFlushOperationKey struct{}

func TestDirectoryMutationFinishesTimeFlushBeforePeerTransaction(t *testing.T) {
	for _, operation := range []string{"rename", "rmdir"} {
		t.Run(operation, func(t *testing.T) {
			f := newLifecycleFixture(t)
			from := f.create(t, f.root, "from", true)
			to := f.create(t, f.root, "to", true)
			first := f.create(t, from, "first", true)
			peer := f.create(t, from, "peer", true)
			metadata.ForceDirectoryTimeFlushForTest(f.svc, from)
			store, err := f.svc.GetStoreForShare("/lifecycle")
			require.NoError(t, err)
			wrapped := store.(*lifecycleStore)
			flushEntered, flushFinished := make(chan struct{}), make(chan struct{})
			peerEntered := make(chan struct{})
			resume := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(resume) }) }
			t.Cleanup(release)
			wrapped.beforeDurable = func(ctx context.Context) {
				if ctx.Value(parentFlushOperationKey{}) == "first" {
					close(flushEntered)
					<-resume
				}
			}
			wrapped.afterDurable = func(ctx context.Context, err error) {
				if ctx.Value(parentFlushOperationKey{}) == "first" {
					if err != nil {
						t.Errorf("timestamp flush failed: %v", err)
					}
					close(flushFinished)
				}
			}
			wrapped.beforeRelaxed = func(ctx context.Context) {
				if ctx.Value(parentFlushOperationKey{}) == "peer" {
					close(peerEntered)
					select {
					case <-flushFinished:
					default:
						t.Error("peer directory mutation entered its transaction before the first timestamp flush completed")
					}
				}
			}
			firstAuth, peerAuth := *f.auth, *f.auth
			firstAuth.Context = context.WithValue(f.auth.Context, parentFlushOperationKey{}, "first")
			peerAuth.Context = context.WithValue(f.auth.Context, parentFlushOperationKey{}, "peer")
			firstDone, peerDone := make(chan error, 1), make(chan error, 1)
			go func() {
				var err error
				if operation == "rename" {
					_, _, err = f.svc.Move(&firstAuth, from, "first", to, "first")
				} else {
					_, err = f.svc.RemoveDirectory(&firstAuth, from, "first")
				}
				firstDone <- err
			}()
			select {
			case <-flushEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("first operation never reached its timestamp flush")
			}
			go func() {
				_, _, err := f.svc.Move(&peerAuth, from, "peer", to, "peer")
				peerDone <- err
			}()
			select {
			case <-peerEntered:
				t.Error("peer transaction overtook the paused timestamp flush")
			case <-time.After(100 * time.Millisecond):
				// The peer must wait until the actual durable flush has finished.
			}
			release()
			require.NoError(t, lifecycleResult(t, firstDone))
			require.NoError(t, lifecycleResult(t, peerDone))
			f.assertChild(t, to, "peer", peer)
			wantToLinks := uint32(3)
			if operation == "rename" {
				f.assertChild(t, to, "first", first)
				wantToLinks++
			} else {
				_, err = f.store.GetChild(f.auth.Context, from, "first")
				require.True(t, metadata.IsNotFoundError(err))
			}
			fromLinks, err := f.store.GetLinkCount(f.auth.Context, from)
			require.NoError(t, err)
			toLinks, err := f.store.GetLinkCount(f.auth.Context, to)
			require.NoError(t, err)
			require.Equal(t, uint32(2), fromLinks)
			require.Equal(t, wantToLinks, toLinks)
		})
	}
}
