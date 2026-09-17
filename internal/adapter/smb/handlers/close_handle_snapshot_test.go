// Regression coverage for the explicit-CLOSE metadata-handle snapshot.
//
// SET_REPARSE_POINT republishes a live open's MetadataHandle under openFile.mu
// when a placeholder becomes a symlink, and the dispatcher runs one goroutine
// per request, so a CLOSE on one connection can observe the field change
// mid-teardown while an FSCTL_SET_REPARSE_POINT on another republishes it. The
// explicit CLOSE path used to read the field directly at each step, so the
// block-store resolve, the pending-write flush, the atime write, the byte-range
// unlock and the lease release could each name a different file.
//
// The test drives Close() end to end against a metadata store that republishes
// MetadataHandle during the teardown (the same in-place mutation
// SET_REPARSE_POINT performs), then asserts that the byte-range lock and the
// lease record on the ORIGINAL handle are gone. A per-step field read would
// release them against the republished handle instead, leaving the original
// open's lock and lease standing.
package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// republishingMetaStore delegates to a real metadata store and, on the first
// GetFile it serves, fires a callback. CLOSE calls GetFile after it has taken
// its handle snapshot and before it releases locks and leases, which is exactly
// the window a concurrent SET_REPARSE_POINT republishes in.
type republishingMetaStore struct {
	metadata.Store
	onGetFile func(handle metadata.FileHandle)
}

func (s *republishingMetaStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if s.onGetFile != nil {
		s.onGetFile(handle)
	}
	return s.Store.GetFile(ctx, handle)
}

