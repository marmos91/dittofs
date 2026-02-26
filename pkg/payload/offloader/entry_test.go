package offloader

import (
	"testing"
)

func TestNewBlockUploadRequest(t *testing.T) {
	req := NewBlockUploadRequest("export/test.txt", 1, 2)

	if req.PayloadID != "export/test.txt" {
		t.Errorf("PayloadID = %s, want export/test.txt", req.PayloadID)
	}

	if req.Type != TransferUpload {
		t.Errorf("Type = %v, want TransferUpload", req.Type)
	}

	if req.ChunkIdx != 1 {
		t.Errorf("ChunkIdx = %d, want 1", req.ChunkIdx)
	}

	if req.BlockIdx != 2 {
		t.Errorf("BlockIdx = %d, want 2", req.BlockIdx)
	}

	if req.Done != nil {
		t.Error("Done channel should be nil for async uploads")
	}
}

func TestTransferRequest_WithPriority(t *testing.T) {
	req := NewBlockUploadRequest("content-id", 0, 0)
	downloadReq := req.WithPriority(TransferDownload)

	// Original should be unchanged (value receiver returns copy)
	if req.Type != TransferUpload {
		t.Errorf("original Type = %v, want TransferUpload", req.Type)
	}

	// New request should have priority
	if downloadReq.Type != TransferDownload {
		t.Errorf("downloadReq.Type = %v, want TransferDownload", downloadReq.Type)
	}

	// Other fields should be copied
	if downloadReq.PayloadID != req.PayloadID {
		t.Errorf("PayloadID mismatch after WithPriority")
	}
	if downloadReq.ChunkIdx != req.ChunkIdx {
		t.Errorf("ChunkIdx mismatch after WithPriority")
	}
	if downloadReq.BlockIdx != req.BlockIdx {
		t.Errorf("BlockIdx mismatch after WithPriority")
	}
}

func TestNewDownloadRequest(t *testing.T) {
	done := make(chan error, 1)
	req := NewDownloadRequest("payload-id", 1, 2, done)

	if req.Type != TransferDownload {
		t.Errorf("Type = %v, want TransferDownload", req.Type)
	}
	if req.PayloadID != "payload-id" {
		t.Errorf("PayloadID = %s, want payload-id", req.PayloadID)
	}
	if req.ChunkIdx != 1 {
		t.Errorf("ChunkIdx = %d, want 1", req.ChunkIdx)
	}
	if req.BlockIdx != 2 {
		t.Errorf("BlockIdx = %d, want 2", req.BlockIdx)
	}
	if req.Done != done {
		t.Error("Done channel not set correctly")
	}
}

func TestNewPrefetchRequest(t *testing.T) {
	req := NewPrefetchRequest("payload-id", 3, 4)

	if req.Type != TransferPrefetch {
		t.Errorf("Type = %v, want TransferPrefetch", req.Type)
	}
	if req.PayloadID != "payload-id" {
		t.Errorf("PayloadID = %s, want payload-id", req.PayloadID)
	}
	if req.ChunkIdx != 3 {
		t.Errorf("ChunkIdx = %d, want 3", req.ChunkIdx)
	}
	if req.BlockIdx != 4 {
		t.Errorf("BlockIdx = %d, want 4", req.BlockIdx)
	}
	if req.Done != nil {
		t.Error("Done channel should be nil for prefetch")
	}
}

func TestTransferRequest_BlockKey(t *testing.T) {
	req := NewDownloadRequest("export/file.txt", 2, 5, nil)
	key := req.BlockKey()

	expected := "export/file.txt/chunk-2/block-5"
	if key != expected {
		t.Errorf("BlockKey() = %s, want %s", key, expected)
	}
}

func TestFormatBlockKey(t *testing.T) {
	key := FormatBlockKey("export/myfile.bin", 0, 3)
	expected := "export/myfile.bin/chunk-0/block-3"
	if key != expected {
		t.Errorf("FormatBlockKey() = %s, want %s", key, expected)
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
