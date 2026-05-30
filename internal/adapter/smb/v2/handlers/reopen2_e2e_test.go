package handlers

import (
	"context"
	"encoding/binary"
	"testing"

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
