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

// TestFreeAll_OverlongNameRejected caps the caller name so a malicious peer
// cannot inflate logs / force expensive cleanup with a giant XDR string.
func TestFreeAll_OverlongNameRejected(t *testing.T) {
	svc := &recordingLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(DefaultBlockingQueueSize))

	calls := 0
	h.SetCrashCleanup(func(string) { calls++ })

	huge := make([]byte, MaxCallerNameLen+1)
	for i := range huge {
		huge[i] = 'a'
	}
	ctx := &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.7:51234",
		Data:       encodeFreeAllWire(string(huge), 0),
	}
	if _, err := h.FreeAll(ctx); err != nil {
		t.Fatalf("FreeAll error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("crash cleanup invoked %d times for over-long name, want 0", calls)
	}
}

// TestFreeAll_SpoofedNameFromDifferentHostIsRejected is the negative control for
// the FREE_ALL lock-theft vector: a victim on host-a holds locks under
// caller_name "victim"; an attacker on host-b sends FREE_ALL("victim") and must
// NOT trigger cleanup of the victim's locks.
func TestFreeAll_SpoofedNameFromDifferentHostIsRejected(t *testing.T) {
	svc := &recordingLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(DefaultBlockingQueueSize))

	calls := 0
	var gotClient string
	h.SetCrashCleanup(func(clientID string) {
		calls++
		gotClient = clientID
	})

	// Victim binds caller_name "victim" from host-a via a successful LOCK.
	victimCtx := &NLMHandlerContext{Context: context.Background(), ClientAddr: "10.0.0.7:51234"}
	if _, err := h.Lock(victimCtx, lockReq("victim")); err != nil {
		t.Fatalf("victim Lock error: %v", err)
	}

	// Attacker on host-b spoofs FREE_ALL for "victim".
	attackerCtx := &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.99:40000",
		Data:       encodeFreeAllWire("victim", 0),
	}
	if _, err := h.FreeAll(attackerCtx); err != nil {
		t.Fatalf("attacker FreeAll error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("attacker FREE_ALL triggered cleanup %d times (client %q), want 0", calls, gotClient)
	}

	// Legitimate FREE_ALL from the victim's host still cleans up.
	victimFree := &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.7:33333", // same host, different ephemeral port (reboot)
		Data:       encodeFreeAllWire("victim", 0),
	}
	if _, err := h.FreeAll(victimFree); err != nil {
		t.Fatalf("victim FreeAll error: %v", err)
	}
	if calls != 1 || gotClient != "victim" {
		t.Fatalf("legitimate FREE_ALL cleanup = %d times (client %q), want 1 / victim", calls, gotClient)
	}
}
