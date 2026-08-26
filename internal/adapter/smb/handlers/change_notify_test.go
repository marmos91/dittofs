package handlers

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

func mustRegister(t *testing.T, r *NotifyRegistry, n *PendingNotify) {
	t.Helper()
	if err := r.Register(n); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
}

// newTestNotifyRegistry builds a NotifyRegistry whose background flush timer
// is effectively disabled (flushDelay set far past any test's runtime). These
// tests deliver buffered events synchronously and deterministically via
// FlushAll; with the production 100µs timer left armed, the time.AfterFunc
// goroutine could fire BEFORE FlushAll stops it, delivering the callback on
// the timer goroutine with no happens-before edge to the test's assertion.
// On a fast machine FlushAll always wins, so the race is invisible; on the
// contended Windows CI runner the timer wins often enough to flake (#714,
// TestNotifyChange_ExactPath). Pushing flushDelay out of reach makes FlushAll
// the sole, synchronous delivery path on every platform.
func newTestNotifyRegistry() *NotifyRegistry {
	reg := NewNotifyRegistry()
	reg.flushDelay = time.Hour
	return reg
}

func TestNotifyRegistry_RegisterAndUnregister(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{1, 2, 3}
	notify := &PendingNotify{
		FileID:           fileID,
		SessionID:        100,
		MessageID:        200,
		AsyncId:          300,
		WatchPath:        "/testdir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	}

	mustRegister(t, r, notify)

	// Verify it's registered
	watchers := r.GetWatchersForPath("/testdir")
	if len(watchers) != 1 {
		t.Fatalf("expected 1 watcher, got %d", len(watchers))
	}
	if watchers[0].AsyncId != 300 {
		t.Errorf("expected asyncId 300, got %d", watchers[0].AsyncId)
	}

	// Unregister
	removed := r.Unregister(fileID)
	if removed == nil {
		t.Fatal("expected non-nil removed notify")
	}
	if removed.AsyncId != 300 {
		t.Errorf("expected asyncId 300, got %d", removed.AsyncId)
	}

	// Verify it's gone
	watchers = r.GetWatchersForPath("/testdir")
	if len(watchers) != 0 {
		t.Fatalf("expected 0 watchers after unregister, got %d", len(watchers))
	}
}

// TestNotifyRegistry_Register_CrossConnectionMessageIDNoEvict is a regression
// test for issue #416. Before the fix, the registry keyed byMessageID
// globally: when a second TCP connection registered a CHANGE_NOTIFY with
// the same MessageID value as a live pending notify on another connection
// (common because MessageID is per-connection), Register silently
// unregistered the first one. The client on the first connection then hung
// — no final response, CANCEL couldn't find the entry, connection
// eventually dropped. After the fix, (ConnID, MessageID) is the key.
func TestNotifyRegistry_Register_CrossConnectionMessageIDNoEvict(t *testing.T) {
	r := newTestNotifyRegistry()

	a := &PendingNotify{
		FileID:           [16]byte{0xA},
		SessionID:        100,
		ConnID:           1,
		MessageID:        521,
		AsyncId:          3600,
		WatchPath:        "/testdir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	}
	b := &PendingNotify{
		FileID:           [16]byte{0xB},
		SessionID:        200,
		ConnID:           2,
		MessageID:        521, // same MessageID, different ConnID
		AsyncId:          3605,
		WatchPath:        "/testdir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	}
	mustRegister(t, r, a)
	mustRegister(t, r, b)

	// Both must still be resolvable — B's Register must NOT have evicted A.
	if got := r.UnregisterByAsyncId(3600); got == nil || got.AsyncId != 3600 {
		t.Fatalf("A (asyncId=3600) missing after B registered with same MessageID on a different ConnID")
	}
	if got := r.UnregisterByAsyncId(3605); got == nil || got.AsyncId != 3605 {
		t.Fatalf("B (asyncId=3605) missing")
	}
}

// TestNotifyRegistry_CancelByMessageID_DisambiguatesByConnID verifies the
// CANCEL-by-MessageID path scopes its lookup to the requesting connection,
// so two pending notifies sharing a MessageID across two TCP connections
// can each be cancelled independently.
func TestNotifyRegistry_CancelByMessageID_DisambiguatesByConnID(t *testing.T) {
	r := newTestNotifyRegistry()

	a := &PendingNotify{
		FileID:           [16]byte{0xA},
		SessionID:        100,
		ConnID:           1,
		MessageID:        521,
		AsyncId:          1,
		WatchPath:        "/d",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
	}
	b := &PendingNotify{
		FileID:           [16]byte{0xB},
		SessionID:        200,
		ConnID:           2,
		MessageID:        521,
		AsyncId:          2,
		WatchPath:        "/d",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
	}
	mustRegister(t, r, a)
	mustRegister(t, r, b)

	got := r.CancelByMessageID(1, 521)
	if got == nil || got.AsyncId != 1 {
		t.Fatalf("CancelByMessageID(connID=1) returned %+v, want A", got)
	}
	// B must still be there.
	if got := r.CancelByMessageID(2, 521); got == nil || got.AsyncId != 2 {
		t.Fatalf("CancelByMessageID(connID=2) returned %+v, want B", got)
	}
}

func TestNotifyRegistry_CancelByMessageID(t *testing.T) {
	r := newTestNotifyRegistry()

	notify := &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        100,
		ConnID:           7,
		MessageID:        42,
		AsyncId:          99,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	}
	mustRegister(t, r, notify)

	// Unregister by (ConnID, MessageID)
	removed := r.CancelByMessageID(7, 42)
	if removed == nil {
		t.Fatal("expected non-nil removed notify")
	}
	if removed.AsyncId != 99 {
		t.Errorf("expected asyncId 99, got %d", removed.AsyncId)
	}

	// Should not find it again
	removed = r.CancelByMessageID(7, 42)
	if removed != nil {
		t.Error("expected nil on second unregister")
	}
}

func TestNotifyRegistry_UnregisterByAsyncId(t *testing.T) {
	r := newTestNotifyRegistry()

	notify := &PendingNotify{
		FileID:           [16]byte{2},
		SessionID:        100,
		MessageID:        50,
		AsyncId:          777,
		WatchPath:        "/dir2",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeDirName,
	}
	mustRegister(t, r, notify)

	// Unregister by async ID
	removed := r.UnregisterByAsyncId(777)
	if removed == nil {
		t.Fatal("expected non-nil removed notify")
	}
	if removed.MessageID != 50 {
		t.Errorf("expected messageID 50, got %d", removed.MessageID)
	}

	// Should not find it again
	removed = r.UnregisterByAsyncId(777)
	if removed != nil {
		t.Error("expected nil on second unregister")
	}
}

// TestNotifyRegistry_SecondNotifyQueuesBehindFirst pins that a second
// CHANGE_NOTIFY on a handle does NOT replace the first. A client may keep
// several outstanding on one handle; the registry queues them.
func TestNotifyRegistry_SecondNotifyQueuesBehindFirst(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{5}
	for _, mid := range []uint64{10, 20} {
		mustRegister(t, r, &PendingNotify{
			FileID:           fileID,
			SessionID:        1,
			ConnID:           1,
			MessageID:        mid,
			AsyncId:          mid * 10,
			WatchPath:        "/dir",
			ShareName:        "share1",
			CompletionFilter: FileNotifyChangeFileName,
		})
	}

	if got := r.WatcherCount(); got != 2 {
		t.Fatalf("WatcherCount = %d, want 2: the second request must not evict the first", got)
	}
	watchers := r.GetWatchersForPath("/dir")
	if len(watchers) != 2 {
		t.Fatalf("expected 2 watchers on /dir, got %d", len(watchers))
	}
	if watchers[0].MessageID != 10 || watchers[1].MessageID != 20 {
		t.Errorf("watchers out of arrival order: %d, %d", watchers[0].MessageID, watchers[1].MessageID)
	}
}

func TestNotifyRegistry_MultipleWatchers(t *testing.T) {
	r := newTestNotifyRegistry()

	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{2},
		MessageID:        20,
		AsyncId:          200,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeDirName,
	})

	watchers := r.GetWatchersForPath("/dir")
	if len(watchers) != 2 {
		t.Fatalf("expected 2 watchers, got %d", len(watchers))
	}
}

func TestNotifyChange_ExactPath(t *testing.T) {
	r := newTestNotifyRegistry()

	notified := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Fire a matching change
	r.NotifyChange("share1", "/dir", "test.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if !notified {
		t.Fatal("expected watcher to be notified")
	}

	// Watcher should be unregistered (one-shot)
	watchers := r.GetWatchersForPath("/dir")
	if len(watchers) != 0 {
		t.Errorf("expected 0 watchers after notify (one-shot), got %d", len(watchers))
	}
}

func TestNotifyChange_NoMatchDifferentShare(t *testing.T) {
	r := newTestNotifyRegistry()

	notified := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Fire change on different share
	r.NotifyChange("share2", "/dir", "test.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if notified {
		t.Error("should not notify watcher on different share")
	}
}

func TestNotifyChange_RecursiveWatchTree(t *testing.T) {
	r := newTestNotifyRegistry()

	notified := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		WatchTree:        true,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Fire change in subdirectory
	r.NotifyChange("share1", "/subdir", "test.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if !notified {
		t.Error("recursive watcher should be notified for subdirectory changes")
	}
}

func TestNotifyChange_NonRecursiveNoMatch(t *testing.T) {
	r := newTestNotifyRegistry()

	notified := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		WatchTree:        false, // NOT recursive
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Fire change in subdirectory (should NOT match non-recursive watcher)
	r.NotifyChange("share1", "/subdir", "test.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if notified {
		t.Error("non-recursive watcher should not be notified for subdirectory changes")
	}
}

func TestNotifyRename_PairedNotification(t *testing.T) {
	r := newTestNotifyRegistry()

	notified := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			// Verify the response has content (paired old+new name entries)
			if len(response.Buffer) == 0 {
				t.Error("rename response should have non-empty buffer")
			}
			return nil
		},
	})

	r.NotifyRename("share1", "/dir", "old.txt", "/dir", "new.txt", FileNotifyChangeFileName)
	r.FlushAll()

	if !notified {
		t.Error("watcher should be notified on rename")
	}

	// Watcher should be unregistered (one-shot)
	watchers := r.GetWatchersForPath("/dir")
	if len(watchers) != 0 {
		t.Errorf("expected 0 watchers after rename (one-shot), got %d", len(watchers))
	}
}

func TestNotifyRename_CrossDirectory(t *testing.T) {
	r := newTestNotifyRegistry()

	notified := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		WatchTree:        true,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			if len(response.Buffer) == 0 {
				t.Error("cross-dir rename response should have non-empty buffer")
			}
			return nil
		},
	})

	// Cross-directory rename: /src/old.txt -> /dst/new.txt
	r.NotifyRename("share1", "/src", "old.txt", "/dst", "new.txt", FileNotifyChangeFileName)
	r.FlushAll()

	if !notified {
		t.Error("recursive root watcher should be notified on cross-directory rename")
	}
}

func TestNotifyChange_MaxOutputLengthExceeded_SendsEnumDir(t *testing.T) {
	r := newTestNotifyRegistry()

	var receivedStatus types.Status
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  1, // Too small for any encoded filename
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			receivedStatus = response.GetStatus()
			return nil
		},
	})

	r.NotifyChange("share1", "/dir", "test.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if receivedStatus != types.StatusNotifyEnumDir {
		t.Errorf("expected STATUS_NOTIFY_ENUM_DIR (0x%08X), got 0x%08X",
			uint32(types.StatusNotifyEnumDir), uint32(receivedStatus))
	}

	// Watcher should still be unregistered
	watchers := r.GetWatchersForPath("/dir")
	if len(watchers) != 0 {
		t.Errorf("expected 0 watchers after enum-dir, got %d", len(watchers))
	}
}

func TestNotifyChange_ConcurrentDoubleFire(t *testing.T) {
	r := newTestNotifyRegistry()

	var callbackCount atomic.Int32
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			callbackCount.Add(1)
			return nil
		},
	})

	// Fire two concurrent events — only one should trigger the callback
	done := make(chan struct{})
	go func() {
		r.NotifyChange("share1", "/dir", "a.txt", FileActionAdded, FileNotifyChangeFileName)
		r.FlushAll()
		done <- struct{}{}
	}()
	go func() {
		r.NotifyChange("share1", "/dir", "b.txt", FileActionAdded, FileNotifyChangeFileName)
		r.FlushAll()
		done <- struct{}{}
	}()
	<-done
	<-done

	count := callbackCount.Load()
	if count != 1 {
		t.Errorf("expected exactly 1 callback invocation (one-shot), got %d", count)
	}
}

