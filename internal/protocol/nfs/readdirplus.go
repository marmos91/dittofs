package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ReadDirPlusRequest represents a READDIRPLUS request
type ReadDirPlusRequest struct {
	DirHandle  []byte
	Cookie     uint64
	CookieVerf uint64
	DirCount   uint32 // Max bytes of directory info
	MaxCount   uint32 // Max bytes of directory + entry info
}

// ReadDirPlusResponse represents a READDIRPLUS response
type ReadDirPlusResponse struct {
	Status     uint32
	DirAttr    *FileAttr // post-op attributes (optional)
	CookieVerf uint64
	Entries    []*DirPlusEntry
	Eof        bool
}

// DirPlusEntry represents a single directory entry with attributes
type DirPlusEntry struct {
	Fileid     uint64
	Name       string
	Cookie     uint64
	Attr       *FileAttr // optional
	FileHandle []byte    // optional
}

// ReadDirPlus reads entries from a directory with their attributes.
// RFC 1813 Section 3.3.17
func (h *DefaultNFSHandler) ReadDirPlus(repository metadata.Repository, req *ReadDirPlusRequest) (*ReadDirPlusResponse, error) {
	logger.Debug("READDIRPLUS for directory %x, cookie=%d", req.DirHandle, req.Cookie)

	// Get directory attributes
	dirAttr, err := repository.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Directory not found: %v", err)
		return &ReadDirPlusResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Handle is not a directory")
		return &ReadDirPlusResponse{Status: NFS3ErrNotDir}, nil
	}

	// Get all children
	children, err := repository.GetChildren(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Error("Failed to get children: %v", err)
		return &ReadDirPlusResponse{Status: NFS3ErrIO}, nil
	}

	// Generate file ID for directory
	dirFileid := binary.BigEndian.Uint64(req.DirHandle[:8])
	nfsDirAttr := MetadataToNFSAttr(dirAttr, dirFileid)

	// Build entries list
	entries := make([]*DirPlusEntry, 0)
	cookie := uint64(1)

	// Add "." entry
	if req.Cookie == 0 {
		entries = append(entries, &DirPlusEntry{
			Fileid:     dirFileid,
			Name:       ".",
			Cookie:     cookie,
			Attr:       nfsDirAttr,
			FileHandle: req.DirHandle,
		})
		cookie++
	}

	// Add ".." entry
	if req.Cookie <= 1 {
		parentFileid := dirFileid
		parentHandle := req.DirHandle
		parentAttr := nfsDirAttr

		ph, err := repository.GetParent(metadata.FileHandle(req.DirHandle))
		if err == nil {
			parentHandle = ph
			parentFileid = binary.BigEndian.Uint64(ph[:8])
			if pAttr, err := repository.GetFile(ph); err == nil {
				parentAttr = MetadataToNFSAttr(pAttr, parentFileid)
			}
		}

		entries = append(entries, &DirPlusEntry{
			Fileid:     parentFileid,
			Name:       "..",
			Cookie:     cookie,
			Attr:       parentAttr,
			FileHandle: parentHandle,
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

		// Get attributes for this entry
		var entryAttr *FileAttr
		if attr, err := repository.GetFile(handle); err == nil {
			entryAttr = MetadataToNFSAttr(attr, fileid)
		}

		entries = append(entries, &DirPlusEntry{
			Fileid:     fileid,
			Name:       name,
			Cookie:     cookie,
			Attr:       entryAttr,
			FileHandle: handle,
		})
		cookie++
	}

	logger.Info("READDIRPLUS returning %d entries (eof=true)", len(entries))

	return &ReadDirPlusResponse{
		Status:     NFS3OK,
		DirAttr:    nfsDirAttr,
		CookieVerf: 0,
		Entries:    entries,
		Eof:        true,
	}, nil
}

func DecodeReadDirPlusRequest(data []byte) (*ReadDirPlusRequest, error) {
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

	// Read dircount
	var dirCount uint32
	if err := binary.Read(reader, binary.BigEndian, &dirCount); err != nil {
		return nil, fmt.Errorf("read dircount: %w", err)
	}

	// Read maxcount
	var maxCount uint32
	if err := binary.Read(reader, binary.BigEndian, &maxCount); err != nil {
		return nil, fmt.Errorf("read maxcount: %w", err)
	}

	return &ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     cookie,
		CookieVerf: cookieVerf,
		DirCount:   dirCount,
		MaxCount:   maxCount,
	}, nil
}

func (resp *ReadDirPlusResponse) Encode() ([]byte, error) {
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

		// Write name_attributes (post-op attributes)
		if entry.Attr != nil {
			if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
				return nil, err
			}
			if err := encodeFileAttr(&buf, entry.Attr); err != nil {
				return nil, err
			}
		} else {
			if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
				return nil, err
			}
		}

		// Write name_handle (post-op file handle)
		if entry.FileHandle != nil {
			if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
				return nil, err
			}

			handleLen := uint32(len(entry.FileHandle))
			if err := binary.Write(&buf, binary.BigEndian, handleLen); err != nil {
				return nil, err
			}
			buf.Write(entry.FileHandle)

			// Add padding
			padding := (4 - (handleLen % 4)) % 4
			for i := uint32(0); i < padding; i++ {
				buf.WriteByte(0)
			}
		} else {
			if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
				return nil, err
			}
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
