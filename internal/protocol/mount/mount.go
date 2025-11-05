package mount

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	xdr "github.com/rasky/go-xdr/xdr2"
)

type DefaultMountHandler struct{}

// MountRequest represents a MOUNT request
type MountRequest struct {
	DirPath string
}

// MountResponse represents a MOUNT response
type MountResponse struct {
	Status      uint32
	FileHandle  []byte
	AuthFlavors []int32
}

// DecodeMountRequest is the "static factory" function
func DecodeMountRequest(data []byte) (*MountRequest, error) {
	req := &MountRequest{}
	_, err := xdr.Unmarshal(bytes.NewReader(data), req)

	if err != nil {
		return nil, err
	}
	return req, nil
}

func (resp *MountResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	logger.Debug("Encoding MountResponse: Status=%d, HandleLen=%d, AuthFlavors=%v",
		resp.Status, len(resp.FileHandle), resp.AuthFlavors)

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, we're done
	if resp.Status != MountOK {
		logger.Debug("Status not OK, returning error response")
		return buf.Bytes(), nil
	}

	// Write file handle (opaque data)
	handleLen := uint32(len(resp.FileHandle))
	logger.Debug("Writing handle length: %d", handleLen)
	if err := binary.Write(&buf, binary.BigEndian, handleLen); err != nil {
		return nil, fmt.Errorf("write handle length: %w", err)
	}

	logger.Debug("Writing handle bytes: %x", resp.FileHandle)
	buf.Write(resp.FileHandle)

	// Add padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	logger.Debug("Adding %d padding bytes", padding)
	for i := 0; i < int(padding); i++ {
		buf.WriteByte(0)
	}

	// Write auth flavors array
	authCount := uint32(len(resp.AuthFlavors))
	logger.Debug("Writing auth count: %d", authCount)
	if err := binary.Write(&buf, binary.BigEndian, authCount); err != nil {
		return nil, fmt.Errorf("write auth count: %w", err)
	}

	for _, flavor := range resp.AuthFlavors {
		logger.Debug("Writing auth flavor: %d", flavor)
		if err := binary.Write(&buf, binary.BigEndian, flavor); err != nil {
			return nil, fmt.Errorf("write auth flavor: %w", err)
		}
	}

	result := buf.Bytes()
	logger.Debug("Final encoded response length: %d bytes", len(result))
	return result, nil
}

// Mount returns a file handle for the requested export path.
// This is the primary procedure used to mount an NFS file system.
// RFC 1813 Appendix I
func (h *DefaultMountHandler) Mount(repository metadata.Repository, req *MountRequest) (*MountResponse, error) {
	logger.Debug("Mount called for path: %s", req.DirPath)

	// Check if the export exists
	export, err := repository.FindExport(req.DirPath)
	if err != nil {
		logger.Debug("Export not found: %s", req.DirPath)
		return &MountResponse{
			Status: MountErrNoEnt,
		}, nil
	}

	logger.Debug("Found export: %s (readonly=%v)", export.Path, export.Options.ReadOnly)

	// Get the root handle for this export
	handleBytes, err := repository.GetRootHandle(export.Path)
	if err != nil {
		logger.Debug("Failed to get root handle: %v", err)
		return &MountResponse{
			Status: MountErrServerFault,
		}, nil
	}

	logger.Debug("Retrieved file handle (length=%d): %x", len(handleBytes), handleBytes)

	// Return success with the file handle
	return &MountResponse{
		Status:      MountOK,
		FileHandle:  handleBytes,
		AuthFlavors: []int32{0}, // AUTH_NULL
	}, nil
}
