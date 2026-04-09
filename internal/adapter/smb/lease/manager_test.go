package lease

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// fakeLockManager is a minimal recording fake for lock.LockManager. It embeds
// the interface so any method we don't need is implicitly nil and will panic
// if a test path unexpectedly calls it.
type fakeLockManager struct {
	lock.LockManager // embedded interface; unimplemented methods will panic if called

	mu sync.Mutex

	breakHandleCalls           []breakCall
	breakReadCalls             []breakCall
	waitForBreakCompletionKeys []string
	// callOrder records the relative order of all observed calls so tests can
	// assert that BreakHandleLeasesForSMBOpen / BreakReadLeasesForParentDir
	// happen BEFORE WaitForBreakCompletion returns.
	callOrder []string

	// waitBlock, if non-nil, blocks WaitForBreakCompletion until closed
	// (used to assert no-deadlock behavior in the exclude-triggering-client test).
	waitBlock chan struct{}
}

type breakCall struct {
	HandleKey    string
	ExcludeOwner *lock.LockOwner
}

func (f *fakeLockManager) BreakHandleLeasesForSMBOpen(handleKey string, excludeOwner *lock.LockOwner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakHandleCalls = append(f.breakHandleCalls, breakCall{HandleKey: handleKey, ExcludeOwner: excludeOwner})
	f.callOrder = append(f.callOrder, "BreakHandleLeasesForSMBOpen")
	return nil
}

func (f *fakeLockManager) BreakReadLeasesForParentDir(handleKey string, excludeOwner *lock.LockOwner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakReadCalls = append(f.breakReadCalls, breakCall{HandleKey: handleKey, ExcludeOwner: excludeOwner})
	f.callOrder = append(f.callOrder, "BreakReadLeasesForParentDir")
	return nil
}