func TestNotifyRegistry_MaxWatchesLimit(t *testing.T) {
	r := newTestNotifyRegistry()

	// Fill up to the limit
	for i := 0; i < MaxPendingWatches; i++ {
		fileID := [16]byte{}
		fileID[0] = byte(i)
		fileID[1] = byte(i >> 8)
		fileID[2] = byte(i >> 16)
		err := r.Register(&PendingNotify{
			FileID:           fileID,
			MessageID:        uint64(i),
			AsyncId:          uint64(i),
			WatchPath:        "/dir",
			ShareName:        "share1",
			CompletionFilter: FileNotifyChangeFileName,
		})
		if err != nil {
			t.Fatalf("Register %d failed: %v", i, err)
		}
	}

	// One more should fail
	err := r.Register(&PendingNotify{
		FileID:           [16]byte{0xFF, 0xFF, 0xFF},
		MessageID:        99999,
		AsyncId:          99999,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})
	if err == nil {
		t.Error("expected error when exceeding MaxPendingWatches")
	}
}

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		name   string
		action uint32
		filter uint32
		want   bool
	}{
		{"Added matches FileName", FileActionAdded, FileNotifyChangeFileName, true},
		{"Added matches DirName", FileActionAdded, FileNotifyChangeDirName, true},
		{"Added no match Size", FileActionAdded, FileNotifyChangeSize, false},
		{"Removed matches FileName", FileActionRemoved, FileNotifyChangeFileName, true},
		{"Modified matches Size", FileActionModified, FileNotifyChangeSize, true},
		{"Modified matches LastWrite", FileActionModified, FileNotifyChangeLastWrite, true},
		{"Modified matches Attributes", FileActionModified, FileNotifyChangeAttributes, true},
		{"Modified no match FileName", FileActionModified, FileNotifyChangeFileName, false},
		{"RenamedOld matches FileName", FileActionRenamedOldName, FileNotifyChangeFileName, true},
		{"RenamedNew matches DirName", FileActionRenamedNewName, FileNotifyChangeDirName, true},
		{"Combined filter", FileActionAdded, FileNotifyChangeFileName | FileNotifyChangeSize, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesFilter(tt.action, tt.filter)
			if got != tt.want {
				t.Errorf("MatchesFilter(%d, 0x%x) = %v, want %v", tt.action, tt.filter, got, tt.want)
			}
		})
	}
}

func TestEncodeFileNotifyInformation(t *testing.T) {
	changes := []FileNotifyInformation{
		{Action: FileActionAdded, FileName: "test.txt"},
	}

	buf := EncodeFileNotifyInformation(changes)
	if len(buf) == 0 {
		t.Fatal("expected non-empty buffer")
	}

	// Minimum size: 12 bytes header + filename in UTF-16LE
	// "test.txt" = 8 chars * 2 bytes = 16 bytes
	// 12 + 16 = 28 bytes, aligned to 4 = 28
	if len(buf) < 28 {
		t.Errorf("buffer too short: %d bytes", len(buf))
	}
}

func TestEncodeFileNotifyInformation_MultipleEntries(t *testing.T) {
	changes := []FileNotifyInformation{
		{Action: FileActionRenamedOldName, FileName: "old.txt"},
		{Action: FileActionRenamedNewName, FileName: "new.txt"},
	}

	buf := EncodeFileNotifyInformation(changes)
	if len(buf) == 0 {
		t.Fatal("expected non-empty buffer")
	}

	// First entry should have non-zero NextEntryOffset
	// (pointing to the second entry)
	if buf[0] == 0 && buf[1] == 0 && buf[2] == 0 && buf[3] == 0 {
		t.Error("first entry should have non-zero NextEntryOffset")
	}
}

func TestEncodeFileNotifyInformation_Empty(t *testing.T) {
	buf := EncodeFileNotifyInformation(nil)
	if buf != nil {
		t.Errorf("expected nil buffer for empty changes, got %d bytes", len(buf))
	}
}

func TestGetParentPath(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"/foo/bar", "/foo"},
		{"/foo", "/"},
		{"/", "/"},
		{"", "/"},
	}
	for _, tt := range tests {
		got := GetParentPath(tt.input)
		if got != tt.want {
			t.Errorf("GetParentPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGetFileName(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"/foo/bar/file.txt", "file.txt"},
		{"/file.txt", "file.txt"},
		{"/", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := GetFileName(tt.input)
		if got != tt.want {
			t.Errorf("GetFileName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRelativePathFromWatch_CrossPath(t *testing.T) {
	// When watchPath is not a prefix of parentPath, should return fileName
	// (no panic from out-of-bounds slice)
	got := relativePathFromWatch("/beta", "/a", "file.txt")
	if got != "file.txt" {
		t.Errorf("expected 'file.txt' for non-prefix watch path, got %q", got)
	}
}

func TestNotifyChange_StreamNameOnADSCreate(t *testing.T) {
	r := newTestNotifyRegistry()

	var notified bool
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeStreamName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Simulate ADS stream creation: file:stream:$DATA created in /dir
	r.NotifyChange("share1", "/dir", "file:stream:$DATA", FileActionAdded, FileNotifyChangeStreamName)
	r.FlushAll()

	if !notified {
		t.Fatal("expected watcher with FileNotifyChangeStreamName to be notified on ADS create")
	}
}

func TestNotifyChange_StreamWriteOnADSWrite(t *testing.T) {
	r := newTestNotifyRegistry()

	var notified bool
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{2},
		SessionID:        1,
		MessageID:        20,
		AsyncId:          200,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeStreamWrite,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Simulate ADS stream write: file:stream:$DATA modified in /dir
	r.NotifyChange("share1", "/dir", "file:stream:$DATA", FileActionModifiedStream, FileNotifyChangeStreamWrite)
	r.FlushAll()

	if !notified {
		t.Fatal("expected watcher with FileNotifyChangeStreamWrite to be notified on ADS write")
	}
}

func TestNotifyChange_StreamSizeOnADSWrite(t *testing.T) {
	r := newTestNotifyRegistry()

	var notified bool
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{3},
		SessionID:        1,
		MessageID:        30,
		AsyncId:          300,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeStreamSize,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Simulate ADS stream size change
	r.NotifyChange("share1", "/dir", "file:stream:$DATA", FileActionModifiedStream, FileNotifyChangeStreamSize)
	r.FlushAll()

	if !notified {
		t.Fatal("expected watcher with FileNotifyChangeStreamSize to be notified on ADS size change")
	}
}

func TestNotifyChange_SecurityDescriptorChange(t *testing.T) {
	r := newTestNotifyRegistry()

	var notified bool
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{4},
		SessionID:        1,
		MessageID:        40,
		AsyncId:          400,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeSecurity,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			notified = true
			return nil
		},
	})

	// Simulate security descriptor change on a file in /dir
	r.NotifyChange("share1", "/dir", "file.txt", FileActionModified, FileNotifyChangeSecurity)
	r.FlushAll()

	if !notified {
		t.Fatal("expected watcher with FileNotifyChangeSecurity to be notified on security change")
	}
}

func TestMatchesFilter_StreamFilters(t *testing.T) {
	tests := []struct {
		name   string
		action uint32
		filter uint32
		want   bool
	}{
		{"Added matches StreamName", FileActionAdded, FileNotifyChangeStreamName, true},
		{"Removed matches StreamName", FileActionRemoved, FileNotifyChangeStreamName, true},
		{"Modified matches StreamSize", FileActionModified, FileNotifyChangeStreamSize, true},
		{"Modified matches StreamWrite", FileActionModified, FileNotifyChangeStreamWrite, true},
		{"Modified no match StreamName", FileActionModified, FileNotifyChangeStreamName, false},
		{"RenamedOld matches StreamName", FileActionRenamedOldName, FileNotifyChangeStreamName, true},
		{"RenamedNew matches StreamName", FileActionRenamedNewName, FileNotifyChangeStreamName, true},
		{"Added no match StreamWrite", FileActionAdded, FileNotifyChangeStreamWrite, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesFilter(tt.action, tt.filter)
			if got != tt.want {
				t.Errorf("MatchesFilter(%d, 0x%x) = %v, want %v", tt.action, tt.filter, got, tt.want)
			}
		})
	}
}

func TestNotifyChange_DoubleWatchers_BothNotified(t *testing.T) {
	r := newTestNotifyRegistry()

	var count1, count2 atomic.Int32
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			count1.Add(1)
			return nil
		},
	})
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{2},
		SessionID:        1,
		MessageID:        20,
		AsyncId:          200,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			count2.Add(1)
			return nil
		},
	})

	// Fire a change — both watchers should be notified
	r.NotifyChange("share1", "/dir", "test.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if count1.Load() != 1 {
		t.Errorf("watcher 1: expected 1 notification, got %d", count1.Load())
	}
	if count2.Load() != 1 {
		t.Errorf("watcher 2: expected 1 notification, got %d", count2.Load())
	}

	// Both should be unregistered (one-shot)
	watchers := r.GetWatchersForPath("/dir")
	if len(watchers) != 0 {
		t.Errorf("expected 0 watchers after double notify, got %d", len(watchers))
	}
}

func TestMatchesFilter_MaskFiltering(t *testing.T) {
	// Only size filter set — should NOT match file create/delete
	if MatchesFilter(FileActionAdded, FileNotifyChangeSize) {
		t.Error("FileActionAdded should NOT match FileNotifyChangeSize")
	}

	// Only attributes filter set — should NOT match file create/delete
	if MatchesFilter(FileActionAdded, FileNotifyChangeAttributes) {
		t.Error("FileActionAdded should NOT match FileNotifyChangeAttributes")
	}

	// Modified matches security
	if !MatchesFilter(FileActionModified, FileNotifyChangeSecurity) {
		t.Error("FileActionModified should match FileNotifyChangeSecurity")
	}

	// Stream filter tests
	if !MatchesFilter(FileActionAddedStream, FileNotifyChangeStreamName) {
		t.Error("FileActionAddedStream should match FileNotifyChangeStreamName")
	}
	if !MatchesFilter(FileActionModifiedStream, FileNotifyChangeStreamWrite) {
		t.Error("FileActionModifiedStream should match FileNotifyChangeStreamWrite")
	}
	if MatchesFilter(FileActionAddedStream, FileNotifyChangeFileName) {
		t.Error("FileActionAddedStream should NOT match FileNotifyChangeFileName")
	}
}

