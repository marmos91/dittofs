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
// Mock UserStore for testing
// =============================================================================

type mockUserStore struct {
	users map[string]*models.User
}

func newMockUserStore() *mockUserStore {
	return &mockUserStore{users: make(map[string]*models.User)}
}

func (m *mockUserStore) addUser(user *models.User) {
	m.users[user.Username] = user
}

func (m *mockUserStore) GetUser(_ context.Context, username string) (*models.User, error) {
	user, ok := m.users[username]
	if !ok {
		return nil, models.ErrUserNotFound
	}
	return user, nil
}

func (m *mockUserStore) ValidateCredentials(_ context.Context, username, _ string) (*models.User, error) {
	user, ok := m.users[username]
	if !ok {
		return nil, models.ErrInvalidCredentials
	}
	return user, nil
}

func (m *mockUserStore) ListUsers(_ context.Context) ([]*models.User, error) {
	var users []*models.User
	for _, u := range m.users {
		users = append(users, u)
	}
	return users, nil
}

func (m *mockUserStore) GetGuestUser(_ context.Context, _ string) (*models.User, error) {
	return nil, errors.New("guest disabled")
}

func (m *mockUserStore) GetGroup(_ context.Context, _ string) (*models.Group, error) {
	return nil, models.ErrGroupNotFound
}

func (m *mockUserStore) ListGroups(_ context.Context) ([]*models.Group, error) {
	return nil, nil
}

func (m *mockUserStore) GetUserGroups(_ context.Context, _ string) ([]*models.Group, error) {
	return nil, nil
}

func (m *mockUserStore) ResolveSharePermission(_ context.Context, _ *models.User, _ string) (models.SharePermission, error) {
	return "", nil
}

// =============================================================================
// Interface Conformance
// =============================================================================

func TestSMBAuthenticator_ImplementsAuthenticator(t *testing.T) {
	var _ adapter.Authenticator = (*SMBAuthenticator)(nil)
}

// =============================================================================
// Constructor Tests
// =============================================================================

func TestNewSMBAuthenticator(t *testing.T) {
	t.Run("WithUserStore", func(t *testing.T) {
		store := newMockUserStore()
		auth := NewSMBAuthenticator(store)
		if auth == nil {
			t.Fatal("NewSMBAuthenticator returned nil")
		}
		if auth.userStore != store {
			t.Error("userStore not set correctly")
		}
	})

	t.Run("WithNilUserStore", func(t *testing.T) {
		auth := NewSMBAuthenticator(nil)
		if auth == nil {
			t.Fatal("NewSMBAuthenticator returned nil")
		}
		if auth.userStore != nil {
			t.Error("userStore should be nil")
		}
	})
}

// =============================================================================
// Authenticate - Empty/Invalid Token Tests
// =============================================================================

