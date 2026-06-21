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
	t.Run("HandleWriteAuthorization", func(t *testing.T) { testCrossProtocolHandleWriteAuthorization(t, factory) })
	t.Run("PerUserReadOnlyShare", func(t *testing.T) { testCrossProtocolPerUserReadOnlyShare(t, factory) })
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
// SMB write authorization is HANDLE-BASED for files that carry a DACL: gate 1
// (CheckFileAccess) is the DACL ceiling enforced at open. When the file has a
// DACL and gate 1 grants write, the production WRITE / SET_INFO handler sets
// AuthContext.WriteAuthorizedByHandle from the open handle's GrantedAccess, and
// gate 2 must then honor the handle rather than re-deny on the file's POSIX mode
// (the smb2.durable-open.read-only scenario). An explicit DENY-write ACE strips
// write at gate 1, so the handle never carries write and the op is denied — the
// security invariant the matrix pins.
//
// For files with NO DACL, gate 1 is intentionally permissive (NULL DACL grants
// everything, MS-DTYP §2.5.3) and is therefore not a real authorization — the
// POSIX-mode per-op check is. The lane leaves WriteAuthorizedByHandle unset in
// that case so SMB and NFS reach the SAME POSIX decision, preserving the
// cross-protocol agreement on plain mode-bit files.
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

			// Gate 1: open-time DACL check. This is the authorization boundary
			// for SMB — an explicit DENY-write ACE or a DACL that omits write
			// strips write from the handle here.
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
			// metadata READ/WRITE ops). For writes to a DACL-bearing file, gate 1
			// was the real DACL authorization and granted write, so the real
			// handler sets WriteAuthorizedByHandle from OpenFile.GrantedAccess.
			// Model that on a copy of the context so the shared authCtx the NFS
			// lanes use stays unmutated. For no-DACL files gate 1 is a permissive
			// freebie, so we leave the flag unset and SMB falls through to the
			// same POSIX check NFS runs.
			opCtx := authCtx
			if op == opWrite && file.ACL != nil {
				ctxCopy := *authCtx
				ctxCopy.WriteAuthorizedByHandle = true
				opCtx = &ctxCopy
			}

			opGranted, err := svc.CheckPermissions(opCtx, handle, perm)
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
// WriteAuthorizedByHandle/ShareReadOnly are left at their zero value (false) so
// the NFS lanes exercise the full ACL/POSIX evaluation path. The SMB lane stamps
// WriteAuthorizedByHandle itself (handle-based write authorization) when gate 1
// grants write on a DACL-bearing file — see smbLane and
// testCrossProtocolHandleWriteAuthorization.
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

// testCrossProtocolHandleWriteAuthorization models SMB's HANDLE-BASED write
// authorization (#1240) and pins the security invariant against an explicit
// DENY-write ACE.
//
// SMB is handle-based: a handle granted FILE_WRITE_DATA at open (the gate-1
// CheckFileAccess DACL evaluation) may write regardless of the file's POSIX
// mode. The smbLane reproduces this by stamping WriteAuthorizedByHandle on the
// op context whenever gate 1 grants write on a DACL-bearing file. NFS has no
// handle and re-checks the ACL/POSIX per op.
//
// The security-critical invariant: with an explicit DENY-write ACE, gate 1
// strips FILE_WRITE_DATA from the open, so the handle never carries write and
// the metadata layer is never told WriteAuthorizedByHandle. Write stays denied
// on every protocol — a handle can never be minted past an explicit DENY, so
// the metadata-layer handle bypass can never override one.
func testCrossProtocolHandleWriteAuthorization(t *testing.T, factory StoreFactory) {
	f := newCrossProtocolFixture(t, factory)

	// Explicit DENY-write ACE → write denied on every protocol. On SMB the open
	// (gate 1) denies write, so no write-authorized handle is ever minted; the
	// metadata-layer handle bypass therefore can never reach this file.
	denySID := "S-1-5-21-9-9-9-6000"
	denyWrite := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_DENIED_ACE_TYPE, Who: "sid:" + denySID, AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: acl.ACE4_READ_DATA},
		},
	}
	denyFile, denyH := f.createFile(t, "handle_write_deny.txt", &metadata.FileAttr{
		Mode: 0o666, UID: 6000, GID: 6000, ACL: denyWrite,
	})
	denied := f.userCtx(6500, 6500, denySID)
	assertLanesAgree(t, f, denyH, denyFile, denied, opRead, true)
	assertLanesAgree(t, f, denyH, denyFile, denied, opWrite, false)

	// Allow-write DACL on a mode 0o000 file: the SMB open grants write (DACL
	// authorization), so the handle is write-authorized and the write succeeds
	// despite the file mode granting nothing. NFS, which re-checks the ACL per
	// op, also grants via the SID-addressed ALLOW. Both protocols agree: WRITE
	// allowed even though POSIX mode is 0o000 — the handle/ACL is the authority.
	grantSID := "S-1-5-21-9-9-9-7000"
	allowWrite := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: "sid:" + grantSID, AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
		},
	}
	allowFile, allowH := f.createFile(t, "handle_write_allow.txt", &metadata.FileAttr{
		Mode: 0o000, UID: 6000, GID: 6000, ACL: allowWrite,
	})
	granted := f.userCtx(6600, 6600, grantSID)
	assertLanesAgree(t, f, allowH, allowFile, granted, opRead, true)
	assertLanesAgree(t, f, allowH, allowFile, granted, opWrite, true)
}

