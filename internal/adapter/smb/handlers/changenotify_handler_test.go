package handlers

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/changenotify"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestChangeNotify_HandlePermissions_GrantedAccessGate mirrors the smbtorture
// smb2.notify.handle-permissions test (source4/torture/smb2/notify.c::
// torture_smb2_notify_handle_permissions): a directory handle opened with only
// FILE_READ_ATTRIBUTES (no FILE_LIST_DIRECTORY) MUST reject CHANGE_NOTIFY
// with STATUS_ACCESS_DENIED per MS-SMB2 §3.3.5.19 / Samba
// source3/smbd/notify.c::change_notify_create (check_any_access_fsp with
// SEC_DIR_LIST).
func TestChangeNotify_HandlePermissions_GrantedAccessGate(t *testing.T) {
	const (
		fileReadAttributes uint32 = 0x00000080 // SEC_FILE_READ_ATTRIBUTE
		fileListDirectory  uint32 = 0x00000001 // SEC_DIR_LIST
	)
	fileID := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}
	const treeID uint32 = 1
	const sessionID uint64 = 42

	cases := []struct {
		name          string
		grantedAccess uint32
		desiredAccess uint32
		wantStatus    types.Status
	}{
		{
			name:          "ReadAttributesOnly_Denied",
			grantedAccess: fileReadAttributes,
			desiredAccess: fileReadAttributes,
			wantStatus:    types.StatusAccessDenied,
		},
		{
			name:          "ListDirectory_Allowed",
			grantedAccess: fileListDirectory | fileReadAttributes,
			desiredAccess: fileListDirectory | fileReadAttributes,
			wantStatus:    types.StatusPending,
		},
		{
			// Regression: an open whose DesiredAccess carries
			// FILE_LIST_DIRECTORY but whose DACL-resolved GrantedAccess
			// stripped it (per-bit intersection at CREATE, MS-SMB2
			// §3.3.5.9 paragraph 8) must still be rejected. The pre-fix
			// gate consulted DesiredAccess and silently let this through.
			name:          "DesiredHasListDir_GrantedDoesNot_Denied",
			grantedAccess: fileReadAttributes,
			desiredAccess: fileListDirectory | fileReadAttributes,
			wantStatus:    types.StatusAccessDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler()

			h.StoreOpenFile((&OpenFile{
				FileID:        fileID,
				TreeID:        treeID,
				SessionID:     sessionID,
				ShareName:     "share1",
				DesiredAccess: tc.desiredAccess,
				GrantedAccess: tc.grantedAccess,
				IsDirectory:   true,
			}).WithName(OpenName{Path: "/HPERM"}))

			ctx := &SMBHandlerContext{
				SessionID:       sessionID,
				TreeID:          treeID,
				MessageID:       100,
				TryReserveAsync: func() bool { return true },
				ReleaseAsync:    func() {},
			}

			body := encodeChangeNotifyReq(changenotify.SMB2WatchTree, 1000, fileID, changenotify.FileNotifyChangeFileName|changenotify.FileNotifyChangeDirName)

			result, err := h.ChangeNotify(ctx, body)
			if err != nil {
				t.Fatalf("ChangeNotify returned error: %v", err)
			}
			if result == nil {
				t.Fatal("ChangeNotify returned nil result")
			}
			if result.Status != tc.wantStatus {
				t.Errorf("status = 0x%08x, want 0x%08x", uint32(result.Status), uint32(tc.wantStatus))
			}

			// On ACCESS_DENIED no watcher must have been registered (also
			// guarantees no async slot was reserved beyond the pre-check).
			watchers := h.NotifyRegistry.WatcherCount()
			if tc.wantStatus == types.StatusAccessDenied && watchers != 0 {
				t.Errorf("expected zero pending watchers after ACCESS_DENIED, got %d", watchers)
			}
			if tc.wantStatus == types.StatusPending && watchers != 1 {
				t.Errorf("expected one pending watcher after STATUS_PENDING, got %d", watchers)
			}
		})
	}
}

