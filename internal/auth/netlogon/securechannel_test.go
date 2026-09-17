package netlogon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
)

// TestWrapTransportFailure pins which connect-phase failures are retryable. A
// transport failure (the shape a concurrent reload produces by tearing the sealed
// SMB session down mid-handshake) must be classified transient so NetworkLogon
// rebuilds; a DC-side rejection such as a wrong machine password must NOT be, so
// the logon fails fast instead of retrying toward machine-account lockout.
func TestWrapTransportFailure(t *testing.T) {
	transient := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		syscall.ECONNRESET,
		syscall.EPIPE,
		fmt.Errorf("alter context: read buffer: %w", io.ErrUnexpectedEOF),
	}
	for _, err := range transient {
		if got := wrapTransportFailure(err); !errors.Is(got, errChannelNotConnected) {
			t.Errorf("wrapTransportFailure(%v) = %v, want it wrapped with the transient sentinel", err, got)
		}
	}

	nonTransient := []error{
		errors.New("netlogon: schannel: defective credential"),
		fmt.Errorf("logon failure: %w", errors.New("STATUS_LOGON_FAILURE")),
	}
	for _, err := range nonTransient {
		if got := wrapTransportFailure(err); errors.Is(got, errChannelNotConnected) {
			t.Errorf("wrapTransportFailure(%v) = %v, want it left unwrapped so the logon fails fast", err, got)
		}
	}
}

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
