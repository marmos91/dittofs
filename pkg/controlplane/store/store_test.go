//go:build integration

package store

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// createTestStore creates an in-memory SQLite store for testing.
func createTestStore(t *testing.T) *GORMStore {
	t.Helper()
	store, err := New(&Config{
		Type: DatabaseTypeSQLite,
		SQLite: SQLiteConfig{
			Path: ":memory:",
		},
	})
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	return store
}

func TestNew(t *testing.T) {
	t.Run("default config uses sqlite", func(t *testing.T) {
		config := &Config{}
		config.ApplyDefaults()

		if config.Type != DatabaseTypeSQLite {
			t.Errorf("expected SQLite, got %s", config.Type)
		}
	})

	t.Run("invalid config returns error", func(t *testing.T) {
		config := &Config{
			Type: "invalid",
		}
		_, err := New(config)
		if err == nil {
			t.Error("expected error for invalid config")
		}
	})

	t.Run("creates in-memory store", func(t *testing.T) {
		store := createTestStore(t)
		defer store.Close()

		if store == nil {
			t.Error("expected non-nil store")
		}
	})
}

func TestUserOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("create user", func(t *testing.T) {
		user := &models.User{
			Username:     "testuser",
			PasswordHash: "hashed-password",
			Role:         "user",
		}

		id, err := store.CreateUser(ctx, user)
		if err != nil {
			t.Fatalf("failed to create user: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty user ID")
		}
	})

	t.Run("duplicate user fails", func(t *testing.T) {
		user := &models.User{
			Username:     "testuser",
			PasswordHash: "hashed-password",
		}

		_, err := store.CreateUser(ctx, user)
		if !errors.Is(err, models.ErrDuplicateUser) {
			t.Errorf("expected ErrDuplicateUser, got %v", err)
		}
	})

	t.Run("get user", func(t *testing.T) {
		user, err := store.GetUser(ctx, "testuser")
		if err != nil {
			t.Fatalf("failed to get user: %v", err)
		}
		if user.Username != "testuser" {
			t.Errorf("expected username 'testuser', got %q", user.Username)
		}
	})

	t.Run("get user not found", func(t *testing.T) {
		_, err := store.GetUser(ctx, "nonexistent")
		if !errors.Is(err, models.ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})

	t.Run("update user", func(t *testing.T) {
		user, _ := store.GetUser(ctx, "testuser")
		user.Email = "test@example.com"

		err := store.UpdateUser(ctx, user)
		if err != nil {
			t.Fatalf("failed to update user: %v", err)
		}

		updated, _ := store.GetUser(ctx, "testuser")
		if updated.Email != "test@example.com" {
			t.Errorf("expected email 'test@example.com', got %q", updated.Email)
		}
	})

	t.Run("list users", func(t *testing.T) {
		users, err := store.ListUsers(ctx)
		if err != nil {
			t.Fatalf("failed to list users: %v", err)
		}
		if len(users) < 1 {
			t.Error("expected at least 1 user")
		}
	})

	t.Run("update password", func(t *testing.T) {
		err := store.UpdatePassword(ctx, "testuser", "new-hash", "new-nt-hash")
		if err != nil {
			t.Fatalf("failed to update password: %v", err)
		}

		user, _ := store.GetUser(ctx, "testuser")
		if user.PasswordHash != "new-hash" {
			t.Error("password hash was not updated")
		}
	})

	t.Run("update last login", func(t *testing.T) {
		now := time.Now()
		err := store.UpdateLastLogin(ctx, "testuser", now)
		if err != nil {
			t.Fatalf("failed to update last login: %v", err)
		}

		user, _ := store.GetUser(ctx, "testuser")
		if user.LastLogin == nil {
			t.Error("last login was not updated")
		}
	})

	t.Run("delete user", func(t *testing.T) {
		// Create a user to delete
		deleteUser := &models.User{
			Username:     "todelete",
			PasswordHash: "hash",
		}
		store.CreateUser(ctx, deleteUser)

		err := store.DeleteUser(ctx, "todelete")
		if err != nil {
			t.Fatalf("failed to delete user: %v", err)
		}

		_, err = store.GetUser(ctx, "todelete")
		if !errors.Is(err, models.ErrUserNotFound) {
			t.Error("user should not exist after deletion")
		}
	})

	t.Run("delete nonexistent user fails", func(t *testing.T) {
		err := store.DeleteUser(ctx, "nonexistent")
		if !errors.Is(err, models.ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})
}

func TestValidateCredentials(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create a user with a known bcrypt hash
	hash, _ := models.HashPassword("password123")
	user := &models.User{
		Username:     "authuser",
		PasswordHash: hash,
		Enabled:      true,
	}
	store.CreateUser(ctx, user)

	t.Run("valid credentials", func(t *testing.T) {
		validated, err := store.ValidateCredentials(ctx, "authuser", "password123")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if validated.Username != "authuser" {
			t.Errorf("expected username 'authuser', got %q", validated.Username)
		}
	})

	t.Run("invalid password", func(t *testing.T) {
		_, err := store.ValidateCredentials(ctx, "authuser", "wrongpassword")
		if !errors.Is(err, models.ErrInvalidCredentials) {
			t.Errorf("expected ErrInvalidCredentials, got %v", err)
		}
	})

	t.Run("nonexistent user returns invalid credentials", func(t *testing.T) {
		// Security: returns ErrInvalidCredentials (not ErrUserNotFound) to prevent user enumeration
		_, err := store.ValidateCredentials(ctx, "nonexistent", "password")
		if !errors.Is(err, models.ErrInvalidCredentials) {
			t.Errorf("expected ErrInvalidCredentials, got %v", err)
		}
	})

	t.Run("disabled user", func(t *testing.T) {
		user, _ := store.GetUser(ctx, "authuser")
		user.Enabled = false
		store.UpdateUser(ctx, user)

		_, err := store.ValidateCredentials(ctx, "authuser", "password123")
		if !errors.Is(err, models.ErrUserDisabled) {
			t.Errorf("expected ErrUserDisabled, got %v", err)
		}
	})
}

func TestGroupOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("create group", func(t *testing.T) {
		group := &models.Group{
			Name:        "developers",
			Description: "Development team",
		}

		id, err := store.CreateGroup(ctx, group)
		if err != nil {
			t.Fatalf("failed to create group: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty group ID")
		}
	})

	t.Run("duplicate group fails", func(t *testing.T) {
		group := &models.Group{Name: "developers"}
		_, err := store.CreateGroup(ctx, group)
		if !errors.Is(err, models.ErrDuplicateGroup) {
			t.Errorf("expected ErrDuplicateGroup, got %v", err)
		}
	})

	t.Run("get group", func(t *testing.T) {
		group, err := store.GetGroup(ctx, "developers")
		if err != nil {
			t.Fatalf("failed to get group: %v", err)
		}
		if group.Name != "developers" {
			t.Errorf("expected name 'developers', got %q", group.Name)
		}
	})

	t.Run("get group not found", func(t *testing.T) {
		_, err := store.GetGroup(ctx, "nonexistent")
		if !errors.Is(err, models.ErrGroupNotFound) {
			t.Errorf("expected ErrGroupNotFound, got %v", err)
		}
	})

	t.Run("get group by GID", func(t *testing.T) {
		gid := uint32(5000)
		grp := &models.Group{Name: "gid-group", GID: &gid, Description: "GID lookup test"}
		_, err := store.CreateGroup(ctx, grp)
		if err != nil {
			t.Fatalf("failed to create group with GID: %v", err)
		}

		found, err := store.GetGroupByGID(ctx, gid)
		if err != nil {
			t.Fatalf("failed to get group by GID: %v", err)
		}
		if found.Name != "gid-group" {
			t.Errorf("expected name 'gid-group', got %q", found.Name)
		}
	})

	t.Run("get group by GID not found", func(t *testing.T) {
		_, err := store.GetGroupByGID(ctx, 99999)
		if !errors.Is(err, models.ErrGroupNotFound) {
			t.Errorf("expected ErrGroupNotFound, got %v", err)
		}
	})

	t.Run("list groups", func(t *testing.T) {
		groups, err := store.ListGroups(ctx)
		if err != nil {
			t.Fatalf("failed to list groups: %v", err)
		}
		if len(groups) < 1 {
			t.Error("expected at least 1 group")
		}
	})
}

