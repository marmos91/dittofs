package journalv2

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

// sentinels lists every sentinel of the public package.
var sentinels = []error{
	ErrNotImplemented, ErrClosed, ErrHandleOpen, ErrJournalExists, ErrNoJournal,
	ErrIdentityMismatch, ErrUnknownFormat, ErrCorrupt, ErrStaleFill, ErrNotOffloaded,
	ErrNotOffered, ErrOfferEnded, ErrZeroLength, ErrSyncFailed, ErrNoSpace, ErrOtherShare,
}

// TestErrorRelations checks the relations callers rely on: each typed error
// matches its own sentinel and no other, survives wrapping for errors.As, and
// a full filesystem is never mistaken for one of the journal's limits.
func TestErrorRelations(t *testing.T) {
	cause := errors.New("device error")
	typed := []struct {
		name string
		err  error
		is   error
		as   func(error) bool
	}{
		{"NoSpaceError", &NoSpaceError{Limit: LimitShare, Missing: 4096, Unit: UnitBytes}, ErrNoSpace,
			func(err error) bool { var e *NoSpaceError; return errors.As(err, &e) && e.Missing == 4096 }},
		{"OtherShareError", &OtherShareError{Tag: 1, Owner: 2}, ErrOtherShare,
			func(err error) bool { var e *OtherShareError; return errors.As(err, &e) && e.Owner == 2 }},
		{"IdentityError", &IdentityError{Segments: true}, ErrIdentityMismatch,
			func(err error) bool { var e *IdentityError; return errors.As(err, &e) && e.Segments }},
		{"SyncError", &SyncError{Segment: 7, Err: cause}, ErrSyncFailed,
			func(err error) bool { var e *SyncError; return errors.As(err, &e) && e.Segment == 7 }},
		{"NotOffloadedError", &NotOffloadedError{Extents: []Extent{{Off: 0, Len: 1}}}, ErrNotOffloaded,
			func(err error) bool { var e *NotOffloadedError; return errors.As(err, &e) && len(e.Extents) == 1 }},
	}

	for _, tc := range typed {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("op: %w", tc.err)
			for _, s := range sentinels {
				if got, want := errors.Is(wrapped, s), s == tc.is; got != want {
					t.Errorf("errors.Is(%s, %q) = %t, want %t", tc.name, s, got, want)
				}
			}
			if !tc.as(wrapped) {
				t.Errorf("errors.As does not recover %s through wrapping", tc.name)
			}
			if tc.err.Error() == "" {
				t.Errorf("%s has an empty message", tc.name)
			}
			if errors.Is(wrapped, syscall.ENOSPC) {
				t.Errorf("%s matches syscall.ENOSPC", tc.name)
			}
		})
	}

	t.Run("SyncError unwraps to its cause", func(t *testing.T) {
		if !errors.Is(&SyncError{Err: cause}, cause) {
			t.Error("errors.Is(SyncError, cause) = false")
		}
	})

	t.Run("full filesystem is not a journal limit", func(t *testing.T) {
		full := &fs.PathError{Op: "write", Path: "0001.seg", Err: syscall.ENOSPC}
		if !errors.Is(full, syscall.ENOSPC) {
			t.Fatal("the platform error does not match syscall.ENOSPC")
		}
		for _, s := range sentinels {
			if errors.Is(full, s) {
				t.Errorf("a full filesystem matches %q", s)
			}
		}
		var e *NoSpaceError
		if errors.As(full, &e) {
			t.Error("a full filesystem is a NoSpaceError")
		}
	})

	t.Run("sentinels are distinct", func(t *testing.T) {
		for i, a := range sentinels {
			for _, b := range sentinels[i+1:] {
				if errors.Is(a, b) || errors.Is(b, a) {
					t.Errorf("%q and %q match each other", a, b)
				}
			}
		}
	})
}

// TestLimitAndUnitNames checks that every limit and unit has its own name, and
// that the zero value names none of them.
func TestLimitAndUnitNames(t *testing.T) {
	names := map[string]bool{}
	for _, l := range []Limit{LimitShare, LimitJournal, LimitIndex, LimitHeadroom, LimitVersion, LimitSpare} {
		names[l.String()] = true
	}
	if len(names) != 6 || names[""] || names[Limit(0).String()] {
		t.Errorf("limit names are not six distinct names apart from the zero Limit's: %v", names)
	}

	names = map[string]bool{}
	for _, u := range []Unit{UnitBytes, UnitEntries, UnitVersions, UnitSegments} {
		names[u.String()] = true
	}
	if len(names) != 4 || names[""] || names[Unit(0).String()] {
		t.Errorf("unit names are not four distinct names apart from the zero Unit's: %v", names)
	}
}
