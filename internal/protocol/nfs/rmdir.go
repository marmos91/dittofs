package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// RmdirRequest represents a RMDIR request
type RmdirRequest struct {
	DirHandle []byte
	Name      string
}

// RmdirResponse represents a RMDIR response
type RmdirResponse struct {
	Status  uint32
	DirAttr *WccAttr // Pre-op and post-op attributes (optional)
}

// Rmdir removes a directory.
// RFC 1813 Section 3.3.13
func (h *DefaultNFSHandler) Rmdir(repository metadata.Repository, req *RmdirRequest) (*RmdirResponse, error) {
	logger.Debug("RMDIR '%s' from directory %x", req.Name, req.DirHandle)

	// Get parent directory attributes
	parentAttr, err := repository.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Parent directory not found: %v", err)
		return &RmdirResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Parent handle is not a directory")
		return &RmdirResponse{Status: NFS3ErrNotDir}, nil
	}

	// Get the directory handle
	dirHandle, err := repository.GetChild(metadata.FileHandle(req.DirHandle), req.Name)
	if err != nil {
		logger.Debug("Directory '%s' not found: %v", req.Name, err)
		return &RmdirResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Get directory attributes to verify it's a directory
	dirAttr, err := repository.GetFile(dirHandle)
	if err != nil {
		logger.Error("Directory handle exists but attributes not found: %v", err)
		return &RmdirResponse{Status: NFS3ErrIO}, nil
	}

	// Verify it's actually a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Attempted to remove a non-directory with RMDIR")
		return &RmdirResponse{Status: NFS3ErrNotDir}, nil
	}

	// Check if directory is empty
	children, err := repository.GetChildren(dirHandle)
	if err != nil {
		logger.Error("Failed to get children: %v", err)
		return &RmdirResponse{Status: NFS3ErrIO}, nil
	}

	if len(children) > 0 {
		logger.Debug("Directory '%s' is not empty (has %d children)", req.Name, len(children))
		return &RmdirResponse{Status: NFS3ErrNotEmpty}, nil
	}

	// Remove from parent directory
	if err := repository.DeleteChild(metadata.FileHandle(req.DirHandle), req.Name); err != nil {
		logger.Error("Failed to remove child: %v", err)
		return &RmdirResponse{Status: NFS3ErrIO}, nil
	}

	// Delete the directory metadata
	if err := repository.DeleteFile(dirHandle); err != nil {
		logger.Error("Failed to delete directory: %v", err)
		// Directory is already removed from parent, but metadata cleanup failed
		// Continue anyway as the directory is effectively removed
	}

	logger.Info("RMDIR successful: '%s'", req.Name)

	return &RmdirResponse{
		Status: NFS3OK,
	}, nil
}

func DecodeRmdirRequest(data []byte) (*RmdirRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short")
	}

	reader := bytes.NewReader(data)

	// Read directory handle length
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	// Read directory handle
	dirHandle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &dirHandle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	// Skip padding
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		reader.ReadByte()
	}

	// Read directory name length
	var nameLen uint32
	if err := binary.Read(reader, binary.BigEndian, &nameLen); err != nil {
		return nil, fmt.Errorf("read name length: %w", err)
	}

	// Read directory name
	nameBytes := make([]byte, nameLen)
	if err := binary.Read(reader, binary.BigEndian, &nameBytes); err != nil {
		return nil, fmt.Errorf("read name: %w", err)
	}

	return &RmdirRequest{
		DirHandle: dirHandle,
		Name:      string(nameBytes),
	}, nil
}

func (resp *RmdirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Write WCC data (pre-op and post-op attributes - optional, we'll skip for now)
	// Pre-op attributes
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, err
	}
	// Post-op attributes
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
