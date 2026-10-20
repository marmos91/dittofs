package handlers

import (
	"context"
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
	policyInsideIP  = "10.1.2.3"
	policyOutsideIP = "198.51.100.7"
)

// newSharePolicyHandler builds a handler over a real runtime holding one
// read-write share that admits only Kerberos and only clients in a netgroup
// whose single member is policyInsideIP.
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

// allow_auth_sys=false alone refuses NTLM too.
func TestTreeConnect_SharePolicy_NTLMRefusedWhenAuthSysDisallowed(t *testing.T) {
	h, rt := newSharePolicyHandler(t)
	if err := rt.SetExportAuthPolicyForTesting(policyShare, false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}
	sess := ntlmSession(h, policyInsideIP)

	result, _ := treeConnectFrom(t, h, sess, policyInsideIP)
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("NTLM TREE_CONNECT with allow_auth_sys=false: status = %s, want STATUS_ACCESS_DENIED", result.Status)
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

// krb5p on SMB means the tree requires encryption: a Kerberos session that can
// encrypt gets an encrypted tree, one that cannot is refused.
func TestTreeConnect_SharePolicy_Krb5pRequiresEncryption(t *testing.T) {
	h, rt := newSharePolicyHandler(t)
	if err := rt.SetMinKerberosLevelForTesting(policyShare, models.KerberosLevelKrb5p); err != nil {
		t.Fatalf("SetMinKerberosLevelForTesting: %v", err)
	}

	plain := kerberosSession(h, policyInsideIP)
	if result, _ := treeConnectFrom(t, h, plain, policyInsideIP); result.Status != types.StatusAccessDenied {
		t.Fatalf("krb5p share, session without encryption: status = %s, want STATUS_ACCESS_DENIED", result.Status)
	}

	enc := kerberosSession(h, policyInsideIP)
	cs := session.DeriveAllKeys(make([]byte, 16), types.Dialect0311, [64]byte{}, types.CipherAES128GCM, 0, false)
	if err := cs.CreateEncryptors(types.CipherAES128GCM); err != nil {
		t.Fatalf("CreateEncryptors: %v", err)
	}
	enc.SetCryptoState(cs)
	enc.SetBindIdentity(types.Dialect0311, 0, types.CipherAES128GCM, [16]byte{})

	result, ctx := treeConnectFrom(t, h, enc, policyInsideIP)
	if result.Status != types.StatusSuccess {
		t.Fatalf("krb5p share, session that can encrypt: status = %s, want STATUS_SUCCESS", result.Status)
	}
	tree, _ := h.GetTree(ctx.TreeID)
	if tree == nil || !tree.EncryptData {
		t.Fatal("krb5p tree was published without requiring encryption")
	}
}
