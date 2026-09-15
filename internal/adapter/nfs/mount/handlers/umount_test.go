package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// newUmountHandler builds a Handler over a bare runtime. The mount tracker
// records whatever it is told, so UMNT/UMNTALL can be exercised without
// registering shares or metadata stores.
func newUmountHandler() (*Handler, *runtime.Runtime) {
	rt := runtime.New(nil)
	return &Handler{Registry: rt}, rt
}

// umountCtx builds a MountHandlerContext for a client address.
func umountCtx(clientAddr string) *MountHandlerContext {
	return &MountHandlerContext{Context: context.Background(), ClientAddr: clientAddr}
}

// mountedPaths returns the export paths recorded for a client.
func mountedPaths(rt *runtime.Runtime, clientIP string) []string {
	var paths []string
	for _, m := range rt.ListMounts() {
		if m.ClientAddr == clientIP {
			paths = append(paths, m.ShareName)
		}
	}
	return paths
}

// UMNT removes only the entry for the requested dirpath (RFC 1813 Appendix I
// procedure 3); a second export the client still has mounted survives.
func TestUmnt_RemovesOnlyRequestedExport(t *testing.T) {
	h, rt := newUmountHandler()
	rt.RecordMount("10.0.0.1", "/share1", 1000)
	rt.RecordMount("10.0.0.1", "/share2", 2000)

	if _, err := h.Umnt(umountCtx("10.0.0.1:987"), &UmountRequest{DirPath: "/share1"}); err != nil {
		t.Fatalf("Umnt: %v", err)
	}

	got := mountedPaths(rt, "10.0.0.1")
	if len(got) != 1 || got[0] != "/share2" {
		t.Errorf("mounts after UMNT /share1 = %v, want [/share2]", got)
	}
}

// UMNT is scoped to NFS: an SMB mount record for the same client address is
// keyed by a different protocol and must be left alone.
func TestUmnt_LeavesSMBMountRecord(t *testing.T) {
	h, rt := newUmountHandler()
	rt.RecordMount("10.0.0.1", "/share1", 1000)
	rt.Mounts().Record("10.0.0.1", "smb", "/share1", nil)

	if _, err := h.Umnt(umountCtx("10.0.0.1:987"), &UmountRequest{DirPath: "/share1"}); err != nil {
		t.Fatalf("Umnt: %v", err)
	}

	if n := len(rt.Mounts().ListByProtocol("smb")); n != 1 {
		t.Errorf("SMB mounts after NFS UMNT = %d, want 1", n)
	}
	if n := len(rt.ListMounts()); n != 1 {
		t.Errorf("total mounts = %d, want 1 (SMB only)", n)
	}
}

// UMNT on a path the client never mounted is a no-op that still succeeds.
func TestUmnt_UnknownPathIsNoOp(t *testing.T) {
	h, rt := newUmountHandler()
	rt.RecordMount("10.0.0.1", "/share1", 1000)

	resp, err := h.Umnt(umountCtx("10.0.0.1:987"), &UmountRequest{DirPath: "/nope"})
	if err != nil {
		t.Fatalf("Umnt: %v", err)
	}
	if resp.Status != MountOK {
		t.Errorf("Status = %d, want MountOK", resp.Status)
	}
	if got := mountedPaths(rt, "10.0.0.1"); len(got) != 1 {
		t.Errorf("mounts = %v, want [/share1] untouched", got)
	}
}

// UMNTALL (procedure 4) removes every NFS entry for the calling client and
// nothing belonging to another client.
func TestUmntAll_ScopedToCallingClient(t *testing.T) {
	h, rt := newUmountHandler()
	rt.RecordMount("10.0.0.1", "/share1", 1000)
	rt.RecordMount("10.0.0.1", "/share2", 2000)
	rt.RecordMount("10.0.0.2", "/share1", 3000)

	if _, err := h.UmntAll(umountCtx("10.0.0.1:987"), &UmountAllRequest{}); err != nil {
		t.Fatalf("UmntAll: %v", err)
	}

	if got := mountedPaths(rt, "10.0.0.1"); len(got) != 0 {
		t.Errorf("calling client mounts = %v, want none", got)
	}
	if got := mountedPaths(rt, "10.0.0.2"); len(got) != 1 {
		t.Errorf("other client mounts = %v, want [/share1] untouched", got)
	}
}

// UMNTALL is scoped to NFS: the calling client's SMB records survive.
func TestUmntAll_LeavesSMBMountRecord(t *testing.T) {
	h, rt := newUmountHandler()
	rt.RecordMount("10.0.0.1", "/share1", 1000)
	rt.Mounts().Record("10.0.0.1", "smb", "/share1", nil)

	if _, err := h.UmntAll(umountCtx("10.0.0.1:987"), &UmountAllRequest{}); err != nil {
		t.Fatalf("UmntAll: %v", err)
	}

	if n := len(rt.Mounts().ListByProtocol("smb")); n != 1 {
		t.Errorf("SMB mounts after NFS UMNTALL = %d, want 1", n)
	}
	if n := len(rt.Mounts().ListByProtocol("nfs")); n != 0 {
		t.Errorf("NFS mounts after UMNTALL = %d, want 0", n)
	}
}