func TestUserGroupMembership(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create user and group
	user := &models.User{Username: "memberuser", PasswordHash: "hash"}
	store.CreateUser(ctx, user)
	group := &models.Group{Name: "testgroup"}
	store.CreateGroup(ctx, group)

	t.Run("add user to group", func(t *testing.T) {
		err := store.AddUserToGroup(ctx, "memberuser", "testgroup")
		if err != nil {
			t.Fatalf("failed to add user to group: %v", err)
		}

		groups, _ := store.GetUserGroups(ctx, "memberuser")
		found := false
		for _, g := range groups {
			if g.Name == "testgroup" {
				found = true
				break
			}
		}
		if !found {
			t.Error("user should be in testgroup")
		}
	})

	t.Run("get group members", func(t *testing.T) {
		members, err := store.GetGroupMembers(ctx, "testgroup")
		if err != nil {
			t.Fatalf("failed to get group members: %v", err)
		}
		found := false
		for _, m := range members {
			if m.Username == "memberuser" {
				found = true
				break
			}
		}
		if !found {
			t.Error("memberuser should be in group members")
		}
	})

	t.Run("remove user from group", func(t *testing.T) {
		err := store.RemoveUserFromGroup(ctx, "memberuser", "testgroup")
		if err != nil {
			t.Fatalf("failed to remove user from group: %v", err)
		}

		groups, _ := store.GetUserGroups(ctx, "memberuser")
		for _, g := range groups {
			if g.Name == "testgroup" {
				t.Error("user should not be in testgroup after removal")
			}
		}
	})
}