// testCrossProtocolPerUserReadOnlyShare pins the #1276 ceiling: when the
// requester's PER-USER share permission is read-only (AuthContext.ShareReadOnly),
// write is denied on every protocol even though the file's POSIX mode or ACL
// would otherwise grant it — while read stays allowed. This is the restriction
// direction complementing the handle/ACL grant cases above.
//
// ShareReadOnly is the per-user grant (set by NFS auth_helper / SMB tree-connect
// from the resolved share permission); it is independent of the store-level
// ShareOptions.ReadOnly. The share here is stored read-write, so only the
// per-user flag forces the denial. The SMB lane's open-time gate (CheckFileAccess)
// does not consult ShareReadOnly, so for a DACL-granting file it still mints a
// write-authorized handle — but the operation-level metadata funnel (gate 2,
// shared with NFS) strips write under ShareReadOnly, so both protocols agree.
func testCrossProtocolPerUserReadOnlyShare(t *testing.T, factory StoreFactory) {
	f := newCrossProtocolFixture(t, factory)

	ownerUID, ownerGID := uint32(1300), uint32(1300)

	// Case 1: no-ACL file, mode 0o666 — POSIX would grant write to everyone.
	posixFile, posixH := f.createFile(t, "ro_user_posix.txt", &metadata.FileAttr{
		Mode: 0o666, UID: ownerUID, GID: ownerGID,
	})

	// Read-write user: baseline write allowed (sanity that the file/grant is real).
	rw := f.userCtx(ownerUID, ownerGID, "")
	assertLanesAgree(t, f, posixH, posixFile, rw, opWrite, true)

	// Read-only user (same identity, ShareReadOnly set): read allowed, write denied.
	ro := f.userCtx(ownerUID, ownerGID, "")
	ro.ShareReadOnly = true
	assertLanesAgree(t, f, posixH, posixFile, ro, opRead, true)
	assertLanesAgree(t, f, posixH, posixFile, ro, opWrite, false)

	// Case 2: DACL granting EVERYONE@ full access — the ACL would grant write,
	// but the per-user read-only ceiling must still deny it on every protocol.
	allowAll := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: 0xFFFFFFFF},
		},
	}
	aclFile, aclH := f.createFile(t, "ro_user_acl.txt", &metadata.FileAttr{
		Mode: 0o600, UID: ownerUID, GID: ownerGID, ACL: allowAll,
	})

	roACL := f.userCtx(7700, 7700, "S-1-5-21-1-3-0-7700")
	roACL.ShareReadOnly = true
	assertLanesAgree(t, f, aclH, aclFile, roACL, opRead, true)
	assertLanesAgree(t, f, aclH, aclFile, roACL, opWrite, false)
}