func TestIsValidCompletionFilter(t *testing.T) {
	tests := []struct {
		name   string
		filter uint32
		want   bool
	}{
		{"zero is invalid", 0, false},
		{"all valid flags", AllValidCompletionFilterFlags, true},
		{"single valid flag", FileNotifyChangeFileName, true},
		{"reserved-only bit is valid", 0x80000000, true},
		{"valid + reserved mixed is valid", FileNotifyChangeFileName | 0x80000000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsValidCompletionFilter(tt.filter)
			if got != tt.want {
				t.Errorf("IsValidCompletionFilter(0x%08X) = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
}

func TestCompleteWatchersForDeletePending(t *testing.T) {
	r := newTestNotifyRegistry()

	statuses := map[uint64]types.Status{}
	register := func(fileID byte, messageID uint64, share, watchPath string) {
		mustRegister(t, r, &PendingNotify{
			FileID:           [16]byte{fileID},
			SessionID:        1,
			MessageID:        messageID,
			AsyncId:          messageID * 10,
			WatchPath:        watchPath,
			ShareName:        share,
			CompletionFilter: FileNotifyChangeFileName | FileNotifyChangeDirName,
			MaxOutputLength:  4096,
			AsyncCallback: func(sessionID, mid, asyncId uint64, response *ChangeNotifyResponse) error {
				statuses[mid] = response.GetStatus()
				return nil
			},
		})
	}

	// Two handles watch the doomed directory; one watches its parent
	// recursively, and one watches the same path on another share.
	register(1, 10, "share1", "/parent/target")
	register(2, 11, "share1", "/parent/target")
	register(3, 12, "share1", "/parent")
	register(4, 13, "share2", "/parent/target")

	if got := r.CompleteWatchersForDeletePending("share1", "/parent/target"); got != 2 {
		t.Fatalf("expected 2 watchers completed, got %d", got)
	}

	for _, mid := range []uint64{10, 11} {
		if statuses[mid] != types.StatusDeletePending {
			t.Errorf("messageID %d: expected STATUS_DELETE_PENDING (0x%08X), got 0x%08X",
				mid, uint32(types.StatusDeletePending), uint32(statuses[mid]))
		}
	}
	// The ancestor watcher's own directory still exists, and another share's
	// identically-named directory is a different object entirely.
	for _, mid := range []uint64{12, 13} {
		if _, answered := statuses[mid]; answered {
			t.Errorf("messageID %d must not be completed: its watched directory is not the one deleted", mid)
		}
	}

	// A second mark finds nothing left to answer.
	if got := r.CompleteWatchersForDeletePending("share1", "/parent/target"); got != 0 {
		t.Errorf("expected 0 on re-mark, got %d", got)
	}
}

func TestUnregisterAllForSession(t *testing.T) {
	r := newTestNotifyRegistry()

	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        100,
		MessageID:        10,
		AsyncId:          1000,
		WatchPath:        "/dir1",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{2},
		SessionID:        100,
		MessageID:        20,
		AsyncId:          2000,
		WatchPath:        "/dir2",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{3},
		SessionID:        200, // different session
		MessageID:        30,
		AsyncId:          3000,
		WatchPath:        "/dir1",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})

	removed := r.UnregisterAllForSession(100)
	if len(removed) != 2 {
		t.Errorf("expected 2 watchers removed, got %d", len(removed))
	}

	// Session 200 watcher should still be present
	watchers := r.GetWatchersForPath("/dir1")
	if len(watchers) != 1 {
		t.Errorf("expected 1 watcher remaining, got %d", len(watchers))
	}
}

func TestAsyncResponseRegistry(t *testing.T) {
	r := NewAsyncResponseRegistry(100)

	var completed bool
	op := &AsyncOperation{
		AsyncId:   42,
		SessionID: 1,
		MessageID: 10,
		Callback: func(sessionID, messageID, asyncId uint64, status types.Status, data []byte) error {
			completed = true
			if status != types.StatusSuccess {
				t.Errorf("expected StatusSuccess, got %v", status)
			}
			return nil
		},
	}

	if err := r.Register(op); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if r.Len() != 1 {
		t.Errorf("expected 1 pending op, got %d", r.Len())
	}

	if err := r.Complete(42, types.StatusSuccess, nil); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	if !completed {
		t.Error("callback should have been called")
	}

	if r.Len() != 0 {
		t.Errorf("expected 0 pending ops after complete, got %d", r.Len())
	}
}

func TestAsyncResponseRegistry_Cancel(t *testing.T) {
	r := NewAsyncResponseRegistry(100)

	var receivedStatus types.Status
	op := &AsyncOperation{
		AsyncId:   99,
		SessionID: 1,
		MessageID: 10,
		Callback: func(sessionID, messageID, asyncId uint64, status types.Status, data []byte) error {
			receivedStatus = status
			return nil
		},
	}

	if err := r.Register(op); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if err := r.Cancel(99); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	if receivedStatus != types.StatusCancelled {
		t.Errorf("expected STATUS_CANCELLED, got 0x%08X", uint32(receivedStatus))
	}
}

func TestAsyncResponseRegistry_MaxLimit(t *testing.T) {
	r := NewAsyncResponseRegistry(2)

	for i := uint64(1); i <= 2; i++ {
		if err := r.Register(&AsyncOperation{AsyncId: i}); err != nil {
			t.Fatalf("Register %d failed: %v", i, err)
		}
	}

	// Third should fail
	err := r.Register(&AsyncOperation{AsyncId: 3})
	if err == nil {
		t.Error("expected error when exceeding max limit")
	}
}

func TestIsValidCompletionFilter_AllBits(t *testing.T) {
	// Each individual valid bit should be accepted
	validBits := []uint32{
		FileNotifyChangeFileName,
		FileNotifyChangeDirName,
		FileNotifyChangeAttributes,
		FileNotifyChangeSize,
		FileNotifyChangeLastWrite,
		FileNotifyChangeLastAccess,
		FileNotifyChangeCreation,
		FileNotifyChangeEa,
		FileNotifyChangeSecurity,
		FileNotifyChangeStreamName,
		FileNotifyChangeStreamSize,
		FileNotifyChangeStreamWrite,
	}
	for _, bit := range validBits {
		if !IsValidCompletionFilter(bit) {
			t.Errorf("IsValidCompletionFilter(0x%08X) = false, want true", bit)
		}
	}

	// Reserved bits alone are accepted (Windows/Samba behavior: they never
	// match any event but are not rejected at the filter-validation gate).
	reservedOnlyBits := []uint32{0x00001000, 0x00010000, 0x01000000, 0x80000000}
	for _, bit := range reservedOnlyBits {
		if !IsValidCompletionFilter(bit) {
			t.Errorf("IsValidCompletionFilter(0x%08X) = false, want true (non-zero)", bit)
		}
	}
}

func TestNotifyChange_OverflowWithMultipleChanges(t *testing.T) {
	// Verify that when we manually build a notification that would exceed
	// MaxOutputLength, the registry sends STATUS_NOTIFY_ENUM_DIR.
	r := newTestNotifyRegistry()

	var receivedStatus types.Status
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  16, // Very small — won't fit even one entry
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			receivedStatus = response.GetStatus()
			return nil
		},
	})

	// Any file change will produce a FileNotifyInformation entry larger than 16 bytes
	r.NotifyChange("share1", "/dir", "longfilename.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if receivedStatus != types.StatusNotifyEnumDir {
		t.Errorf("expected STATUS_NOTIFY_ENUM_DIR (0x%08X), got 0x%08X",
			uint32(types.StatusNotifyEnumDir), uint32(receivedStatus))
	}
}

func TestUnregisterAllForSession_PreservesOtherSessions(t *testing.T) {
	// Verify that UnregisterAllForSession does NOT affect other sessions.
	// This is critical for session reconnect/re-auth scenarios.
	r := newTestNotifyRegistry()

	// Session 100: two watches
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{1}, SessionID: 100, MessageID: 10, AsyncId: 1000,
		WatchPath: "/dir1", ShareName: "share1", CompletionFilter: FileNotifyChangeFileName,
	})
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{2}, SessionID: 100, MessageID: 20, AsyncId: 2000,
		WatchPath: "/dir2", ShareName: "share1", CompletionFilter: FileNotifyChangeFileName,
	})

	// Session 200: one watch
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{3}, SessionID: 200, MessageID: 30, AsyncId: 3000,
		WatchPath: "/dir1", ShareName: "share1", CompletionFilter: FileNotifyChangeFileName,
	})

	// Session 300: one watch
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{4}, SessionID: 300, MessageID: 40, AsyncId: 4000,
		WatchPath: "/dir2", ShareName: "share1", CompletionFilter: FileNotifyChangeDirName,
	})

	// Remove session 100 only
	removed := r.UnregisterAllForSession(100)
	if len(removed) != 2 {
		t.Errorf("expected 2 watchers removed for session 100, got %d", len(removed))
	}

	// Session 200 and 300 watchers must still be present
	dir1Watchers := r.GetWatchersForPath("/dir1")
	if len(dir1Watchers) != 1 || dir1Watchers[0].SessionID != 200 {
		t.Errorf("expected session 200 watcher on /dir1, got %d watchers", len(dir1Watchers))
	}

	dir2Watchers := r.GetWatchersForPath("/dir2")
	if len(dir2Watchers) != 1 || dir2Watchers[0].SessionID != 300 {
		t.Errorf("expected session 300 watcher on /dir2, got %d watchers", len(dir2Watchers))
	}
}

// TestSendAndUnregister_UndersizedBufferYieldsEnumDir covers the
// smb2.notify.valid-req contract for one notify cycle: when the encoded
// change list exceeds MaxOutputLength, the registry MUST return
// STATUS_NOTIFY_ENUM_DIR to the client. The "if the first notify returns
// NOTIFY_ENUM_DIR, all do" sticky property is enforced one layer up at the
// handler via OpenFile.NotifyMaxBufferSize, not by the registry.
func TestSendAndUnregister_UndersizedBufferYieldsEnumDir(t *testing.T) {
	r := newTestNotifyRegistry()

	var deliveredStatus types.Status
	fileID := [16]byte{0xAA}

	mustRegister(t, r, &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  0, // any encoded notification exceeds this
		AsyncCallback: func(_, _, _ uint64, response *ChangeNotifyResponse) error {
			deliveredStatus = response.GetStatus()
			return nil
		},
	})

	r.NotifyChange("share1", "/dir", "file.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if deliveredStatus != types.StatusNotifyEnumDir {
		t.Errorf("expected STATUS_NOTIFY_ENUM_DIR, got 0x%08X", uint32(deliveredStatus))
	}
}

// TestUnregisterAllForSession_ReturnedNotifiesPreserveAsyncCallback verifies
// that watchers removed by UnregisterAllForSession retain their AsyncCallback
// so the caller can fire STATUS_NOTIFY_CLEANUP per MS-SMB2 3.3.5.5.2
// (smb2.notify.invalid-reauth / session-reconnect / .tcon / .dir).
func TestUnregisterAllForSession_ReturnedNotifiesPreserveAsyncCallback(t *testing.T) {
	r := newTestNotifyRegistry()

	var calledWithStatus types.Status
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        100,
		MessageID:        10,
		AsyncId:          1000,
		WatchPath:        "/dir1",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		AsyncCallback: func(_, _, _ uint64, response *ChangeNotifyResponse) error {
			calledWithStatus = response.GetStatus()
			return nil
		},
	})

	removed := r.UnregisterAllForSession(100)
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed, got %d", len(removed))
	}

	// Caller invokes the callback to deliver STATUS_NOTIFY_CLEANUP.
	resp := &ChangeNotifyResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyCleanup},
	}
	if err := removed[0].AsyncCallback(removed[0].SessionID, removed[0].MessageID, removed[0].AsyncId, resp); err != nil {
		t.Fatalf("AsyncCallback returned error: %v", err)
	}
	if calledWithStatus != types.StatusNotifyCleanup {
		t.Errorf("expected callback to receive STATUS_NOTIFY_CLEANUP, got 0x%08X", uint32(calledWithStatus))
	}
}

// TestArmedBuffer_OverflowsAfterCancelWithNoLiveWatcher reproduces the
// smb2.notify.overflow torture flow: a CHANGE_NOTIFY arms the handle and is
// then cancelled (no live watcher remains), 100 directory-create events
// fire (each FILE_ACTION_ADDED), and the next CHANGE_NOTIFY on the handle
// must observe the armed overflow trip via OnOverflow. The bug before the
// fix was that NotifyChange short-circuited when no live watcher matched,
// so events accumulated in the gap were never counted and the handle's
// sticky overflow was never set.
func TestArmedBuffer_OverflowsAfterCancelWithNoLiveWatcher(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{0xCC}
	var overflowFireCount int32
	var lastOverflowFileID [16]byte

	// Match the torture test parameters: 1000-byte buffer, recursive,
	// FILE_NOTIFY_CHANGE_NAME (covered by FileNotifyChangeFileName |
	// FileNotifyChangeDirName).
	first := &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		ConnID:           1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/basedir_ovf",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName | FileNotifyChangeDirName,
		WatchTree:        true,
		MaxOutputLength:  1000,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			return nil
		},
		OnOverflow: func(id [16]byte) {
			atomic.AddInt32(&overflowFireCount, 1)
			lastOverflowFileID = id
		},
	}
	mustRegister(t, r, first)

	// Client cancels the initial notify (smbtorture does this to "set up
	// the buffer"). Pending entry is removed but the handle stays armed.
	if got := r.UnregisterByAsyncId(100); got == nil {
		t.Fatalf("expected to unregister live watcher on cancel")
	}

	// Fire 100 FILE_ACTION_ADDED events on subdirs (mirroring the torture
	// loop that creates 100 directories inside the watched root).
	for i := 0; i < 100; i++ {
		r.NotifyChange("share1", "/basedir_ovf", fmt.Sprintf("test%d.txt", i), FileActionAdded, FileNotifyChangeFileName)
		r.FlushAll()
	}

	// Sticky overflow must have tripped exactly once.
	if atomic.LoadInt32(&overflowFireCount) != 1 {
		t.Fatalf("expected OnOverflow to fire exactly 1× across 100 events, got %d", overflowFireCount)
	}
	if lastOverflowFileID != fileID {
		t.Errorf("expected OnOverflow with fileID %v, got %v", fileID, lastOverflowFileID)
	}

	// Closing the handle clears the armed slot — events fired after this
	// must NOT re-trip overflow.
	r.CloseByFileID(fileID)
	r.NotifyChange("share1", "/basedir_ovf", "post-close.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if atomic.LoadInt32(&overflowFireCount) != 1 {
		t.Errorf("expected no additional OnOverflow after close, got count=%d", overflowFireCount)
	}
}

// TestArmedBuffer_ResetClearsOverflowForNextWindow exercises the
// ResetArmedOverflow path: after the handler consumes the sticky overflow
// (returning STATUS_NOTIFY_ENUM_DIR) it must reset the armed counter so
// the next batch of events accumulates against the freshly advertised
// MaxOutputLength rather than re-tripping immediately.
func TestArmedBuffer_ResetClearsOverflowForNextWindow(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{0xDD}
	var overflowFireCount int32

	mustRegister(t, r, &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		ConnID:           1,
		MessageID:        20,
		AsyncId:          200,
		WatchPath:        "/d",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  16, // overflow on the first event
		AsyncCallback:    func(_, _, _ uint64, _ *ChangeNotifyResponse) error { return nil },
		OnOverflow:       func(_ [16]byte) { atomic.AddInt32(&overflowFireCount, 1) },
	})
	if got := r.UnregisterByAsyncId(200); got == nil {
		t.Fatalf("cancel")
	}

	// First event trips overflow.
	r.NotifyChange("s", "/d", "a.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if atomic.LoadInt32(&overflowFireCount) != 1 {
		t.Fatalf("expected first event to trip overflow, count=%d", overflowFireCount)
	}

	// More events while overflowed must not re-fire OnOverflow.
	for i := 0; i < 5; i++ {
		r.NotifyChange("s", "/d", "b.txt", FileActionAdded, FileNotifyChangeFileName)
		r.FlushAll()
	}
	if atomic.LoadInt32(&overflowFireCount) != 1 {
		t.Errorf("expected overflow to latch (no re-fire), got count=%d", overflowFireCount)
	}

	// Client issues a new CHANGE_NOTIFY with a generous buffer; the handler
	// consumes the sticky flag and resets the armed accounting.
	r.ResetArmedOverflow(fileID, 64*1024)

	// Single small event must NOT trip overflow against the new 64KB buffer.
	r.NotifyChange("s", "/d", "c.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if atomic.LoadInt32(&overflowFireCount) != 1 {
		t.Errorf("expected no overflow after reset+small event, got count=%d", overflowFireCount)
	}
}

