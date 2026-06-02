package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// fakeNLMCallbackListener is a minimal TCP server that stands in for an NLM
// client's callback port. ProcessGrantedCallback dials this address, writes a
// record-marked NLM_GRANTED RPC call, and blocks reading the reply; the
// listener accepts the connection, drains the call fragment, and writes back a
// single record-marked reply so SendGrantedCallback returns success. It signals
// granted on the channel exactly once so the test can await the grant
// deterministically (no polling, no real sleep on the grant path).
type fakeNLMCallbackListener struct {
	ln      net.Listener
	granted chan struct{}
	once    sync.Once
}

func newFakeNLMCallbackListener(t *testing.T) *fakeNLMCallbackListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	f := &fakeNLMCallbackListener{ln: ln, granted: make(chan struct{})}
	go f.serve()
	t.Cleanup(func() { _ = f.ln.Close() })
	return f
}

func (f *fakeNLMCallbackListener) addr() string { return f.ln.Addr().String() }

func (f *fakeNLMCallbackListener) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go f.handle(conn)
	}
}

func (f *fakeNLMCallbackListener) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// Read the record-marking header + drain the call fragment so the caller's
	// Write completes.
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return
	}
	fragLen := binary.BigEndian.Uint32(header[:]) & 0x7FFFFFFF
	if fragLen > 1*1024*1024 {
		return // corrupt/oversized header — fail fast rather than block on a bad read
	}
	if fragLen > 0 {
		if _, err := io.CopyN(io.Discard, conn, int64(fragLen)); err != nil {
			return
		}
	}

	// The waiter is granted the moment the server delivers the NLM_GRANTED
	// callback. Signal before replying so the test observes the grant.
	f.once.Do(func() { close(f.granted) })

	// Write a minimal record-marked reply body so readAndDiscardReply unblocks
	// and SendGrantedCallback reports success. Content is irrelevant (the sender
	// discards it); a tiny 4-byte body is enough.
	body := []byte{0, 0, 0, 0}
	reply := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(reply[0:4], uint32(len(body))|0x80000000)
	copy(reply[4:], body)
	_, _ = conn.Write(reply)
}

