package gss

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	pkgidentity "github.com/marmos91/dittofs/pkg/identity"
)

// stubProvider resolves every credential to a fixed identity, so the test
// exercises resolveIdentity's conversion rather than the resolver's chain.
type stubProvider struct {
	resolved *pkgidentity.ResolvedIdentity
}

func (s *stubProvider) Name() string                              { return "kerberos" }
func (s *stubProvider) CanResolve(_ *pkgidentity.Credential) bool { return true }

func (s *stubProvider) Resolve(_ context.Context, _ *pkgidentity.Credential) (*pkgidentity.ResolvedIdentity, error) {
	return s.resolved, nil
}

// TestResolveIdentity_CarriesSIDAndGroupSIDs pins the Windows identity across
// the Kerberos resolver path. The resolver populates SID/GroupSIDs from the
// persisted user record, and ACL evaluation matches ACEs against them
// (pkg/metadata/acl/evaluate.go). Dropping them here fails closed: a principal
// holding a SID-keyed grant over SMB would be denied the same grant over NFS.
func TestResolveIdentity_CarriesSIDAndGroupSIDs(t *testing.T) {
	const (
		userSID  = "S-1-5-21-3623811015-3361044348-30300820-1013"
		groupSID = "S-1-5-21-3623811015-3361044348-30300820-513"
	)

	provider := &stubProvider{resolved: &pkgidentity.ResolvedIdentity{
		Username:  "alice",
		UID:       1001,
		GID:       1002,
		Domain:    "EXAMPLE.COM",
		Found:     true,
		SID:       userSID,
		GroupSIDs: []string{groupSID},
	}}

	p := NewGSSProcessor(nil, nil, 16, 0)
	p.SetResolver(pkgidentity.NewResolver(pkgidentity.WithProvider(provider)))

	identity, err := p.resolveIdentity(context.Background(), "alice", "EXAMPLE.COM")
	require.NoError(t, err)
	require.NotNil(t, identity)

	require.NotNil(t, identity.SID, "SID must survive the resolver conversion")
	require.Equal(t, userSID, *identity.SID)
	require.Equal(t, []string{groupSID}, identity.GroupSIDs)

	// The Unix half must be unaffected by the added fields.
	require.Equal(t, uint32(1001), *identity.UID)
	require.Equal(t, uint32(1002), *identity.GID)
	require.Equal(t, "alice", identity.Username)
	require.Equal(t, "EXAMPLE.COM", identity.Domain)
}

// TestResolveIdentity_NoSIDLeavesSIDNil pins the POSIX-only case: a resolved
// user with no Windows identity must not gain a fabricated one.
func TestResolveIdentity_NoSIDLeavesSIDNil(t *testing.T) {
	provider := &stubProvider{resolved: &pkgidentity.ResolvedIdentity{
		Username: "bob",
		UID:      2001,
		GID:      2002,
		Found:    true,
	}}

	p := NewGSSProcessor(nil, nil, 16, 0)
	p.SetResolver(pkgidentity.NewResolver(pkgidentity.WithProvider(provider)))

	identity, err := p.resolveIdentity(context.Background(), "bob", "EXAMPLE.COM")
	require.NoError(t, err)
	require.Nil(t, identity.SID, "an empty SID must stay nil, not become a pointer to empty string")
	require.Empty(t, identity.GroupSIDs)
}
