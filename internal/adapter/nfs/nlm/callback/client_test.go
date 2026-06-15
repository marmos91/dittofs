package callback

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
)

// TestValidateCallbackAddr_RejectsUnsafe is the negative control for the SSRF
// guard. The server must refuse to dial unspecified, multicast, port-0, or
// malformed callback destinations.
func TestValidateCallbackAddr_RejectsUnsafe(t *testing.T) {
	bad := []string{
		"0.0.0.0:111",        // unspecified
		"[::]:111",           // unspecified v6
		"224.0.0.1:111",      // multicast v4
		"[ff02::1]:111",      // multicast v6
		"169.254.169.254:80", // cloud metadata endpoint (link-local) — SSRF pivot
		"[fe80::1]:4045",     // link-local v6
		"10.0.0.7:0",         // invalid port
		"not-an-ip:111",      // non-literal host
		"10.0.0.7",           // missing port
	}
	for _, addr := range bad {
		if err := validateCallbackAddr(addr); err == nil {
			t.Errorf("validateCallbackAddr(%q) = nil, want rejection", addr)
		}
	}
}

// TestValidateCallbackAddr_AllowsUnicast confirms ordinary unicast client
// addresses are accepted.
func TestValidateCallbackAddr_AllowsUnicast(t *testing.T) {
	// Loopback is intentionally allowed for NLM (co-located NFS mounts).
	ok := []string{"10.0.0.7:54321", "192.168.1.5:1023", "127.0.0.1:1023", "[::1]:4045"}
	for _, addr := range ok {
		if err := validateCallbackAddr(addr); err != nil {
			t.Errorf("validateCallbackAddr(%q) = %v, want nil", addr, err)
		}
	}
}

// TestSendGrantedCallback_RejectsSSRFTarget verifies the SSRF guard fires
// before any dial: an unsafe address returns a "reject callback address" error
// rather than attempting a connection.
func TestSendGrantedCallback_RejectsSSRFTarget(t *testing.T) {
	err := SendGrantedCallback(context.Background(), "224.0.0.1:111",
		types.ProgramNLM, types.NLMVersion4, &types.NLM4GrantedArgs{})
	if err == nil {
		t.Fatal("SendGrantedCallback to multicast addr = nil, want SSRF rejection")
	}
	if !strings.Contains(err.Error(), "reject callback address") {
		t.Fatalf("error = %q, want SSRF rejection", err.Error())
	}
}
