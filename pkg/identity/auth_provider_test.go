package identity

import (
	"context"
	"testing"
)

// createTestUserStore creates a ConfigUserStore for testing auth providers.
func createTestUserStore(t *testing.T) *ConfigUserStore {
	t.Helper()

	hash, err := HashPassword("password123")
	if err != nil {
		t.Fatalf("Failed to hash password: %v", err)
	}

	users := []*User{
		{
			Username:     "testuser",
			PasswordHash: hash,
			Enabled:      true,
			UID:          1000,
			GID:          1000,
		},
		{
			Username:     "disabled",
			PasswordHash: hash,
			Enabled:      false,
			UID:          1001,
			GID:          1000,
		},
	}

	store, err := NewConfigUserStore(users, nil, nil)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}

	return store
}

func TestLocalAuthProvider_Name(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)

	if provider.Name() != "local" {
		t.Errorf("Name() = %q, want 'local'", provider.Name())
	}
}

func TestLocalAuthProvider_SupportsCredentialType(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)

	tests := []struct {
		credType string
		expected bool
	}{
		{"password", true},
		{"ntlm", false},
		{"kerberos", false},
		{"certificate", false},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.credType, func(t *testing.T) {
			result := provider.SupportsCredentialType(tc.credType)
			if result != tc.expected {
				t.Errorf("SupportsCredentialType(%q) = %v, want %v", tc.credType, result, tc.expected)
			}
		})
	}
}

func TestLocalAuthProvider_Authenticate_Success(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	creds := &PasswordCredentials{
		Username: "testuser",
		Password: "password123",
	}

	user, err := provider.Authenticate(ctx, creds)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if user == nil {
		t.Fatal("Authenticate() returned nil user")
	}
	if user.Username != "testuser" {
		t.Errorf("Authenticate() username = %q, want 'testuser'", user.Username)
	}
}

func TestLocalAuthProvider_Authenticate_WrongPassword(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	creds := &PasswordCredentials{
		Username: "testuser",
		Password: "wrongpassword",
	}

	_, err := provider.Authenticate(ctx, creds)
	if err != ErrAuthenticationFailed {
		t.Errorf("Authenticate() error = %v, want ErrAuthenticationFailed", err)
	}
}

func TestLocalAuthProvider_Authenticate_UserNotFound(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	creds := &PasswordCredentials{
		Username: "nonexistent",
		Password: "password123",
	}

	_, err := provider.Authenticate(ctx, creds)
	if err != ErrAuthenticationFailed {
		t.Errorf("Authenticate() error = %v, want ErrAuthenticationFailed", err)
	}
}

func TestLocalAuthProvider_Authenticate_DisabledUser(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	creds := &PasswordCredentials{
		Username: "disabled",
		Password: "password123",
	}

	_, err := provider.Authenticate(ctx, creds)
	if err != ErrAuthenticationFailed {
		t.Errorf("Authenticate() error = %v, want ErrAuthenticationFailed", err)
	}
}

// mockCredentials is an unsupported credential type for testing.
type mockCredentials struct{}

func (m *mockCredentials) Type() string { return "mock" }

func TestLocalAuthProvider_Authenticate_UnsupportedCredType(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	_, err := provider.Authenticate(ctx, &mockCredentials{})
	if err != ErrUnsupportedCredType {
		t.Errorf("Authenticate() error = %v, want ErrUnsupportedCredType", err)
	}
}

func TestLocalAuthProvider_LookupUser_ByUsername(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	user, err := provider.LookupUser(ctx, UserIdentifier{Username: "testuser"})
	if err != nil {
		t.Fatalf("LookupUser() error = %v", err)
	}
	if user.Username != "testuser" {
		t.Errorf("LookupUser() username = %q, want 'testuser'", user.Username)
	}
}

func TestLocalAuthProvider_LookupUser_ByUID(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	uid := uint32(1000)
	user, err := provider.LookupUser(ctx, UserIdentifier{UID: &uid})
	if err != nil {
		t.Fatalf("LookupUser() error = %v", err)
	}
	if user.UID != 1000 {
		t.Errorf("LookupUser() UID = %d, want 1000", user.UID)
	}
}

func TestLocalAuthProvider_LookupUser_NotFound(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	ctx := context.Background()

	_, err := provider.LookupUser(ctx, UserIdentifier{Username: "nonexistent"})
	if err != ErrUserNotFound {
		t.Errorf("LookupUser() error = %v, want ErrUserNotFound", err)
	}
}

