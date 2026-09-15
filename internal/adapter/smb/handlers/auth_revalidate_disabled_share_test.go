package handlers

import (
	"context"
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
