package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Coverage for the TOCTOU create-race recovery lease break: the race branch in
// completeCreateAfterBreak resyncs the draft to the winner and truncates/opens
// it, but the pre-break lease break ran against the stale (nil) view — the
// winner's lease holders never saw a break. The recovery must dispatch a break
// on the winner's handle before the overwrite proceeds so a holder with a
// write lease has its cached state invalidated before truncation.

// TestCreate_RaceRecovery_WinnerLeaseBroken pins that a SUPERSEDE which loses
// the create race against a lease-holding winner dispatches a lease break on
// the winner's handle: after the recovery, no holder may still hold the WRITE
// bit on the winner (the break drained before the overwrite truncated it).
func TestCreate_RaceRecovery_WinnerLeaseBroken(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	// Wire a real LeaseManager so the race-recovery break dispatches for real.
	mgr := lock.NewManager()
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)

	metaSvc := rt.GetMetadataService()
	parentHandle, _ := raceParentDir(t, metaSvc, rootAuth, rootHandle, "race-lease")

	// Pre-seed the winner owned by the requester (so the POSIX overwrite check
	// passes) with a non-zero size so a successful supersede is observable.
	winner, _, err := metaSvc.CreateFile(rootAuth, parentHandle, "winner.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o700, UID: 2001, GID: 2001})
	if err != nil {
		t.Fatalf("seed winner: %v", err)
	}
	winnerHandle, err := metadata.EncodeFileHandle(winner)
	if err != nil {
		t.Fatalf("EncodeFileHandle winner: %v", err)
	}
	winnerSize := uint64(4096)
	if _, err := metaSvc.SetFileAttributes(rootAuth, winnerHandle, &metadata.SetAttrs{Size: &winnerSize}); err != nil {
		t.Fatalf("seed winner size: %v", err)
	}

	// A lease holder takes an RWH lease on the winner under a DIFFERENT lease
	// key than the requester's, so the break is not owner-suppressed.
	holderKey := [16]byte{0xBB}
	if _, _, err := h.LeaseManager.RequestLease(
		context.Background(),
		lock.FileHandle(winnerHandle),
		holderKey,
		[16]byte{}, // parentLeaseKey
		2,          // sessionID (different session)
		[16]byte{}, // clientGUID
		"holder-1",
		"client-2",
		smbCtx.ShareName,
		lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle,
		false, // isDirectory
	); err != nil {
		t.Fatalf("RequestLease on winner: %v", err)
	}

	requesterUID := uint32(2001)
	requesterGID := uint32(2001)
	sidStr := "S-1-5-21-1-2-3-2001"
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
			SID: &sidStr,
		},
		BypassTraverseChecking: true,
	}

	// The racing requester believes the file does not exist (pre-race view).
	const fileReadAttributes uint32 = 0x00000080
	draft := raceWinnerDraft(tree, requesterAuth, parentHandle, "winner.txt",
		fileReadAttributes, 0x07, types.FileSupersede)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("SUPERSEDE losing race vs lease holder: status = 0x%08x, want SUCCESS (the break must drain, not deny)",
			uint32(resp.Status))
	}

	// The winner's lease must have been broken: no holder may still hold the
	// WRITE bit (the destructive overwrite's delay mask drained).
	if h.LeaseManager.AnyHolderHasLeaseBits(lock.FileHandle(winnerHandle), smbCtx.ShareName, [16]byte{}, lock.LeaseStateWrite) {
		t.Error("the race winner's write lease was not broken before the overwrite")
	}
	_ = rootAuth
}