// TestChangeNotify_StickyMaxBufferSize_SubsumesValidReq is the unit-level
// cover for smb2.notify.valid-req's "if the first notify returns
// NOTIFY_ENUM_DIR, all do" property. Per Samba `change_notify_create` the
// notify_buffer's max_buffer_size is captured from the FIRST notify on the
// handle and MIN-capped into every subsequent reply. A small first call
// therefore caps every later call on the same handle — even when the later
// call requests max_trans_size.
func TestChangeNotify_StickyMaxBufferSize_SubsumesValidReq(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = changenotify.NewNotifyRegistry()
	h.MaxTransactSize = 1 << 20

	var fileID [16]byte
	copy(fileID[:], []byte{0x77, 0x88})

	openFile := (&OpenFile{
		FileID:        fileID,
		IsDirectory:   true,
		ShareName:     "share1",
		SessionID:     1,
		TreeID:        1,
		DesiredAccess: 0x00000001, // FILE_LIST_DIRECTORY
		GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/dir"})
	h.StoreOpenFile(openFile)

	makeCtx := func() *SMBHandlerContext {
		return &SMBHandlerContext{
			SessionID:       1,
			TreeID:          1,
			MessageID:       1,
			ConnID:          1,
			TryReserveAsync: func() bool { return true },
			ReleaseAsync:    func() {},
			AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
				return nil
			},
		}
	}

	// First CHANGE_NOTIFY with a tiny buffer (1 byte). The handler must
	// accept it and store NotifyMaxBufferSize = 1 on the OpenFile.
	body1 := encodeChangeNotifyReq(0, 1, fileID, changenotify.FileNotifyChangeFileName)
	res1, err := h.ChangeNotify(makeCtx(), body1)
	if err != nil {
		t.Fatalf("first CHANGE_NOTIFY error: %v", err)
	}
	if res1 == nil || res1.Status != types.StatusPending {
		t.Fatalf("first CHANGE_NOTIFY: want STATUS_PENDING, got %+v", res1)
	}
	if got, set := openFile.NotifyMaxBufferSizeValue(); !set || got != 1 {
		t.Fatalf("NotifyMaxBufferSize after first call = (%d, set=%v), want (1, true)", got, set)
	}

	// Drain the registered watcher so the second CHANGE_NOTIFY can register
	// a fresh one (Register replaces same-FileID entries).
	h.NotifyRegistry.Unregister(fileID)

	// Second CHANGE_NOTIFY with max_trans_size — must NOT be rejected as
	// "previously-accepted requests" and must be MIN-capped down to 1 so
	// any encoded change overflows and yields STATUS_NOTIFY_ENUM_DIR.
	body2 := encodeChangeNotifyReq(0, h.MaxTransactSize, fileID, changenotify.FileNotifyChangeFileName|changenotify.FileNotifyChangeDirName)
	res2, err := h.ChangeNotify(makeCtx(), body2)
	if err != nil {
		t.Fatalf("second CHANGE_NOTIFY error: %v", err)
	}
	if res2 == nil || res2.Status != types.StatusPending {
		t.Fatalf("second CHANGE_NOTIFY: want STATUS_PENDING (not InvalidParameter), got %+v", res2)
	}
	if got, set := openFile.NotifyMaxBufferSizeValue(); !set || got != 1 {
		t.Fatalf("NotifyMaxBufferSize after second call = (%d, set=%v), want (1, true) (stuck)", got, set)
	}

	// The pending notify must carry the MIN-capped MaxOutputLength, not the
	// request's max_trans_size — this is what guarantees overflow on
	// delivery and matches Samba `change_notify_reply` MIN semantics.
	var pendingMax uint32
	h.NotifyRegistry.RangeWatchers(func(p *changenotify.PendingNotify) bool {
		if p.FileID == fileID {
			pendingMax = p.MaxOutputLength
		}
		return true
	})
	if pendingMax != 1 {
		t.Errorf("registered PendingNotify.MaxOutputLength = %d, want 1 (MIN-capped to first call's value)", pendingMax)
	}
}

// TestChangeNotify_FirstLargeBuffer_ThenSmallUsesRequest verifies the
// inverse: when the first notify uses a large buffer, a subsequent notify
// with a smaller request honors the smaller value (no upward cap, the cap
// is asymmetric — Samba `MIN(max_param, notify_buf->max_buffer_size)`).
func TestChangeNotify_FirstLargeBuffer_ThenSmallUsesRequest(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = changenotify.NewNotifyRegistry()
	h.MaxTransactSize = 1 << 20

	fileID := [16]byte{0x11}
	openFile := (&OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1", SessionID: 1, TreeID: 1, DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/dir"})
	h.StoreOpenFile(openFile)

	makeCtx := func() *SMBHandlerContext {
		return &SMBHandlerContext{
			SessionID: 1, TreeID: 1, MessageID: 1, ConnID: 1,
			TryReserveAsync: func() bool { return true },
			ReleaseAsync:    func() {},
			AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
				return nil
			},
		}
	}

	// First call: 65536 byte buffer.
	body1 := encodeChangeNotifyReq(0, 65536, fileID, changenotify.FileNotifyChangeFileName)
	if _, err := h.ChangeNotify(makeCtx(), body1); err != nil {
		t.Fatalf("first CHANGE_NOTIFY error: %v", err)
	}
	h.NotifyRegistry.Unregister(fileID)

	// Second call: 256 byte buffer — smaller than stored, must be used as-is.
	body2 := encodeChangeNotifyReq(0, 256, fileID, changenotify.FileNotifyChangeFileName)
	if _, err := h.ChangeNotify(makeCtx(), body2); err != nil {
		t.Fatalf("second CHANGE_NOTIFY error: %v", err)
	}

	var pendingMax uint32
	h.NotifyRegistry.RangeWatchers(func(p *changenotify.PendingNotify) bool {
		if p.FileID == fileID {
			pendingMax = p.MaxOutputLength
		}
		return true
	})
	if pendingMax != 256 {
		t.Errorf("PendingNotify.MaxOutputLength = %d, want 256 (request smaller than stored max)", pendingMax)
	}
	if got, set := openFile.NotifyMaxBufferSizeValue(); !set || got != 65536 {
		t.Errorf("NotifyMaxBufferSize must not be updated by later calls; got (%d, set=%v), want (65536, true)", got, set)
	}
}

