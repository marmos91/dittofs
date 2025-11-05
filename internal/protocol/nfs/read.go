package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ReadRequest represents a READ request
type ReadRequest struct {
	Handle []byte
	Offset uint64
	Count  uint32
}

// ReadResponse represents a READ response
type ReadResponse struct {
	Status uint32
	Attr   *FileAttr // Post-op attributes (optional)
	Count  uint32    // Number of bytes read
	Eof    bool      // True if end of file reached
	Data   []byte    // The actual data read
}

// Read reads data from a file.
// RFC 1813 Section 3.3.6
func (h *DefaultNFSHandler) Read(contentRepo content.Repository, metadataRepo metadata.Repository, req *ReadRequest) (*ReadResponse, error) {
	logger.Debug("READ for handle: %x, offset=%d, count=%d", req.Handle, req.Offset, req.Count)

	// Get file attributes
	attr, err := metadataRepo.GetFile(metadata.FileHandle(req.Handle))
	if err != nil {
		logger.Warn("File not found: %v", err)
		return &ReadResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a regular file
	if attr.Type != metadata.FileTypeRegular {
		logger.Warn("Handle is not a regular file")
		return &ReadResponse{Status: NFS3ErrIsDir}, nil
	}

	// Generate file ID
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	// Check if we have content
	if attr.ContentID == "" {
		logger.Warn("File has no content ID")
		return &ReadResponse{
			Status: NFS3OK,
			Attr:   nfsAttr,
			Count:  0,
			Eof:    true,
			Data:   []byte{},
		}, nil
	}

	// Open the content
	reader, err := contentRepo.ReadContent(attr.ContentID)
	if err != nil {
		logger.Error("Failed to read content: %v", err)
		return &ReadResponse{Status: NFS3ErrIO}, nil
	}
	defer reader.Close()

	// Seek to the requested offset
	if seeker, ok := reader.(io.Seeker); ok {
		_, err = seeker.Seek(int64(req.Offset), io.SeekStart)
		if err != nil {
			logger.Error("Failed to seek: %v", err)
			return &ReadResponse{Status: NFS3ErrIO}, nil
		}
	} else {
		// If not seekable, read and discard bytes until offset
		if req.Offset > 0 {
			_, err = io.CopyN(io.Discard, reader, int64(req.Offset))
			if err != nil && err != io.EOF {
				logger.Error("Failed to skip to offset: %v", err)
				return &ReadResponse{Status: NFS3ErrIO}, nil
			}
		}
	}

	// Read the requested data
	data := make([]byte, req.Count)
	n, err := io.ReadFull(reader, data)

	eof := false
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// We've reached EOF
		eof = true
		data = data[:n]
	} else if err != nil {
		logger.Error("Failed to read data: %v", err)
		return &ReadResponse{Status: NFS3ErrIO}, nil
	}

	// Check if we're at EOF even if read was successful
	if req.Offset+uint64(n) >= attr.Size {
		eof = true
	}

	logger.Info("READ successful: read %d bytes, eof=%v", n, eof)

	return &ReadResponse{
		Status: NFS3OK,
		Attr:   nfsAttr,
		Count:  uint32(n),
		Eof:    eof,
		Data:   data,
	}, nil
}

func DecodeReadRequest(data []byte) (*ReadRequest, error) {
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
	for i := uint32(0); i < padding; i++ {
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

	return &ReadRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

func (resp *ReadResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, return just the status
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

	// Write count
	if err := binary.Write(&buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("write count: %w", err)
	}

	// Write EOF flag
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("write eof: %w", err)
	}

	// Write data as opaque (length + data + padding)
	dataLen := uint32(len(resp.Data))
	if err := binary.Write(&buf, binary.BigEndian, dataLen); err != nil {
		return nil, fmt.Errorf("write data length: %w", err)
	}

	buf.Write(resp.Data)

	// Add padding to 4-byte boundary
	padding := (4 - (dataLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		buf.WriteByte(0)
	}

	return buf.Bytes(), nil
}