// TestArmedBuffer_ScopedByShareAndPath confirms armed-handle accounting
// respects ShareName, WatchPath, WatchTree, and CompletionFilter — events
// on unrelated shares/paths/filters must not charge against an armed
// handle. Guards against false-positive overflows on unrelated buckets.
func TestArmedBuffer_ScopedByShareAndPath(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{0xEE}
	var overflowFireCount int32

	mustRegister(t, r, &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		ConnID:           1,
		MessageID:        30,
		AsyncId:          300,
		WatchPath:        "/watched",
		ShareName:        "share-a",
		CompletionFilter: FileNotifyChangeFileName,
		WatchTree:        false, // non-recursive
		MaxOutputLength:  16,
		AsyncCallback:    func(_, _, _ uint64, _ *ChangeNotifyResponse) error { return nil },
		OnOverflow:       func(_ [16]byte) { atomic.AddInt32(&overflowFireCount, 1) },
	})
	if got := r.UnregisterByAsyncId(300); got == nil {
		t.Fatalf("cancel")
	}

	// Different share — must not charge.
	r.NotifyChange("share-b", "/watched", "x.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	// Subdirectory but non-recursive — must not charge.
	r.NotifyChange("share-a", "/watched/sub", "x.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	// Wrong filter (Modified vs FileName) — must not charge.
	r.NotifyChange("share-a", "/watched", "x.txt", FileActionModified, FileNotifyChangeAttributes)
	r.FlushAll()

	if atomic.LoadInt32(&overflowFireCount) != 0 {
		t.Errorf("expected no overflow on unrelated events, got count=%d", overflowFireCount)
	}

	// Matching event on the watched path — overflow must trip.
	r.NotifyChange("share-a", "/watched", "x.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if atomic.LoadInt32(&overflowFireCount) != 1 {
		t.Errorf("expected overflow trip for matching event, got count=%d", overflowFireCount)
	}
}

// TestArmedBuffer_NotChargedWhenLiveWatcherServesEvent guards against
// double-counting: the live-watcher one-shot path already encodes and
// delivers the event, so the armed accounting must skip handles that just
// fired (the armed entry will be torn down/replaced on the next Register).
func TestArmedBuffer_NotChargedWhenLiveWatcherServesEvent(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{0xEF}
	var overflowFireCount int32

	mustRegister(t, r, &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		ConnID:           1,
		MessageID:        40,
		AsyncId:          400,
		WatchPath:        "/d",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  16, // would overflow if double-counted
		AsyncCallback:    func(_, _, _ uint64, _ *ChangeNotifyResponse) error { return nil },
		OnOverflow:       func(_ [16]byte) { atomic.AddInt32(&overflowFireCount, 1) },
	})

	// Live watcher is present; one matching event fires through the live
	// path (which itself overflows the 16-byte buffer → OnOverflow). The
	// armed-accounting path must NOT also charge the event and double-fire.
	r.NotifyChange("s", "/d", "a.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if got := atomic.LoadInt32(&overflowFireCount); got != 1 {
		t.Errorf("expected OnOverflow exactly 1× (live path only), got %d — armed path is double-counting", got)
	}
}

// TestArmedBuffer_RecursiveWatcherChargesRelativePath asserts the buffered-
// byte accounting uses the per-watcher relative path (what would actually
// be encoded into FILE_NOTIFY_INFORMATION.FileName), not the bare
// fileName. A WatchTree watcher rooted at "/" that sees an event in a deep
// subdirectory must accumulate the longer "subdir/file.txt" — not the
// truncated "file.txt" — toward MaxOutputLength.
//
// Regression test for PR #613 Copilot review: charging the bare fileName
// systematically undercounted recursive watchers and let overflow latch
// later than a real marshal (or Samba notify_marshall_changes) would.
func TestArmedBuffer_RecursiveWatcherChargesRelativePath(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{0xF0}
	var overflowFireCount int32

	// Pick MaxOutputLength so the bare-name accounting fits but the
	// relative-path accounting overflows on the first event:
	//   bare     "x.txt"          -> 12 + 2*5  = 22, pad to 24 bytes
	//   relative "a/b/c/d/x.txt"  -> 12 + 2*13 = 38, pad to 40 bytes
	// MaxOutputLength=32 leaves room for the bare-name entry but not the
	// relative-path entry, so charging bare would NOT trip overflow and
	// charging relative WILL.
	mustRegister(t, r, &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		ConnID:           1,
		MessageID:        50,
		AsyncId:          500,
		WatchPath:        "/",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
		WatchTree:        true,
		MaxOutputLength:  32,
		AsyncCallback:    func(_, _, _ uint64, _ *ChangeNotifyResponse) error { return nil },
		OnOverflow:       func(_ [16]byte) { atomic.AddInt32(&overflowFireCount, 1) },
	})
	if r.UnregisterByAsyncId(500) == nil {
		t.Fatalf("cancel pending watcher to leave handle armed-but-unwatched")
	}

	// Event in a deep subdir. Relative-from-root encoding is
	// "a/b/c/d/x.txt" — 26 bytes UTF-16LE plus 12-byte header = 38, pad
	// to 40. That single entry alone exceeds MaxOutputLength=32, so
	// overflow must trip on this event.
	r.NotifyChange("s", "/a/b/c/d", "x.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if got := atomic.LoadInt32(&overflowFireCount); got != 1 {
		t.Errorf("expected overflow to trip on first deep-path event (relative-path accounting), got count=%d — recursive watcher is undercounting", got)
	}
}

// TestEncodedNotifyEntrySize_MatchesMarshaledSize asserts the byte estimate
// used by chargeArmedBuffer agrees with the encoder for representative
// names — guarding against drift between the accounting and the real wire
// marshal (EncodeFileNotifyInformation).
func TestEncodedNotifyEntrySize_MatchesMarshaledSize(t *testing.T) {
	cases := []string{
		"a",                 // 1 BMP rune → 12+2+pad(2) = 16
		"file.txt",          // 8 BMP runes → 12+16 = 28 → pad to 28
		"sub/deep/name.txt", // 17 BMP runes → 12+34 = 46 → pad to 48
		"",                  // empty → floor = minNotifyEntryBytes
		"é",                 // 1 BMP rune (precomposed) → 12+2+pad(2) = 16
	}
	for _, name := range cases {
		got := encodedNotifyEntrySize(name)
		marshaled := EncodeFileNotifyInformation([]FileNotifyInformation{
			{Action: FileActionAdded, FileName: name},
		})
		want := uint32(len(marshaled))
		// Single-entry marshal has NextEntryOffset=0 so the trailing pad
		// is the only difference from a real "next entry follows" frame.
		// For the empty-name floor case the estimator over-counts by
		// design (sentinel-size); allow the estimator ≥ marshaled.
		if name == "" {
			if got < want {
				t.Errorf("encodedNotifyEntrySize(%q) = %d < marshaled %d (must be ≥)", name, got, want)
			}
			continue
		}
		if got != want {
			t.Errorf("encodedNotifyEntrySize(%q) = %d; marshaled = %d", name, got, want)
		}
	}
}

// TestReleaseSessionLeasesAndNotifies_FiresCleanupSynchronously verifies that
// pending CHANGE_NOTIFY watchers belonging to a session are completed with
// STATUS_NOTIFY_CLEANUP SYNCHRONOUSLY — before releaseSessionLeasesAndNotifies
// returns. This is critical for smb2.notify.session-reconnect (issue #473):
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
	if err := h.NotifyRegistry.Register(&PendingNotify{
		FileID:           [16]byte{1, 2, 3, 4},
		SessionID:        sessionID,
		ConnID:           1,
		MessageID:        42,
		AsyncId:          7,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		AsyncCallback: func(sid, _, _ uint64, response *ChangeNotifyResponse) error {
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
	if err := h.NotifyRegistry.Register(&PendingNotify{
		FileID:           [16]byte{5, 6, 7, 8},
		SessionID:        otherSessionID,
		ConnID:           2,
		MessageID:        99,
		AsyncId:          17,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
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

func TestUnregisterAllForTree_PreservesOtherTrees(t *testing.T) {
	r := newTestNotifyRegistry()

	// Same session and share, different tree IDs (two tree connects to same share)
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{1}, SessionID: 100, MessageID: 10, AsyncId: 1000,
		WatchPath: "/dir1", ShareName: "share1", TreeID: 1, CompletionFilter: FileNotifyChangeFileName,
	})
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{2}, SessionID: 100, MessageID: 20, AsyncId: 2000,
		WatchPath: "/dir1", ShareName: "share1", TreeID: 2, CompletionFilter: FileNotifyChangeFileName,
	})

	// Disconnect tree 1 only
	removed := r.UnregisterAllForTree(100, 1)
	if len(removed) != 1 {
		t.Errorf("expected 1 watcher removed for tree 1, got %d", len(removed))
	}

	// Tree 2 watcher must remain
	watchers := r.GetWatchersForPath("/dir1")
	if len(watchers) != 1 || watchers[0].TreeID != 2 {
		t.Errorf("expected tree 2 watcher to remain, got %d watchers", len(watchers))
	}
}

// TestChangeNotify_HandlePermissions_GrantedAccessGate mirrors the smbtorture
// smb2.notify.handle-permissions test (source4/torture/smb2/notify.c::
// torture_smb2_notify_handle_permissions): a directory handle opened with only
// FILE_READ_ATTRIBUTES (no FILE_LIST_DIRECTORY) MUST reject CHANGE_NOTIFY
// with STATUS_ACCESS_DENIED per MS-SMB2 §3.3.5.19 / Samba
// source3/smbd/notify.c::change_notify_create (check_any_access_fsp with
// SEC_DIR_LIST). Refs #473.
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

			body := encodeChangeNotifyReq(SMB2WatchTree, 1000, fileID, FileNotifyChangeFileName|FileNotifyChangeDirName)

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

// encodeChangeNotifyReq builds an SMB2 CHANGE_NOTIFY request body
// per MS-SMB2 2.2.35.
func encodeChangeNotifyReq(flags uint16, outBufLen uint32, fileID [16]byte, completionFilter uint32) []byte {
	body := make([]byte, 32)
	// StructureSize = 32
	body[0] = 0x20
	body[1] = 0x00
	// Flags
	body[2] = byte(flags)
	body[3] = byte(flags >> 8)
	// OutputBufferLength
	body[4] = byte(outBufLen)
	body[5] = byte(outBufLen >> 8)
	body[6] = byte(outBufLen >> 16)
	body[7] = byte(outBufLen >> 24)
	// FileID
	copy(body[8:24], fileID[:])
	// CompletionFilter
	body[24] = byte(completionFilter)
	body[25] = byte(completionFilter >> 8)
	body[26] = byte(completionFilter >> 16)
	body[27] = byte(completionFilter >> 24)
	return body
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
	h.NotifyRegistry = NewNotifyRegistry()
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
			AsyncNotifyCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
				return nil
			},
		}
	}

	// First CHANGE_NOTIFY with a tiny buffer (1 byte). The handler must
	// accept it and store NotifyMaxBufferSize = 1 on the OpenFile.
	body1 := encodeChangeNotifyReq(0, 1, fileID, FileNotifyChangeFileName)
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
	body2 := encodeChangeNotifyReq(0, h.MaxTransactSize, fileID, FileNotifyChangeFileName|FileNotifyChangeDirName)
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
	h.NotifyRegistry.RangeWatchers(func(p *PendingNotify) bool {
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
	h.NotifyRegistry = NewNotifyRegistry()
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
			AsyncNotifyCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
				return nil
			},
		}
	}

	// First call: 65536 byte buffer.
	body1 := encodeChangeNotifyReq(0, 65536, fileID, FileNotifyChangeFileName)
	if _, err := h.ChangeNotify(makeCtx(), body1); err != nil {
		t.Fatalf("first CHANGE_NOTIFY error: %v", err)
	}
	h.NotifyRegistry.Unregister(fileID)

	// Second call: 256 byte buffer — smaller than stored, must be used as-is.
	body2 := encodeChangeNotifyReq(0, 256, fileID, FileNotifyChangeFileName)
	if _, err := h.ChangeNotify(makeCtx(), body2); err != nil {
		t.Fatalf("second CHANGE_NOTIFY error: %v", err)
	}

	var pendingMax uint32
	h.NotifyRegistry.RangeWatchers(func(p *PendingNotify) bool {
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
	h.NotifyRegistry = NewNotifyRegistry()
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
			AsyncNotifyCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
				return nil
			},
		}
	}

	// First CHANGE_NOTIFY: OutputBufferLength = 0 — returns ENUM_DIR
	// synchronously without registering a watcher (buffer=0 fast path).
	body1 := encodeChangeNotifyReq(0, 0, fileID, FileNotifyChangeFileName)
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
	body2 := encodeChangeNotifyReq(0, h.MaxTransactSize, fileID, FileNotifyChangeFileName)
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

