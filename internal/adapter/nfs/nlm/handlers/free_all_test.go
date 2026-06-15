package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
)

// encodeFreeAllWire builds the XDR wire form of an NLM4_FREE_ALL request:
// a variable-length string (4-byte length + bytes + 4-byte padding) followed by
// an int32 state.
func encodeFreeAllWire(name string, state int32) []byte {
	var b []byte
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(name)))
	b = append(b, lenBuf...)
	b = append(b, []byte(name)...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	stateBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(stateBuf, uint32(state))
	b = append(b, stateBuf...)
	return b
}

// TestFreeAll_InvokesCrashCleanup is the negative control for the FREE_ALL
// no-op finding. FREE_ALL must release the named client's locks by invoking the
// wired crash-cleanup callback. Before the fix the handler only logged, so no
// cleanup callback was ever called and a crashed client's locks survived until
// (and only if) a separate SM_NOTIFY arrived.
func TestFreeAll_InvokesCrashCleanup(t *testing.T) {
	svc := &recordingLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(DefaultBlockingQueueSize))

	var gotClient string
	calls := 0
	h.SetCrashCleanup(func(clientID string) {
		calls++
		gotClient = clientID
	})

	ctx := &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.7:51234",
		Data:       encodeFreeAllWire("host-a", 7),
	}

	if _, err := h.FreeAll(ctx); err != nil {
		t.Fatalf("FreeAll error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("crash cleanup invoked %d times, want 1", calls)
	}
	if gotClient != "host-a" {
		t.Fatalf("crash cleanup client = %q, want %q", gotClient, "host-a")
	}
}

// TestFreeAll_EmptyNameDoesNotCleanup ensures an empty caller name is ignored
// rather than triggering a release for the empty client (which could match an
// unintended set).
func TestFreeAll_EmptyNameDoesNotCleanup(t *testing.T) {
	svc := &recordingLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(DefaultBlockingQueueSize))

	calls := 0
	h.SetCrashCleanup(func(string) { calls++ })

	ctx := &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.7:51234",
		Data:       encodeFreeAllWire("", 0),
	}
	if _, err := h.FreeAll(ctx); err != nil {
		t.Fatalf("FreeAll error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("crash cleanup invoked %d times for empty name, want 0", calls)
	}
}
