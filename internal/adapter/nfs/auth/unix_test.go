package auth

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// =============================================================================
// Mock IdentityStore for testing
// =============================================================================

type mockIdentityStore struct {
	usersByUID map[uint32]*models.User
}

func newMockIdentityStore() *mockIdentityStore {
	return &mockIdentityStore{usersByUID: make(map[uint32]*models.User)}
}

func (m *mockIdentityStore) addUserByUID(uid uint32, user *models.User) {
	m.usersByUID[uid] = user
}

func (m *mockIdentityStore) GetUser(_ context.Context, _ string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}

func (m *mockIdentityStore) ValidateCredentials(_ context.Context, _, _ string) (*models.User, error) {
	return nil, models.ErrInvalidCredentials
}

func (m *mockIdentityStore) ListUsers(_ context.Context) ([]*models.User, error) {
	return nil, nil
}

func (m *mockIdentityStore) GetGuestUser(_ context.Context, _ string) (*models.User, error) {
	return nil, errors.New("guest disabled")
}

func (m *mockIdentityStore) GetGroup(_ context.Context, _ string) (*models.Group, error) {
	return nil, models.ErrGroupNotFound
}

func (m *mockIdentityStore) ListGroups(_ context.Context) ([]*models.Group, error) {
	return nil, nil
}

func (m *mockIdentityStore) GetUserGroups(_ context.Context, _ string) ([]*models.Group, error) {
	return nil, nil
}

func (m *mockIdentityStore) ResolveSharePermission(_ context.Context, _ *models.User, _ string) (models.SharePermission, error) {
	return "", nil
}

func (m *mockIdentityStore) GetUserByUID(_ context.Context, uid uint32) (*models.User, error) {
	user, ok := m.usersByUID[uid]
	if !ok {
		return nil, models.ErrUserNotFound
	}
	return user, nil
}

func (m *mockIdentityStore) GetUserByID(_ context.Context, _ string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}

func (m *mockIdentityStore) IsGuestEnabled(_ context.Context, _ string) bool {
	return false
}

// =============================================================================
// Interface Conformance
// =============================================================================

func TestUnixAuthenticator_ImplementsAuthenticator(t *testing.T) {
	var _ adapter.Authenticator = (*UnixAuthenticator)(nil)
}

// =============================================================================
// Constructor Tests
// =============================================================================

func TestNewUnixAuthenticator(t *testing.T) {
	t.Run("WithIdentityStore", func(t *testing.T) {
		store := newMockIdentityStore()
		auth := NewUnixAuthenticator(store)
		if auth == nil {
			t.Fatal("NewUnixAuthenticator returned nil")
		}
	})

	t.Run("WithNilStore", func(t *testing.T) {
		auth := NewUnixAuthenticator(nil)
		if auth == nil {
			t.Fatal("NewUnixAuthenticator returned nil")
		}
	})
}

// =============================================================================
// Authenticate - Valid AUTH_UNIX Bytes
// =============================================================================

