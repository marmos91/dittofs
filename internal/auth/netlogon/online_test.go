package netlogon

import (
	"context"
	"errors"
	"sync"
	"testing"

	ldapv3 "github.com/go-ldap/ldap/v3"
)

// memSecretStore is an in-memory SecretStore for online-provider tests.
type memSecretStore struct {
	mu     sync.Mutex
	val    string
	setN   int
	setErr error
	getErr error
}

func (m *memSecretStore) GetMachineSecret(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.val, m.getErr
}
func (m *memSecretStore) SetMachineSecret(_ context.Context, s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	m.val = s
	m.setN++
	return nil
}

func onlineCfg() OnlineConfig {
	return OnlineConfig{
		AccountName: "DITTOFS$",
		Workstation: "DITTOFS",
		DomainName:  "DITTOFS",
		Realm:       "DITTOFS.AD",
		Join: JoinConfig{
			LDAPURL:      "ldaps://dc.dittofs.ad",
			BindDN:       "CN=Administrator,CN=Users,DC=dittofs,DC=ad",
			BindPassword: "Passw0rd!2024",
			BaseDN:       "DC=dittofs,DC=ad",
			MachineName:  "DITTOFS",
		},
	}
}

// countingDial returns a dialer that records how many times it is invoked and
// the fake conn it returns.
func countingDial(fake *fakeLDAP, count *int) ldapDialer {
	return func(context.Context, *JoinConfig) (ldapConn, error) {
		*count++
		return fake, nil
	}
}

func TestOnlineProvider_FirstCallJoinsAndPersists(t *testing.T) {
	fake := &fakeLDAP{}
	secret := &memSecretStore{}
	var dialN int

	p := &onlineProvider{cfg: onlineCfg(), secret: secret, dial: countingDial(fake, &dialN)}

	cred, err := p.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.AccountName != "DITTOFS$" || cred.Realm != "DITTOFS.AD" {
		t.Errorf("unexpected credential: %+v", cred)
	}
	if cred.Password == "" {
		t.Fatal("expected a generated password")
	}
	if dialN != 1 {
		t.Errorf("expected 1 join dial, got %d", dialN)
	}
	if secret.val != cred.Password {
		t.Errorf("persisted secret %q != credential password %q", secret.val, cred.Password)
	}
	if len(fake.adds) != 1 {
		t.Errorf("expected computer Add on first join, got %d", len(fake.adds))
	}
}

func TestOnlineProvider_SecondCallDoesNotRejoin(t *testing.T) {
	fake := &fakeLDAP{}
	secret := &memSecretStore{}
	var dialN int
	p := &onlineProvider{cfg: onlineCfg(), secret: secret, dial: countingDial(fake, &dialN)}

	first, _ := p.Credential(context.Background())
	second, err := p.Credential(context.Background())
	if err != nil {
		t.Fatalf("second Credential: %v", err)
	}
	if dialN != 1 {
		t.Errorf("expected join to run only once, dialed %d times", dialN)
	}
	if first.Password != second.Password {
		t.Error("password changed between calls without rotation")
	}
}

func TestOnlineProvider_ReusesPersistedSecretAcrossRestart(t *testing.T) {
	// Simulate a restart: a fresh provider with a pre-populated secret store must
	// NOT re-join — it reuses the persisted password.
	secret := &memSecretStore{val: "PersistedPass1!"}
	fake := &fakeLDAP{}
	var dialN int
	p := &onlineProvider{cfg: onlineCfg(), secret: secret, dial: countingDial(fake, &dialN)}

	cred, err := p.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.Password != "PersistedPass1!" {
		t.Errorf("expected persisted password, got %q", cred.Password)
	}
	if dialN != 0 {
		t.Errorf("expected no join dial on restart with persisted secret, got %d", dialN)
	}
	if len(fake.adds) != 0 {
		t.Errorf("expected no computer Add on restart, got %d", len(fake.adds))
	}
}

func TestOnlineProvider_DoesNotMarkJoinedWhenPersistFails(t *testing.T) {
	fake := &fakeLDAP{}
	secret := &memSecretStore{setErr: errors.New("db down")}
	var dialN int
	p := &onlineProvider{cfg: onlineCfg(), secret: secret, dial: countingDial(fake, &dialN)}

	if _, err := p.Credential(context.Background()); err == nil {
		t.Fatal("expected error when persistence fails")
	}
	// A second attempt must retry the join (not silently treat it as joined).
	secret.setErr = nil
	if _, err := p.Credential(context.Background()); err != nil {
		t.Fatalf("retry Credential: %v", err)
	}
	if dialN != 2 {
		t.Errorf("expected join retried after persistence failure, dialed %d times", dialN)
	}
}

func TestNewRotationManager_NilForOfflineOrZeroInterval(t *testing.T) {
	offline := NewOfflineProvider(MachineCredential{AccountName: "X$", Password: "p", DomainName: "D", Realm: "R"})
	auth := NewAuthenticator(offline)
	if m := NewRotationManager(offline, auth, 1); m != nil {
		t.Error("expected nil rotation manager for offline provider")
	}

	online := NewOnlineProvider(onlineCfg(), &memSecretStore{})
	if m := NewRotationManager(online, NewAuthenticator(online), 0); m != nil {
		t.Error("expected nil rotation manager for zero interval")
	}
}

func TestRotationManager_NilSafe(t *testing.T) {
	var m *RotationManager
	m.Start() // must not panic
	m.Stop()  // must not panic
}

// ensure fakeLDAP search returns an empty (non-nil) result for the no-OU path.
var _ = ldapv3.NewSearchRequest
