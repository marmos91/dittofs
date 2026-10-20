package smb

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// tcpPeerConn reports a TCP peer address, as a real SMB connection does.
type tcpPeerConn struct {
	net.Conn
	peer net.Addr
}

func (c tcpPeerConn) RemoteAddr() net.Addr { return c.peer }

// A tree connected by a Kerberos session to a Kerberos-only share is refused at
// dispatch once the session re-authenticates over NTLM — MS-SMB2 keeps the tree
// across that — while CLOSE still reaches its handler.
func TestPrepareDispatch_ReappliesSharePolicyPerRequest(t *testing.T) {
	rt, bsID := testruntime.NewWithShareStore(t)
	if err := rt.RegisterMetadataStore("test-meta", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              "/secure",
		MetadataStore:     "test-meta",
		BlockStoreID:      bsID,
		Enabled:           true,
		DefaultPermission: "read-write",
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	if err := rt.SetExportAuthPolicyForTesting("/secure", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	mgr := session.NewDefaultManager()
	h := handlers.NewHandlerWithSessionManager(mgr)
	h.Registry = rt
	user := &models.User{Username: "alice", Enabled: true}
	sess := h.CreateSessionWithUserAndExpiry(h.GenerateSessionID(), "10.1.2.3:50000", user, "CORP", time.Now().Add(time.Hour))
	const treeID = uint32(9)
	h.StoreTree(&handlers.TreeConnection{
		TreeID:     treeID,
		SessionID:  sess.SessionID,
		ShareName:  "/secure",
		ShareType:  types.SMB2ShareTypeDisk,
		Permission: models.PermissionReadWrite,
	})

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	connInfo := &ConnInfo{
		Conn:           tcpPeerConn{Conn: server, peer: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 50000}},
		Handler:        h,
		SessionManager: mgr,
	}
	dispatch := func(cmd types.Command) types.Status {
		_, _, st := prepareDispatch(context.Background(), &header.SMB2Header{
			StructureSize: header.HeaderSize,
			Command:       cmd,
			SessionID:     sess.SessionID,
			TreeID:        treeID,
		}, connInfo)
		return st
	}

	if st := dispatch(types.SMB2Write); st != 0 {
		t.Fatalf("WRITE on a Kerberos session's tree: status = %s, want dispatched", st)
	}

	sess.UpdateIdentity("alice", "CORP", user, false, false, nil, "")

	if st := dispatch(types.SMB2Write); st != types.StatusAccessDenied {
		t.Fatalf("WRITE after NTLM re-authentication on a Kerberos-only share: status = %s, want STATUS_ACCESS_DENIED", st)
	}
	if st := dispatch(types.SMB2Close); st != 0 {
		t.Fatalf("CLOSE after NTLM re-authentication: status = %s, want dispatched so the handle can be released", st)
	}
}
