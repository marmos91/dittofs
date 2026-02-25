package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// =============================================================================
// Test Helper Functions
// =============================================================================

// buildTreeConnectRequestBody builds a TREE_CONNECT request body for the given share path.
// The path is encoded as UTF-16LE and placed after the fixed 8-byte header.
func buildTreeConnectRequestBody(sharePath string) []byte {
	// Encode share path as UTF-16LE
	pathBytes := encodeUTF16LE(sharePath)

	// Fixed size: 8 bytes + path
	body := make([]byte, 8+len(pathBytes))

	// StructureSize at offset 0 (always 9)
	binary.LittleEndian.PutUint16(body[0:2], 9)

	// Reserved/Flags at offset 2 (set to 0)
	binary.LittleEndian.PutUint16(body[2:4], 0)

	// PathOffset at offset 4 (64 header + 8 fixed = 72)
	binary.LittleEndian.PutUint16(body[4:6], 72)

	// PathLength at offset 6
	binary.LittleEndian.PutUint16(body[6:8], uint16(len(pathBytes)))

	// Path starts at offset 8 in body
	if len(pathBytes) > 0 {
		copy(body[8:], pathBytes)
	}

	return body
}

// newTreeConnectTestContext creates a test context with the given session ID.
func newTreeConnectTestContext(sessionID uint64) *SMBHandlerContext {
	return NewSMBHandlerContext(
		context.Background(),
		"127.0.0.1:12345",
		sessionID,
		0,
		1,
	)
}

// =============================================================================
// IPC$ Share Tests
// =============================================================================

func TestTreeConnect_IPCShare(t *testing.T) {
	t.Run("AcceptsIPCShareUppercase", func(t *testing.T) {
		h := NewHandler()

		// Create a valid session first
		sess := h.CreateSession("127.0.0.1:12345", false, "testuser", "")
		ctx := newTreeConnectTestContext(sess.SessionID)

		body := buildTreeConnectRequestBody("\\\\server\\IPC$")

		result, err := h.TreeConnect(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Status = 0x%x, expected StatusSuccess (0x%x)",
				result.Status, types.StatusSuccess)
		}

		// Verify response format (16 bytes)
		if len(result.Data) != 16 {
			t.Fatalf("Response should be 16 bytes, got %d", len(result.Data))
		}

		// Verify StructureSize
		structSize := binary.LittleEndian.Uint16(result.Data[0:2])
		if structSize != 16 {
			t.Errorf("StructureSize = %d, expected 16", structSize)
		}

		// Verify ShareType is PIPE (0x02)
		shareType := result.Data[2]
		if shareType != types.SMB2ShareTypePipe {
			t.Errorf("ShareType = 0x%x, expected 0x%x (PIPE)",
				shareType, types.SMB2ShareTypePipe)
		}

		// Verify MaximalAccess
		maxAccess := binary.LittleEndian.Uint32(result.Data[12:16])
		if maxAccess != ipcMaximalAccess {
			t.Errorf("MaximalAccess = 0x%x, expected 0x%x",
				maxAccess, ipcMaximalAccess)
		}

		// Verify tree connection was stored
		tree, ok := h.GetTree(ctx.TreeID)
		if !ok {
			t.Fatal("Tree connection should be stored")
		}

		if tree.ShareName != "/ipc$" {
			t.Errorf("ShareName = %q, expected %q", tree.ShareName, "/ipc$")
		}

		if tree.ShareType != types.SMB2ShareTypePipe {
			t.Errorf("ShareType = 0x%x, expected 0x%x (PIPE)",
				tree.ShareType, types.SMB2ShareTypePipe)
		}

		if tree.Permission != models.PermissionReadWrite {
			t.Errorf("Permission = %v, expected %v",
				tree.Permission, models.PermissionReadWrite)
		}
	})

	t.Run("AcceptsIPCShareLowercase", func(t *testing.T) {
		h := NewHandler()

		// Create a valid session first
		sess := h.CreateSession("127.0.0.1:12345", false, "testuser", "")
		ctx := newTreeConnectTestContext(sess.SessionID)

		body := buildTreeConnectRequestBody("\\\\server\\ipc$")

		result, err := h.TreeConnect(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
		}
	})

	t.Run("AcceptsIPCShareMixedCase", func(t *testing.T) {
		h := NewHandler()

		// Create a valid session first
		sess := h.CreateSession("127.0.0.1:12345", false, "testuser", "")
		ctx := newTreeConnectTestContext(sess.SessionID)

		body := buildTreeConnectRequestBody("\\\\server\\Ipc$")

		result, err := h.TreeConnect(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusSuccess {
			t.Errorf("Status = 0x%x, expected StatusSuccess", result.Status)
		}
	})

	t.Run("RejectsIPCShareWithoutSession", func(t *testing.T) {
		h := NewHandler()

		// Do not create a session - use an invalid session ID
		ctx := newTreeConnectTestContext(99999)

		body := buildTreeConnectRequestBody("\\\\server\\IPC$")

		result, err := h.TreeConnect(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusUserSessionDeleted {
			t.Errorf("Status = 0x%x, expected StatusUserSessionDeleted (0x%x)",
				result.Status, types.StatusUserSessionDeleted)
		}
	})

	t.Run("UpdatesContextForIPCShare", func(t *testing.T) {
		h := NewHandler()

		// Create a valid session first
		sess := h.CreateSession("127.0.0.1:12345", false, "testuser", "")
		ctx := newTreeConnectTestContext(sess.SessionID)

		body := buildTreeConnectRequestBody("\\\\server\\IPC$")

		_, err := h.TreeConnect(ctx, body)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Verify context was updated
		if ctx.TreeID == 0 {
			t.Error("TreeID should be set in context")
		}

		if ctx.ShareName != "/ipc$" {
			t.Errorf("ShareName = %q, expected %q", ctx.ShareName, "/ipc$")
		}
	})
}

// =============================================================================
// parseSharePath Tests
// =============================================================================

func TestParseSharePath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "UNCPath",
			input:    "\\\\server\\share",
			expected: "/share",
		},
		{
			name:     "UNCPathUppercase",
			input:    "\\\\SERVER\\SHARE",
			expected: "/share",
		},
		{
			name:     "UNCPathMixed",
			input:    "\\\\Server\\ShareName",
			expected: "/sharename",
		},
		{
			name:     "UNCPathIPC",
			input:    "\\\\server\\IPC$",
			expected: "/ipc$",
		},
		{
			name:     "SlashPath",
			input:    "/export",
			expected: "/export",
		},
		{
			name:     "NoServerPart",
			input:    "share",
			expected: "/share",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSharePath(tt.input)
			if result != tt.expected {
				t.Errorf("parseSharePath(%q) = %q, expected %q",
					tt.input, result, tt.expected)
			}
		})
	}
}

