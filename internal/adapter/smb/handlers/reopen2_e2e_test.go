package handlers

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// End-to-end coverage for the smbtorture reopen2 family
// (smb2.durable-open.reopen2{,-lease,-lease-v2} and the V2 equivalents,
// #738/#739). These tests drive the REAL CREATE handler through a full
// persist → disconnect → reconnect cycle:
//
//  1. Initial open via completeCreateAfterBreak (the real lease-grant + durable-
//     grant call site) — registers a live OpenFile with a real LeaseManager
//     lease and a real durable grant.
//  2. Real disconnect (CloseAllFilesForSession+releaseSessionLeasesAndNotifies,
//     the persist + lease-release steps of CleanupSession) — persists the
//     durable handle (capturing the lease state via GetLeaseState) and releases
//     the session's leases from the LeaseManager.
//  3. Reconnect via the real Create() entrypoint with a DHnC/DH2C reconnect
//     context (+ RqLs) — the path that must restore the lease state (RWH),
//     report OplockLevel=Lease in the response, echo the lease key, EXISTED,
//     size==0, and NOT emit a DHnQ/DH2Q grant blob.
//
// The previous-agent lesson (isolated decode/Create repros returning the wrong
// value) is avoided: these assert the actual *CreateResponse the wire encoder
// serializes, after the lease re-request through the LeaseManager, not the
// intermediate ReconnectResult.

// reopen2Env bundles the wired handler + helpers for a reopen2 cycle.
type reopen2Env struct {
	h          *Handler
	tree       *TreeConnection
	clientGUID [16]byte
}

// setupReopen2Env stands up a Handler with an in-memory metadata store, a real
// LeaseManager, and a durable store, then creates the test file ("durable.txt")
// in the share root. The returned env is ready for an initial lease+durable
// open followed by a disconnect/reconnect cycle.
func setupReopen2Env(t *testing.T) *reopen2Env {
	t.Helper()

	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	// Real LeaseManager backed by a single lock.Manager (the SMB lease tier is
	// independent of the metadata store's own lock manager).
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: lock.NewManager()}, nil)
	h.DurableStore = newMockDurableStore()
	h.DurableTimeoutMs = 60000

	metaSvc := rt.GetMetadataService()

	// Create the file that the durable handle will reference.
	_, err := metaSvc.CreateFile(rootAuth, rootHandle, "durable.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	return &reopen2Env{
		h:          h,
		tree:       tree,
		clientGUID: [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
	}
}

// makeSMBCtx builds a SMBHandlerContext for the given session carrying the
// connection ClientGUID via a mock crypto state (so connClientGUID(ctx) is
// non-zero, matching the live dispatch path).
func (e *reopen2Env) makeSMBCtx(sessionID uint64) *SMBHandlerContext {
	cs := &mockCryptoState{}
	cs.SetClientGUID(e.clientGUID)
	return &SMBHandlerContext{
		Context:         context.Background(),
		SessionID:       sessionID,
		TreeID:          e.tree.TreeID,
		ShareName:       e.tree.ShareName,
		ConnCryptoState: cs,
	}
}

// initialLeaseDurableOpen performs the first open of durable.txt with a lease
// (RWH) and a durable-handle request, driving the REAL lease + durable grant
// through completeCreateAfterBreak. Returns the granted CreateResponse and the
// registered OpenFile.
func (e *reopen2Env) initialLeaseDurableOpen(
	t *testing.T,
	sessionID uint64,
	leaseKey [16]byte,
	durableCtx []CreateContext,
) (*CreateResponse, *OpenFile) {
	t.Helper()

	metaSvc := e.h.Registry.GetMetadataService()
	rootHandle, err := e.h.Registry.GetRootHandle(e.tree.ShareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	file, _, err := e.h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "durable.txt")
	if err != nil || file == nil {
		t.Fatalf("lookup durable.txt: file=%v err=%v", file, err)
	}
	existingHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	// RWH lease + durable contexts.
	contexts := []CreateContext{
		{Name: LeaseContextTagRequest, Data: encodeV2LeaseContext(leaseKey,
			lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0)},
	}
	contexts = append(contexts, durableCtx...)

	const fullAccess uint32 = 0x001F01FF
	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     fullAccess,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			CreateOptions:     0,
			OplockLevel:       OplockLevelLease,
			CreateContexts:    contexts,
		},
		tree:           e.tree,
		authCtx:        rootAuthCtx(),
		filename:       "durable.txt",
		baseName:       "durable.txt",
		parentHandle:   rootHandle,
		existingFile:   file,
		existingHandle: existingHandle,
		fileExists:     true,
		createAction:   types.FileOpened,
	}

	smbCtx := e.makeSMBCtx(sessionID)
	// Register a session so disconnect/persist can read username/sessionKeyHash.
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")

	resp := e.h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("initial open: status=0x%08x, want SUCCESS", uint32(resp.Status))
	}
	openFile, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("initial open: OpenFile not registered")
	}
	return resp, openFile
}

// disconnect models a transport drop for the session: it runs the real persist
// half (CloseAllFilesForSession with isDisconnect=true, which captures the lease
// state via GetLeaseState and writes the durable handle) and the real lease
// release (releaseSessionLeasesAndNotifies, which clears the LeaseManager). These
// are exactly steps 1 and 2 of Handler.CleanupSession; calling CleanupSession
// directly would trip its cleanupWg.Done without a matching Add (that bookkeeping
// is owned by the dispatch-layer scheduler, not the cleanup work itself).
func (e *reopen2Env) disconnect(sessionID uint64) {
	ctx := context.Background()
	e.h.CloseAllFilesForSession(ctx, sessionID, true)
	e.h.releaseSessionLeasesAndNotifies(ctx, sessionID)
}

// rootAuthCtx returns a root AuthContext for metadata operations in the repro.
func rootAuthCtx() *metadata.AuthContext {
	uid := uint32(0)
	gid := uint32(0)
	return &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
}

