package journalv2

import (
	"errors"
	"testing"
)

// TestTypedErrorsMatchTheirSentinel catches the one bug an Is method of a
// typed error can have: matching another error's sentinel.
func TestTypedErrorsMatchTheirSentinel(t *testing.T) {
	for err, want := range map[error]error{
		&NoSpaceError{}:      ErrNoSpace,
		&OtherShareError{}:   ErrOtherShare,
		&IdentityError{}:     ErrIdentityMismatch,
		&SyncError{}:         ErrSyncFailed,
		&NotOffloadedError{}: ErrNotOffloaded,
	} {
		if !errors.Is(err, want) {
			t.Errorf("%T does not match %v", err, want)
		}
	}
}