// TestChangeNotify_FirstZeroBuffer_StickyAtZero pins the OutputBufferLength=0
// edge case. SMB2 CHANGE_NOTIFY permits OutputBufferLength=0 as a valid
// request; the per-handle "first wins" max_buffer_size must remember that
// zero and cap every later notify at zero (so even a max_trans_size follow-up
// overflows immediately, matching Samba `change_notify_create` semantics).
//
// The old encoding used 0 as the "unset" sentinel and would silently let a
// later large request overwrite the captured cap — breaking the sticky
// invariant. Guards against that regression.
func TestChangeNotify_FirstZeroBuffer_StickyAtZero(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = changenotify.NewNotifyRegistry()
	h.MaxTransactSize = 1 << 20

	fileID := [16]byte{0x99}
	openFile := (&OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1", SessionID: 1, TreeID: 1, DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/dir"})
	h.StoreOpenFile(openFile)

	makeCtx := func() *SMBHandlerContext {
		return &SMBHandlerContext{
			SessionID: 1, TreeID: 1, MessageID: 1, ConnID: 1,
			TryReserveAsync: func() bool { return true },
			ReleaseAsync:    func() {},
			AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
				return nil
			},
		}
	}

	// First CHANGE_NOTIFY: OutputBufferLength = 0 — returns ENUM_DIR
	// synchronously without registering a watcher (buffer=0 fast path).
	body1 := encodeChangeNotifyReq(0, 0, fileID, changenotify.FileNotifyChangeFileName)
	res1, err := h.ChangeNotify(makeCtx(), body1)
	if err != nil {
		t.Fatalf("first CHANGE_NOTIFY (OutputBufferLength=0) error: %v", err)
	}
	if res1.Status != types.StatusNotifyEnumDir {
		t.Fatalf("first CHANGE_NOTIFY status = 0x%08X, want STATUS_NOTIFY_ENUM_DIR", res1.Status)
	}

	// The capture MUST be recorded even though the value is zero.
	got, set := openFile.NotifyMaxBufferSizeValue()
	if !set {
		t.Fatal("NotifyMaxBufferSize was not marked set after first CHANGE_NOTIFY with OutputBufferLength=0")
	}
	if got != 0 {
		t.Fatalf("NotifyMaxBufferSize after first call = %d, want 0", got)
	}

	// Second CHANGE_NOTIFY: max_trans_size buffer. The sticky cap MUST clamp
	// effectiveMax to zero, causing another synchronous ENUM_DIR (no watcher
	// registered). This matches Samba: buffer=0 is immediate ENUM_DIR.
	body2 := encodeChangeNotifyReq(0, h.MaxTransactSize, fileID, changenotify.FileNotifyChangeFileName)
	res2, err := h.ChangeNotify(makeCtx(), body2)
	if err != nil {
		t.Fatalf("second CHANGE_NOTIFY error: %v", err)
	}
	if res2.Status != types.StatusNotifyEnumDir {
		t.Fatalf("second CHANGE_NOTIFY status = 0x%08X, want STATUS_NOTIFY_ENUM_DIR (sticky zero)", res2.Status)
	}

	got, set = openFile.NotifyMaxBufferSizeValue()
	if !set || got != 0 {
		t.Fatalf("NotifyMaxBufferSize after second call = (%d, set=%v), want (0, true) — sticky-zero broken", got, set)
	}

	if h.NotifyRegistry.WatcherCount() != 0 {
		t.Fatal("buffer=0 fast path should NOT register a watcher")
	}
}

// TestChangeNotify_PreArrivalCancel_HandlerReturnsCancelledSync is the
// end-to-end regression: invoke the handler with a tombstone already in
// place and confirm it returns STATUS_CANCELLED synchronously rather than
// STATUS_PENDING. This is what unblocks the in-flight smbtorture client.
func TestChangeNotify_PreArrivalCancel_HandlerReturnsCancelledSync(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = changenotify.NewNotifyRegistry()
	h.MaxTransactSize = 1 << 20

	fileID := [16]byte{0x42}
	openFile := (&OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1", SessionID: 1, TreeID: 1,
		DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/dir"})
	h.StoreOpenFile(openFile)

	ctx := &SMBHandlerContext{
		SessionID: 1, TreeID: 1, MessageID: 77, ConnID: 5,
		TryReserveAsync: func() bool { return true },
		ReleaseAsync:    func() {},
		AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
			return nil
		},
	}

	// CANCEL arrived ahead of us.
	h.NotifyRegistry.CancelByMessageID(ctx.ConnID, ctx.MessageID)

	body := encodeChangeNotifyReq(0, 1000, fileID, changenotify.FileNotifyChangeFileName)
	res, err := h.ChangeNotify(ctx, body)
	if err != nil {
		t.Fatalf("ChangeNotify error: %v", err)
	}
	if res == nil {
		t.Fatal("ChangeNotify returned nil result")
	}
	if res.Status != types.StatusCancelled {
		t.Fatalf("ChangeNotify status = %v, want STATUS_CANCELLED", res.Status)
	}
	if res.AsyncId != 0 {
		t.Errorf("ChangeNotify AsyncId = %d on cancelled sync reply, want 0", res.AsyncId)
	}
	if got := h.NotifyRegistry.WatcherCount(); got != 0 {
		t.Errorf("WatcherCount after cancelled CHANGE_NOTIFY = %d, want 0", got)
	}
}

