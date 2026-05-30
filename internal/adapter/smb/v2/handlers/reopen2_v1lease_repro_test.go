package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestRepro_V1Lease_V1WireContext mirrors smbtorture smb2.durable-open.reopen2-lease
// AS FAITHFULLY AS THE HANDLER ALLOWS: the initial open uses a V1 (32-byte) RqLs
// lease context (smb2_lease_create) + a V1 DHnQ durable request (io.in.durable_open
// = true), then disconnects and reconnects via DHnC + a V1 (32-byte) RqLs context.
//
// This is distinct from the existing TestReopen2_V1Lease_* tests which inject a
// V2 (52-byte) RqLs context. The goal is to find any handler-reachable divergence
// caused by the V1 lease wire shape.
func TestRepro_V1Lease_V1WireContext(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0xAB, 0xCD, 0xEF, 0x01}
	rwh := uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)

	const sessionID = uint64(220)
	_, of := e.initialLeaseDurableOpenV1Wire(t, sessionID, leaseKey, []CreateContext{dhnqContext()})

	if of.OplockLevel != OplockLevelLease {
		t.Fatalf("initial open: OplockLevel = 0x%02x, want Lease", of.OplockLevel)
	}
	if !of.IsDurable {
		t.Fatal("initial open: V1 durability NOT granted (lease with Handle, V1 wire) — handle will never persist")
	}
	persistedFileID := of.FileID

	e.disconnect(sessionID)

	const sessionID2 = uint64(221)
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
			{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, rwh)},
		},
	})
	if err != nil {
		t.Fatalf("reconnect Create error: %v", err)
	}
	if rcResp.Status != types.StatusSuccess {
		t.Fatalf("V1-lease (V1 wire ctx) reconnect: status=0x%08x, want SUCCESS", uint32(rcResp.Status))
	}
}

// TestRepro_V1Lease_NegativeLadder_V1WireContext is the closest handler-level
// model of smbtorture smb2.durable-open.reopen2-lease: a single persisted V1
// durable+lease handle, then the EXACT negative ladder (ONF, ONF, ONF, ONF,
// INVALID_PARAMETER) followed by the positive reconnect (OK) — all using V1
// (32-byte) RqLs lease contexts (smb2_lease_create), against ONE persisted
// handle. The handle must survive every non-destructive negative attempt.
func TestRepro_V1Lease_NegativeLadder_V1WireContext(t *testing.T) {
	e := setupReopen2Env(t)

	leaseKey := [16]byte{0x71, 0x72, 0x73}
	wrongLeaseKey := [16]byte{0x81, 0x82, 0x83}
	rwh := uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)

	const sessionID = uint64(260)
	_, of := e.initialLeaseDurableOpenV1Wire(t, sessionID, leaseKey, []CreateContext{dhnqContext()})
	if !of.IsDurable {
		t.Fatal("initial open: V1 durability not granted")
	}
	persistedFileID := of.FileID
	e.disconnect(sessionID)

	v1rwh := func(k [16]byte) CreateContext {
		return CreateContext{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(k, rwh)}
	}

	cases := []struct {
		name      string
		fileName  string
		leaseKey  [16]byte
		omitLease bool
		want      types.Status
	}{
		{"empty-fname-no-lease", "", leaseKey, true, types.StatusObjectNameNotFound},
		{"nonexisting-no-lease", "__non_existing_fname__", leaseKey, true, types.StatusObjectNameNotFound},
		{"correct-fname-no-lease", "durable.txt", leaseKey, true, types.StatusObjectNameNotFound},
		{"wrong-lease-key", "durable.txt", wrongLeaseKey, false, types.StatusObjectNameNotFound},
		{"wrong-fname-with-lease", "__non_existing_fname__", leaseKey, false, types.StatusInvalidParameter},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := sessionID + 100 + uint64(len(tc.name))
			e.h.CreateSessionWithID(sid, "127.0.0.1:7", false, "alice", "WORKGROUP")
			rcCtx := e.makeSMBCtx(sid)
			contexts := []CreateContext{dhncContext(persistedFileID)}
			if !tc.omitLease {
				contexts = append(contexts, v1rwh(tc.leaseKey))
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

	// Positive reconnect (OK) — same persisted handle, V1 RqLs, correct fname+key.
	const finalSID = uint64(997)
	e.h.CreateSessionWithID(finalSID, "127.0.0.1:8", false, "alice", "WORKGROUP")
	rcResp, err := e.h.Create(e.makeSMBCtx(finalSID), &CreateRequest{
		FileName:          "durable.txt",
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		OplockLevel:       OplockLevelLease,
		CreateContexts:    []CreateContext{dhncContext(persistedFileID), v1rwh(leaseKey)},
	})
	if err != nil {
		t.Fatalf("final reopen: Create error: %v", err)
	}
	if rcResp.Status != types.StatusSuccess {
		t.Fatalf("final V1-lease reopen (V1 wire ctx): status=0x%08x, want SUCCESS", uint32(rcResp.Status))
	}
	if rcResp.OplockLevel != OplockLevelLease {
		t.Errorf("final reopen OplockLevel = 0x%02x, want Lease", rcResp.OplockLevel)
	}
}

// initialLeaseDurableOpenV1Wire is initialLeaseDurableOpen but with a V1 (32-byte)
// RqLs context instead of a V2 (52-byte) one.
func (e *reopen2Env) initialLeaseDurableOpenV1Wire(
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

	rwh := uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)
	contexts := []CreateContext{
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, rwh)},
	}
	contexts = append(contexts, durableCtx...)

	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     0x001F01FF,
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