// findCtx returns the create context with the given name from a slice, or nil.
func findRespCtx(resp *CreateResponse, name string) *CreateContext {
	for i := range resp.CreateContexts {
		if resp.CreateContexts[i].Name == name {
			return &resp.CreateContexts[i]
		}
	}
	return nil
}

// assertLeaseReconnect runs the shared assertions for a successful lease-backed
// reconnect: OplockLevel=Lease, EXISTED (FileOpened), size==0, lease state RWH,
// lease key echoed, and NO durable grant blob (DHnQ/DH2Q) in the response.
func assertLeaseReconnect(t *testing.T, resp *CreateResponse, leaseKey [16]byte, wantV2Lease bool) {
	t.Helper()
	if resp.Status != types.StatusSuccess {
		t.Fatalf("reconnect: status=0x%08x, want SUCCESS", uint32(resp.Status))
	}
	if resp.OplockLevel != OplockLevelLease {
		t.Errorf("reconnect OplockLevel = 0x%02x, want Lease (0x%02x)", resp.OplockLevel, OplockLevelLease)
	}
	if resp.CreateAction != types.FileOpened {
		t.Errorf("reconnect CreateAction = %d, want FileOpened (EXISTED)", resp.CreateAction)
	}
	if resp.EndOfFile != 0 {
		t.Errorf("reconnect EndOfFile = %d, want 0 (no data written)", resp.EndOfFile)
	}
	// No durable grant blob on reconnect (MS-SMB2 §3.3.5.9.7/12).
	if findRespCtx(resp, DurableHandleV1RequestTag) != nil {
		t.Error("reconnect response unexpectedly carries DHnQ grant blob")
	}
	if findRespCtx(resp, DurableHandleV2RequestTag) != nil {
		t.Error("reconnect response unexpectedly carries DH2Q grant blob")
	}
	// Lease response: key echoed + state RWH. The wire layout (shared by the V1
	// and V2 response shapes) is LeaseKey[0:16] + LeaseState[16:20].
	lr := findRespCtx(resp, LeaseContextTagResponse)
	if lr == nil {
		t.Fatal("reconnect response missing lease response context")
	}
	if len(lr.Data) < 20 {
		t.Fatalf("reconnect lease response too short: %d bytes", len(lr.Data))
	}
	var gotKey [16]byte
	copy(gotKey[:], lr.Data[0:16])
	if gotKey != leaseKey {
		t.Errorf("reconnect lease key = %x, want %x", gotKey, leaseKey)
	}
	gotState := binary.LittleEndian.Uint32(lr.Data[16:20])
	wantState := uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)
	if gotState != wantState {
		t.Errorf("reconnect lease state = 0x%x (%s), want RWH 0x%x",
			gotState, lock.LeaseStateToString(gotState), wantState)
	}
	// V1 vs V2 lease response shape mirrors the request context size.
	gotV2 := len(lr.Data) >= LeaseV2ContextSize
	if gotV2 != wantV2Lease {
		t.Errorf("reconnect lease response V2=%v, wantV2Lease=%v (mismatch, len=%d)", gotV2, wantV2Lease, len(lr.Data))
	}
}

// dh2qContext builds a DH2Q (V2 durable request) create context.
func dh2qContext(createGuid [16]byte, timeoutMs uint32) CreateContext {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[0:4], timeoutMs)
	copy(data[16:32], createGuid[:])
	return CreateContext{Name: DurableHandleV2RequestTag, Data: data}
}

// dhnqContext builds a DHnQ (V1 durable request) create context.
func dhnqContext() CreateContext {
	return CreateContext{Name: DurableHandleV1RequestTag, Data: make([]byte, 16)}
}

// dh2cContext builds a DH2C (V2 reconnect) create context.
func dh2cContext(fileID, createGuid [16]byte) CreateContext {
	data := make([]byte, 36)
	copy(data[0:16], fileID[:])
	copy(data[16:32], createGuid[:])
	return CreateContext{Name: DurableHandleV2ReconnectTag, Data: data}
}

// dhncContext builds a DHnC (V1 reconnect) create context.
func dhncContext(fileID [16]byte) CreateContext {
	data := make([]byte, 16)
	copy(data, fileID[:])
	return CreateContext{Name: DurableHandleV1ReconnectTag, Data: data}
}

// TestReopen2_V1Oplock_RestoresBatchOplock covers smb2.durable-open.reopen2:
// an oplock-backed (no-lease) durable handle reconnected via DHnC must restore
// OplockLevel=Batch in the CREATE response, with EXISTED + size==0.
func TestReopen2_V1Oplock_RestoresBatchOplock(t *testing.T) {
	e := setupReopen2Env(t)

	// Initial open with a BATCH oplock (no lease) + V1 durable request.
	metaSvc := e.h.Registry.GetMetadataService()
	rootHandle, _ := e.h.Registry.GetRootHandle(e.tree.ShareName)
	file, _, _ := e.h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "durable.txt")
	existingHandle, _ := metadata.EncodeFileHandle(file)

	const sessionID = uint64(1)
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")
	smbCtx := e.makeSMBCtx(sessionID)

	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     0x001F01FF,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelBatch,
			CreateContexts:    []CreateContext{dhnqContext()},
		},
		tree:           e.tree,
		authCtx:        rootAuthCtx(),
		filename:       "durable.txt",
		baseName:       "durable.txt",
		parentHandle:   rootHandle,
		existingFile:   file,
		existingHandle: existingHandle,
		fileExists:     true,
		createAction:   types.FileOpened,
	}

	resp := e.h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("initial open: status=0x%08x", uint32(resp.Status))
	}
	of, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("OpenFile not registered")
	}
	if !of.IsDurable {
		t.Fatal("initial open: durability not granted (V1 batch oplock)")
	}
	if of.OplockLevel != OplockLevelBatch {
		t.Fatalf("initial open: OplockLevel = 0x%02x, want Batch", of.OplockLevel)
	}
	persistedFileID := of.FileID

	// Disconnect: persist durable handle + release leases.
	e.disconnect(sessionID)

	// Reconnect on a NEW session via the real Create() entrypoint.
	const sessionID2 = uint64(2)
	e.h.CreateSessionWithID(sessionID2, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(sessionID2)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateContexts:    []CreateContext{dhncContext(persistedFileID)},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	if rcResp.Status != types.StatusSuccess {
		t.Fatalf("reconnect: status=0x%08x, want SUCCESS", uint32(rcResp.Status))
	}
	if rcResp.OplockLevel != OplockLevelBatch {
		t.Errorf("reconnect OplockLevel = 0x%02x, want Batch (0x%02x)", rcResp.OplockLevel, OplockLevelBatch)
	}
	if rcResp.CreateAction != types.FileOpened {
		t.Errorf("reconnect CreateAction = %d, want FileOpened (EXISTED)", rcResp.CreateAction)
	}
	if rcResp.EndOfFile != 0 {
		t.Errorf("reconnect EndOfFile = %d, want 0", rcResp.EndOfFile)
	}
	if findRespCtx(rcResp, DurableHandleV1RequestTag) != nil {
		t.Error("reconnect response unexpectedly carries DHnQ grant blob")
	}
}

