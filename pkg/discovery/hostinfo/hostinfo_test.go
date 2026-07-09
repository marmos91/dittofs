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
