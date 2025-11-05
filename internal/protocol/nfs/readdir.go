package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ReadDirRequest represents a READDIR request
type ReadDirRequest struct {
	DirHandle  []byte
	Cookie     uint64
	CookieVerf uint64
	Count      uint32
}

// ReadDirResponse represents a READDIR response
type ReadDirResponse struct {
	Status     uint32
	DirAttr    *FileAttr // post-op attributes (optional)
	CookieVerf uint64
	Entries    []*DirEntry
	Eof        bool
}

// DirEntry represents a single directory entry
type DirEntry struct {
	Fileid uint64
	Name   string
	Cookie uint64
}

// ReadDir reads entries from a directory.
// RFC 1813 Section 3.3.16
func (h *DefaultNFSHandler) ReadDir(repository metadata.Repository, req *ReadDirRequest) (*ReadDirResponse, error) {
	logger.Debug("READDIR for directory %x, cookie=%d, count=%d", req.DirHandle, req.Cookie, req.Count)

	// Get directory attributes
	dirAttr, err := repository.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Directory not found: %v", err)
		return &ReadDirResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Handle is not a directory")
		return &ReadDirResponse{Status: NFS3ErrNotDir}, nil
	}

	// Get all children
	children, err := repository.GetChildren(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Error("Failed to get children: %v", err)
		return &ReadDirResponse{Status: NFS3ErrIO}, nil
	}

	// Generate file ID for directory
	dirFileid := binary.BigEndian.Uint64(req.DirHandle[:8])
	nfsDirAttr := MetadataToNFSAttr(dirAttr, dirFileid)

	// Build entries list
	entries := make([]*DirEntry, 0)
	cookie := uint64(1)

	// Add "." entry
	if req.Cookie == 0 {
		entries = append(entries, &DirEntry{
			Fileid: dirFileid,
			Name:   ".",
			Cookie: cookie,
		})
		cookie++
	}

	// Add ".." entry
	if req.Cookie <= 1 {
		// For root or when parent doesn't exist, use same fileid
		parentFileid := dirFileid
		parentHandle, err := repository.GetParent(metadata.FileHandle(req.DirHandle))
		if err == nil {
			parentFileid = binary.BigEndian.Uint64(parentHandle[:8])
		}

		entries = append(entries, &DirEntry{
			Fileid: parentFileid,
			Name:   "..",
			Cookie: cookie,
		})
		cookie++
	}

	// Add regular entries
	for name, handle := range children {
		if cookie <= req.Cookie {
			cookie++
			continue
		}

		fileid := binary.BigEndian.Uint64(handle[:8])
		entries = append(entries, &DirEntry{
			Fileid: fileid,
			Name:   name,
			Cookie: cookie,
		})
		cookie++
	}

	logger.Info("READDIR returning %d entries (eof=true)", len(entries))

	return &ReadDirResponse{
		Status:     NFS3OK,
		DirAttr:    nfsDirAttr,
		CookieVerf: 0, // We don't use cookie verification
		Entries:    entries,
		Eof:        true, // For simplicity, return all entries at once
	}, nil
}

func DecodeReadDirRequest(data []byte) (*ReadDirRequest, error) {
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

	// Read cookie
	var cookie uint64
	if err := binary.Read(reader, binary.BigEndian, &cookie); err != nil {
		return nil, fmt.Errorf("read cookie: %w", err)
	}

	// Read cookieverf
	var cookieVerf uint64
	if err := binary.Read(reader, binary.BigEndian, &cookieVerf); err != nil {
		return nil, fmt.Errorf("read cookieverf: %w", err)
	}

	// Read count
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read count: %w", err)
	}

	return &ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     cookie,
		CookieVerf: cookieVerf,
		Count:      count,
	}, nil
}

func (resp *ReadDirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// If status is not OK, return just the status
	if resp.Status != NFS3OK {
		return buf.Bytes(), nil
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

	// Write cookieverf
	if err := binary.Write(&buf, binary.BigEndian, resp.CookieVerf); err != nil {
		return nil, err
	}

	// Write entries
	for _, entry := range resp.Entries {
		// value_follows = TRUE
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}

		// Write fileid
		if err := binary.Write(&buf, binary.BigEndian, entry.Fileid); err != nil {
			return nil, err
		}

		// Write name (string)
		nameLen := uint32(len(entry.Name))
		if err := binary.Write(&buf, binary.BigEndian, nameLen); err != nil {
			return nil, err
		}
		buf.Write([]byte(entry.Name))

		// Add padding to 4-byte boundary
		padding := (4 - (nameLen % 4)) % 4
		for i := uint32(0); i < padding; i++ {
			buf.WriteByte(0)
		}

		// Write cookie
		if err := binary.Write(&buf, binary.BigEndian, entry.Cookie); err != nil {
			return nil, err
		}
	}

	// value_follows = FALSE (no more entries)
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, err
	}

	// Write EOF
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, eofVal); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
