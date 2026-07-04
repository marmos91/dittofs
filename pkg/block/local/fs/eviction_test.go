package fs

import (
	"testing"
	"time"
)

// Access-tracker bookkeeping tests.
//
// The per-chunk cas-file LRU that this file used to cover was removed together
// with the legacy per-chunk storage path (blocks-only migration): chunks now
// live in the log-blob substrate and are accounted via logBlobDiskUsed, with
// blob-level eviction handled separately. accessTracker itself is still used by
// the store for last-access bookkeeping, so its tests are retained here.

func TestAccessTracker_Touch(t *testing.T) {
	at := newAccessTracker()
	before := time.Now()
	at.Touch("file1")
	after := time.Now()
	lastAccess := at.LastAccess("file1")
	if lastAccess.Before(before) || lastAccess.After(after) {
		t.Errorf("expected lastAccess between %v and %v, got %v", before, after, lastAccess)
	}
}

func TestAccessTracker_LastAccess_ZeroForUntouched(t *testing.T) {
	at := newAccessTracker()
	lastAccess := at.LastAccess("never-touched")
	if !lastAccess.IsZero() {
		t.Errorf("expected zero time for untouched file, got %v", lastAccess)
	}
}

func TestAccessTracker_Remove(t *testing.T) {
	at := newAccessTracker()
	at.Touch("file1")
	at.Remove("file1")
	if !at.LastAccess("file1").IsZero() {
		t.Error("expected zero time after Remove")
	}
}
