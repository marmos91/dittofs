package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyIdentityMapping_AllSquash_ClearsNamedPrincipal is the
// fails-before / passes-after regression test for the all_squash bypass.
//
// Before the fix, Username and Domain survived MapAllToAnonymous, letting
// named-principal ACEs (e.g. "alice@EXAMPLE") still match squashed clients.
func TestApplyIdentityMapping_AllSquash_ClearsNamedPrincipal(t *testing.T) {
	t.Parallel()

	anonUID := uint32(65534)
	anonGID := uint32(65534)
	anonSID := "S-1-5-7"

	identity := &Identity{
		UID:       Uint32Ptr(1000),
		GID:       Uint32Ptr(1000),
		GIDs:      []uint32{100, 200},
		SID:       StringPtr("S-1-5-21-123-456-789-1001"),
		GroupSIDs: []string{"S-1-5-32-545"},
		Username:  "alice",
		Domain:    "EXAMPLE",
	}
	mapping := &IdentityMapping{
		MapAllToAnonymous: true,
		AnonymousUID:      &anonUID,
		AnonymousGID:      &anonGID,
		AnonymousSID:      &anonSID,
	}

	result := ApplyIdentityMapping(identity, mapping)

	// Numeric / SID fields must be replaced with anonymous values.
	require.NotNil(t, result.UID)
	assert.Equal(t, anonUID, *result.UID, "UID must be anonymous")
	require.NotNil(t, result.GID)
	assert.Equal(t, anonGID, *result.GID, "GID must be anonymous")
	require.NotNil(t, result.SID)
	assert.Equal(t, anonSID, *result.SID, "SID must be anonymous")

	// Supplementary groups must be cleared.
	assert.Nil(t, result.GIDs, "GIDs must be cleared by all_squash")
	assert.Nil(t, result.GroupSIDs, "GroupSIDs must be cleared by all_squash")

	// Named-principal fields must be empty so ACE matching cannot bypass squash.
	assert.Empty(t, result.Username, "Username must be cleared by all_squash")
	assert.Empty(t, result.Domain, "Domain must be cleared by all_squash")
}
