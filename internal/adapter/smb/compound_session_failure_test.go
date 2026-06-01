package smb

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestRelatedSessionFailureStatus pins the compound related-command failure
// propagation: a NETWORK_SESSION_EXPIRED predecessor propagates SESSION_EXPIRED
// to its related followers (smbtorture smb2.session.expire2s/expire2e drive a
// CREATE+QUERY_DIRECTORY+CLOSE compound on an expired session and assert every
// member returns SESSION_EXPIRED), while every other session/tree-level failure
// collapses to INVALID_PARAMETER because the follower has no valid context to
// inherit (MS-SMB2 §3.3.5.2.7.2).
func TestRelatedSessionFailureStatus(t *testing.T) {
	cases := []struct {
		name string
		prev types.Status
		want types.Status
	}{
		{"session expired propagates", types.StatusNetworkSessionExpired, types.StatusNetworkSessionExpired},
		{"user session deleted collapses", types.StatusUserSessionDeleted, types.StatusInvalidParameter},
		{"network name deleted collapses", types.StatusNetworkNameDeleted, types.StatusInvalidParameter},
		{"zero status collapses", 0, types.StatusInvalidParameter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relatedSessionFailureStatus(tc.prev); got != tc.want {
				t.Fatalf("relatedSessionFailureStatus(%v) = %v, want %v", tc.prev, got, tc.want)
			}
		})
	}
}
