// Package transfer implements background upload for cache-to-block-store persistence.
//
// The transfer package is responsible for:
//   - Eager upload: Upload 4MB blocks as soon as they're ready (don't wait for COMMIT)
//   - Flush: Wait for in-flight uploads and flush remaining partial blocks on COMMIT/CLOSE
//   - Download: Fetch blocks from block store on cache miss, cache them for future reads
//
// Key Design Principles:
//   - Maximize bandwidth: Upload blocks as soon as 4MB is available
//   - Parallel I/O: Upload/download multiple blocks concurrently
//   - Protocol agnostic: Works with both NFS COMMIT and SMB CLOSE
//   - Share-aware keys: Block keys include share name for multi-tenant support
package transfer

// TransferRequest holds data for a pending transfer operation.
// This is a simple data struct - the TransferManager handles execution.
type TransferRequest struct {
	ShareName  string
	FileHandle string
	PayloadID  string
	Priority   int
}

// NewTransferRequest creates a new transfer request.
func NewTransferRequest(shareName, fileHandle, payloadID string) TransferRequest {
	return TransferRequest{
		ShareName:  shareName,
		FileHandle: fileHandle,
		PayloadID:  payloadID,
		Priority:   0,
	}
}

// WithPriority returns a copy of the request with the specified priority.
func (r TransferRequest) WithPriority(priority int) TransferRequest {
	r.Priority = priority
	return r
}
