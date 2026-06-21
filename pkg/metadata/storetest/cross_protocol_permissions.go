package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// RunCrossProtocolPermissionMatrix is the cross-protocol permission conformance
// suite (AD-0). It is the regression net guaranteeing that NFSv3, NFSv4.0,
// NFSv4.1, and SMB all reach the SAME allow/deny decision for the SAME file +
// ACL on the SAME metadata backend.
//
// AD identities are worthless if the two protocol families enforce a file's ACL
// differently. The original NFSv4 ACCESS handler reimplemented a Unix-mode-only
// check that ignored ACLs, DENY ACEs, and SID-based grants, so the same file
// produced a different answer on NFSv4 than on NFSv3/SMB. This suite pins the
// fix: every protocol "lane" below drives the SAME central
// metadata.FileAccessChecker entry point its production adapter uses, and the
// suite asserts every lane agrees with every other lane and with the expected
// decision — including DENY ACEs and SID-only grants (the bug class).
//
// Run it from each backend's conformance test (memory / badger / postgres) so
// the matrix is (memory|badger|postgres) x (NFSv3|NFSv4.0|NFSv4.1|SMB).
func RunCrossProtocolPermissionMatrix(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("ReadWriteMatrix", func(t *testing.T) { testCrossProtocolReadWrite(t, factory) })
	t.Run("DenyACE", func(t *testing.T) { testCrossProtocolDenyACE(t, factory) })
	t.Run("SIDOnlyGrant", func(t *testing.T) { testCrossProtocolSIDOnlyGrant(t, factory) })
	t.Run("SIDOnlyDeny", func(t *testing.T) { testCrossProtocolSIDOnlyDeny(t, factory) })
	t.Run("ShareCeiling", func(t *testing.T) { testCrossProtocolShareCeiling(t, factory) })
}

// canonOp is a backend-neutral operation the matrix probes through every
// protocol lane.
type canonOp int

const (
	opRead canonOp = iota
	opWrite
)

func (o canonOp) String() string {
	if o == opRead {
		return "READ"
	}
	return "WRITE"
}

// protocolLane evaluates a canonical operation against a file through the
// translation + checker path a specific protocol adapter uses. It returns
// whether the operation is allowed. err is reserved for checker failures the
// caller treats as fatal (not allow/deny).
type protocolLane struct {
	name string
	eval func(t *testing.T, svc *metadata.Service, handle metadata.FileHandle, file *metadata.File, authCtx *metadata.AuthContext, op canonOp) bool
}

// nfsLane drives the canonical op through MetadataService.CheckPermissions,
// exactly as the NFSv3 ACCESS handler (internal/adapter/nfs/v3/handlers) and
// the now-fixed NFSv4 ACCESS handler (internal/adapter/nfs/v4/handlers) do.
// NFSv3, NFSv4.0, and NFSv4.1 all funnel through the identical metadata entry
// point with the same ACCESS-bit -> Permission mapping (the bit values are
// numerically identical across RFC 1813 §3.3.4 and RFC 7530 §6), so one lane
// faithfully models all three NFS versions.
func nfsLane(name string) protocolLane {
	return protocolLane{
		name: name,
		eval: func(t *testing.T, svc *metadata.Service, handle metadata.FileHandle, file *metadata.File, authCtx *metadata.AuthContext, op canonOp) bool {
			t.Helper()
			var req metadata.Permission
			switch op {
			case opRead:
				if file.Type == metadata.FileTypeDirectory {
					req = metadata.PermissionListDirectory
				} else {
					req = metadata.PermissionRead
				}
			case opWrite:
				req = metadata.PermissionWrite
			}
			granted, err := svc.CheckPermissions(authCtx, handle, req)
			if err != nil {
				t.Fatalf("%s: CheckPermissions(%s) error: %v", name, op, err)
			}
			return granted&req == req
		},
	}
}

