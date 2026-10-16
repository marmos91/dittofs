package journalv2

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// surfaceCall is one exported function or method, called with zero or minimal
// arguments. call returns the error the callee returned, or nil.
type surfaceCall struct {
	name string
	call func() error
}

// surface lists every exported function and method of the public package.
// Each one is added in the change that declares it.
var surface = []surfaceCall{
	{"NewFileID", func() error { _ = NewFileID([16]byte{}); return nil }},
	{"VersionFromBytes", func() error { _ = VersionFromBytes([16]byte{}); return nil }},
	{"Version.Compare", func() error { _ = Version{}.Compare(Version{}); return nil }},
	{"Version.Less", func() error { _ = Version{}.Less(Version{}); return nil }},
	{"Version.IsZero", func() error { _ = Version{}.IsZero(); return nil }},
	{"Version.Bytes", func() error { _ = Version{}.Bytes(); return nil }},
	{"FileID.Bytes", func() error { _ = FileID{}.Bytes(); return nil }},
	{"Extent.End", func() error { _ = Extent{}.End(); return nil }},
	{"Extent.Overlaps", func() error { _ = Extent{}.Overlaps(Extent{}); return nil }},
	{"Limit.String", func() error { _ = Limit(0).String(); return nil }},
	{"Unit.String", func() error { _ = Unit(0).String(); return nil }},
	{"NoSpaceError.Error", func() error { _ = (&NoSpaceError{}).Error(); return nil }},
	{"OtherShareError.Error", func() error { _ = (&OtherShareError{}).Error(); return nil }},
	{"IdentityError.Error", func() error { _ = (&IdentityError{}).Error(); return nil }},
	{"SyncError.Error", func() error { _ = (&SyncError{}).Error(); return nil }},
	{"NotOffloadedError.Error", func() error { _ = (&NotOffloadedError{}).Error(); return nil }},
	{"Init", func() error { _, err := Init("", InstallationID{}); return err }},
	{"Open", func() error { _, err := Open("", Version{}, JournalID{}); return err }},
	{"Restamp", func() error { return Restamp("", JournalID{}, InstallationID{}) }},
	{"Journal.Close", func() error { return (&Journal{}).Close() }},
	{"WithClock", func() error { WithClock(nil)(&core.Config{}); return nil }},
	{"WithStorage", func() error { WithStorage(nil)(&core.Config{}); return nil }},
	{"WithSettings", func() error { WithSettings(Settings{})(&core.Config{}); return nil }},
	{"WithDescriptorBudget", func() error { WithDescriptorBudget(nil)(&core.Config{}); return nil }},
	{"NewDescriptorBudget", func() error { _ = NewDescriptorBudget(0); return nil }},
}

// TestSurface calls every entry of surface and asserts that it returns or
// errors cleanly: a panic fails the entry by name instead of aborting the test
// binary. The assertion holds both for a stub and for an implementation, so the
// table never needs to change when a method is implemented.
func TestSurface(t *testing.T) {
	for _, c := range surface {
		t.Run(c.name, func(t *testing.T) {
			if err := callClean(c.call); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// callClean runs call and converts a panic into an error.
func callClean(call func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panicked: %v", r)
		}
	}()

	_ = call()
	return nil
}
