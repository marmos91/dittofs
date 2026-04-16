package destination

import (
	"errors"
	"fmt"
	"testing"
)

// allSentinels returns every D-07 sentinel exported by this package.
// Any new sentinel added to errors.go must be appended here so the
// distinct-identity test below continues to cover the full surface.
func allSentinels() []error {
	return []error{
		// Transient / retryable
		ErrDestinationUnavailable,
		ErrDestinationThrottled,
		// Permanent / do-not-retry
		ErrIncompatibleConfig,
		ErrPermissionDenied,
		ErrDuplicateBackupID,
		ErrSHA256Mismatch,
		ErrManifestMissing,
		ErrEncryptionKeyMissing,
		ErrInvalidKeyMaterial,
		ErrDecryptFailed,
		ErrIncompleteBackup,
	}
}

// TestSentinels_DistinctIdentity asserts that each sentinel compares equal
// under errors.Is only to itself. If two sentinels shared identity, callers
// using errors.Is to branch on retry-vs-no-retry would misroute errors.
func TestSentinels_DistinctIdentity(t *testing.T) {
	sentinels := allSentinels()
	for i, a := range sentinels {
		for j, b := range sentinels {
			got := errors.Is(a, b)
			want := i == j
			if got != want {
				t.Errorf("errors.Is(%q, %q) = %v; want %v", a, b, got, want)
			}
		}
	}
}

// TestSentinels_WrappingPreservesIdentity asserts that the standard
// fmt.Errorf("...: %w", sentinel) wrap idiom keeps errors.Is detectable.
// This is the contract every call site (D-07) relies on.
func TestSentinels_WrappingPreservesIdentity(t *testing.T) {
	for _, s := range allSentinels() {
		wrapped := fmt.Errorf("context for %s: %w", "some-detail", s)
		if !errors.Is(wrapped, s) {
			t.Errorf("errors.Is(wrapped, %q) = false; want true", s)
		}
	}
}

// TestSentinels_StableMessages spot-checks three sentinel messages. Their
// exact text is part of the public API (operators grep logs, tests assert
// substrings) so any wording change must be deliberate.
func TestSentinels_StableMessages(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrIncompatibleConfig, "incompatible destination config"},
		{ErrSHA256Mismatch, "sha256 mismatch on read-back"},
		{ErrDecryptFailed, "decrypt failed (wrong key, tampered, or truncated)"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%T.Error() = %q; want %q", tc.err, got, tc.want)
		}
	}
}