// TestReopen2_V2Lease_RestoresRWH covers smb2.durable-v2-open.reopen2-lease:
// a V2 durable + lease handle reconnected via DH2C (+ RqLs) must restore the
// lease at RWH and report OplockLevel=Lease.
func TestReopen2_V2Lease_RestoresRWH(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	createGuid := [16]byte{0xC0, 0xC1, 0xC2, 0xC3, 0xC4, 0xC5, 0xC6, 0xC7, 0xC8, 0xC9, 0xCA, 0xCB, 0xCC, 0xCD, 0xCE, 0xCF}

	const sessionID = uint64(10)
	resp, of := e.initialLeaseDurableOpen(t, sessionID, leaseKey, []CreateContext{dh2qContext(createGuid, 60000)})

	// Initial open must have granted a lease (RWH) + V2 durability.
	if of.OplockLevel != OplockLevelLease {
		t.Fatalf("initial open: OplockLevel = 0x%02x, want Lease", of.OplockLevel)
	}
	if !of.IsDurable || of.CreateGuid != createGuid {
		t.Fatalf("initial open: V2 durability not granted (durable=%v guid=%x)", of.IsDurable, of.CreateGuid)
	}
	if findRespCtx(resp, DurableHandleV2RequestTag) == nil {
		t.Fatal("initial open: missing DH2Q grant blob")
	}
	// Confirm the lease really sits at RWH in the LeaseManager before disconnect.
	if state, _, found := e.h.LeaseManager.GetLeaseState(context.Background(), leaseKey); !found ||
		state != uint32(lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle) {
		t.Fatalf("initial open: LeaseManager state = 0x%x found=%v, want RWH", state, found)
	}
	persistedFileID := of.FileID

	// Disconnect: persist + release leases.
	e.disconnect(sessionID)

	// Reconnect via DH2C + RqLs(RWH) on a new session.
	const sessionID2 = uint64(11)
	e.h.CreateSessionWithID(sessionID2, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(sessionID2)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelLease,
		CreateContexts: []CreateContext{
			dh2cContext(persistedFileID, createGuid),
			{Name: LeaseContextTagRequest, Data: encodeV2LeaseContext(leaseKey,
				lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0)},
		},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	assertLeaseReconnect(t, rcResp, leaseKey, true /* V2 lease */)
}

// TestReopen2_V1Lease_RestoresRWH covers smb2.durable-open.reopen2-lease: a V1
// durable + lease handle reconnected via DHnC (+ RqLs) restores RWH.
func TestReopen2_V1Lease_RestoresRWH(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0x1A, 0x2B, 0x3C, 0x4D, 0x5E, 0x6F}

	const sessionID = uint64(20)
	_, of := e.initialLeaseDurableOpen(t, sessionID, leaseKey, []CreateContext{dhnqContext()})

	if of.OplockLevel != OplockLevelLease {
		t.Fatalf("initial open: OplockLevel = 0x%02x, want Lease", of.OplockLevel)
	}
	if !of.IsDurable {
		t.Fatal("initial open: V1 durability not granted (lease with Handle)")
	}
	persistedFileID := of.FileID

	e.disconnect(sessionID)

	const sessionID2 = uint64(21)
	e.h.CreateSessionWithID(sessionID2, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(sessionID2)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelLease,
		CreateContexts: []CreateContext{
			dhncContext(persistedFileID),
			{Name: LeaseContextTagRequest, Data: encodeV2LeaseContext(leaseKey,
				lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0)},
		},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	assertLeaseReconnect(t, rcResp, leaseKey, true /* SMB3 lease ctx is V2 shape */)
}

// TestReopen2_V2Lease_MultiCycle covers the multi-disconnect/reconnect cycle in
// smb2.durable-v2-open.reopen2-lease: after the first reconnect the handle must
// still be reconnectable via the same CreateGuid (the V2 identity + lease key +
// ClientGUID must survive each cycle). RWH must be restored each time.
func TestReopen2_V2Lease_MultiCycle(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0x7A, 0x8B, 0x9C}
	createGuid := [16]byte{0xD0, 0xD1, 0xD2, 0xD3, 0xD4, 0xD5, 0xD6, 0xD7, 0xD8, 0xD9, 0xDA, 0xDB, 0xDC, 0xDD, 0xDE, 0xDF}

	sessionID := uint64(30)
	_, of := e.initialLeaseDurableOpen(t, sessionID, leaseKey, []CreateContext{dh2qContext(createGuid, 60000)})
	persistedFileID := of.FileID

	for cycle := 0; cycle < 3; cycle++ {
		e.disconnect(sessionID)

		sessionID++
		e.h.CreateSessionWithID(sessionID, "127.0.0.1:9", false, "alice", "WORKGROUP")
		rcCtx := e.makeSMBCtx(sessionID)
		rcResp, err := e.h.Create(rcCtx, &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     0x001F01FF,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelLease,
			CreateContexts: []CreateContext{
				dh2cContext(persistedFileID, createGuid),
				{Name: LeaseContextTagRequest, Data: encodeV2LeaseContext(leaseKey,
					lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0)},
			},
		})
		if err != nil {
			t.Fatalf("cycle %d: reconnect Create error: %v", cycle, err)
		}
		assertLeaseReconnect(t, rcResp, leaseKey, true)

		// The reconnected OpenFile must carry the CreateGuid forward so the next
		// disconnect re-persists it as a V2 handle (otherwise GetDurableHandleByCreateGuid
		// fails on the following cycle).
		rof, ok := e.h.GetOpenFile(rcResp.FileID)
		if !ok {
			t.Fatalf("cycle %d: reconnected OpenFile not registered", cycle)
		}
		if rof.CreateGuid != createGuid {
			t.Fatalf("cycle %d: reconnected CreateGuid = %x, want %x", cycle, rof.CreateGuid, createGuid)
		}
		persistedFileID = rof.FileID
	}
}

