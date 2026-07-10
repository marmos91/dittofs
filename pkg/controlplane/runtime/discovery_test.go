package runtime

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
)

// TestDiscoveryName_DefaultWithoutStore verifies the auto-default is used when
// no store is wired (and thus no override can be read).
func TestDiscoveryName_DefaultWithoutStore(t *testing.T) {
	rt := &Runtime{}
	if got, want := rt.DiscoveryName(), hostinfo.DefaultDiscoveryName(); got != want {
		t.Fatalf("DiscoveryName without store = %q, want default %q", got, want)
	}
}