func (f *fakeLockManager) WaitForBreakCompletion(ctx context.Context, handleKey string) error {
	f.mu.Lock()
	f.waitForBreakCompletionKeys = append(f.waitForBreakCompletionKeys, handleKey)
	f.callOrder = append(f.callOrder, "WaitForBreakCompletion")
	wb := f.waitBlock
	f.mu.Unlock()
	if wb != nil {
		select {
		case <-wb:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// fakeResolver returns the same fakeLockManager for any share name.
type fakeResolver struct {
	mgr lock.LockManager
}

func (r *fakeResolver) GetLockManagerForShare(_ string) lock.LockManager {
	return r.mgr
}

// TestBreakParentHandleLeasesOnCreate_WaitsForAck asserts that
// BreakParentHandleLeasesOnCreate calls WaitForBreakCompletion AFTER
// BreakHandleLeasesForSMBOpen and BEFORE returning. Per MS-SMB2 3.3.4.7, the
// server must wait for LEASE_BREAK_ACK before completing the triggering CREATE.
func TestBreakParentHandleLeasesOnCreate_WaitsForAck(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentHandle := lock.FileHandle("parent-dir-handle")
	if err := lm.BreakParentHandleLeasesOnCreate(context.Background(), parentHandle, "share1", "smb:A"); err != nil {
		t.Fatalf("BreakParentHandleLeasesOnCreate returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakHandleCalls); got != 1 {
		t.Fatalf("BreakHandleLeasesForSMBOpen call count = %d, want 1", got)
	}
	if fake.breakHandleCalls[0].HandleKey != string(parentHandle) {
		t.Errorf("BreakHandleLeasesForSMBOpen handleKey = %q, want %q",
			fake.breakHandleCalls[0].HandleKey, string(parentHandle))
	}

	if got := len(fake.waitForBreakCompletionKeys); got != 1 {
		t.Fatalf("WaitForBreakCompletion call count = %d, want 1 (parent break must wait for ack per MS-SMB2 3.3.4.7)", got)
	}
	if fake.waitForBreakCompletionKeys[0] != string(parentHandle) {
		t.Errorf("WaitForBreakCompletion handleKey = %q, want %q",
			fake.waitForBreakCompletionKeys[0], string(parentHandle))
	}

	// Order: break must come before wait.
	if len(fake.callOrder) < 2 {
		t.Fatalf("expected at least 2 calls in order, got %v", fake.callOrder)
	}
	if fake.callOrder[0] != "BreakHandleLeasesForSMBOpen" {
		t.Errorf("first call = %q, want BreakHandleLeasesForSMBOpen", fake.callOrder[0])
	}
	if fake.callOrder[1] != "WaitForBreakCompletion" {
		t.Errorf("second call = %q, want WaitForBreakCompletion", fake.callOrder[1])
	}
}

// TestBreakParentReadLeasesOnModify_WaitsForAck asserts the same ack-wait
// guarantee for BreakParentReadLeasesOnModify.
func TestBreakParentReadLeasesOnModify_WaitsForAck(t *testing.T) {
	t.Parallel()

	fake := &fakeLockManager{}
	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	parentHandle := lock.FileHandle("parent-dir-handle-2")
	if err := lm.BreakParentReadLeasesOnModify(context.Background(), parentHandle, "share1", "smb:A"); err != nil {
		t.Fatalf("BreakParentReadLeasesOnModify returned error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if got := len(fake.breakReadCalls); got != 1 {
		t.Fatalf("BreakReadLeasesForParentDir call count = %d, want 1", got)
	}
	if fake.breakReadCalls[0].HandleKey != string(parentHandle) {
		t.Errorf("BreakReadLeasesForParentDir handleKey = %q, want %q",
			fake.breakReadCalls[0].HandleKey, string(parentHandle))
	}

	if got := len(fake.waitForBreakCompletionKeys); got != 1 {
		t.Fatalf("WaitForBreakCompletion call count = %d, want 1 (parent break must wait for ack per MS-SMB2 3.3.4.7)", got)
	}
	if fake.waitForBreakCompletionKeys[0] != string(parentHandle) {
		t.Errorf("WaitForBreakCompletion handleKey = %q, want %q",
			fake.waitForBreakCompletionKeys[0], string(parentHandle))
	}

	if len(fake.callOrder) < 2 {
		t.Fatalf("expected at least 2 calls in order, got %v", fake.callOrder)
	}
	if fake.callOrder[0] != "BreakReadLeasesForParentDir" {
		t.Errorf("first call = %q, want BreakReadLeasesForParentDir", fake.callOrder[0])
	}
	if fake.callOrder[1] != "WaitForBreakCompletion" {
		t.Errorf("second call = %q, want WaitForBreakCompletion", fake.callOrder[1])
	}
}

// TestBreakParentHandle_ExcludesTriggeringClient asserts that the triggering
// CREATE's clientID is forwarded as the excludeOwner so that the triggering
// session's own parent-dir lease (if any) is NOT in the breakable set, and
// that the function honours its caller's context cancellation rather than
// blocking forever — proving the wait cannot deadlock the triggering CREATE.
func TestBreakParentHandle_ExcludesTriggeringClient(t *testing.T) {
	t.Parallel()

	// waitBlock simulates an outstanding break that never gets acked. With a
	// bounded context the call must return when the context expires, NOT
	// deadlock indefinitely.
	fake := &fakeLockManager{
		waitBlock: make(chan struct{}),
	}
	defer close(fake.waitBlock)

	lm := NewLeaseManager(&fakeResolver{mgr: fake}, nil)

	// Use a short caller-side timeout to bound the test.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- lm.BreakParentHandleLeasesOnCreate(ctx, lock.FileHandle("parent"), "share1", "smb:A")
	}()

	select {
	case <-done:
		// Returned (either nil or ctx.DeadlineExceeded). Both are acceptable —
		// what we are asserting is that the call does NOT block forever.
	case <-time.After(2 * time.Second):
		t.Fatal("BreakParentHandleLeasesOnCreate deadlocked: did not return within 2s of caller context expiry")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.breakHandleCalls) != 1 {
		t.Fatalf("BreakHandleLeasesForSMBOpen call count = %d, want 1", len(fake.breakHandleCalls))
	}
	excludeOwner := fake.breakHandleCalls[0].ExcludeOwner
	if excludeOwner == nil {
		t.Fatal("excludeOwner is nil; the triggering client's session must be excluded from the breakable set")
	}
	if excludeOwner.ClientID != "smb:A" {
		t.Errorf("excludeOwner.ClientID = %q, want %q", excludeOwner.ClientID, "smb:A")
	}

	// And we must have actually attempted to wait — proving the new contract
	// is wired (rather than the test passing trivially against unmodified code).
	if len(fake.waitForBreakCompletionKeys) != 1 {
		t.Fatalf("WaitForBreakCompletion call count = %d, want 1", len(fake.waitForBreakCompletionKeys))
	}
}
