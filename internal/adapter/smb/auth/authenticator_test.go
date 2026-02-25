package auth

import (
	"context"
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
	msg[8] = byte(Negotiate)
	msg[9] = 0
	msg[10] = 0
	msg[11] = 0
	// Flags: NTLM | Unicode
	flags := uint32(FlagNTLM | FlagUnicode)
	msg[12] = byte(flags)
	msg[13] = byte(flags >> 8)
	msg[14] = byte(flags >> 16)
	msg[15] = byte(flags >> 24)
	return msg
}

// buildNTLMAuthenticate creates a minimal NTLM Type 3 (AUTHENTICATE) message
// with the given username and domain.
func buildNTLMAuthenticate(username, domain string) []byte {
	// Encode strings as UTF-16LE
	usernameBytes := encodeUTF16LE(username)
	domainBytes := encodeUTF16LE(domain)

	// Calculate payload offsets - payload starts after fixed fields (88 bytes to be safe)
	payloadOffset := 88
	domainOffset := payloadOffset
	userOffset := domainOffset + len(domainBytes)

	// Allocate buffer
	totalSize := userOffset + len(usernameBytes)
	msg := make([]byte, totalSize)

	// Signature
	copy(msg[0:8], Signature)

	// MessageType: 3 (AUTHENTICATE)
	msg[8] = byte(Authenticate)

	// LmChallengeResponse: empty (offset 12-19)
	// Already zero

	// NtChallengeResponse: empty (offset 20-27)
	// Already zero

	// DomainName fields (offset 28-35)
	msg[28] = byte(len(domainBytes))
	msg[29] = byte(len(domainBytes) >> 8)
	msg[30] = byte(len(domainBytes))
	msg[31] = byte(len(domainBytes) >> 8)
	msg[32] = byte(domainOffset)
	msg[33] = byte(domainOffset >> 8)
	msg[34] = byte(domainOffset >> 16)
	msg[35] = byte(domainOffset >> 24)

	// UserName fields (offset 36-43)
	msg[36] = byte(len(usernameBytes))
	msg[37] = byte(len(usernameBytes) >> 8)
	msg[38] = byte(len(usernameBytes))
	msg[39] = byte(len(usernameBytes) >> 8)
	msg[40] = byte(userOffset)
	msg[41] = byte(userOffset >> 8)
	msg[42] = byte(userOffset >> 16)
	msg[43] = byte(userOffset >> 24)

	// Workstation: empty (offset 44-51)
	// Already zero

	// EncryptedRandomSessionKey: empty (offset 52-59)
	// Already zero

	// NegotiateFlags at offset 60: Unicode flag
	flags := uint32(FlagUnicode | FlagNTLM)
	msg[60] = byte(flags)
	msg[61] = byte(flags >> 8)
	msg[62] = byte(flags >> 16)
	msg[63] = byte(flags >> 24)

	// Copy payload
	copy(msg[domainOffset:], domainBytes)
	copy(msg[userOffset:], usernameBytes)

	return msg
}
