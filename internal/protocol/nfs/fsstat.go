package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// FsStatRequest represents a FSSTAT request
type FsStatRequest struct {
	Handle []byte
}

// FsStatResponse represents a FSSTAT response
type FsStatResponse struct {
	Status   uint32
	Attr     *FileAttr // Post-op attributes (optional)
	Tbytes   uint64    // Total size in bytes
	Fbytes   uint64    // Free space in bytes
	Abytes   uint64    // Available space in bytes
	Tfiles   uint64    // Total file slots
	Ffiles   uint64    // Free file slots
	Afiles   uint64    // Available file slots
	Invarsec uint32    // Invariant time in seconds
}

// FsStat returns dynamic information about a file system.
// RFC 1813 Section 3.3.18
func (h *DefaultNFSHandler) FsStat(repository metadata.Repository, req *FsStatRequest) (*FsStatResponse, error) {
	logger.Debug("FSSTAT for handle: %x", req.Handle)

	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Debug("File not found: %v", err)
		return &FsStatResponse{Status: NFS3ErrNoEnt}, nil
	}

	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	// Return some reasonable values for a virtual filesystem
	totalBytes := uint64(1024 * 1024 * 1024 * 1024) // 1TB
	freeBytes := uint64(512 * 1024 * 1024 * 1024)   // 512GB

	resp := &FsStatResponse{
		Status:   NFS3OK,
		Attr:     nfsAttr,
		Tbytes:   totalBytes,
		Fbytes:   freeBytes,
		Abytes:   freeBytes,
		Tfiles:   1000000, // Total inodes
		Ffiles:   900000,  // Free inodes
		Afiles:   900000,  // Available inodes
		Invarsec: 0,       // Filesystem not expected to change
	}

	logger.Debug("Returning FSSTAT")
	return resp, nil
}

func DecodeFsStatRequest(data []byte) (*FsStatRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short")
	}

	reader := bytes.NewReader(data)

	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	return &FsStatRequest{Handle: handle}, nil
}

func (resp *FsStatResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	if resp.Status != NFS3OK {
		return buf.Bytes(), nil
	}

	// Write post-op attributes
	if resp.Attr != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := encodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	if err := binary.Write(&buf, binary.BigEndian, resp.Tbytes); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Fbytes); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Abytes); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Tfiles); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Ffiles); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Afiles); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, resp.Invarsec); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
