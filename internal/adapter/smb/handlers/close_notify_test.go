package handlers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestClose_PendingNotifyCleanup_DeferredViaPostSend verifies the fix for
// BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close: when a CLOSE is received
// for a directory with a pending CHANGE_NOTIFY watch, the handler MUST NOT
// invoke the async cleanup callback inline. Instead, it must register a
// ctx.PostSend hook so the dispatch layer can deliver the STATUS_NOTIFY_CLEANUP
// response AFTER the CLOSE response has been written.
//
// Per MS-SMB2 3.3.4.1: "CHANGE_NOTIFY responses MUST be the last responses
// sent for the FileId". Violating this ordering causes the Windows Test Suite
// client to miss the cleanup (its async-receive callback is only armed once
// the CLOSE response is consumed).
func TestClose_PendingNotifyCleanup_DeferredViaPostSend(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = NewNotifyRegistry()

	var fileID [16]byte
	copy(fileID[:], []byte{0xab, 0xcd, 0xef, 0x01})

	// Install a directory open file so the CLOSE handler reaches step 10.
	openFile := (&OpenFile{
		FileID:      fileID,
		IsDirectory: true,
		ShareName:   "share",
		OplockLevel: OplockLevelNone,
	}).WithName(OpenName{Path: "/share/watched-dir", FileName: "watched-dir"})
	h.StoreOpenFile(openFile)

	// Register a pending CHANGE_NOTIFY with a callback that flips an atomic
	// flag when invoked. The test asserts the flag is FALSE when Close
	// returns, and TRUE only after ctx.PostSend is invoked.
	var callbackFired atomic.Bool
	var callbackStatus atomic.Uint32
	var callbackAsyncId atomic.Uint64

	notify := &PendingNotify{
		FileID:    fileID,
		SessionID: 42,
		MessageID: 6,
		AsyncId:   14,
		WatchPath: "/share/watched-dir",
		ShareName: "share",
		AsyncCallback: func(sessionID, messageID, asyncId uint64, resp *ChangeNotifyResponse) error {
			callbackFired.Store(true)
			callbackStatus.Store(uint32(resp.GetStatus()))
			callbackAsyncId.Store(asyncId)
			return nil
		},
	}
	if err := h.NotifyRegistry.Register(notify); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "test-client",
		SessionID:  42,
		MessageID:  7,
	}

	req := &CloseRequest{
		FileID: fileID,
		Flags:  0, // no POSTQUERY_ATTRIB, avoids metadata service lookup
	}

	resp, err := h.Close(ctx, req)
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Close: expected StatusSuccess, got %+v", resp)
	}

	// CRITICAL: the async cleanup callback MUST NOT have fired yet. If it
	// did, the cleanup response would race the CLOSE response on the wire
	// and the WPTS client would miss it.
	if callbackFired.Load() {
		t.Fatal("async CHANGE_NOTIFY cleanup callback fired inline during Close; " +
			"must be deferred to ctx.PostSend so it runs AFTER the CLOSE response " +
			"(MS-SMB2 3.3.4.1)")
	}

	// The notify must already be unregistered so a concurrent CANCEL/remove
	// cannot double-fire it.
	if h.NotifyRegistry.WatcherCount() != 0 {
		t.Errorf("notify still registered after Close: want 0 watchers, got %d",
			h.NotifyRegistry.WatcherCount())
	}

	// The handler must have published a PostSend hook on the context so the
	// dispatch layer can deliver the cleanup after writing the CLOSE response.
	if ctx.PostSend == nil {
		t.Fatal("Close did not set ctx.PostSend; dispatch layer cannot deliver " +
			"STATUS_NOTIFY_CLEANUP after the CLOSE response")
	}

	// Simulate the dispatch layer invoking PostSend after the CLOSE response
	// has been written. This must trigger the cleanup callback exactly once
	// with STATUS_NOTIFY_CLEANUP on the original AsyncId.
	ctx.PostSend()

	if !callbackFired.Load() {
		t.Fatal("PostSend did not trigger the cleanup callback")
	}
	if got := types.Status(callbackStatus.Load()); got != types.StatusNotifyCleanup {
		t.Errorf("cleanup callback status = 0x%08x, want STATUS_NOTIFY_CLEANUP (0x%08x)",
			uint32(got), uint32(types.StatusNotifyCleanup))
	}
	if got := callbackAsyncId.Load(); got != 14 {
		t.Errorf("cleanup callback AsyncId = %d, want 14", got)
	}
}

// TestClose_NoPendingNotify_PostSendNil ensures we don't set PostSend for
// ordinary CLOSE calls (no watcher registered), so the dispatch layer has
// nothing extra to do and the common path stays zero-overhead.
func TestClose_NoPendingNotify_PostSendNil(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = NewNotifyRegistry()

	var fileID [16]byte
	copy(fileID[:], []byte{0x11, 0x22, 0x33, 0x44})

	openFile := (&OpenFile{
		FileID:      fileID,
		IsDirectory: true,
		ShareName:   "share",
		OplockLevel: OplockLevelNone,
	}).WithName(OpenName{Path: "/share/plain-dir", FileName: "plain-dir"})
	h.StoreOpenFile(openFile)

	ctx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "test-client",
		SessionID:  1,
		MessageID:  2,
	}
	req := &CloseRequest{FileID: fileID, Flags: 0}

	resp, err := h.Close(ctx, req)
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Close: expected StatusSuccess, got %+v", resp)
	}
	if ctx.PostSend != nil {
		t.Error("ctx.PostSend should be nil when there is no pending CHANGE_NOTIFY")
	}
}

