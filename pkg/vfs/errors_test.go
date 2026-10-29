package vfs_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// The service's error set, written out by hand: a sentinel or a kind added
// to the package, or dropped from Errors, fails here until it is listed.
var sentinels = []error{
	vfs.ErrNotFound, vfs.ErrExist, vfs.ErrNotEmpty, vfs.ErrNotDir, vfs.ErrIsDir,
	vfs.ErrAccess, vfs.ErrPerm, vfs.ErrStale, vfs.ErrReadOnly, vfs.ErrNameTooLong,
	vfs.ErrInvalid, vfs.ErrNoSpace, vfs.ErrQuota, vfs.ErrLocked, vfs.ErrRangeNotLocked,
	vfs.ErrShareViolation, vfs.ErrDeletePending, vfs.ErrGrace, vfs.ErrDelay, vfs.ErrBlocked,
	vfs.ErrWrongSecurity, vfs.ErrState, vfs.ErrCross, vfs.ErrIncomplete, vfs.ErrInconsistent,
	vfs.ErrLost, vfs.ErrUnavailable, vfs.ErrCorrupt,
}

var wantErrors = append(slices.Clone(sentinels),
	&vfs.AccessError{Cause: vfs.AccessReadOnly},

	&vfs.QuotaError{Scope: vfs.QuotaShare},
	&vfs.QuotaError{Scope: vfs.QuotaProject},
	&vfs.QuotaError{Scope: vfs.QuotaUser},
	&vfs.QuotaError{Scope: vfs.QuotaGroup},

	&vfs.Delay{Cause: vfs.DelayRecall},
	&vfs.Delay{Cause: vfs.DelayClone},
	&vfs.Delay{Cause: vfs.DelayConflict},
	&vfs.Delay{Cause: vfs.DelayFrozen},
	&vfs.Delay{Cause: vfs.DelayCapacity},
	&vfs.Delay{Cause: vfs.DelayCommit},
	&vfs.Delay{Cause: vfs.DelayResources},
	&vfs.Delay{Cause: vfs.DelayFrozen, Moved: true},

	&vfs.StateError{Kind: vfs.StateClientInUse},
	&vfs.StateError{Kind: vfs.StateClientBusy},
	&vfs.StateError{Kind: vfs.StateRecordChanged},
	&vfs.StateError{Kind: vfs.StateAdapterVersion},
	&vfs.StateError{Kind: vfs.StateStaleClient},
	&vfs.StateError{Kind: vfs.StateStaleState},
	&vfs.StateError{Kind: vfs.StateOldState},
	&vfs.StateError{Kind: vfs.StateBadState},
	&vfs.StateError{Kind: vfs.StateNoReclaim},
	&vfs.StateError{Kind: vfs.StateReclaimConflict},
	&vfs.StateError{Kind: vfs.StateLocksHeld},
	&vfs.StateError{Kind: vfs.StateBadSeqID},
	&vfs.StateError{Kind: vfs.StateNoRecall},
	&vfs.StateError{Kind: vfs.StateCreateInUse},
	&vfs.StateError{Kind: vfs.StateNotYours},
	&vfs.StateError{Kind: vfs.StateRevoked},
	&vfs.StateError{Kind: vfs.StateRevoked, Admin: true},

	&vfs.CrossError{Kind: vfs.CrossNamespace},
	&vfs.CrossError{Kind: vfs.CrossShare},
	&vfs.CrossError{Kind: vfs.CrossProject},
	&vfs.CrossError{Kind: vfs.OtherShard},
)

func TestErrorsIsTheErrorSet(t *testing.T) {
	if got := vfs.Errors(); !reflect.DeepEqual(got, wantErrors) {
		t.Fatalf("Errors() =\n%v\nwant\n%v", got, wantErrors)
	}
}

// An adapter's table keys on each value, and its log names it: every value
// matches its own sentinel and no other, a sentinel being its own, and no two
// read alike.
func TestErrorsMatchTheirSentinelAndReadApart(t *testing.T) {
	seen := map[string]bool{}
	for _, err := range vfs.Errors() {
		msg := err.Error()
		if seen[msg] {
			t.Errorf("two errors read %q", msg)
		}
		seen[msg] = true

		var sentinel error
		switch err.(type) {
		case *vfs.AccessError:
			sentinel = vfs.ErrAccess
		case *vfs.QuotaError:
			sentinel = vfs.ErrQuota
		case *vfs.Delay:
			sentinel = vfs.ErrDelay
		case *vfs.StateError:
			sentinel = vfs.ErrState
		case *vfs.CrossError:
			sentinel = vfs.ErrCross
		default:
			sentinel = err
		}
		for _, other := range sentinels {
			if got, want := errors.Is(err, other), other == sentinel; got != want {
				t.Errorf("errors.Is(%v, %v) = %v", err, other, got)
			}
		}
	}
}

// A kind no name is declared for still reads as its sentinel's text, rather
// than panicking or ending in nothing.
func TestErrorsOfAnUndeclaredKindRead(t *testing.T) {
	for _, err := range []error{
		&vfs.AccessError{}, &vfs.QuotaError{}, &vfs.Delay{}, &vfs.Delay{Cause: 200},
		&vfs.StateError{}, &vfs.StateError{Kind: 200}, &vfs.CrossError{Kind: 200},
	} {
		if msg := err.Error(); !strings.HasSuffix(msg, ": unknown") {
			t.Errorf("%#v reads %q, want its sentinel's text and \": unknown\"", err, msg)
		}
	}
}