// TestNotifyRegistry_PreArrivalCancel_TombstoneShortCircuitsRegister is the
// unit regression for issue #623. It mirrors what smb2.notify.dir's "notify
// cancel" subtest does on the wire: fire CHANGE_NOTIFY immediately followed
// by SMB2_CANCEL. Because every SMB2 request runs on its own goroutine
// (pkg/adapter/smb/connection.go), the CANCEL can dispatch first, find
// nothing in the NotifyRegistry, and return — and the still-running
// CHANGE_NOTIFY then registers a watcher that will never be cancelled.
// The whole smb2.notify suite then exceeds smbtorture's 120s per-suite
// timeout, taking the four other notify tests down with it.
//
// The fix tracks pre-arrival CANCELs as tombstones; Register checks the
// tombstone and returns ErrAlreadyCancelled so the handler can answer
// STATUS_CANCELLED synchronously.
func TestNotifyRegistry_PreArrivalCancel_TombstoneShortCircuitsRegister(t *testing.T) {
	r := newTestNotifyRegistry()

	const connID, messageID uint64 = 7, 42

	// CANCEL arrives first — no matching entry, so the handler drops a
	// tombstone for the future Register to find.
	r.CancelByMessageID(connID, messageID)

	notify := &PendingNotify{
		FileID:           [16]byte{0xAB},
		SessionID:        1,
		ConnID:           connID,
		MessageID:        messageID,
		AsyncId:          9001,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	}
	err := r.Register(notify)
	if err == nil {
		t.Fatal("Register: want ErrAlreadyCancelled, got nil")
	}
	if !errors.Is(err, ErrAlreadyCancelled) {
		t.Fatalf("Register: want ErrAlreadyCancelled, got %v", err)
	}

	// Tombstone must be one-shot: a subsequent Register with the same
	// (ConnID, MessageID) should succeed normally.
	if err := r.Register(notify); err != nil {
		t.Fatalf("Register after tombstone consumed: want nil, got %v", err)
	}
	if got := r.WatcherCount(); got != 1 {
		t.Fatalf("WatcherCount after re-register = %d, want 1", got)
	}
}

// TestNotifyRegistry_CancelTombstoneNoCrossMessageIDLeak guards the bound on
// the tombstone: a CANCEL on (conn=1, msg=10) MUST NOT short-circuit a
// future CHANGE_NOTIFY with a different MessageID (or different ConnID).
// Without this guard, the tombstone would be a global blocker.
func TestNotifyRegistry_CancelTombstoneNoCrossMessageIDLeak(t *testing.T) {
	r := newTestNotifyRegistry()
	r.CancelByMessageID(1, 10)

	otherMsg := &PendingNotify{
		FileID: [16]byte{0xFE}, ConnID: 1, MessageID: 11, AsyncId: 1,
		WatchPath: "/a", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
	}
	if err := r.Register(otherMsg); err != nil {
		t.Fatalf("Register (different MessageID): want nil, got %v", err)
	}

	otherConn := &PendingNotify{
		FileID: [16]byte{0xFD}, ConnID: 2, MessageID: 10, AsyncId: 2,
		WatchPath: "/b", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
	}
	if err := r.Register(otherConn); err != nil {
		t.Fatalf("Register (different ConnID): want nil, got %v", err)
	}
}

// TestNotifyRegistry_CancelTombstoneExpires verifies the TTL: a tombstone
// older than cancelTombstoneTTL is treated as absent so a CANCEL that was
// dropped on a notify the server already rejected synchronously cannot
// cancel a much-later unrelated notify that happens to reuse the same
// (ConnID, MessageID). This uses the public surface — direct map-poking
// would couple the test to the internal layout.
func TestNotifyRegistry_CancelTombstoneExpires(t *testing.T) {
	r := newTestNotifyRegistry()
	r.CancelByMessageID(1, 99)

	// Manually age the tombstone by rewriting it past the TTL. Going through
	// the map directly is the only way to simulate time passage without
	// sleeping for cancelTombstoneTTL in tests.
	r.mu.Lock()
	r.cancelTombstones[notifyMsgKey{ConnID: 1, MessageID: 99}] = time.Now().Add(-2 * cancelTombstoneTTL)
	r.mu.Unlock()

	notify := &PendingNotify{
		FileID: [16]byte{0xDE}, ConnID: 1, MessageID: 99, AsyncId: 1,
		WatchPath: "/c", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
	}
	if err := r.Register(notify); err != nil {
		t.Fatalf("Register after tombstone TTL: want nil, got %v", err)
	}
}

// TestChangeNotify_PreArrivalCancel_HandlerReturnsCancelledSync is the
// end-to-end regression: invoke the handler with a tombstone already in
// place and confirm it returns STATUS_CANCELLED synchronously rather than
// STATUS_PENDING. This is what unblocks the in-flight smbtorture client.
func TestChangeNotify_PreArrivalCancel_HandlerReturnsCancelledSync(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = NewNotifyRegistry()
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
		AsyncNotifyCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			return nil
		},
	}

	// CANCEL arrived ahead of us.
	h.NotifyRegistry.CancelByMessageID(ctx.ConnID, ctx.MessageID)

	body := encodeChangeNotifyReq(0, 1000, fileID, FileNotifyChangeFileName)
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

// TestNotifyRegistry_ConcurrentCancelBeforeRegister stresses the race that
// caused #623. We spin up many goroutines that each fire CancelByMessageID
// followed by Register on the same (ConnID, MessageID). Either outcome is
// correct (tombstone consumed before Register, or Register raced past) but
// no goroutine can land in a state where a watcher remains registered for a
// (ConnID, MessageID) we already tombstoned — the smbtorture hang condition.
func TestNotifyRegistry_ConcurrentCancelBeforeRegister(t *testing.T) {
	const iters = 200
	for i := 0; i < iters; i++ {
		r := newTestNotifyRegistry()
		connID := uint64(i)
		messageID := uint64(i*2 + 1)
		fileID := [16]byte{byte(i), byte(i >> 8)}

		done := make(chan struct{}, 2)
		// Cancel first, then notify — guarantees ErrAlreadyCancelled.
		go func() {
			r.CancelByMessageID(connID, messageID)
			done <- struct{}{}
		}()
		go func() {
			<-done // Force ordering: cancel definitely first.
			notify := &PendingNotify{
				FileID: fileID, ConnID: connID, MessageID: messageID, AsyncId: uint64(i + 100000),
				WatchPath: "/x", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
			}
			err := r.Register(notify)
			if !errors.Is(err, ErrAlreadyCancelled) {
				t.Errorf("iter %d: Register want ErrAlreadyCancelled, got %v", i, err)
			}
			if got := r.WatcherCount(); got != 0 {
				t.Errorf("iter %d: WatcherCount = %d, want 0", i, got)
			}
			done <- struct{}{}
		}()
		<-done
	}
}

// TestArmedBuffer_ReplayDeliversBufferedEventsOnReregister covers the
// goroutine-per-request race in smb2.notify.tcon: an event arrives while the
// first CHANGE_NOTIFY has already completed one-shot but the client hasn't
// re-registered yet. The event must buffer on the armed handle and be
// replayed on the next Register so the client doesn't miss it.
func TestArmedBuffer_ReplayDeliversBufferedEventsOnReregister(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xAA, 0xBB}

	// First Register — arms the handle — then UnregisterByAsyncId to leave
	// the handle armed but with no live watcher (mirrors one-shot completion).
	first := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 10, AsyncId: 100,
		WatchPath: "/d", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength: 64 * 1024,
		AsyncCallback:   func(uint64, uint64, uint64, *ChangeNotifyResponse) error { return nil },
	}
	mustRegister(t, r, first)
	if got := r.UnregisterByAsyncId(100); got == nil {
		t.Fatalf("expected to unregister first watcher")
	}

	// Event arrives in the gap between consumption and re-arm.
	r.NotifyChange("s", "/d", "appeared.txt", FileActionAdded, FileNotifyChangeFileName)

	// Re-arm with a callback that captures the delivered events. The replay
	// path in Register should hand off the buffered event to bufferEventLocked
	// so the flush timer drains it via the new callback.
	var got []FileNotifyInformation
	var mu sync.Mutex
	done := make(chan struct{}, 1)
	second := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 11, AsyncId: 101,
		WatchPath: "/d", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength: 64 * 1024,
		AsyncCallback: func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
			mu.Lock()
			defer mu.Unlock()
			if resp.Status == 0 {
				got = decodeFileNotifyInfos(resp.Buffer)
			}
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		},
	}
	mustRegister(t, r, second)

	// Drain timer.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		r.FlushAll()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].FileName != "appeared.txt" || got[0].Action != FileActionAdded {
		t.Fatalf("expected replay of [{Added appeared.txt}], got %#v", got)
	}
}

// TestArmedBuffer_CancelClearsBufferedEvents covers smb2.notify.mask: when
// the client cancels a pending notify, any events buffered on the armed
// handle must be discarded so a subsequent register does not replay stale
// state into a fresh request with a different filter.
func TestArmedBuffer_CancelClearsBufferedEvents(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xCD}

	first := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 10, AsyncId: 100,
		WatchPath: "/d", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength: 64 * 1024,
		AsyncCallback:   func(uint64, uint64, uint64, *ChangeNotifyResponse) error { return nil },
	}
	mustRegister(t, r, first)
	// Cancel via UnregisterByAsyncId (the CANCEL path in stub_handlers calls
	// this and then ClearBufferedEvents).
	if got := r.UnregisterByAsyncId(100); got == nil {
		t.Fatalf("expected to unregister")
	}

	// Buffer events while no live watcher is pending.
	r.NotifyChange("s", "/d", "leak.txt", FileActionAdded, FileNotifyChangeFileName)

	// Simulate the CANCEL handler's discard.
	r.ClearBufferedEvents(fileID)

	// Re-register: the previously buffered events must NOT replay.
	var got []FileNotifyInformation
	var mu sync.Mutex
	done := make(chan struct{}, 1)
	second := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 11, AsyncId: 101,
		WatchPath: "/d", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength: 64 * 1024,
		AsyncCallback: func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
			mu.Lock()
			defer mu.Unlock()
			got = decodeFileNotifyInfos(resp.Buffer)
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		},
	}
	mustRegister(t, r, second)

	select {
	case <-done:
		t.Fatalf("expected no replay after ClearBufferedEvents; got %#v", got)
	case <-time.After(50 * time.Millisecond):
		// No callback fired → buffer was empty as expected.
	}
}

// TestArmedBuffer_OverflowClearsBufferedEvents asserts that when the sticky
// overflow flag is consumed (handler returns STATUS_NOTIFY_ENUM_DIR), any
// buffered events on the armed handle are dropped — the client has been
// told to re-enumerate, so the stale buffer must not replay.
func TestArmedBuffer_OverflowClearsBufferedEvents(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xEE}

	first := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 10, AsyncId: 100,
		WatchPath: "/d", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength: 64 * 1024,
		AsyncCallback:   func(uint64, uint64, uint64, *ChangeNotifyResponse) error { return nil },
	}
	mustRegister(t, r, first)
	if got := r.UnregisterByAsyncId(100); got == nil {
		t.Fatalf("expected to unregister")
	}

	// Buffer an event on the armed handle.
	r.NotifyChange("s", "/d", "stale.txt", FileActionAdded, FileNotifyChangeFileName)

	// Simulate the handler consuming the sticky overflow.
	r.ClearBufferedEvents(fileID)
	r.ResetArmedOverflow(fileID, 64*1024)

	// Re-register: nothing should replay.
	done := make(chan struct{}, 1)
	second := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 11, AsyncId: 101,
		WatchPath: "/d", ShareName: "s", CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength: 64 * 1024,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		},
	}
	mustRegister(t, r, second)

	select {
	case <-done:
		t.Fatalf("expected no replay after overflow clears buffer")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestNameChangeFilterFor covers the ADS-aware filter selection used by
// CREATE / delete-on-close / rename notification sites: stream entries
// (FileName contains ':') route via FILE_NOTIFY_CHANGE_STREAM_NAME, dirs via
// FILE_NOTIFY_CHANGE_DIR_NAME, everything else via FILE_NOTIFY_CHANGE_FILE_NAME.
func TestNameChangeFilterFor(t *testing.T) {
	cases := []struct {
		name string
		dir  bool
		want uint32
	}{
		{"file.txt", false, FileNotifyChangeFileName},
		{"file.txt", true, FileNotifyChangeDirName},
		{"file.txt:stream:$DATA", false, FileNotifyChangeStreamName},
		{"file.txt:stream:$DATA", true, FileNotifyChangeStreamName},
	}
	for _, c := range cases {
		if got := NameChangeFilterFor(c.name, c.dir); got != c.want {
			t.Errorf("NameChangeFilterFor(%q, %v) = 0x%x, want 0x%x", c.name, c.dir, got, c.want)
		}
	}
}

// decodeFileNotifyInfos walks a FILE_NOTIFY_INFORMATION list (MS-FSCC §2.4.42).
// Test helper only — production decode happens client-side.
func decodeFileNotifyInfos(buf []byte) []FileNotifyInformation {
	var out []FileNotifyInformation
	off := 0
	for off+12 <= len(buf) {
		next := uint32(buf[off]) | uint32(buf[off+1])<<8 | uint32(buf[off+2])<<16 | uint32(buf[off+3])<<24
		action := uint32(buf[off+4]) | uint32(buf[off+5])<<8 | uint32(buf[off+6])<<16 | uint32(buf[off+7])<<24
		nameLen := uint32(buf[off+8]) | uint32(buf[off+9])<<8 | uint32(buf[off+10])<<16 | uint32(buf[off+11])<<24
		if off+12+int(nameLen) > len(buf) {
			break
		}
		u16 := make([]uint16, nameLen/2)
		for i := range u16 {
			u16[i] = uint16(buf[off+12+i*2]) | uint16(buf[off+12+i*2+1])<<8
		}
		out = append(out, FileNotifyInformation{Action: action, FileName: string(utf16.Decode(u16))})
		if next == 0 {
			break
		}
		off += int(next)
	}
	return out
}

// TestExpireSessionNotifies_CompletesPendingNotify verifies that an expired
// Kerberos session completes its outstanding async CHANGE_NOTIFY with
// STATUS_CANCELLED so the client's smb2_notify_recv unblocks (smbtorture
// smb2.session.expire2s / expire2e: session.c:1641 expects NT_STATUS_CANCELLED
// for the cancelled notify). The flush must be idempotent (the test fires
// several expired requests in the same window) and must not touch other
// sessions' watchers.
func TestExpireSessionNotifies_CompletesPendingNotify(t *testing.T) {
	r := NewNotifyRegistry()
	h := &Handler{NotifyRegistry: r}

	var calls int
	var gotStatus types.Status
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{7},
		SessionID:        42,
		ConnID:           1,
		MessageID:        9,
		AsyncId:          900,
		WatchPath:        "/d",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
		// GateInterim false → final response runs inline (no dispatcher to
		// signal interim in a unit test).
		AsyncCallback: func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
			calls++
			gotStatus = resp.GetStatus()
			return nil
		},
	})

	// A watcher on a different session must survive the flush.
	var otherCalls int
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{8},
		SessionID:        99,
		ConnID:           2,
		MessageID:        9,
		AsyncId:          901,
		WatchPath:        "/other",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
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