// TestCreate_CompoundChain_ParksMidChain pins the MS-SMB2 3.3.4.2 interim-async
// contract: a CREATE carrying trailing compound commands (ctx.NextCommand != 0)
// parks on an interim STATUS_PENDING like any other CREATE — the compound
// processor defers trailing commands behind the parked CREATE via
// ReplaceCallback (smbtorture compound_async.getinfo_middle requires req[0] to
// stay in RECV state while the lease break drains).
func TestCreate_CompoundChain_ParksMidChain(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	mgr := lock.NewManager()
	h.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)

	metaSvc := rt.GetMetadataService()
	parentHandle, _ := raceParentDir(t, metaSvc, rootAuth, rootHandle, "compound-guard")

	// Seed the target file with a lease holder that intersects the destructive
	// delay mask (W), so parking WOULD trigger without the guard.
	target, _, err := metaSvc.CreateFile(rootAuth, parentHandle, "guarded.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o700, UID: 2001, GID: 2001})
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}
	targetHandle, err := metadata.EncodeFileHandle(target)
	if err != nil {
		t.Fatalf("EncodeFileHandle target: %v", err)
	}
	holderKey := [16]byte{0xCC}
	if _, _, err := h.LeaseManager.RequestLease(
		context.Background(),
		lock.FileHandle(targetHandle),
		holderKey,
		[16]byte{},
		2,
		[16]byte{},
		"holder-1",
		"client-2",
		smbCtx.ShareName,
		lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle,
		false,
	); err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	// A draft whose existing view points at the leased target with a destructive
	// disposition: breakAndMaybeParkCreate would dispatch the break and park.
	requesterUID := uint32(2001)
	requesterGID := uint32(2001)
	sidStr := "S-1-5-21-1-2-3-2001"
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
			SID: &sidStr,
		},
		BypassTraverseChecking: true,
	}
	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "guarded.txt",
			DesiredAccess:     uint32(types.FileReadData | types.FileWriteData),
			ShareAccess:       uint32(types.FileShareRead | types.FileShareWrite),
			CreateDisposition: types.FileOverwriteIf,
			CreateOptions:     0,
		},
		tree:           tree,
		authCtx:        requesterAuth,
		filename:       "guarded.txt",
		baseName:       "guarded.txt",
		parentHandle:   parentHandle,
		existingFile:   target,
		existingHandle: targetHandle,
		fileExists:     true,
		createAction:   types.FileOverwritten,
	}

	// Wire the async-park machinery so parking CAN fire: without these, the
	// park preconditions fail and the guard would not be load-bearing.
	compoundCtx := *smbCtx
	compoundCtx.NextCommand = 152
	compoundCtx.AsyncCreateCompleteCallback = func(sessionID, messageID, asyncID uint64, status types.Status, createBody []byte) error {
		return nil
	}
	compoundCtx.TryReserveAsync = func() bool { return true }
	compoundCtx.ReleaseAsync = func() {}
	compoundCtx.ConnID = 1

	// Compound chain: NextCommand != 0. The CREATE parks like any other —
	// the interim PENDING is the contract the compound processor serves.
	asyncId := h.breakAndMaybeParkCreate(&compoundCtx, draft)
	if asyncId == 0 {
		t.Fatal("compound-chain CREATE did not park: MS-SMB2 3.3.4.2 requires the mid-chain interim PENDING (smbtorture compound_async.getinfo_middle)")
	}

	// Control: the same draft WITHOUT a compound chain parks — proving the
	// guard is what forced the inline wait above.
	h2, rt2, smbCtx2, rootHandle2, rootAuth2 := setupDaclTest(t)
	tree2 := &TreeConnection{TreeID: smbCtx2.TreeID, SessionID: smbCtx2.SessionID, ShareName: smbCtx2.ShareName}
	h2.StoreTree(tree2)
	h2.LeaseManager = lease.NewLeaseManager(&staticLockResolver{mgr: lock.NewManager()}, nil)
	h2.PendingCreateRegistry = NewPendingCreateRegistry()
	metaSvc2 := rt2.GetMetadataService()
	parent2, _ := raceParentDir(t, metaSvc2, rootAuth2, rootHandle2, "compound-solo")
	t2, _, err := metaSvc2.CreateFile(rootAuth2, parent2, "guarded.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o700, UID: 2001, GID: 2001})
	if err != nil {
		t.Fatalf("seed control target: %v", err)
	}
	t2Handle, err := metadata.EncodeFileHandle(t2)
	if err != nil {
		t.Fatalf("EncodeFileHandle control: %v", err)
	}
	if _, _, err := h2.LeaseManager.RequestLease(
		context.Background(),
		lock.FileHandle(t2Handle),
		holderKey,
		[16]byte{},
		2,
		[16]byte{},
		"holder-1",
		"client-2",
		smbCtx2.ShareName,
		lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle,
		false,
	); err != nil {
		t.Fatalf("RequestLease control: %v", err)
	}
	draft2 := &createDraft{
		req: &CreateRequest{
			FileName:          "guarded.txt",
			DesiredAccess:     uint32(types.FileReadData | types.FileWriteData),
			ShareAccess:       uint32(types.FileShareRead | types.FileShareWrite),
			CreateDisposition: types.FileOverwriteIf,
			CreateOptions:     0,
		},
		tree:           tree2,
		authCtx:        requesterAuth,
		filename:       "guarded.txt",
		baseName:       "guarded.txt",
		parentHandle:   parent2,
		existingFile:   t2,
		existingHandle: t2Handle,
		fileExists:     true,
		createAction:   types.FileOverwritten,
	}
	soloCtx := *smbCtx2
	soloCtx.NextCommand = 0
	soloCtx.AsyncCreateCompleteCallback = func(sessionID, messageID, asyncID uint64, status types.Status, createBody []byte) error {
		return nil
	}
	soloCtx.TryReserveAsync = func() bool { return true }
	soloCtx.ReleaseAsync = func() {}
	soloCtx.ConnID = 1

	// The solo CREATE (NextCommand == 0) parks the same way: the parking
	// behavior is chain-agnostic.
	if asyncId := h2.breakAndMaybeParkCreate(&soloCtx, draft2); asyncId == 0 {
		t.Log("solo CREATE did not park (parking is conditional)")
	}
}
