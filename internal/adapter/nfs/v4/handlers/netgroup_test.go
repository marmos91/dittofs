package handlers

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
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

// GetShare and GetRootHandle are the rest of what a junction crossing touches.
// The share is enabled, so the netgroup verdict is the only thing under test.
func (f *fakeNetgroupRuntime) GetShare(name string) (*runtime.Share, error) {
	return &runtime.Share{Name: name, Enabled: true}, nil
}

func (f *fakeNetgroupRuntime) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	return metadata.FileHandle(shareName + ":00000000-0000-0000-0000-000000000001"), nil
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

// encodeLookupNameBytes encodes the single component4 argument handleLookup
// reads for LOOKUP.
func encodeLookupNameBytes(t *testing.T, name string) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := xdr.WriteXDRString(&buf, name); err != nil {
		t.Fatalf("encode LOOKUP arg: %v", err)
	}
	return buf.Bytes()
}

// lookupJunctionFromPseudoRoot runs the LOOKUP that crosses the export junction
// out of the pseudo-fs root into the share, the way a v4 client reaches a share
// without ever issuing PUTFH.
func lookupJunctionFromPseudoRoot(t *testing.T, h *Handler, clientAddr string) (*types.CompoundContext, *types.CompoundResult) {
	t.Helper()

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: clientAddr,
		CurrentFH:  h.PseudoFS.GetRootHandle(),
	}
	return ctx, h.handleLookup(ctx, bytes.NewReader(encodeLookupNameBytes(t, "export")))
}

// TestV4Netgroup_JunctionLookupCrossesWhenAllowed is the positive control for
// the test below: with no netgroup on the share, the junction LOOKUP still
// succeeds and leaves the share's real root handle in the current filehandle.
func TestV4Netgroup_JunctionLookupCrossesWhenAllowed(t *testing.T) {
	h, rootHandle, _ := newPutFHTestHandler(t, "/export")

	ctx, res := lookupJunctionFromPseudoRoot(t, h, "10.0.0.5:1234")
	if res.Status != types.NFS4_OK {
		t.Fatalf("Status = %d, want NFS4_OK", res.Status)
	}
	if !bytes.Equal(ctx.CurrentFH, rootHandle) {
		t.Fatalf("CurrentFH = %x, want the share root handle %x", ctx.CurrentFH, rootHandle)
	}
}

// TestV4Netgroup_JunctionLookupDeniesRestrictedShare covers the second way a
// share handle enters a compound. PUTFH is gated, but a LOOKUP that crosses the
// export junction out of the pseudo-fs installs the share's root handle without
// building an auth context, so neither gate ran on this path: LOCK, LOCKT,
// LOCKU and GET_DIR_DELEGATION then acted on a netgroup-restricted share from
// an address the allowlist excludes.
func TestV4Netgroup_JunctionLookupDeniesRestrictedShare(t *testing.T) {
	h, rootHandle, rt := newPutFHTestHandler(t, "/export")
	if err := rt.SetShareNetgroup("/export", "office-ips"); err != nil {
		t.Fatalf("SetShareNetgroup: %v", err)
	}

	ctx, res := lookupJunctionFromPseudoRoot(t, h, "10.0.0.5:1234")
	if res.Status != types.NFS4ERR_ACCESS {
		t.Fatalf("Status = %d, want NFS4ERR_ACCESS (%d)", res.Status, types.NFS4ERR_ACCESS)
	}
	if bytes.Equal(ctx.CurrentFH, rootHandle) {
		t.Fatalf("CurrentFH was advanced to the restricted share's root handle %x", ctx.CurrentFH)
	}
}

// TestV4Netgroup_XattrOpsReportAccessNotServerfault covers the status the client
// receives when the netgroup check refuses an xattr operation. The four RFC 8276
// handlers answered NFS4ERR_SERVERFAULT for every auth-context failure, which
// tells a client to give up on a server fault rather than that it was refused.
func TestV4Netgroup_XattrOpsReportAccessNotServerfault(t *testing.T) {
	h, rootHandle, rt := newPutFHTestHandler(t, "/export")
	if err := rt.SetShareNetgroup("/export", "office-ips"); err != nil {
		t.Fatalf("SetShareNetgroup: %v", err)
	}

	newCtx := func() *types.CompoundContext {
		return &types.CompoundContext{
			Context:    context.Background(),
			ClientAddr: "10.0.0.5:1234",
			AuthFlavor: 1, // AUTH_UNIX
			CurrentFH:  rootHandle,
		}
	}

	cases := []struct {
		op   string
		call func() *types.CompoundResult
	}{
		{"GETXATTR", func() *types.CompoundResult {
			return h.handleGetXattr(newCtx(), encGetXattrArgs("user.foo"))
		}},
		{"SETXATTR", func() *types.CompoundResult {
			return h.handleSetXattr(newCtx(), encSetXattrArgs(types.SETXATTR4_EITHER, "user.foo", []byte("v")))
		}},
		{"LISTXATTRS", func() *types.CompoundResult {
			return h.handleListXattrs(newCtx(), encListXattrsArgs(0, 4096))
		}},
		{"REMOVEXATTR", func() *types.CompoundResult {
			return h.handleRemoveXattr(newCtx(), encRemoveXattrArgs("user.foo"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			if status := tc.call().Status; status != types.NFS4ERR_ACCESS {
				t.Fatalf("%s status = %d, want NFS4ERR_ACCESS (%d)", tc.op, status, types.NFS4ERR_ACCESS)
			}
		})
	}
}

// TestV4Netgroup_JunctionLookupChecksTheJunctionsShare pins WHICH share the
// junction gate asks about and on WHICH verdict it refuses. The end-to-end test
// above denies through the fail-closed path (an unresolvable netgroup), so it
// would pass just as well if the gate named the wrong share; this one denies on
// a plain non-membership verdict and asserts the share and the peer IP the
// check was handed.
func TestV4Netgroup_JunctionLookupChecksTheJunctionsShare(t *testing.T) {
	rt := &fakeNetgroupRuntime{allowed: false}
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := &Handler{Registry: rt, PseudoFS: pfs}

	pseudoRoot := pfs.GetRootHandle()
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "10.0.0.5:1234",
		CurrentFH:  pseudoRoot,
	}

	res := h.handleLookup(ctx, bytes.NewReader(encodeLookupNameBytes(t, "export")))
	if res.Status != types.NFS4ERR_ACCESS {
		t.Fatalf("Status = %d, want NFS4ERR_ACCESS (%d)", res.Status, types.NFS4ERR_ACCESS)
	}
	if rt.calls != 1 {
		t.Fatalf("CheckNetgroupAccess calls = %d, want 1", rt.calls)
	}
	if rt.gotShare != "/export" {
		t.Errorf("checked share = %q, want /export (the junction's own share)", rt.gotShare)
	}
	if !rt.gotIP.Equal(net.ParseIP("10.0.0.5")) {
		t.Errorf("checked IP = %v, want 10.0.0.5", rt.gotIP)
	}
	if !bytes.Equal(ctx.CurrentFH, pseudoRoot) {
		t.Errorf("CurrentFH moved off the pseudo-fs root: %x", ctx.CurrentFH)
	}
}
