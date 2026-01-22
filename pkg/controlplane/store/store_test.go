//go:build integration

package store

import (
	"context"
	"errors"
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

func TestShareOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create prerequisite stores
	metaStore := &models.MetadataStoreConfig{Name: "test-meta", Type: "memory"}
	metaStoreID, _ := store.CreateMetadataStore(ctx, metaStore)
	payloadStore := &models.PayloadStoreConfig{Name: "test-payload", Type: "memory"}
	payloadStoreID, _ := store.CreatePayloadStore(ctx, payloadStore)

	t.Run("create share", func(t *testing.T) {
		share := &models.Share{
			Name:            "/export",
			MetadataStoreID: metaStoreID,
			PayloadStoreID:  payloadStoreID,
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
			Name:            "/export",
			MetadataStoreID: metaStoreID,
			PayloadStoreID:  payloadStoreID,
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
	payloadStore := &models.PayloadStoreConfig{Name: "perm-payload", Type: "memory"}
	payloadStoreID, _ := store.CreatePayloadStore(ctx, payloadStore)
	share := &models.Share{
		Name:            "/permshare",
		MetadataStoreID: metaStoreID,
		PayloadStoreID:  payloadStoreID,
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

func TestPayloadStoreOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("create payload store", func(t *testing.T) {
		payloadStore := &models.PayloadStoreConfig{
			Name:   "payload-store",
			Type:   "memory",
			Config: `{}`,
		}

		id, err := store.CreatePayloadStore(ctx, payloadStore)
		if err != nil {
			t.Fatalf("failed to create payload store: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}
	})

	t.Run("duplicate payload store fails", func(t *testing.T) {
		payloadStore := &models.PayloadStoreConfig{Name: "payload-store", Type: "memory"}
		_, err := store.CreatePayloadStore(ctx, payloadStore)
		if !errors.Is(err, models.ErrDuplicateStore) {
			t.Errorf("expected ErrDuplicateStore, got %v", err)
		}
	})

	t.Run("get payload store", func(t *testing.T) {
		payloadStore, err := store.GetPayloadStore(ctx, "payload-store")
		if err != nil {
			t.Fatalf("failed to get payload store: %v", err)
		}
		if payloadStore.Name != "payload-store" {
			t.Errorf("expected name 'payload-store', got %q", payloadStore.Name)
		}
	})

	t.Run("list payload stores", func(t *testing.T) {
		stores, err := store.ListPayloadStores(ctx)
		if err != nil {
			t.Fatalf("failed to list stores: %v", err)
		}
		if len(stores) < 1 {
			t.Error("expected at least 1 store")
		}
	})
}

func TestEnsureAdminUser(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("creates admin if not exists", func(t *testing.T) {
		password, err := store.EnsureAdminUser(ctx)
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
	})

	t.Run("second call returns empty password", func(t *testing.T) {
		password, err := store.EnsureAdminUser(ctx)
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
		t.Error("expected non-empty DSN")
	}
	// Check that all parts are present
	if !contains(dsn, "host=localhost") {
		t.Error("DSN should contain host")
	}
	if !contains(dsn, "port=5432") {
		t.Error("DSN should contain port")
	}
	if !contains(dsn, "sslmode=require") {
		t.Error("DSN should contain sslmode")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(s) > len(substr) && (s[:len(substr)] == substr ||
			s[len(s)-len(substr):] == substr ||
			containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 1; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
