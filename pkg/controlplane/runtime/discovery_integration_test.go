//go:build integration

package runtime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
)

// TestDiscoveryName_OverrideFromSetting verifies the instance-wide discovery
// name falls back to the auto-default when unset and returns the (trimmed)
// operator override once the discovery.name setting is stored.
func TestDiscoveryName_OverrideFromSetting(t *testing.T) {
	rt, cpStore := newRuntimeForChecks(t)

	if got, want := rt.DiscoveryName(), hostinfo.DefaultDiscoveryName(); got != want {
		t.Fatalf("unset DiscoveryName = %q, want default %q", got, want)
	}

	if err := cpStore.SetSetting(context.Background(), DiscoveryNameKey, "  My Filer  "); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got, want := rt.DiscoveryName(), "My Filer"; got != want {
		t.Fatalf("override DiscoveryName = %q, want trimmed %q", got, want)
	}
}
