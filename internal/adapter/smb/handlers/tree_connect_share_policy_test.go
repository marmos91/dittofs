package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

const (
	policyShare     = "/secure"
	policyOpenShare = "/open"
	policyInsideIP  = "10.1.2.3"
	policyOutsideIP = "198.51.100.7"
)

// newSharePolicyHandler builds a handler over a real runtime holding one
// read-write share that admits only Kerberos and only clients in a netgroup
// whose single member is policyInsideIP, and a read-write share
// (policyOpenShare) with no policy at all.
func newSharePolicyHandler(t *testing.T) (*Handler, *runtime.Runtime) {
	t.Helper()
	rt, bsID := newTestShareRuntime(t)
	if err := rt.RegisterMetadataStore("test-meta", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              policyShare,
		MetadataStore:     "test-meta",
		BlockStoreID:      bsID,
		Enabled:           true,
		DefaultPermission: "read-write",
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              policyOpenShare,
		MetadataStore:     "test-meta",
		BlockStoreID:      bsID,
		Enabled:           true,
		DefaultPermission: "read-write",
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare(%s): %v", policyOpenShare, err)
	}
	if err := rt.SetExportAuthPolicyForTesting(policyShare, true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	ns, ok := rt.Store().(store.NetgroupStore)
	if !ok {
		t.Fatal("control-plane store does not implement NetgroupStore")
	}
	ctx := context.Background()
	if _, err := ns.CreateNetgroup(ctx, &models.Netgroup{ID: uuid.New().String(), Name: "office"}); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}
	if err := ns.AddNetgroupMember(ctx, "office", &models.NetgroupMember{Type: "ip", Value: policyInsideIP}); err != nil {
		t.Fatalf("AddNetgroupMember: %v", err)
	}
	if err := rt.SetShareNetgroup(policyShare, "office"); err != nil {
		t.Fatalf("SetShareNetgroup: %v", err)
	}

	h := NewHandler()
	h.Registry = defaultGrantRuntime{rt}
	return h, rt
}

// defaultGrantRuntime resolves every session to the share's default
// permission, so the share policy is the only thing under test.
type defaultGrantRuntime struct{ smbRuntime }

func (defaultGrantRuntime) GetUserStore() models.UserStore { return nil }

func policyUser() *models.User {
	return &models.User{ID: "u1", Username: "alice", Enabled: true}
}

// kerberosSession goes through the constructor the Kerberos SESSION_SETUP path
// uses, so the session carries whatever that path marks on it.
func kerberosSession(h *Handler, ip string) *session.Session {
	return h.CreateSessionWithUserAndExpiry(h.GenerateSessionID(), ip+":50000", policyUser(), "CORP", time.Now().Add(time.Hour))
}

// ntlmSession goes through the constructor the NTLM SESSION_SETUP path uses.
func ntlmSession(h *Handler, ip string) *session.Session {
	return h.CreateSessionWithUser(h.GenerateSessionID(), ip+":50000", policyUser(), "CORP")
}

func treeConnectFrom(t *testing.T, h *Handler, sess *session.Session, ip string) (*HandlerResult, *SMBHandlerContext) {
	t.Helper()
	ctx := NewSMBHandlerContext(context.Background(), ip+":50000", sess.SessionID, 0, 1)
	result, err := h.TreeConnect(ctx, buildTreeConnectRequestBody(`\\server\secure`))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	return result, ctx
}

// An NTLM session from inside the netgroup must not reach a Kerberos-only share.
func TestTreeConnect_SharePolicy_NTLMRefusedOnKerberosOnlyShare(t *testing.T) {
	h, _ := newSharePolicyHandler(t)
	sess := ntlmSession(h, policyInsideIP)

	result, ctx := treeConnectFrom(t, h, sess, policyInsideIP)
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("NTLM TREE_CONNECT to a Kerberos-only share: status = %s, want STATUS_ACCESS_DENIED", result.Status)
	}
	if ctx.TreeID != 0 {
		t.Fatalf("NTLM TREE_CONNECT to a Kerberos-only share published tree %d", ctx.TreeID)
	}
}

