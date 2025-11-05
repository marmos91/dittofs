package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// LinkRequest represents a LINK request
type LinkRequest struct {
	FileHandle []byte // File to create a link to
	DirHandle  []byte // Directory to create the link in
	Name       string // Name of the new link
}

// LinkResponse represents a LINK response
type LinkResponse struct {
	Status      uint32
	FileAttr    *FileAttr // Post-op attributes of the file (optional)
	LinkDirAttr *WccAttr  // Pre-op and post-op attributes of link directory (optional)
}

// Link creates a hard link to a file.
// RFC 1813 Section 3.3.15
func (h *DefaultNFSHandler) Link(repository metadata.Repository, req *LinkRequest) (*LinkResponse, error) {
	logger.Debug("LINK file %x to '%s' in directory %x", req.FileHandle, req.Name, req.DirHandle)

	// Get the file attributes
	fileAttr, err := repository.GetFile(metadata.FileHandle(req.FileHandle))
	if err != nil {
		logger.Warn("File not found: %v", err)
		return &LinkResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Don't allow hard links to directories
	if fileAttr.Type == metadata.FileTypeDirectory {
		logger.Warn("Attempted to create hard link to a directory")
		return &LinkResponse{Status: NFS3ErrIsDir}, nil
	}

	// Get directory attributes
	dirAttr, err := repository.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Directory not found: %v", err)
		return &LinkResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Target handle is not a directory")
		return &LinkResponse{Status: NFS3ErrNotDir}, nil
	}

	// Check if name already exists
	_, err = repository.GetChild(metadata.FileHandle(req.DirHandle), req.Name)
	if err == nil {
		logger.Debug("Name '%s' already exists", req.Name)
		return &LinkResponse{Status: NFS3ErrExist}, nil
	}

	// Add the link (same file handle, new name in directory)
	if err := repository.AddChild(metadata.FileHandle(req.DirHandle), req.Name, metadata.FileHandle(req.FileHandle)); err != nil {
		logger.Error("Failed to add child: %v", err)
		return &LinkResponse{Status: NFS3ErrIO}, nil
	}

	// Generate file ID
	fileid := binary.BigEndian.Uint64(req.FileHandle[:8])
	nfsAttr := MetadataToNFSAttr(fileAttr, fileid)

	logger.Info("LINK successful: '%s' -> handle %x", req.Name, req.FileHandle)

	return &LinkResponse{
		Status:   NFS3OK,
		FileAttr: nfsAttr,
	}, nil
}

func DecodeLinkRequest(data []byte) (*LinkRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short")
	}

	reader := bytes.NewReader(data)

	// Read file handle length
	var fileHandleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &fileHandleLen); err != nil {
		return nil, fmt.Errorf("read file handle length: %w", err)
	}

	// Read file handle
	fileHandle := make([]byte, fileHandleLen)
	if err := binary.Read(reader, binary.BigEndian, &fileHandle); err != nil {
		return nil, fmt.Errorf("read file handle: %w", err)
	}

	// Skip padding
	padding := (4 - (fileHandleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		reader.ReadByte()
	}

	// Read directory handle length
	var dirHandleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &dirHandleLen); err != nil {
		return nil, fmt.Errorf("read dir handle length: %w", err)
	}

	// Read directory handle
	dirHandle := make([]byte, dirHandleLen)
	if err := binary.Read(reader, binary.BigEndian, &dirHandle); err != nil {
		return nil, fmt.Errorf("read dir handle: %w", err)
	}

	// Skip padding
	padding = (4 - (dirHandleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		reader.ReadByte()
	}

	// Read name length
	var nameLen uint32
	if err := binary.Read(reader, binary.BigEndian, &nameLen); err != nil {
		return nil, fmt.Errorf("read name length: %w", err)
	}

	// Read name
	nameBytes := make([]byte, nameLen)
	if err := binary.Read(reader, binary.BigEndian, &nameBytes); err != nil {
		return nil, fmt.Errorf("read name: %w", err)
	}

	return &LinkRequest{
		FileHandle: fileHandle,
		DirHandle:  dirHandle,
		Name:       string(nameBytes),
	}, nil
}

func (resp *LinkResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Write post-op file attributes
	if resp.FileAttr != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := encodeFileAttr(&buf, resp.FileAttr); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	// Write WCC data for link directory (pre-op and post-op attributes - optional, we'll skip for now)
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