// TestClose_ReleasesLocksAndLeaseOnTheSnapshotHandle pins that one
// MetadataHandle snapshot spans the lock release and the lease break, so a
// republish landing mid-CLOSE cannot split them across two files.
func TestClose_ReleasesLocksAndLeaseOnTheSnapshotHandle(t *testing.T) {
	var (
		mu              sync.Mutex
		openFile        *OpenFile
		originalFH      metadata.FileHandle
		republished     metadata.FileHandle
		republishedOnce bool
	)

	// The wrapper is registered as the share's metadata store, so it must be
	// usable from AddShare onward; only its callback is armed once the handle
	// under test exists.
	store := &republishingMetaStore{Store: metamemory.NewMemoryMetadataStoreWithDefaults()}

	h, smbCtx, fileHandle, fileID := setupWriteTestShare(t, store)
	ctx := context.Background()
	shareName := smbCtx.ShareName
	metaSvc := h.Registry.GetMetadataService()

	of, ok := h.GetOpenFile(fileID)
	if !ok {
		t.Fatal("setup: open file not registered")
	}

	// A second file in the same share: the handle a mid-CLOSE republish moves
	// the open onto. A real handle keeps the "wrong file" failure observable —
	// the release would land on this file's lock-manager bucket.
	rootHandle, err := h.Registry.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	other, _, err := metaSvc.CreateFile(&metadata.AuthContext{
		Context: ctx,
		Identity: &metadata.Identity{
			UID: func() *uint32 { u := uint32(0); return &u }(),
			GID: func() *uint32 { g := uint32(0); return &g }(),
		},
	}, rootHandle, "other", &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644})
	if err != nil {
		t.Fatalf("CreateFile other: %v", err)
	}
	otherFH, err := metadata.EncodeFileHandle(other)
	if err != nil {
		t.Fatalf("EncodeFileHandle other: %v", err)
	}

	mu.Lock()
	openFile = of
	originalFH = fileHandle
	republished = otherFH
	mu.Unlock()

	// Arm the republish: on the next read of the ORIGINAL handle, mimic the
	// exact in-place mutation SET_REPARSE_POINT performs.
	store.onGetFile = func(handle metadata.FileHandle) {
		mu.Lock()
		defer mu.Unlock()
		if republishedOnce || string(handle) != string(originalFH) {
			return
		}
		republishedOnce = true
		openFile.mu.Lock()
		openFile.MetadataHandle = republished
		openFile.PayloadID = ""
		openFile.mu.Unlock()
	}

	// A byte-range lock held by this open on the ORIGINAL handle, acquired
	// through the same lock manager CLOSE's unlock routes to.
	shareLM := metaSvc.GetLockManagerForShare(shareName)
	if shareLM == nil {
		t.Fatal("setup: share has no lock manager")
	}
	if err := shareLM.Lock(string(fileHandle), lock.FileLock{
		SessionID: smbCtx.SessionID,
		OpenID:    of.OpenID(),
		Offset:    0,
		Length:    0,
		Exclusive: true,
	}); err != nil {
		t.Fatalf("Lock on the original handle: %v", err)
	}
	// A lock on the republish target under a different open. It must survive:
	// a release that acted on the wrong handle would show up as its absence.
	if err := shareLM.Lock(string(otherFH), lock.FileLock{
		SessionID: smbCtx.SessionID,
		OpenID:    "other-open",
		Offset:    0,
		Length:    0,
		Exclusive: true,
	}); err != nil {
		t.Fatalf("Lock on the republish target: %v", err)
	}

	// A lease record on the original handle, so the release path runs. The
	// lease manager gets its own lock manager; the assertions below inspect it
	// directly rather than through the share's.
	leaseMgr := lock.NewManager()
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: leaseMgr}, nil)
	leaseKey := [16]byte{0x26, 0x65}
	of.OplockLevel = OplockLevelLease
	of.LeaseKey = leaseKey
	if _, _, err := h.LeaseManager.RequestLease(
		ctx, lock.FileHandle(fileHandle), leaseKey, [16]byte{},
		smbCtx.SessionID, [16]byte{}, of.OpenID(), "client-1", shareName,
		lock.LeaseStateRead|lock.LeaseStateHandle, false,
	); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	// A coalesced LastAccessTime bump on the handle: CLOSE persists it before the
	// frozen restore, and that read is the first metadata call after the
	// snapshot. Arming it puts the republish (fired by that read) ahead of the
	// restore, which is the ordering the frozen check below needs to be able to
	// tell the two files apart.
	pendingAtime := time.Now().Add(time.Hour)

	// A frozen timestamp on the handle, so CLOSE's final restore runs. The restore
	// writes the frozen value back through the handle; it must land on the file
	// the snapshot names, not the republished one. The value is in the future so
	// it wins GetFile's max(pending, stored) merge on either file, which is what
	// makes "which file did the restore write to" observable.
	//
	// FILE_WRITE_ATTRIBUTES on the open is what the restore's write authorizes
	// against; without it the restore declines and the check below is vacuous.
	frozen := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	of.mu.Lock()
	of.GrantedAccess |= uint32(types.FileWriteAttributes)
	of.SmbPendingAtime = pendingAtime
	of.MtimeFrozen = true
	of.FrozenMtime = &frozen
	of.mu.Unlock()

	// POSTQUERY_ATTRIB makes the attributes step read the file, which is where
	// the republish fires.
	resp, err := h.Close(smbCtx, &CloseRequest{FileID: fileID, Flags: uint16(types.SMB2ClosePostQueryAttrib)})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if resp == nil {
		t.Fatal("Close: nil response")
	}

	// Non-vacuous: the republish really landed mid-CLOSE.
	if got := of.GetMetadataHandle(); string(got) != string(otherFH) {
		t.Fatalf("the handle was not republished during CLOSE (handle = %x, want the republish target %x); "+
			"the test cannot observe the race", got, otherFH)
	}

	// The lock this open held on the file it was operating on must be gone.
	for _, l := range shareLM.ListLocks(string(fileHandle)) {
		if l.OpenID == of.OpenID() {
			t.Fatalf("CLOSE left the open's byte-range lock on the file it was operating on: "+
				"the unlock was routed through the republished handle (%x) instead of the "+
				"snapshot taken at CLOSE entry (%x)", otherFH, fileHandle)
		}
	}
	// And the release must not have touched the file the handle moved to.
	var otherStillLocked bool
	for _, l := range shareLM.ListLocks(string(otherFH)) {
		if l.OpenID == "other-open" {
			otherStillLocked = true
		}
	}
	if !otherStillLocked {
		t.Fatal("CLOSE released a byte-range lock on the republish target, a file this open never held")
	}

	// Same for the lease record: released on the snapshot handle, not the
	// republished one.
	if _, _, found := h.LeaseManager.GetLeaseState(ctx, lock.FileHandle(fileHandle), shareName, leaseKey); found {
		t.Fatal("CLOSE left the open's lease record on the file it was operating on: the lease " +
			"release named the republished handle instead of the same snapshot the lock " +
			"release used")
	}

	// The frozen-timestamp restore must also have landed on the snapshot file.
	restored, err := metaSvc.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile after CLOSE: %v", err)
	}
	if !restored.Mtime.Equal(frozen) {
		t.Fatalf("CLOSE restored the frozen Mtime on the wrong file: the original handle's "+
			"Mtime is %v, want the frozen %v. The restore named the republished handle (%x) "+
			"instead of the snapshot taken at CLOSE entry (%x)", restored.Mtime, frozen, otherFH, fileHandle)
	}
	moved, err := metaSvc.GetFile(ctx, otherFH)
	if err != nil {
		t.Fatalf("GetFile republish target: %v", err)
	}
	if moved.Mtime.Equal(frozen) {
		t.Fatal("CLOSE restored a frozen timestamp on the republish target, a file this open never held")
	}

	if _, ok := h.GetOpenFile(fileID); ok {
		t.Fatal("OpenFile still registered after CLOSE")
	}
}
