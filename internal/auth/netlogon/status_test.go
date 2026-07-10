package netlogon

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestController_OfflineStatusAndRotate verifies the offline provider's status
// snapshot reflects the static credential, tracks channel connectivity, and that
// Rotate is rejected (the admin owns the static secret).
func TestController_OfflineStatusAndRotate(t *testing.T) {
	st := &fakeState{}
	withFakeChannels(t, st)

	prov := NewMutableProvider(validCred("DITTOFS$"))
	auth := NewAuthenticator(prov)
	ctrl := NewController(ProviderOffline, auth, prov, nil)
	ctx := context.Background()

	s := ctrl.Status(ctx)
	if s.Provider != ProviderOffline {
		t.Errorf("provider = %q, want offline", s.Provider)
	}
	if s.AccountName != "DITTOFS$" || s.Realm != "DITTOFS.AD" || s.NetBIOSDomain != "DITTOFS" {
		t.Errorf("unexpected status identity: %+v", s)
	}
	if s.ChannelConnected {
		t.Error("no channel should be connected before the first logon")
	}
	if s.RotationEnabled {
		t.Error("offline provider must never report rotation enabled")
	}

	// A logon establishes the cached channel; status must then report connected.
	if _, err := auth.NetworkLogon(ctx, NetworkLogonRequest{Username: "alice", Domain: "DITTOFS"}); err != nil {
		t.Fatalf("logon: %v", err)
	}
	if !ctrl.Status(ctx).ChannelConnected {
		t.Error("channel should be reported connected after a successful logon")
	}

	// Rotate is not applicable to the static provider.
	if err := ctrl.Rotate(ctx); !errors.Is(err, ErrRotateNotOnlineJoin) {
		t.Errorf("Rotate() error = %v, want ErrRotateNotOnlineJoin", err)
	}
}

// TestController_OfflineStatusReportsIncompleteCredential proves Status reads the
// live MutableProvider snapshot WITHOUT validation, so a passthrough-disabling
// hot-reload (empty credential) is still introspectable rather than erroring.
func TestController_OfflineStatusReportsIncompleteCredential(t *testing.T) {
	prov := NewMutableProvider(MachineCredential{}) // incomplete: Credential() would error
	ctrl := NewController(ProviderOffline, NewAuthenticator(prov), prov, nil)

	s := ctrl.Status(context.Background())
	if s.Provider != ProviderOffline {
		t.Errorf("provider = %q, want offline", s.Provider)
	}
	if s.AccountName != "" {
		t.Errorf("account = %q, want empty for a disabled/incomplete credential", s.AccountName)
	}
}

// TestController_OnlineStatusDoesNotJoin proves Status never triggers the lazy AD
// join: a fresh online provider reports joined=false and performs no dial.
func TestController_OnlineStatusDoesNotJoin(t *testing.T) {
	fake := &fakeLDAP{}
	var dialN int
	prov := &onlineProvider{cfg: onlineCfg(), secret: &memSecretStore{}, dial: countingDial(fake, &dialN)}
	ctrl := NewController(ProviderOnlineJoin, NewAuthenticator(prov), prov, nil)

	s := ctrl.Status(context.Background())
	if s.Provider != ProviderOnlineJoin {
		t.Errorf("provider = %q, want online-join", s.Provider)
	}
	if s.AccountName != "DITTOFS$" || s.Realm != "DITTOFS.AD" || s.NetBIOSDomain != "DITTOFS" {
		t.Errorf("unexpected status identity: %+v", s)
	}
	if s.Joined {
		t.Error("Status must not perform the lazy join")
	}
	if dialN != 0 {
		t.Errorf("Status must not dial LDAP; dialed %d times", dialN)
	}
	if !s.LastRotation.IsZero() {
		t.Error("LastRotation should be zero before any rotation")
	}
}

// TestController_OnlineRotatePersistsAndSwitches verifies the COMPLETE rotation
// through the Controller: the DC set succeeds, the in-memory password switches,
// the secret is persisted, and status reflects the last-rotation time.
func TestController_OnlineRotatePersistsAndSwitches(t *testing.T) {
	st := &fakeState{}
	withFakeChannels(t, st)

	fake := &fakeLDAP{}
	secret := &memSecretStore{}
	var dialN int
	prov := &onlineProvider{cfg: onlineCfg(), secret: secret, dial: countingDial(fake, &dialN)}
	auth := NewAuthenticator(prov)
	ctrl := NewController(ProviderOnlineJoin, auth, prov, nil)
	ctx := context.Background()

	// Join first (persists the initial generated password).
	cred, err := prov.Credential(ctx)
	if err != nil {
		t.Fatalf("initial join: %v", err)
	}
	initialPass := cred.Password
	if secret.val != initialPass {
		t.Fatalf("persisted secret %q != initial password %q", secret.val, initialPass)
	}
	if !ctrl.Status(ctx).Joined {
		t.Fatal("expected joined=true after the initial Credential call")
	}

	before := time.Now()
	if err := ctrl.Rotate(ctx); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// The in-memory credential must now be the NEW password, and the persisted
	// secret must match it (no desync between the DC-set value and the store).
	rotated, err := prov.Credential(ctx)
	if err != nil {
		t.Fatalf("post-rotate Credential: %v", err)
	}
	if rotated.Password == initialPass {
		t.Error("password did not change after rotation")
	}
	if secret.val != rotated.Password {
		t.Errorf("persisted secret %q != rotated in-memory password %q (desync)", secret.val, rotated.Password)
	}

	s := ctrl.Status(ctx)
	if s.LastRotation.IsZero() || s.LastRotation.Before(before) {
		t.Errorf("LastRotation not updated: %v", s.LastRotation)
	}
}

// TestRotationManager_NextRotationSchedule verifies nextRotation projects the
// ticker cadence once started, and is zero before Start.
func TestRotationManager_NextRotationSchedule(t *testing.T) {
	online := NewOnlineProvider(onlineCfg(), &memSecretStore{})
	m := NewRotationManager(online, NewAuthenticator(online), time.Hour)
	if m == nil {
		t.Fatal("expected non-nil rotation manager for a positive interval")
	}
	if !m.nextRotation().IsZero() {
		t.Error("nextRotation must be zero before Start")
	}
	m.Start()
	defer m.Stop()
	next := m.nextRotation()
	if next.IsZero() {
		t.Fatal("nextRotation must be set after Start")
	}
	// The next fire is within one interval of now.
	if d := time.Until(next); d <= 0 || d > time.Hour+time.Second {
		t.Errorf("next rotation %v is not within one interval (%v from now)", next, d)
	}
}