// armWatcher registers a root watcher with a catch-all filter and returns the
// records it is answered with.
func armWatcher(t *testing.T, r *NotifyRegistry, msgID uint64, got *[]FileNotifyInformation) {
	t.Helper()
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		ConnID:           1,
		MessageID:        msgID,
		AsyncId:          msgID * 10,
		WatchPath:        "/",
		ShareName:        "share1",
		CompletionFilter: AllValidCompletionFilterFlags,
		MaxOutputLength:  4096,
		AsyncCallback: func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
			if resp.GetStatus() != types.StatusSuccess {
				t.Errorf("expected STATUS_SUCCESS, got 0x%08X", uint32(resp.GetStatus()))
			}
			*got = append(*got, decodeFileNotifyInfos(resp.Buffer)...)
			return nil
		},
	})
}

func checkRecords(t *testing.T, label string, got []FileNotifyInformation, want []FileNotifyInformation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: expected %d changes, got %d: %+v", label, len(want), len(got), got)
	}
	for i := range want {
		if got[i].Action != want[i].Action || got[i].FileName != want[i].FileName {
			t.Errorf("%s: change[%d] = {action=0x%X name=%q}, want {action=0x%X name=%q}",
				label, i, got[i].Action, got[i].FileName, want[i].Action, want[i].FileName)
		}
	}
}

// TestNotifyChange_OneOperationPerResponse is the smb2.notify.valid-req
// scenario: a watcher is armed, then the client creates a file and writes to
// it, and the test requires ONE record back — the create's.
//
// The create and the write are separate client requests, so how many records
// the response carries must not depend on how quickly the second arrives. The
// accumulation window exists to hold together the several records ONE operation
// emits; letting it also absorb the next operation's makes the reply's contents
// a function of client timing, which is what made this fail intermittently.
func TestNotifyChange_OneOperationPerResponse(t *testing.T) {
	r := newTestNotifyRegistry()

	var first []FileNotifyInformation
	armWatcher(t, r, 10, &first)

	// torture_setup_simple_file on a file that does not exist yet: the unlink
	// is a no-op, the CREATE fires ADDED and the WRITE that follows it fires
	// MODIFIED. Two client requests, so two responses — the extra record the
	// test saw was a second distinct event, never the first one repeated.
	r.NotifyChange("share1", "/", "fname", FileActionAdded, NameChangeFilterFor("fname", false))
	r.NotifyChange("share1", "/", "fname", FileActionModified,
		FileNotifyChangeSize|FileNotifyChangeLastWrite|FileNotifyChangeAttributes)
	r.FlushAll()

	checkRecords(t, "first response", first, []FileNotifyInformation{
		{Action: FileActionAdded, FileName: "fname"},
	})

	// Nothing is lost: the client re-issues and collects the backlog, in order.
	backlog := r.TakeBufferedEvents([16]byte{1}, AllValidCompletionFilterFlags, false)
	checkRecords(t, "backlog", backlog, []FileNotifyInformation{
		{Action: FileActionModified, FileName: "fname"},
	})
}

// TestNotifyChange_OneOperationPerResponse_TimerPath drives the same case
// through the production delivery path — the flush timer — rather than the
// test-only FlushAll. Both events are emitted before the timer can fire, so
// the window provably contains both and the split is what keeps them apart.
func TestNotifyChange_OneOperationPerResponse_TimerPath(t *testing.T) {
	r := NewNotifyRegistry()
	r.flushDelay = 25 * time.Millisecond

	delivered := make(chan []FileNotifyInformation, 4)
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		ConnID:           1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/",
		ShareName:        "share1",
		CompletionFilter: AllValidCompletionFilterFlags,
		MaxOutputLength:  4096,
		AsyncCallback: func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
			delivered <- decodeFileNotifyInfos(resp.Buffer)
			return nil
		},
	})

	r.NotifyChange("share1", "/", "fname", FileActionAdded, NameChangeFilterFor("fname", false))
	r.NotifyChange("share1", "/", "fname", FileActionModified,
		FileNotifyChangeSize|FileNotifyChangeLastWrite|FileNotifyChangeAttributes)

	select {
	case got := <-delivered:
		checkRecords(t, "timer flush", got, []FileNotifyInformation{
			{Action: FileActionAdded, FileName: "fname"},
		})
	case <-time.After(5 * time.Second):
		t.Fatal("flush timer never delivered")
	}
}

// TestNotifyChange_BatchedOperationStaysInOneResponse is the other half: a
// CREATE that overwrites an existing file emits REMOVED + ADDED + MODIFIED and
// the client is entitled to all three in one response, because they are one
// operation.
func TestNotifyChange_BatchedOperationStaysInOneResponse(t *testing.T) {
	r := newTestNotifyRegistry()

	var got []FileNotifyInformation
	armWatcher(t, r, 10, &got)

	nameFilter := NameChangeFilterFor("fname", false)
	r.NotifyChanges("share1", "/", []NotifyEvent{
		{FileName: "fname", Action: FileActionRemoved, Filter: nameFilter},
		{FileName: "fname", Action: FileActionAdded, Filter: nameFilter},
		{FileName: "fname", Action: FileActionModified,
			Filter: FileNotifyChangeAttributes | FileNotifyChangeLastWrite | FileNotifyChangeSize},
	})
	// A later, separate operation must not join them.
	r.NotifyChange("share1", "/", "other", FileActionAdded, NameChangeFilterFor("other", false))
	r.FlushAll()

	checkRecords(t, "overwrite response", got, []FileNotifyInformation{
		{Action: FileActionRemoved, FileName: "fname"},
		{Action: FileActionAdded, FileName: "fname"},
		{Action: FileActionModified, FileName: "fname"},
	})
}

// TestNotifyChange_WriteModified_RespectsNarrowFilter verifies the WRITE-path
// MODIFIED event reaches a watcher that requested only FILE_NOTIFY_CHANGE_LAST_WRITE
// (a strict subset of the WRITE event's size|last-write|attributes mask), and
// does NOT reach a watcher requesting only FILE_NOTIFY_CHANGE_FILE_NAME.
func TestNotifyChange_WriteModified_RespectsNarrowFilter(t *testing.T) {
	r := newTestNotifyRegistry()

	lastWriteFired := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeLastWrite,
		MaxOutputLength:  4096,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			lastWriteFired = true
			return nil
		},
	})

	nameOnlyFired := false
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{2},
		SessionID:        1,
		MessageID:        11,
		AsyncId:          101,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			nameOnlyFired = true
			return nil
		},
	})

	// Same mask the WRITE handler emits.
	r.NotifyChange("share1", "/dir", "file.txt", FileActionModified,
		FileNotifyChangeSize|FileNotifyChangeLastWrite|FileNotifyChangeAttributes)
	r.FlushAll()

	if !lastWriteFired {
		t.Error("LAST_WRITE watcher should be notified of a WRITE MODIFIED event")
	}
	if nameOnlyFired {
		t.Error("FILE_NAME-only watcher must NOT be notified of a WRITE MODIFIED event")
	}
}

// TestFlushWatcher_StaleTimerAfterRearm_IsNoop verifies that a flushWatcher
// callback from a stale timer (fired after the watcher was replaced) is a
// no-op: it must NOT drain or unregister the new watcher, and must NOT invoke
// the new watcher's AsyncCallback.
func TestFlushWatcher_StaleTimerAfterRearm_IsNoop(t *testing.T) {
	r := newTestNotifyRegistry() // flushDelay = 1h (timers never fire spontaneously)

	fileID := [16]byte{0x55}
	var callbackCount atomic.Int32

	// Register watcher A — captures generation G1.
	wA := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 10, AsyncId: 100,
		WatchPath: "/d", ShareName: "s",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  64 * 1024,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			callbackCount.Add(1)
			return nil
		},
	}
	mustRegister(t, r, wA)
	genA := wA.generation

	// Buffer an event so flushTimer is set on wA.
	r.mu.Lock()
	r.bufferEventLocked(wA, FileNotifyInformation{Action: FileActionAdded, FileName: "x.txt"}, 1)
	r.mu.Unlock()

	// Cancel wA — unregisterLocked stops its timer (Stop returns false if
	// already fired, but under flushDelay=1h it hasn't fired).
	r.Unregister(fileID)

	// Register watcher B for the same fileID — gets generation G2 > G1.
	wB := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 11, AsyncId: 101,
		WatchPath: "/d", ShareName: "s",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  64 * 1024,
		AsyncCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			callbackCount.Add(1)
			return nil
		},
	}
	mustRegister(t, r, wB)

	if wB.generation == genA {
		t.Fatalf("wB.generation = %d must differ from genA = %d", wB.generation, genA)
	}

	// Simulate the stale timer firing: call flushWatcher with A's generation.
	// Before the fix this finds wB (same fileID) and destroys it.
	// After the fix the generation mismatch makes it a no-op.
	r.flushWatcher(fileID, genA)

	// wB must still be registered and its callback must NOT have been invoked.
	if got := r.WatcherCount(); got != 1 {
		t.Errorf("WatcherCount = %d after stale flushWatcher; want 1 (wB must survive)", got)
	}
	if n := callbackCount.Load(); n != 0 {
		t.Errorf("AsyncCallback invoked %d times; want 0 (stale timer must be no-op)", n)
	}

	// Legitimate flush for wB must still work.
	r.mu.Lock()
	r.bufferEventLocked(wB, FileNotifyInformation{Action: FileActionAdded, FileName: "y.txt"}, 1)
	r.mu.Unlock()
	r.flushWatcher(fileID, wB.generation)

	if n := callbackCount.Load(); n != 1 {
		t.Errorf("Legitimate flushWatcher invoked callback %d times; want 1", n)
	}
	if got := r.WatcherCount(); got != 0 {
		t.Errorf("WatcherCount = %d after legitimate flush; want 0", got)
	}
}

// TestArmedBuffer_RenameDoesNotDoubleChargeAncestorWatcher is the regression
// test for the NotifyRename double-charge bug on cross-directory renames.
//
// Scenario: a WatchTree watcher rooted at "/" is armed but has no live pending
// notify. A file is renamed from /src/old.txt to /dst/new.txt (cross-dir).
// The watcher should be charged exactly two FILE_NOTIFY_INFORMATION entries:
//   - RENAMED_OLD_NAME "src/old.txt"  (relative to "/")
//   - RENAMED_NEW_NAME "dst/new.txt"  (relative to "/")
//
// Before the fix, chargeArmedBuffer was called twice (once per parent path)
// with both filenames each time, charging 4 entries and triggering a false
// overflow. After the fix, chargeArmedRename charges each handle exactly once.
func TestArmedBuffer_RenameDoesNotDoubleChargeAncestorWatcher(t *testing.T) {
	r := newTestNotifyRegistry()

	fileID := [16]byte{0x77}
	var overflowCount atomic.Int32

	// Compute the exact byte cost of the two correct entries:
	//   RENAMED_OLD_NAME "src/old.txt"
	//   RENAMED_NEW_NAME "dst/new.txt"
	twoEntryCost := encodedNotifyEntrySize("src/old.txt") + encodedNotifyEntrySize("dst/new.txt")

	// MaxOutputLength = twoEntryCost: fits the correct 2 entries exactly, but
	// would overflow if 4 entries (the buggy path) were charged.
	maxLen := twoEntryCost

	first := &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 10, AsyncId: 100,
		WatchPath: "/", ShareName: "s",
		CompletionFilter: FileNotifyChangeFileName,
		WatchTree:        true,
		MaxOutputLength:  maxLen,
		AsyncCallback:    func(_, _, _ uint64, _ *ChangeNotifyResponse) error { return nil },
		OnOverflow: func(_ [16]byte) {
			overflowCount.Add(1)
		},
	}
	mustRegister(t, r, first)
	// Cancel to leave handle armed-but-unwatched.
	if r.UnregisterByAsyncId(100) == nil {
		t.Fatal("expected to unregister")
	}

	// Cross-directory rename: /src/old.txt -> /dst/new.txt
	r.NotifyRename("s", "/src", "old.txt", "/dst", "new.txt", FileNotifyChangeFileName)

	// The armed handle should be charged exactly 2 entries total.
	// Before fix: 4 entries charged -> overflow fires.
	// After fix:  2 entries charged -> no overflow (fits within maxLen exactly).
	if got := overflowCount.Load(); got != 0 {
		t.Errorf("OnOverflow fired %d times; want 0 (ancestor watcher double-charged by buggy path)", got)
	}

	// Confirm buffered bytes on the armed handle equals the 2-entry cost.
	r.mu.RLock()
	armed := r.armed[string(fileID[:])]
	var bufferedBytes uint32
	if armed != nil {
		bufferedBytes = armed.BufferedBytes
	}
	r.mu.RUnlock()

	if bufferedBytes != twoEntryCost {
		t.Errorf("armed.BufferedBytes = %d; want %d (exactly 2 rename entries)", bufferedBytes, twoEntryCost)
	}
}