// TestExpireSessionNotifies_CompletesPendingNotify verifies that an expired
// Kerberos session completes its outstanding async CHANGE_NOTIFY with
// STATUS_CANCELLED so the client's smb2_notify_recv unblocks (smbtorture
// smb2.session.expire2s / expire2e: session.c:1641 expects NT_STATUS_CANCELLED
// for the cancelled notify). The flush must be idempotent (the test fires
// several expired requests in the same window) and must not touch other
// sessions' watchers.
func TestExpireSessionNotifies_CompletesPendingNotify(t *testing.T) {
	r := changenotify.NewNotifyRegistry()
	h := &Handler{NotifyRegistry: r}

	var calls int
	var gotStatus types.Status
	mustRegister(t, r, &changenotify.PendingNotify{
		FileID:           [16]byte{7},
		SessionID:        42,
		ConnID:           1,
		MessageID:        9,
		AsyncId:          900,
		WatchPath:        "/d",
		ShareName:        "s",
		CompletionFilter: changenotify.FileNotifyChangeFileName,
		// GateInterim false → final response runs inline (no dispatcher to
		// signal interim in a unit test).
		AsyncCallback: func(_, _, _ uint64, resp *changenotify.ChangeNotifyResponse) error {
			calls++
			gotStatus = resp.GetStatus()
			return nil
		},
	})

	// A watcher on a different session must survive the flush.
	var otherCalls int
	mustRegister(t, r, &changenotify.PendingNotify{
		FileID:           [16]byte{8},
		SessionID:        99,
		ConnID:           2,
		MessageID:        9,
		AsyncId:          901,
		WatchPath:        "/other",
		ShareName:        "s",
		CompletionFilter: changenotify.FileNotifyChangeFileName,
		AsyncCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
			otherCalls++
			return nil
		},
	})

	h.ExpireSessionNotifies(42)

	if calls != 1 {
		t.Fatalf("expected pending notify completed once, got %d calls", calls)
	}
	if gotStatus != types.StatusCancelled {
		t.Errorf("expected STATUS_CANCELLED, got 0x%08X", uint32(gotStatus))
	}
	if otherCalls != 0 {
		t.Errorf("session 99 watcher must not be completed, got %d calls", otherCalls)
	}
	if r.WatcherCount() != 1 {
		t.Errorf("expected 1 surviving watcher (session 99), got %d", r.WatcherCount())
	}

	// Idempotent: the subsequent expired requests in the same window are no-ops.
	h.ExpireSessionNotifies(42)
	if calls != 1 {
		t.Errorf("ExpireSessionNotifies must be idempotent, got %d calls", calls)
	}
}

