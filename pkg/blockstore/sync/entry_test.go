package sync

import (
	"testing"
)

func TestNewBlockUploadRequest(t *testing.T) {
	req := NewBlockUploadRequest("export/test.txt", 2)

	if req.PayloadID != "export/test.txt" {
		t.Errorf("PayloadID = %s, want export/test.txt", req.PayloadID)
	}

	if req.Type != TransferUpload {
		t.Errorf("Type = %v, want TransferUpload", req.Type)
	}

	if req.BlockIdx != 2 {
		t.Errorf("BlockIdx = %d, want 2", req.BlockIdx)
	}

	if req.Done != nil {
		t.Error("Done channel should be nil for async uploads")
	}
}

func TestNewDownloadRequest(t *testing.T) {
	done := make(chan error, 1)
	req := NewDownloadRequest("payload-id", 2, done)

	if req.Type != TransferDownload {
		t.Errorf("Type = %v, want TransferDownload", req.Type)
	}
	if req.PayloadID != "payload-id" {
		t.Errorf("PayloadID = %s, want payload-id", req.PayloadID)
	}
	if req.BlockIdx != 2 {
		t.Errorf("BlockIdx = %d, want 2", req.BlockIdx)
	}
	if req.Done != done {
		t.Error("Done channel not set correctly")
	}
}

func TestNewPrefetchRequest(t *testing.T) {
	req := NewPrefetchRequest("payload-id", 4)

	if req.Type != TransferPrefetch {
		t.Errorf("Type = %v, want TransferPrefetch", req.Type)
	}
	if req.PayloadID != "payload-id" {
		t.Errorf("PayloadID = %s, want payload-id", req.PayloadID)
	}
	if req.BlockIdx != 4 {
		t.Errorf("BlockIdx = %d, want 4", req.BlockIdx)
	}
	if req.Done != nil {
		t.Error("Done channel should be nil for prefetch")
	}
}

func TestTransferRequest_BlockKey(t *testing.T) {
	req := NewDownloadRequest("export/file.txt", 5, nil)
	key := req.BlockKey()

	expected := "export/file.txt/block-5"
	if key != expected {
		t.Errorf("BlockKey() = %s, want %s", key, expected)
	}
}

func TestTransferType_String(t *testing.T) {
	tests := []struct {
		t        TransferType
		expected string
	}{
		{TransferDownload, "download"},
		{TransferUpload, "upload"},
		{TransferPrefetch, "prefetch"},
		{TransferType(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.t.String(); got != tt.expected {
			t.Errorf("TransferType(%d).String() = %s, want %s", tt.t, got, tt.expected)
		}
	}
}
