package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// newTestPipeRead builds a PendingPipeRead. If done is non-nil it receives the
// callback status exactly once, letting tests wait deterministically for an
// async displaced-callback completion.
func newTestPipeRead(fileID byte, sessionID, messageID, asyncId uint64, done chan<- types.Status) *PendingPipeRead {
	var fid [16]byte
	fid[0] = fileID
	return &PendingPipeRead{
		FileID:    fid,
		SessionID: sessionID,
		MessageID: messageID,
		AsyncId:   asyncId,
		Callback: func(_, _, _ uint64, st types.Status, _ []byte) error {
			if done != nil {
				done <- st
			}
			return nil
		},
	}
}

func TestPipeReadRegistry_RegisterAndUnregisterByFileID(t *testing.T) {
	r := NewPipeReadRegistry()
	p := newTestPipeRead(1, 10, 100, 1000, nil)
	r.Register(p)

	if got := r.UnregisterByMessageID(100); got != p {
		t.Fatalf("UnregisterByMessageID = %v, want p", got)
	}
	// Now gone from all indexes.
	if got := r.UnregisterByFileID(p.FileID); got != nil {
		t.Fatalf("UnregisterByFileID after removal = %v, want nil", got)
	}
	if got := r.UnregisterByAsyncId(1000); got != nil {
		t.Fatalf("UnregisterByAsyncId after removal = %v, want nil", got)
	}
}

// TestPipeReadRegistry_RegisterDisplacesSameFileID verifies that registering a
// second read for the same FileID cancels the prior one (STATUS_CANCELLED) so
// async slots are not leaked, matching the one-pending-read-per-handle rule.
func TestPipeReadRegistry_RegisterDisplacesSameFileID(t *testing.T) {
	r := NewPipeReadRegistry()
	done := make(chan types.Status, 1)
	old := newTestPipeRead(1, 10, 100, 1000, done)
	r.Register(old)

	newer := newTestPipeRead(1, 10, 101, 1001, nil) // same FileID
	r.Register(newer)

	// The displaced entry's callback fires asynchronously with STATUS_CANCELLED.
	select {
	case st := <-done:
		if st != types.StatusCancelled {
			t.Fatalf("displaced callback status = %d, want StatusCancelled", st)
		}
	case <-time.After(time.Second):
		t.Fatal("displaced callback did not fire within 1s")
	}

	// Only the newer entry remains; old indexes are gone.
	if got := r.UnregisterByAsyncId(1000); got != nil {
		t.Errorf("old entry still present: %v", got)
	}
	if got := r.UnregisterByFileID(newer.FileID); got != newer {
		t.Errorf("UnregisterByFileID = %v, want newer", got)
	}
}

func TestPipeReadRegistry_UnregisterAllForSession(t *testing.T) {
	r := NewPipeReadRegistry()
	r.Register(newTestPipeRead(1, 10, 100, 1000, nil))
	r.Register(newTestPipeRead(2, 10, 101, 1001, nil))
	r.Register(newTestPipeRead(3, 11, 102, 1002, nil)) // different session

	got := r.UnregisterAllForSession(10)
	if len(got) != 2 {
		t.Fatalf("UnregisterAllForSession(10) = %d, want 2", len(got))
	}
	// Session 11 entry survives.
	if got := r.UnregisterByAsyncId(1002); got == nil {
		t.Errorf("session 11 entry should survive teardown of session 10")
	}
}
