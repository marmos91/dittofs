package format

import (
	"errors"
	"testing"
)

// TestRecordErrorClassesAreDistinct pins that the record decoder's two new
// classes are separate errors: an unknown kind is not damage, so a scan never
// mistakingly steps over a kind it does not know by a length it read, and a
// non-zero epoch is not damage, so recovery can tell a version this format
// forbids from a rotted byte.
func TestRecordErrorClassesAreDistinct(t *testing.T) {
	errs := []error{ErrDamaged, ErrWrongPlace, ErrUnknownKind, ErrNonZeroEpoch}
	for i, a := range errs {
		for j, b := range errs {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("%v is %v", a, b)
			}
			if a.Error() == b.Error() {
				t.Errorf("%v and %v read the same", a, b)
			}
		}
	}
}
