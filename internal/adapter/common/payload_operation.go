package common

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// WithFilePayloadScope keeps a file's content operation and metadata update in
// one shared payload scope. An immutable routing hint locates the payload; the callback
// must validate its operation against fresh metadata using the supplied context.
func WithFilePayloadScope[T any](authCtx *metadata.AuthContext, metaSvc *metadata.Service, blockStore *engine.Store, handle metadata.FileHandle, fn func(*metadata.AuthContext) (T, error)) (T, error) {
	var result T
	scopeCtx, cancel := WithRequestDeadline(authCtx.Context)
	defer cancel()
	payloadID, err := metaSvc.PayloadIDForIO(scopeCtx, handle)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, normalizeBlockStoreError(err)
		}
		return result, err
	}
	// decision: admission and the callback share one request budget. Remote
	// draining happens outside replacement admission, but a local byte copy or
	// a slow metadata commit can still delay shared entry. Keep that wait bounded
	// and report exhaustion as an I/O error the client can retry.
	entered := false
	err = blockStore.WithPayloadScope(scopeCtx, []string{string(payloadID)}, false, func(ctx context.Context) error {
		entered = true
		scoped := *authCtx
		scoped.Context = ctx
		var err error
		result, err = fn(&scoped)
		return err
	})
	if err != nil && !entered {
		return result, normalizeBlockStoreError(err)
	}
	return result, err
}
