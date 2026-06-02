package engine

import (
	"testing"
)

func TestTransferRequest_BlockKey(t *testing.T) {
	req := TransferRequest{
		Type:       TransferDownload,
		PayloadID:  "export/file.txt",
		BlockIndex: 5,
	}
	key := req.BlockKey()

	// BlockKey switched from the legacy "{payloadID}/block-{N}"
	// shape (deleted with block.FormatStoreKey) to "{payloadID}/{N}"
	// for the engine in-flight dedup map.
	expected := "export/file.txt/5"
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
