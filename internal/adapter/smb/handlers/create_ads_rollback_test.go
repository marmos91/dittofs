package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestCreate_ADS_LeaseKeyInUse_RollsBackBaseFile verifies that when a lease
// request is rejected with ErrLeaseKeyInUse AFTER a fresh ADS stream CREATE,
// the rollback removes BOTH the stream entry ("file.txt:StreamName") AND the
// base file ("file.txt") that the ADS auto-create path created for this
// request.
//
// Before the fix, only the stream entry was removed and the base file was
// left as an unreachable orphan: a subsequent FILE_CREATE on "file.txt" then
// failed with ErrAlreadyExists even though no client could see the file.
//
// The scenario is driven directly through completeCreateAfterBreak with a
// pre-set draft (mirroring what create.go produces after the ADS auto-create
// in Step 6):
//   - baseName            = "file.txt:StreamName" (ADS-qualified, created here)
//   - adsBaseFileName      = "file.txt"           (base seeded by Step 6)
//   - adsBaseCreatedByUs   = true                 (Step 6 owned the base)
//   - createAction         = FileCreated          (fresh stream CREATE)
//
// ErrLeaseKeyInUse is produced by a real LeaseManager: the request's lease key
// is pre-bound to a DIFFERENT file handle under the same client, so the
// cross-file lease-key uniqueness check (MS-SMB2 §3.3.5.9.8 / Samba
// lease_match) rejects this CREATE.
func TestCreate_ADS_LeaseKeyInUse_RollsBackBaseFile(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)

	tree := &TreeConnection{
		TreeID:    smbCtx.TreeID,
		SessionID: smbCtx.SessionID,
		ShareName: smbCtx.ShareName,
	}
	h.StoreTree(tree)

	// Wire a real LeaseManager backed by a real lock.Manager so RequestLease
	// can authentically produce ErrLeaseKeyInUse via lease_match.
	mgr := lock.NewManager()
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)

	metaSvc := rt.GetMetadataService()

	// Use the root identity so createNewFile (Step 7) has write access to the
	// share root (mode 0o755, owned by uid 0). The rollback path under test is
	// independent of the requester identity.
	auth := rootAuth

	// Lease key that we will bind to another file first, so the CREATE under
	// test trips ErrLeaseKeyInUse.
	var leaseKey [16]byte
	for i := range leaseKey {
		leaseKey[i] = byte(i + 1)
	}

	// completeCreateAfterBreak derives clientID as fmt.Sprintf("smb:%d",
	// ctx.SessionID) and uses tree.ShareName. Bind the key to a DIFFERENT file
	// handle under that exact (clientID, shareName) with a non-None state so
	// the in-memory hasLeaseKeyOnOtherFile check fires for our CREATE.
	clientID := "smb:1" // smbCtx.SessionID == 1 (setupDaclTest)
	otherHandle := lock.FileHandle("some-other-file-handle")
	granted, _, err := mgr.RequestLease(
		context.Background(),
		otherHandle,
		leaseKey,
		[16]byte{},
		"smb:lease:preexisting",
		clientID,
		smbCtx.ShareName,
		lock.LeaseStateRead|lock.LeaseStateHandle,
		false,
	)
	if err != nil {
		t.Fatalf("seed lease on other file: %v", err)
	}
	if granted == lock.LeaseStateNone {
		t.Fatalf("seed lease denied (granted=None); cannot set up ErrLeaseKeyInUse scenario")
	}

	// Seed the base file to simulate the ADS auto-create path (Step 6) having
	// already created it. The stream entry is NOT seeded: Step 7 of
	// completeCreateAfterBreak creates it via createNewFile.
	_, _, err = metaSvc.CreateFile(rootAuth, rootHandle, "file.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644})
	if err != nil {
		t.Fatalf("seed base file: %v", err)
	}

	// V2 lease request blob carrying the conflicting key with R|H state. A
	// non-stat DesiredAccess routes through RequestLease (not the stat-open
	// variant), so the cross-file lease_match rejection fires.
	leaseBlob := encodeV2LeaseContext(leaseKey, lock.LeaseStateRead|lock.LeaseStateHandle, 0)

	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "file.txt:StreamName",
			DesiredAccess:     0x001F01FF, // full access (not stat-only)
			ShareAccess:       0x07,
			CreateDisposition: types.FileCreate,
			CreateOptions:     0,
			FileAttributes:    types.FileAttributeNormal,
			OplockLevel:       OplockLevelLease,
			CreateContexts: []CreateContext{
				{Name: LeaseContextTagRequest, Data: leaseBlob},
			},
		},
		tree:               tree,
		authCtx:            auth,
		filename:           "file.txt:StreamName",
		baseName:           "file.txt:StreamName",
		parentHandle:       rootHandle,
		fileExists:         false,
		createAction:       types.FileCreated,
		adsBaseFileName:    "file.txt",
		adsBaseCreatedByUs: true,
	}

	resp := h.completeCreateAfterBreak(smbCtx, draft)

	// The lease rejection must surface as StatusInvalidParameter.
	if resp.Status != types.StatusInvalidParameter {
		t.Fatalf("status = 0x%08x; want StatusInvalidParameter (0x%08x)",
			uint32(resp.Status), uint32(types.StatusInvalidParameter))
	}

	// After rollback: stream entry must be gone.
	if streamFile, _, _ := metaSvc.LookupCaseInsensitive(auth, rootHandle, "file.txt:StreamName"); streamFile != nil {
		t.Error("stream entry still exists after rollback; expected removal")
	}

	// After rollback: base file must also be gone (the regression check).
	if baseFile, _, _ := metaSvc.LookupCaseInsensitive(auth, rootHandle, "file.txt"); baseFile != nil {
		t.Error("base file still exists after rollback; ADS base-file orphan not cleaned up")
	}

	// A fresh FILE_CREATE on the base name must now succeed (it was previously
	// broken because the orphaned base returned ErrAlreadyExists).
	if _, _, createErr := metaSvc.CreateFile(auth, rootHandle, "file.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}); createErr != nil {
		t.Errorf("re-create of base file after rollback: %v (expected success)", createErr)
	}
}
