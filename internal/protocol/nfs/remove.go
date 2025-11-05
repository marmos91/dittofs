package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// RemoveRequest represents a REMOVE request
type RemoveRequest struct {
	DirHandle []byte
	Filename  string
}

// RemoveResponse represents a REMOVE response
type RemoveResponse struct {
	Status  uint32
	DirAttr *WccAttr // Pre-op and post-op attributes (optional)
}

// Remove deletes a file from a directory.
// RFC 1813 Section 3.3.12
func (h *DefaultNFSHandler) Remove(repository metadata.Repository, req *RemoveRequest) (*RemoveResponse, error) {
	logger.Debug("REMOVE file '%s' from directory %x", req.Filename, req.DirHandle)

	// Get directory attributes
	dirAttr, err := repository.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Directory not found: %v", err)
		return &RemoveResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Handle is not a directory")
		return &RemoveResponse{Status: NFS3ErrNotDir}, nil
	}

	// Get the file handle
	fileHandle, err := repository.GetChild(metadata.FileHandle(req.DirHandle), req.Filename)
	if err != nil {
		logger.Debug("File '%s' not found: %v", req.Filename, err)
		return &RemoveResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Get file attributes to check if it's a directory
	fileAttr, err := repository.GetFile(fileHandle)
	if err != nil {
		logger.Error("File handle exists but attributes not found: %v", err)
		return &RemoveResponse{Status: NFS3ErrIO}, nil
	}

	// Don't allow removing directories with REMOVE (use RMDIR instead)
	if fileAttr.Type == metadata.FileTypeDirectory {
		logger.Warn("Attempted to remove a directory with REMOVE")
		return &RemoveResponse{Status: NFS3ErrIsDir}, nil
	}

	// Remove from directory
	if err := repository.DeleteChild(metadata.FileHandle(req.DirHandle), req.Filename); err != nil {
		logger.Error("Failed to remove child: %v", err)
		return &RemoveResponse{Status: NFS3ErrIO}, nil
	}

	// Delete the file metadata
	if err := repository.DeleteFile(fileHandle); err != nil {
		logger.Error("Failed to delete file: %v", err)
		// File is already removed from directory, but metadata cleanup failed
		// Continue anyway as the file is effectively removed
	}

	logger.Info("REMOVE successful: '%s'", req.Filename)

	return &RemoveResponse{
		Status: NFS3OK,
	}, nil
}

func DecodeRemoveRequest(data []byte) (*RemoveRequest, error) {
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

	// Read filename length
	var filenameLen uint32
	if err := binary.Read(reader, binary.BigEndian, &filenameLen); err != nil {
		return nil, fmt.Errorf("read filename length: %w", err)
	}

	// Read filename
	filenameBytes := make([]byte, filenameLen)
	if err := binary.Read(reader, binary.BigEndian, &filenameBytes); err != nil {
		return nil, fmt.Errorf("read filename: %w", err)
	}

	return &RemoveRequest{
		DirHandle: dirHandle,
		Filename:  string(filenameBytes),
	}, nil
}

func (resp *RemoveResponse) Encode() ([]byte, error) {
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
