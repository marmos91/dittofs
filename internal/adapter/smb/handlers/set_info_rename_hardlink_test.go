package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// encodeFileRenameInfoWire serialises a FILE_RENAME_INFORMATION blob. MS-FSCC
// 2.4.42.2 (FileRenameInformation for the SMB2 Protocol) and 2.4.28.2 share a
// field-for-field identical layout, so the link encoder produces both.
func encodeFileRenameInfoWire(t *testing.T, replaceIfExists bool, rootDir [8]byte, fileName string) []byte {
	t.Helper()
	return encodeFileLinkInfoWire(t, replaceIfExists, rootDir, fileName)
}

// renameWatcher arms reg with a watcher on the share root and reports through
// the returned pointer whether a name-change notification reached it. Delivery
// is buffered, so callers must FlushAll before reading the flag.
func renameWatcher(t *testing.T, reg *NotifyRegistry) *bool {
	t.Helper()
	notified := false
	mustRegister(t, reg, &PendingNotify{
		FileID:           [16]byte{0x9E},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/",
		ShareName:        hardlinkTestShareName,
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})
	return &notified
}

// TestSetInfo_Rename_OntoOwnHardLink pins that renaming a file onto a name
// that is already another hard link to it changes nothing a client can see.
// The metadata layer refuses to move one link over the other — destroying a
// link is not a rename — and returns success without touching the store, so
// the handler must not follow that success with a rename notification telling
// watchers the source name disappeared, nor repoint the open handle at a name
// the rename never gave it.
//
// The sibling control below renames onto a name that does not exist yet and
// asserts the opposite on the same fixture: without it a broken watcher or a
// missing DELETE grant would let this test pass while pinning nothing.
func TestSetInfo_Rename_OntoOwnHardLink(t *testing.T) {
	rt, rootHandle, authCtx := newHardlinkTestShare(t)
	ctx := authCtx.Context
	metaSvc := rt.GetMetadataService()

	handle, _ := createHardlinkTestFile(t, rt, authCtx, rootHandle, "a.txt", 1024)
	if _, err := metaSvc.CreateHardLink(authCtx, rootHandle, "b.txt", handle); err != nil {
		t.Fatalf("CreateHardLink b.txt: %v", err)
	}

	h, open := openHardlinkTestFile(t, rt, rootHandle, handle, "a.txt")
	open.GrantedAccess = uint32(types.Delete)
	h.NotifyRegistry = newTestNotifyRegistry()
	notified := renameWatcher(t, h.NotifyRegistry)

	// Rename a.txt onto b.txt, which is the same inode reached by its other
	// link. Exact case, so the case-mismatch pre-remove branch stays out.
	buf := encodeFileRenameInfoWire(t, true, [8]byte{}, "b.txt")
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileRenameInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore(rename onto own link): err=%v resp=%v", err, resp)
	}

	h.NotifyRegistry.FlushAll()
	if *notified {
		t.Error("watcher told a.txt was renamed away, but both links still resolve")
	}

	if got := open.Name().FileName; got != "a.txt" {
		t.Errorf("open handle renamed to %q by a rename that moved nothing", got)
	}

	for _, name := range []string{"a.txt", "b.txt"} {
		child, cErr := metaSvc.GetChild(ctx, rootHandle, name)
		if cErr != nil {
			t.Fatalf("%s no longer resolves after renaming a.txt onto its own link: %v", name, cErr)
		}
		if !bytes.Equal(child, handle) {
			t.Errorf("%s resolves to a different file after the rename", name)
		}
	}
}

// TestSetInfo_Rename_DistinctDestination is the control for the test above: on
// the same fixture, a rename to a name that is not already a link to the
// source must still notify watchers and repoint the handle.
func TestSetInfo_Rename_DistinctDestination(t *testing.T) {
	rt, rootHandle, authCtx := newHardlinkTestShare(t)
	ctx := authCtx.Context
	metaSvc := rt.GetMetadataService()

	handle, _ := createHardlinkTestFile(t, rt, authCtx, rootHandle, "a.txt", 1024)

	h, open := openHardlinkTestFile(t, rt, rootHandle, handle, "a.txt")
	open.GrantedAccess = uint32(types.Delete)
	h.NotifyRegistry = newTestNotifyRegistry()
	notified := renameWatcher(t, h.NotifyRegistry)

	buf := encodeFileRenameInfoWire(t, true, [8]byte{}, "c.txt")
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileRenameInformation, buf)
	if err != nil || resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("setFileInfoFromStore(rename): err=%v resp=%v", err, resp)
	}

	h.NotifyRegistry.FlushAll()
	if !*notified {
		t.Error("watcher missed a rename that did move the file")
	}

	if got := open.Name().FileName; got != "c.txt" {
		t.Errorf("open handle still named %q after the file was renamed to c.txt", got)
	}

	if _, cErr := metaSvc.GetChild(ctx, rootHandle, "a.txt"); cErr == nil {
		t.Error("a.txt still resolves after being renamed to c.txt")
	}
	child, cErr := metaSvc.GetChild(ctx, rootHandle, "c.txt")
	if cErr != nil {
		t.Fatalf("c.txt does not resolve after the rename: %v", cErr)
	}
	if !bytes.Equal(child, handle) {
		t.Error("c.txt resolves to a different file than the one renamed")
	}
}
