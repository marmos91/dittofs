package mount

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	xdr "github.com/rasky/go-xdr/xdr2"
)

// UmountRequest represents an UMNT request
type UmountRequest struct {
	DirPath string
}

// UmountResponse represents an UMNT response (void - no response data)
type UmountResponse struct {
	// UMNT has no response data, just success
}

// Umount removes a mount entry from the mount list.
// RFC 1813 Appendix I
func (h *DefaultMountHandler) Umnt(repository metadata.Repository, req *UmountRequest) (*UmountResponse, error) {
	logger.Info("UMOUNT called for path: %s", req.DirPath)

	// In a real implementation, you might want to track active mounts
	// For now, we'll just acknowledge the unmount request
	// The client is responsible for actually unmounting on their side

	return &UmountResponse{}, nil
}

func DecodeUmountRequest(data []byte) (*UmountRequest, error) {
	req := &UmountRequest{}
	_, err := xdr.Unmarshal(bytes.NewReader(data), req)
	if err != nil {
		return nil, fmt.Errorf("unmarshal umount request: %w", err)
	}
	return req, nil
}

func (resp *UmountResponse) Encode() ([]byte, error) {
	// UMNT returns void (no data)
	return []byte{}, nil
}
