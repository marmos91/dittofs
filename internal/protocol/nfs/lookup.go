package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// LookupRequest represents a LOOKUP request
type LookupRequest struct {
	DirHandle []byte
	Filename  string
}

// LookupResponse represents a LOOKUP response
type LookupResponse struct {
	Status     uint32
	FileHandle []byte    // only present if Status == NFS3OK
	Attr       *FileAttr // only present if Status == NFS3OK
	DirAttr    *FileAttr // post-op attributes for directory (optional)
}

// Lookup searches a directory for a specific name and returns its file handle.
// RFC 1813 Section 3.3.3
func (h *DefaultNFSHandler) Lookup(repository metadata.Repository, req *LookupRequest) (*LookupResponse, error) {
	logger.Debug("LOOKUP for file '%s' in directory %x", req.Filename, req.DirHandle)

	// Get the directory attributes
	dirAttr, err := repository.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Directory not found: %v", err)
		return &LookupResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Handle is not a directory")
		return &LookupResponse{Status: NFS3ErrNotDir}, nil
	}

	// Look up the child
	childHandle, err := repository.GetChild(metadata.FileHandle(req.DirHandle), req.Filename)
	if err != nil {
		logger.Debug("Child '%s' not found: %v", req.Filename, err)
		return &LookupResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Get child attributes
	childAttr, err := repository.GetFile(childHandle)
	if err != nil {
		logger.Error("Child handle exists but attributes not found: %v", err)
		return &LookupResponse{Status: NFS3ErrIO}, nil
	}

	// Generate file IDs
	dirFileid := binary.BigEndian.Uint64(req.DirHandle[:8])
	childFileid := binary.BigEndian.Uint64(childHandle[:8])

	logger.Info("LOOKUP successful: '%s' -> handle %x", req.Filename, childHandle)

	return &LookupResponse{
		Status:     NFS3OK,
		FileHandle: childHandle,
		Attr:       MetadataToNFSAttr(childAttr, childFileid),
		DirAttr:    MetadataToNFSAttr(dirAttr, dirFileid),
	}, nil
}

func DecodeLookupRequest(data []byte) (*LookupRequest, error) {
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
	if padding > 0 {
		if err := reader.UnreadByte(); err == nil {
			reader.ReadByte()
		}
		for i := uint32(0); i < padding; i++ {
			reader.ReadByte()
		}
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

	return &LookupRequest{
		DirHandle: dirHandle,
		Filename:  string(filenameBytes),
	}, nil
}

func (resp *LookupResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, return just the status
	if resp.Status != NFS3OK {
		return buf.Bytes(), nil
	}

	// Write file handle (opaque data)
	handleLen := uint32(len(resp.FileHandle))
	if err := binary.Write(&buf, binary.BigEndian, handleLen); err != nil {
		return nil, fmt.Errorf("write handle length: %w", err)
	}
	buf.Write(resp.FileHandle)

	// Add padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		buf.WriteByte(0)
	}

	// Write object attributes (present = true, then attributes)
	if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
		return nil, err
	}
	if err := encodeFileAttr(&buf, resp.Attr); err != nil {
		return nil, err
	}

	// Write post-op dir attributes
	if resp.DirAttr != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := encodeFileAttr(&buf, resp.DirAttr); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}
