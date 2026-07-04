package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

func enumerateHandles(t *testing.T, sm *StateManager) map[string]int {
	t.Helper()
	got := make(map[string]int)
	if err := sm.EnumerateOpenFiles(context.Background(), func(fh []byte) error {
		got[string(fh)]++
		return nil
	}); err != nil {
		t.Fatalf("EnumerateOpenFiles: %v", err)
	}
	return got
}

// TestEnumerateOpenFiles_OpenThenClose verifies the enumeration reflects the
// live open-state table: a file appears exactly once while open (regardless
// of how many opens it has) and disappears after the last CLOSE — which is
// what releases the block-GC open-handle hold (#1448).
func TestEnumerateOpenFiles_OpenThenClose(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	fhA := []byte("fh-enum-a")
	fhB := []byte("fh-enum-b")

	sidA := openConfirmed(t, sm, 0, []byte("owner-enum-a"), fhA,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)
	openConfirmed(t, sm, 0, []byte("owner-enum-b1"), fhB,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)
	openConfirmed(t, sm, 0, []byte("owner-enum-b2"), fhB,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)

	got := enumerateHandles(t, sm)
	if len(got) != 2 {
		t.Fatalf("open files = %d, want 2 (%v)", len(got), got)
	}
	if got[string(fhA)] != 1 || got[string(fhB)] != 1 {
		t.Fatalf("each file must be emitted exactly once, got %v", got)
	}

	// Last close of A drops it from the enumeration.
	if _, err := sm.CloseFile(&sidA, 3); err != nil {
		t.Fatalf("CloseFile: %v", err)
	}
	got = enumerateHandles(t, sm)
	if len(got) != 1 || got[string(fhB)] != 1 {
		t.Fatalf("after close: got %v, want only %q", got, fhB)
	}
}

// TestEnumerateOpenFiles_CallbackErrorPropagates verifies fn errors abort the
// enumeration (the GC hold consumer fails closed on them).
func TestEnumerateOpenFiles_CallbackErrorPropagates(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	openConfirmed(t, sm, 0, []byte("owner-err"), []byte("fh-err"),
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE)

	boom := errors.New("boom")
	if err := sm.EnumerateOpenFiles(context.Background(), func([]byte) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}