// guestSession and anonymousSession go through the constructor the guest and
// anonymous SESSION_SETUP paths use.
func guestSession(h *Handler, ip string) *session.Session {
	return h.CreateSessionWithID(h.GenerateSessionID(), ip+":50000", true, "guest", "")
}

func anonymousSession(h *Handler, ip string) *session.Session {
	return h.CreateSessionWithID(h.GenerateSessionID(), ip+":50000", false, "", "")
}

// allow_auth_sys=false refuses the sessions nothing verified, guest and
// anonymous, and admits NTLM, which is a real proof.
func TestTreeConnect_SharePolicy_AuthSysDisallowedRefusesGuestAndAnonymousOnly(t *testing.T) {
	h, rt := newSharePolicyHandler(t)
	if err := rt.SetExportAuthPolicyForTesting(policyShare, false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	for _, tc := range []struct {
		name string
		sess *session.Session
		want types.Status
	}{
		{"guest", guestSession(h, policyInsideIP), types.StatusAccessDenied},
		{"anonymous", anonymousSession(h, policyInsideIP), types.StatusAccessDenied},
		{"NTLM", ntlmSession(h, policyInsideIP), types.StatusSuccess},
	} {
		if result, _ := treeConnectFrom(t, h, tc.sess, policyInsideIP); result.Status != tc.want {
			t.Errorf("%s TREE_CONNECT with allow_auth_sys=false: status = %s, want %s", tc.name, result.Status, tc.want)
		}
	}
}

// A Kerberos session from outside the netgroup must not reach the share.
func TestTreeConnect_SharePolicy_OutsideNetgroupRefused(t *testing.T) {
	h, _ := newSharePolicyHandler(t)
	sess := kerberosSession(h, policyOutsideIP)

	result, ctx := treeConnectFrom(t, h, sess, policyOutsideIP)
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("TREE_CONNECT from outside the netgroup: status = %s, want STATUS_ACCESS_DENIED", result.Status)
	}
	if ctx.TreeID != 0 {
		t.Fatalf("TREE_CONNECT from outside the netgroup published tree %d", ctx.TreeID)
	}
}

// A Kerberos session from inside the netgroup is admitted with its grant.
func TestTreeConnect_SharePolicy_KerberosInsideNetgroupAccepted(t *testing.T) {
	h, _ := newSharePolicyHandler(t)
	sess := kerberosSession(h, policyInsideIP)

	result, ctx := treeConnectFrom(t, h, sess, policyInsideIP)
	if result.Status != types.StatusSuccess {
		t.Fatalf("Kerberos TREE_CONNECT from inside the netgroup: status = %s, want STATUS_SUCCESS", result.Status)
	}
	tree, ok := h.GetTree(ctx.TreeID)
	if !ok {
		t.Fatal("accepted TREE_CONNECT published no tree")
	}
	if tree.Permission != models.PermissionReadWrite {
		t.Fatalf("tree permission = %s, want read-write", tree.Permission)
	}
}

// A connected tree loses access on the next request when the session
// re-authenticates over NTLM, which MS-SMB2 lets it do without dropping the
// tree, or when the request arrives from an address outside the netgroup.
func TestTreeSharePolicyStatus_RechecksEstablishedTree(t *testing.T) {
	h, _ := newSharePolicyHandler(t)
	sess := kerberosSession(h, policyInsideIP)
	result, ctx := treeConnectFrom(t, h, sess, policyInsideIP)
	if result.Status != types.StatusSuccess {
		t.Fatalf("setup TREE_CONNECT: status = %s", result.Status)
	}
	tree, _ := h.GetTree(ctx.TreeID)
	bg := context.Background()

	if st := h.TreeSharePolicyStatus(bg, tree, policyInsideIP+":50000"); st != 0 {
		t.Fatalf("Kerberos request from inside the netgroup: status = %s, want allowed", st)
	}
	if st := h.TreeSharePolicyStatus(bg, tree, policyOutsideIP+":50001"); st != types.StatusAccessDenied {
		t.Fatalf("request on the tree from outside the netgroup: status = %s, want STATUS_ACCESS_DENIED", st)
	}

	sess.UpdateIdentity("alice", "CORP", policyUser(), false, false, nil, "")
	if st := h.TreeSharePolicyStatus(bg, tree, policyInsideIP+":50000"); st != types.StatusAccessDenied {
		t.Fatalf("request after NTLM re-authentication: status = %s, want STATUS_ACCESS_DENIED", st)
	}
}