// TestReopen2_Lease_NegativeLadder covers the negative reconnect ladder from
// smb2.durable-open.reopen2-lease (and the V2 equivalent), asserting the exact
// NTSTATUS for each failure case the test walks before the successful reopen.
func TestReopen2_Lease_NegativeLadder(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0x42, 0x43, 0x44}
	wrongLeaseKey := [16]byte{0x99, 0x98, 0x97}
	createGuid := [16]byte{0xE0, 0xE1, 0xE2, 0xE3, 0xE4, 0xE5, 0xE6, 0xE7, 0xE8, 0xE9, 0xEA, 0xEB, 0xEC, 0xED, 0xEE, 0xEF}

	const sessionID = uint64(40)
	_, of := e.initialLeaseDurableOpen(t, sessionID, leaseKey, []CreateContext{dh2qContext(createGuid, 60000)})
	persistedFileID := of.FileID

	e.disconnect(sessionID)

	rwh := encodeV2LeaseContext(leaseKey, lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0)

	cases := []struct {
		name       string
		fileName   string
		leaseKeyHd [16]byte
		omitLease  bool
		want       types.Status
	}{
		// empty fname, no lease ctx → ONF (persisted handle has a lease).
		{"empty-fname-no-lease", "", leaseKey, true, types.StatusObjectNameNotFound},
		// non-existing fname, no lease ctx → ONF.
		{"nonexisting-no-lease", "__non_existing__", leaseKey, true, types.StatusObjectNameNotFound},
		// correct fname, no lease ctx → ONF (persisted lease, request omits it).
		{"correct-fname-no-lease", "durable.txt", leaseKey, true, types.StatusObjectNameNotFound},
		// wrong lease key (with lease ctx) → ONF.
		{"wrong-lease-key", "durable.txt", wrongLeaseKey, false, types.StatusObjectNameNotFound},
		// wrong fname WITH lease → INVALID_PARAMETER (not object-name).
		{"wrong-fname-with-lease", "__non_existing__", leaseKey, false, types.StatusInvalidParameter},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := sessionID + 100 + uint64(len(tc.name))
			e.h.CreateSessionWithID(sid, "127.0.0.1:7", false, "alice", "WORKGROUP")
			rcCtx := e.makeSMBCtx(sid)
			contexts := []CreateContext{dh2cContext(persistedFileID, createGuid)}
			if !tc.omitLease {
				contexts = append(contexts, CreateContext{
					Name: LeaseContextTagRequest,
					Data: encodeV2LeaseContext(tc.leaseKeyHd,
						lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0),
				})
			}
			rcResp, err := e.h.Create(rcCtx, &CreateRequest{
				FileName:          tc.fileName,
				DesiredAccess:     0x001F01FF,
				ShareAccess:       0x07,
				CreateDisposition: types.FileOpen,
				OplockLevel:       OplockLevelLease,
				CreateContexts:    contexts,
			})
			if err != nil {
				t.Fatalf("%s: Create error: %v", tc.name, err)
			}
			if rcResp.Status != tc.want {
				t.Errorf("%s: status=0x%08x, want 0x%08x", tc.name, uint32(rcResp.Status), uint32(tc.want))
			}
		})
	}

	// After all the negative attempts (which are NON-destructive), the handle
	// must still be reconnectable for real with the correct fname + lease key.
	const finalSID = uint64(999)
	e.h.CreateSessionWithID(finalSID, "127.0.0.1:8", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(finalSID)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelLease,
		CreateContexts:    []CreateContext{dh2cContext(persistedFileID, createGuid), {Name: LeaseContextTagRequest, Data: rwh}},
	})
	if err != nil {
		t.Fatalf("final reopen: Create error: %v", err)
	}
	assertLeaseReconnect(t, rcResp, leaseKey, true)
}

