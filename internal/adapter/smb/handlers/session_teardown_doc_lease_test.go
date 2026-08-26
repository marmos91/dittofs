package handlers

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// A delete-on-close carried into the LOGOFF / tree-disconnect / transport-drop
// teardown breaks the closing file's Handle leases. Per MS-FSA 2.1.5.5 phase 1
// step 2.1.1 a delete-on-close that meets a directory which still holds entries
// declines the removal and the close still succeeds, so reaching that break
// says nothing about whether anything was deleted. Handle caching on an entry
// that is still there stays valid, and stripping it tells every other holder
// its cached handle can no longer be reopened when it can.
//
// The break is directory-specific in practice: SET_INFO's equivalent is gated
// on !IsDirectory and CLOSE has no pre-removal break at all, so the teardown
// path is the only one that breaks a directory's Handle leases on
// delete-on-close — and a declined removal is by definition a directory.

// docLeaseBreakRecorder wraps a real lock manager and records the handle key of
// every lease break dispatched through it. The teardown dispatches two breaks
// against two different keys — the closing file's own handle for the Handle
// break, the parent directory's for the content-change break — so recording the
// key is what tells them apart. Dispatch is synchronous, so a break owed by a
// teardown has been recorded by the time that teardown returns.
type docLeaseBreakRecorder struct {
	lock.LockManager
	mu   sync.Mutex
	keys []string
}

func (r *docLeaseBreakRecorder) BreakLeasesOnOpenConflict(
	handleKey string, excludeOwner *lock.LockOwner, reason lock.BreakReason,
) error {
	r.mu.Lock()
	r.keys = append(r.keys, handleKey)
	r.mu.Unlock()
	return r.LockManager.BreakLeasesOnOpenConflict(handleKey, excludeOwner, reason)
}

func (r *docLeaseBreakRecorder) sawBreakFor(handle metadata.FileHandle) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range r.keys {
		if k == string(handle) {
			return true
		}
	}
	return false
}

// docLeaseEnv is a share holding directory "docdir", a Handle-caching (RH)
// directory lease on it owned by a session that is NOT the one being torn
// down, and one open on "docdir" carrying a delete-on-close on the session the
// test tears down.
type docLeaseEnv struct {
	h         *Handler
	rec       *docLeaseBreakRecorder
	root      metadata.FileHandle
	dirFH     metadata.FileHandle
	rootAuth  *metadata.AuthContext
	docSessID uint64
}

// holderSessionID owns the directory lease; docSessionID owns the
// delete-on-close open the teardown closes. They must differ: the break
// excludes the closing session's own leases.
const (
	holderSessionID = uint64(0xA1)
	docSessionID    = uint64(0xB2)
)

func newDocLeaseEnv(t *testing.T, withChild bool) *docLeaseEnv {
	t.Helper()
	h, rt, smbCtx, root, rootAuth := setupDaclTest(t)

	rec := &docLeaseBreakRecorder{LockManager: lock.NewManager()}
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: rec}, nil)

	metaSvc := rt.GetMetadataService()
	dir, _, err := metaSvc.CreateDirectory(rootAuth, root, "docdir", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateDirectory docdir: %v", err)
	}
	dirFH, err := metadata.EncodeFileHandle(dir)
	if err != nil {
		t.Fatalf("EncodeFileHandle docdir: %v", err)
	}
	if withChild {
		if _, _, err := metaSvc.CreateFile(rootAuth, dirFH, "child", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
		}); err != nil {
			t.Fatalf("CreateFile docdir/child: %v", err)
		}
	}

	// The other holder's RH directory lease. It has no open in h.files — the
	// state a durable handle persisted across a transport drop leaves behind,
	// and the state a concurrent CLOSE passes through between its
	// delete-on-close election and its lease release.
	holderKey := [16]byte{0xA1, 0xA1}
	granted, _, err := h.LeaseManager.RequestLease(
		context.Background(),
		lock.FileHandle(dirFH),
		holderKey,
		[16]byte{},
		holderSessionID,
		[16]byte{0x01},
		"owner-holder",
		fmt.Sprintf("smb:%d", holderSessionID),
		smbCtx.ShareName,
		lock.LeaseStateRead|lock.LeaseStateHandle,
		true,
	)
	if err != nil {
		t.Fatalf("RequestLease: %v", err)
	}
	if granted&lock.LeaseStateHandle == 0 {
		t.Fatalf("precondition: RH dir lease granted %s, want Handle caching",
			lock.LeaseStateToString(granted))
	}

	// The delete-on-close open the teardown will close.
	docOpen := (&OpenFile{
		FileID:         [16]byte{0xDC, 0x01},
		SessionID:      docSessionID,
		TreeID:         smbCtx.TreeID,
		IsDirectory:    true,
		ShareName:      smbCtx.ShareName,
		MetadataHandle: dirFH,
		DeletePending:  true,
	}).WithName(OpenName{Path: "/docdir", FileName: "docdir", ParentHandle: root})
	h.StoreOpenFile(docOpen)

	return &docLeaseEnv{
		h:         h,
		rec:       rec,
		root:      root,
		dirFH:     dirFH,
		rootAuth:  rootAuth,
		docSessID: docSessionID,
	}
}

// dirStillThere reports whether "docdir" survived the teardown.
func (e *docLeaseEnv) dirStillThere(t *testing.T) bool {
	t.Helper()
	f, _, err := e.h.Registry.GetMetadataService().LookupCaseInsensitive(e.rootAuth, e.root, "docdir")
	return err == nil && f != nil
}

// TestSessionTeardown_DeclinedDirRemoval_KeepsHandleLease is the regression:
// the teardown must not strip another holder's Handle caching for a removal
// that was declined.
func TestSessionTeardown_DeclinedDirRemoval_KeepsHandleLease(t *testing.T) {
	e := newDocLeaseEnv(t, true)

	e.h.CloseAllFilesForSession(context.Background(), e.docSessID, false)

	if !e.dirStillThere(t) {
		t.Fatal("precondition: docdir holds an entry, so the delete-on-close must have " +
			"declined its removal — the test cannot say anything about the break otherwise")
	}
	if e.rec.sawBreakFor(e.dirFH) {
		t.Error("teardown broke the directory's Handle lease for a removal that was declined: " +
			"docdir was not empty and is still there, so the holder's cached handle is still " +
			"reopenable and its Handle caching must survive")
	}
}

// TestSessionTeardown_DirRemoved_BreaksHandleLease is the other direction, and
// the reason the fix gates the break rather than deleting it: when the removal
// does happen, the Handle break still fires.
func TestSessionTeardown_DirRemoved_BreaksHandleLease(t *testing.T) {
	e := newDocLeaseEnv(t, false)

	e.h.CloseAllFilesForSession(context.Background(), e.docSessID, false)

	if e.dirStillThere(t) {
		t.Fatal("precondition: docdir is empty, so the delete-on-close must have removed it")
	}
	if !e.rec.sawBreakFor(e.dirFH) {
		t.Error("teardown removed docdir without breaking the directory's Handle lease: " +
			"the entry is gone, so every other holder's cached handle is unreopenable")
	}
}
