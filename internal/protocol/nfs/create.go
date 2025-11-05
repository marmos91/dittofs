package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// Create modes (RFC 1813 Section 3.3.8)
const (
	CreateUnchecked = 0 // Create file or truncate if exists
	CreateGuarded   = 1 // Create only if file doesn't exist
	CreateExclusive = 2 // Create exclusively with verifier
)

// CreateRequest represents a CREATE request
type CreateRequest struct {
	DirHandle []byte
	Filename  string
	Mode      uint32    // Create mode (unchecked/guarded/exclusive)
	Attr      *SetAttrs // Attributes to set
	Verf      uint64    // Verifier for exclusive create
}

// CreateResponse represents a CREATE response
type CreateResponse struct {
	Status     uint32
	FileHandle []byte    // Handle of created file (optional)
	Attr       *FileAttr // Post-op attributes of file (optional)
	DirBefore  *WccAttr  // Pre-op dir attributes (optional)
	DirAfter   *FileAttr // Post-op dir attributes (optional)
}

// Create creates a regular file.
// RFC 1813 Section 3.3.8
func (h *DefaultNFSHandler) Create(contentRepo content.Repository, metadataRepo metadata.Repository, req *CreateRequest) (*CreateResponse, error) {
	logger.Debug("CREATE file '%s' in directory %x, mode=%d", req.Filename, req.DirHandle, req.Mode)

	// Get directory attributes
	dirAttr, err := metadataRepo.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Directory not found: %v", err)
		return &CreateResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Handle is not a directory")
		return &CreateResponse{Status: NFS3ErrNotDir}, nil
	}

	// Store pre-op dir attributes for WCC
	dirWccAttr := &WccAttr{
		Size: dirAttr.Size,
		Mtime: TimeVal{
			Seconds:  uint32(dirAttr.Mtime.Unix()),
			Nseconds: uint32(dirAttr.Mtime.Nanosecond()),
		},
		Ctime: TimeVal{
			Seconds:  uint32(dirAttr.Ctime.Unix()),
			Nseconds: uint32(dirAttr.Ctime.Nanosecond()),
		},
	}

	// Check if file already exists
	existingHandle, err := metadataRepo.GetChild(metadata.FileHandle(req.DirHandle), req.Filename)
	fileExists := (err == nil)

	// Handle different create modes
	switch req.Mode {
	case CreateGuarded:
		if fileExists {
			logger.Debug("File already exists (guarded create)")
			return &CreateResponse{Status: NFS3ErrExist}, nil
		}
	case CreateExclusive:
		if fileExists {
			// Check verifier - if it matches, return success (idempotent)
			// For simplicity, we'll just return exist error
			logger.Debug("File already exists (exclusive create)")
			return &CreateResponse{Status: NFS3ErrExist}, nil
		}
	case CreateUnchecked:
		// If file exists, truncate it
		if fileExists {
			logger.Debug("File exists, truncating (unchecked create)")
			existingAttr, _ := metadataRepo.GetFile(existingHandle)
			if existingAttr != nil {
				existingAttr.Size = 0
				existingAttr.Mtime = time.Now()
				existingAttr.Ctime = time.Now()

				// Apply any new attributes from request
				if req.Attr != nil {
					if req.Attr.SetMode {
						existingAttr.Mode = req.Attr.Mode
					}
					if req.Attr.SetUID {
						existingAttr.UID = req.Attr.UID
					}
					if req.Attr.SetGID {
						existingAttr.GID = req.Attr.GID
					}
				}

				metadataRepo.UpdateFile(existingHandle, existingAttr)

				// Truncate content if it exists
				if existingAttr.ContentID != "" {
					if writeRepo, ok := contentRepo.(content.WriteRepository); ok {
						writeRepo.WriteAt(existingAttr.ContentID, []byte{}, 0)
					}
				}

				// Generate file ID
				fileid := binary.BigEndian.Uint64(existingHandle[:8])
				nfsAttr := MetadataToNFSAttr(existingAttr, fileid)

				// Update directory mtime
				dirAttr.Mtime = time.Now()
				dirAttr.Ctime = time.Now()
				metadataRepo.UpdateFile(metadata.FileHandle(req.DirHandle), dirAttr)

				dirFileid := binary.BigEndian.Uint64(req.DirHandle[:8])
				nfsDirAttr := MetadataToNFSAttr(dirAttr, dirFileid)

				logger.Info("CREATE successful (truncated existing): '%s'", req.Filename)
				return &CreateResponse{
					Status:     NFS3OK,
					FileHandle: existingHandle,
					Attr:       nfsAttr,
					DirBefore:  dirWccAttr,
					DirAfter:   nfsDirAttr,
				}, nil
			}
		}
	}

	// Create new file
	now := time.Now()
	fileAttr := &metadata.FileAttr{
		Type:      metadata.FileTypeRegular,
		Mode:      0644, // Default mode
		UID:       0,
		GID:       0,
		Size:      0,
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Will be set on first write
	}

	// Apply attributes from request
	if req.Attr != nil {
		if req.Attr.SetMode {
			fileAttr.Mode = req.Attr.Mode
		}
		if req.Attr.SetUID {
			fileAttr.UID = req.Attr.UID
		}
		if req.Attr.SetGID {
			fileAttr.GID = req.Attr.GID
		}
		if req.Attr.SetSize {
			fileAttr.Size = req.Attr.Size
		}
	}

	// Add file to repository
	fileHandle, err := metadataRepo.(interface {
		AddFileToDirectory(metadata.FileHandle, string, *metadata.FileAttr) (metadata.FileHandle, error)
	}).AddFileToDirectory(metadata.FileHandle(req.DirHandle), req.Filename, fileAttr)

	if err != nil {
		logger.Error("Failed to create file: %v", err)
		return &CreateResponse{Status: NFS3ErrIO}, nil
	}

	// Generate file ID
	fileid := binary.BigEndian.Uint64(fileHandle[:8])
	nfsAttr := MetadataToNFSAttr(fileAttr, fileid)

	// Update directory mtime
	dirAttr.Mtime = time.Now()
	dirAttr.Ctime = time.Now()
	metadataRepo.UpdateFile(metadata.FileHandle(req.DirHandle), dirAttr)

	dirFileid := binary.BigEndian.Uint64(req.DirHandle[:8])
	nfsDirAttr := MetadataToNFSAttr(dirAttr, dirFileid)

	logger.Info("CREATE successful: '%s' -> handle %x", req.Filename, fileHandle)

	return &CreateResponse{
		Status:     NFS3OK,
		FileHandle: fileHandle,
		Attr:       nfsAttr,
		DirBefore:  dirWccAttr,
		DirAfter:   nfsDirAttr,
	}, nil
}

