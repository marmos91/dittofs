package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// disabledShareRuntime reports the share as disabled while leaving every other
// lookup on the genuine registry, which is the state DisableShare leaves the
// runtime in.
type disabledShareRuntime struct {
	smbRuntime
	users models.UserStore
}

func (r *disabledShareRuntime) GetUserStore() models.UserStore { return r.users }

func (r *disabledShareRuntime) GetShare(name string) (*runtime.Share, error) {
	share, err := r.smbRuntime.GetShare(name)
	if err != nil || share == nil {
		return share, err
	}
	copied := *share
	copied.Enabled = false
	return &copied, nil
}

// TestRevalidateAuthorization_DisabledShareRemovesTree is the guard for the
// disable path. Permission resolution reads grants and the share default and
// never the enabled flag, so a disabled share re-resolves to exactly the
// permission the tree already carries and the sweep changes nothing — the
// invalidation DisableShare raises would be inert, and an established tree
// would keep serving a share TREE_CONNECT now refuses outright.
func TestRevalidateAuthorization_DisabledShareRemovesTree(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionReadWrite}
	h, _, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)
	h.Registry = &disabledShareRuntime{smbRuntime: h.Registry, users: store}

	h.RevalidateAuthorization(context.Background())

	if _, ok := h.GetTree(treeID); ok {
		t.Error("tree survived on a disabled share; the disable never reached the connection")
	}
}

// missingShareRuntime reports the share as gone, which is what RemoveShare
// leaves the registry in — it deletes the entry before the invalidation fires.
type missingShareRuntime struct {
	smbRuntime
	users models.UserStore
}

func (r *missingShareRuntime) GetUserStore() models.UserStore { return r.users }

func (r *missingShareRuntime) GetShare(string) (*runtime.Share, error) {
	return nil, errors.New("share not found")
}

// TestRevalidateAuthorization_RemovedShareRemovesTree is the same class as the
// disabled-share case above, with a different trigger. Leaving a tree alone
// because its share no longer resolves made the removal's invalidation inert:
// every later pass takes the same branch, so the tree keeps the removed share's
// permission, its opens and its byte-range locks until the connection drops,
// and only a same-name re-create ever disturbs it.
func TestRevalidateAuthorization_RemovedShareRemovesTree(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionReadWrite}
	h, _, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)
	h.Registry = &missingShareRuntime{smbRuntime: h.Registry, users: store}

	h.RevalidateAuthorization(context.Background())

	if _, ok := h.GetTree(treeID); ok {
		t.Error("tree survived after its share was removed; the removal invalidation reached a sweep that could not act on it")
	}
}

// TestRevalidateAuthorization_CancelledSweepLeavesTreesAlone pins what makes
// Stop's bounded join defensible: a cancelled sweep abandons its walk rather
// than running it out, so a join that reaches its deadline means the worker is
// wedged and not merely slow.
func TestRevalidateAuthorization_CancelledSweepLeavesTreesAlone(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionNone}
	h, _, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.RevalidateAuthorization(ctx)

	if _, ok := h.GetTree(treeID); !ok {
		t.Error("a cancelled sweep still applied a revocation; it must abandon the walk instead")
	}
}

// TestRevalidateAuthorization_EnabledShareKeepsTree is the other side: the
// removal above must be the disable doing it, not the sweep tearing down every
// tree it walks.
func TestRevalidateAuthorization_EnabledShareKeepsTree(t *testing.T) {
	store := &revalidateUserStore{user: enabledUser(), perm: models.PermissionReadWrite}
	h, _, treeID := newRevalidateHandler(t, enabledUser(), store, models.PermissionReadWrite, true)

	h.RevalidateAuthorization(context.Background())

	if _, ok := h.GetTree(treeID); !ok {
		t.Error("tree removed on an enabled share with an unchanged grant")
	}
}
