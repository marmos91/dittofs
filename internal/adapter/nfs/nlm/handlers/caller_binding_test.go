package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// recordingLockService records whether UnlockFileNLM was invoked, so a test can
// assert that a rejected (spoofed) UNLOCK performs no actual release.
type recordingLockService struct {
	unlockCalls int
}

func (r *recordingLockService) LockFileNLM(_ context.Context, _ []byte, _ lock.LockOwner, _, _ uint64, _, _ bool) (*lock.LockResult, error) {
	return &lock.LockResult{Success: true}, nil
}

func (r *recordingLockService) TestLockNLM(_ context.Context, _ []byte, _ lock.LockOwner, _, _ uint64, _ bool) (bool, *lock.UnifiedLockConflict, error) {
	return true, nil, nil
}

func (r *recordingLockService) UnlockFileNLM(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	r.unlockCalls++
	return nil
}

func (r *recordingLockService) CancelBlockingLock(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	return nil
}

func lockReq(caller string) *LockRequest {
	return &LockRequest{
		Exclusive: true,
		Lock: types.NLM4Lock{
			CallerName: caller,
			FH:         []byte("share-a:file-1"),
			OH:         []byte{0x01},
			Svid:       42,
			Offset:     0,
			Length:     100,
		},
	}
}

func unlockReq(caller string) *UnlockRequest {
	return &UnlockRequest{
		Lock: types.NLM4Lock{
			CallerName: caller,
			FH:         []byte("share-a:file-1"),
			OH:         []byte{0x01},
			Svid:       42,
			Offset:     0,
			Length:     100,
		},
	}
}

// TestUnlock_SpoofedCallerFromDifferentHostIsRejected is the negative control
// for the lock-theft fix. A victim on host-a acquires a lock under caller_name
// "victim". An attacker on host-b reconstructs the same caller_name/svid/oh and
// sends UNLOCK. Without the caller<->source binding the attacker's UNLOCK would
// call through to UnlockFileNLM and release the victim's lock. With the fix the
// release must NOT happen, while the legitimate host-a UNLOCK still releases.
func TestUnlock_SpoofedCallerFromDifferentHostIsRejected(t *testing.T) {
	svc := &recordingLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(DefaultBlockingQueueSize))

	victimCtx := &NLMHandlerContext{Context: context.Background(), ClientAddr: "10.0.0.7:51234"}
	if _, err := h.Lock(victimCtx, lockReq("victim")); err != nil {
		t.Fatalf("victim Lock error: %v", err)
	}

	// Attacker on a different source host spoofs the victim's caller_name.
	attackerCtx := &NLMHandlerContext{Context: context.Background(), ClientAddr: "10.0.0.99:40000"}
	if _, err := h.Unlock(attackerCtx, unlockReq("victim")); err != nil {
		t.Fatalf("attacker Unlock error: %v", err)
	}
	if svc.unlockCalls != 0 {
		t.Fatalf("attacker UNLOCK released victim lock: UnlockFileNLM called %d times, want 0", svc.unlockCalls)
	}

	// The legitimate owner on the original host must still be able to unlock.
	if _, err := h.Unlock(victimCtx, unlockReq("victim")); err != nil {
		t.Fatalf("victim Unlock error: %v", err)
	}
	if svc.unlockCalls != 1 {
		t.Fatalf("legitimate UNLOCK did not release: UnlockFileNLM called %d times, want 1", svc.unlockCalls)
	}
}

// TestCancel_SpoofedCallerFromDifferentHostIsRejected is the negative control
// for the CANCEL lock-theft path. A victim queues a blocking lock; an attacker
// on a different host must not be able to dequeue it via a spoofed caller_name.
func TestCancel_SpoofedCallerFromDifferentHostIsRejected(t *testing.T) {
	// Conflicting service: first lock succeeds (different owner already holds),
	// second returns conflict so the victim's blocking request is queued.
	svc := &conflictLockService{}
	q := blocking.NewBlockingQueue(DefaultBlockingQueueSize)
	h := NewHandler(svc, q)

	victimCtx := &NLMHandlerContext{Context: context.Background(), ClientAddr: "10.0.0.7:51234"}
	blockingReq := lockReq("victim")
	blockingReq.Block = true
	resp, err := h.Lock(victimCtx, blockingReq)
	if err != nil {
		t.Fatalf("victim blocking Lock error: %v", err)
	}
	if resp.Status != types.NLM4Blocked {
		t.Fatalf("victim Lock status = %d, want NLM4Blocked (%d)", resp.Status, types.NLM4Blocked)
	}

	handleKey := string(blockingReq.Lock.FH)
	if got := len(q.GetWaiters(handleKey)); got != 1 {
		t.Fatalf("queue length after enqueue = %d, want 1", got)
	}

	// Attacker on a different host attempts CANCEL with the victim's caller_name.
	attackerCtx := &NLMHandlerContext{Context: context.Background(), ClientAddr: "10.0.0.99:40000"}
	cancelReq := &CancelRequest{
		Block:     true,
		Exclusive: true,
		Lock:      blockingReq.Lock,
	}
	if _, err := h.Cancel(attackerCtx, cancelReq); err != nil {
		t.Fatalf("attacker Cancel error: %v", err)
	}
	if got := len(q.GetWaiters(handleKey)); got != 1 {
		t.Fatalf("attacker CANCEL dequeued victim waiter: queue length = %d, want 1", got)
	}

	// Legitimate owner can cancel its own pending request.
	if _, err := h.Cancel(victimCtx, cancelReq); err != nil {
		t.Fatalf("victim Cancel error: %v", err)
	}
	if got := len(q.GetWaiters(handleKey)); got != 0 {
		t.Fatalf("legitimate CANCEL did not dequeue: queue length = %d, want 0", got)
	}
}

// conflictLockService always reports a conflict so blocking locks get queued.
type conflictLockService struct{}

func (c *conflictLockService) LockFileNLM(_ context.Context, _ []byte, _ lock.LockOwner, _, _ uint64, _, _ bool) (*lock.LockResult, error) {
	return &lock.LockResult{Success: false}, nil
}

func (c *conflictLockService) TestLockNLM(_ context.Context, _ []byte, _ lock.LockOwner, _, _ uint64, _ bool) (bool, *lock.UnifiedLockConflict, error) {
	return false, nil, nil
}

func (c *conflictLockService) UnlockFileNLM(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	return nil
}

func (c *conflictLockService) CancelBlockingLock(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	return nil
}
