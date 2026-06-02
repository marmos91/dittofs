package handlers

import (
	"context"
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
	openFile := &OpenFile{
		FileID:      fileID,
		FileName:    "watched-dir",
		Path:        "/share/watched-dir",
		IsDirectory: true,
		ShareName:   "share",
		OplockLevel: OplockLevelNone,
	}
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

	openFile := &OpenFile{
		FileID:      fileID,
		FileName:    "plain-dir",
		Path:        "/share/plain-dir",
		IsDirectory: true,
		ShareName:   "share",
		OplockLevel: OplockLevelNone,
	}
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
