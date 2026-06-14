package engine

import (
	"testing"
)

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
