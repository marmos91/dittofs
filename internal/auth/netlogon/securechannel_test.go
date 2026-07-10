package netlogon

import (
	"context"
	"testing"
)

func TestNetworkLogonRequiresCredential(t *testing.T) {
	a := NewAuthenticator(NewOfflineProvider(MachineCredential{})) // incomplete -> Credential() errors
	_, err := a.NetworkLogon(context.Background(), NetworkLogonRequest{
		Username: "alice", Domain: "DITTOFS",
	})
	if err == nil {
		t.Fatal("expected error when machine credential is incomplete")
	}
}

// TestProbe verifies that Probe connects a secure channel and tears it down,
// leaving no cached channel behind (backing `dfs netlogon test`, #1629).
func TestProbe(t *testing.T) {
	st := &fakeState{}
	withFakeChannels(t, st)

	a := NewAuthenticator(NewOfflineProvider(validCred("DITTOFS$")))
	if err := a.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	st.mu.Lock()
	built := st.built
	st.mu.Unlock()
	if built != 1 {
		t.Errorf("expected exactly 1 channel connected, got %d", built)
	}
	// Probe must not leave a live channel cached on the authenticator.
	a.mu.Lock()
	leftover := a.chan_
	a.mu.Unlock()
	if leftover != nil {
		t.Error("Probe should tear down the channel, but one is still cached")
	}
}

// TestProbeRequiresCredential ensures Probe surfaces a credential error (e.g. an
// incomplete machine account) rather than dialing.
func TestProbeRequiresCredential(t *testing.T) {
	a := NewAuthenticator(NewOfflineProvider(MachineCredential{})) // incomplete
	if err := a.Probe(context.Background()); err == nil {
		t.Fatal("expected Probe to error on an incomplete machine credential")
	}
}

// TestDeriveLogonServer covers the LogonServer name derived locally from the DC's
// Kerberos SPN (the GetDCName replacement, #1629): the short host label, uppercased
// and UNC-prefixed, with a domain-name fallback when no host is present.
func TestDeriveLogonServer(t *testing.T) {
	tests := []struct {
		name   string
		spn    string
		domain string
		want   string
	}{
		{"fqdn spn", "cifs/dc01.example.com", "EXAMPLE", `\\DC01`},
		{"short spn", "cifs/dc01", "EXAMPLE", `\\DC01`},
		{"already upper", "cifs/DC01.example.com", "EXAMPLE", `\\DC01`},
		{"no cifs prefix", "dc01.example.com", "EXAMPLE", `\\DC01`},
		{"empty spn falls back to domain", "", "EXAMPLE", `\\EXAMPLE`},
		{"cifs-only falls back to domain", "cifs/", "EXAMPLE", `\\EXAMPLE`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveLogonServer(tt.spn, tt.domain); got != tt.want {
				t.Errorf("deriveLogonServer(%q, %q) = %q, want %q", tt.spn, tt.domain, got, tt.want)
			}
		})
	}
}