// TestReopen2_V1Lease_NegativeLadder covers the V1 (DHnC) variant of the
// reopen2-lease negative ladder (smb2.durable-open.reopen2-lease). The status
// expectations are identical to the V2 ladder; the only difference is the
// reconnect context (DHnC instead of DH2C) and that the lookup keys on FileID.
func TestReopen2_V1Lease_NegativeLadder(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0x51, 0x52, 0x53}
	wrongLeaseKey := [16]byte{0x61, 0x62, 0x63}

	const sessionID = uint64(60)
	_, of := e.initialLeaseDurableOpen(t, sessionID, leaseKey, []CreateContext{dhnqContext()})
	persistedFileID := of.FileID

	e.disconnect(sessionID)

	rwh := encodeV2LeaseContext(leaseKey, lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0)

	cases := []struct {
		name       string
		fileName   string
		leaseKeyHd [16]byte
		omitLease  bool
		want       types.Status
	}{
		{"empty-fname-no-lease", "", leaseKey, true, types.StatusObjectNameNotFound},
		{"nonexisting-no-lease", "__non_existing__", leaseKey, true, types.StatusObjectNameNotFound},
		{"correct-fname-no-lease", "durable.txt", leaseKey, true, types.StatusObjectNameNotFound},
		{"wrong-lease-key", "durable.txt", wrongLeaseKey, false, types.StatusObjectNameNotFound},
		{"wrong-fname-with-lease", "__non_existing__", leaseKey, false, types.StatusInvalidParameter},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := sessionID + 100 + uint64(len(tc.name))
			e.h.CreateSessionWithID(sid, "127.0.0.1:7", false, "alice", "WORKGROUP")
			rcCtx := e.makeSMBCtx(sid)
			contexts := []CreateContext{dhncContext(persistedFileID)}
			if !tc.omitLease {
				contexts = append(contexts, CreateContext{
					Name: LeaseContextTagRequest,
					Data: encodeV2LeaseContext(tc.leaseKeyHd,
						lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0),
				})
			}
			rcResp, err := e.h.Create(rcCtx, &CreateRequest{
				FileName:          tc.fileName,
				DesiredAccess:     0x001F01FF,
				ShareAccess:       0x07,
				CreateDisposition: types.FileOpen,
				OplockLevel:       OplockLevelLease,
				CreateContexts:    contexts,
			})
			if err != nil {
				t.Fatalf("%s: Create error: %v", tc.name, err)
			}
			if rcResp.Status != tc.want {
				t.Errorf("%s: status=0x%08x, want 0x%08x", tc.name, uint32(rcResp.Status), uint32(tc.want))
			}
		})
	}

	// Handle survives the non-destructive negative attempts → final real reopen.
	const finalSID = uint64(998)
	e.h.CreateSessionWithID(finalSID, "127.0.0.1:8", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(finalSID)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelLease,
		CreateContexts:    []CreateContext{dhncContext(persistedFileID), {Name: LeaseContextTagRequest, Data: rwh}},
	})
	if err != nil {
		t.Fatalf("final reopen: Create error: %v", err)
	}
	assertLeaseReconnect(t, rcResp, leaseKey, true)
}

// ----------------------------------------------------------------------------
// reopen2 junk-fields cross-connection repro (the core #738/#739 fix).
//
// smbtorture's reopen2 family reconnects from a FRESH connection+session+tree
// and, BY DESIGN, fills every CREATE request field except the reconnect blob
// with junk (security_flags=0x78, oplock_level=0x78, impersonation=0x12345678,
// create_flags=0x12345678, desired_access=0x12345678, share_access=0x12345678,
// create_disposition=0x12345678; for the oplock case also a junk fname) — to
// prove the server consults ONLY the reconnect context. The reconnect must
// return NT_STATUS_OK, EXISTED, size==0, and the ORIGINAL granted access.
//
// These drive the REAL Create() entrypoint with those junk values. Before the
// fix, validateAndRestore re-validated DesiredAccess/ShareAccess (-> ACCESS_DENIED)
// and processV2Reconnect always path-checked (-> INVALID_PARAMETER for the V2
// junk-fname oplock case). Confirmed against Samba source3/smbd/smb2_create.c:
// the durable reconnect path performs NO desired_access/share_access compare.
const (
	reopen2JunkAccess uint32 = 0x12345678 // smbtorture reopen2 garbage value
	reopen2JunkShare  uint32 = 0x12345678
	reopen2JunkFname         = "__non_existing_fname__"
	reopen2OrigAccess uint32 = 0x001F01FF // access granted at the initial open
)

// assertReopen2Restored asserts a successful junk-field reconnect: SUCCESS,
// EXISTED, size==0, and that the reconnected OpenFile carries the ORIGINAL
// granted access (never the junk requested on the reconnect CREATE).
func (e *reopen2Env) assertReopen2Restored(t *testing.T, resp *CreateResponse) {
	t.Helper()
	if resp.Status != types.StatusSuccess {
		t.Fatalf("junk-field reconnect: status=0x%08x, want SUCCESS (junk request fields must be ignored)", uint32(resp.Status))
	}
	if resp.CreateAction != types.FileOpened {
		t.Errorf("reconnect CreateAction = %d, want FileOpened (EXISTED)", resp.CreateAction)
	}
	if resp.EndOfFile != 0 {
		t.Errorf("reconnect EndOfFile = %d, want 0 (size==0)", resp.EndOfFile)
	}
	rof, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("reconnect: OpenFile not registered")
	}
	if rof.GrantedAccess != reopen2OrigAccess {
		t.Errorf("reconnect GrantedAccess = 0x%x, want original 0x%x (junk 0x%x must NOT be applied)",
			rof.GrantedAccess, reopen2OrigAccess, reopen2JunkAccess)
	}
}

// rwhLeaseCtx builds an RWH lease request context for the reconnect.
func rwhLeaseCtx(leaseKey [16]byte) CreateContext {
	return CreateContext{Name: LeaseContextTagRequest, Data: encodeV2LeaseContext(leaseKey,
		lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, 0)}
}

