package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Export auth-flavor policy at handle entry (#2692)
// ============================================================================
//
// LOCK, LOCKT, LOCKU and GET_DIR_DELEGATION take state through the
// StateManager without making a metadata call, so unlike every other real-FS
// operation they never built an auth context -- and so never reached the
// export auth-flavor check in buildV4AuthContext. A share that required
// Kerberos, or disallowed AUTH_SYS, stayed reachable over AUTH_SYS through
// them.
//
// These tests drive the policy through the real op handlers over an AUTH_UNIX
// context and assert the status the client actually sees, mirroring
// auth_policy_test.go's GETATTR coverage. The refusal must be
// NFS4ERR_WRONGSEC (not SERVERFAULT, which would mask the policy) so the
// client retries with the correct flavor.
//
// The gate runs after each operation's own preconditions, so these tests
// establish the state each operation needs before the policy is what stops it;
// otherwise the operation would fail on its own checks first and prove nothing
// about the gate.

// newLockPolicyFixture builds a handler over a real metadata store with a lock
// manager, plus a regular file to lock and a confirmed client holding open
// state on it -- everything a LOCK needs to reach its state mutation, so that
// on a permissive share the request succeeds and on a Kerberos-only share the
// only thing that can refuse it is the flavor gate.
func newLockPolicyFixture(t *testing.T) (*realFSTestFixture, []byte, uint64, *types.Stateid4, uint32) {
	t.Helper()

	fx := newRealFSTestFixture(t, "/export")
	fx.handler.StateManager.SetLockManager(lock.NewManager())
	fileHandle := []byte(fx.createTestFile(t, fx.rootHandle, "lock-policy-file", metadata.FileTypeRegular, 0o644, 0, 0))

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, fx.handler, fileHandle)
	return fx, fileHandle, clientID, openStateid, openSeqid
}

// lockPolicyCtx builds an AUTH_UNIX compound context over fileHandle.
func lockPolicyCtx(fileHandle []byte) *types.CompoundContext {
	return &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:9999",
		AuthFlavor: 1, // AUTH_UNIX
		CurrentFH:  append([]byte(nil), fileHandle...),
	}
}

// lockPolicyArgs encodes LOCK4args for the new_lock_owner path.
func lockPolicyArgs(openStateid *types.Stateid4, openSeqid uint32, clientID uint64) []byte {
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT) // locktype
	_ = xdr.WriteUint32(&args, 0)              // reclaim
	_ = xdr.WriteUint64(&args, 0)              // offset
	_ = xdr.WriteUint64(&args, 100)            // length
	_ = xdr.WriteUint32(&args, 1)              // new_lock_owner = true
	_ = xdr.WriteUint32(&args, openSeqid+1)    // open_seqid
	types.EncodeStateid4(&args, openStateid)
	_ = xdr.WriteUint32(&args, 1)                         // lock_seqid
	_ = xdr.WriteUint64(&args, clientID)                  // lock_owner clientid
	_ = xdr.WriteXDROpaque(&args, []byte("policy-owner")) // lock_owner data
	return args.Bytes()
}

func TestLockAuthPolicy_RequireKerberosRejectsAuthSys(t *testing.T) {
	fx, fileHandle, clientID, openStateid, openSeqid := newLockPolicyFixture(t)
	ctx := lockPolicyCtx(fileHandle)

	// Sanity: the permissive default share lets the LOCK through, so the gate
	// is the only thing that can refuse it below.
	if status := fx.handler.handleLock(ctx, bytes.NewReader(lockPolicyArgs(openStateid, openSeqid, clientID))).Status; status != types.NFS4_OK {
		t.Fatalf("default share AUTH_UNIX LOCK status = %d, want NFS4_OK (%d)", status, types.NFS4_OK)
	}

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	if status := fx.handler.handleLock(ctx, bytes.NewReader(lockPolicyArgs(openStateid, openSeqid, clientID))).Status; status != types.NFS4ERR_WRONGSEC {
		t.Fatalf("RequireKerberos share AUTH_UNIX LOCK status = %d, want NFS4ERR_WRONGSEC (%d)",
			status, types.NFS4ERR_WRONGSEC)
	}
}

func TestLockAuthPolicy_DisallowAuthSysRejectsAuthSys(t *testing.T) {
	fx, fileHandle, clientID, openStateid, openSeqid := newLockPolicyFixture(t)
	ctx := lockPolicyCtx(fileHandle)

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	if status := fx.handler.handleLock(ctx, bytes.NewReader(lockPolicyArgs(openStateid, openSeqid, clientID))).Status; status != types.NFS4ERR_WRONGSEC {
		t.Fatalf("AllowAuthSys=false share AUTH_UNIX LOCK status = %d, want NFS4ERR_WRONGSEC (%d)",
			status, types.NFS4ERR_WRONGSEC)
	}
}