// smbLane models the EFFECTIVE SMB read/write decision, which the protocol
// reaches through two gates that must BOTH pass for an operation to succeed:
//
//  1. The CREATE open-time gate — MetadataService.CheckFileAccess: DesiredAccess
//     vs the file's DACL (MS-SMB2 §3.3.5.9). For a file with NO DACL this gate
//     is intentionally permissive (MS-DTYP §2.5.3: a NULL DACL grants every
//     right), deferring enforcement to the operation layer.
//  2. The operation-level POSIX/ACL check — the same checkFilePermissions /
//     calculatePermissions core every metadata READ/WRITE runs (io.go), reached
//     here via CheckPermissions. This is the IDENTICAL core the NFS lanes use.
//
// A real SMB client must pass both, so the lane ANDs them. The result is the
// effective allow/deny a Windows client observes, which the suite then asserts
// matches the NFS lanes — the cross-protocol unification guarantee.
func smbLane() protocolLane {
	return protocolLane{
		name: "SMB",
		eval: func(t *testing.T, svc *metadata.Service, handle metadata.FileHandle, file *metadata.File, authCtx *metadata.AuthContext, op canonOp) bool {
			t.Helper()
			var mask uint32
			var perm metadata.Permission
			switch op {
			case opRead:
				mask = acl.ACE4_READ_DATA
				perm = metadata.PermissionRead
			case opWrite:
				mask = acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA
				perm = metadata.PermissionWrite
			}

			// Gate 1: open-time DACL check.
			granted, err := svc.CheckFileAccess(file, authCtx, mask)
			if err != nil {
				var storeErr *metadata.StoreError
				if errors.As(err, &storeErr) && storeErr.Code == metadata.ErrAccessDenied {
					return false
				}
				t.Fatalf("SMB: CheckFileAccess(%s) unexpected error: %v", op, err)
			}
			if granted&mask != mask {
				return false
			}

			// Gate 2: operation-level POSIX/ACL check (same core as NFS + the
			// metadata READ/WRITE ops).
			opGranted, err := svc.CheckPermissions(authCtx, handle, perm)
			if err != nil {
				t.Fatalf("SMB: CheckPermissions(%s) unexpected error: %v", op, err)
			}
			return opGranted&perm == perm
		},
	}
}

// allLanes is the full protocol matrix every case is evaluated against.
func allLanes() []protocolLane {
	return []protocolLane{
		nfsLane("NFSv3"),
		nfsLane("NFSv4.0"),
		nfsLane("NFSv4.1"),
		smbLane(),
	}
}

// crossProtocolFixture wraps a freshly-built store in a metadata.Service and
// exposes the share root so cases can create files.
type crossProtocolFixture struct {
	svc        *metadata.Service
	store      metadata.Store
	shareName  string
	rootHandle metadata.FileHandle
}

func newCrossProtocolFixture(t *testing.T, factory StoreFactory) *crossProtocolFixture {
	t.Helper()

	store := factory(t)
	shareName := "/xproto"
	ctx := context.Background()

	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("CreateShare(%q): %v", shareName, err)
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o777,
		UID:  0,
		GID:  0,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", shareName, err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle(root): %v", err)
	}

	svc := metadata.New()
	if err := svc.RegisterStoreForShare(shareName, store); err != nil {
		t.Fatalf("RegisterStoreForShare(%q): %v", shareName, err)
	}

	return &crossProtocolFixture{
		svc:        svc,
		store:      store,
		shareName:  shareName,
		rootHandle: rootHandle,
	}
}

// rootCtx returns a root (UID 0) auth context.
func (f *crossProtocolFixture) rootCtx() *metadata.AuthContext {
	return f.userCtx(0, 0, "")
}

// userCtx builds an auth context for the given UID/GID and optional SID.
//
// ShareReadOnly is left false so the matrix exercises the full ACL/POSIX
// evaluation path. The share permission is a ceiling, never a floor, so a
// read-write share is simply the absence of the ShareReadOnly restriction —
// the file's ACL/POSIX alone decides write.
func (f *crossProtocolFixture) userCtx(uid, gid uint32, sid string) *metadata.AuthContext {
	id := &metadata.Identity{
		UID:  metadata.Uint32Ptr(uid),
		GID:  metadata.Uint32Ptr(gid),
		GIDs: []uint32{gid},
	}
	if sid != "" {
		s := sid
		id.SID = &s
	}
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "test",
		Identity:   id,
		ClientAddr: "127.0.0.1",
	}
}

