package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// srvsvcBindPDU builds a DCE/RPC bind for the srvsvc interface.
func srvsvcBindPDU(callID uint32) []byte {
	buf := make([]byte, 72)
	buf[0] = 5
	buf[2] = rpc.PDUBind
	buf[3] = rpc.FlagFirstFrag | rpc.FlagLastFrag
	buf[4] = 0x10
	binary.LittleEndian.PutUint16(buf[8:10], 72)
	binary.LittleEndian.PutUint32(buf[12:16], callID)
	binary.LittleEndian.PutUint16(buf[16:18], 4280)
	binary.LittleEndian.PutUint16(buf[18:20], 4280)
	buf[24] = 1
	buf[30] = 1
	copy(buf[32:48], rpc.SRVSVCInterfaceUUID[:])
	binary.LittleEndian.PutUint32(buf[48:52], 3) // srvsvc v3.0
	copy(buf[52:68], rpc.NDRTransferSyntaxUUID[:])
	binary.LittleEndian.PutUint32(buf[68:72], 2)
	return buf
}

// srvsvcRequestPDU wraps a stub in a DCE/RPC request PDU for opnum.
func srvsvcRequestPDU(callID uint32, opnum uint16, stub []byte) []byte {
	fragLen := rpc.HeaderSize + 8 + len(stub)
	buf := make([]byte, fragLen)
	buf[0] = 5
	buf[2] = rpc.PDURequest
	buf[3] = rpc.FlagFirstFrag | rpc.FlagLastFrag
	buf[4] = 0x10
	binary.LittleEndian.PutUint16(buf[8:10], uint16(fragLen))
	binary.LittleEndian.PutUint32(buf[12:16], callID)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(stub)))
	binary.LittleEndian.PutUint16(buf[22:24], opnum)
	copy(buf[24:], stub)
	return buf
}

// ndrWideString encodes s as an NDR conformant/varying wide string.
func ndrWideString(s string) []byte {
	units := uint32(len(s) + 1)
	buf := binary.LittleEndian.AppendUint32(nil, units)
	buf = binary.LittleEndian.AppendUint32(buf, 0)
	buf = binary.LittleEndian.AppendUint32(buf, units)
	buf = append(buf, encodeUTF16LE(s)...)
	buf = append(buf, 0, 0)
	for len(buf)%4 != 0 {
		buf = append(buf, 0)
	}
	return buf
}

// openSRVSVCPipe connects IPC$, opens and binds a srvsvc pipe for sessionID,
// and returns a function issuing one request on it.
func openSRVSVCPipe(t *testing.T, h *Handler, sessionID uint64) func(opnum uint16, stub []byte) []byte {
	t.Helper()
	return openSRVSVCPipeFrom(t, h, sessionID, "127.0.0.1:12345")
}

// openSRVSVCPipeFrom is openSRVSVCPipe over a connection from clientAddr.
func openSRVSVCPipeFrom(t *testing.T, h *Handler, sessionID uint64, clientAddr string) func(opnum uint16, stub []byte) []byte {
	t.Helper()
	ipcCtx := NewSMBHandlerContext(context.Background(), clientAddr, sessionID, 0, 1)
	if res, err := h.TreeConnect(ipcCtx, buildTreeConnectRequestBody(`\\server\IPC$`)); err != nil || res.Status != types.StatusSuccess {
		t.Fatalf("TREE_CONNECT IPC$ failed: %v %v", res.Status, err)
	}
	ipcTree, _ := h.GetTree(ipcCtx.TreeID)
	createResp, err := h.handlePipeCreate(ipcCtx, &CreateRequest{FileName: "srvsvc"}, ipcTree)
	if err != nil || createResp.Status != types.StatusSuccess {
		t.Fatalf("CREATE srvsvc failed: %v %v", createResp.Status, err)
	}
	pipe := h.PipeManager.GetPipe(createResp.FileID)
	if _, err := pipe.Transact(srvsvcBindPDU(1), 65536); err != nil {
		t.Fatalf("bind: %v", err)
	}
	callID := uint32(2)
	return func(opnum uint16, stub []byte) []byte {
		t.Helper()
		resp, err := pipe.Transact(srvsvcRequestPDU(callID, opnum, stub), 65536)
		callID++
		if err != nil || len(resp) < 28 {
			t.Fatalf("opnum %d: resp len %d err %v", opnum, len(resp), err)
		}
		return resp
	}
}

// srvsvcEnum issues NetrShareEnum at level.
func srvsvcEnum(call func(uint16, []byte) []byte, level uint32) []byte {
	stub := binary.LittleEndian.AppendUint32(nil, 0) // null ServerName
	stub = binary.LittleEndian.AppendUint32(stub, level)
	return call(rpc.OpNetrShareEnum, stub)
}

// srvsvcGetInfo502 issues NetrShareGetInfo for share at level 502.
func srvsvcGetInfo502(call func(uint16, []byte) []byte, share string) []byte {
	stub := binary.LittleEndian.AppendUint32(nil, 0) // null ServerName
	stub = append(stub, ndrWideString(share)...)
	stub = binary.LittleEndian.AppendUint32(stub, 502)
	return call(rpc.OpNetrShareGetInfo, stub)
}

// srvsvcStatus reads the trailing status word of a srvsvc response.
func srvsvcStatus(resp []byte) uint32 { return binary.LittleEndian.Uint32(resp[len(resp)-4:]) }