// TestReopen2_V1Oplock_JunkFields_E2E — smb2.durable-open.reopen2 (ACCESS_DENIED
// pre-fix). V1 (DHnC) batch-oplock reconnect with junk access + junk fname.
func TestReopen2_V1Oplock_JunkFields_E2E(t *testing.T) {
	e := setupReopen2Env(t)
	metaSvc := e.h.Registry.GetMetadataService()
	rootHandle, _ := e.h.Registry.GetRootHandle(e.tree.ShareName)
	file, _, _ := e.h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "durable.txt")
	existingHandle, _ := metadata.EncodeFileHandle(file)

	const sid = uint64(70)
	e.h.CreateSessionWithID(sid, "127.0.0.1:1", false, "alice", "WORKGROUP")
	resp := e.h.completeCreateAfterBreak(e.makeSMBCtx(sid), &createDraft{
		req: &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     reopen2OrigAccess,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelBatch,
			CreateContexts:    []CreateContext{dhnqContext()},
		},
		tree: e.tree, authCtx: rootAuthCtx(), filename: "durable.txt", baseName: "durable.txt",
		parentHandle: rootHandle, existingFile: file, existingHandle: existingHandle,
		fileExists: true, createAction: types.FileOpened,
	})
	if resp.Status != types.StatusSuccess {
		t.Fatalf("initial V1 oplock open: status=0x%08x", uint32(resp.Status))
	}
	of, _ := e.h.GetOpenFile(resp.FileID)
	fid := of.FileID
	e.disconnect(sid)

	e.h.CreateSessionWithID(71, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcResp, err := e.h.Create(e.makeSMBCtx(71), &CreateRequest{
		FileName:          reopen2JunkFname, // junk fname — ignored for oplock reconnect
		DesiredAccess:     reopen2JunkAccess,
		ShareAccess:       reopen2JunkShare,
		CreateDisposition: 0x12345678,
		OplockLevel:       0x78,
		CreateContexts:    []CreateContext{dhncContext(fid)},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	e.assertReopen2Restored(t, rcResp)
	if rcResp.OplockLevel != OplockLevelBatch {
		t.Errorf("reconnect OplockLevel = 0x%02x, want Batch", rcResp.OplockLevel)
	}
}

// initJunkLeaseReconnect runs the shared body of the lease-backed junk reconnect
// tests: initial RWH lease + durable open, disconnect, then a fresh reconnect
// with junk access but the CORRECT lease key + fname (a lease reconnect
// path-checks; the lease key is the identity). durableReq builds the initial
// durable request (DHnQ or DH2Q); reconnectCtx builds the reconnect contexts.
func initJunkLeaseReconnect(t *testing.T, durableReq func(cg [16]byte) []CreateContext,
	reconnectCtx func(fid, cg, lk [16]byte) []CreateContext) {
	t.Helper()
	e := setupReopen2Env(t)
	leaseKey := [16]byte{0x5A, 0x5B, 0x5C, 0x5D}
	createGuid := [16]byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF, 0xB0}

	const sid = uint64(80)
	_, of := e.initialLeaseDurableOpen(t, sid, leaseKey, durableReq(createGuid))
	fid := of.FileID
	e.disconnect(sid)

	const sid2 = uint64(81)
	e.h.CreateSessionWithID(sid2, "127.0.0.1:2", false, "alice", "WORKGROUP")
	resp, err := e.h.Create(e.makeSMBCtx(sid2), &CreateRequest{
		FileName:          "durable.txt", // correct fname (lease reconnect path-checks)
		DesiredAccess:     reopen2JunkAccess,
		ShareAccess:       reopen2JunkShare,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelLease,
		CreateContexts:    reconnectCtx(fid, createGuid, leaseKey),
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	e.assertReopen2Restored(t, resp)
	assertLeaseReconnect(t, resp, leaseKey, true)
}

// TestReopen2_V1Lease_JunkAccess_E2E — smb2.durable-open.reopen2-lease.
func TestReopen2_V1Lease_JunkAccess_E2E(t *testing.T) {
	initJunkLeaseReconnect(t,
		func(cg [16]byte) []CreateContext { return []CreateContext{dhnqContext()} },
		func(fid, cg, lk [16]byte) []CreateContext {
			return []CreateContext{dhncContext(fid), rwhLeaseCtx(lk)}
		})
}

// TestReopen2_V1LeaseV2_JunkAccess_E2E — smb2.durable-open.reopen2-lease-v2.
func TestReopen2_V1LeaseV2_JunkAccess_E2E(t *testing.T) {
	initJunkLeaseReconnect(t,
		func(cg [16]byte) []CreateContext { return []CreateContext{dhnqContext()} },
		func(fid, cg, lk [16]byte) []CreateContext {
			return []CreateContext{dhncContext(fid), rwhLeaseCtx(lk)}
		})
}

// TestReopen2_V2Oplock_JunkFields_E2E — smb2.durable-v2-open.reopen2
// (INVALID_PARAMETER pre-fix). V2 (DH2C) batch-oplock reconnect with junk
// access + JUNK fname — the V2-specific trap the always-path-check bug hit.
func TestReopen2_V2Oplock_JunkFields_E2E(t *testing.T) {
	e := setupReopen2Env(t)
	metaSvc := e.h.Registry.GetMetadataService()
	rootHandle, _ := e.h.Registry.GetRootHandle(e.tree.ShareName)
	file, _, _ := e.h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "durable.txt")
	existingHandle, _ := metadata.EncodeFileHandle(file)
	createGuid := [16]byte{0xC1, 0xC2, 0xC3, 0xC4, 0xC5, 0xC6, 0xC7, 0xC8, 0xC9, 0xCA, 0xCB, 0xCC, 0xCD, 0xCE, 0xCF, 0xD0}

	const sid = uint64(90)
	e.h.CreateSessionWithID(sid, "127.0.0.1:1", false, "alice", "WORKGROUP")
	resp := e.h.completeCreateAfterBreak(e.makeSMBCtx(sid), &createDraft{
		req: &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     reopen2OrigAccess,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelBatch,
			CreateContexts:    []CreateContext{dh2qContext(createGuid, 60000)},
		},
		tree: e.tree, authCtx: rootAuthCtx(), filename: "durable.txt", baseName: "durable.txt",
		parentHandle: rootHandle, existingFile: file, existingHandle: existingHandle,
		fileExists: true, createAction: types.FileOpened,
	})
	if resp.Status != types.StatusSuccess {
		t.Fatalf("initial V2 oplock open: status=0x%08x", uint32(resp.Status))
	}
	of, _ := e.h.GetOpenFile(resp.FileID)
	if !of.IsDurable || of.CreateGuid != createGuid {
		t.Fatalf("initial open: V2 durability not granted (durable=%v guid=%x)", of.IsDurable, of.CreateGuid)
	}
	fid := of.FileID
	e.disconnect(sid)

	e.h.CreateSessionWithID(91, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcResp, err := e.h.Create(e.makeSMBCtx(91), &CreateRequest{
		FileName:          reopen2JunkFname, // junk fname — ignored for non-lease V2 reconnect
		DesiredAccess:     reopen2JunkAccess,
		ShareAccess:       reopen2JunkShare,
		CreateDisposition: 0x12345678,
		OplockLevel:       0x78,
		CreateContexts:    []CreateContext{dh2cContext(fid, createGuid)},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	e.assertReopen2Restored(t, rcResp)
}

// TestReopen2_V2Lease_JunkAccess_E2E — smb2.durable-v2-open.reopen2-lease
// (ACCESS_DENIED pre-fix). V2 (DH2C) lease reconnect with junk access.
func TestReopen2_V2Lease_JunkAccess_E2E(t *testing.T) {
	initJunkLeaseReconnect(t,
		func(cg [16]byte) []CreateContext { return []CreateContext{dh2qContext(cg, 60000)} },
		func(fid, cg, lk [16]byte) []CreateContext {
			return []CreateContext{dh2cContext(fid, cg), rwhLeaseCtx(lk)}
		})
}

// TestReopen2_V2LeaseV2_JunkAccess_E2E — smb2.durable-v2-open.reopen2-lease-v2
// (ACCESS_DENIED pre-fix). Same as above with the V2 lease wire shape.
func TestReopen2_V2LeaseV2_JunkAccess_E2E(t *testing.T) {
	initJunkLeaseReconnect(t,
		func(cg [16]byte) []CreateContext { return []CreateContext{dh2qContext(cg, 60000)} },
		func(fid, cg, lk [16]byte) []CreateContext {
			return []CreateContext{dh2cContext(fid, cg), rwhLeaseCtx(lk)}
		})
}

// takeExclusiveLock records an exclusive byte-range lock on the open's backing
// file under the open's OpenID, exactly as the LOCK handler does. Used to model
// the held byte-range lock in smb2.durable-v2-open.lock-{oplock,lease}.
func (e *reopen2Env) takeExclusiveLock(t *testing.T, of *OpenFile, sessionID uint64, offset, length uint64) {
	t.Helper()
	metaSvc := e.h.Registry.GetMetadataService()
	fl := metadata.FileLock{
		SessionID:  sessionID,
		OpenID:     of.OpenID(),
		ClientID:   "smb:lock-test",
		Offset:     offset,
		Length:     length,
		Exclusive:  true,
		AcquiredAt: time.Now(),
	}
	if err := metaSvc.LockFile(rootAuthCtx(), of.MetadataHandle, fl); err != nil {
		t.Fatalf("LockFile: %v", err)
	}
	of.HasByteRangeLocks.Store(true)
}

// lockHeldUnderOpenID reports whether a byte-range lock exists on the file under
// the given OpenID — i.e. the lock survived disconnect and the reconnect restored
// the same OpenID (so a subsequent UNLOCK can find it).
func (e *reopen2Env) lockHeldUnderOpenID(metaHandle []byte, openID string) bool {
	lm, err := e.h.Registry.GetMetadataService().GetLockManagerForHandle(metaHandle)
	if err != nil || lm == nil {
		return false
	}
	for _, fl := range lm.ListLocks(string(metaHandle)) {
		if fl.OpenID == openID {
			return true
		}
	}
	return false
}

// TestReopen2_LockLease_RestoresLeaseAndKeepsLock covers
// smb2.durable-v2-open.lock-lease: open(RWH lease, durable-v2), take an
// exclusive byte-range lock, disconnect, reconnect. The reconnect MUST restore
// the lease at RWH (the lease re-grant must NOT be suppressed by the still-held
// byte-range lock) AND the lock must survive under the restored OpenID so the
// client's subsequent UNLOCK succeeds.
func TestReopen2_LockLease_RestoresLeaseAndKeepsLock(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0x4C, 0x4B, 0x4C, 0x4C} // "LKLL"
	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}

	const sessionID = uint64(50)
	_, of := e.initialLeaseDurableOpen(t, sessionID, leaseKey, []CreateContext{dh2qContext(createGuid, 60000)})
	persistedFileID := of.FileID
	origOpenID := of.OpenID()

	// Hold an exclusive BRL [0,1), as the test does before disconnect.
	e.takeExclusiveLock(t, of, sessionID, 0, 1)
	if !e.lockHeldUnderOpenID(of.MetadataHandle, origOpenID) {
		t.Fatal("setup: lock not recorded under original OpenID")
	}

	e.disconnect(sessionID)

	const sessionID2 = uint64(51)
	e.h.CreateSessionWithID(sessionID2, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(sessionID2)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelLease,
		CreateContexts: []CreateContext{
			dh2cContext(persistedFileID, createGuid),
			rwhLeaseCtx(leaseKey),
		},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	assertLeaseReconnect(t, rcResp, leaseKey, true)

	// The lock must still be present under the restored OpenID (FileID), or the
	// client's UNLOCK on the reconnected handle would fail RANGE_NOT_LOCKED.
	rof, ok := e.h.GetOpenFile(rcResp.FileID)
	if !ok {
		t.Fatal("reconnect: OpenFile not registered")
	}
	if !e.lockHeldUnderOpenID(rof.MetadataHandle, rof.OpenID()) {
		t.Errorf("reconnect: byte-range lock lost (OpenID %s) — UNLOCK would fail", rof.OpenID())
	}
}

// TestReopen2_LockOplock_RestoresBatchAndKeepsLock covers
// smb2.durable-v2-open.lock-oplock: open(BATCH oplock, durable-v2), take an
// exclusive byte-range lock, disconnect, reconnect. The reconnect MUST restore
// OplockLevel=Batch (not None) and keep the byte-range lock under the restored
// OpenID.
func TestReopen2_LockOplock_RestoresBatchAndKeepsLock(t *testing.T) {
	e := setupReopen2Env(t)

	createGuid := [16]byte{0xB0, 0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7, 0xB8, 0xB9, 0xBA, 0xBB, 0xBC, 0xBD, 0xBE, 0xBF}

	metaSvc := e.h.Registry.GetMetadataService()
	rootHandle, _ := e.h.Registry.GetRootHandle(e.tree.ShareName)
	file, _, _ := e.h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "durable.txt")
	existingHandle, _ := metadata.EncodeFileHandle(file)

	const sessionID = uint64(60)
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")
	smbCtx := e.makeSMBCtx(sessionID)

	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     0x001F01FF,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelBatch,
			CreateContexts:    []CreateContext{dh2qContext(createGuid, 60000)},
		},
		tree:           e.tree,
		authCtx:        rootAuthCtx(),
		filename:       "durable.txt",
		baseName:       "durable.txt",
		parentHandle:   rootHandle,
		existingFile:   file,
		existingHandle: existingHandle,
		fileExists:     true,
		createAction:   types.FileOpened,
	}
	resp := e.h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("initial open: status=0x%08x", uint32(resp.Status))
	}
	of, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("initial open: OpenFile not registered")
	}
	if of.OplockLevel != OplockLevelBatch {
		t.Fatalf("initial open: OplockLevel = 0x%02x, want Batch", of.OplockLevel)
	}
	persistedFileID := of.FileID
	origOpenID := of.OpenID()

	e.takeExclusiveLock(t, of, sessionID, 0, 1)

	e.disconnect(sessionID)

	const sessionID2 = uint64(61)
	e.h.CreateSessionWithID(sessionID2, "127.0.0.1:2", false, "alice", "WORKGROUP")
	rcCtx := e.makeSMBCtx(sessionID2)
	rcResp, err := e.h.Create(rcCtx, &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateContexts:    []CreateContext{dh2cContext(persistedFileID, createGuid)},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	if rcResp.Status != types.StatusSuccess {
		t.Fatalf("reconnect: status=0x%08x, want SUCCESS", uint32(rcResp.Status))
	}
	if rcResp.OplockLevel != OplockLevelBatch {
		t.Errorf("reconnect OplockLevel = 0x%02x, want Batch (0x%02x)", rcResp.OplockLevel, OplockLevelBatch)
	}
	rof, ok := e.h.GetOpenFile(rcResp.FileID)
	if !ok {
		t.Fatal("reconnect: OpenFile not registered")
	}
	if rof.OpenID() != origOpenID {
		t.Errorf("reconnect OpenID = %s, want original %s (BRL would be orphaned)", rof.OpenID(), origOpenID)
	}
	if !e.lockHeldUnderOpenID(rof.MetadataHandle, rof.OpenID()) {
		t.Errorf("reconnect: byte-range lock lost (OpenID %s) — UNLOCK would fail", rof.OpenID())
	}
}

