package sync

import "github.com/marmos91/dittofs/pkg/blockstore"

// TransferRequest holds data for a pending transfer operation (download, upload, or prefetch).
type TransferRequest struct {
	Type      TransferType // Transfer type and priority
	PayloadID string       // Content ID
	BlockIdx  uint64       // Flat block index (fileOffset / BlockSize)
	Done      chan error   // Completion channel; nil for async (fire-and-forget)
}

// NewDownloadRequest creates a download request for a specific block.
func NewDownloadRequest(payloadID string, blockIdx uint64, done chan error) TransferRequest {
	return TransferRequest{
		Type:      TransferDownload,
		PayloadID: payloadID,
		BlockIdx:  blockIdx,
		Done:      done,
	}
}

// NewPrefetchRequest creates a prefetch request for a specific block (best-effort, async).
func NewPrefetchRequest(payloadID string, blockIdx uint64) TransferRequest {
	return TransferRequest{
		Type:      TransferPrefetch,
		PayloadID: payloadID,
		BlockIdx:  blockIdx,
	}
}

// NewBlockUploadRequest creates an async upload request for a specific block.
func NewBlockUploadRequest(payloadID string, blockIdx uint64) TransferRequest {
	return TransferRequest{
		Type:      TransferUpload,
		PayloadID: payloadID,
		BlockIdx:  blockIdx,
	}
}

// BlockKey returns a unique string key for this block.
func (r TransferRequest) BlockKey() string {
	return blockstore.FormatStoreKey(r.PayloadID, r.BlockIdx)
}