// TestNLMWaiter_GrantedWhenSMBHolderReleases is the cross-protocol liveness
// regression guard for #1035 (area #5 H-1). It exercises the FULL wakeup path
// end-to-end through the wired NFS adapter:
//
//  1. A share is exported (runtime + metadata service + per-share lock manager).
//  2. The adapter registers the byte-range release hook exactly as SetRuntime
//     does (SetByteRangeReleaseHook on the service + SetByteRangeReleaseCallback
//     on already-created managers).
//  3. An SMB-style byte-range lock (OpenID + SessionID set) is held over a range.
//  4. An NLM F_SETLKW waiter for an overlapping range is enqueued in the real
//     blocking queue (this is what nlm/handlers/lock.go does on NLM4_BLOCKED).
//  5. An SMB UNLOCK (MetadataService.UnlockFile) releases the range.
//
// The SMB unlock must fire the release hook → processNLMWaiters → AddUnifiedLock
// (cross-scan grant) → NLM_GRANTED callback. The test asserts the waiter is
// granted within a bounded timeout (the GRANTED callback fires and the waiter
// leaves the queue) WITHOUT any intervening NLM op.
//
// Before the fix the SMB UNLOCK path had no hook into the NLM drain, so the
// waiter would stay queued forever and this test would time out.
func TestNLMWaiter_GrantedWhenSMBHolderReleases(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const shareName = "/xproto-wakeup"

	// --- Runtime + share with a memory metadata store -----------------------
	rt := runtime.New(nil)
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	require.NoError(t, rt.RegisterMetadataStore("test-meta", metaStore))
	require.NoError(t, rt.AddShare(ctx, &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		Enabled:       true,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}))

	metaSvc := rt.GetMetadataService()
	require.NotNil(t, metaSvc)

	// --- Minimal NFS adapter wired like SetRuntime --------------------------
	shutdownCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &NFSAdapter{
		BaseAdapter:   &adapter.BaseAdapter{Registry: rt, ShutdownCtx: shutdownCtx},
		blockingQueue: blocking.NewBlockingQueue(100),
	}

	// Register the cross-protocol blocked-waiter wakeup hook exactly as the real
	// SetRuntime wiring does: on the service for shares added later AND on lock
	// managers already created at boot (this share's manager exists already).
	byteRangeReleaseHook := func(handleKey string) {
		go s.processNLMWaiters(metadata.FileHandle(handleKey))
	}
	metaSvc.SetByteRangeReleaseHook(byteRangeReleaseHook)
	lm := metaSvc.GetLockManagerForShare(shareName)
	require.NotNil(t, lm)
	lm.SetByteRangeReleaseCallback(byteRangeReleaseHook)

	// --- Create a file and encode its handle --------------------------------
	rootHandle, err := rt.GetRootHandle(shareName)
	require.NoError(t, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: metadata.Uint32Ptr(0),
			GID: metadata.Uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	file, _, err := metaSvc.CreateFile(authCtx, rootHandle, "locked.bin", &metadata.FileAttr{Mode: 0o644})
	require.NoError(t, err)
	handle, err := metadata.EncodeFileHandle(file)
	require.NoError(t, err)
	handleKey := string(handle)

	// --- SMB holder takes an exclusive byte-range lock ----------------------
	const smbSessionID = uint64(42)
	const smbOpenID = "smb-open-1"
	require.NoError(t, metaSvc.LockFile(authCtx, handle, metadata.FileLock{
		ID:        1,
		OpenID:    smbOpenID,
		SessionID: smbSessionID,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}))

	// --- NLM client issues a blocking lock that conflicts -------------------
	// This mirrors nlm/handlers/lock.go: on NLM4_BLOCKED the request is parked
	// in the blocking queue. The waiter's CallbackAddr points at our fake NLM
	// callback listener so the GRANTED callback succeeds deterministically.
	cb := newFakeNLMCallbackListener(t)

	const nlmOwnerID = "nlm:clientX:7:aa"
	waiter := &blocking.Waiter{
		Lock: lock.NewUnifiedLock(
			lock.LockOwner{OwnerID: nlmOwnerID, ClientID: "clientX"},
			lock.FileHandle(handle),
			0, 100, lock.LockTypeExclusive,
		),
		Exclusive:    true,
		CallbackAddr: cb.addr(),
		CallbackProg: 100021, // NLM program (value irrelevant to the fake listener)
		CallbackVers: 4,
		CallerName:   "clientX",
		FileHandle:   handle,
	}
	require.NoError(t, s.blockingQueue.Enqueue(handleKey, waiter))

	// Sanity: the NLM lock truly conflicts with the held SMB lock right now —
	// a drain attempt before the SMB release must NOT grant it.
	s.processNLMWaiters(handle)
	require.Equal(t, 1, len(s.blockingQueue.GetWaiters(handleKey)),
		"NLM waiter must stay queued while the SMB holder still owns the range")
	select {
	case <-cb.granted:
		t.Fatal("NLM waiter was granted while the conflicting SMB lock was still held")
	case <-time.After(50 * time.Millisecond):
		// expected: no grant yet
	}

	// --- SMB holder UNLOCKs — this must wake the NLM waiter ------------------
	require.NoError(t, metaSvc.UnlockFile(ctx, handle, smbOpenID, smbSessionID, 0, 100))

	// The release hook fires processNLMWaiters in a goroutine; await the grant
	// signal (GRANTED callback delivered to the fake client) within a bounded
	// timeout. Without the fix the SMB unlock never drives the drain and this
	// blocks until the deadline.
	select {
	case <-cb.granted:
		// granted — the cross-protocol wakeup worked
	case <-time.After(5 * time.Second):
		t.Fatal("NLM waiter was NOT granted after the SMB holder released " +
			"(SMB UNLOCK did not drive processNLMWaiters)")
	}

	// The granted waiter must be removed from the queue and the NLM lock must
	// now be held in the lock manager.
	require.Eventually(t, func() bool {
		return len(s.blockingQueue.GetWaiters(handleKey)) == 0
	}, 5*time.Second, 5*time.Millisecond,
		"granted NLM waiter must be removed from the blocking queue")

	held := lm.ListUnifiedLocks(handleKey)
	require.Len(t, held, 1, "exactly one unified lock (the granted NLM lock) must be held")
	require.Equal(t, nlmOwnerID, held[0].Owner.OwnerID,
		"the held unified lock must be the granted NLM waiter's lock")
}
