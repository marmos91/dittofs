package nfs

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// cancelOnceStore cancels the request context from inside the first GetFile it
// serves after being armed, then fails the way a real store fails once its
// context is gone. It reproduces a request that is cancelled mid-flight rather
// than before it starts, which is the only way past a handler's own entry
// check.
type cancelOnceStore struct {
	*metadatamemory.MemoryMetadataStore

	armed  atomic.Bool
	fired  atomic.Bool
	cancel context.CancelFunc
}

func (s *cancelOnceStore) GetFile(ctx context.Context, h metadata.FileHandle) (*metadata.File, error) {
	if s.armed.CompareAndSwap(true, false) {
		s.fired.Store(true)
		s.cancel()
		return nil, context.Canceled
	}
	return s.MemoryMetadataStore.GetFile(ctx, h)
}

// removeCall builds the RPC call and XDR payload for REMOVE(dir, name).
// REMOVE3args is a diropargs3: an nfs_fh3 followed by a filename3, each an XDR
// opaque of a 4-byte length plus data padded to a 4-byte boundary.
func removeCall(xid uint32, dir metadata.FileHandle, name string) (*rpc.RPCCallMessage, []byte) {
	authBody := make([]byte, 24)
	binary.BigEndian.PutUint32(authBody[8:12], 0) // uid 0
	binary.BigEndian.PutUint32(authBody[12:16], 0)

	call := &rpc.RPCCallMessage{
		XID:       xid,
		Program:   rpc.ProgramNFS,
		Version:   rpc.NFSVersion3,
		Procedure: nfs_types.NFSProcRemove,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthUnix, Body: authBody},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}

	opaque := func(b []byte) []byte {
		padded := (len(b) + 3) / 4 * 4
		out := make([]byte, 4+padded)
		binary.BigEndian.PutUint32(out[0:4], uint32(len(b)))
		copy(out[4:], b)
		return out
	}

	return call, append(opaque(dir), opaque([]byte(name))...)
}

// TestV3DRC_CancelledRequestDoesNotPoisonItsRetransmit pins that a v3 request
// cancelled mid-flight leaves nothing behind that answers the client's retry.
//
// The two mechanisms involved are each correct alone and wrong together. When a
// handler returns a Go error, the dispatcher does not send that handler's
// response at all: a context error makes it write no reply whatsoever
// (dispatch.go, "Handler cancelled"), which is deliberate — a cancelled request
// usually means the peer is gone. Separately, the duplicate-request cache
// records the reply of every cacheable (non-idempotent) procedure so that a
// client's retransmit replays it instead of executing the mutation twice.
//
// The seam is that the reply the DRC records is one the client never received,
// and for a cancelled request it is not even a real reply — it is the fallback
// error the encoding layer fabricates for a response that was discarded. So the
// client, having been sent nothing, does exactly what it is supposed to do and
// retransmits, and the cache answers the retry with a fabricated permanent
// error. The mutation never happens and the client is told it failed.
//
// This asserts the observable: after the retransmit, the file is actually gone.
func TestV3DRC_CancelledRequestDoesNotPoisonItsRetransmit(t *testing.T) {
	const clientAddr = "10.0.0.7:2049"

	rt, bsID := newTestShareRuntime(t)
	reqCtx, cancel := context.WithCancel(context.Background())
	store := &cancelOnceStore{
		MemoryMetadataStore: metadatamemory.NewMemoryMetadataStoreWithDefaults(),
		cancel:              cancel,
	}
	if err := rt.RegisterMetadataStore("test-meta", store); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              "/export",
		MetadataStore:     "test-meta",
		BlockStoreID:      bsID,
		Enabled:           true,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle("/export")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	adapter.Registry = rt
	adapter.nfsHandler.Registry = rt
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn := NewNFSConnection(adapter, server, 1)

	metaSvc := rt.GetMetadataService()
	rootUID, rootGID := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:    context.Background(),
		ClientAddr: clientAddr,
		AuthMethod: "unix",
		Identity:   &metadata.Identity{UID: &rootUID, GID: &rootGID, GIDs: []uint32{rootGID}},
	}
	if _, _, err := metaSvc.CreateFile(authCtx, rootHandle, "victim.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o644,
	}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	call, data := removeCall(0xDEAD, rootHandle, "victim.txt")

	// First attempt: cancelled while the store call is in flight.
	store.armed.Store(true)
	if _, err := conn.handleNFSProcedure(reqCtx, call, data, clientAddr); err == nil {
		t.Fatal("first REMOVE returned no error; the cancellation path was not exercised")
	}
	if !store.fired.Load() {
		t.Fatal("the store call never ran, so the request was not cancelled mid-flight")
	}
	if _, err := metaSvc.GetChild(context.Background(), rootHandle, "victim.txt"); err != nil {
		t.Fatalf("the cancelled REMOVE deleted the file anyway: %v", err)
	}

	// The client was sent nothing, so it retransmits the same XID and body.
	reply, err := conn.handleNFSProcedure(context.Background(), call, data, clientAddr)
	if err != nil {
		t.Fatalf("retransmitted REMOVE: %v", err)
	}
	if status := binary.BigEndian.Uint32(reply[0:4]); status != nfs_types.NFS3OK {
		t.Errorf("retransmitted REMOVE status = %d, want NFS3OK (%d): the cache answered the retry "+
			"with the fabricated reply of the cancelled attempt", status, nfs_types.NFS3OK)
	}
	if _, err := metaSvc.GetChild(context.Background(), rootHandle, "victim.txt"); err == nil {
		t.Error("the file still exists after the retransmitted REMOVE: the mutation never ran, " +
			"and the client was told it failed")
	}
}