// TestSRVSVC_HidesSharesTheSessionCannotConnectTo drives a srvsvc pipe opened
// over IPC$ by a user holding a grant on /public only. /secret refuses that
// user at TREE_CONNECT, so the share list must not name it at any level, and
// NetrShareGetInfo at level 502 must not hand out its security descriptor.
func TestSRVSVC_HidesSharesTheSessionCannotConnectTo(t *testing.T) {
	bg := context.Background()
	rt, bsID := newTestShareRuntime(t)
	cps := rt.Store()
	if err := rt.RegisterMetadataStore("test-meta", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	metaID, err := cps.CreateMetadataStore(bg, &models.MetadataStoreConfig{Name: "test-meta", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}

	shareIDs := map[string]string{}
	for _, name := range []string{"/public", "/secret"} {
		id, err := cps.CreateShare(bg, &models.Share{
			Name: name, MetadataStoreID: metaID, BlockStoreID: bsID,
			Enabled: true, DefaultPermission: string(models.PermissionNone),
		})
		if err != nil {
			t.Fatalf("CreateShare(%s): %v", name, err)
		}
		shareIDs[name] = id
		if err := rt.AddShare(bg, &runtime.ShareConfig{
			Name: name, MetadataStore: "test-meta", BlockStoreID: bsID, Enabled: true,
			DefaultPermission: string(models.PermissionNone),
			RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
		}); err != nil {
			t.Fatalf("AddShare(%s): %v", name, err)
		}
	}

	uid := uint32(1001)
	aliceID, err := cps.CreateUser(bg, &models.User{Username: "alice", UID: &uid, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := cps.SetUserSharePermission(bg, &models.UserSharePermission{
		UserID: aliceID, ShareID: shareIDs["/public"], ShareName: "/public",
		Permission: string(models.PermissionRead),
	}); err != nil {
		t.Fatalf("SetUserSharePermission: %v", err)
	}
	alice, err := cps.GetUser(bg, "alice")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}

	h := NewHandler()
	h.Registry = rt
	sess := h.CreateSessionWithUser(42, "127.0.0.1:12345", alice, "")

	// The grant is what TREE_CONNECT says it is: /public admits alice, /secret
	// refuses her. The share list must agree with exactly this.
	for share, want := range map[string]types.Status{
		"public": types.StatusSuccess,
		"secret": types.StatusAccessDenied,
	} {
		res, err := h.TreeConnect(newTreeConnectTestContext(sess.SessionID), buildTreeConnectRequestBody(`\\server\`+share))
		if err != nil || res.Status != want {
			t.Fatalf("TREE_CONNECT %s = (%v, %v), want %v", share, res.Status, err, want)
		}
	}

	call := openSRVSVCPipe(t, h, sess.SessionID)
	public, secret := encodeUTF16LE("public"), encodeUTF16LE("secret")

	for _, level := range []uint32{1, 502} {
		resp := srvsvcEnum(call, level)
		if !bytes.Contains(resp, public) {
			t.Errorf("NetrShareEnum level %d omits /public, which alice can connect to", level)
		}
		if bytes.Contains(resp, secret) {
			t.Errorf("NetrShareEnum level %d names /secret to a user TREE_CONNECT refuses", level)
		}
	}

	getInfo := func(share string) []byte { return srvsvcGetInfo502(call, share) }
	status := srvsvcStatus
	if resp := getInfo("secret"); status(resp) != rpc.NERR_NetNameNotFound {
		t.Errorf("NetrShareGetInfo(secret, 502) status = 0x%x, want NERR_NetNameNotFound; a refused session got the share's security descriptor", status(resp))
	}
	if resp := getInfo("public"); status(resp) != rpc.NERR_Success {
		t.Errorf("NetrShareGetInfo(public, 502) status = 0x%x, want NERR_Success", status(resp))
	}
}

// TestSRVSVC_ReauthDuringResolveHidesShare pins the generation gate on the
// share list. The admission answer is resolved against one identity across
// store round trips; a re-authentication landing inside that window must not
// let the srvsvc pipe show a share, or hand out its level-502 descriptor, on
// the previous principal's grants. The re-authentication is injected from
// inside the record lookup, which is where the window actually is.
func TestSRVSVC_ReauthDuringResolveHidesShare(t *testing.T) {
	sessionRecord := userWithGrant(models.PermissionAdmin, 1000, 1000)
	store := &revalidateUserStore{resolveFromRecord: true, user: sessionRecord}

	h, sessionID, _ := newRevalidateHandler(t, sessionRecord, store, models.PermissionReadWrite, false)
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("fixture is wrong: no session")
	}
	call := openSRVSVCPipe(t, h, sessionID)
	export := encodeUTF16LE("export")

	// Without the interleaving the share is listed, so the assertions below
	// are about the re-authentication rather than a list that was always empty.
	if !bytes.Contains(srvsvcEnum(call, 1), export) {
		t.Fatal("fixture is wrong: /export is not listed before any re-authentication")
	}

	bobUID := uint32(4242)
	bob := &models.User{ID: "user-2", Username: "bob", UID: &bobUID, Enabled: true}
	store.onGetUser = func() {
		sess.UpdateIdentity(bob.Username, "", bob, false, false, nil, "")
	}

	for _, level := range []uint32{1, 502} {
		if bytes.Contains(srvsvcEnum(call, level), export) {
			t.Errorf("NetrShareEnum level %d listed /export on an answer about alice to a "+
				"session that re-authenticated as bob mid-resolution", level)
		}
	}
	if st := srvsvcStatus(srvsvcGetInfo502(call, "export")); st != rpc.NERR_NetNameNotFound {
		t.Errorf("NetrShareGetInfo(export, 502) status = 0x%x, want NERR_NetNameNotFound: the "+
			"descriptor was returned on an answer about alice after re-authentication as bob", st)
	}
}