func TestUpdatePasswordAndFlags(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	store.CreateUser(ctx, &models.User{
		Username:           "pwuser",
		PasswordHash:       "old-hash",
		NTHash:             "old-nt",
		MustChangePassword: true,
		Role:               "user",
	})

	t.Run("updates password, nt hash, and flag atomically", func(t *testing.T) {
		if err := store.UpdatePasswordAndFlags(ctx, "pwuser", "new-hash", "new-nt", false); err != nil {
			t.Fatalf("UpdatePasswordAndFlags: %v", err)
		}
		u, err := store.GetUser(ctx, "pwuser")
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if u.PasswordHash != "new-hash" {
			t.Errorf("password hash = %q, want new-hash", u.PasswordHash)
		}
		if u.NTHash != "new-nt" {
			t.Errorf("nt hash = %q, want new-nt", u.NTHash)
		}
		if u.MustChangePassword {
			t.Error("MustChangePassword should be cleared")
		}
	})

	t.Run("can re-set the must-change flag", func(t *testing.T) {
		if err := store.UpdatePasswordAndFlags(ctx, "pwuser", "h2", "n2", true); err != nil {
			t.Fatalf("UpdatePasswordAndFlags: %v", err)
		}
		u, _ := store.GetUser(ctx, "pwuser")
		if !u.MustChangePassword {
			t.Error("MustChangePassword should be set")
		}
	})

	t.Run("unknown user returns ErrUserNotFound", func(t *testing.T) {
		err := store.UpdatePasswordAndFlags(ctx, "nope", "h", "n", false)
		if !errors.Is(err, models.ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})
}

func TestReplaceUserGroups(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	store.CreateUser(ctx, &models.User{Username: "repluser", PasswordHash: "h", Role: "user"})
	store.CreateGroup(ctx, &models.Group{Name: "g1"})
	store.CreateGroup(ctx, &models.Group{Name: "g2"})
	store.CreateGroup(ctx, &models.Group{Name: "g3"})

	groupNames := func(t *testing.T) map[string]bool {
		t.Helper()
		groups, err := store.GetUserGroups(ctx, "repluser")
		if err != nil {
			t.Fatalf("GetUserGroups: %v", err)
		}
		set := map[string]bool{}
		for _, g := range groups {
			set[g.Name] = true
		}
		return set
	}

	t.Run("sets initial memberships", func(t *testing.T) {
		if err := store.ReplaceUserGroups(ctx, "repluser", []string{"g1", "g2"}); err != nil {
			t.Fatalf("ReplaceUserGroups: %v", err)
		}
		got := groupNames(t)
		if !got["g1"] || !got["g2"] || got["g3"] || len(got) != 2 {
			t.Errorf("groups = %v, want {g1,g2}", got)
		}
	})

	t.Run("replaces (adds and removes) atomically", func(t *testing.T) {
		if err := store.ReplaceUserGroups(ctx, "repluser", []string{"g2", "g3"}); err != nil {
			t.Fatalf("ReplaceUserGroups: %v", err)
		}
		got := groupNames(t)
		if got["g1"] || !got["g2"] || !got["g3"] || len(got) != 2 {
			t.Errorf("groups = %v, want {g2,g3}", got)
		}
	})

	t.Run("deduplicates input", func(t *testing.T) {
		if err := store.ReplaceUserGroups(ctx, "repluser", []string{"g1", "g1", "g1"}); err != nil {
			t.Fatalf("ReplaceUserGroups: %v", err)
		}
		got := groupNames(t)
		if !got["g1"] || len(got) != 1 {
			t.Errorf("groups = %v, want {g1}", got)
		}
	})

	t.Run("empty list clears all memberships", func(t *testing.T) {
		if err := store.ReplaceUserGroups(ctx, "repluser", nil); err != nil {
			t.Fatalf("ReplaceUserGroups: %v", err)
		}
		if got := groupNames(t); len(got) != 0 {
			t.Errorf("groups = %v, want empty", got)
		}
	})

	t.Run("unknown group rolls back and returns ErrGroupNotFound", func(t *testing.T) {
		store.ReplaceUserGroups(ctx, "repluser", []string{"g1"})
		err := store.ReplaceUserGroups(ctx, "repluser", []string{"g2", "nope"})
		if !errors.Is(err, models.ErrGroupNotFound) {
			t.Errorf("expected ErrGroupNotFound, got %v", err)
		}
		// Prior memberships must be untouched on failure.
		got := groupNames(t)
		if !got["g1"] || len(got) != 1 {
			t.Errorf("groups after failed replace = %v, want unchanged {g1}", got)
		}
	})

	t.Run("unknown user returns ErrUserNotFound", func(t *testing.T) {
		err := store.ReplaceUserGroups(ctx, "ghost", []string{"g1"})
		if !errors.Is(err, models.ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})
}

func TestCreateUserWithGroups(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	// Create groups
	s.CreateGroup(ctx, &models.Group{Name: "devs"})
	s.CreateGroup(ctx, &models.Group{Name: "ops"})

	t.Run("creates user with groups atomically", func(t *testing.T) {
		user := &models.User{Username: "alice", PasswordHash: "hash", Role: "user"}
		id, err := s.CreateUserWithGroups(ctx, user, []string{"devs", "ops"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}

		groups, err := s.GetUserGroups(ctx, "alice")
		if err != nil {
			t.Fatalf("failed to get user groups: %v", err)
		}
		if len(groups) != 2 {
			t.Errorf("expected 2 groups, got %d", len(groups))
		}
		names := map[string]bool{}
		for _, g := range groups {
			names[g.Name] = true
		}
		if !names["devs"] || !names["ops"] {
			t.Errorf("expected groups devs and ops, got %v", names)
		}
	})

	t.Run("fails if group does not exist", func(t *testing.T) {
		user := &models.User{Username: "bob", PasswordHash: "hash", Role: "user"}
		_, err := s.CreateUserWithGroups(ctx, user, []string{"devs", "nonexistent"})
		if !errors.Is(err, models.ErrGroupNotFound) {
			t.Errorf("expected ErrGroupNotFound, got %v", err)
		}

		// User should NOT have been created
		_, err = s.GetUser(ctx, "bob")
		if !errors.Is(err, models.ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound (rollback), got %v", err)
		}
	})

	t.Run("fails on duplicate user", func(t *testing.T) {
		user := &models.User{Username: "alice", PasswordHash: "hash", Role: "user"}
		_, err := s.CreateUserWithGroups(ctx, user, []string{"devs"})
		if !errors.Is(err, models.ErrDuplicateUser) {
			t.Errorf("expected ErrDuplicateUser, got %v", err)
		}
	})

	t.Run("empty groups creates user without groups", func(t *testing.T) {
		user := &models.User{Username: "charlie", PasswordHash: "hash", Role: "user"}
		id, err := s.CreateUserWithGroups(ctx, user, []string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}
		groups, _ := s.GetUserGroups(ctx, "charlie")
		if len(groups) != 0 {
			t.Errorf("expected 0 groups, got %d", len(groups))
		}
	})
}

func TestShareOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create prerequisite stores
	metaStore := &models.MetadataStoreConfig{Name: "test-meta", Type: "memory"}
	metaStoreID, _ := store.CreateMetadataStore(ctx, metaStore)
	localBlockStore := &models.BlockStoreConfig{Name: "test-local", Kind: models.BlockStoreKindLocal, Type: "fs"}
	localBlockStoreID, _ := store.CreateBlockStore(ctx, localBlockStore)

	t.Run("create share", func(t *testing.T) {
		share := &models.Share{
			Name:              "/export",
			MetadataStoreID:   metaStoreID,
			LocalBlockStoreID: localBlockStoreID,
		}

		id, err := store.CreateShare(ctx, share)
		if err != nil {
			t.Fatalf("failed to create share: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty share ID")
		}
	})

	t.Run("duplicate share fails", func(t *testing.T) {
		share := &models.Share{
			Name:              "/export",
			MetadataStoreID:   metaStoreID,
			LocalBlockStoreID: localBlockStoreID,
		}
		_, err := store.CreateShare(ctx, share)
		if !errors.Is(err, models.ErrDuplicateShare) {
			t.Errorf("expected ErrDuplicateShare, got %v", err)
		}
	})

	t.Run("get share", func(t *testing.T) {
		share, err := store.GetShare(ctx, "/export")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		if share.Name != "/export" {
			t.Errorf("expected name '/export', got %q", share.Name)
		}
	})

	t.Run("get share not found", func(t *testing.T) {
		_, err := store.GetShare(ctx, "/nonexistent")
		if !errors.Is(err, models.ErrShareNotFound) {
			t.Errorf("expected ErrShareNotFound, got %v", err)
		}
	})

	t.Run("list shares", func(t *testing.T) {
		shares, err := store.ListShares(ctx)
		if err != nil {
			t.Fatalf("failed to list shares: %v", err)
		}
		if len(shares) < 1 {
			t.Error("expected at least 1 share")
		}
	})

	t.Run("update share", func(t *testing.T) {
		share, _ := store.GetShare(ctx, "/export")
		share.ReadOnly = true

		err := store.UpdateShare(ctx, share)
		if err != nil {
			t.Fatalf("failed to update share: %v", err)
		}

		updated, _ := store.GetShare(ctx, "/export")
		if !updated.ReadOnly {
			t.Error("expected ReadOnly to be true")
		}
	})

	t.Run("new share defaults enabled=true", func(t *testing.T) {
		share := &models.Share{
			Name:              "/export-enabled-default",
			MetadataStoreID:   metaStoreID,
			LocalBlockStoreID: localBlockStoreID,
		}
		if _, err := store.CreateShare(ctx, share); err != nil {
			t.Fatalf("failed to create share: %v", err)
		}
		got, err := store.GetShare(ctx, "/export-enabled-default")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		if !got.Enabled {
			t.Error("expected newly created share to default Enabled=true (D-01)")
		}
	})

	t.Run("create share persists acl_flag_inherited_canonicalization=true verbatim (#514)", func(t *testing.T) {
		// Refs #514: after the CreateShare store-layer fix, operator
		// intent for AclFlagInheritedCanonicalization is authoritative —
		// the caller (API layer) is responsible for choosing the
		// "default true" value when the request omits the field.
		share := &models.Share{
			Name:                             "/export-acl-canon-default",
			MetadataStoreID:                  metaStoreID,
			LocalBlockStoreID:                localBlockStoreID,
			AclFlagInheritedCanonicalization: true,
		}
		if _, err := store.CreateShare(ctx, share); err != nil {
			t.Fatalf("failed to create share: %v", err)
		}
		got, err := store.GetShare(ctx, "/export-acl-canon-default")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		if !got.AclFlagInheritedCanonicalization {
			t.Error("expected CreateShare with AclFlagInheritedCanonicalization=true to persist true")
		}
	})

	t.Run("allow_mfsymlink round-trips through create and update", func(t *testing.T) {
		// Guards the explicit column:allow_mfsymlink pin — GORM's default
		// naming would mangle the "MFsymlink" initialism, so the field-map and
		// backfill literal would target a column AutoMigrate never created.
		share := &models.Share{
			Name:              "/export-mfsymlink",
			MetadataStoreID:   metaStoreID,
			LocalBlockStoreID: localBlockStoreID,
			AllowMFsymlink:    true,
		}
		if _, err := store.CreateShare(ctx, share); err != nil {
			t.Fatalf("failed to create share: %v", err)
		}
		got, err := store.GetShare(ctx, "/export-mfsymlink")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		if !got.AllowMFsymlink {
			t.Error("expected CreateShare with AllowMFsymlink=true to persist true")
		}

		got.AllowMFsymlink = false
		if err := store.UpdateShare(ctx, got); err != nil {
			t.Fatalf("failed to update share: %v", err)
		}
		updated, err := store.GetShare(ctx, "/export-mfsymlink")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		if updated.AllowMFsymlink {
			t.Error("expected UpdateShare to persist AllowMFsymlink=false")
		}
	})

	t.Run("create share persists acl_flag_inherited_canonicalization=false (#514 GORM default override)", func(t *testing.T) {
		// Refs #514: GORM substitutes the SQL `default:true` for the
		// Go zero-value `false`, silently coercing operator intent.
		// CreateShare must override that substitution so `false`
		// round-trips verbatim — otherwise the handler/CLI bool toggle
		// is undetectably ignored.
		share := &models.Share{
			Name:                             "/export-acl-canon-false",
			MetadataStoreID:                  metaStoreID,
			LocalBlockStoreID:                localBlockStoreID,
			AclFlagInheritedCanonicalization: false,
		}
		if _, err := store.CreateShare(ctx, share); err != nil {
			t.Fatalf("failed to create share: %v", err)
		}
		got, err := store.GetShare(ctx, "/export-acl-canon-false")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		if got.AclFlagInheritedCanonicalization {
			t.Error("expected CreateShare with AclFlagInheritedCanonicalization=false to persist false; got true (GORM default-coercion override missing?)")
		}
	})

	t.Run("update share persists enabled=false (D-25 whitelist fix)", func(t *testing.T) {
		share, err := store.GetShare(ctx, "/export")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		if !share.Enabled {
			t.Fatalf("precondition: share must start enabled; got Enabled=%v", share.Enabled)
		}
		share.Enabled = false
		if err := store.UpdateShare(ctx, share); err != nil {
			t.Fatalf("failed to update share: %v", err)
		}
		updated, err := store.GetShare(ctx, "/export")
		if err != nil {
			t.Fatalf("failed to re-read share: %v", err)
		}
		if updated.Enabled {
			t.Error("expected Enabled=false to round-trip through UpdateShare; got Enabled=true (whitelist entry missing?)")
		}

		// Restore to enabled so downstream tests see a clean state.
		updated.Enabled = true
		if err := store.UpdateShare(ctx, updated); err != nil {
			t.Fatalf("failed to restore enabled=true: %v", err)
		}
	})

	t.Run("update share persists AclFlagInheritedCanonicalization=false (#514 whitelist)", func(t *testing.T) {
		share, err := store.GetShare(ctx, "/export")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}
		// Seed AclFlagInheritedCanonicalization=true so the flip-to-false
		// below has a meaningful starting state. CreateShare in the
		// first sub-test left the Go zero-value `false` (now authoritative
		// at the store layer post-#514 fix).
		share.AclFlagInheritedCanonicalization = true
		if err := store.UpdateShare(ctx, share); err != nil {
			t.Fatalf("failed to seed AclFlagInheritedCanonicalization=true: %v", err)
		}

		share.AclFlagInheritedCanonicalization = false
		if err := store.UpdateShare(ctx, share); err != nil {
			t.Fatalf("failed to update share: %v", err)
		}
		updated, err := store.GetShare(ctx, "/export")
		if err != nil {
			t.Fatalf("failed to re-read share: %v", err)
		}
		if updated.AclFlagInheritedCanonicalization {
			t.Error("expected AclFlagInheritedCanonicalization=false to round-trip through UpdateShare; got true (whitelist entry missing?)")
		}

		// Restore default so downstream tests see a clean state.
		updated.AclFlagInheritedCanonicalization = true
		if err := store.UpdateShare(ctx, updated); err != nil {
			t.Fatalf("failed to restore AclFlagInheritedCanonicalization=true: %v", err)
		}
	})
}

func TestSharePermissions(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create prerequisites
	user := &models.User{Username: "permuser", PasswordHash: "hash"}
	store.CreateUser(ctx, user)
	group := &models.Group{Name: "permgroup"}
	store.CreateGroup(ctx, group)
	metaStore := &models.MetadataStoreConfig{Name: "perm-meta", Type: "memory"}
	metaStoreID, _ := store.CreateMetadataStore(ctx, metaStore)
	localBlockStore := &models.BlockStoreConfig{Name: "perm-local", Kind: models.BlockStoreKindLocal, Type: "fs"}
	localBlockStoreID, _ := store.CreateBlockStore(ctx, localBlockStore)
	share := &models.Share{
		Name:              "/permshare",
		MetadataStoreID:   metaStoreID,
		LocalBlockStoreID: localBlockStoreID,
	}
	store.CreateShare(ctx, share)

	t.Run("set user share permission", func(t *testing.T) {
		shareInfo, _ := store.GetShare(ctx, "/permshare")
		userInfo, _ := store.GetUser(ctx, "permuser")

		perm := &models.UserSharePermission{
			UserID:     userInfo.ID,
			ShareID:    shareInfo.ID,
			ShareName:  "/permshare",
			Permission: "read-write",
		}

		err := store.SetUserSharePermission(ctx, perm)
		if err != nil {
			t.Fatalf("failed to set permission: %v", err)
		}
	})

	t.Run("set user share permission is idempotent upsert (first call persists)", func(t *testing.T) {
		// Use a fresh user/share pair distinct from the shared fixtures so this
		// sub-test is independent of execution order.
		uUser := &models.User{Username: "upsert-user", PasswordHash: "h"}
		store.CreateUser(ctx, uUser)
		uMeta := &models.MetadataStoreConfig{Name: "upsert-meta", Type: "memory"}
		uMetaID, _ := store.CreateMetadataStore(ctx, uMeta)
		uLocal := &models.BlockStoreConfig{Name: "upsert-local", Kind: models.BlockStoreKindLocal, Type: "fs"}
		uLocalID, _ := store.CreateBlockStore(ctx, uLocal)
		uShare := &models.Share{Name: "/upsert-share", MetadataStoreID: uMetaID, LocalBlockStoreID: uLocalID}
		store.CreateShare(ctx, uShare)

		shareInfo, _ := store.GetShare(ctx, "/upsert-share")
		userInfo, _ := store.GetUser(ctx, "upsert-user")

		// First call — before the fix this silently dropped the row.
		perm1 := &models.UserSharePermission{
			UserID:     userInfo.ID,
			ShareID:    shareInfo.ID,
			ShareName:  "/upsert-share",
			Permission: "read",
		}
		if err := store.SetUserSharePermission(ctx, perm1); err != nil {
			t.Fatalf("first SetUserSharePermission: %v", err)
		}
		got, err := store.GetUserSharePermission(ctx, "upsert-user", "/upsert-share")
		if err != nil {
			t.Fatalf("GetUserSharePermission after first set: %v", err)
		}
		if got == nil {
			t.Fatal("permission not persisted on first SetUserSharePermission call (nil returned) — upsert bug still present")
		}
		if got.Permission != "read" {
			t.Errorf("expected 'read' after first set, got %q", got.Permission)
		}

		// Second call — must overwrite, not error.
		perm2 := &models.UserSharePermission{
			UserID:     userInfo.ID,
			ShareID:    shareInfo.ID,
			ShareName:  "/upsert-share",
			Permission: "read-write",
		}
		if err := store.SetUserSharePermission(ctx, perm2); err != nil {
			t.Fatalf("second SetUserSharePermission: %v", err)
		}
		got2, err := store.GetUserSharePermission(ctx, "upsert-user", "/upsert-share")
		if err != nil {
			t.Fatalf("GetUserSharePermission after second set: %v", err)
		}
		if got2 == nil || got2.Permission != "read-write" {
			t.Errorf("expected 'read-write' after second set, got %v", got2)
		}
	})

	t.Run("get user share permission", func(t *testing.T) {
		perm, err := store.GetUserSharePermission(ctx, "permuser", "/permshare")
		if err != nil {
			t.Fatalf("failed to get permission: %v", err)
		}
		if perm.Permission != "read-write" {
			t.Errorf("expected 'read-write', got %q", perm.Permission)
		}
	})

	t.Run("set group share permission", func(t *testing.T) {
		shareInfo, _ := store.GetShare(ctx, "/permshare")
		groupInfo, _ := store.GetGroup(ctx, "permgroup")

		perm := &models.GroupSharePermission{
			GroupID:    groupInfo.ID,
			ShareID:    shareInfo.ID,
			ShareName:  "/permshare",
			Permission: "read",
		}

		err := store.SetGroupSharePermission(ctx, perm)
		if err != nil {
			t.Fatalf("failed to set group permission: %v", err)
		}
	})

	t.Run("set group share permission is idempotent upsert (first call persists)", func(t *testing.T) {
		uGroup := &models.Group{Name: "upsert-group"}
		store.CreateGroup(ctx, uGroup)
		uMeta2 := &models.MetadataStoreConfig{Name: "upsert-meta2", Type: "memory"}
		uMetaID2, _ := store.CreateMetadataStore(ctx, uMeta2)
		uLocal2 := &models.BlockStoreConfig{Name: "upsert-local2", Kind: models.BlockStoreKindLocal, Type: "fs"}
		uLocalID2, _ := store.CreateBlockStore(ctx, uLocal2)
		uShare2 := &models.Share{Name: "/upsert-gshare", MetadataStoreID: uMetaID2, LocalBlockStoreID: uLocalID2}
		store.CreateShare(ctx, uShare2)

		shareInfo, _ := store.GetShare(ctx, "/upsert-gshare")
		groupInfo, _ := store.GetGroup(ctx, "upsert-group")

		// First call — before the fix this silently dropped the row.
		gperm1 := &models.GroupSharePermission{
			GroupID:    groupInfo.ID,
			ShareID:    shareInfo.ID,
			ShareName:  "/upsert-gshare",
			Permission: "read",
		}
		if err := store.SetGroupSharePermission(ctx, gperm1); err != nil {
			t.Fatalf("first SetGroupSharePermission: %v", err)
		}
		ggot, err := store.GetGroupSharePermission(ctx, "upsert-group", "/upsert-gshare")
		if err != nil {
			t.Fatalf("GetGroupSharePermission after first set: %v", err)
		}
		if ggot == nil {
			t.Fatal("permission not persisted on first SetGroupSharePermission call (nil returned) — upsert bug still present")
		}
		if ggot.Permission != "read" {
			t.Errorf("expected 'read' after first set, got %q", ggot.Permission)
		}

		// Second call — must overwrite, not error.
		gperm2 := &models.GroupSharePermission{
			GroupID:    groupInfo.ID,
			ShareID:    shareInfo.ID,
			ShareName:  "/upsert-gshare",
			Permission: "admin",
		}
		if err := store.SetGroupSharePermission(ctx, gperm2); err != nil {
			t.Fatalf("second SetGroupSharePermission: %v", err)
		}
		ggot2, err := store.GetGroupSharePermission(ctx, "upsert-group", "/upsert-gshare")
		if err != nil {
			t.Fatalf("GetGroupSharePermission after second set: %v", err)
		}
		if ggot2 == nil || ggot2.Permission != "admin" {
			t.Errorf("expected 'admin' after second set, got %v", ggot2)
		}
	})

	t.Run("get group share permission", func(t *testing.T) {
		perm, err := store.GetGroupSharePermission(ctx, "permgroup", "/permshare")
		if err != nil {
			t.Fatalf("failed to get group permission: %v", err)
		}
		if perm.Permission != "read" {
			t.Errorf("expected 'read', got %q", perm.Permission)
		}
	})

	t.Run("resolve share permission - user explicit wins", func(t *testing.T) {
		userInfo, _ := store.GetUser(ctx, "permuser")
		perm, err := store.ResolveSharePermission(ctx, userInfo, "/permshare")
		if err != nil {
			t.Fatalf("failed to resolve permission: %v", err)
		}
		if perm != models.PermissionReadWrite {
			t.Errorf("expected read-write, got %q", perm)
		}
	})

	t.Run("delete user share permission", func(t *testing.T) {
		err := store.DeleteUserSharePermission(ctx, "permuser", "/permshare")
		if err != nil {
			t.Fatalf("failed to delete permission: %v", err)
		}

		perm, err := store.GetUserSharePermission(ctx, "permuser", "/permshare")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if perm != nil {
			t.Error("permission should be nil after deletion")
		}
	})
}

func TestAdapterOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("create adapter", func(t *testing.T) {
		adapter := &models.AdapterConfig{
			Type:    "nfs",
			Port:    2049,
			Enabled: true,
		}

		id, err := store.CreateAdapter(ctx, adapter)
		if err != nil {
			t.Fatalf("failed to create adapter: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty adapter ID")
		}
	})

	t.Run("duplicate adapter fails", func(t *testing.T) {
		adapter := &models.AdapterConfig{Type: "nfs", Port: 2049}
		_, err := store.CreateAdapter(ctx, adapter)
		if !errors.Is(err, models.ErrDuplicateAdapter) {
			t.Errorf("expected ErrDuplicateAdapter, got %v", err)
		}
	})

	t.Run("get adapter", func(t *testing.T) {
		adapter, err := store.GetAdapter(ctx, "nfs")
		if err != nil {
			t.Fatalf("failed to get adapter: %v", err)
		}
		if adapter.Type != "nfs" {
			t.Errorf("expected type 'nfs', got %q", adapter.Type)
		}
	})

	t.Run("get adapter not found", func(t *testing.T) {
		_, err := store.GetAdapter(ctx, "nonexistent")
		if !errors.Is(err, models.ErrAdapterNotFound) {
			t.Errorf("expected ErrAdapterNotFound, got %v", err)
		}
	})

	t.Run("list adapters", func(t *testing.T) {
		adapters, err := store.ListAdapters(ctx)
		if err != nil {
			t.Fatalf("failed to list adapters: %v", err)
		}
		if len(adapters) < 1 {
			t.Error("expected at least 1 adapter")
		}
	})

	t.Run("update adapter", func(t *testing.T) {
		adapter, _ := store.GetAdapter(ctx, "nfs")
		adapter.Port = 12049

		err := store.UpdateAdapter(ctx, adapter)
		if err != nil {
			t.Fatalf("failed to update adapter: %v", err)
		}

		updated, _ := store.GetAdapter(ctx, "nfs")
		if updated.Port != 12049 {
			t.Errorf("expected port 12049, got %d", updated.Port)
		}
	})

	t.Run("delete adapter", func(t *testing.T) {
		err := store.DeleteAdapter(ctx, "nfs")
		if err != nil {
			t.Fatalf("failed to delete adapter: %v", err)
		}

		_, err = store.GetAdapter(ctx, "nfs")
		if !errors.Is(err, models.ErrAdapterNotFound) {
			t.Error("adapter should not exist after deletion")
		}
	})
}

func TestSettingsOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("set setting", func(t *testing.T) {
		err := store.SetSetting(ctx, "test-key", "test-value")
		if err != nil {
			t.Fatalf("failed to set setting: %v", err)
		}
	})

	t.Run("get setting", func(t *testing.T) {
		value, err := store.GetSetting(ctx, "test-key")
		if err != nil {
			t.Fatalf("failed to get setting: %v", err)
		}
		if value != "test-value" {
			t.Errorf("expected 'test-value', got %q", value)
		}
	})

	t.Run("get non-existing setting returns empty", func(t *testing.T) {
		value, err := store.GetSetting(ctx, "nonexistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if value != "" {
			t.Errorf("expected empty string, got %q", value)
		}
	})

	t.Run("list settings", func(t *testing.T) {
		settings, err := store.ListSettings(ctx)
		if err != nil {
			t.Fatalf("failed to list settings: %v", err)
		}
		if len(settings) < 1 {
			t.Error("expected at least 1 setting")
		}
	})

	t.Run("delete setting", func(t *testing.T) {
		err := store.DeleteSetting(ctx, "test-key")
		if err != nil {
			t.Fatalf("failed to delete setting: %v", err)
		}

		value, _ := store.GetSetting(ctx, "test-key")
		if value != "" {
			t.Error("setting should be empty after deletion")
		}
	})
}

func TestMetadataStoreOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("create metadata store", func(t *testing.T) {
		metaStore := &models.MetadataStoreConfig{
			Name:   "meta-store",
			Type:   "memory",
			Config: `{}`,
		}

		id, err := store.CreateMetadataStore(ctx, metaStore)
		if err != nil {
			t.Fatalf("failed to create metadata store: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}
	})

	t.Run("duplicate metadata store fails", func(t *testing.T) {
		metaStore := &models.MetadataStoreConfig{Name: "meta-store", Type: "memory"}
		_, err := store.CreateMetadataStore(ctx, metaStore)
		if !errors.Is(err, models.ErrDuplicateStore) {
			t.Errorf("expected ErrDuplicateStore, got %v", err)
		}
	})

	t.Run("get metadata store", func(t *testing.T) {
		metaStore, err := store.GetMetadataStore(ctx, "meta-store")
		if err != nil {
			t.Fatalf("failed to get metadata store: %v", err)
		}
		if metaStore.Name != "meta-store" {
			t.Errorf("expected name 'meta-store', got %q", metaStore.Name)
		}
	})

	t.Run("list metadata stores", func(t *testing.T) {
		stores, err := store.ListMetadataStores(ctx)
		if err != nil {
			t.Fatalf("failed to list stores: %v", err)
		}
		if len(stores) < 1 {
			t.Error("expected at least 1 store")
		}
	})
}

func TestBlockStoreOperationsBasic(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("create block store", func(t *testing.T) {
		blockStore := &models.BlockStoreConfig{
			Name:   "block-store",
			Kind:   models.BlockStoreKindRemote,
			Type:   "memory",
			Config: `{}`,
		}

		id, err := store.CreateBlockStore(ctx, blockStore)
		if err != nil {
			t.Fatalf("failed to create block store: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}
	})

	t.Run("duplicate block store fails", func(t *testing.T) {
		blockStore := &models.BlockStoreConfig{Name: "block-store", Kind: models.BlockStoreKindRemote, Type: "memory"}
		_, err := store.CreateBlockStore(ctx, blockStore)
		if !errors.Is(err, models.ErrDuplicateStore) {
			t.Errorf("expected ErrDuplicateStore, got %v", err)
		}
	})

	t.Run("get block store", func(t *testing.T) {
		blockStore, err := store.GetBlockStore(ctx, "block-store", models.BlockStoreKindRemote)
		if err != nil {
			t.Fatalf("failed to get block store: %v", err)
		}
		if blockStore.Name != "block-store" {
			t.Errorf("expected name 'block-store', got %q", blockStore.Name)
		}
	})

	t.Run("list block stores", func(t *testing.T) {
		stores, err := store.ListBlockStores(ctx, models.BlockStoreKindRemote)
		if err != nil {
			t.Fatalf("failed to list stores: %v", err)
		}
		if len(stores) < 1 {
			t.Error("expected at least 1 store")
		}
	})
}

func TestEnsureAdminUser(t *testing.T) {
	// EnsureAdminUser disables the forced change when an initial password is
	// supplied via env; clear it so the MustChangePassword assertions below are
	// deterministic regardless of the developer/CI shell.
	t.Setenv(models.EnvAdminInitialPassword, "")
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("creates admin if not exists", func(t *testing.T) {
		password, err := store.EnsureAdminUser(ctx, true)
		if err != nil {
			t.Fatalf("failed to ensure admin user: %v", err)
		}
		if password == "" {
			t.Error("expected non-empty initial password")
		}

		// Verify admin exists
		user, err := store.GetUser(ctx, "admin")
		if err != nil {
			t.Fatalf("admin user should exist: %v", err)
		}
		if user.Role != "admin" {
			t.Errorf("expected admin role, got %q", user.Role)
		}
		// Default behavior: admin must change its password on first login.
		if !user.MustChangePassword {
			t.Error("expected MustChangePassword=true when forced change is required")
		}
	})

	t.Run("second call returns empty password", func(t *testing.T) {
		password, err := store.EnsureAdminUser(ctx, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if password != "" {
			t.Error("expected empty password on second call")
		}
	})

	t.Run("is admin initialized", func(t *testing.T) {
		initialized, err := store.IsAdminInitialized(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !initialized {
			t.Error("admin should be initialized")
		}
	})
}

// TestEnsureAdminUser_OptOutForcedChange verifies that the
// require_initial_password_change knob, when disabled, provisions the bootstrap
// admin without the forced first-login password change.
func TestEnsureAdminUser_OptOutForcedChange(t *testing.T) {
	// Clear the env override so this exercises the requireInitialPasswordChange
	// path rather than the env-driven skip.
	t.Setenv(models.EnvAdminInitialPassword, "")
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	password, err := store.EnsureAdminUser(ctx, false)
	if err != nil {
		t.Fatalf("failed to ensure admin user: %v", err)
	}
	if password == "" {
		t.Error("expected non-empty initial password")
	}

	user, err := store.GetUser(ctx, "admin")
	if err != nil {
		t.Fatalf("admin user should exist: %v", err)
	}
	if user.MustChangePassword {
		t.Error("expected MustChangePassword=false when forced change is disabled")
	}
}

func TestEnsureDefaultGroups(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("creates all default groups on fresh store", func(t *testing.T) {
		created, err := store.EnsureDefaultGroups(ctx)
		if err != nil {
			t.Fatalf("failed to ensure default groups: %v", err)
		}
		if !created {
			t.Error("expected created=true on first call")
		}

		expected := []struct {
			name string
			gid  uint32
		}{
			{"admins", 0},
			{"operators", 999},
			{"users", 1000},
		}

		for _, exp := range expected {
			group, err := store.GetGroup(ctx, exp.name)
			if err != nil {
				t.Fatalf("group %q should exist: %v", exp.name, err)
			}
			if group.GID == nil || *group.GID != exp.gid {
				t.Errorf("group %q: expected GID %d, got %v", exp.name, exp.gid, group.GID)
			}
		}
	})

	t.Run("idempotent on second call", func(t *testing.T) {
		created, err := store.EnsureDefaultGroups(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if created {
			t.Error("expected created=false on second call")
		}
	})

	t.Run("adds admin user to admins group", func(t *testing.T) {
		// Create admin user first
		_, err := store.EnsureAdminUser(ctx, true)
		if err != nil {
			t.Fatalf("failed to ensure admin user: %v", err)
		}

		// Re-run to trigger the admin-to-admins logic
		_, err = store.EnsureDefaultGroups(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		members, err := store.GetGroupMembers(ctx, "admins")
		if err != nil {
			t.Fatalf("failed to get admins members: %v", err)
		}

		found := false
		for _, m := range members {
			if m.Username == "admin" {
				found = true
				break
			}
		}
		if !found {
			t.Error("admin user should be a member of the admins group")
		}
	})
}

func TestHealthcheck(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	err := store.Healthcheck(ctx)
	if err != nil {
		t.Errorf("healthcheck should pass: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Run("sqlite requires path", func(t *testing.T) {
		config := &Config{
			Type:   DatabaseTypeSQLite,
			SQLite: SQLiteConfig{Path: ""},
		}
		err := config.Validate()
		if err == nil {
			t.Error("expected error for empty sqlite path")
		}
	})

	t.Run("postgres requires host", func(t *testing.T) {
		config := &Config{
			Type: DatabaseTypePostgres,
			Postgres: PostgresConfig{
				Database: "test",
				User:     "test",
			},
		}
		err := config.Validate()
		if err == nil {
			t.Error("expected error for missing postgres host")
		}
	})

	t.Run("postgres requires database", func(t *testing.T) {
		config := &Config{
			Type: DatabaseTypePostgres,
			Postgres: PostgresConfig{
				Host: "localhost",
				User: "test",
			},
		}
		err := config.Validate()
		if err == nil {
			t.Error("expected error for missing postgres database")
		}
	})

	t.Run("postgres requires user", func(t *testing.T) {
		config := &Config{
			Type: DatabaseTypePostgres,
			Postgres: PostgresConfig{
				Host:     "localhost",
				Database: "test",
			},
		}
		err := config.Validate()
		if err == nil {
			t.Error("expected error for missing postgres user")
		}
	})
}

func TestPostgresDSN(t *testing.T) {
	config := PostgresConfig{
		Host:        "localhost",
		Port:        5432,
		Database:    "dittofs",
		User:        "admin",
		Password:    "secret",
		SSLMode:     "require",
		SSLRootCert: "/path/to/cert",
	}

	dsn := config.DSN()
	if dsn == "" {
		t.Fatal("expected non-empty DSN")
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN() is not a valid URL: %v", err)
	}
	if u.Hostname() != "localhost" {
		t.Errorf("host = %q, want localhost", u.Hostname())
	}
	if u.Port() != "5432" {
		t.Errorf("port = %q, want 5432", u.Port())
	}
	if u.Query().Get("sslmode") != "require" {
		t.Errorf("sslmode = %q, want require", u.Query().Get("sslmode"))
	}
	if u.Query().Get("sslrootcert") != "/path/to/cert" {
		t.Errorf("sslrootcert = %q, want /path/to/cert", u.Query().Get("sslrootcert"))
	}
}

// TestAccessBasedEnumerationBackfill verifies that the post-AutoMigrate
// backfill replaces NULL access_based_enumeration values with false. SQLite
// ALTER TABLE ADD COLUMN can leave NULL despite the DEFAULT clause when the
// column was added before the NOT NULL tag landed, which would surface as a
// Scan failure on the non-nullable bool. Mirrors the shares.enabled backfill
// contract (refs PR #536 review).
//
// The test simulates a legacy row by recreating the table without the NOT
// NULL constraint, inserting NULL, then running the backfill statement and
// asserting the value was replaced with false.
func TestAccessBasedEnumerationBackfill(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	db := store.DB()

	// Recreate the table without NOT NULL on access_based_enumeration so we
	// can simulate a row that predates the constraint. AutoMigrate adds the
	// constraint at first run; legacy SQLite DBs that started life without
	// it (i.e. created before the model tag was added) are exactly what
	// the backfill targets.
	if err := db.Exec(`DROP TABLE shares`).Error; err != nil {
		t.Fatalf("drop shares: %v", err)
	}
	if err := db.Exec(`
		CREATE TABLE shares (
			id TEXT PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			metadata_store_id TEXT NOT NULL,
			local_block_store_id TEXT NOT NULL,
			remote_block_store_id TEXT,
			read_only BOOLEAN DEFAULT FALSE,
			enabled BOOLEAN DEFAULT TRUE NOT NULL,
			encrypt_data BOOLEAN DEFAULT FALSE,
			acl_flag_inherited_canonicalization BOOLEAN DEFAULT TRUE NOT NULL,
			access_based_enumeration BOOLEAN DEFAULT FALSE,
			default_permission TEXT DEFAULT 'read-write',
			config TEXT,
			blocked_operations TEXT,
			retention_policy TEXT DEFAULT '',
			retention_ttl INTEGER DEFAULT 0,
			local_store_size INTEGER DEFAULT 0,
			read_buffer_size INTEGER DEFAULT 0,
			quota_bytes INTEGER DEFAULT 0,
			created_at DATETIME,
			updated_at DATETIME
		)
	`).Error; err != nil {
		t.Fatalf("recreate shares: %v", err)
	}

	// Insert a legacy row with NULL access_based_enumeration.
	if err := db.Exec(`
		INSERT INTO shares (id, name, metadata_store_id, local_block_store_id, access_based_enumeration)
		VALUES (?, ?, ?, ?, NULL)
	`, "legacy-id", "/legacy", "meta-id", "local-id").Error; err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Verify the row is NULL before backfill (sanity check).
	var pre *bool
	if err := db.Raw("SELECT access_based_enumeration FROM shares WHERE name = ?", "/legacy").Scan(&pre).Error; err != nil {
		t.Fatalf("pre-backfill scan: %v", err)
	}
	if pre != nil {
		t.Fatalf("setup: expected NULL, got %v", *pre)
	}

	// Run the backfill (mirrors gorm.go).
	if err := db.Exec(
		"UPDATE shares SET access_based_enumeration = ? WHERE access_based_enumeration IS NULL",
		false,
	).Error; err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var post *bool
	if err := db.Raw("SELECT access_based_enumeration FROM shares WHERE name = ?", "/legacy").Scan(&post).Error; err != nil {
		t.Fatalf("post-backfill scan: %v", err)
	}
	if post == nil {
		t.Fatalf("backfill left value NULL")
	}
	if *post {
		t.Fatalf("backfill set true, want false")
	}
}
