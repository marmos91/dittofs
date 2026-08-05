package handlers

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Netgroup (export client allowlist) enforcement on NFSv4
// ============================================================================
//
// NFSv4 has no MOUNT protocol, so the netgroup allowlist the v3 MOUNT handler
// evaluates never ran for a v4 client: a share restricted to a netgroup was
// reachable from any address over PUTROOTFH/PUTFH/LOOKUP. buildV4AuthContext
// now runs the same check at the point an operation resolves a share.
//
// Matching a client IP against netgroup members (IP, CIDR, hostname) is covered
// by the Runtime tests in pkg/controlplane/runtime; the tests here cover the v4
// seam: that the check runs on a real operation, and how its verdicts map onto
// NFSv4 statuses.

// fakeNetgroupRuntime satisfies nfsRuntime for the netgroup check alone. The
// embedded nil interface panics if anything else is called, which keeps the
// fake honest about what these tests exercise.
type fakeNetgroupRuntime struct {
	nfsRuntime

	allowed bool
	err     error

	gotShare string
	gotIP    net.IP
	calls    int
}

func (f *fakeNetgroupRuntime) CheckNetgroupAccess(_ context.Context, shareName string, clientIP net.IP) (bool, error) {
	f.calls++
	f.gotShare = shareName
	f.gotIP = clientIP
	return f.allowed, f.err
}

// checkStatus runs checkNetgroupAccess against the fake and returns the NFSv4
// status the client would see (NFS4_OK when the check passes).
func checkStatus(t *testing.T, rt *fakeNetgroupRuntime, clientAddr string) uint32 {
	t.Helper()

	h := &Handler{Registry: rt}
	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: clientAddr}

	err := h.checkNetgroupAccess(ctx, "/export")
	if err == nil {
		return types.NFS4_OK
	}
	return nfs4StatusForAuthError(err)
}

// TestV4Netgroup_ClientOutsideNetgroupDenied covers a client the allowlist does
// not cover: the compound must fail with NFS4ERR_ACCESS, not proceed.
func TestV4Netgroup_ClientOutsideNetgroupDenied(t *testing.T) {
	rt := &fakeNetgroupRuntime{allowed: false}

	if status := checkStatus(t, rt, "10.0.0.5:1023"); status != types.NFS4ERR_ACCESS {
		t.Fatalf("denied client status = %d, want NFS4ERR_ACCESS (%d)", status, types.NFS4ERR_ACCESS)
	}
	if !rt.gotIP.Equal(net.ParseIP("10.0.0.5")) {
		t.Fatalf("checked IP = %v, want 10.0.0.5 (host must be split from the port)", rt.gotIP)
	}
	if rt.gotShare != "/export" {
		t.Fatalf("checked share = %q, want /export", rt.gotShare)
	}
}

// TestV4Netgroup_ClientInsideNetgroupAllowed covers the allow verdict: the
// check must not interfere with a client the allowlist covers.
func TestV4Netgroup_ClientInsideNetgroupAllowed(t *testing.T) {
	rt := &fakeNetgroupRuntime{allowed: true}

	if status := checkStatus(t, rt, "192.168.1.100:9999"); status != types.NFS4_OK {
		t.Fatalf("allowed client status = %d, want NFS4_OK", status)
	}
	if !rt.gotIP.Equal(net.ParseIP("192.168.1.100")) {
		t.Fatalf("checked IP = %v, want 192.168.1.100", rt.gotIP)
	}
}

// TestV4Netgroup_LookupErrorDenies covers the fail-closed contract: a netgroup
// lookup that errors must deny rather than fall through to the operation.
func TestV4Netgroup_LookupErrorDenies(t *testing.T) {
	rt := &fakeNetgroupRuntime{allowed: true, err: errors.New("netgroup store unavailable")}

	if status := checkStatus(t, rt, "192.168.1.100:9999"); status != types.NFS4ERR_ACCESS {
		t.Fatalf("lookup-error status = %d, want NFS4ERR_ACCESS (%d)", status, types.NFS4ERR_ACCESS)
	}
}

// TestV4Netgroup_UnparseableClientAddrChecked covers a peer address the adapter
// cannot turn into an IP: the check still runs, with a nil IP that matches no
// netgroup member, so a restricted share denies instead of being skipped.
func TestV4Netgroup_UnparseableClientAddrChecked(t *testing.T) {
	rt := &fakeNetgroupRuntime{allowed: false}

	if status := checkStatus(t, rt, "not-an-address"); status != types.NFS4ERR_ACCESS {
		t.Fatalf("unparseable-addr status = %d, want NFS4ERR_ACCESS (%d)", status, types.NFS4ERR_ACCESS)
	}
	if rt.calls != 1 {
		t.Fatalf("CheckNetgroupAccess calls = %d, want 1 (the check must not be skipped)", rt.calls)
	}
	if rt.gotIP != nil {
		t.Fatalf("checked IP = %v, want nil for an unparseable address", rt.gotIP)
	}
}

// TestV4Netgroup_NoNetgroupAllowsAll covers the existing empty-allowlist
// semantics end to end: the fixture share has no netgroup, so a real GETATTR
// succeeds.
func TestV4Netgroup_NoNetgroupAllowsAll(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "f.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	if status := getAttrStatusForFile(fx, fileHandle); status != types.NFS4_OK {
		t.Fatalf("share without a netgroup GETATTR status = %d, want NFS4_OK", status)
	}
}

// TestV4Netgroup_RestrictedShareDeniesOnRealOp drives a real GETATTR against a
// share pointed at a netgroup the fixture's runtime cannot resolve. It proves
// the check is wired into the operation path and that an unresolvable netgroup
// fails closed rather than allowing the operation through.
func TestV4Netgroup_RestrictedShareDeniesOnRealOp(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "f.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	if err := fx.rt.SetShareNetgroup("/export", "office-ips"); err != nil {
		t.Fatalf("SetShareNetgroup: %v", err)
	}

	if status := getAttrStatusForFile(fx, fileHandle); status != types.NFS4ERR_ACCESS {
		t.Fatalf("restricted share GETATTR status = %d, want NFS4ERR_ACCESS (%d)", status, types.NFS4ERR_ACCESS)
	}
}

// TestV4Netgroup_PutFHDeniesRestrictedShare covers the operations that act on
// the current filehandle without building an auth context (LOCK, LOCKT, LOCKU,
// GET_DIR_DELEGATION): they can only name a share handle via PUTFH, so PUTFH
// must refuse a handle for a share whose netgroup the client is not in.
func TestV4Netgroup_PutFHDeniesRestrictedShare(t *testing.T) {
	h, rootHandle, rt := newPutFHTestHandler(t, "/export")
	if err := rt.SetShareNetgroup("/export", "office-ips"); err != nil {
		t.Fatalf("SetShareNetgroup: %v", err)
	}

	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: "10.0.0.5:1234"}
	res := h.handlePutFH(ctx, bytes.NewReader(encodePutFHArgsBytes(t, rootHandle)))
	if res.Status != types.NFS4ERR_ACCESS {
		t.Fatalf("Status = %d, want NFS4ERR_ACCESS (%d)", res.Status, types.NFS4ERR_ACCESS)
	}
	if ctx.CurrentFH != nil {
		t.Errorf("CurrentFH was set despite refusal: %x", ctx.CurrentFH)
	}
}
