package memory

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/identity"
)

func TestNewMemoryIdentityStore(t *testing.T) {
	store := NewMemoryIdentityStore()
	if store == nil {
		t.Fatal("Expected store to be non-nil")
	}
}

func TestCreateUser(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	user := &identity.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     identity.RoleUser,
	}

	err := store.CreateUser(ctx, user)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Try to create duplicate
	err = store.CreateUser(ctx, user)
	if err != identity.ErrDuplicateUser {
		t.Errorf("Expected ErrDuplicateUser, got: %v", err)
	}
}

func TestGetUser(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	user := &identity.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     identity.RoleUser,
		Groups:   []string{"developers"},
	}
	_ = store.CreateUser(ctx, user)

	// Get existing user
	retrieved, err := store.GetUser("testuser")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if retrieved.Username != "testuser" {
		t.Errorf("Expected username 'testuser', got '%s'", retrieved.Username)
	}
	if len(retrieved.Groups) != 1 || retrieved.Groups[0] != "developers" {
		t.Errorf("Expected groups ['developers'], got %v", retrieved.Groups)
	}

	// Get non-existing user
	_, err = store.GetUser("nonexistent")
	if err != identity.ErrUserNotFound {
		t.Errorf("Expected ErrUserNotFound, got: %v", err)
	}
}

func TestGetUserByID(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	user := &identity.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     identity.RoleUser,
	}
	_ = store.CreateUser(ctx, user)

	// Get by ID
	retrieved, err := store.GetUserByID("test-uuid")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if retrieved.Username != "testuser" {
		t.Errorf("Expected username 'testuser', got '%s'", retrieved.Username)
	}

	// Get by non-existing ID
	_, err = store.GetUserByID("nonexistent")
	if err != identity.ErrUserNotFound {
		t.Errorf("Expected ErrUserNotFound, got: %v", err)
	}
}

func TestListUsers(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	// Empty list
	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("Expected empty list, got %d users", len(users))
	}

	// Add users
	_ = store.CreateUser(ctx, &identity.User{ID: "1", Username: "user1"})
	_ = store.CreateUser(ctx, &identity.User{ID: "2", Username: "user2"})

	users, err = store.ListUsers()
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("Expected 2 users, got %d", len(users))
	}
}

func TestUpdateUser(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	user := &identity.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     identity.RoleUser,
	}
	_ = store.CreateUser(ctx, user)

	// Update user
	user.Role = identity.RoleAdmin
	err := store.UpdateUser(ctx, user)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify update
	retrieved, _ := store.GetUser("testuser")
	if retrieved.Role != identity.RoleAdmin {
		t.Errorf("Expected role 'admin', got '%s'", retrieved.Role)
	}

	// Update non-existing user
	err = store.UpdateUser(ctx, &identity.User{Username: "nonexistent"})
	if err != identity.ErrUserNotFound {
		t.Errorf("Expected ErrUserNotFound, got: %v", err)
	}
}

func TestDeleteUser(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	user := &identity.User{
		ID:       "test-uuid",
		Username: "testuser",
		Role:     identity.RoleUser,
	}
	_ = store.CreateUser(ctx, user)

	// Delete user
	err := store.DeleteUser(ctx, "testuser")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify deletion
	_, err = store.GetUser("testuser")
	if err != identity.ErrUserNotFound {
		t.Errorf("Expected ErrUserNotFound, got: %v", err)
	}

	// Delete non-existing user
	err = store.DeleteUser(ctx, "nonexistent")
	if err != identity.ErrUserNotFound {
		t.Errorf("Expected ErrUserNotFound, got: %v", err)
	}
}