func TestPasswordCredentials_Type(t *testing.T) {
	creds := &PasswordCredentials{
		Username: "user",
		Password: "pass",
	}

	if creds.Type() != "password" {
		t.Errorf("Type() = %q, want 'password'", creds.Type())
	}
}

// ============================================================================
// AuthProviderChain Tests
// ============================================================================

func TestAuthProviderChain_Name(t *testing.T) {
	chain := NewAuthProviderChain()

	if chain.Name() != "chain" {
		t.Errorf("Name() = %q, want 'chain'", chain.Name())
	}
}

func TestAuthProviderChain_Empty(t *testing.T) {
	chain := NewAuthProviderChain()
	ctx := context.Background()

	creds := &PasswordCredentials{Username: "user", Password: "pass"}

	_, err := chain.Authenticate(ctx, creds)
	if err != ErrUnsupportedCredType {
		t.Errorf("Authenticate() on empty chain error = %v, want ErrUnsupportedCredType", err)
	}

	_, err = chain.LookupUser(ctx, UserIdentifier{Username: "user"})
	if err != ErrUserNotFound {
		t.Errorf("LookupUser() on empty chain error = %v, want ErrUserNotFound", err)
	}
}

func TestAuthProviderChain_SingleProvider(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	chain := NewAuthProviderChain(provider)
	ctx := context.Background()

	creds := &PasswordCredentials{Username: "testuser", Password: "password123"}

	user, err := chain.Authenticate(ctx, creds)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if user.Username != "testuser" {
		t.Errorf("Authenticate() username = %q, want 'testuser'", user.Username)
	}
}

func TestAuthProviderChain_AddProvider(t *testing.T) {
	chain := NewAuthProviderChain()
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)

	chain.AddProvider(provider)

	if !chain.SupportsCredentialType("password") {
		t.Error("Chain should support 'password' after adding LocalAuthProvider")
	}
}

func TestAuthProviderChain_SupportsCredentialType(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	chain := NewAuthProviderChain(provider)

	if !chain.SupportsCredentialType("password") {
		t.Error("Chain should support 'password'")
	}
	if chain.SupportsCredentialType("kerberos") {
		t.Error("Chain should not support 'kerberos'")
	}
}

func TestAuthProviderChain_LookupUser(t *testing.T) {
	store := createTestUserStore(t)
	provider := NewLocalAuthProvider(store)
	chain := NewAuthProviderChain(provider)
	ctx := context.Background()

	user, err := chain.LookupUser(ctx, UserIdentifier{Username: "testuser"})
	if err != nil {
		t.Fatalf("LookupUser() error = %v", err)
	}
	if user.Username != "testuser" {
		t.Errorf("LookupUser() username = %q, want 'testuser'", user.Username)
	}
}

func TestAuthProviderChain_FailsThenSucceeds(t *testing.T) {
	// Create two stores - first one doesn't have the user, second one does
	emptyStore, _ := NewConfigUserStore(nil, nil, nil)
	fullStore := createTestUserStore(t)

	emptyProvider := NewLocalAuthProvider(emptyStore)
	fullProvider := NewLocalAuthProvider(fullStore)

	chain := NewAuthProviderChain(emptyProvider, fullProvider)
	ctx := context.Background()

	creds := &PasswordCredentials{Username: "testuser", Password: "password123"}

	// Should succeed because second provider has the user
	user, err := chain.Authenticate(ctx, creds)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if user.Username != "testuser" {
		t.Errorf("Authenticate() username = %q, want 'testuser'", user.Username)
	}
}

func TestAuthProviderChain_AllFail(t *testing.T) {
	store1, _ := NewConfigUserStore(nil, nil, nil)
	store2, _ := NewConfigUserStore(nil, nil, nil)

	chain := NewAuthProviderChain(
		NewLocalAuthProvider(store1),
		NewLocalAuthProvider(store2),
	)
	ctx := context.Background()

	creds := &PasswordCredentials{Username: "nonexistent", Password: "password"}

	_, err := chain.Authenticate(ctx, creds)
	if err != ErrAuthenticationFailed {
		t.Errorf("Authenticate() error = %v, want ErrAuthenticationFailed", err)
	}
}
