package handlers

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

// TestAuthDenialStatus verifies that a share-permission denial maps to
// NFS3ErrAccess (EACCES "permission denied") while every other auth-context
// failure stays NFS3ErrIO. This is the regression guard for the NFSv3 bug where
// an export-gate denial surfaced to the client as "Input/output error" instead
// of "Permission denied" (the NFSv4 path already mapped it to NFS4ERR_ACCESS).
func TestAuthDenialStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want uint32
	}{
		{
			name: "share access denied -> EACCES",
			err:  ErrShareAccessDenied,
			want: types.NFS3ErrAccess,
		},
		{
			name: "wrapped share access denied -> EACCES",
			err:  fmt.Errorf("build auth context: %w", ErrShareAccessDenied),
			want: types.NFS3ErrAccess,
		},
		{
			name: "generic internal fault -> EIO",
			err:  errors.New("share not found"),
			want: types.NFS3ErrIO,
		},
		{
			name: "identity-mapping failure -> EIO",
			err:  fmt.Errorf("failed to apply identity mapping: %w", errors.New("share %q not found")),
			want: types.NFS3ErrIO,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := authDenialStatus(tc.err); got != tc.want {
				t.Fatalf("authDenialStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