func TestUnixAuthenticator_Authenticate_ValidCredentials(t *testing.T) {
	auth := NewUnixAuthenticator(nil)

	token := buildAuthUnixToken(1000, 1000, "testhost", []uint32{100, 200})
	result, challenge, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if challenge != nil {
		t.Error("AUTH_UNIX should never return a challenge")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.IsGuest {
		t.Error("AUTH_UNIX should not produce guest result")
	}
	if result.User == nil {
		t.Fatal("expected non-nil user")
	}
	if result.User.UID == nil || *result.User.UID != 1000 {
		t.Error("expected UID 1000")
	}
	if result.User.GID == nil || *result.User.GID != 1000 {
		t.Error("expected GID 1000")
	}
}

func TestUnixAuthenticator_Authenticate_ResolvesKnownUID(t *testing.T) {
	store := newMockIdentityStore()
	uid := uint32(1000)
	gid := uint32(1000)
	user := &models.User{
		ID:       "known-user-id",
		Username: "alice",
		Enabled:  true,
		UID:      &uid,
		GID:      &gid,
	}
	store.addUserByUID(1000, user)

	auth := NewUnixAuthenticator(store)

	token := buildAuthUnixToken(1000, 1000, "client-host", nil)
	result, _, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.User == nil {
		t.Fatal("expected non-nil user")
	}
	if result.User.Username != "alice" {
		t.Errorf("expected username 'alice', got %q", result.User.Username)
	}
	if result.User.ID != "known-user-id" {
		t.Errorf("expected real user ID, got %q", result.User.ID)
	}
}

func TestUnixAuthenticator_Authenticate_UnknownUID_SyntheticUser(t *testing.T) {
	store := newMockIdentityStore()
	auth := NewUnixAuthenticator(store)

	token := buildAuthUnixToken(9999, 9999, "unknown-host", nil)
	result, _, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.User == nil {
		t.Fatal("expected synthetic user")
	}
	if result.User.Username != "unix:9999" {
		t.Errorf("expected synthetic username 'unix:9999', got %q", result.User.Username)
	}
	if result.User.UID == nil || *result.User.UID != 9999 {
		t.Error("expected UID 9999 on synthetic user")
	}
}

func TestUnixAuthenticator_Authenticate_RootUID(t *testing.T) {
	auth := NewUnixAuthenticator(nil)

	token := buildAuthUnixToken(0, 0, "root-host", []uint32{0})
	result, _, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.User.UID == nil || *result.User.UID != 0 {
		t.Error("expected UID 0 for root")
	}
}

// =============================================================================
// Authenticate - Error Cases
// =============================================================================

func TestUnixAuthenticator_Authenticate_EmptyToken(t *testing.T) {
	auth := NewUnixAuthenticator(nil)

	_, _, err := auth.Authenticate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestUnixAuthenticator_Authenticate_EmptyBytes(t *testing.T) {
	auth := NewUnixAuthenticator(nil)

	_, _, err := auth.Authenticate(context.Background(), []byte{})
	if err == nil {
		t.Fatal("expected error for empty bytes")
	}
}

func TestUnixAuthenticator_Authenticate_TooShort(t *testing.T) {
	auth := NewUnixAuthenticator(nil)

	// Only 4 bytes (just the stamp, missing everything else)
	_, _, err := auth.Authenticate(context.Background(), []byte{0x00, 0x00, 0x00, 0x01})
	if err == nil {
		t.Fatal("expected error for too-short token")
	}
}

func TestUnixAuthenticator_Authenticate_InvalidFormat(t *testing.T) {
	auth := NewUnixAuthenticator(nil)

	// Corrupt data: valid stamp but absurd machine name length
	token := make([]byte, 12)
	binary.BigEndian.PutUint32(token[0:4], 1)     // stamp
	binary.BigEndian.PutUint32(token[4:8], 10000)  // machine name length (too large)
	binary.BigEndian.PutUint32(token[8:12], 0)     // garbage

	_, _, err := auth.Authenticate(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
}

// =============================================================================
// Never Returns ErrMoreProcessingRequired
// =============================================================================

func TestUnixAuthenticator_NeverReturnsMoreProcessingRequired(t *testing.T) {
	auth := NewUnixAuthenticator(nil)

	token := buildAuthUnixToken(1000, 1000, "host", nil)
	_, _, err := auth.Authenticate(context.Background(), token)

	if errors.Is(err, adapter.ErrMoreProcessingRequired) {
		t.Fatal("AUTH_UNIX should never return ErrMoreProcessingRequired")
	}
}

// =============================================================================
// Helper: Build AUTH_UNIX Token
// =============================================================================

// buildAuthUnixToken creates XDR-encoded AUTH_UNIX credentials for testing.
func buildAuthUnixToken(uid, gid uint32, machineName string, gids []uint32) []byte {
	var buf []byte

	// Stamp (4 bytes)
	stampBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(stampBytes, 0)
	buf = append(buf, stampBytes...)

	// Machine name (XDR string: length + data + padding)
	nameLen := make([]byte, 4)
	binary.BigEndian.PutUint32(nameLen, uint32(len(machineName)))
	buf = append(buf, nameLen...)
	buf = append(buf, []byte(machineName)...)

	// Pad to 4-byte boundary
	padding := (4 - (len(machineName) % 4)) % 4
	for i := 0; i < padding; i++ {
		buf = append(buf, 0)
	}

	// UID (4 bytes)
	uidBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(uidBytes, uid)
	buf = append(buf, uidBytes...)

	// GID (4 bytes)
	gidBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(gidBytes, gid)
	buf = append(buf, gidBytes...)

	// Supplementary GIDs (count + elements)
	gidsCountBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(gidsCountBytes, uint32(len(gids)))
	buf = append(buf, gidsCountBytes...)

	for _, g := range gids {
		gBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(gBytes, g)
		buf = append(buf, gBytes...)
	}

	return buf
}