// createFile creates a regular file as root with the given attributes and
// returns the file + its handle.
func (f *crossProtocolFixture) createFile(t *testing.T, name string, attr *metadata.FileAttr) (*metadata.File, metadata.FileHandle) {
	t.Helper()
	attr.Type = metadata.FileTypeRegular
	file, _, err := f.svc.CreateFile(f.rootCtx(), f.rootHandle, name, attr)
	if err != nil {
		t.Fatalf("CreateFile(%q): %v", name, err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle(%q): %v", name, err)
	}
	// Re-read through the service so the file carries whatever the backend
	// actually persisted (postgres/badger round-trip the ACL).
	stored, err := f.svc.GetFile(context.Background(), handle)
	if err != nil {
		t.Fatalf("GetFile(%q): %v", name, err)
	}
	return stored, handle
}

// assertLanesAgree evaluates op on every protocol lane and asserts they all
// match each other and the expected decision. This is the core cross-protocol
// invariant.
func assertLanesAgree(t *testing.T, f *crossProtocolFixture, handle metadata.FileHandle, file *metadata.File, authCtx *metadata.AuthContext, op canonOp, want bool) {
	t.Helper()
	for _, lane := range allLanes() {
		got := lane.eval(t, f.svc, handle, file, authCtx, op)
		if got != want {
			t.Errorf("lane %s op %s: got allowed=%v, want %v (other lanes must agree — cross-protocol drift)", lane.name, op, got, want)
		}
	}
}

// testCrossProtocolReadWrite checks POSIX-mode and allow-ACL files: every
// protocol must reach the same read/write decision for owner, group, other,
// and a DACL granting EVERYONE@ read-only.
func testCrossProtocolReadWrite(t *testing.T, factory StoreFactory) {
	f := newCrossProtocolFixture(t, factory)

	ownerUID, ownerGID := uint32(1000), uint32(2000)

	// Case 1: mode 0o640 — owner rw, group r, other none, no ACL.
	mode640, mode640H := f.createFile(t, "mode640.txt", &metadata.FileAttr{Mode: 0o640, UID: ownerUID, GID: ownerGID})

	// Owner: read allowed, write allowed.
	owner := f.userCtx(ownerUID, ownerGID, "")
	assertLanesAgree(t, f, mode640H, mode640, owner, opRead, true)
	assertLanesAgree(t, f, mode640H, mode640, owner, opWrite, true)

	// Group member (different UID, same GID): read allowed, write denied.
	groupMember := f.userCtx(3000, ownerGID, "")
	assertLanesAgree(t, f, mode640H, mode640, groupMember, opRead, true)
	assertLanesAgree(t, f, mode640H, mode640, groupMember, opWrite, false)

	// Other (no UID/GID overlap): read denied, write denied.
	stranger := f.userCtx(4000, 5000, "")
	assertLanesAgree(t, f, mode640H, mode640, stranger, opRead, false)
	assertLanesAgree(t, f, mode640H, mode640, stranger, opWrite, false)

	// Case 2: allow-only DACL granting EVERYONE@ read but not write.
	everyoneRead := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: acl.ACE4_READ_DATA},
		},
	}
	aclFile, aclH := f.createFile(t, "everyone_read.txt", &metadata.FileAttr{Mode: 0o600, UID: ownerUID, GID: ownerGID, ACL: everyoneRead})

	// Any user can read; nobody (non-root) can write — the DACL grants only read.
	assertLanesAgree(t, f, aclH, aclFile, stranger, opRead, true)
	assertLanesAgree(t, f, aclH, aclFile, stranger, opWrite, false)
}

