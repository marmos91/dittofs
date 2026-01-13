package transfer

import (
	"testing"
)

func TestDefaultEntry_Interface(t *testing.T) {
	entry := NewDefaultEntry("export", []byte("test-handle"), "export/test.txt")

	if entry.ShareName() != "export" {
		t.Errorf("ShareName() = %s, want export", entry.ShareName())
	}

	if string(entry.FileHandle()) != "test-handle" {
		t.Errorf("FileHandle() = %s, want test-handle", entry.FileHandle())
	}

	if entry.ContentID() != "export/test.txt" {
		t.Errorf("ContentID() = %s, want export/test.txt", entry.ContentID())
	}

	if entry.Priority() != 0 {
		t.Errorf("Priority() = %d, want 0", entry.Priority())
	}
}

func TestDefaultEntry_WithPriority(t *testing.T) {
	entry := NewDefaultEntry("export", []byte("handle"), "content-id")
	highPriority := entry.WithPriority(10)

	// Original should be unchanged
	if entry.Priority() != 0 {
		t.Errorf("original Priority() = %d, want 0", entry.Priority())
	}

	// New entry should have priority
	if highPriority.Priority() != 10 {
		t.Errorf("highPriority.Priority() = %d, want 10", highPriority.Priority())
	}

	// Other fields should be copied
	if highPriority.ShareName() != entry.ShareName() {
		t.Errorf("ShareName mismatch after WithPriority")
	}
	if highPriority.ContentID() != entry.ContentID() {
		t.Errorf("ContentID mismatch after WithPriority")
	}
}

func TestDefaultEntry_ImplementsInterface(t *testing.T) {
	var _ TransferQueueEntry = (*DefaultEntry)(nil)
}