func TestValidateCredentials(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	passwordHash, _ := identity.HashPassword("testpassword")
	user := &identity.User{
		ID:           "test-uuid",
		Username:     "testuser",
		PasswordHash: passwordHash,
		Enabled:      true,
	}
	_ = store.CreateUser(ctx, user)

	// Valid credentials
	validated, err := store.ValidateCredentials("testuser", "testpassword")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if validated.Username != "testuser" {
		t.Errorf("Expected username 'testuser', got '%s'", validated.Username)
	}

	// Invalid password
	_, err = store.ValidateCredentials("testuser", "wrongpassword")
	if err != identity.ErrInvalidCredentials {
		t.Errorf("Expected ErrInvalidCredentials, got: %v", err)
	}

	// Non-existing user
	_, err = store.ValidateCredentials("nonexistent", "password")
	if err != identity.ErrUserNotFound {
		t.Errorf("Expected ErrUserNotFound, got: %v", err)
	}

	// Disabled user
	user.Enabled = false
	_ = store.UpdateUser(ctx, user)
	_, err = store.ValidateCredentials("testuser", "testpassword")
	if err != identity.ErrUserDisabled {
		t.Errorf("Expected ErrUserDisabled, got: %v", err)
	}
}

func TestUpdatePassword(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	oldHash, _ := identity.HashPassword("oldpassword")
	user := &identity.User{
		ID:           "test-uuid",
		Username:     "testuser",
		PasswordHash: oldHash,
		Enabled:      true,
	}
	_ = store.CreateUser(ctx, user)

	// Update password
	newHash, _ := identity.HashPassword("newpassword")
	err := store.UpdatePassword(ctx, "testuser", newHash, "nthash")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify new password works
	_, err = store.ValidateCredentials("testuser", "newpassword")
	if err != nil {
		t.Errorf("Expected valid credentials, got: %v", err)
	}

	// Verify old password doesn't work
	_, err = store.ValidateCredentials("testuser", "oldpassword")
	if err != identity.ErrInvalidCredentials {
		t.Errorf("Expected ErrInvalidCredentials, got: %v", err)
	}
}

func TestUpdateLastLogin(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	user := &identity.User{
		ID:       "test-uuid",
		Username: "testuser",
	}
	_ = store.CreateUser(ctx, user)

	// Update last login
	loginTime := time.Now()
	err := store.UpdateLastLogin(ctx, "testuser", loginTime)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify
	retrieved, _ := store.GetUser("testuser")
	if !retrieved.LastLogin.Equal(loginTime) {
		t.Errorf("Expected LastLogin %v, got %v", loginTime, retrieved.LastLogin)
	}
}

func TestGroupOperations(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	// Create group
	group := &identity.Group{
		Name:        "developers",
		Description: "Development team",
	}
	err := store.CreateGroup(ctx, group)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Create duplicate
	err = store.CreateGroup(ctx, group)
	if err != identity.ErrDuplicateGroup {
		t.Errorf("Expected ErrDuplicateGroup, got: %v", err)
	}

	// Get group
	retrieved, err := store.GetGroup("developers")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if retrieved.Name != "developers" {
		t.Errorf("Expected name 'developers', got '%s'", retrieved.Name)
	}

	// List groups
	groups, _ := store.ListGroups()
	if len(groups) != 1 {
		t.Errorf("Expected 1 group, got %d", len(groups))
	}

	// Update group
	group.Description = "Updated description"
	err = store.UpdateGroup(ctx, group)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Delete group
	err = store.DeleteGroup(ctx, "developers")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	_, err = store.GetGroup("developers")
	if err != identity.ErrGroupNotFound {
		t.Errorf("Expected ErrGroupNotFound, got: %v", err)
	}
}

func TestUserGroupMembership(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	// Create group and user
	_ = store.CreateGroup(ctx, &identity.Group{Name: "developers"})
	_ = store.CreateUser(ctx, &identity.User{ID: "1", Username: "testuser"})

	// Add user to group
	err := store.AddUserToGroup(ctx, "testuser", "developers")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify membership
	user, _ := store.GetUser("testuser")
	if len(user.Groups) != 1 || user.Groups[0] != "developers" {
		t.Errorf("Expected user to be in 'developers' group, got %v", user.Groups)
	}

	// Get user groups
	groups, _ := store.GetUserGroups("testuser")
	if len(groups) != 1 || groups[0].Name != "developers" {
		t.Errorf("Expected user to be in 'developers' group")
	}

	// Remove user from group
	err = store.RemoveUserFromGroup(ctx, "testuser", "developers")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	user, _ = store.GetUser("testuser")
	if len(user.Groups) != 0 {
		t.Errorf("Expected user to have no groups, got %v", user.Groups)
	}
}