// recordingBreakNotifier counts SendLeaseBreak calls so a test can assert that
// an AppInstanceId failover does NOT emit any oplock/lease break.
type recordingBreakNotifier struct{ count int }

func (n *recordingBreakNotifier) SendLeaseBreak(_ uint64, _ [16]byte, _, _ uint32, _ uint16) error {
	n.count++
	return nil
}

// appInstanceIdCtx builds an SMB2_CREATE_APP_INSTANCE_ID create context.
func appInstanceIdCtx(appID [16]byte) CreateContext {
	data := make([]byte, 20)
	binary.LittleEndian.PutUint16(data[0:2], 20) // StructureSize
	copy(data[4:20], appID[:])
	return CreateContext{Name: AppInstanceIdTag, Data: data}
}

// TestAppInstance_SilentFailover covers smb2.durable-v2-open.app-instance: a
// second open of the same file carrying the same AppInstanceId (but a different
// CreateGuid) while the first open is still live MUST force-close the first open
// WITHOUT emitting an oplock break (break_info.count == 0), and the displaced
// handle must no longer be registered. The AppInstanceId force-close runs before
// the oplock-break dispatch in the CREATE path; if it ran after, the second
// batch-oplock open would break the first's batch oplock first (count == 1).
func TestAppInstance_SilentFailover(t *testing.T) {
	e := setupReopen2Env(t)
	notifier := &recordingBreakNotifier{}
	e.h.LeaseManager.SetNotifier(notifier)

	appID := [16]byte{0xA9, 0x91, 0x01, 0xFF, 0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90, 0xA0, 0xB0, 0xC0}
	createGuid1 := [16]byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, 0x10}
	createGuid2 := [16]byte{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, 0x20}

	open := func(sessionID uint64, createGuid [16]byte) *CreateResponse {
		e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")
		rcCtx := e.makeSMBCtx(sessionID)
		resp, err := e.h.Create(rcCtx, &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     0x001F01FF,
			ShareAccess:       0, // share_access("") — no sharing
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelBatch,
			CreateContexts: []CreateContext{
				dh2qContext(createGuid, 60000),
				appInstanceIdCtx(appID),
			},
		})
		if err != nil {
			t.Fatalf("session %d open: %v", sessionID, err)
		}
		if resp.Status != types.StatusSuccess {
			t.Fatalf("session %d open: status=0x%08x, want SUCCESS", sessionID, uint32(resp.Status))
		}
		return resp
	}

	resp1 := open(70, createGuid1)
	of1ID := resp1.FileID

	// Second open: same file, same AppInstanceId, different CreateGuid, on a
	// distinct session — must force-close open #1 silently.
	resp2 := open(71, createGuid2)

	if notifier.count != 0 {
		t.Errorf("break_info.count = %d, want 0 (AppInstanceId failover must be silent)", notifier.count)
	}
	if _, ok := e.h.GetOpenFile(of1ID); ok {
		t.Error("first open still registered after same-AppInstanceId failover (should be force-closed)")
	}
	if resp2.OplockLevel != OplockLevelBatch {
		t.Errorf("second open OplockLevel = 0x%02x, want Batch (no conflicting open remains)", resp2.OplockLevel)
	}
}