// =============================================================================
// calculateMaximalAccess Tests
// =============================================================================

func TestCalculateMaximalAccess(t *testing.T) {
	t.Run("AdminPermission", func(t *testing.T) {
		access := calculateMaximalAccess(models.PermissionAdmin)
		// Full access = 0x001F01FF
		expected := uint32(0x001F01FF)
		if access != expected {
			t.Errorf("calculateMaximalAccess(Admin) = 0x%x, expected 0x%x",
				access, expected)
		}
	})

	t.Run("ReadWritePermission", func(t *testing.T) {
		access := calculateMaximalAccess(models.PermissionReadWrite)
		// Should include read, write, and delete rights
		if access == 0 {
			t.Error("ReadWrite permission should grant non-zero access")
		}
	})

	t.Run("ReadPermission", func(t *testing.T) {
		access := calculateMaximalAccess(models.PermissionRead)
		// Should grant read-only access
		if access == 0 {
			t.Error("Read permission should grant non-zero access")
		}
	})

	t.Run("NonePermission", func(t *testing.T) {
		access := calculateMaximalAccess(models.PermissionNone)
		if access != 0 {
			t.Errorf("calculateMaximalAccess(None) = 0x%x, expected 0", access)
		}
	})
}

// =============================================================================
// TreeConnect Request Validation Tests
// =============================================================================

func TestTreeConnect_RequestValidation(t *testing.T) {
	t.Run("RejectsTooShortBody", func(t *testing.T) {
		h := NewHandler()
		ctx := newTreeConnectTestContext(1)

		// Body less than 9 bytes should be rejected
		shortBody := make([]byte, 8)

		result, err := h.TreeConnect(ctx, shortBody)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if result.Status != types.StatusInvalidParameter {
			t.Errorf("Status = 0x%x, expected StatusInvalidParameter",
				result.Status)
		}
	})
}

// =============================================================================
// Constants Tests
// =============================================================================

func TestTreeConnectConstants(t *testing.T) {
	t.Run("FixedSize", func(t *testing.T) {
		if treeConnectFixedSize != 8 {
			t.Errorf("treeConnectFixedSize = %d, expected 8", treeConnectFixedSize)
		}
	})

	t.Run("IPCMaximalAccess", func(t *testing.T) {
		// ipcMaximalAccess should be 0x1F (FILE_READ_DATA | FILE_WRITE_DATA |
		// FILE_APPEND_DATA | FILE_READ_EA | FILE_WRITE_EA)
		expected := uint32(0x1F)
		if ipcMaximalAccess != expected {
			t.Errorf("ipcMaximalAccess = 0x%x, expected 0x%x",
				ipcMaximalAccess, expected)
		}
	})
}

// =============================================================================
// Root User Bypass Tests
// =============================================================================

func TestIsRootUser(t *testing.T) {
	t.Run("NilUser", func(t *testing.T) {
		if isRootUser(nil) {
			t.Error("nil user should not be root")
		}
	})

	t.Run("UserWithNilUID", func(t *testing.T) {
		user := &models.User{Username: "test"}
		if isRootUser(user) {
			t.Error("user with nil UID should not be root")
		}
	})

	t.Run("UserWithNonZeroUID", func(t *testing.T) {
		uid := uint32(1000)
		user := &models.User{Username: "test", UID: &uid}
		if isRootUser(user) {
			t.Error("user with UID 1000 should not be root")
		}
	})

	t.Run("UserWithZeroUID", func(t *testing.T) {
		uid := uint32(0)
		user := &models.User{Username: "admin", UID: &uid}
		if !isRootUser(user) {
			t.Error("user with UID 0 should be root")
		}
	})
}

