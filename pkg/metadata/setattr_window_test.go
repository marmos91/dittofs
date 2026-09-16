package metadata_test

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// TestSetFileAttributes_SizeCommittedInWindowSurvivesChmod pins that
// SetFileAttributes writes the row its own transaction read, not the copy it
// took before opening one.
//
// A chmod changes one column. Every other column on the row it writes must
// therefore come from committed state: writing back the earlier snapshot
// restores whatever that snapshot held, here a Size a WRITE has since
// superseded, which is a lost update rather than a chmod.
//
// The distinction is invisible unless a writer commits between the two reads,
// so the store hook places one exactly there. Asserting on Size rather than
// Mode is deliberate — the chmod is entitled to move Mode, so Mode cannot show
// where the rest of the row came from.
//
// No isolation level closes this one: the stale copy is already in hand before
// the transaction opens, so on every backend the fix has to be the re-read.
func TestSetFileAttributes_SizeCommittedInWindowSurvivesChmod(t *testing.T) {
	ws := &windowStore{SQLiteMetadataStore: newSQLiteRenameStore(t)}
	svc, rootHandle, share := registerRenameStore(t, ws)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "c.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	const grown = uint64(4096)
	var mid *metadata.File
	ws.beforeTx = func() {
		size := grown
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Size: &size})
		require.NoError(t, hookErr)
		mid, hookErr = svc.GetFile(root.Context, handle)
		require.NoError(t, hookErr)
	}

	newMode := uint32(0o600)
	wcc, err := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mode: &newMode})
	require.NoError(t, err)
	require.NotNil(t, mid, "hook did not fire: SetFileAttributes opened no transaction through the wrapper")
	require.Equal(t, grown, mid.Size, "precondition: the injected size must have committed")

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.Equal(t, grown, after.Size,
		"Size = %d; want the %d committed inside the chmod's own window — SetFileAttributes wrote "+
			"back the inode snapshot it read before opening its transaction, discarding the newer size",
		after.Size, grown)
	require.Equal(t, newMode, after.Mode&0o777, "the chmod itself must still land")
	require.NotNil(t, wcc.After)
	require.Equal(t, grown, wcc.After.Size, "post-op attrs must describe the row that was written")
}

// TestSetFileAttributes_MtimeCommittedInWindowSurvivesChown is the same shape
// on a different column, because Mtime is the one a chmod/chown is most likely
// to revert in practice: the NFS client sends SETATTR(mode) to clear SUID right
// before a WRITE, and the WRITE's mtime is what gets rolled back.
func TestSetFileAttributes_MtimeCommittedInWindowSurvivesChown(t *testing.T) {
	ws := &windowStore{SQLiteMetadataStore: newSQLiteRenameStore(t)}
	svc, rootHandle, share := registerRenameStore(t, ws)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "m.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)

	advanced := time.Now().Add(120 * time.Second)
	var mid *metadata.File
	ws.beforeTx = func() {
		m := advanced
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mtime: &m})
		require.NoError(t, hookErr)
		mid, hookErr = svc.GetFile(root.Context, handle)
		require.NoError(t, hookErr)
	}

	newGID := uint32(42)
	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{GID: &newGID})
	require.NoError(t, err)
	require.NotNil(t, mid, "hook did not fire: SetFileAttributes opened no transaction through the wrapper")

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.True(t, after.Mtime.Equal(mid.Mtime),
		"Mtime = %v; want the %v committed inside the chown's own window — the chown wrote back a "+
			"snapshot taken before it",
		after.Mtime.UTC(), mid.Mtime.UTC())
	require.Equal(t, newGID, after.GID, "the chown itself must still land")
}

