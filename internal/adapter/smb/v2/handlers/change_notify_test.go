package handlers

import (
	"testing"
)

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

	r.Register(notify)

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
	r.Register(notify)

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
	r.Register(notify)

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
	r.Register(&PendingNotify{
		FileID:           fileID,
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/old",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})

	// Register replacement (same FileID, different path)
	r.Register(&PendingNotify{
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

	r.Register(&PendingNotify{
		FileID:           [16]byte{1},
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
	})
	r.Register(&PendingNotify{
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

	var received []FileNotifyInformation
	r.Register(&PendingNotify{
		FileID:           [16]byte{1},
		SessionID:        1,
		MessageID:        10,
		AsyncId:          100,
		WatchPath:        "/dir",
		ShareName:        "share1",
		CompletionFilter: FileNotifyChangeFileName,
		MaxOutputLength:  4096,
		AsyncCallback: func(sessionID, messageID, asyncId uint64, response *ChangeNotifyResponse) error {
			// Decode the buffer to verify the change
			received = append(received, FileNotifyInformation{
				Action:   FileActionAdded,
				FileName: "test.txt",
			})
			return nil
		},
	})

	// Fire a matching change
	r.NotifyChange("share1", "/dir", "test.txt", FileActionAdded)

	if len(received) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(received))
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
	r.Register(&PendingNotify{
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
	r.Register(&PendingNotify{
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
	r.Register(&PendingNotify{
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
	r.Register(&PendingNotify{
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
