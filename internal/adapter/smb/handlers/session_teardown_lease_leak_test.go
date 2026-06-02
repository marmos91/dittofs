package handlers

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Regression coverage for #568: SMB session teardown (transport drop / LOGOFF /
// tree-disconnect) must release each closing open's per-handle lease/oplock
// record and unregister its traditional-oplock FileID mapping — exactly as the
// explicit CLOSE handler does. Before the fix, closeFilesWithFilter deleted the
// OpenFile from the table but relied solely on LeaseManager.ReleaseSessionLeases
// (a sessionMap scan keyed by lease key) to drop lease records. Two server-side
// resources leaked across the test boundary and poisoned the next smbtorture
// suite, producing the rotating "new failure" flake:
//
//  1. The lock-manager lease/oplock RECORD, whenever a later session reused the
//     same numeric lease key on a different file and overwrote the sessionMap
//     entry (smbtorture reuses fixed LEASE1/LEASE2 macros on fresh connections).
//  2. The server-global oplock FileID registry entry for traditional oplocks,
//     which was only ever unregistered on explicit CLOSE.

// leakProbeNotifier implements lease.LeaseBreakNotifier + OplockFileIDRegistrar
// so the test can observe the server-global oplock FileID registry across a
// teardown.
type leakProbeNotifier struct {
	mu      sync.Mutex
	oplocks map[string][16]byte
}

func newLeakProbeNotifier() *leakProbeNotifier {
	return &leakProbeNotifier{oplocks: map[string][16]byte{}}
}

func (p *leakProbeNotifier) SendLeaseBreak(uint64, [16]byte, uint32, uint32, uint16) error {
	return nil
}

func (p *leakProbeNotifier) RegisterOplockFileID(leaseKey [16]byte, fileID [16]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.oplocks[string(leaseKey[:])] = fileID
}

func (p *leakProbeNotifier) UnregisterOplockFileID(leaseKey [16]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.oplocks, string(leaseKey[:]))
}

func (p *leakProbeNotifier) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.oplocks)
}

// teardownLeakEnv bundles a wired Handler with a real LeaseManager whose
// underlying lock.Manager and notifier are exposed for residual-state assertions.
type teardownLeakEnv struct {
	h        *Handler
	mgr      *lock.Manager
	notifier *leakProbeNotifier
	tree     *TreeConnection
	root     metadata.FileHandle
	rootAuth *metadata.AuthContext
	metaSvc  *metadata.Service
}

func setupTeardownLeakEnv(t *testing.T) *teardownLeakEnv {
	t.Helper()
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	mgr := lock.NewManager()
	notifier := newLeakProbeNotifier()
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, notifier)

	return &teardownLeakEnv{
		h:        h,
		mgr:      mgr,
		notifier: notifier,
		tree:     tree,
		root:     rootHandle,
		rootAuth: rootAuth,
		metaSvc:  rt.GetMetadataService(),
	}
}

// makeFile creates a regular file in the share root and returns its handle.
func (e *teardownLeakEnv) makeFile(t *testing.T, name string) ([]byte, *metadata.File) {
	t.Helper()
	if _, _, err := e.metaSvc.CreateFile(e.rootAuth, e.root, name, &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	}); err != nil {
		t.Fatalf("CreateFile %s: %v", name, err)
	}
	f, _, err := e.h.lookupCaseInsensitive(e.rootAuth, e.metaSvc, e.root, name)
	if err != nil || f == nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	fh, err := metadata.EncodeFileHandle(f)
	if err != nil {
		t.Fatalf("EncodeFileHandle %s: %v", name, err)
	}
	return fh, f
}

