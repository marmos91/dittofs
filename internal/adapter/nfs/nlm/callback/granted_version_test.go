package callback

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestProcessGrantedCallback_ResolvesWithNegotiatedVersion guards against
// regressing the NLM_GRANTED portmap lookup to a hardcoded version. A v1/v3
// client registers its lockd under (100021, v1/v3); querying the client's
// portmapper for the wrong version returns port 0 and the blocking-lock grant
// is silently lost. The version the resolver receives must be the one the
// client negotiated (waiter.CallbackVers).
func TestProcessGrantedCallback_ResolvesWithNegotiatedVersion(t *testing.T) {
	for _, vers := range []uint32{types.NLMVersion1, types.NLMVersion3, types.NLMVersion4} {
		t.Run("v"+string(rune('0'+vers)), func(t *testing.T) {
			var gotVers uint32
			restore := SetCallbackAddrResolver(func(_ context.Context, _ string, v uint32) (string, error) {
				gotVers = v
				// Return an error so we short-circuit before dialing; the
				// version capture above is all this test needs.
				return "", errors.New("resolve disabled in test")
			})
			defer restore()

			lm := lock.NewManager()
			waiter := &blocking.Waiter{
				CallbackHost: "192.0.2.10",
				CallbackProg: types.ProgramNLM,
				CallbackVers: vers,
				Lock: &lock.UnifiedLock{
					Owner:      lock.LockOwner{OwnerID: "owner"},
					FileHandle: lock.FileHandle("fh"),
				},
			}

			if ok := ProcessGrantedCallback(context.Background(), waiter, lm); ok {
				t.Fatalf("expected callback to fail (resolver errored), got success")
			}
			if gotVers != vers {
				t.Fatalf("resolver received version %d, want %d (negotiated)", gotVers, vers)
			}
		})
	}
}
