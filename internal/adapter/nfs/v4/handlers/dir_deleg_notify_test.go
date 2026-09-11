package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// grantDelegOnRoot registers a confirmed client on the fixture's state manager
// and grants it a directory delegation on the fixture's root handle. Returns
// the delegation so tests can inspect its pending notification queue.
func grantDelegOnRoot(t *testing.T, fx *realFSTestFixture, mask uint32) *state.DelegationState {
	t.Helper()
	result, err := fx.handler.StateManager.SetClientID("dir-deleg-notify-client", [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		state.CallbackInfo{Addr: "127.0.0.1", Program: 0x40000000}, "127.0.0.1")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	if err := fx.handler.StateManager.ConfirmClientID(result.ClientID, result.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	deleg, err := fx.handler.StateManager.GrantDirDelegation(result.ClientID, []byte(fx.rootHandle), mask)
	if err != nil {
		t.Fatalf("GrantDirDelegation: %v", err)
	}
	return deleg
}

// rootContext returns a CompoundContext rooted at the fixture's root handle.
func rootContext(fx *realFSTestFixture) *types.CompoundContext {
	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)
	return ctx
}

// pendingNotifCount snapshots the delegation's pending notification count.
func pendingNotifCount(deleg *state.DelegationState) int {
	deleg.NotifMu.Lock()
	defer deleg.NotifMu.Unlock()
	return len(deleg.PendingNotifs)
}

func TestCreate_NotifiesDirDelegationHolders(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	deleg := grantDelegOnRoot(t, fx, 1<<types.NOTIFY4_ADD_ENTRY)

	result := fx.handler.handleCreate(rootContext(fx), bytes.NewReader(encodeCreateDirArgs("created-dir")))
	if result.Status != types.NFS4_OK {
		t.Fatalf("CREATE status = %d, want NFS4_OK", result.Status)
	}

	if got := pendingNotifCount(deleg); got != 1 {
		t.Fatalf("pending notifications after CREATE = %d, want 1", got)
	}
	deleg.NotifMu.Lock()
	notif := deleg.PendingNotifs[0]
	deleg.NotifMu.Unlock()
	if notif.Type != types.NOTIFY4_ADD_ENTRY {
		t.Errorf("notification type = %d, want NOTIFY4_ADD_ENTRY (%d)", notif.Type, types.NOTIFY4_ADD_ENTRY)
	}
	if notif.EntryName != "created-dir" {
		t.Errorf("entry name = %q, want %q", notif.EntryName, "created-dir")
	}
}

func TestRemove_NotifiesDirDelegationHolders(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	deleg := grantDelegOnRoot(t, fx, 1<<types.NOTIFY4_REMOVE_ENTRY)

	fx.createTestFile(t, fx.rootHandle, "doomed.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	result := fx.handler.handleRemove(rootContext(fx), bytes.NewReader(encodeRemoveArgs("doomed.txt")))
	if result.Status != types.NFS4_OK {
		t.Fatalf("REMOVE status = %d, want NFS4_OK", result.Status)
	}

	if got := pendingNotifCount(deleg); got != 1 {
		t.Fatalf("pending notifications after REMOVE = %d, want 1", got)
	}
	deleg.NotifMu.Lock()
	notif := deleg.PendingNotifs[0]
	deleg.NotifMu.Unlock()
	if notif.Type != types.NOTIFY4_REMOVE_ENTRY {
		t.Errorf("notification type = %d, want NOTIFY4_REMOVE_ENTRY (%d)", notif.Type, types.NOTIFY4_REMOVE_ENTRY)
	}
	if notif.EntryName != "doomed.txt" {
		t.Errorf("entry name = %q, want %q", notif.EntryName, "doomed.txt")
	}
}

func TestRename_NotifiesDirDelegationHolders(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	deleg := grantDelegOnRoot(t, fx, 1<<types.NOTIFY4_RENAME_ENTRY)

	fx.createTestFile(t, fx.rootHandle, "old-name.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	// Same-directory rename: CurrentFH carries the source dir (SAVEFH semantics
	// are satisfied by pointing both at the root).
	ctx := rootContext(fx)
	ctx.SavedFH = make([]byte, len(fx.rootHandle))
	copy(ctx.SavedFH, fx.rootHandle)

	result := fx.handler.handleRename(ctx, bytes.NewReader(encodeRenameArgs("old-name.txt", "new-name.txt")))
	if result.Status != types.NFS4_OK {
		t.Fatalf("RENAME status = %d, want NFS4_OK", result.Status)
	}

	if got := pendingNotifCount(deleg); got != 1 {
		t.Fatalf("pending notifications after RENAME = %d, want 1", got)
	}
	deleg.NotifMu.Lock()
	notif := deleg.PendingNotifs[0]
	deleg.NotifMu.Unlock()
	if notif.Type != types.NOTIFY4_RENAME_ENTRY {
		t.Errorf("notification type = %d, want NOTIFY4_RENAME_ENTRY (%d)", notif.Type, types.NOTIFY4_RENAME_ENTRY)
	}
	if notif.EntryName != "old-name.txt" || notif.NewName != "new-name.txt" {
		t.Errorf("names = %q->%q, want %q->%q", notif.EntryName, notif.NewName, "old-name.txt", "new-name.txt")
	}
}

func TestLink_NotifiesDirDelegationHolders(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	deleg := grantDelegOnRoot(t, fx, 1<<types.NOTIFY4_ADD_ENTRY)

	sourceHandle := fx.createTestFile(t, fx.rootHandle, "source.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := rootContext(fx)
	ctx.SavedFH = make([]byte, len(sourceHandle))
	copy(ctx.SavedFH, sourceHandle)

	result := fx.handler.handleLink(ctx, bytes.NewReader(encodeLinkArgs("linked.txt")))
	if result.Status != types.NFS4_OK {
		t.Fatalf("LINK status = %d, want NFS4_OK", result.Status)
	}

	if got := pendingNotifCount(deleg); got != 1 {
		t.Fatalf("pending notifications after LINK = %d, want 1", got)
	}
	deleg.NotifMu.Lock()
	notif := deleg.PendingNotifs[0]
	deleg.NotifMu.Unlock()
	if notif.Type != types.NOTIFY4_ADD_ENTRY {
		t.Errorf("notification type = %d, want NOTIFY4_ADD_ENTRY (%d)", notif.Type, types.NOTIFY4_ADD_ENTRY)
	}
	if notif.EntryName != "linked.txt" {
		t.Errorf("entry name = %q, want %q", notif.EntryName, "linked.txt")
	}
}
