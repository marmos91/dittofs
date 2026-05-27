package handlers

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

func mustRegister(t *testing.T, r *NotifyRegistry, n *PendingNotify) {
	t.Helper()
	if err := r.Register(n); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
}

func TestNotifyRegistry_RegisterAndUnregister(t *testing.T) {
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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

// TestNotifyRegistry_UnregisterByMessageID_DisambiguatesByConnID verifies the
// CANCEL-by-MessageID path scopes its lookup to the requesting connection,
// so two pending notifies sharing a MessageID across two TCP connections
// can each be cancelled independently.
func TestNotifyRegistry_UnregisterByMessageID_DisambiguatesByConnID(t *testing.T) {
	r := NewNotifyRegistry()

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

	got := r.UnregisterByMessageID(1, 521)
	if got == nil || got.AsyncId != 1 {
		t.Fatalf("UnregisterByMessageID(connID=1) returned %+v, want A", got)
	}
	// B must still be there.
	if got := r.UnregisterByMessageID(2, 521); got == nil || got.AsyncId != 2 {
		t.Fatalf("UnregisterByMessageID(connID=2) returned %+v, want B", got)
	}
}

func TestNotifyRegistry_UnregisterByMessageID(t *testing.T) {
	r := NewNotifyRegistry()

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
	removed := r.UnregisterByMessageID(7, 42)
	if removed == nil {
		t.Fatal("expected non-nil removed notify")
	}
	if removed.AsyncId != 99 {
		t.Errorf("expected asyncId 99, got %d", removed.AsyncId)
	}

	// Should not find it again
	removed = r.UnregisterByMessageID(7, 42)
	if removed != nil {
		t.Error("expected nil on second unregister")
	}
}

func TestNotifyRegistry_UnregisterByAsyncId(t *testing.T) {
	r := NewNotifyRegistry()

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

func TestNotifyRegistry_ReplaceExisting(t *testing.T) {
	r := NewNotifyRegistry()

	fileID := [16]byte{5}

	// Register first notify
	mustRegister(t, r, &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/old",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})

	// Register replacement (same FileID, different path)
	mustRegister(t, r, &PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		MessageID:        20,
		AsyncId:          200,
		WatchPath:        "/new",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})

	// Old path should be empty
	watchers := r.GetWatchersForPath("/old")
	if len(watchers) != 0 {
		t.Errorf("expected 0 watchers on old path, got %d", len(watchers))
	}

	// New path should have the replacement
	watchers = r.GetWatchersForPath("/new")
	if len(watchers) != 1 {
		t.Fatalf("expected 1 watcher on new path, got %d", len(watchers))
	}
	if watchers[0].AsyncId != 200 {
		t.Errorf("expected asyncId 200, got %d", watchers[0].AsyncId)
	}
}

func TestNotifyRegistry_MultipleWatchers(t *testing.T) {
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
		{"reserved-only bit is invalid", 0x80000000, false},
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

func TestNotifyRmdir_SendsCleanupToWatchersOnRemovedDir(t *testing.T) {
	r := NewNotifyRegistry()

	var receivedStatus types.Status
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/parent/target",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			receivedStatus = response.GetStatus()
			return nil
		},
	})

	// Remove the directory being watched
	r.NotifyRmdir("share1", "/parent", "target")
	r.FlushAll()

	if receivedStatus != types.StatusNotifyCleanup {
		t.Errorf("expected STATUS_NOTIFY_CLEANUP (0x%08X), got 0x%08X",
			uint32(types.StatusNotifyCleanup), uint32(receivedStatus))
	}
}

func TestNotifyRmdir_NotifiesParentWatcher(t *testing.T) {
	r := NewNotifyRegistry()

	var parentNotified bool
	mustRegister(t, r, &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/parent",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeDirName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			parentNotified = true
			return nil
		},
	})

	r.NotifyRmdir("share1", "/parent", "child")
	r.FlushAll()

	if !parentNotified {
		t.Error("parent watcher should receive FileActionRemoved notification for rmdir")
	}
}

func TestUnregisterAllForSession(t *testing.T) {
	r := NewNotifyRegistry()

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

	// Reserved bits alone (no valid bits) should be rejected
	reservedOnlyBits := []uint32{0x00001000, 0x00010000, 0x01000000, 0x80000000}
	for _, bit := range reservedOnlyBits {
		if IsValidCompletionFilter(bit) {
			t.Errorf("IsValidCompletionFilter(0x%08X) = true, want false (reserved-only)", bit)
		}
	}

	// Reserved bits combined with valid bits should be accepted (Windows behavior)
	for _, bit := range reservedOnlyBits {
		mixed := bit | FileNotifyChangeFileName
		if !IsValidCompletionFilter(mixed) {
			t.Errorf("IsValidCompletionFilter(0x%08X) = false, want true (valid + reserved)", mixed)
		}
	}
}

