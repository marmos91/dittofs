package handlers

import (
	"context"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// sidGrantStore is a minimal models.UserStore that also resolves direct AD/SID
// grants (#1528), for exercising the PAC-SID path of resolveSharePermission.
type sidGrantStore struct {
	// localPerm is returned by ResolveSharePermission (the local user/group
	// path); PermissionNone means "no local grant".
	localPerm models.SharePermission
	// grant maps a granted SID to its level (the SID grant table).
	grant map[string]models.SharePermission
}

func (s *sidGrantStore) GetUser(context.Context, string) (*models.User, error) { return nil, models.ErrUserNotFound }
func (s *sidGrantStore) ValidateCredentials(context.Context, string, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (s *sidGrantStore) ListUsers(context.Context) ([]*models.User, error)         { return nil, nil }
func (s *sidGrantStore) GetGuestUser(context.Context, string) (*models.User, error) { return nil, nil }
func (s *sidGrantStore) GetGroup(context.Context, string) (*models.Group, error)    { return nil, models.ErrGroupNotFound }
func (s *sidGrantStore) ListGroups(context.Context) ([]*models.Group, error)        { return nil, nil }
func (s *sidGrantStore) GetUserGroups(context.Context, string) ([]*models.Group, error) {
	return nil, nil
}
func (s *sidGrantStore) ResolveSharePermission(context.Context, *models.User, string) (models.SharePermission, error) {
	return s.localPerm, nil
}
func (s *sidGrantStore) ResolveSharePermissionForSIDs(_ context.Context, sids []string, _ string) (models.SharePermission, error) {
	highest := models.PermissionNone
	for sid, lvl := range s.grant {
		if slices.Contains(sids, sid) && lvl.Level() > highest.Level() {
			highest = lvl
		}
	}
	return highest, nil
}

func TestResolveSharePermission_SIDGrant(t *testing.T) {
	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:12345", 1, 0, 1)
	const groupSID = "S-1-5-21-1-2-3-1104"
	share := &runtime.Share{Name: "/export", DefaultPermission: "none"}

	t.Run("AD user with no local account is authorized by a group-SID grant", func(t *testing.T) {
		// No local User on the session (sess.User == nil) — the #1528 target.
		sess := session.NewSession(1, "127.0.0.1", false, "alice@cubbit.local", "")
		sess.SetPACIdentity([]string{groupSID}, "S-1-5-21-1-2-3-1200")

		store := &sidGrantStore{
			localPerm: models.PermissionNone,
			grant:     map[string]models.SharePermission{groupSID: models.PermissionReadWrite},
		}

		perm, _ := resolveSharePermission(ctx, sess, share, models.PermissionNone, store)
		if perm != models.PermissionReadWrite {
			t.Errorf("PAC group-SID grant should authorize read-write, got %v", perm)
		}
	})

	t.Run("no matching SID grant denies (default none)", func(t *testing.T) {
		sess := session.NewSession(1, "127.0.0.1", false, "bob@cubbit.local", "")
		sess.SetPACIdentity([]string{"S-1-5-21-9-9-9-500"}, "S-1-5-21-9-9-9-501")

		store := &sidGrantStore{
			localPerm: models.PermissionNone,
			grant:     map[string]models.SharePermission{groupSID: models.PermissionReadWrite},
		}

		perm, _ := resolveSharePermission(ctx, sess, share, models.PermissionNone, store)
		if perm != models.PermissionNone {
			t.Errorf("unmatched PAC SIDs should deny (none), got %v", perm)
		}
	})

	t.Run("SID grant elevates a lower local permission", func(t *testing.T) {
		uid := uint32(1000)
		sess := session.NewSessionWithUser(1, "127.0.0.1", &models.User{Username: "alice", UID: &uid}, "")
		sess.SetPACIdentity([]string{groupSID}, "")

		store := &sidGrantStore{
			localPerm: models.PermissionRead, // local grant is read...
			grant:     map[string]models.SharePermission{groupSID: models.PermissionReadWrite}, // ...SID grant is higher
		}

		perm, _ := resolveSharePermission(ctx, sess, share, models.PermissionNone, store)
		if perm != models.PermissionReadWrite {
			t.Errorf("higher SID grant should elevate local read to read-write, got %v", perm)
		}
	})
}
