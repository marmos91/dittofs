package hostinfo

import (
	"os"
	"strings"
	"testing"
)

func TestServerName_StripsDomainAndUppercases(t *testing.T) {
	got := serverNameFrom("vm2.cubbit.local", nil)
	if got != "VM2" {
		t.Fatalf("serverNameFrom = %q, want VM2", got)
	}
}

func TestServerName_Fallback(t *testing.T) {
	if got := serverNameFrom("", nil); got != FallbackName {
		t.Fatalf("empty hostname -> %q, want %q", got, FallbackName)
	}
	if got := serverNameFrom("ignored", os.ErrInvalid); got != FallbackName {
		t.Fatalf("hostname error -> %q, want %q", got, FallbackName)
	}
}

func TestServerName_SingleLabel(t *testing.T) {
	if got := serverNameFrom("dittofs", nil); got != "DITTOFS" {
		t.Fatalf("serverNameFrom = %q, want DITTOFS", got)
	}
}

func TestServerName_RealHostDoesNotPanic(t *testing.T) {
	// Smoke: the real accessor returns a non-empty, upper-cased name.
	got := ServerName()
	if got == "" {
		t.Fatal("ServerName returned empty")
	}
	if got != strings.ToUpper(got) {
		t.Fatalf("ServerName %q is not upper-cased", got)
	}
}

func TestDefaultDiscoveryName_RealHost(t *testing.T) {
	got := DefaultDiscoveryName()
	// Either "DittoFS-<HOST>" or the bare prefix when the hostname is unavailable.
	if got != DefaultNamePrefix && !strings.HasPrefix(got, DefaultNamePrefix+"-") {
		t.Fatalf("DefaultDiscoveryName = %q, want %q or %q-<host>", got, DefaultNamePrefix, DefaultNamePrefix)
	}
}

func TestNetBIOSSafe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"DittoFS-VM2", "DITTOFS-VM2"},           // already legal, upper-cased
		{"DittoFS@vm2", "DITTOFS-VM2"},           // '@' is illegal -> '-'
		{"My File Server!", "MY-FILE-SERVER"},    // spaces/punct -> '-', trailing trimmed
		{"a.b.c", "A-B-C"},                          // dots -> '-'
		{"DittoFS-ReallyLongHostname", "DITTOFS-REALLYL"}, // capped at 15 chars
		{"@@@", FallbackName},                        // nothing legal survives
		{"", FallbackName},                           // empty
	}
	for _, c := range cases {
		if got := NetBIOSSafe(c.in); got != c.want {
			t.Errorf("NetBIOSSafe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNetBIOSSafe_NeverExceeds15(t *testing.T) {
	got := NetBIOSSafe("this-is-a-very-long-discovery-name")
	if len(got) > 15 {
		t.Fatalf("NetBIOSSafe len = %d (%q), want <= 15", len(got), got)
	}
}