// TestMarkInterimSent_AfterUnregister_StillDeliversFinal covers the ordering
// that leaves a cancelled CHANGE_NOTIFY without a final response.
//
// Every path that completes a pending notify (CANCEL, CLOSE, session/tree
// teardown, event delivery) removes it from the registry's maps first and
// only then queues the final response through QueueFinalAfterInterim. If the
// dispatcher writes the interim STATUS_PENDING inside that window, the
// interim-sent signal must still reach the notify — otherwise the queued
// final is deferred against a signal that has already passed and the client
// waits forever on a MessageID that never gets a response.
func TestMarkInterimSent_AfterUnregister_StillDeliversFinal(t *testing.T) {
	r := NewNotifyRegistry()

	var sent []types.Status
	notify := &PendingNotify{
		FileID:           [16]byte{3},
		SessionID:        42,
		ConnID:           1,
		MessageID:        7,
		AsyncId:          5741,
		WatchPath:        "/d",
		ShareName:        "s",
		CompletionFilter: FileNotifyChangeFileName,
		GateInterim:      true,
		AsyncCallback: func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
			sent = append(sent, resp.GetStatus())
			return nil
		},
	}
	mustRegister(t, r, notify)

	// CANCEL removes the watch...
	cancelled := r.CancelByMessageID(notify.ConnID, notify.MessageID)
	if cancelled == nil {
		t.Fatal("CancelByMessageID found no watch to cancel")
	}

	// ...the dispatcher writes the interim STATUS_PENDING here, after the
	// watch is gone from the maps but before CANCEL queues its final...
	r.MarkInterimSent(cancelled)

	// ...and only now does CANCEL queue STATUS_CANCELLED.
	r.QueueFinalAfterInterim(cancelled, func() {
		_ = cancelled.AsyncCallback(cancelled.SessionID, cancelled.MessageID, cancelled.AsyncId,
			&ChangeNotifyResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusCancelled}})
	})

	if len(sent) != 1 || sent[0] != types.StatusCancelled {
		t.Fatalf("cancelled CHANGE_NOTIFY got no final response: %v", sent)
	}
}

// TestNotifyRegistry_ConcurrentCancelRacesRegister drives the SMB2_CANCEL
// lookup and the CHANGE_NOTIFY registration on the same (ConnID, MessageID)
// from two goroutines with no imposed ordering — the interleaving a client
// produces when it fires NOTIFY and CANCEL back to back and the server
// dispatches each on its own goroutine.
//
// Exactly one of the two must win: either the cancel finds and removes the
// watch, or the register short-circuits with ErrAlreadyCancelled. The state
// this rules out is a cancel that found nothing while a live watch remains
// registered, because then nothing will ever complete that MessageID and the
// client blocks forever.
//
// The window is narrow, so the interleaving needs the race detector's
// scheduling perturbation to be hit reliably: under `-race` this failed on
// every run against a lookup and a tombstone-write taking the lock separately.
func TestNotifyRegistry_ConcurrentCancelRacesRegister(t *testing.T) {
	const iters = 2000
	for i := 0; i < iters; i++ {
		r := newTestNotifyRegistry()
		connID := uint64(i)
		messageID := uint64(i*2 + 1)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var cancelled *PendingNotify
		var regErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			cancelled = r.CancelByMessageID(connID, messageID)
		}()
		go func() {
			defer wg.Done()
			<-start
			regErr = r.Register(&PendingNotify{
				FileID: [16]byte{byte(i), byte(i >> 8)}, ConnID: connID,
				MessageID: messageID, AsyncId: uint64(i + 100000),
				WatchPath: "/x", ShareName: "s",
				CompletionFilter: FileNotifyChangeFileName,
			})
		}()
		close(start)
		wg.Wait()

		if cancelled == nil && regErr == nil {
			t.Fatalf("iter %d: cancel found nothing and register succeeded — "+
				"watch left live on a cancelled MessageID (watchers=%d)",
				i, r.WatcherCount())
		}
		if got := r.WatcherCount(); got != 0 {
			t.Fatalf("iter %d: WatcherCount = %d, want 0 (cancelled=%v regErr=%v)",
				i, got, cancelled != nil, regErr)
		}
	}
}

// TestNotifyRegistry_ConcurrentCloseRacesRegister is the CLOSE-side twin of
// TestNotifyRegistry_ConcurrentCancelRacesRegister. CLOSE completes a pending
// CHANGE_NOTIFY before it removes the handle from the open-file table, so a
// CHANGE_NOTIFY dispatched concurrently passes its own handle lookup and can
// reach Register after the close has already looked and found nothing.
//
// Exactly one of the two must win: either the close removes the watch and
// answers STATUS_NOTIFY_CLEANUP, or the register short-circuits with
// ErrHandleClosed. A close that found nothing while a live watch remains is
// the hang: the handle is gone, no further event can arrive for it, and
// nothing will ever complete that MessageID.
//
// As with the cancel twin, the window needs the race detector's scheduling
// perturbation to be hit reliably.
func TestNotifyRegistry_ConcurrentCloseRacesRegister(t *testing.T) {
	const iters = 2000
	for i := 0; i < iters; i++ {
		r := newTestNotifyRegistry()
		fileID := [16]byte{byte(i), byte(i >> 8), 0xC1}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var closed []*PendingNotify
		var regErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			closed = r.CloseByFileID(fileID)
		}()
		go func() {
			defer wg.Done()
			<-start
			regErr = r.Register(&PendingNotify{
				FileID: fileID, ConnID: uint64(i), MessageID: uint64(i*2 + 1),
				AsyncId: uint64(i + 200000), WatchPath: "/x", ShareName: "s",
				CompletionFilter: FileNotifyChangeFileName,
			})
		}()
		close(start)
		wg.Wait()

		if len(closed) == 0 && regErr == nil {
			t.Fatalf("iter %d: close found nothing and register succeeded — "+
				"watch left live on a closed handle (watchers=%d)",
				i, r.WatcherCount())
		}
		if got := r.WatcherCount(); got != 0 {
			t.Fatalf("iter %d: WatcherCount = %d, want 0 (closed=%v regErr=%v)",
				i, got, closed != nil, regErr)
		}
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
	h.NotifyRegistry = NewNotifyRegistry()
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
		AsyncNotifyCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
			return nil
		},
	}

	res, err := h.ChangeNotify(ctx, encodeChangeNotifyReq(0, 1000, fileID, FileNotifyChangeFileName))
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

// TestNotifyRegistry_PreArrivalCancel_StillArmsHandle covers the event-buffering
// side of the pre-arrival cancel short-circuit.
//
// smbtorture's notify.tcon fires CHANGE_NOTIFY, cancels it, then issues a
// second CHANGE_NOTIFY and makes a directory — and the server may process the
// mkdir before that second request registers. The armed handle is what carries
// the event across that gap. A watch that registers and is then cancelled
// leaves the handle armed, so a cancel that beats registration must too;
// otherwise the same client sequence loses the event depending only on which
// goroutine won the race.
func TestNotifyRegistry_PreArrivalCancel_StillArmsHandle(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xA7}

	base := func(msgID, asyncID uint64) *PendingNotify {
		return &PendingNotify{
			FileID: fileID, SessionID: 1, ConnID: 1, MessageID: msgID,
			AsyncId: asyncID, WatchPath: "/d", ShareName: "s",
			CompletionFilter: FileNotifyChangeDirName, WatchTree: true,
			MaxOutputLength: 1000,
		}
	}

	// CANCEL beats the first CHANGE_NOTIFY to the registry.
	if got := r.CancelByMessageID(1, 10); got != nil {
		t.Fatalf("CancelByMessageID found a watch that was never registered: %+v", got)
	}
	if err := r.Register(base(10, 100)); !errors.Is(err, ErrAlreadyCancelled) {
		t.Fatalf("Register = %v, want ErrAlreadyCancelled", err)
	}

	// An event now arrives with no live watcher. It must buffer on the handle.
	r.NotifyChange("s", "/d", "subdir-name", FileActionAdded, FileNotifyChangeDirName)

	// The client re-issues; the buffered event must be replayed to it.
	var got []FileNotifyInformation
	n := base(11, 101)
	n.AsyncCallback = func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
		got = append(got, decodeFileNotifyInfos(resp.Buffer)...)
		return nil
	}
	if err := r.Register(n); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	r.FlushAll()

	if len(got) != 1 || got[0].FileName != "subdir-name" {
		t.Fatalf("event fired between cancel and re-issue was dropped: %+v", got)
	}
}

// notifyQueueFixture registers `count` CHANGE_NOTIFYs on one handle and
// returns the events each one is answered with, indexed by MessageID (which is
// 10, 20, 30... in registration order).
func notifyQueueFixture(t *testing.T, r *NotifyRegistry, fileID [16]byte, count int) map[uint64]*[]FileNotifyInformation {
	t.Helper()
	sinks := map[uint64]*[]FileNotifyInformation{}
	for i := 0; i < count; i++ {
		mid := uint64(10 * (i + 1))
		sink := &[]FileNotifyInformation{}
		sinks[mid] = sink
		mustRegister(t, r, &PendingNotify{
			FileID: fileID, SessionID: 1, ConnID: 1, MessageID: mid,
			AsyncId: mid + 500, WatchPath: "/d", ShareName: "s",
			MaxOutputLength:  1000,
			CompletionFilter: FileNotifyChangeFileName,
			AsyncCallback: func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
				*sink = append(*sink, decodeFileNotifyInfos(resp.Buffer)...)
				return nil
			},
		})
	}
	return sinks
}

func namesOf(events []FileNotifyInformation) []string {
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.FileName)
	}
	return names
}

// TestNotifyRegistry_EventGoesToOldestWaiterOnly is the smb2.notify.double
// shape: two CHANGE_NOTIFYs outstanding on one handle, one change each, in
// order. Both must be live at once, and a change must not be copied to every
// waiter — that answers a request the client has not read yet with a change it
// will see twice, and the next change then has nobody left to go to.
func TestNotifyRegistry_EventGoesToOldestWaiterOnly(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xD0}
	sinks := notifyQueueFixture(t, r, fileID, 2)

	r.NotifyChange("s", "/d", "first.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if got := namesOf(*sinks[10]); len(got) != 1 || got[0] != "first.txt" {
		t.Fatalf("oldest waiter got %v, want [first.txt]", got)
	}
	if got := namesOf(*sinks[20]); len(got) != 0 {
		t.Fatalf("second waiter got %v, want nothing: the change belongs to the request ahead of it", got)
	}

	r.NotifyChange("s", "/d", "second.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if got := namesOf(*sinks[20]); len(got) != 1 || got[0] != "second.txt" {
		t.Fatalf("second waiter got %v after the first was answered, want [second.txt]", got)
	}
	if got := namesOf(*sinks[10]); len(got) != 1 {
		t.Fatalf("oldest waiter answered twice: %v", got)
	}
	if got := r.WatcherCount(); got != 0 {
		t.Errorf("WatcherCount = %d, want 0 after both were answered", got)
	}
}

// TestNotifyRegistry_ReleasingOldestPromotesSecondNotLast pins the removal
// order inside a handle's waiter list.
//
// A swap-remove (list[i] = list[len-1]) produces the same *set* as an
// order-preserving removal and is indistinguishable when the middle element
// goes. It is only visible when the HEAD goes: swap-remove moves the NEWEST
// waiter into the oldest slot, so the next change is handed to the request the
// client will read last.
func TestNotifyRegistry_ReleasingOldestPromotesSecondNotLast(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xD2}
	sinks := notifyQueueFixture(t, r, fileID, 3)

	// Answer and remove the head.
	r.NotifyChange("s", "/d", "first.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if got := namesOf(*sinks[10]); len(got) != 1 {
		t.Fatalf("head waiter got %v, want one change", got)
	}

	// The next change belongs to the SECOND request, not the third.
	r.NotifyChange("s", "/d", "second.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if got := namesOf(*sinks[20]); len(got) != 1 || got[0] != "second.txt" {
		t.Fatalf("waiter 20 got %v, want [second.txt] — releasing the head must promote the "+
			"next arrival, not the last one", got)
	}
	if got := namesOf(*sinks[30]); len(got) != 0 {
		t.Fatalf("waiter 30 got %v, want nothing: waiter 20 is still ahead of it", got)
	}
}

