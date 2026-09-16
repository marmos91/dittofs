package nfs

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/stretchr/testify/require"
)

// graceFileChecker is a lockAccessChecker stub that authorizes every handle, so
// the NLM lock path proceeds to the grace gate.
type graceFileChecker struct{}

func (graceFileChecker) CheckLockAccess(_ *metadata.AuthContext, _ []byte) error {
	return nil
}

// newGraceNLMService builds an nlmService backed by a grace-aware lock manager
// already in the grace period, with the given expected reclaim clients.
func newGraceNLMService(expectedClients []string) (*nlmService, *lock.Manager) {
	gpm := lock.NewGracePeriodManager(time.Hour, nil)
	lm := lock.NewManagerWithGracePeriod(gpm)
	lm.EnterGracePeriod(expectedClients)
	return newNLMService(lm, graceFileChecker{}), lm
}

// TestGracePeriod_NLMLockDenied asserts a non-reclaim NLM lock attempted during
// the grace window is rejected with ErrGracePeriod (which the handler maps to
// NLM4_DENIED_GRACE_PERIOD), not granted.
func TestGracePeriod_NLMLockDenied(t *testing.T) {
	svc, _ := newGraceNLMService([]string{"client-1"})

	owner := lock.LockOwner{OwnerID: "nlm:client-1", ClientID: "client-1", ShareName: "share-a"}
	_, err := svc.LockFileNLM(context.Background(), testAuthCtx(), []byte("share-a:file-1"), owner, 0, 100, true, false)

	require.Error(t, err, "new lock during grace must be denied")
	require.True(t, errors.IsGracePeriodError(err),
		"denial must be ErrGracePeriod so the handler maps it to NLM4_DENIED_GRACE_PERIOD, got %v", err)
}

// TestGracePeriod_NLMReclaimAllowed asserts a reclaim NLM lock is granted during
// the grace window and records the client so grace can exit early.
func TestGracePeriod_NLMReclaimAllowed(t *testing.T) {
	svc, lm := newGraceNLMService([]string{"client-1"})

	owner := lock.LockOwner{OwnerID: "nlm:client-1", ClientID: "client-1", ShareName: "share-a"}
	res, err := svc.LockFileNLM(context.Background(), testAuthCtx(), []byte("share-a:file-1"), owner, 0, 100, true, true)

	require.NoError(t, err, "reclaim during grace must be allowed")
	require.NotNil(t, res)
	require.True(t, res.Success, "reclaim lock must be granted")

	// The reclaim must have been recorded against the client.
	require.Contains(t, lm.GetReclaimedClients(), "client-1",
		"a successful reclaim must MarkReclaimed the owning client")
}

// testAuthCtx is the resolved identity a per-share nlmService is handed; the
// routing layer builds the real one from the share's policy.
func testAuthCtx() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
	}
}
