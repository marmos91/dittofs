//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestStoreAndShareNameOrIDResolution locks the docker-style addressing
// contract: metadata stores, block stores, and shares can all be looked up and
// deleted by either their name or their full ID.
func TestStoreAndShareNameOrIDResolution(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	metaID, err := store.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "meta", Type: "memory"})
	if err != nil {
		t.Fatalf("create metadata store: %v", err)
	}
	localID, err := store.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "local", Kind: models.BlockStoreKindLocal, Type: "fs"})
	if err != nil {
		t.Fatalf("create block store: %v", err)
	}

	t.Run("metadata store by name and by id", func(t *testing.T) {
		byName, err := store.GetMetadataStore(ctx, "meta")
		if err != nil || byName.ID != metaID {
			t.Fatalf("by name: got %+v err %v", byName, err)
		}
		byID, err := store.GetMetadataStore(ctx, metaID)
		if err != nil || byID.Name != "meta" {
			t.Fatalf("by id: got %+v err %v", byID, err)
		}
	})

	t.Run("block store by id is kind-scoped", func(t *testing.T) {
		byID, err := store.GetBlockStore(ctx, localID, models.BlockStoreKindLocal)
		if err != nil || byID.Name != "local" {
			t.Fatalf("by id (local): got %+v err %v", byID, err)
		}
		// The same ID under the wrong kind must not resolve.
		if _, err := store.GetBlockStore(ctx, localID, models.BlockStoreKindRemote); !errors.Is(err, models.ErrStoreNotFound) {
			t.Fatalf("by id (remote): expected ErrStoreNotFound, got %v", err)
		}
	})

	shareID, err := store.CreateShare(ctx, &models.Share{
		Name:              "/share",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID,
	})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}

	t.Run("share by name, by id, and by api-prefixed id", func(t *testing.T) {
		byName, err := store.GetShare(ctx, "/share")
		if err != nil || byName.ID != shareID {
			t.Fatalf("by name: got %+v err %v", byName, err)
		}
		byID, err := store.GetShare(ctx, shareID)
		if err != nil || byID.Name != "/share" {
			t.Fatalf("by id: got %+v err %v", byID, err)
		}
		// The API layer prepends "/" to the path token, so an ID arrives as
		// "/<id>"; it must still resolve.
		byPrefixedID, err := store.GetShare(ctx, "/"+shareID)
		if err != nil || byPrefixedID.ID != shareID {
			t.Fatalf("by prefixed id: got %+v err %v", byPrefixedID, err)
		}
	})

	t.Run("unknown token does not resolve", func(t *testing.T) {
		if _, err := store.GetMetadataStore(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, models.ErrStoreNotFound) {
			t.Fatalf("expected ErrStoreNotFound, got %v", err)
		}
	})

	t.Run("delete share by id", func(t *testing.T) {
		if err := store.DeleteShare(ctx, shareID); err != nil {
			t.Fatalf("delete share by id: %v", err)
		}
		if _, err := store.GetShare(ctx, shareID); !errors.Is(err, models.ErrShareNotFound) {
			t.Fatalf("expected ErrShareNotFound after delete, got %v", err)
		}
	})

	t.Run("delete metadata store by id", func(t *testing.T) {
		if err := store.DeleteMetadataStore(ctx, metaID); err != nil {
			t.Fatalf("delete metadata store by id: %v", err)
		}
		if _, err := store.GetMetadataStore(ctx, metaID); !errors.Is(err, models.ErrStoreNotFound) {
			t.Fatalf("expected ErrStoreNotFound after delete, got %v", err)
		}
	})
}

// TestUserGroupNetgroupNameOrIDResolution extends the docker-style addressing
// contract to identity entities: users (by username or id), groups and netgroups
// (by name or id). It also pins that a POSIX uid/gid is NOT an addressing token —
// it resolves only via the explicit *ByUID/*ByGID methods.
func TestUserGroupNetgroupNameOrIDResolution(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()
	u32 := func(v uint32) *uint32 { return &v }

	uid := uint32(5000)
	userID, err := store.CreateUser(ctx, &models.User{Username: "alice", PasswordHash: "x", Role: "user", UID: u32(uid)})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	gid := uint32(6000)
	groupID, err := store.CreateGroup(ctx, &models.Group{Name: "editors", GID: u32(gid)})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	netID, err := store.CreateNetgroup(ctx, &models.Netgroup{Name: "ng"})
	if err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	t.Run("user by username and by id", func(t *testing.T) {
		byName, err := store.GetUser(ctx, "alice")
		if err != nil || byName.ID != userID {
			t.Fatalf("by username: got %+v err %v", byName, err)
		}
		byID, err := store.GetUser(ctx, userID)
		if err != nil || byID.Username != "alice" {
			t.Fatalf("by id: got %+v err %v", byID, err)
		}
	})

	t.Run("group and netgroup by name and by id", func(t *testing.T) {
		if g, err := store.GetGroup(ctx, "editors"); err != nil || g.ID != groupID {
			t.Fatalf("group by name: got %+v err %v", g, err)
		}
		if g, err := store.GetGroup(ctx, groupID); err != nil || g.Name != "editors" {
			t.Fatalf("group by id: got %+v err %v", g, err)
		}
		if n, err := store.GetNetgroup(ctx, "ng"); err != nil || n.ID != netID {
			t.Fatalf("netgroup by name: got %+v err %v", n, err)
		}
		if n, err := store.GetNetgroup(ctx, netID); err != nil || n.Name != "ng" {
			t.Fatalf("netgroup by id: got %+v err %v", n, err)
		}
	})

	t.Run("uid/gid is not an addressing token", func(t *testing.T) {
		// A POSIX uid passed to GetUser must NOT resolve — it is neither the
		// username nor the UUID. The explicit *ByUID path still works.
		if _, err := store.GetUser(ctx, fmt.Sprintf("%d", uid)); !errors.Is(err, models.ErrUserNotFound) {
			t.Fatalf("uid as token: expected ErrUserNotFound, got %v", err)
		}
		if u, err := store.GetUserByUID(ctx, uid); err != nil || u.ID != userID {
			t.Fatalf("GetUserByUID: got %+v err %v", u, err)
		}
		if _, err := store.GetGroup(ctx, fmt.Sprintf("%d", gid)); !errors.Is(err, models.ErrGroupNotFound) {
			t.Fatalf("gid as token: expected ErrGroupNotFound, got %v", err)
		}
	})

	t.Run("unknown id does not resolve", func(t *testing.T) {
		if _, err := store.GetUser(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, models.ErrUserNotFound) {
			t.Fatalf("expected ErrUserNotFound, got %v", err)
		}
	})

	t.Run("delete by id", func(t *testing.T) {
		if err := store.DeleteUser(ctx, userID); err != nil {
			t.Fatalf("delete user by id: %v", err)
		}
		if _, err := store.GetUser(ctx, userID); !errors.Is(err, models.ErrUserNotFound) {
			t.Fatalf("expected ErrUserNotFound after delete, got %v", err)
		}
		if err := store.DeleteGroup(ctx, groupID); err != nil {
			t.Fatalf("delete group by id: %v", err)
		}
		if err := store.DeleteNetgroup(ctx, netID); err != nil {
			t.Fatalf("delete netgroup by id: %v", err)
		}
	})
}