func DecodeCreateRequest(data []byte) (*CreateRequest, error) {
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

	// Skip padding
	padding = (4 - (filenameLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		reader.ReadByte()
	}

	// Read create mode
	var mode uint32
	if err := binary.Read(reader, binary.BigEndian, &mode); err != nil {
		return nil, fmt.Errorf("read mode: %w", err)
	}

	req := &CreateRequest{
		DirHandle: dirHandle,
		Filename:  string(filenameBytes),
		Mode:      mode,
	}

	// Read attributes based on mode
	if mode == CreateExclusive {
		// Read verifier
		var verf uint64
		if err := binary.Read(reader, binary.BigEndian, &verf); err != nil {
			return nil, fmt.Errorf("read verifier: %w", err)
		}
		req.Verf = verf
	} else {
		// Read set attributes (similar to SETATTR)
		attr := &SetAttrs{}

		// Mode
		var setMode uint32
		if err := binary.Read(reader, binary.BigEndian, &setMode); err != nil {
			return nil, fmt.Errorf("read set_mode: %w", err)
		}
		attr.SetMode = (setMode == 1)
		if attr.SetMode {
			if err := binary.Read(reader, binary.BigEndian, &attr.Mode); err != nil {
				return nil, fmt.Errorf("read mode: %w", err)
			}
		}

		// UID
		var setUID uint32
		if err := binary.Read(reader, binary.BigEndian, &setUID); err != nil {
			return nil, fmt.Errorf("read set_uid: %w", err)
		}
		attr.SetUID = (setUID == 1)
		if attr.SetUID {
			if err := binary.Read(reader, binary.BigEndian, &attr.UID); err != nil {
				return nil, fmt.Errorf("read uid: %w", err)
			}
		}

		// GID
		var setGID uint32
		if err := binary.Read(reader, binary.BigEndian, &setGID); err != nil {
			return nil, fmt.Errorf("read set_gid: %w", err)
		}
		attr.SetGID = (setGID == 1)
		if attr.SetGID {
			if err := binary.Read(reader, binary.BigEndian, &attr.GID); err != nil {
				return nil, fmt.Errorf("read gid: %w", err)
			}
		}

		// Size
		var setSize uint32
		if err := binary.Read(reader, binary.BigEndian, &setSize); err != nil {
			return nil, fmt.Errorf("read set_size: %w", err)
		}
		attr.SetSize = (setSize == 1)
		if attr.SetSize {
			if err := binary.Read(reader, binary.BigEndian, &attr.Size); err != nil {
				return nil, fmt.Errorf("read size: %w", err)
			}
		}

		// Atime
		var setAtime uint32
		if err := binary.Read(reader, binary.BigEndian, &setAtime); err != nil {
			return nil, fmt.Errorf("read set_atime: %w", err)
		}
		if setAtime == 1 {
			attr.SetAtime = true
			if err := binary.Read(reader, binary.BigEndian, &attr.Atime.Seconds); err != nil {
				return nil, fmt.Errorf("read atime seconds: %w", err)
			}
			if err := binary.Read(reader, binary.BigEndian, &attr.Atime.Nseconds); err != nil {
				return nil, fmt.Errorf("read atime nseconds: %w", err)
			}
		} else if setAtime == 2 {
			attr.SetAtime = true
			now := time.Now()
			attr.Atime = TimeVal{
				Seconds:  uint32(now.Unix()),
				Nseconds: uint32(now.Nanosecond()),
			}
		}

		// Mtime
		var setMtime uint32
		if err := binary.Read(reader, binary.BigEndian, &setMtime); err != nil {
			return nil, fmt.Errorf("read set_mtime: %w", err)
		}
		if setMtime == 1 {
			attr.SetMtime = true
			if err := binary.Read(reader, binary.BigEndian, &attr.Mtime.Seconds); err != nil {
				return nil, fmt.Errorf("read mtime seconds: %w", err)
			}
			if err := binary.Read(reader, binary.BigEndian, &attr.Mtime.Nseconds); err != nil {
				return nil, fmt.Errorf("read mtime nseconds: %w", err)
			}
		} else if setMtime == 2 {
			attr.SetMtime = true
			now := time.Now()
			attr.Mtime = TimeVal{
				Seconds:  uint32(now.Unix()),
				Nseconds: uint32(now.Nanosecond()),
			}
		}

		req.Attr = attr
	}

	return req, nil
}

func (resp *CreateResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Write file handle (post-op FH)
	if resp.FileHandle != nil && resp.Status == NFS3OK {
		// handle_follows = TRUE
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}

		handleLen := uint32(len(resp.FileHandle))
		if err := binary.Write(&buf, binary.BigEndian, handleLen); err != nil {
			return nil, fmt.Errorf("write handle length: %w", err)
		}
		buf.Write(resp.FileHandle)

		// Add padding
		padding := (4 - (handleLen % 4)) % 4
		for i := uint32(0); i < padding; i++ {
			buf.WriteByte(0)
		}
	} else {
		// handle_follows = FALSE
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	// Write post-op file attributes
	if resp.Attr != nil && resp.Status == NFS3OK {
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

	// Write WCC data for directory
	// Pre-op attributes
	if resp.DirBefore != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.DirBefore.Size); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.DirBefore.Mtime.Seconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.DirBefore.Mtime.Nseconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.DirBefore.Ctime.Seconds); err != nil {
			return nil, err
		}
		if err := binary.Write(&buf, binary.BigEndian, resp.DirBefore.Ctime.Nseconds); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	// Post-op attributes
	if resp.DirAfter != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := encodeFileAttr(&buf, resp.DirAfter); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}
