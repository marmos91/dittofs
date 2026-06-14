package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMatchesIPPattern_BareIPOnly documents the existing contract:
// MatchesIPPattern expects a bare IP, not "IP:port".
// This is the unit-level anchor; the integration-level test (in package
// metadata_test) covers CheckShareAccess stripping the port before calling
// this function.
func TestMatchesIPPattern_BareIPOnly(t *testing.T) {
	t.Parallel()

	// Sanity: bare IP matches CIDR — must continue to work.
	assert.True(t, MatchesIPPattern("192.168.1.100", "192.168.1.0/24"))
	// Sanity: bare IP matches exact.
	assert.True(t, MatchesIPPattern("192.168.1.100", "192.168.1.100"))

	// Before the fix in CheckShareAccess, "IP:port" strings reached
	// MatchesIPPattern and silently failed. These two assertions codify the
	// expected behaviour of MatchesIPPattern itself (it does not strip ports —
	// that is the caller's job).
	assert.False(t, MatchesIPPattern("192.168.1.100:54321", "192.168.1.0/24"),
		"MatchesIPPattern must not accept IP:port — stripping is the caller's job")
	assert.False(t, MatchesIPPattern("192.168.1.100:54321", "192.168.1.100"),
		"MatchesIPPattern must not accept IP:port — stripping is the caller's job")
}
