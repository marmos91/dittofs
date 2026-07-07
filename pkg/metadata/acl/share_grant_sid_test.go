package acl

import "testing"

// TestBuildShareRootACL_SIDGrant verifies a direct AD/SID grant (#1528) projects
// a "sid:<SID>" ACE for the SMB PAC-SID path, and — when the grant carries a
// resolved Unix id — an additional "{id}@localdomain" numeric ACE for NFS.
func TestBuildShareRootACL_SIDGrant(t *testing.T) {
	const groupSID = "S-1-5-21-111-222-333-1104"

	t.Run("sid only (no unix id) projects only the sid: ACE", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, []RootGrant{
			{SID: groupSID, IsGroup: true, Level: GrantReadWrite},
		})
		sidACE := allowACE(a, "sid:"+groupSID)
		if sidACE == nil {
			t.Fatalf("missing sid: ACE for %q", groupSID)
		}
		if !maskAllows(sidACE.AccessMask, modeWrite) {
			t.Errorf("sid: ACE mask %#x does not allow write for read-write grant", sidACE.AccessMask)
		}
		// A pure SID grant (ID == 0) must NOT emit a "0@localdomain" ACE.
		if ace := allowACE(a, LocalDomainPrincipal(0)); ace != nil {
			t.Errorf("unexpected numeric ACE for a pure SID grant (id 0)")
		}
	})

	t.Run("sid with unix id projects both sid: and numeric ACEs", func(t *testing.T) {
		const gid uint32 = 1104
		a := BuildShareRootACL(GrantNone, []RootGrant{
			{ID: gid, SID: groupSID, IsGroup: true, Level: GrantRead},
		})
		if ace := allowACE(a, "sid:"+groupSID); ace == nil {
			t.Errorf("missing sid: ACE (SMB path)")
		}
		if ace := allowACE(a, LocalDomainPrincipal(gid)); ace == nil {
			t.Errorf("missing {gid}@localdomain numeric ACE (NFS path)")
		}
	})

	t.Run("highest level wins on duplicate SID", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, []RootGrant{
			{SID: groupSID, IsGroup: true, Level: GrantRead},
			{SID: groupSID, IsGroup: true, Level: GrantAdmin},
		})
		ace := allowACE(a, "sid:"+groupSID)
		if ace == nil {
			t.Fatalf("missing sid: ACE")
		}
		if ace.AccessMask != FullAccessMask {
			t.Errorf("merged sid: ACE mask = %#x, want FullAccessMask (admin) %#x", ace.AccessMask, FullAccessMask)
		}
	})

	t.Run("GrantNone SID emits no ACE", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, []RootGrant{
			{SID: groupSID, IsGroup: true, Level: GrantNone},
		})
		if ace := allowACE(a, "sid:"+groupSID); ace != nil {
			t.Errorf("GrantNone SID grant unexpectedly emitted a sid: ACE")
		}
	})
}

// maskAllows reports whether mask grants the rwx bit(s) implied by the given
// mode bits, using the same rwxToFullMask mapping BuildShareRootACL uses.
func maskAllows(mask, modeBits uint32) bool {
	want := rwxToFullMask(modeBits, true)
	return mask&want == want
}