func TestRootHasAdminAccess(t *testing.T) {
	t.Run("NilShare", func(t *testing.T) {
		if rootHasAdminAccess(nil) {
			t.Error("nil share should not allow root admin access")
		}
	})

	t.Run("EmptySquash", func(t *testing.T) {
		share := &runtime.Share{Name: "/test", Squash: ""}
		if !rootHasAdminAccess(share) {
			t.Error("empty squash should allow root admin access")
		}
	})

	t.Run("SquashNone", func(t *testing.T) {
		share := &runtime.Share{Name: "/test", Squash: models.SquashNone}
		if !rootHasAdminAccess(share) {
			t.Error("SquashNone should allow root admin access")
		}
	})

	t.Run("SquashRootToAdmin", func(t *testing.T) {
		share := &runtime.Share{Name: "/test", Squash: models.SquashRootToAdmin}
		if !rootHasAdminAccess(share) {
			t.Error("SquashRootToAdmin should allow root admin access")
		}
	})

	t.Run("SquashAllToAdmin", func(t *testing.T) {
		share := &runtime.Share{Name: "/test", Squash: models.SquashAllToAdmin}
		if !rootHasAdminAccess(share) {
			t.Error("SquashAllToAdmin should allow root admin access")
		}
	})

	t.Run("SquashRootToGuest", func(t *testing.T) {
		share := &runtime.Share{Name: "/test", Squash: models.SquashRootToGuest}
		if rootHasAdminAccess(share) {
			t.Error("SquashRootToGuest should not allow root admin access")
		}
	})

	t.Run("SquashAllToGuest", func(t *testing.T) {
		share := &runtime.Share{Name: "/test", Squash: models.SquashAllToGuest}
		if rootHasAdminAccess(share) {
			t.Error("SquashAllToGuest should not allow root admin access")
		}
	})
}

func TestResolveSharePermission_RootBypass(t *testing.T) {
	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:12345", 1, 0, 1)

	t.Run("RootUserGetsAdminPermission", func(t *testing.T) {
		uid := uint32(0)
		user := &models.User{Username: "admin", UID: &uid}
		sess := session.NewSessionWithUser(1, "127.0.0.1", user, "")
		share := &runtime.Share{Name: "/export", Squash: "", DefaultPermission: "read-write"}
		defaultPerm := models.PermissionReadWrite

		perm, username := resolveSharePermission(ctx, sess, share, defaultPerm, nil)

		if perm != models.PermissionAdmin {
			t.Errorf("Root user should get PermissionAdmin, got %v", perm)
		}
		if username != "admin" {
			t.Errorf("Username should be 'admin', got %q", username)
		}
	})

	t.Run("RootUserWithRootToGuestSquash", func(t *testing.T) {
		uid := uint32(0)
		user := &models.User{Username: "admin", UID: &uid}
		sess := session.NewSessionWithUser(1, "127.0.0.1", user, "")
		share := &runtime.Share{Name: "/export", Squash: models.SquashRootToGuest, DefaultPermission: "read-write"}
		defaultPerm := models.PermissionReadWrite

		perm, _ := resolveSharePermission(ctx, sess, share, defaultPerm, nil)

		// Root bypass should NOT apply when squash mode denies root
		// Without userStore, it falls back to default permission
		if perm != models.PermissionReadWrite {
			t.Errorf("Root user with root_to_guest squash should get default permission (read-write), got %v", perm)
		}
	})

	t.Run("NonRootUserDoesNotGetAdminBypass", func(t *testing.T) {
		uid := uint32(1000)
		user := &models.User{Username: "user", UID: &uid}
		sess := session.NewSessionWithUser(1, "127.0.0.1", user, "")
		share := &runtime.Share{Name: "/export", Squash: "", DefaultPermission: "read-write"}
		defaultPerm := models.PermissionReadWrite

		perm, _ := resolveSharePermission(ctx, sess, share, defaultPerm, nil)

		// Non-root user should NOT get admin bypass
		// Without userStore, it falls back to default permission
		if perm == models.PermissionAdmin {
			t.Error("Non-root user should not get PermissionAdmin from root bypass")
		}
	})

	t.Run("GuestSessionUsesDefaultPermission", func(t *testing.T) {
		sess := session.NewSession(1, "127.0.0.1", true, "guest", "")
		share := &runtime.Share{Name: "/export", Squash: "", DefaultPermission: "read"}
		defaultPerm := models.PermissionRead

		perm, username := resolveSharePermission(ctx, sess, share, defaultPerm, nil)

		if perm != models.PermissionRead {
			t.Errorf("Guest session should get default permission, got %v", perm)
		}
		if username != "guest" {
			t.Errorf("Username should be 'guest', got %q", username)
		}
	})
}