// TestClose_DeleteOnClose_CompletesOtherHandlesNotify covers the smbtorture
// smb2.notify.rmdir1-4 shape: one handle watches a directory while a second
// handle on the SAME directory is closed carrying a delete-on-close. The
// watcher's own handle is untouched, so nothing on the close path answers it
// unless the delete mark itself does — [MS-FSA] 2.1.5.15.3 requires
// STATUS_DELETE_PENDING.
func TestClose_DeleteOnClose_CompletesOtherHandlesNotify(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = NewNotifyRegistry()

	metaHandle := []byte{0x11, 0x22, 0x33}
	watcherID := [16]byte{0xa1}
	deleterID := [16]byte{0xb2}

	watcher := (&OpenFile{
		FileID:         watcherID,
		IsDirectory:    true,
		ShareName:      "share",
		MetadataHandle: metaHandle,
		OplockLevel:    OplockLevelNone,
	}).WithName(OpenName{Path: "/watched", FileName: "watched", ParentHandle: []byte{0x01}})
	h.StoreOpenFile(watcher)

	// The closing handle asked for FILE_DELETE_ON_CLOSE at CREATE, which
	// DittoFS promotes to the shared delete-pending flag in the election.
	deleter := (&OpenFile{
		FileID:               deleterID,
		IsDirectory:          true,
		ShareName:            "share",
		MetadataHandle:       metaHandle,
		InitialDeleteOnClose: true,
		OplockLevel:          OplockLevelNone,
	}).WithName(OpenName{Path: "/watched", FileName: "watched", ParentHandle: []byte{0x01}})
	h.StoreOpenFile(deleter)

	var gotStatus atomic.Uint32
	var fired atomic.Bool
	if err := h.NotifyRegistry.Register(&PendingNotify{
		FileID:           watcherID,
		SessionID:        42,
		MessageID:        6,
		AsyncId:          14,
		WatchPath:        "/watched",
		ShareName:        "share",
		CompletionFilter: FileNotifyChangeFileName | FileNotifyChangeDirName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, resp *ChangeNotifyResponse) error {
			fired.Store(true)
			gotStatus.Store(uint32(resp.GetStatus()))
			return nil
		},
	}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	ctx := &SMBHandlerContext{Context: context.Background(), SessionID: 42, MessageID: 7}
	resp, err := h.Close(ctx, &CloseRequest{FileID: deleterID})
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if resp == nil || resp.GetStatus() != types.StatusSuccess {
		t.Fatalf("Close: expected StatusSuccess, got %+v", resp)
	}

	if !fired.Load() {
		t.Fatal("the watcher's CHANGE_NOTIFY was never answered: a client watching a " +
			"directory another handle marked for deletion waits until its own transport " +
			"gives up (MS-FSA 2.1.5.15.3)")
	}
	if got := types.Status(gotStatus.Load()); got != types.StatusDeletePending {
		t.Errorf("expected STATUS_DELETE_PENDING (0x%08X), got 0x%08X",
			uint32(types.StatusDeletePending), uint32(got))
	}
	if h.NotifyRegistry.WatcherCount() != 0 {
		t.Errorf("watch survived the delete mark: %d", h.NotifyRegistry.WatcherCount())
	}
}

// TestClose_DirectoryDeleteOnClose_RetiresNotifyMarker walks the marker's whole
// life through the real handlers: SET_INFO commits the delete disposition, a
// CHANGE_NOTIFY arriving after that is answered rather than parked, and the
// CLOSE that actually removes the entry frees the name to be watched again.
//
// The clear is deliberately not wired to the close paths' own sweep: both of
// them remove the directory entry before they sweep, so a marker recorded there
// would be stamped onto a name that has just been freed.
func TestClose_DirectoryDeleteOnClose_RetiresNotifyMarker(t *testing.T) {
	h, ctx, _ := setupStreamsDisabledShare(t, false)
	if h.NotifyRegistry == nil {
		h.NotifyRegistry = NewNotifyRegistry()
	}

	dirID := dirTestCreate(t, h, ctx, "doomed", types.FileOpenIf, types.FileDirectoryFile)
	openFile, ok := h.GetOpenFile(dirID)
	if !ok {
		t.Fatal("directory handle missing from the open-file table")
	}
	shareName, watchPath := openFile.ShareName, openFile.Name().Path

	lateNotify := func(messageID uint64) *PendingNotify {
		return &PendingNotify{
			FileID:           [16]byte{0x51, byte(messageID)},
			SessionID:        ctx.SessionID,
			MessageID:        messageID,
			AsyncId:          messageID * 10,
			WatchPath:        watchPath,
			ShareName:        shareName,
			CompletionFilter: FileNotifyChangeFileName,
			MaxOutputLength:  4096,
			AsyncCallback:    func(uint64, uint64, uint64, *ChangeNotifyResponse) error { return nil },
		}
	}

	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileDispositionInformation),
		FileID:        dirID,
		Buffer:        encodeDispositionInfo(true),
	})
	if err != nil {
		t.Fatalf("SetInfo disposition: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("disposition on an empty directory = %v, want STATUS_SUCCESS", resp.Status)
	}

	if err := h.NotifyRegistry.Register(lateNotify(11)); !errors.Is(err, ErrDirectoryDeletePending) {
		t.Fatalf("a watch registering after the mark: got %v, want ErrDirectoryDeletePending", err)
	}

	if _, err := h.Close(ctx, &CloseRequest{FileID: dirID}); err != nil {
		t.Fatalf("close doomed: %v", err)
	}

	if err := h.NotifyRegistry.Register(lateNotify(12)); err != nil {
		t.Fatalf("once the entry is gone the name must be watchable again: %v", err)
	}
}