// TestNotifyRegistry_RegisterLeavesBufferedEventsToTheEarliestArrival is the
// registry half of the earliest-outstanding rule the handler's
// TakeBufferedEvents fast path already follows.
//
// Two CHANGE_NOTIFYs arrive on the wire in order 9, 10 and are dispatched in
// the opposite order, so 10 reaches Register first. The events buffered on the
// handle belong to 9 — the request the client is blocked on and the reason it
// has not produced another event. Replaying them into 10 answers a request the
// client reads second and strands the one it reads first.
func TestNotifyRegistry_RegisterLeavesBufferedEventsToTheEarliestArrival(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xD1}

	base := func(msgID uint64) *PendingNotify {
		return &PendingNotify{
			FileID: fileID, SessionID: 1, ConnID: 1, MessageID: msgID,
			AsyncId: msgID + 500, WatchPath: "/d", ShareName: "s",
			MaxOutputLength: 1000, CompletionFilter: FileNotifyChangeFileName,
		}
	}

	// The handle is armed and an event lands with no watch registered.
	r.Arm(base(9))
	r.NotifyChange("s", "/d", "a.txt", FileActionAdded, FileNotifyChangeFileName)

	// Wire order 9 then 10; 10 is dispatched first and registers.
	done9 := r.MarkNotifyInFlight(fileID, 1, 9)
	done10 := r.MarkNotifyInFlight(fileID, 1, 10)
	defer done9()
	defer done10()

	var late []FileNotifyInformation
	n := base(10)
	n.AsyncCallback = func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
		late = append(late, decodeFileNotifyInfos(resp.Buffer)...)
		return nil
	}
	mustRegister(t, r, n)
	r.FlushAll()

	if len(late) != 0 {
		t.Fatalf("the later arrival took the buffered events: %v", namesOf(late))
	}

	// They are still there for messageID 9, which is what the client is
	// waiting on.
	got := r.TakeBufferedEvents(fileID, FileNotifyChangeFileName, false)
	if len(got) != 1 || got[0].FileName != "a.txt" {
		t.Fatalf("earliest arrival found %v, want [a.txt]", namesOf(got))
	}
}

// TestNotifyRegistry_CloseCompletesEveryOutstandingWatch: closing the handle
// has to answer every request queued on it, not just the oldest. One left
// behind is a MessageID nothing will ever respond to.
func TestNotifyRegistry_CloseCompletesEveryOutstandingWatch(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xD3}
	notifyQueueFixture(t, r, fileID, 3)

	closed := r.CloseByFileID(fileID)
	if len(closed) != 3 {
		t.Fatalf("CloseByFileID returned %d watches, want 3", len(closed))
	}
	seen := map[uint64]bool{}
	for _, n := range closed {
		seen[n.MessageID] = true
	}
	for _, mid := range []uint64{10, 20, 30} {
		if !seen[mid] {
			t.Errorf("messageID %d was not completed by the close", mid)
		}
	}
	if got := r.WatcherCount(); got != 0 {
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
	e.h.NotifyRegistry = NewNotifyRegistry()

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

	if got := e.h.NotifyRegistry.closeTombstoneCount(); got != 0 {
		t.Fatalf("close tombstones after tearing down 64 file handles = %d, want 0", got)
	}
}

// TestNotifyRegistry_CloseTombstonesEvenWhenAWatchWasFound covers the case
// where the close path finds a watch to complete.
//
// Completing that one says nothing about a second CHANGE_NOTIFY already past
// its open-file lookup and still on its way to Register. If the close only
// tombstoned on a miss, that second request would register a watch on a handle
// that no longer exists and wait forever — the handle is going away regardless
// of what happened to be registered at the instant the close took the lock.
func TestNotifyRegistry_CloseTombstonesEvenWhenAWatchWasFound(t *testing.T) {
	r := newTestNotifyRegistry()
	fileID := [16]byte{0xE5}

	mustRegister(t, r, &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 10, AsyncId: 700,
		WatchPath: "/d", ShareName: "s", MaxOutputLength: 1000,
		CompletionFilter: FileNotifyChangeFileName,
	})

	// The close finds that first watch and completes it.
	if got := r.CloseByFileID(fileID); got == nil {
		t.Fatal("CloseByFileID did not return the registered watch")
	}

	// A second CHANGE_NOTIFY on the same handle, still in flight, now reaches
	// Register. It must be refused rather than left pending on a dead handle.
	err := r.Register(&PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 11, AsyncId: 701,
		WatchPath: "/d", ShareName: "s", MaxOutputLength: 1000,
		CompletionFilter: FileNotifyChangeFileName,
	})
	if !errors.Is(err, ErrHandleClosed) {
		t.Fatalf("Register after close = %v, want ErrHandleClosed", err)
	}
	if got := r.WatcherCount(); got != 0 {
		t.Fatalf("WatcherCount = %d, want 0 — watch left live on a closed handle", got)
	}
}

// notifyHandlerEnv builds a handler with one armed directory handle whose
// buffered events are ready to be collected.
func notifyHandlerEnv(t *testing.T, fileID [16]byte) (*Handler, *NotifyRegistry) {
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
	mustRegister(t, h.NotifyRegistry, &PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, MessageID: 1, AsyncId: 1,
		WatchPath: "/d", ShareName: "share1", MaxOutputLength: 1000,
		CompletionFilter: FileNotifyChangeFileName, WatchTree: true,
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
		AsyncNotifyCallback: func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
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

	r.NotifyChange("share1", "/d", "a.txt", FileActionAdded, FileNotifyChangeFileName)

	reserved := 0
	res, err := h.ChangeNotify(notifyCtx(7, &reserved),
		encodeChangeNotifyReq(SMB2WatchTree, 1000, fileID, FileNotifyChangeFileName))
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

	r.NotifyChange("share1", "/d", "b.txt", FileActionAdded, FileNotifyChangeFileName)

	// The CANCEL for this MessageID lands before the CHANGE_NOTIFY is dispatched.
	if got := r.CancelByMessageID(1, 9); got != nil {
		t.Fatalf("setup: CancelByMessageID found a watch it should not have: %+v", got)
	}

	reserved := 0
	res, err := h.ChangeNotify(notifyCtx(9, &reserved),
		encodeChangeNotifyReq(SMB2WatchTree, 1000, fileID, FileNotifyChangeFileName))
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
// invariant #2131 established. That PR made every watch the registry removes
// get an answer; this one must not produce a second answer for the same
// MessageID. A request answered synchronously was never queued, so the CANCEL
// that follows it has nothing to complete.
func TestChangeNotify_SyncAnswerThenCancelRespondsExactlyOnce(t *testing.T) {
	fileID := [16]byte{0x93}
	h, r := notifyHandlerEnv(t, fileID)

	r.NotifyChange("share1", "/d", "c.txt", FileActionAdded, FileNotifyChangeFileName)

	var asyncResponses int
	ctx := notifyCtx(11, new(int))
	ctx.AsyncNotifyCallback = func(_, _, _ uint64, _ *ChangeNotifyResponse) error {
		asyncResponses++
		return nil
	}

	res, err := h.ChangeNotify(ctx, encodeChangeNotifyReq(SMB2WatchTree, 1000, fileID, FileNotifyChangeFileName))
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

// TestTakeBufferedEvents_LeavesNonMatchingEvents checks that collecting events
// for one request does not consume events it would not have reported.
func TestTakeBufferedEvents_LeavesNonMatchingEvents(t *testing.T) {
	fileID := [16]byte{0x94}
	_, r := notifyHandlerEnv(t, fileID)

	r.NotifyChange("share1", "/d", "top.txt", FileActionAdded, FileNotifyChangeFileName)
	r.NotifyChange("share1", "/d/sub", "deep.txt", FileActionAdded, FileNotifyChangeFileName)

	// A non-recursive request takes only the top-level entry.
	got := r.TakeBufferedEvents(fileID, FileNotifyChangeFileName, false)
	if len(got) != 1 || got[0].FileName != "top.txt" {
		t.Fatalf("non-recursive take = %+v, want only top.txt", got)
	}

	// The subdirectory entry is still there for a recursive request.
	got = r.TakeBufferedEvents(fileID, FileNotifyChangeFileName, true)
	if len(got) != 1 || !strings.Contains(got[0].FileName, "deep.txt") {
		t.Fatalf("recursive take = %+v, want the subdirectory entry", got)
	}
	if got = r.TakeBufferedEvents(fileID, FileNotifyChangeFileName, true); got != nil {
		t.Fatalf("third take = %+v, want nil — events must be consumed once", got)
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
		encodeChangeNotifyReq(SMB2WatchTree, 1000, fileID, FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify: %v", err)
	}
	if res.Status != types.StatusPending {
		t.Fatalf("status = %v, want STATUS_PENDING (nothing buffered yet)", res.Status)
	}

	// An event on the directory the handle actually refers to must reach it.
	var got []FileNotifyInformation
	r := h.NotifyRegistry
	var watch *PendingNotify
	r.RangeWatchers(func(n *PendingNotify) bool {
		watch = n
		return true
	})
	if watch == nil {
		t.Fatal("no watch registered")
	}
	if watch.WatchPath != "/d" {
		t.Fatalf("registered WatchPath = %q, want %q", watch.WatchPath, "/d")
	}
	watch.AsyncCallback = func(_, _, _ uint64, resp *ChangeNotifyResponse) error {
		got = append(got, decodeFileNotifyInfos(resp.Buffer)...)
		return nil
	}
	// The dispatcher's PostSend hook does not run in a unit test, so the
	// interim-sent signal has to be delivered by hand or the final response
	// stays deferred. Outside RangeWatchers: that holds the registry lock.
	r.MarkInterimSent(watch)

	r.NotifyChange("share1", "/d", "x.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()

	if len(got) != 1 || got[0].FileName != "x.txt" {
		t.Fatalf("watch opened as /d/sub/.. saw %+v, want the event on /d", got)
	}
}

// TestTakeBufferedEvents_KeepsByteCountForRemainingEvents checks the overflow
// byte counter still measures what is actually buffered after a partial take.
//
// BufferedBytes is what the proactive overflow latch sizes the backlog with.
// Zeroing it while non-matching entries remain would under-count them, and the
// latch would stop firing for a backlog that is really still growing.
func TestTakeBufferedEvents_KeepsByteCountForRemainingEvents(t *testing.T) {
	fileID := [16]byte{0x96}
	_, r := notifyHandlerEnv(t, fileID)

	r.NotifyChange("share1", "/d", "top.txt", FileActionAdded, FileNotifyChangeFileName)
	r.NotifyChange("share1", "/d/sub", "deep.txt", FileActionAdded, FileNotifyChangeFileName)

	if got := r.TakeBufferedEvents(fileID, FileNotifyChangeFileName, false); len(got) != 1 {
		t.Fatalf("non-recursive take = %+v, want one entry", got)
	}

	var bytes uint32
	var remaining int
	r.mu.Lock()
	if a, ok := r.armed[string(fileID[:])]; ok {
		bytes, remaining = a.BufferedBytes, len(a.BufferedEvents)
	}
	r.mu.Unlock()

	if remaining != 1 {
		t.Fatalf("remaining buffered events = %d, want 1", remaining)
	}
	if bytes == 0 {
		t.Fatal("BufferedBytes = 0 while an event is still buffered — the overflow latch has nothing to measure")
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
	r.Arm(&PendingNotify{
		FileID: fileID, SessionID: 1, ConnID: 1, WatchPath: "/d", ShareName: "share1",
		CompletionFilter: FileNotifyChangeFileName, WatchTree: false, MaxOutputLength: 1000,
	})

	// A top-level event so this request has something to be answered with.
	r.NotifyChange("share1", "/d", "top.txt", FileActionAdded, FileNotifyChangeFileName)

	// A RECURSIVE request is answered synchronously from that event.
	reserved := 0
	res, err := h.ChangeNotify(notifyCtx(31, &reserved),
		encodeChangeNotifyReq(SMB2WatchTree, 1000, fileID, FileNotifyChangeFileName))
	if err != nil {
		t.Fatalf("ChangeNotify: %v", err)
	}
	if res.Status != types.StatusSuccess {
		t.Fatalf("status = %v, want STATUS_SUCCESS", res.Status)
	}

	// The handle must now be armed recursive, so a subdirectory event buffers.
	r.NotifyChange("share1", "/d/sub", "deep.txt", FileActionAdded, FileNotifyChangeFileName)

	got := r.TakeBufferedEvents(fileID, FileNotifyChangeFileName, true)
	if len(got) != 1 || !strings.Contains(got[0].FileName, "deep.txt") {
		t.Fatalf("subdirectory event after a recursive sync answer = %+v, want it buffered — "+
			"the armed handle kept the previous request's non-recursive flag", got)
	}
}
