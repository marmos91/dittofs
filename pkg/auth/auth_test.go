package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// mockProvider is a test AuthProvider.
type mockProvider struct {
	name       string
	canHandle  func(token []byte) bool
	authResult *AuthResult
	authErr    error
}

func (m *mockProvider) CanHandle(token []byte) bool { return m.canHandle(token) }
func (m *mockProvider) Name() string                { return m.name }
func (m *mockProvider) Authenticate(_ context.Context, _ []byte) (*AuthResult, error) {
	return m.authResult, m.authErr
}

func TestAuthenticator_ProvidersTriedInOrder(t *testing.T) {
	var order []string
	mkProvider := func(name string, handle bool) *mockProvider {
		return &mockProvider{
			name: name,
			canHandle: func(_ []byte) bool {
				order = append(order, name)
				return handle
			},
			authResult: &AuthResult{Provider: name, Authenticated: true},
		}
	}

	auth := NewAuthenticator(mkProvider("first", false), mkProvider("second", true), mkProvider("third", true))
	res, err := auth.Authenticate(context.Background(), []byte("token"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Provider != "second" {
		t.Errorf("Provider = %q, want %q", res.Provider, "second")
	}
	// first and second should be checked; third should not (second handled it)
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("CanHandle call order = %v, want [first second]", order)
	}
}

func TestAuthenticator_NoProviderCanHandle(t *testing.T) {
	p := &mockProvider{
		name:      "nope",
		canHandle: func(_ []byte) bool { return false },
	}
	auth := NewAuthenticator(p)
	_, err := auth.Authenticate(context.Background(), []byte("token"))
	if !errors.Is(err, ErrUnsupportedMechanism) {
		t.Errorf("err = %v, want ErrUnsupportedMechanism", err)
	}
}

func TestAuthenticator_ErrUnsupportedMechanism_ContinuesToNext(t *testing.T) {
	spnego := &mockProvider{
		name:      "spnego-krb5",
		canHandle: func(_ []byte) bool { return true },
		authErr:   ErrUnsupportedMechanism,
	}
	fallback := &mockProvider{
		name:       "spnego-ntlm",
		canHandle:  func(_ []byte) bool { return true },
		authResult: &AuthResult{Provider: "spnego-ntlm", Authenticated: true},
	}
	auth := NewAuthenticator(spnego, fallback)
	res, err := auth.Authenticate(context.Background(), []byte("token"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Provider != "spnego-ntlm" {
		t.Errorf("Provider = %q, want %q", res.Provider, "spnego-ntlm")
	}
}

func TestAuthenticator_AllReturnErrUnsupported(t *testing.T) {
	p1 := &mockProvider{name: "a", canHandle: func(_ []byte) bool { return true }, authErr: ErrUnsupportedMechanism}
	p2 := &mockProvider{name: "b", canHandle: func(_ []byte) bool { return true }, authErr: ErrUnsupportedMechanism}
	auth := NewAuthenticator(p1, p2)
	_, err := auth.Authenticate(context.Background(), []byte("token"))
	if !errors.Is(err, ErrUnsupportedMechanism) {
		t.Errorf("err = %v, want ErrUnsupportedMechanism", err)
	}
}

func TestAuthenticator_ConcurrentAuthenticate(t *testing.T) {
	p := &mockProvider{
		name:       "concurrent",
		canHandle:  func(_ []byte) bool { return true },
		authResult: &AuthResult{Provider: "concurrent", Authenticated: true},
	}
	auth := NewAuthenticator(p)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := auth.Authenticate(context.Background(), []byte("token"))
			if err != nil {
				t.Errorf("concurrent auth error: %v", err)
			}
			if res == nil || !res.Authenticated {
				t.Error("expected authenticated result")
			}
		}()
	}
	wg.Wait()
}

func TestAuthenticator_AuthFailedPropagated(t *testing.T) {
	p := &mockProvider{
		name:      "failing",
		canHandle: func(_ []byte) bool { return true },
		authErr:   ErrAuthFailed,
	}
	auth := NewAuthenticator(p)
	_, err := auth.Authenticate(context.Background(), []byte("token"))
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}