func TestNotifyChange_OverflowWithMultipleChanges(t *testing.T) {
	// Verify that when we manually build a notification that would exceed
	// MaxOutputLength, the registry sends STATUS_NOTIFY_ENUM_DIR.
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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

	// Disarm clears the armed slot — events fired after this must NOT
	// re-trip overflow (the handle has been closed).
	if !r.Disarm(fileID) {
		t.Errorf("expected Disarm to report removal")
	}
	r.NotifyChange("share1", "/basedir_ovf", "post-close.txt", FileActionAdded, FileNotifyChangeFileName)
	r.FlushAll()
	if atomic.LoadInt32(&overflowFireCount) != 1 {
		t.Errorf("expected no additional OnOverflow after Disarm, got count=%d", overflowFireCount)
	}
}

// TestArmedBuffer_ResetClearsOverflowForNextWindow exercises the
// ResetArmedOverflow path: after the handler consumes the sticky overflow
// (returning STATUS_NOTIFY_ENUM_DIR) it must reset the armed counter so
// the next batch of events accumulates against the freshly advertised
// MaxOutputLength rather than re-tripping immediately.
func TestArmedBuffer_ResetClearsOverflowForNextWindow(t *testing.T) {
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

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
	r := NewNotifyRegistry()

	// Same session, different shares
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{1}, SessionID: 100, MessageID: 10, AsyncId: 1000,
		WatchPath: "/dir1", ShareName: "share1", CompletionFilter: FileNotifyChangeFileName,
	})
	mustRegister(t, r, &PendingNotify{
		FileID: [16]byte{2}, SessionID: 100, MessageID: 20, AsyncId: 2000,
		WatchPath: "/dir1", ShareName: "share2", CompletionFilter: FileNotifyChangeFileName,
	})

	// Disconnect share1 tree only
	removed := r.UnregisterAllForTree(100, "share1")
	if len(removed) != 1 {
		t.Errorf("expected 1 watcher removed for share1, got %d", len(removed))
	}

	// share2 watcher must remain
	watchers := r.GetWatchersForPath("/dir1")
	if len(watchers) != 1 || watchers[0].ShareName != "share2" {
		t.Errorf("expected share2 watcher to remain, got %d watchers", len(watchers))
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

			h.StoreOpenFile(&OpenFile{
				FileID:        fileID,
				TreeID:        treeID,
				SessionID:     sessionID,
				Path:          "/HPERM",
				ShareName:     "share1",
				DesiredAccess: tc.desiredAccess,
				GrantedAccess: tc.grantedAccess,
				IsDirectory:   true,
			})

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

	openFile := &OpenFile{
		FileID:        fileID,
		IsDirectory:   true,
		ShareName:     "share1",
		Path:          "/dir",
		SessionID:     1,
		TreeID:        1,
		DesiredAccess: 0x00000001, // FILE_LIST_DIRECTORY
		GrantedAccess: 0x00000001,
	}
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
	openFile := &OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1",
		Path: "/dir", SessionID: 1, TreeID: 1, DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}
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
	openFile := &OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1",
		Path: "/dir", SessionID: 1, TreeID: 1, DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}
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
	r := NewNotifyRegistry()

	const connID, messageID uint64 = 7, 42

	// CANCEL arrives first — no matching entry, so the handler drops a
	// tombstone for the future Register to find.
	r.MarkPendingCancel(connID, messageID)

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
	r := NewNotifyRegistry()
	r.MarkPendingCancel(1, 10)

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
	r := NewNotifyRegistry()
	r.MarkPendingCancel(1, 99)

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
	openFile := &OpenFile{
		FileID: fileID, IsDirectory: true, ShareName: "share1",
		Path: "/dir", SessionID: 1, TreeID: 1,
		DesiredAccess: 0x00000001, GrantedAccess: 0x00000001,
	}
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
	h.NotifyRegistry.MarkPendingCancel(ctx.ConnID, ctx.MessageID)

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
// caused #623. We spin up many goroutines that each fire MarkPendingCancel
// followed by Register on the same (ConnID, MessageID). Either outcome is
// correct (tombstone consumed before Register, or Register raced past) but
// no goroutine can land in a state where a watcher remains registered for a
// (ConnID, MessageID) we already tombstoned — the smbtorture hang condition.
func TestNotifyRegistry_ConcurrentCancelBeforeRegister(t *testing.T) {
	const iters = 200
	for i := 0; i < iters; i++ {
		r := NewNotifyRegistry()
		connID := uint64(i)
		messageID := uint64(i*2 + 1)
		fileID := [16]byte{byte(i), byte(i >> 8)}

		done := make(chan struct{}, 2)
		// Cancel first, then notify — guarantees ErrAlreadyCancelled.
		go func() {
			r.MarkPendingCancel(connID, messageID)
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
