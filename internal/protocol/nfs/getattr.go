package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// GetAttrRequest represents a GETATTR request
type GetAttrRequest struct {
	Handle []byte
}

// GetAttrResponse represents a GETATTR response
type GetAttrResponse struct {
	Status uint32
	Attr   *FileAttr // only present if Status == NFS3OK
}

// GetAttr returns the attributes for a file system object.
// RFC 1813 Section 3.3.1
func (h *DefaultNFSHandler) GetAttr(repository metadata.Repository, req *GetAttrRequest) (*GetAttrResponse, error) {

	logger.Debug("GETATTR for handle: %x", req.Handle)

	// Look up the file in the repository
	attr, err := repository.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Debug("File not found: %v", err)
		resp := &GetAttrResponse{Status: NFS3ErrNoEnt}
		return resp, nil
	}

	// Generate a file ID from the handle
	fileid := binary.BigEndian.Uint64(req.Handle[:8])

	// Convert to NFS attributes
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	logger.Debug("Returning attributes: Type=%d, Mode=%o, Size=%d", nfsAttr.Type, nfsAttr.Mode, nfsAttr.Size)

	resp := &GetAttrResponse{
		Status: NFS3OK,
		Attr:   nfsAttr,
	}

	return resp, nil
}

func DecodeGetAttrRequest(data []byte) (*GetAttrRequest, error) {
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

	return &GetAttrRequest{Handle: handle}, nil
}

func (resp *GetAttrResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, we're done
	if resp.Status != NFS3OK {
		return buf.Bytes(), nil
	}

	// Write attributes
	if err := encodeFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("encode attr: %w", err)
	}

	return buf.Bytes(), nil
}

func encodeFileAttr(buf *bytes.Buffer, attr *FileAttr) error {
	if err := binary.Write(buf, binary.BigEndian, attr.Type); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mode); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Nlink); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.UID); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.GID); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Size); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Used); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Rdev); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Fsid); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Fileid); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Atime.Seconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Atime.Nseconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Seconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Mtime.Nseconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Seconds); err != nil {
		return err
	}
	if err := binary.Write(buf, binary.BigEndian, attr.Ctime.Nseconds); err != nil {
		return err
	}
	return nil
}