// testCrossProtocolDenyACE is the headline negative control: a DENY ACE on the
// owner must override the POSIX owner-everything fallback identically on every
// protocol. NFSv4's old Unix-mode-only path would have granted write here while
// SMB denied it — exactly the bug AD-0 closes.
func testCrossProtocolDenyACE(t *testing.T, factory StoreFactory) {
	f := newCrossProtocolFixture(t, factory)

	ownerUID, ownerGID := uint32(1001), uint32(2001)
	ownerSID := "S-1-5-21-1-2-3-1001"

	// DENY write to the owner's SID, then ALLOW EVERYONE@ everything. The
	// DENY must win for the owner (DENY precedes ALLOW in evaluation).
	denyWrite := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_DENIED_ACE_TYPE, Who: "sid:" + ownerSID, AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: 0xFFFFFFFF},
		},
	}
	file, handle := f.createFile(t, "deny_owner_write.txt", &metadata.FileAttr{
		Mode: 0o777, UID: ownerUID, GID: ownerGID, ACL: denyWrite,
	})

	owner := f.userCtx(ownerUID, ownerGID, ownerSID)

	// Read is allowed (EVERYONE@ grants it); WRITE must be denied on EVERY
	// protocol because the DENY ACE targets the owner's SID. If NFS lanes
	// disagree with SMB here, the cross-protocol bug has regressed.
	assertLanesAgree(t, f, handle, file, owner, opRead, true)
	assertLanesAgree(t, f, handle, file, owner, opWrite, false)

	// A different user (not matched by the DENY) gets full access via the
	// EVERYONE@ ALLOW — identical on every protocol.
	other := f.userCtx(9999, 9999, "S-1-5-21-1-2-3-9999")
	assertLanesAgree(t, f, handle, file, other, opRead, true)
	assertLanesAgree(t, f, handle, file, other, opWrite, true)
}

// testCrossProtocolSIDOnlyGrant pins the second bug class: a grant addressed
// ONLY by SID (no Unix UID/GID match, mode 0o000) must be honored identically
// on NFS and SMB. NFSv4's old mode-only ACCESS path ignored SID grants entirely
// and would have denied a user that SMB allows.
func testCrossProtocolSIDOnlyGrant(t *testing.T, factory StoreFactory) {
	f := newCrossProtocolFixture(t, factory)

	granteeSID := "S-1-5-21-7-7-7-2500"

	// mode 0o000 so POSIX bits grant nothing; the ONLY path to access is the
	// SID-addressed ALLOW ACE.
	sidGrant := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: "sid:" + granteeSID, AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
		},
	}
	file, handle := f.createFile(t, "sid_only_grant.txt", &metadata.FileAttr{
		Mode: 0o000, UID: 6000, GID: 6000, ACL: sidGrant,
	})

	// The grantee is identified ONLY by SID; its UID/GID match nothing on the
	// file. Read and write must be allowed on every protocol.
	grantee := f.userCtx(8000, 8000, granteeSID)
	assertLanesAgree(t, f, handle, file, grantee, opRead, true)
	assertLanesAgree(t, f, handle, file, grantee, opWrite, true)

	// A user with a different SID and no POSIX rights is denied everywhere.
	outsider := f.userCtx(8001, 8001, "S-1-5-21-7-7-7-9000")
	assertLanesAgree(t, f, handle, file, outsider, opRead, false)
	assertLanesAgree(t, f, handle, file, outsider, opWrite, false)
}

