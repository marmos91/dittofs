package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// Write stability levels (RFC 1813 Section 3.3.7)
const (
	UnstableWrite = 0
	DataSyncWrite = 1
	FileSyncWrite = 2
)

// WriteRequest represents a WRITE request
type WriteRequest struct {
	Handle []byte
	Offset uint64
	Count  uint32
	Stable uint32 // Stability level
	Data   []byte
}

// WriteResponse represents a WRITE response
type WriteResponse struct {
	Status     uint32
	AttrBefore *types.WccAttr     // Pre-op attributes (optional)
	AttrAfter  *types.NFSFileAttr // Post-op attributes (optional)
	Count      uint32             // Number of bytes written
	Committed  uint32             // How data was committed
	Verf       uint64             // Write verifier
}

// Write writes data to a file.
// RFC 1813 Section 3.3.7
func (h *DefaultNFSHandler) Write(contentRepo content.Repository, metadataRepo metadata.Repository, req *WriteRequest) (*WriteResponse, error) {
	logger.Debug("WRITE for handle: %x, offset=%d, count=%d, stable=%d",
		req.Handle, req.Offset, req.Count, req.Stable)

	// Get file attributes
	attr, err := metadataRepo.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Warn("File not found: %v", err)
		return &WriteResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Verify it's a regular file
	if attr.Type != metadata.FileTypeRegular {
		logger.Warn("Handle is not a regular file")
		return &WriteResponse{Status: types.NFS3ErrIsDir}, nil
	}

	// Store pre-op attributes for WCC
	wccAttr := &types.WccAttr{
		Size: attr.Size,
		Mtime: types.TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}

	// Check if content repository supports writing
	writeRepo, ok := contentRepo.(content.WriteRepository)
	if !ok {
		logger.Error("Content repository does not support writing")
		return &WriteResponse{Status: types.NFS3ErrRofs}, nil
	}

	// If no ContentID exists, create one
	if attr.ContentID == "" {
		attr.ContentID = content.ContentID(fmt.Sprintf("file-%x", req.Handle[:8]))
	}

	// Write the data
	err = writeRepo.WriteAt(attr.ContentID, req.Data, int64(req.Offset))
	if err != nil {
		logger.Error("Failed to write content: %v", err)
		return &WriteResponse{Status: types.NFS3ErrIO}, nil
	}

	// Update file size if needed
	newSize := req.Offset + uint64(len(req.Data))
	if newSize > attr.Size {
		attr.Size = newSize
	}

	// Update timestamps
	attr.Mtime = time.Now()
	attr.Ctime = time.Now()

	// Save updated attributes
	if err := metadataRepo.UpdateFile(metadata.FileHandle(req.Handle), attr); err != nil {
		logger.Error("Failed to update file metadata: %v", err)
		return &WriteResponse{Status: types.NFS3ErrIO}, nil
	}

	// Generate file ID
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("WRITE successful: wrote %d bytes at offset %d", len(req.Data), req.Offset)

	return &WriteResponse{
		Status:     types.NFS3OK,
		AttrBefore: wccAttr,
		AttrAfter:  nfsAttr,
		Count:      uint32(len(req.Data)),
		Committed:  FileSyncWrite, // We commit immediately
		Verf:       0,
	}, nil
}

func DecodeWriteRequest(data []byte) (*WriteRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short")
	}

	reader := bytes.NewReader(data)

	// Read handle length
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	// Read handle
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("read handle: %w", err)
	}

	// Skip padding
	padding := (4 - (handleLen % 4)) % 4
	for range padding {
		reader.ReadByte()
	}

	// Read offset
	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("read offset: %w", err)
	}

	// Read count
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read count: %w", err)
	}

	// Read stable
	var stable uint32
	if err := binary.Read(reader, binary.BigEndian, &stable); err != nil {
		return nil, fmt.Errorf("read stable: %w", err)
	}

	// Read data length
	var dataLen uint32
	if err := binary.Read(reader, binary.BigEndian, &dataLen); err != nil {
		return nil, fmt.Errorf("read data length: %w", err)
	}

	// Read data
	writeData := make([]byte, dataLen)
	if err := binary.Read(reader, binary.BigEndian, &writeData); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}

	return &WriteRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
		Stable: stable,
		Data:   writeData,
	}, nil
}

func (resp *WriteResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, still need to write WCC data
	// Write WCC data (pre-op and post-op attributes)
	// Pre-op attributes
	if resp.AttrBefore != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Size); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Seconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Nseconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Seconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Nseconds); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	// Post-op attributes
	if resp.AttrAfter != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := xdr.EncodeFileAttr(&buf, resp.AttrAfter); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	if resp.Status != types.NFS3OK {
		return buf.Bytes(), nil
	}

	// Write count
	if err := binary.Write(&buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("write count: %w", err)
	}

	// Write committed
	if err := binary.Write(&buf, binary.BigEndian, resp.Committed); err != nil {
		return nil, fmt.Errorf("write committed: %w", err)
	}

	// Write verifier
	if err := binary.Write(&buf, binary.BigEndian, resp.Verf); err != nil {
		return nil, fmt.Errorf("write verifier: %w", err)
	}

	return buf.Bytes(), nil
}
