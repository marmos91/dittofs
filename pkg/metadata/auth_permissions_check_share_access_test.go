package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckShareAccess_IPACLWithPort verifies that DeniedClients and
// AllowedClients match correctly when clientAddr carries a port suffix (the
// normal protocol-handler format, "IP:port"). Before the fix, CheckShareAccess
// passed the raw "IP:port" string to MatchesIPPattern, which never matched a
// bare-IP or CIDR pattern — silently turning every IP ACL entry into a no-op.
func TestCheckShareAccess_IPACLWithPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		clientAddr  string // as supplied by protocol handler ("IP:port" or bare IP)
		denied      []string
		allowed     []string
		wantAllowed bool
	}{
		{
			name:        "denied CIDR matches IP:port client",
			clientAddr:  "192.168.1.50:54321",
			denied:      []string{"192.168.1.0/24"},
			wantAllowed: false, // FAILS before fix; passes after
		},
		{
			name:        "allowed CIDR matches IP:port client",
			clientAddr:  "192.168.1.50:54321",
			allowed:     []string{"192.168.1.0/24"},
			wantAllowed: true, // FAILS before fix; passes after
		},
		{
			name:        "denied exact IP matches IP:port client",
			clientAddr:  "10.0.0.5:12345",
			denied:      []string{"10.0.0.5"},
			wantAllowed: false, // FAILS before fix; passes after
		},
		{
			name:        "bare IP client still works (no port)",
			clientAddr:  "192.168.1.50",
			allowed:     []string{"192.168.1.0/24"},
			wantAllowed: true, // must not regress
		},
		{
			name:        "IPv6 with port — denied",
			clientAddr:  "[::1]:54321",
			denied:      []string{"::1"},
			wantAllowed: false, // FAILS before fix; passes after
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFixture(t)
			ctx := context.Background()

			// CreateRootDirectory in the fixture builds the file tree but does
			// not register a share record, so GetShareOptions/CheckShareAccess
			// would fail with "share not found". Register the share with the
			// ACL lists under test.
			require.NoError(t, f.store.CreateShare(ctx, &metadata.Share{
				Name: f.shareName,
				Options: metadata.ShareOptions{
					DeniedClients:  tt.denied,
					AllowedClients: tt.allowed,
				},
			}))

			decision, _, err := f.service.CheckShareAccess(
				ctx, f.shareName, tt.clientAddr, "unix",
				&metadata.Identity{UID: metadata.Uint32Ptr(1000)},
			)
			require.NoError(t, err)
			require.NotNil(t, decision)
			assert.Equal(t, tt.wantAllowed, decision.Allowed,
				"client %q with denied=%v allowed=%v: reason=%q",
				tt.clientAddr, tt.denied, tt.allowed, decision.Reason)
		})
	}
}