// openWithLease drives the real CREATE handler to open name with an RH lease
// under leaseKey on the given session, and returns the registered OpenFile.
func (e *teardownLeakEnv) openWithLease(
	t *testing.T, name string, fh []byte, f *metadata.File, sessionID uint64, leaseKey [16]byte,
) *OpenFile {
	t.Helper()
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")
	contexts := []CreateContext{{
		Name: LeaseContextTagRequest,
		Data: encodeV2LeaseContext(leaseKey, lock.LeaseStateRead|lock.LeaseStateHandle, 0),
	}}
	resp := e.create(t, name, fh, f, sessionID, OplockLevelLease, contexts)
	of, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatalf("open %s: OpenFile not registered", name)
	}
	return of
}

// openWithBatchOplock drives the real CREATE handler to open name requesting a
// Batch oplock (traditional, synthetic-key) on the given session.
func (e *teardownLeakEnv) openWithBatchOplock(
	t *testing.T, name string, fh []byte, f *metadata.File, sessionID uint64,
) *OpenFile {
	t.Helper()
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")
	resp := e.create(t, name, fh, f, sessionID, OplockLevelBatch, nil)
	of, ok := e.h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatalf("open %s: OpenFile not registered", name)
	}
	return of
}

func (e *teardownLeakEnv) create(
	t *testing.T,
	name string,
	fh []byte,
	f *metadata.File,
	sessionID uint64,
	oplockLevel uint8,
	contexts []CreateContext,
) *CreateResponse {
	t.Helper()
	draft := &createDraft{
		req: &CreateRequest{
			FileName:          name,
			DesiredAccess:     0x001F01FF,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       oplockLevel,
			CreateContexts:    contexts,
		},
		tree:           e.tree,
		authCtx:        e.rootAuth,
		filename:       name,
		baseName:       name,
		parentHandle:   e.root,
		existingFile:   f,
		existingHandle: fh,
		fileExists:     true,
		createAction:   types.FileOpened,
	}
	cs := &mockCryptoState{}
	cs.SetClientGUID([16]byte{byte(sessionID)})
	ctx := &SMBHandlerContext{
		Context:         context.Background(),
		SessionID:       sessionID,
		TreeID:          e.tree.TreeID,
		ShareName:       e.tree.ShareName,
		ConnCryptoState: cs,
	}
	resp := e.h.completeCreateAfterBreak(ctx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("create %s: status=0x%08x", name, uint32(resp.Status))
	}
	return resp
}

// disconnect runs the persist + lease-release halves of CleanupSession for a
// transport drop (steps 1+2). Calling CleanupSession directly would trip its
// cleanupWg.Done without a matching Add (owned by the dispatch scheduler).
func (e *teardownLeakEnv) disconnect(sessionID uint64) {
	ctx := context.Background()
	e.h.CloseAllFilesForSession(ctx, sessionID, true)
	e.h.releaseSessionLeasesAndNotifies(ctx, sessionID)
}

// TestSessionTeardown_ReleasesTraditionalOplockRegistry asserts that a transport
// drop releases the lock-manager record AND unregisters the synthetic oplock
// FileID mapping for a traditional (Batch) oplock — the registry was previously
// only cleaned on explicit CLOSE.
func TestSessionTeardown_ReleasesTraditionalOplockRegistry(t *testing.T) {
	e := setupTeardownLeakEnv(t)
	fh, f := e.makeFile(t, "oplock.txt")

	const sessionID = uint64(0xA1)
	of := e.openWithBatchOplock(t, "oplock.txt", fh, f, sessionID)
	if of.LeaseKey == ([16]byte{}) {
		t.Fatal("expected a synthetic lease key for the Batch oplock")
	}
	if e.notifier.count() != 1 {
		t.Fatalf("expected 1 oplock registry entry after open, got %d", e.notifier.count())
	}
	if !e.mgr.HasActiveLeaseRecord(string(fh), [16]byte{}) {
		t.Fatal("expected an active lock-manager record after oplock grant")
	}

	e.disconnect(sessionID)

	if e.mgr.HasActiveLeaseRecord(string(fh), [16]byte{}) {
		t.Error("LEAK: lock-manager oplock record survived session teardown")
	}
	if _, _, found := e.mgr.GetLeaseState(context.Background(), of.LeaseKey); found {
		t.Error("LEAK: oplock lease state survived session teardown")
	}
	if e.notifier.count() != 0 {
		t.Errorf("LEAK: oplock FileID registry has %d stale entries after teardown (want 0)", e.notifier.count())
	}
}