// krb5p on SMB means the tree requires encryption, for an NTLM session as for
// a Kerberos one, since the floor applies to any authenticated session: one
// that can encrypt gets an encrypted tree, one that cannot is refused. A guest
// session holds no key to encrypt with, so it answers to allow_auth_sys alone.
func TestTreeConnect_SharePolicy_Krb5pRequiresEncryption(t *testing.T) {
	h, rt := newSharePolicyHandler(t)
	// Kerberos is not required, so the floor alone decides for NTLM and guest.
	if err := rt.SetExportAuthPolicyForTesting(policyShare, true, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}
	if err := rt.SetMinKerberosLevelForTesting(policyShare, models.KerberosLevelKrb5p); err != nil {
		t.Fatalf("SetMinKerberosLevelForTesting: %v", err)
	}

	for _, proof := range []struct {
		name       string
		newSession func(*Handler, string) *session.Session
	}{
		{"Kerberos", kerberosSession},
		{"NTLM", ntlmSession},
	} {
		plain := proof.newSession(h, policyInsideIP)
		if result, _ := treeConnectFrom(t, h, plain, policyInsideIP); result.Status != types.StatusAccessDenied {
			t.Fatalf("krb5p share, %s session without encryption: status = %s, want STATUS_ACCESS_DENIED", proof.name, result.Status)
		}

		enc := proof.newSession(h, policyInsideIP)
		cs := session.DeriveAllKeys(make([]byte, 16), types.Dialect0311, [64]byte{}, types.CipherAES128GCM, 0, false)
		if err := cs.CreateEncryptors(types.CipherAES128GCM); err != nil {
			t.Fatalf("CreateEncryptors: %v", err)
		}
		enc.SetCryptoState(cs)
		enc.SetBindIdentity(types.Dialect0311, 0, types.CipherAES128GCM, [16]byte{})

		result, ctx := treeConnectFrom(t, h, enc, policyInsideIP)
		if result.Status != types.StatusSuccess {
			t.Fatalf("krb5p share, %s session that can encrypt: status = %s, want STATUS_SUCCESS", proof.name, result.Status)
		}
		tree, _ := h.GetTree(ctx.TreeID)
		if tree == nil || !tree.EncryptData {
			t.Fatalf("krb5p tree for a %s session was published without requiring encryption", proof.name)
		}
	}

	if result, _ := treeConnectFrom(t, h, guestSession(h, policyInsideIP), policyInsideIP); result.Status != types.StatusSuccess {
		t.Fatalf("krb5p share, guest session: status = %s, want STATUS_SUCCESS (allow_auth_sys alone decides)", result.Status)
	}
}

// The srvsvc share list applies the same admission as TREE_CONNECT, the share
// policy included: a session is not shown a share the policy would refuse it.
func TestSRVSVC_SharePolicyHidesShare(t *testing.T) {
	h, _ := newSharePolicyHandler(t)
	secure, open := encodeUTF16LE("secure"), encodeUTF16LE("open")

	for _, tc := range []struct {
		name       string
		sess       *session.Session
		wantSecure bool
	}{
		{"Kerberos inside the netgroup", kerberosSession(h, policyInsideIP), true},
		{"NTLM", ntlmSession(h, policyInsideIP), false},
		{"Kerberos outside the netgroup", kerberosSession(h, policyOutsideIP), false},
	} {
		resp := srvsvcEnum(openSRVSVCPipe(t, h, tc.sess.SessionID), 1)
		if !bytes.Contains(resp, open) {
			t.Errorf("%s session: NetrShareEnum omits /open, which has no policy", tc.name)
		}
		if got := bytes.Contains(resp, secure); got != tc.wantSecure {
			t.Errorf("%s session: NetrShareEnum lists /secure = %v, want %v", tc.name, got, tc.wantSecure)
		}
	}
}