// TestSetFileAttributes_ACLCommittedInWindowSurvivesChmod pins that a chmod
// adjusts the ACL the row holds, not the copy it read before its transaction.
//
// A chmod is entitled to rewrite the OWNER@/GROUP@/EVERYONE@ ACEs to match the
// new mode, and nothing else. Recomputing that from the pre-transaction copy
// and writing the result over the row replaces the whole ACL, so an ACL a peer
// committed in the window disappears — and when the chmod's own read saw no
// ACL at all, it is replaced with nothing.
func TestSetFileAttributes_ACLCommittedInWindowSurvivesChmod(t *testing.T) {
	ws := &windowStore{SQLiteMetadataStore: newSQLiteRenameStore(t)}
	svc, rootHandle, share := registerRenameStore(t, ws)
	root := rootAuth()

	created, _, err := svc.CreateFile(root, rootHandle, "a.bin",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(share, created.ID)
	require.NoError(t, err)
	require.Nil(t, created.ACL, "precondition: the file starts with no ACL")

	injected := &acl.ACL{ACEs: []acl.ACE{{
		Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
		AccessMask: acl.ACE4_READ_DATA,
		Who:        "auditor@localdomain",
	}}}
	var mid *metadata.File
	ws.beforeTx = func() {
		_, hookErr := svc.SetFileAttributes(root, handle, &metadata.SetAttrs{ACL: injected})
		require.NoError(t, hookErr)
		mid, hookErr = svc.GetFile(root.Context, handle)
		require.NoError(t, hookErr)
	}

	newMode := uint32(0o600)
	_, err = svc.SetFileAttributes(root, handle, &metadata.SetAttrs{Mode: &newMode})
	require.NoError(t, err)
	require.NotNil(t, mid, "hook did not fire: SetFileAttributes opened no transaction through the wrapper")
	require.NotNil(t, mid.ACL, "precondition: the injected ACL must have committed")

	after, err := svc.GetFile(root.Context, handle)
	require.NoError(t, err)
	require.NotNil(t, after.ACL,
		"the ACL committed inside the chmod's own window was wiped — the chmod wrote back an ACL "+
			"derived from the copy it read before opening its transaction, which had none")

	var found bool
	for _, ace := range after.ACL.ACEs {
		if ace.Who == "auditor@localdomain" {
			found = true
		}
	}
	require.True(t, found,
		"the non-mode ACE committed inside the chmod's window is gone; a chmod may rewrite the "+
			"OWNER@/GROUP@/EVERYONE@ entries and nothing else, ACEs=%+v", after.ACL.ACEs)
	require.Equal(t, newMode, after.Mode&0o777, "the chmod itself must still land")
}

// TestMove_SizeCommittedInWindowSurvivesOverwritingRename pins that an
// overwriting rename stamps the victim's ChangeTime on the row its own
// transaction read, not on the copy Move took before opening one.
//
// Move reads the destination inode outside the transaction to run the sticky-bit
// and type checks, and the only column it means to change on that row is Ctime.
// Writing the earlier copy back therefore reverts every other column to what the
// check-time read saw — here a Size a WRITE has since committed. The victim has
// a second hard link so the row outlives the rename and can be read back; with
// no surviving link the inode is gone and the revert is unobservable rather than
// absent.
//
// No isolation level closes this one either: the stale copy is in hand before
// the transaction opens, so the transaction's own snapshot already contains the
// write and the update conflicts with nothing.
func TestMove_SizeCommittedInWindowSurvivesOverwritingRename(t *testing.T) {
	ws := &windowStore{SQLiteMetadataStore: newSQLiteRenameStore(t)}
	svc, rootHandle, share := registerRenameStore(t, ws)
	root := rootAuth()

	victim, _, err := svc.CreateFile(root, rootHandle, "dst.bin", &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)
	victimHandle, err := metadata.EncodeShareHandle(share, victim.ID)
	require.NoError(t, err)
	// A second name keeps the inode alive across the rename that unlinks
	// "dst.bin", so the committed row can be read back afterwards.
	_, err = svc.CreateHardLink(root, rootHandle, "survivor.bin", victimHandle)
	require.NoError(t, err)

	_, _, err = svc.CreateFile(root, rootHandle, "src.bin", &metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666})
	require.NoError(t, err)

	const grown = uint64(4096)
	hookFired := false
	ws.beforeTx = func() {
		hookFired = true
		size := grown
		_, hookErr := svc.SetFileAttributes(root, victimHandle, &metadata.SetAttrs{Size: &size})
		require.NoError(t, hookErr)
	}

	_, _, err = svc.Move(root, rootHandle, "src.bin", rootHandle, "dst.bin")
	require.NoError(t, err)
	require.True(t, hookFired, "the window hook never fired, so nothing committed inside Move's window")

	after, err := svc.GetFile(root.Context, victimHandle)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, grown, after.Size, "the overwriting rename reverted the victim's Size to the copy it read before its transaction")
}