func TestShareIdentityMapping(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	// Create user first
	_ = store.CreateUser(ctx, &identity.User{ID: "1", Username: "testuser"})

	// Set mapping
	mapping := &identity.ShareIdentityMapping{
		Username:  "testuser",
		ShareName: "/export",
		UID:       1000,
		GID:       1000,
		GIDs:      []uint32{1001, 1002},
	}
	err := store.SetShareIdentityMapping(ctx, mapping)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Get mapping
	retrieved, err := store.GetShareIdentityMapping("testuser", "/export")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if retrieved.UID != 1000 {
		t.Errorf("Expected UID 1000, got %d", retrieved.UID)
	}
	if len(retrieved.GIDs) != 2 {
		t.Errorf("Expected 2 GIDs, got %d", len(retrieved.GIDs))
	}

	// List mappings
	mappings, _ := store.ListUserShareMappings("testuser")
	if len(mappings) != 1 {
		t.Errorf("Expected 1 mapping, got %d", len(mappings))
	}

	// Delete mapping
	err = store.DeleteShareIdentityMapping(ctx, "testuser", "/export")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	retrieved, err = store.GetShareIdentityMapping("testuser", "/export")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if retrieved != nil {
		t.Error("Expected nil mapping after delete")
	}
}

func TestEnsureAdminUser(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	// First call should create admin with password
	password, err := store.EnsureAdminUser(ctx)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if password == "" {
		t.Error("Expected non-empty initial password")
	}

	// Verify admin exists
	admin, err := store.GetUser(identity.AdminUsername)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if admin.Role != identity.RoleAdmin {
		t.Errorf("Expected role 'admin', got '%s'", admin.Role)
	}
	if !admin.MustChangePassword {
		t.Error("Expected MustChangePassword to be true")
	}

	// Second call should not return password
	password2, err := store.EnsureAdminUser(ctx)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if password2 != "" {
		t.Errorf("Expected empty password on second call, got '%s'", password2)
	}
}

func TestIsAdminInitialized(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	// Before initialization
	initialized, _ := store.IsAdminInitialized(ctx)
	if initialized {
		t.Error("Expected admin to not be initialized initially")
	}

	// After initialization
	_, _ = store.EnsureAdminUser(ctx)
	initialized, _ = store.IsAdminInitialized(ctx)
	if !initialized {
		t.Error("Expected admin to be initialized after EnsureAdminUser")
	}
}

func TestHealthcheck(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	err := store.Healthcheck(ctx)
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
}

func TestClose(t *testing.T) {
	store := NewMemoryIdentityStore()

	err := store.Close()
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
}

func TestResolveSharePermission(t *testing.T) {
	store := NewMemoryIdentityStore()
	ctx := context.Background()

	// Create group with permission
	_ = store.CreateGroup(ctx, &identity.Group{
		Name: "developers",
		SharePermissions: map[string]identity.SharePermission{
			"/export": identity.PermissionReadWrite,
		},
	})

	// Create user in group
	user := &identity.User{
		ID:       "1",
		Username: "testuser",
		Groups:   []string{"developers"},
	}
	_ = store.CreateUser(ctx, user)

	// Resolve from group
	perm := store.ResolveSharePermission(user, "/export", identity.PermissionNone)
	if perm != identity.PermissionReadWrite {
		t.Errorf("Expected PermissionReadWrite, got %s", perm)
	}

	// User explicit permission overrides group
	user.SharePermissions = map[string]identity.SharePermission{
		"/export": identity.PermissionAdmin,
	}
	_ = store.UpdateUser(ctx, user)
	user, _ = store.GetUser("testuser")

	perm = store.ResolveSharePermission(user, "/export", identity.PermissionNone)
	if perm != identity.PermissionAdmin {
		t.Errorf("Expected PermissionAdmin, got %s", perm)
	}

	// Default when no permission
	perm = store.ResolveSharePermission(user, "/other", identity.PermissionRead)
	if perm != identity.PermissionRead {
		t.Errorf("Expected PermissionRead (default), got %s", perm)
	}
}
