package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	metaerrors "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// deniedLockService stands in for a lock service whose access gate refuses the
// caller, and records the identity the handler threaded to it.
type deniedLockService struct {
	gotCaller *metadata.Identity
}

func (d *deniedLockService) denial(caller *metadata.Identity) error {
	d.gotCaller = caller
	return &metaerrors.StoreError{Code: metaerrors.ErrAccessDenied, Message: "lock permission denied"}
}

func (d *deniedLockService) LockFileNLM(_ context.Context, caller *metadata.Identity, _ []byte, _ lock.LockOwner, _, _ uint64, _, _ bool) (*lock.LockResult, error) {
	return nil, d.denial(caller)
}

func (d *deniedLockService) TestLockNLM(_ context.Context, caller *metadata.Identity, _ []byte, _ lock.LockOwner, _, _ uint64, _ bool) (bool, *lock.UnifiedLockConflict, error) {
	return false, nil, d.denial(caller)
}

func (d *deniedLockService) UnlockFileNLM(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	return nil
}

func (d *deniedLockService) CancelBlockingLock(_ context.Context, _ []byte, _ string, _, _ uint64) error {
	return nil
}

func deniedCtx() *NLMHandlerContext {
	uid, gid := uint32(2000), uint32(2000)
	return &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.7:51234",
		AuthFlavor: 1,
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{4000},
	}
}

func deniedLockReq() *LockRequest {
	return &LockRequest{
		Cookie:    []byte{9},
		Exclusive: true,
		Lock: types.NLM4Lock{
			CallerName: "clientA",
			FH:         []byte("share:file"),
			Svid:       1,
			OH:         []byte{0xaa},
			Offset:     0,
			Length:     100,
		},
	}
}

// TestLock_PermissionDenialIsNLM4Denied: a refusal to authorize the lock is an
// authorization outcome, so the client must see NLM4_DENIED rather than
// NLM4_FAILED, which reads as a broken server and invites a retry loop.
func TestLock_PermissionDenialIsNLM4Denied(t *testing.T) {
	t.Parallel()

	svc := &deniedLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(10))

	resp, err := h.Lock(deniedCtx(), deniedLockReq())
	if err != nil {
		t.Fatalf("Lock returned a transport error: %v", err)
	}
	if resp.Status != types.NLM4Denied {
		t.Fatalf("want NLM4_DENIED (%d), got %d", types.NLM4Denied, resp.Status)
	}
	if svc.gotCaller == nil || svc.gotCaller.UID == nil || *svc.gotCaller.UID != 2000 {
		t.Fatalf("handler did not thread the AUTH_UNIX identity to the lock service: %+v", svc.gotCaller)
	}
	if len(svc.gotCaller.GIDs) != 1 || svc.gotCaller.GIDs[0] != 4000 {
		t.Fatalf("supplementary groups lost on the way to the lock service: %+v", svc.gotCaller.GIDs)
	}
}

func TestTest_PermissionDenialIsNLM4Denied(t *testing.T) {
	t.Parallel()

	svc := &deniedLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(10))

	req := deniedLockReq()
	resp, err := h.Test(deniedCtx(), &TestRequest{Cookie: req.Cookie, Exclusive: true, Lock: req.Lock})
	if err != nil {
		t.Fatalf("Test returned a transport error: %v", err)
	}
	if resp.Status != types.NLM4Denied {
		t.Fatalf("want NLM4_DENIED (%d), got %d", types.NLM4Denied, resp.Status)
	}
}

// TestLock_NoCredentialsYieldsNilIdentity: an AUTH_NULL call carries no
// credentials, and the metadata layer must see that rather than a zero uid,
// which would read as root.
func TestLock_NoCredentialsYieldsNilIdentity(t *testing.T) {
	t.Parallel()

	svc := &deniedLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(10))

	ctx := &NLMHandlerContext{Context: context.Background(), ClientAddr: "10.0.0.7:51234"}
	if _, err := h.Lock(ctx, deniedLockReq()); err != nil {
		t.Fatalf("Lock returned a transport error: %v", err)
	}
	if svc.gotCaller != nil {
		t.Fatalf("AUTH_NULL must reach the lock service as no identity, got %+v", svc.gotCaller)
	}
}