// testCrossProtocolSIDOnlyDeny is the symmetric SID negative control: a DENY
// ACE addressed only by SID must suppress access the POSIX mode would otherwise
// grant — identically on NFS and SMB.
func testCrossProtocolSIDOnlyDeny(t *testing.T, factory StoreFactory) {
	f := newCrossProtocolFixture(t, factory)

	deniedSID := "S-1-5-21-3-3-3-4242"

	// mode 0o666 grants read+write to everyone via POSIX, but a SID-addressed
	// DENY removes write for one specific principal.
	sidDeny := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_DENIED_ACE_TYPE, Who: "sid:" + deniedSID, AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
		},
	}
	file, handle := f.createFile(t, "sid_only_deny.txt", &metadata.FileAttr{
		Mode: 0o666, UID: 7000, GID: 7000, ACL: sidDeny,
	})

	// The denied principal: read allowed (EVERYONE@), write denied (SID DENY) —
	// on every protocol.
	denied := f.userCtx(7500, 7500, deniedSID)
	assertLanesAgree(t, f, handle, file, denied, opRead, true)
	assertLanesAgree(t, f, handle, file, denied, opWrite, false)

	// An unrelated principal keeps full EVERYONE@ access on every protocol.
	other := f.userCtx(7600, 7600, "S-1-5-21-3-3-3-1111")
	assertLanesAgree(t, f, handle, file, other, opRead, true)
	assertLanesAgree(t, f, handle, file, other, opWrite, true)
}

// testCrossProtocolShareCeiling pins the share-permission model: the share grant
// is a ceiling, never a floor. A read-write share grants no write that the file's
// own ACL/POSIX denies — it can only restrict — so NFS and SMB reach the same
// decision from the file's ACL for every case. Mirrors Windows (share ∧ NTFS),
// NetApp export Access_Type, NFS-Ganesha, and Samba (whose "write list" cannot
// grant beyond the underlying filesystem perms).
//
// All cases run on a read-write share (ShareReadOnly=false, the userCtx default).
func testCrossProtocolShareCeiling(t *testing.T, factory StoreFactory) {
	f := newCrossProtocolFixture(t, factory)

	// (1) SECURITY — explicit DENY-write ACE → write denied on every protocol.
	// A read-write share must never let a user past an explicit DENY.
	denySID := "S-1-5-21-9-9-9-6000"
	denyWrite := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_DENIED_ACE_TYPE, Who: "sid:" + denySID, AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: acl.ACE4_READ_DATA},
		},
	}
	denyFile, denyH := f.createFile(t, "ceiling_deny.txt", &metadata.FileAttr{
		Mode: 0o666, UID: 6000, GID: 6000, ACL: denyWrite,
	})
	denied := f.userCtx(6500, 6500, denySID)
	assertLanesAgree(t, f, denyH, denyFile, denied, opRead, true)
	assertLanesAgree(t, f, denyH, denyFile, denied, opWrite, false)

	// (2) CEILING — allow-ONLY DACL granting only READ, on a mode 0o000 file. The
	// file grants the requester no write by either ACL or POSIX, so even on a
	// read-write share write is denied on every protocol, while read stays granted
	// (the DACL allows it). NFS and SMB agree.
	allowOnly := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: acl.ACE4_READ_DATA},
		},
	}
	allowFile, allowH := f.createFile(t, "ceiling_allow_only.txt", &metadata.FileAttr{
		Mode: 0o000, UID: 7000, GID: 7000, ACL: allowOnly,
	})
	other := f.userCtx(7500, 7500, "S-1-5-21-9-9-9-7000")
	assertLanesAgree(t, f, allowH, allowFile, other, opRead, true)
	assertLanesAgree(t, f, allowH, allowFile, other, opWrite, false)

	// (3) ADDITIVE GRANT — allow-only DACL that DOES grant the requester write.
	// Write is granted on every protocol, sourced from the ACL itself: an allow
	// ACE is additive over the POSIX mode, so a file whose mode denies everyone
	// still grants write to a principal the DACL names.
	grantSID := "S-1-5-21-9-9-9-8000"
	allowWrite := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: "sid:" + grantSID, AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
		},
	}
	grantFile, grantH := f.createFile(t, "ceiling_allow_write.txt", &metadata.FileAttr{
		Mode: 0o000, UID: 8000, GID: 8000, ACL: allowWrite,
	})
	grantee := f.userCtx(8500, 8500, grantSID)
	assertLanesAgree(t, f, grantH, grantFile, grantee, opRead, true)
	assertLanesAgree(t, f, grantH, grantFile, grantee, opWrite, true)
}
