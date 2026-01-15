package transfer

import (
	"testing"
)

func TestTransferRequest_Fields(t *testing.T) {
	req := NewTransferRequest("export", "test-handle", "export/test.txt")

	if req.ShareName != "export" {
		t.Errorf("ShareName = %s, want export", req.ShareName)
	}

	if req.FileHandle != "test-handle" {
		t.Errorf("FileHandle = %s, want test-handle", req.FileHandle)
	}

	if req.PayloadID != "export/test.txt" {
		t.Errorf("PayloadID = %s, want export/test.txt", req.PayloadID)
	}

	if req.Priority != 0 {
		t.Errorf("Priority = %d, want 0", req.Priority)
	}
}

func TestTransferRequest_WithPriority(t *testing.T) {
	req := NewTransferRequest("export", "handle", "content-id")
	highPriority := req.WithPriority(10)

	// Original should be unchanged (value receiver returns copy)
	if req.Priority != 0 {
		t.Errorf("original Priority = %d, want 0", req.Priority)
	}

	// New request should have priority
	if highPriority.Priority != 10 {
		t.Errorf("highPriority.Priority = %d, want 10", highPriority.Priority)
	}

	// Other fields should be copied
	if highPriority.ShareName != req.ShareName {
		t.Errorf("ShareName mismatch after WithPriority")
	}
	if highPriority.PayloadID != req.PayloadID {
		t.Errorf("PayloadID mismatch after WithPriority")
	}
}
