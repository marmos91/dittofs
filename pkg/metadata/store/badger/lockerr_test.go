package badger

import (
	"errors"
	"testing"
)

// TestIsDirLockErr checks that BadgerDB's directory-lock failure (a second server
// opening the same data dir) is recognized so the store can surface an actionable
// error instead of the raw "resource temporarily unavailable".
func TestIsDirLockErr(t *testing.T) {
	locked := []error{
		errors.New(`Cannot acquire directory lock on "/var/lib/dittofs/meta".  Another process is using this Badger database. error: resource temporarily unavailable`),
		errors.New("resource temporarily unavailable"),
		errors.New("Another process is using this Badger database"),
	}
	for _, e := range locked {
		if !isDirLockErr(e) {
			t.Errorf("expected lock error to be detected: %v", e)
		}
	}
	notLocked := []error{
		errors.New("failed to open BadgerDB: manifest corrupted"),
		errors.New("no space left on device"),
	}
	for _, e := range notLocked {
		if isDirLockErr(e) {
			t.Errorf("did not expect lock detection for: %v", e)
		}
	}
}