// TestSessionTeardown_ReleasesLeaseDespiteSessionMapOverwrite is the core #568
// regression. Session A opens file1 with lease key K; a later session B opens
// file2 with the SAME key K (a different file, which the lock manager permits
// across distinct clients). That overwrites the LeaseManager's sessionMap entry
// for K to point at B. When A disconnects, the old sessionMap-scan release could
// not find K mapped to A, so A's lock-manager record on file1 leaked. The fix
// releases by the open's own handle+key, so A's record is dropped while B's
// independent record on file2 is preserved.
func TestSessionTeardown_ReleasesLeaseDespiteSessionMapOverwrite(t *testing.T) {
	e := setupTeardownLeakEnv(t)
	fh1, f1 := e.makeFile(t, "file1.txt")
	fh2, f2 := e.makeFile(t, "file2.txt")

	key := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}
	const sessA = uint64(0xA1)
	const sessB = uint64(0xB2)

	e.openWithLease(t, "file1.txt", fh1, f1, sessA, key) // sessionMap[K] = A
	e.openWithLease(t, "file2.txt", fh2, f2, sessB, key) // sessionMap[K] = B (overwrite)

	if !e.mgr.HasActiveLeaseRecord(string(fh1), [16]byte{}) {
		t.Fatal("precondition: file1 should have an active lease record")
	}

	e.disconnect(sessA)

	if e.mgr.HasActiveLeaseRecord(string(fh1), [16]byte{}) {
		t.Error("LEAK: session A's lease record on file1 survived A's disconnect (sessionMap overwrite)")
	}
	if !e.mgr.HasActiveLeaseRecord(string(fh2), [16]byte{}) {
		t.Error("over-release: session B's independent lease record on file2 was wrongly removed by A's teardown")
	}
}

// TestSessionTeardown_ReleasesLeaseWithMultipleOpensSameFile guards the second-
// vs-first-pass placement of the release. Session A holds two opens of the SAME
// file under the SAME lease key; a third session reuses that key on another file
// (overwriting the sessionMap backstop). The per-handle release must run AFTER
// the open-file table is shrunk — otherwise each of A's two opens observes the
// other still present, both defer ("other open shares the key"), and the record
// leaks. Releasing in a post-delete pass makes the sibling scan see the shrunk
// table so the record is dropped exactly once.
func TestSessionTeardown_ReleasesLeaseWithMultipleOpensSameFile(t *testing.T) {
	e := setupTeardownLeakEnv(t)
	fh1, f1 := e.makeFile(t, "shared.txt")
	fh2, f2 := e.makeFile(t, "other.txt")

	key := [16]byte{0x01, 0x02, 0x03}
	const sessA = uint64(0xA1)
	const sessB = uint64(0xB2)

	// Two opens of shared.txt under the same key on session A.
	e.openWithLease(t, "shared.txt", fh1, f1, sessA, key)
	e.openWithLease(t, "shared.txt", fh1, f1, sessA, key)
	// A third session reuses the key on a different file, overwriting sessionMap.
	e.openWithLease(t, "other.txt", fh2, f2, sessB, key)

	if !e.mgr.HasActiveLeaseRecord(string(fh1), [16]byte{}) {
		t.Fatal("precondition: shared.txt should have an active lease record")
	}

	e.disconnect(sessA)

	if e.mgr.HasActiveLeaseRecord(string(fh1), [16]byte{}) {
		t.Error("LEAK: shared.txt lease record survived A teardown (multi-open same file/key)")
	}
	if !e.mgr.HasActiveLeaseRecord(string(fh2), [16]byte{}) {
		t.Error("over-release: session B's lease record on other.txt was wrongly removed")
	}
}