func treeConnectFromAddr(t *testing.T, h *Handler, sess *session.Session, addr string) types.Status {
	t.Helper()
	ctx := NewSMBHandlerContext(context.Background(), addr, sess.SessionID, 0, 1)
	result, err := h.TreeConnect(ctx, buildTreeConnectRequestBody(`\\server\secure`))
	if err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	return result.Status
}

// A link-local IPv6 peer carries its interface zone in the address; the zone
// must not keep it from matching the netgroup member that names the address.
func TestTreeConnect_SharePolicy_ScopedIPv6PeerMatchesNetgroup(t *testing.T) {
	h, rt := newSharePolicyHandler(t)
	ns := rt.Store().(store.NetgroupStore)
	if err := ns.AddNetgroupMember(context.Background(), "office", &models.NetgroupMember{Type: "ip", Value: "fe80::1234"}); err != nil {
		t.Fatalf("AddNetgroupMember: %v", err)
	}
	addr := "[fe80::1234%eth0]:50000"
	sess := h.CreateSessionWithUserAndExpiry(h.GenerateSessionID(), addr, policyUser(), "CORP", time.Now().Add(time.Hour))

	if st := treeConnectFromAddr(t, h, sess, addr); st != types.StatusSuccess {
		t.Fatalf("TREE_CONNECT from a scoped IPv6 netgroup member: status = %s, want STATUS_SUCCESS", st)
	}
}

// A peer address that is not an IP is refused only by a share that has a
// netgroup allowlist; a share without one admits it.
func TestTreeConnect_SharePolicy_NonIPPeerAdmittedWithoutAllowlist(t *testing.T) {
	h, rt := newSharePolicyHandler(t)
	const addr = "not-an-ip"
	sess := h.CreateSessionWithUserAndExpiry(h.GenerateSessionID(), addr, policyUser(), "CORP", time.Now().Add(time.Hour))

	if st := treeConnectFromAddr(t, h, sess, addr); st != types.StatusAccessDenied {
		t.Fatalf("non-IP peer on a share with an allowlist: status = %s, want STATUS_ACCESS_DENIED", st)
	}
	if err := rt.SetShareNetgroup(policyShare, ""); err != nil {
		t.Fatalf("SetShareNetgroup: %v", err)
	}
	if st := treeConnectFromAddr(t, h, sess, addr); st != types.StatusSuccess {
		t.Fatalf("non-IP peer on a share without an allowlist: status = %s, want STATUS_SUCCESS", st)
	}
}

// COPYCHUNK reads a source reached through a resume key, which may sit on
// another tree of the session. That tree's share policy must admit the request
// too: an NTLM re-authentication must not copy a Kerberos-only share's file
// into an unrestricted one.
func TestCopyChunk_SharePolicy_AppliesToCrossTreeSource(t *testing.T) {
	h, _ := newSharePolicyHandler(t)
	const openShare = policyOpenShare

	sess := kerberosSession(h, policyInsideIP)
	const srcTree, dstTree uint32 = 10, 11
	h.StoreTree(&TreeConnection{TreeID: srcTree, SessionID: sess.SessionID, ShareName: policyShare, Permission: models.PermissionReadWrite})
	h.StoreTree(&TreeConnection{TreeID: dstTree, SessionID: sess.SessionID, ShareName: openShare, Permission: models.PermissionReadWrite})

	srcCtx := NewSMBHandlerContext(context.Background(), policyInsideIP+":50000", sess.SessionID, srcTree, 1)
	srcCtx.ShareName = policyShare
	dstCtx := NewSMBHandlerContext(context.Background(), policyInsideIP+":50000", sess.SessionID, dstTree, 2)
	dstCtx.ShareName = openShare
	srcFileID := storeSecondHandle(t, h, srcCtx, "src", srcTree)
	dstFileID := storeSecondHandle(t, h, dstCtx, "dst", dstTree)

	resumeKey, err := h.resumeKeys.issue(srcFileID)
	if err != nil {
		t.Fatalf("issue resume key: %v", err)
	}
	body := buildIoctlRequestBody(FsctlSrvCopyChunk, dstFileID, copyChunkInput(resumeKey), 4096)

	res, err := h.Ioctl(dstCtx, body)
	if err != nil {
		t.Fatalf("Ioctl (Kerberos): %v", err)
	}
	if res.Status == types.StatusAccessDenied {
		t.Fatalf("Kerberos COPYCHUNK from the Kerberos-only share: status = %s, want admitted", res.Status)
	}

	sess.UpdateIdentity("alice", "CORP", policyUser(), false, false, nil, "")
	res, err = h.Ioctl(dstCtx, body)
	if err != nil {
		t.Fatalf("Ioctl (NTLM): %v", err)
	}
	if res.Status != types.StatusAccessDenied {
		t.Fatalf("COPYCHUNK after NTLM re-authentication with a source on a Kerberos-only share: status = %s, want STATUS_ACCESS_DENIED", res.Status)
	}
}