func TestSMBAuthenticator_Authenticate_EmptyToken(t *testing.T) {
	auth := NewSMBAuthenticator(nil)

	result, challenge, err := auth.Authenticate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if challenge != nil {
		t.Error("expected nil challenge for empty token")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsGuest {
		t.Error("expected guest result for empty token")
	}
}

func TestSMBAuthenticator_Authenticate_InvalidToken(t *testing.T) {
	auth := NewSMBAuthenticator(nil)

	result, challenge, err := auth.Authenticate(context.Background(), []byte{0xDE, 0xAD, 0xBE, 0xEF})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if challenge != nil {
		t.Error("expected nil challenge for invalid token")
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsGuest {
		t.Error("expected guest result for invalid token")
	}
}

// =============================================================================
// Authenticate - NTLM Round-Trip Tests
// =============================================================================

func TestSMBAuthenticator_Authenticate_NTLMNegotiate_ReturnsChallenge(t *testing.T) {
	auth := NewSMBAuthenticator(nil)

	// Build a raw NTLM Type 1 (NEGOTIATE) message
	negotiate := buildNTLMNegotiate()

	result, challenge, err := auth.Authenticate(context.Background(), negotiate)
	if !errors.Is(err, adapter.ErrMoreProcessingRequired) {
		t.Fatalf("expected ErrMoreProcessingRequired, got: %v", err)
	}
	if result != nil {
		t.Error("expected nil result during negotiation")
	}
	if len(challenge) == 0 {
		t.Fatal("expected non-empty challenge token")
	}

	// The challenge should be a valid NTLM Type 2 message (raw, since input was raw)
	if !IsValid(challenge) {
		t.Error("challenge should be valid NTLM message")
	}
	if GetMessageType(challenge) != Challenge {
		t.Errorf("expected Challenge message type, got %d", GetMessageType(challenge))
	}
}

func TestSMBAuthenticator_Authenticate_NTLMFullRoundTrip_GuestNoStore(t *testing.T) {
	auth := NewSMBAuthenticator(nil)

	// Round 1: NEGOTIATE
	negotiate := buildNTLMNegotiate()
	_, challenge, err := auth.Authenticate(context.Background(), negotiate)
	if !errors.Is(err, adapter.ErrMoreProcessingRequired) {
		t.Fatalf("round 1: expected ErrMoreProcessingRequired, got: %v", err)
	}
	if len(challenge) == 0 {
		t.Fatal("round 1: expected challenge")
	}

	// Round 2: AUTHENTICATE with a username (no user store -> guest)
	authenticate := buildNTLMAuthenticate("testuser", "WORKGROUP")
	result, _, err := auth.Authenticate(context.Background(), authenticate)
	if err != nil {
		t.Fatalf("round 2: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("round 2: expected non-nil result")
	}
	if !result.IsGuest {
		t.Error("round 2: expected guest when no user store")
	}
}

func TestSMBAuthenticator_Authenticate_NTLMFullRoundTrip_WithUser(t *testing.T) {
	store := newMockUserStore()
	uid := uint32(1000)
	user := &models.User{
		ID:       "test-id",
		Username: "alice",
		Enabled:  true,
		UID:      &uid,
	}
	store.addUser(user)

	auth := NewSMBAuthenticator(store)

	// Round 1: NEGOTIATE
	negotiate := buildNTLMNegotiate()
	_, challenge, err := auth.Authenticate(context.Background(), negotiate)
	if !errors.Is(err, adapter.ErrMoreProcessingRequired) {
		t.Fatalf("round 1: expected ErrMoreProcessingRequired, got: %v", err)
	}
	if len(challenge) == 0 {
		t.Fatal("round 1: expected challenge")
	}

	// Round 2: AUTHENTICATE with known username (no NT hash on user -> auth without validation)
	authenticate := buildNTLMAuthenticate("alice", "WORKGROUP")
	result, _, err := auth.Authenticate(context.Background(), authenticate)
	if err != nil {
		t.Fatalf("round 2: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("round 2: expected non-nil result")
	}
	if result.IsGuest {
		t.Error("round 2: should not be guest for known user")
	}
	if result.User == nil {
		t.Fatal("round 2: expected non-nil user")
	}
	if result.User.Username != "alice" {
		t.Errorf("round 2: expected username 'alice', got %q", result.User.Username)
	}
}

func TestSMBAuthenticator_Authenticate_AnonymousNTLM(t *testing.T) {
	store := newMockUserStore()
	auth := NewSMBAuthenticator(store)

	// NEGOTIATE
	negotiate := buildNTLMNegotiate()
	_, _, err := auth.Authenticate(context.Background(), negotiate)
	if !errors.Is(err, adapter.ErrMoreProcessingRequired) {
		t.Fatalf("expected ErrMoreProcessingRequired, got: %v", err)
	}

	// AUTHENTICATE with empty username (anonymous)
	authenticate := buildNTLMAuthenticate("", "")
	result, _, err := auth.Authenticate(context.Background(), authenticate)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsGuest {
		t.Error("expected guest for anonymous auth")
	}
}

func TestSMBAuthenticator_Authenticate_DisabledUser(t *testing.T) {
	store := newMockUserStore()
	store.addUser(&models.User{
		ID:       "disabled-id",
		Username: "disabled",
		Enabled:  false,
	})

	auth := NewSMBAuthenticator(store)

	// NEGOTIATE
	negotiate := buildNTLMNegotiate()
	_, _, _ = auth.Authenticate(context.Background(), negotiate)

	// AUTHENTICATE with disabled user
	authenticate := buildNTLMAuthenticate("disabled", "WORKGROUP")
	result, _, err := auth.Authenticate(context.Background(), authenticate)
	if err == nil {
		t.Fatal("expected error for disabled user")
	}
	if result != nil {
		t.Error("expected nil result for disabled user")
	}
}

// =============================================================================
// ErrMoreProcessingRequired Sentinel Tests
// =============================================================================

func TestErrMoreProcessingRequired(t *testing.T) {
	// Verify the sentinel error works with errors.Is
	if !errors.Is(adapter.ErrMoreProcessingRequired, adapter.ErrMoreProcessingRequired) {
		t.Error("ErrMoreProcessingRequired should match itself")
	}
}

// =============================================================================
// Helper: Build NTLM Messages for Testing
// =============================================================================

// buildNTLMNegotiate creates a minimal NTLM Type 1 (NEGOTIATE) message.
func buildNTLMNegotiate() []byte {
	msg := make([]byte, 32)
	copy(msg[0:8], Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(Negotiate))
	binary.LittleEndian.PutUint32(msg[12:16], uint32(FlagNTLM|FlagUnicode))
	return msg
}

// buildNTLMAuthenticate creates a minimal NTLM Type 3 (AUTHENTICATE) message
// with the given username and domain.
func buildNTLMAuthenticate(username, domain string) []byte {
	usernameBytes := encodeUTF16LE(username)
	domainBytes := encodeUTF16LE(domain)

	// Payload starts after fixed fields (88 bytes to be safe)
	payloadOffset := 88
	domainOffset := payloadOffset
	userOffset := domainOffset + len(domainBytes)

	msg := make([]byte, userOffset+len(usernameBytes))

	// Header
	copy(msg[0:8], Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(Authenticate))

	// DomainName fields (offset 28-35)
	binary.LittleEndian.PutUint16(msg[28:30], uint16(len(domainBytes)))
	binary.LittleEndian.PutUint16(msg[30:32], uint16(len(domainBytes)))
	binary.LittleEndian.PutUint32(msg[32:36], uint32(domainOffset))

	// UserName fields (offset 36-43)
	binary.LittleEndian.PutUint16(msg[36:38], uint16(len(usernameBytes)))
	binary.LittleEndian.PutUint16(msg[38:40], uint16(len(usernameBytes)))
	binary.LittleEndian.PutUint32(msg[40:44], uint32(userOffset))

	// NegotiateFlags (offset 60)
	binary.LittleEndian.PutUint32(msg[60:64], uint32(FlagUnicode|FlagNTLM))

	// Payload
	copy(msg[domainOffset:], domainBytes)
	copy(msg[userOffset:], usernameBytes)

	return msg
}
