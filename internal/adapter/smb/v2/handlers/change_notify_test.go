package handlers

import (
	"sync/atomic"
	"testing"

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

func TestNotifyRegistry_UnregisterByMessageID(t *testing.T) {
	r := NewNotifyRegistry()

	notify := &PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        100,
		MessageID:        42,
		AsyncId:          99,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	}
	mustRegister(t, r, notify)

	// Unregister by message ID
	removed := r.UnregisterByMessageID(42)
	if removed == nil {
		t.Fatal("expected non-nil removed notify")
	}
	if removed.AsyncId != 99 {
		t.Errorf("expected asyncId 99, got %d", removed.AsyncId)
	}

	// Should not find it again
	removed = r.UnregisterByMessageID(42)
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
	r.NotifyChange("share1", "/dir", "test.txt", FileActionAdded)

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
	r.NotifyChange("share2", "/dir", "test.txt", FileActionAdded)

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
	r.NotifyChange("share1", "/subdir", "test.txt", FileActionAdded)

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
	r.NotifyChange("share1", "/subdir", "test.txt", FileActionAdded)

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

	r.NotifyRename("share1", "/dir", "old.txt", "/dir", "new.txt")

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
	r.NotifyRename("share1", "/src", "old.txt", "/dst", "new.txt")

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

	r.NotifyChange("share1", "/dir", "test.txt", FileActionAdded)

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
		r.NotifyChange("share1", "/dir", "a.txt", FileActionAdded)
		done <- struct{}{}
	}()
	go func() {
		r.NotifyChange("share1", "/dir", "b.txt", FileActionAdded)
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
	r.NotifyChange("share1", "/dir", "file:stream:$DATA", FileActionAdded)

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
	r.NotifyChange("share1", "/dir", "file:stream:$DATA", FileActionModified)

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
	r.NotifyChange("share1", "/dir", "file:stream:$DATA", FileActionModified)

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
	r.NotifyChange("share1", "/dir", "file.txt", FileActionModified)

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
	r.NotifyChange("share1", "/dir", "test.txt", FileActionAdded)

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
		{"invalid flag bit", 0x80000000, false},
		{"valid + invalid mixed", FileNotifyChangeFileName | 0x80000000, false},
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

	// Reserved bits above 0x00000FFF should be rejected
	reservedBits := []uint32{0x00001000, 0x00010000, 0x01000000, 0x80000000}
	for _, bit := range reservedBits {
		if IsValidCompletionFilter(bit) {
			t.Errorf("IsValidCompletionFilter(0x%08X) = true, want false (reserved bit)", bit)
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
	r.NotifyChange("share1", "/dir", "longfilename.txt", FileActionAdded)

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