// lockBody encodes a one-element SMB2 LOCK request [MS-SMB2] 2.2.26.
func lockBody(fileID [16]byte, offset, length uint64, flags uint32) []byte {
	b := make([]byte, 48)
	binary.LittleEndian.PutUint16(b[0:], 48)
	binary.LittleEndian.PutUint16(b[2:], 1)
	copy(b[8:24], fileID[:])
	binary.LittleEndian.PutUint64(b[24:], offset)
	binary.LittleEndian.PutUint64(b[32:], length)
	binary.LittleEndian.PutUint32(b[40:], flags)
	return b
}

// LOCK is exempt from the dispatch-level policy gate so a refused client can
// still release its locks; the handler refuses an acquisition itself.
func TestLock_SharePolicy_RefusesAcquireAdmitsUnlock(t *testing.T) {
	h, _ := newSharePolicyHandler(t)
	sess := kerberosSession(h, policyInsideIP)
	const treeID uint32 = 12
	h.StoreTree(&TreeConnection{TreeID: treeID, SessionID: sess.SessionID, ShareName: policyShare, Permission: models.PermissionReadWrite})
	ctx := NewSMBHandlerContext(context.Background(), policyInsideIP+":50000", sess.SessionID, treeID, 1)
	ctx.ShareName = policyShare
	fileID := storeSecondHandle(t, h, ctx, "locked", treeID)

	lock := lockBody(fileID, 0, 10, SMB2LockFlagSharedLock|SMB2LockFlagFailImmediately)
	if res, err := h.Lock(ctx, lock); err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("Kerberos LOCK: status = %v, err = %v, want STATUS_SUCCESS", res.Status, err)
	}

	sess.UpdateIdentity("alice", "CORP", policyUser(), false, false, nil, "")

	res, err := h.Lock(ctx, lockBody(fileID, 20, 10, SMB2LockFlagSharedLock|SMB2LockFlagFailImmediately))
	if err != nil {
		t.Fatalf("Lock (NTLM acquire): %v", err)
	}
	if res.Status != types.StatusAccessDenied {
		t.Fatalf("LOCK acquisition after NTLM re-authentication on a Kerberos-only share: status = %s, want STATUS_ACCESS_DENIED", res.Status)
	}
	res, err = h.Lock(ctx, lockBody(fileID, 0, 10, SMB2LockFlagUnlock))
	if err != nil {
		t.Fatalf("Lock (NTLM unlock): %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("UNLOCK after NTLM re-authentication: status = %s, want STATUS_SUCCESS so the range is released", res.Status)
	}

	// A tree disconnected after dispatch has no policy left to admit an
	// acquisition, so the handler refuses it rather than skipping the check.
	h.DeleteTree(treeID)
	res, err = h.Lock(ctx, lockBody(fileID, 40, 10, SMB2LockFlagSharedLock|SMB2LockFlagFailImmediately))
	if err != nil {
		t.Fatalf("Lock (tree gone): %v", err)
	}
	if res.Status != types.StatusNetworkNameDeleted {
		t.Fatalf("LOCK acquisition on a disconnected tree: status = %s, want STATUS_NETWORK_NAME_DELETED", res.Status)
	}
}
