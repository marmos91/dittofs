package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// captureLockService records the owner passed to LockFileNLM so a test can
// assert the identity the handler derives from the request.
type captureLockService struct {
	gotOwner lock.LockOwner
}

func (c *captureLockService) LockFileNLM(_ context.Context, _ []byte, owner lock.LockOwner, _, _ uint64, _, _ bool) (*lock.LockResult, error) {
	c.gotOwner = owner
	return &lock.LockResult{Success: true}, nil
}

func (c *captureLockService) TestLockNLM(_ context.Context, _ []byte, _ lock.LockOwner, _, _ uint64, _ bool) (bool, *lock.UnifiedLockConflict, error) {
	return true, nil, nil
}

func (c *captureLockService) UnlockFileNLM(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	return nil
}

func (c *captureLockService) CancelBlockingLock(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	return nil
}

// TestLock_OwnerClientIDIsCallerName pins the grace/crash identity fix: the
// handler must key a lock's ClientID by the NSM caller_name (stable client
// hostname), NOT the transport address. The transport port changes when a
// client reconnects after a restart, which would make the grace-period reclaim
// roster and NSM crash cleanup (FREE_ALL carries caller_name) miss entirely.
func TestLock_OwnerClientIDIsCallerName(t *testing.T) {
	svc := &captureLockService{}
	h := &Handler{nlmService: svc}

	ctx := &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.7:51234", // ephemeral port — must NOT become the ClientID
	}
	req := &LockRequest{
		Exclusive: true,
		Lock: types.NLM4Lock{
			CallerName: "host-a",
			FH:         []byte("share-a:file-1"),
			OH:         []byte{0x01},
			Svid:       42,
			Offset:     0,
			Length:     100,
		},
	}

	if _, err := h.Lock(ctx, req); err != nil {
		t.Fatalf("Lock returned error: %v", err)
	}

	if svc.gotOwner.ClientID != "host-a" {
		t.Errorf("owner.ClientID = %q, want caller_name %q (transport addr %q must not leak in)",
			svc.gotOwner.ClientID, "host-a", ctx.ClientAddr)
	}
}