// TestChangeNotify_HandleClosed_ReturnsEncodedCleanupBody pins the wire shape
// of the synchronous STATUS_NOTIFY_CLEANUP reply.
//
// STATUS_NOTIFY_CLEANUP is success-severity, so the response carries a real
// CHANGE_NOTIFY body with zero changes. Returning it with no body at all
// leaves a bare SMB2 header on the wire, which fails the client's parse and
// fails every request in flight on that connection — not just this one.
func TestChangeNotify_HandleClosed_ReturnsEncodedCleanupBody(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = changenotify.NewNotifyRegistry()
	h.MaxTransactSize = 1 << 20

	fileID := [16]byte{0x43}
	openFile := (&OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1", SessionID: 1, TreeID: 1,
		DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/dir"})
	h.StoreOpenFile(openFile)

	// CLOSE ran before this CHANGE_NOTIFY could register.
	h.NotifyRegistry.CloseByFileID(fileID)

	ctx := &SMBHandlerContext{
		SessionID: 1, TreeID: 1, MessageID: 78, ConnID: 5,
		TryReserveAsync: func() bool { return true },
		ReleaseAsync:    func() {},
		AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
			return nil
		},
	}

	res, err := h.ChangeNotify(ctx, encodeChangeNotifyReq(0, 1000, fileID, changenotify.FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify error: %v", err)
	}
	if res.Status != types.StatusNotifyCleanup {
		t.Fatalf("status = %v, want STATUS_NOTIFY_CLEANUP", res.Status)
	}
	if len(res.Data) == 0 {
		t.Fatal("STATUS_NOTIFY_CLEANUP returned with no body — bare header on the wire")
	}
	if got := binary.LittleEndian.Uint16(res.Data[0:2]); got != 9 {
		t.Errorf("body StructureSize = %d, want 9", got)
	}
	if got := binary.LittleEndian.Uint32(res.Data[4:8]); got != 0 {
		t.Errorf("OutputBufferLength = %d, want 0", got)
	}
	if res.AsyncId != 0 {
		t.Errorf("AsyncId = %d on synchronous cleanup, want 0", res.AsyncId)
	}
	if got := h.NotifyRegistry.WatcherCount(); got != 0 {
		t.Errorf("WatcherCount = %d, want 0", got)
	}
}

// TestCloseFilesWithFilter_OnlyTombstonesDirectories checks that session
// teardown does not record a close tombstone for handles that could never
// have carried a watch.
//
// CHANGE_NOTIFY is refused on anything but a directory, so a file or pipe
// handle has no watch to complete. Running the completion for one anyway
// leaves a tombstone nothing will ever consume, and because the sweep that
// reclaims them is O(n) per call, tearing down a session holding many file
// handles would pay that sweep once per handle.
func TestCloseFilesWithFilter_OnlyTombstonesDirectories(t *testing.T) {
	e := setupTeardownLeakEnv(t)
	e.h.NotifyRegistry = changenotify.NewNotifyRegistry()

	const sessionID = uint64(0x5E)
	for i := 0; i < 64; i++ {
		name := fmt.Sprintf("plain%d.txt", i)
		fh, f := e.makeFile(t, name)
		of := &OpenFile{
			FileID:         [16]byte{byte(i), 0xF1},
			IsDirectory:    false,
			SessionID:      sessionID,
			TreeID:         e.tree.TreeID,
			ShareName:      e.tree.ShareName,
			MetadataHandle: fh,
		}
		_ = f
		e.h.StoreOpenFile(of.WithName(OpenName{Path: "/" + name}))
	}

	e.h.CloseAllFilesForSession(t.Context(), sessionID, true)

	if got := e.h.NotifyRegistry.CloseTombstoneCount(); got != 0 {
		t.Fatalf("close tombstones after tearing down 64 file handles = %d, want 0", got)
	}
}

// notifyHandlerEnv builds a handler with one armed directory handle whose
// buffered events are ready to be collected.
func notifyHandlerEnv(t *testing.T, fileID [16]byte) (*Handler, *changenotify.NotifyRegistry) {
	t.Helper()
	h := NewHandler()
	h.NotifyRegistry = newTestNotifyRegistry()
	h.MaxTransactSize = 1 << 20
	h.StoreOpenFile((&OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1", SessionID: 1, TreeID: 1,
		DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/d"}))

	// Arm the handle the way a first CHANGE_NOTIFY would, then take the watch
	// away again so later events have nowhere live to go and must buffer.
	mustRegister(t, h.NotifyRegistry, &changenotify.PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 1, AsyncId: 1,
		WatchPath: "/d", ShareName: "share1", MaxOutputLength: 1000,
		CompletionFilter: changenotify.FileNotifyChangeFileName, WatchTree: true,
	})
	if got := h.NotifyRegistry.CancelByMessageID(1, 1); got == nil {
		t.Fatal("setup: expected to remove the arming watch")
	}
	return h, h.NotifyRegistry
}

func notifyCtx(msgID uint64, reserved *int) *SMBHandlerContext {
	return &SMBHandlerContext{
		SessionID: 1, TreeID: 1, MessageID: msgID, ConnID: 1,
		TryReserveAsync: func() bool { *reserved++; return true },
		ReleaseAsync:    func() {},
		AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
			return nil
		},
	}
}

// TestChangeNotify_AnswersFromBufferedEventsWithoutGoingPending is the core of
// the fix: a request that arrives when events are already buffered is answered
// synchronously with STATUS_OK and never goes pending.
//
// A client that polls by sending a CHANGE_NOTIFY and cancelling it immediately
// — which smb2.notify.tree does, counting num_changes from the reply — can only
// ever see an event this way. smb2_notify_recv leaves num_changes untouched on
// any non-OK status, so an interim PENDING followed by a cancel reports nothing.
func TestChangeNotify_AnswersFromBufferedEventsWithoutGoingPending(t *testing.T) {
	fileID := [16]byte{0x91}
	h, r := notifyHandlerEnv(t, fileID)

	r.NotifyChange("share1", "/d", "a.txt", changenotify.FileActionAdded, changenotify.FileNotifyChangeFileName)

	reserved := 0
	res, err := h.ChangeNotify(notifyCtx(7, &reserved),
		encodeChangeNotifyReq(changenotify.SMB2WatchTree, 1000, fileID, changenotify.FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("status = %v, want STATUS_SUCCESS", res.Status)
	}
	if res.AsyncId != 0 {
		t.Errorf("AsyncId = %d, want 0 — the request must not go pending", res.AsyncId)
	}
	if reserved != 0 {
		t.Errorf("TryReserveAsync called %d times, want 0", reserved)
	}
	if got := r.WatcherCount(); got != 0 {
		t.Errorf("WatcherCount = %d, want 0 — nothing should be registered", got)
	}
	changes := decodeFileNotifyInfos(res.Data[8:])
	if len(changes) != 1 || changes[0].FileName != "a.txt" {
		t.Fatalf("reply carried %+v, want one entry for a.txt", changes)
	}
}

// TestChangeNotify_BufferedEventsBeatTheCancelTombstone pins the ordering the
// fix depends on: buffered events are collected BEFORE the pre-arrival cancel
// tombstone is consulted.
//
// The tombstone exists to stop a watch being armed that would wait forever. It
// has no say over events that already exist — and because the client cancels
// every request it sends, letting the tombstone win means it never sees one.
func TestChangeNotify_BufferedEventsBeatTheCancelTombstone(t *testing.T) {
	fileID := [16]byte{0x92}
	h, r := notifyHandlerEnv(t, fileID)

	r.NotifyChange("share1", "/d", "b.txt", changenotify.FileActionAdded, changenotify.FileNotifyChangeFileName)

	// The CANCEL for this MessageID lands before the CHANGE_NOTIFY is dispatched.
	if got := r.CancelByMessageID(1, 9); got != nil {
		t.Fatalf("setup: CancelByMessageID found a watch it should not have: %+v", got)
	}

	reserved := 0
	res, err := h.ChangeNotify(notifyCtx(9, &reserved),
		encodeChangeNotifyReq(changenotify.SMB2WatchTree, 1000, fileID, changenotify.FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("status = %v, want STATUS_SUCCESS — the tombstone must not swallow existing events", res.Status)
	}
	changes := decodeFileNotifyInfos(res.Data[8:])
	if len(changes) != 1 || changes[0].FileName != "b.txt" {
		t.Fatalf("reply carried %+v, want one entry for b.txt", changes)
	}
}

// TestChangeNotify_SyncAnswerThenCancelRespondsExactlyOnce is the mirror of the
// "every watch the registry removes gets an answer" invariant: a watch must be
// answered exactly once, so a request already answered synchronously must not
// produce a second answer for the same MessageID. Such a request was never
// queued, so the CANCEL that follows it has nothing to complete.
func TestChangeNotify_SyncAnswerThenCancelRespondsExactlyOnce(t *testing.T) {
	fileID := [16]byte{0x93}
	h, r := notifyHandlerEnv(t, fileID)

	r.NotifyChange("share1", "/d", "c.txt", changenotify.FileActionAdded, changenotify.FileNotifyChangeFileName)

	var asyncResponses int
	ctx := notifyCtx(11, new(int))
	ctx.AsyncNotifyCallback = func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
		asyncResponses++
		return nil
	}

	res, err := h.ChangeNotify(ctx, encodeChangeNotifyReq(changenotify.SMB2WatchTree, 1000, fileID, changenotify.FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("status = %v, want STATUS_SUCCESS", res.Status)
	}

	// The client's CANCEL arrives after the reply is already on the wire.
	cancelRes, err := h.Cancel(&SMBHandlerContext{SessionID: 1, TreeID: 1, MessageID: 11, ConnID: 1},
		[]byte{0x04, 0x00, 0x00, 0x00})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelRes != nil {
		t.Errorf("Cancel produced a response (%v); CANCEL never answers", cancelRes.Status)
	}
	if asyncResponses != 0 {
		t.Fatalf("%d async responses after a synchronous answer, want 0 — MessageID answered twice", asyncResponses)
	}
	if got := r.WatcherCount(); got != 0 {
		t.Errorf("WatcherCount = %d, want 0", got)
	}
}

// TestChangeNotify_WatchPathIsNormalised covers a directory handle opened by a
// path the client spelled with a traversal component.
//
// The handle stores the filename exactly as the client sent it, while events
// are reported against resolved paths, so a handle opened as `zqy\..` would
// never match an event on the parent it actually refers to. smbtorture's
// smb2.notify.tree opens one that way and expects it to see the parent's
// events.
func TestChangeNotify_WatchPathIsNormalised(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = newTestNotifyRegistry()
	h.MaxTransactSize = 1 << 20

	fileID := [16]byte{0x95}
	h.StoreOpenFile((&OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1", SessionID: 1, TreeID: 1,
		DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/d/sub/.."}))

	reserved := 0
	res, err := h.ChangeNotify(notifyCtx(21, &reserved),
		encodeChangeNotifyReq(changenotify.SMB2WatchTree, 1000, fileID, changenotify.FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify: %v", err)
	}
	if res.Status != types.StatusPending {
		t.Fatalf("status = %v, want STATUS_PENDING (nothing buffered yet)", res.Status)
	}

	// An event on the directory the handle actually refers to must reach it.
	var got []changenotify.FileNotifyInformation
	r := h.NotifyRegistry
	var watch *changenotify.PendingNotify
	r.RangeWatchers(func(n *changenotify.PendingNotify) bool {
		watch = n
		return true
	})
	if watch == nil {
		t.Fatal("no watch registered")
	}
	if watch.WatchPath != "/d" {
		t.Fatalf("registered WatchPath = %q, want %q", watch.WatchPath, "/d")
	}
	watch.AsyncCallback = func(_, _, _ uint64, resp *changenotify.ChangeNotifyResponse) error {
		got = append(got, decodeFileNotifyInfos(resp.Buffer)...)
		return nil
	}
	// The dispatcher's PostSend hook does not run in a unit test, so the
	// interim-sent signal has to be delivered by hand or the final response
	// stays deferred. Outside RangeWatchers: that holds the registry lock.
	r.MarkInterimSent(watch)

	r.NotifyChange("share1", "/d", "x.txt", changenotify.FileActionAdded, changenotify.FileNotifyChangeFileName)
	r.FlushAll()

	if len(got) != 1 || got[0].FileName != "x.txt" {
		t.Fatalf("watch opened as /d/sub/.. saw %+v, want the event on /d", got)
	}
}

// TestChangeNotify_SyncAnswerRefreshesArmedRouting covers the armed handle's
// routing fields when a request is answered synchronously.
//
// With no watch pending it is the armed entry, not the request, that decides
// which events get buffered — and WatchTree is non-sticky. A request answered
// from the buffer never reaches Register, so without an explicit refresh the
// previous request's recursion flag stays in place. Stale non-recursive is the
// damaging direction: subdirectory events are dropped outright and no later
// recursive request can recover them.
func TestChangeNotify_SyncAnswerRefreshesArmedRouting(t *testing.T) {
	fileID := [16]byte{0x97}
	h, r := notifyHandlerEnv(t, fileID)

	// The handle was armed non-recursive by a previous request.
	r.Arm(&changenotify.PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, WatchPath: "/d", ShareName: "share1",
		CompletionFilter: changenotify.FileNotifyChangeFileName, WatchTree: false, MaxOutputLength: 1000,
	})

	// A top-level event so this request has something to be answered with.
	r.NotifyChange("share1", "/d", "top.txt", changenotify.FileActionAdded, changenotify.FileNotifyChangeFileName)

	// A RECURSIVE request is answered synchronously from that event.
	reserved := 0
	res, err := h.ChangeNotify(notifyCtx(31, &reserved),
		encodeChangeNotifyReq(changenotify.SMB2WatchTree, 1000, fileID, changenotify.FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("status = %v, want STATUS_SUCCESS", res.Status)
	}

	// The handle must now be armed recursive, so a subdirectory event buffers.
	r.NotifyChange("share1", "/d/sub", "deep.txt", changenotify.FileActionAdded, changenotify.FileNotifyChangeFileName)

	got := r.TakeBufferedEvents(fileID, changenotify.FileNotifyChangeFileName, true)
	if len(got) != 1 || !strings.Contains(got[0].FileName, "deep.txt") {
		t.Fatalf("subdirectory event after a recursive sync answer = %+v, want it buffered — "+
			"the armed handle kept the previous request's non-recursive flag", got)
	}
}

// TestChangeNotify_EmptyFilterInheritsTheArmedMask covers smb2.notify.rec's
// re-issued request, which sets completion_filter = 0 and expects the server to
// keep watching for what the handle was already armed with.
//
// [MS-FSA] 2.1.5.11 makes the filter a property of the directory's
// ChangeNotifyEntry, constructed by the FIRST CHANGE_NOTIFY on the handle;
// neither it nor MS-SMB2 3.3.5.19 validates the field, and Samba does not
// either. Only a request that is the first on its handle and names no filter
// has nothing to watch for.
func TestChangeNotify_EmptyFilterInheritsTheArmedMask(t *testing.T) {
	newDirHandle := func(h *Handler, id byte) [16]byte {
		var fileID [16]byte
		fileID[0] = id
		h.StoreOpenFile((&OpenFile{
			FileID:        fileID,
			IsDirectory:   true,
			ShareName:     "share1",
			SessionID:     1,
			TreeID:        1,
			DesiredAccess: 0x00000001,
			GrantedAccess: 0x00000001,
		}).WithName(OpenName{Path: "/dir"}))
		return fileID
	}
	makeCtx := func(msgID uint64) *SMBHandlerContext {
		return &SMBHandlerContext{
			SessionID: 1, TreeID: 1, MessageID: msgID, ConnID: 1,
			TryReserveAsync:     func() bool { return true },
			ReleaseAsync:        func() {},
			AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error { return nil },
		}
	}

	t.Run("armed handle", func(t *testing.T) {
		h := NewHandler()
		h.NotifyRegistry = changenotify.NewNotifyRegistry()
		h.MaxTransactSize = 1 << 20
		fileID := newDirHandle(h, 0x91)

		res, err := h.ChangeNotify(makeCtx(1), encodeChangeNotifyReq(0, 1000, fileID, changenotify.FileNotifyChangeDirName))
		if err != nil || res == nil || res.Status != types.StatusPending {
			t.Fatalf("first CHANGE_NOTIFY: want STATUS_PENDING, got %+v (err=%v)", res, err)
		}

		res, err = h.ChangeNotify(makeCtx(2), encodeChangeNotifyReq(0, 1000, fileID, 0))
		if err != nil {
			t.Fatalf("re-issued CHANGE_NOTIFY error: %v", err)
		}
		if res == nil || res.Status != types.StatusPending {
			t.Fatalf("re-issued CHANGE_NOTIFY with an empty filter: want STATUS_PENDING, got %+v", res)
		}

		// It must watch for the armed mask, not for nothing.
		var seen bool
		h.NotifyRegistry.RangeWatchers(func(p *changenotify.PendingNotify) bool {
			if p.MessageID == 2 {
				seen = true
				if p.CompletionFilter != changenotify.FileNotifyChangeDirName {
					t.Errorf("re-issued watch filter = 0x%08X, want the armed 0x%08X",
						p.CompletionFilter, changenotify.FileNotifyChangeDirName)
				}
			}
			return true
		})
		if !seen {
			t.Error("the re-issued request registered no watch")
		}
	})

	t.Run("unarmed handle", func(t *testing.T) {
		h := NewHandler()
		h.NotifyRegistry = changenotify.NewNotifyRegistry()
		h.MaxTransactSize = 1 << 20
		fileID := newDirHandle(h, 0x92)

		res, err := h.ChangeNotify(makeCtx(1), encodeChangeNotifyReq(0, 1000, fileID, 0))
		if err != nil {
			t.Fatalf("CHANGE_NOTIFY error: %v", err)
		}
		if res == nil || res.Status != types.StatusInvalidParameter {
			t.Fatalf("first CHANGE_NOTIFY with an empty filter: want STATUS_INVALID_PARAMETER, got %+v", res)
		}

		// The refusal must not have armed the handle with an empty mask: a
		// later request naming a real filter has to work.
		res, err = h.ChangeNotify(makeCtx(2), encodeChangeNotifyReq(0, 1000, fileID, changenotify.FileNotifyChangeFileName))
		if err != nil || res == nil || res.Status != types.StatusPending {
			t.Fatalf("CHANGE_NOTIFY after a refused empty filter: want STATUS_PENDING, got %+v (err=%v)", res, err)
		}
	})
}

// TestChangeNotify_DeletePendingDirectory_AnsweredSynchronously pins the wire
// answer for the late-arrival ordering: the directory's delete disposition is
// committed, and only then does the CHANGE_NOTIFY reach the handler. It
// must come back on its own MessageID with STATUS_DELETE_PENDING rather than
// going async on a wait that has already been swept past.
func TestChangeNotify_DeletePendingDirectory_AnsweredSynchronously(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = changenotify.NewNotifyRegistry()
	h.MaxTransactSize = 1 << 20

	fileID := [16]byte{0x44}
	openFile := (&OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1", SessionID: 1, TreeID: 1,
		DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}).WithName(OpenName{Path: "/dir"})
	h.StoreOpenFile(openFile)

	// Another handle marked the directory for deletion first. The sweep finds
	// nothing: this watch has not registered yet.
	h.NotifyRegistry.MarkDirectoryDeletePending("share1", "/dir")

	var wentAsync bool
	ctx := &SMBHandlerContext{
		SessionID: 1, TreeID: 1, MessageID: 79, ConnID: 5,
		TryReserveAsync: func() bool { return true },
		ReleaseAsync:    func() {},
		AsyncNotifyCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
			wentAsync = true
			return nil
		},
	}

	res, err := h.ChangeNotify(ctx, encodeChangeNotifyReq(0, 1000, fileID, changenotify.FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify error: %v", err)
	}
	if res.Status != types.StatusDeletePending {
		t.Fatalf("status = %v, want STATUS_DELETE_PENDING", res.Status)
	}
	if res.AsyncId != 0 {
		t.Errorf("AsyncId = %d, want 0 — the reply is synchronous on the original MessageID", res.AsyncId)
	}
	if wentAsync {
		t.Error("the request must not be parked as an async watch")
	}
	if n := h.NotifyRegistry.WatcherCount(); n != 0 {
		t.Errorf("WatcherCount = %d, want 0 — nothing may be left waiting", n)
	}
}

// TestReleaseSessionLeasesAndNotifies_FiresCleanupSynchronously verifies that
// pending CHANGE_NOTIFY watchers belonging to a session are completed with
// STATUS_NOTIFY_CLEANUP SYNCHRONOUSLY — before releaseSessionLeasesAndNotifies
// returns, which is what smb2.notify.session-reconnect exercises:
// CleanupSession calls DeleteSession immediately after releasing notifies,
// and SendMessage requires the session to still exist to sign the response.
// An async (`go func`) delivery races with DeleteSession and emits an
// unsigned response that the client rejects, hanging the test.
func TestReleaseSessionLeasesAndNotifies_FiresCleanupSynchronously(t *testing.T) {
	h := NewHandler()

	const sessionID uint64 = 0xA1B2C3D4E5F60001

	var firedCount atomic.Int32
	var firedStatus atomic.Uint32
	var firedSessionID atomic.Uint64
	if err := h.NotifyRegistry.Register(&changenotify.PendingNotify{
		FileID:           [16]byte{1, 2, 3, 4},
		SessionID:        sessionID,
		ConnID:           1,
		MessageID:        42,
		AsyncId:          7,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: changenotify.FileNotifyChangeFileName,
		AsyncCallback: func(sid, _, _ uint64, response *changenotify.ChangeNotifyResponse) error {
			firedCount.Add(1)
			firedStatus.Store(uint32(response.GetStatus()))
			firedSessionID.Store(sid)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// A watcher belonging to a different session must NOT be fired.
	const otherSessionID uint64 = 0xA1B2C3D4E5F60002
	var otherFired atomic.Int32
	if err := h.NotifyRegistry.Register(&changenotify.PendingNotify{
		FileID:           [16]byte{5, 6, 7, 8},
		SessionID:        otherSessionID,
		ConnID:           2,
		MessageID:        99,
		AsyncId:          17,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: changenotify.FileNotifyChangeFileName,
		AsyncCallback: func(_, _, _ uint64, _ *changenotify.ChangeNotifyResponse) error {
			otherFired.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("Register(other) failed: %v", err)
	}

	// LeaseManager is nil on a fresh handler — exercise the notify branch only.
	h.releaseSessionLeasesAndNotifies(t.Context(), sessionID)

	// The cleanup callback MUST have fired by the time the call returns —
	// no `go func`, no sleep, no eventually loop. Sync delivery is the fix.
	if got := firedCount.Load(); got != 1 {
		t.Fatalf("AsyncCallback fired %d times, want 1 synchronous fire", got)
	}
	if got := types.Status(firedStatus.Load()); got != types.StatusNotifyCleanup {
		t.Errorf("callback received status 0x%08X, want STATUS_NOTIFY_CLEANUP (0x%08X)",
			uint32(got), uint32(types.StatusNotifyCleanup))
	}
	if got := firedSessionID.Load(); got != sessionID {
		t.Errorf("callback received sessionID 0x%X, want 0x%X (must be OLD session for signing)",
			got, sessionID)
	}
	if got := otherFired.Load(); got != 0 {
		t.Errorf("other-session watcher fired %d times, want 0 (cleanup must be scoped to sessionID)", got)
	}
	// Watcher must be unregistered so a subsequent CHANGE_NOTIFY on a new
	// session can re-register without colliding on FileID.
	if got := h.NotifyRegistry.WatcherCount(); got != 1 {
		t.Errorf("WatcherCount = %d, want 1 (only the other-session watcher should remain)", got)
	}
}
