package nfs

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	metaerrors "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// lockGateFixture exports one share holding a file with the given mode, owned
// by uid 1000, and returns the live NLM service the dispatch path uses plus the
// file's handle.
func lockGateFixture(t *testing.T, mode uint32) (*routingNLMService, []byte) {
	t.Helper()

	ctx := context.Background()
	const shareName = "/lockgate"

	rt, bsID := newTestShareRuntime(t)
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	require.NoError(t, rt.RegisterMetadataStore("test-meta", metaStore))
	require.NoError(t, rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "test-meta",
		BlockStoreID:      bsID,
		DefaultPermission: string(models.PermissionReadWrite),
		Enabled:           true,
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}))

	metaSvc := rt.GetMetadataService()
	require.NotNil(t, metaSvc)

	rootAuth := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0)},
		ClientAddr: "127.0.0.1",
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	require.NoError(t, err)

	file, _, err := metaSvc.CreateFile(rootAuth, rootHandle, "owned.bin", &metadata.FileAttr{
		Mode: mode,
		UID:  1000,
		GID:  1000,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(1000), file.UID, "fixture must create the file owned by uid 1000")
	require.Equal(t, mode, file.Mode&0o7777, "fixture must create the file with the requested mode")

	handle, err := metadata.EncodeFileHandle(file)
	require.NoError(t, err)

	// A freshly exported share opens in the post-restart reclaim window, which
	// refuses every non-reclaim lock before the access gate is even reached.
	if lm := metaSvc.GetLockManagerForShare(shareName); lm != nil {
		lm.ExitGracePeriod()
	}

	s := &NFSAdapter{BaseAdapter: &adapter.BaseAdapter{Registry: rt}}
	return s.createRoutingNLMService(metaSvc), handle
}

func nlmOwner(name string) lock.LockOwner {
	return lock.LockOwner{OwnerID: "nlm:" + name + ":1:aa", ClientID: name}
}

// TestNLMLock_DeniedWithoutReadOrOwnership is the gate the NLM path never had:
// a caller with a valid handle but no access to the file it names could take an
// exclusive lock on it regardless of mode, ACL or ownership.
func TestNLMLock_DeniedWithoutReadOrOwnership(t *testing.T) {
	t.Parallel()

	svc, handle := lockGateFixture(t, 0o600)
	stranger := &metadata.Identity{UID: metadata.Uint32Ptr(2000), GID: metadata.Uint32Ptr(2000)}

	_, err := svc.LockFileNLM(context.Background(), stranger, handle, nlmOwner("stranger"), 0, 100, true, false)
	require.Error(t, err, "a caller with neither read access nor ownership must not take a lock")
	require.True(t, metaerrors.IsAccessDeniedError(err), "want a permission denial, got %v", err)

	_, _, err = svc.TestLockNLM(context.Background(), stranger, handle, nlmOwner("stranger"), 0, 100, true)
	require.Error(t, err, "TEST must be gated the same way as LOCK")
	require.True(t, metaerrors.IsAccessDeniedError(err), "want a permission denial, got %v", err)
}

// TestNLMLock_DeniedWithoutCredentials covers AUTH_NULL: no credentials means
// only the file's world permissions apply, and 0600 grants none.
func TestNLMLock_DeniedWithoutCredentials(t *testing.T) {
	t.Parallel()

	svc, handle := lockGateFixture(t, 0o600)

	_, err := svc.LockFileNLM(context.Background(), nil, handle, nlmOwner("anon"), 0, 100, true, false)
	require.Error(t, err, "an uncredentialed caller must not lock a file it cannot read")
	require.True(t, metaerrors.IsAccessDeniedError(err), "want a permission denial, got %v", err)
}

// TestNLMLock_GrantedToOwnerWhateverTheMode pins the owner override: knfsd's
// NFSD_MAY_OWNER_OVERRIDE grants the lock on ownership alone, so a file whose
// mode denies even its owner is still lockable by them.
func TestNLMLock_GrantedToOwnerWhateverTheMode(t *testing.T) {
	t.Parallel()

	// 0100 denies read to everyone, its owner included.
	svc, handle := lockGateFixture(t, 0o100)
	owner := &metadata.Identity{UID: metadata.Uint32Ptr(1000), GID: metadata.Uint32Ptr(1000)}

	res, err := svc.LockFileNLM(context.Background(), owner, handle, nlmOwner("owner"), 0, 100, true, false)
	require.NoError(t, err)
	require.True(t, res.Success, "the file's owner must be able to lock it")
}

// TestNLMLock_ReadAccessIsEnoughForAWriteLock pins the other half of the knfsd
// rule: a write lock asks for no more than a read lock does, so the
// lock-then-write sequence on a read-opened descriptor keeps working.
func TestNLMLock_ReadAccessIsEnoughForAWriteLock(t *testing.T) {
	t.Parallel()

	svc, handle := lockGateFixture(t, 0o644)
	reader := &metadata.Identity{UID: metadata.Uint32Ptr(2000), GID: metadata.Uint32Ptr(2000)}

	res, err := svc.LockFileNLM(context.Background(), reader, handle, nlmOwner("reader"), 0, 100, true, false)
	require.NoError(t, err)
	require.True(t, res.Success, "world-readable file must be lockable exclusively by a reader")

	granted, _, err := svc.TestLockNLM(context.Background(), reader, handle, nlmOwner("reader"), 200, 100, true)
	require.NoError(t, err)
	require.True(t, granted)
}
