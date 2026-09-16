package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	metaerrors "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// deniedLockService stands in for a lock service whose access gate refuses the
// caller, and records the identity the handler threaded to it.
type deniedLockService struct {
	gotCaller auth.Credentials
	called    bool
}

func (d *deniedLockService) denial(caller auth.Credentials) error {
	d.gotCaller = caller
	d.called = true
	return &metaerrors.StoreError{Code: metaerrors.ErrAccessDenied, Message: "lock permission denied"}
}

func (d *deniedLockService) LockFileNLM(_ context.Context, caller auth.Credentials, _ []byte, _ lock.LockOwner, _, _ uint64, _, _ bool) (*lock.LockResult, error) {
	return nil, d.denial(caller)
}

func (d *deniedLockService) TestLockNLM(_ context.Context, caller auth.Credentials, _ []byte, _ lock.LockOwner, _, _ uint64, _ bool) (bool, *lock.UnifiedLockConflict, error) {
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
func TestLock_PermissionRefusalIsEncodableAndTerminal(t *testing.T) {
	t.Parallel()

	svc := &deniedLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(10))

	resp, err := h.Lock(deniedCtx(), deniedLockReq())
	if err != nil {
		t.Fatalf("Lock returned a transport error: %v", err)
	}
	if resp.Status != types.NLM4DeniedNoLocks {
		t.Fatalf("want NLM4_DENIED_NOLOCKS (%d), got %d", types.NLM4DeniedNoLocks, resp.Status)
	}
	if _, err := EncodeLockResponse(resp); err != nil {
		t.Fatalf("the refusal must be encodable for the client to ever see it: %v", err)
	}
	if !svc.called || svc.gotCaller.UID == nil || *svc.gotCaller.UID != 2000 {
		t.Fatalf("handler did not thread the AUTH_UNIX credentials to the lock service: %+v", svc.gotCaller)
	}
	if svc.gotCaller.ClientAddr != "10.0.0.7:51234" {
		t.Fatalf("client address lost on the way to the lock service: %q", svc.gotCaller.ClientAddr)
	}
	if len(svc.gotCaller.GIDs) != 1 || svc.gotCaller.GIDs[0] != 4000 {
		t.Fatalf("supplementary groups lost on the way to the lock service: %+v", svc.gotCaller.GIDs)
	}
}

func TestTest_PermissionRefusalIsEncodableAndTerminal(t *testing.T) {
	t.Parallel()

	svc := &deniedLockService{}
	h := NewHandler(svc, blocking.NewBlockingQueue(10))

	req := deniedLockReq()
	resp, err := h.Test(deniedCtx(), &TestRequest{Cookie: req.Cookie, Exclusive: true, Lock: req.Lock})
	if err != nil {
		t.Fatalf("Test returned a transport error: %v", err)
	}
	if resp.Status != types.NLM4DeniedNoLocks {
		t.Fatalf("want NLM4_DENIED_NOLOCKS (%d), got %d", types.NLM4DeniedNoLocks, resp.Status)
	}
	// The DENIED arm of the nlm4_testres union carries the conflicting holder,
	// and a permission refusal has none to name. Encoding a holder-less DENIED
	// fails, and the client gets an RPC fault instead of an answer -- so the
	// status is only right if it survives the encoder, on every dialect width.
	for _, vers := range []uint32{1, 3, 4} {
		if _, err := EncodeTestResponse(resp, vers); err != nil {
			t.Fatalf("refusal does not encode at NLM v%d: %v", vers, err)
		}
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
	if !svc.called {
		t.Fatal("the lock service was never called")
	}
	if svc.gotCaller.UID != nil {
		t.Fatalf("AUTH_NULL must reach the lock service with no UID, got %d", *svc.gotCaller.UID)
	}
}