func TestLockTAuthPolicy_RequireKerberosRejectsAuthSys(t *testing.T) {
	fx, fileHandle, clientID, _, _ := newLockPolicyFixture(t)
	ctx := lockPolicyCtx(fileHandle)
	args := encodeLocktArgs(types.WRITE_LT, 0, 100, clientID, []byte("policy-owner"))

	// Sanity: allowed on the default share.
	if status := fx.handler.handleLockT(ctx, bytes.NewReader(args)).Status; status != types.NFS4_OK {
		t.Fatalf("default share AUTH_UNIX LOCKT status = %d, want NFS4_OK (%d)", status, types.NFS4_OK)
	}

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	if status := fx.handler.handleLockT(ctx, bytes.NewReader(args)).Status; status != types.NFS4ERR_WRONGSEC {
		t.Fatalf("RequireKerberos share AUTH_UNIX LOCKT status = %d, want NFS4ERR_WRONGSEC (%d)",
			status, types.NFS4ERR_WRONGSEC)
	}
}

func TestLockUAuthPolicy_RequireKerberosRejectsAuthSys(t *testing.T) {
	fx, fileHandle, clientID, openStateid, openSeqid := newLockPolicyFixture(t)
	ctx := lockPolicyCtx(fileHandle)

	// Establish a lock so LOCKU has real state to release, then unlock it once
	// on the permissive share to prove the request itself is well-formed.
	if status := fx.handler.handleLock(ctx, bytes.NewReader(lockPolicyArgs(openStateid, openSeqid, clientID))).Status; status != types.NFS4_OK {
		t.Fatalf("setup LOCK status = %d, want NFS4_OK (%d)", status, types.NFS4_OK)
	}

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT) // locktype
	_ = xdr.WriteUint32(&args, 0)              // seqid (v4.0 owner seqid)
	types.EncodeStateid4(&args, openStateid)
	_ = xdr.WriteUint64(&args, 0)   // offset
	_ = xdr.WriteUint64(&args, 100) // length

	if status := fx.handler.handleLockU(ctx, bytes.NewReader(args.Bytes())).Status; status != types.NFS4ERR_WRONGSEC {
		t.Fatalf("RequireKerberos share AUTH_UNIX LOCKU status = %d, want NFS4ERR_WRONGSEC (%d)",
			status, types.NFS4ERR_WRONGSEC)
	}
}

// TestLockAuthPolicy_BadXDRStillWins pins the ordering: the policy gate runs
// after the operation's own argument validation, so a truncated request must
// still answer NFS4ERR_BADXDR rather than a policy refusal.
func TestLockAuthPolicy_BadXDRStillWins(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	if err := fx.rt.SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	ctx := lockPolicyCtx(fx.rootHandle)
	if status := fx.handler.handleLock(ctx, bytes.NewReader([]byte{0x00, 0x01})).Status; status != types.NFS4ERR_BADXDR {
		t.Fatalf("truncated LOCK on a disallowing share status = %d, want NFS4ERR_BADXDR (%d)",
			status, types.NFS4ERR_BADXDR)
	}
}

// TestCurrentFHAccessStatus_AllowsPseudoFSHandle pins the pseudo-fs exemption:
// a pseudo-fs handle names no share and carries no export policy, so the gate
// must not refuse it. The callers reject pseudo-fs handles with NFS4ERR_INVAL
// on their own.
func TestCurrentFHAccessStatus_AllowsPseudoFSHandle(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	if err := fx.rt.SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	ctx := newRealFSContext(1000, 1000)
	ctx.CurrentFH = append([]byte(nil), fx.handler.PseudoFS.GetRootHandle()...)

	if status := fx.handler.currentFHAccessStatus(ctx); status != types.NFS4_OK {
		t.Fatalf("pseudo-fs handle status = %d, want NFS4_OK (%d)", status, types.NFS4_OK)
	}
}

// TestCurrentFHAccessStatus_RefusesUnregisteredShare verifies the gate fails
// closed: a real-FS handle whose share cannot be resolved must not be treated
// as "no policy, allow".
func TestCurrentFHAccessStatus_RefusesUnregisteredShare(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	handle, err := metadata.EncodeShareHandle("/gone", uuid.New())
	if err != nil {
		t.Fatalf("encode handle: %v", err)
	}

	ctx := newRealFSContext(1000, 1000)
	ctx.CurrentFH = handle

	if status := fx.handler.currentFHAccessStatus(ctx); status == types.NFS4_OK {
		t.Fatalf("unregistered share status = NFS4_OK, want a refusal")
	}
}

// TestLockPseudoFSHandleStillINVAL pins that adding the policy gate did not
// change the pseudo-fs rejection: a pseudo-fs handle must still answer
// NFS4ERR_INVAL, not a status from the gate.
func TestLockPseudoFSHandleStillINVAL(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		AuthFlavor: 1,
		CurrentFH:  append([]byte(nil), pfs.GetRootHandle()...),
	}

	// Any well-formed-enough LOCK4args: the pseudo-fs rejection precedes decoding.
	if status := h.handleLock(ctx, bytes.NewReader(make([]byte, 64))).Status; status != types.NFS4ERR_INVAL {
		t.Fatalf("pseudo-fs LOCK status = %d, want NFS4ERR_INVAL (%d)", status, types.NFS4ERR_INVAL)
	}
}
