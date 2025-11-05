package nfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// CreateRequest represents an NFS CREATE request (RFC 1813 Section 3.3.8).
// The CREATE procedure creates a new regular file.
type CreateRequest struct {
	// DirHandle is the file handle of the parent directory
	DirHandle []byte

	// Filename is the name of the file to create
	Filename string

	// Mode specifies the creation mode (unchecked/guarded/exclusive)
	Mode uint32

	// Attr contains optional attributes to set on the new file
	Attr *SetAttrs

	// Verf is the verifier for exclusive create mode
	Verf uint64
}

// CreateResponse represents an NFS CREATE response (RFC 1813 Section 3.3.8).
type CreateResponse struct {
	// Status is the NFS status code
	Status uint32

	// FileHandle is the handle of the created file (only if Status == NFS3OK)
	FileHandle []byte

	// Attr contains post-operation attributes of the created file
	Attr *FileAttr

	// DirBefore contains pre-operation weak cache consistency data for the directory
	DirBefore *WccAttr

	// DirAfter contains post-operation attributes for the directory
	DirAfter *FileAttr
}

// Create creates a regular file in the specified directory.
//
// This implements the NFS CREATE procedure as defined in RFC 1813 Section 3.3.8.
// The procedure supports three creation modes:
//
//   - UNCHECKED: Creates a file or truncates if it exists
//   - GUARDED: Creates a file only if it doesn't exist
//   - EXCLUSIVE: Creates a file with a verifier for idempotency
//
// Authentication: This layer performs no authentication. Auth checks should be
// performed by the caller based on the authenticated RPC credentials.
//
// Error Handling:
//   - Returns NFS3ErrNoEnt if the parent directory doesn't exist
//   - Returns NFS3ErrNotDir if DirHandle is not a directory
//   - Returns NFS3ErrExist if file exists in GUARDED or EXCLUSIVE mode
//   - Returns NFS3ErrIO for metadata or content repository errors
//   - Returns NFS3ErrInval for invalid filenames or attributes
func (h *DefaultNFSHandler) Create(
	contentRepo content.Repository,
	metadataRepo metadata.Repository,
	req *CreateRequest,
) (*CreateResponse, error) {
	logger.Debug("CREATE file '%s' in directory %x, mode=%d", req.Filename, req.DirHandle, req.Mode)

	// Validate request
	if err := validateCreateRequest(req); err != nil {
		logger.Warn("Invalid CREATE request: %v", err)
		return &CreateResponse{Status: NFS3ErrInval}, nil
	}

	// Get and validate parent directory
	dirAttr, err := metadataRepo.GetFile(metadata.FileHandle(req.DirHandle))
	if err != nil {
		logger.Warn("Parent directory not found: %v", err)
		return &CreateResponse{Status: NFS3ErrNoEnt}, nil
	}

	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("Parent handle %x is not a directory", req.DirHandle)
		return &CreateResponse{Status: NFS3ErrNotDir}, nil
	}

	// Capture pre-operation directory attributes for WCC
	dirWccAttr := captureWccAttr(dirAttr)

	// Check if file already exists
	existingHandle, err := metadataRepo.GetChild(metadata.FileHandle(req.DirHandle), req.Filename)
	fileExists := (err == nil)

	// Handle creation based on mode
	var fileHandle metadata.FileHandle
	var fileAttr *metadata.FileAttr

	switch req.Mode {
	case CreateGuarded:
		if fileExists {
			logger.Debug("File '%s' already exists (guarded create)", req.Filename)
			return &CreateResponse{Status: NFS3ErrExist}, nil
		}
		fileHandle, fileAttr, err = createNewFile(metadataRepo, req)

	case CreateExclusive:
		if fileExists {
			// RFC 1813: Should check verifier for idempotency
			// TODO: Implement verifier checking
			logger.Debug("File '%s' already exists (exclusive create)", req.Filename)
			return &CreateResponse{Status: NFS3ErrExist}, nil
		}
		fileHandle, fileAttr, err = createNewFile(metadataRepo, req)

	case CreateUnchecked:
		if fileExists {
			fileHandle = existingHandle
			fileAttr, err = truncateExistingFile(contentRepo, metadataRepo, existingHandle, req)
		} else {
			fileHandle, fileAttr, err = createNewFile(metadataRepo, req)
		}

	default:
		logger.Warn("Invalid create mode: %d", req.Mode)
		return &CreateResponse{Status: NFS3ErrInval}, nil
	}

	if err != nil {
		logger.Error("Failed to create file '%s': %v", req.Filename, err)
		return &CreateResponse{Status: NFS3ErrIO}, nil
	}

	// Link file to parent directory (if new file)
	if !fileExists || req.Mode != CreateUnchecked {
		if err := linkFileToParent(metadataRepo, fileHandle, metadata.FileHandle(req.DirHandle), req.Filename); err != nil {
			logger.Error("Failed to link file to parent: %v", err)
			// Cleanup the created file
			metadataRepo.DeleteFile(fileHandle)
			return &CreateResponse{Status: NFS3ErrIO}, nil
		}
	}

	// Update parent directory timestamps
	now := time.Now()
	dirAttr.Mtime = now
	dirAttr.Ctime = now
	if err := metadataRepo.UpdateFile(metadata.FileHandle(req.DirHandle), dirAttr); err != nil {
		logger.Warn("Failed to update directory timestamps: %v", err)
		// Non-fatal, continue
	}

	// Convert to NFS attributes
	fileID := extractFileID(fileHandle)
	nfsAttr := MetadataToNFSAttr(fileAttr, fileID)

	dirID := extractFileID(metadata.FileHandle(req.DirHandle))
	nfsDirAttr := MetadataToNFSAttr(dirAttr, dirID)

	logger.Info("CREATE successful: '%s' -> handle %x", req.Filename, fileHandle)

	return &CreateResponse{
		Status:     NFS3OK,
		FileHandle: fileHandle,
		Attr:       nfsAttr,
		DirBefore:  dirWccAttr,
		DirAfter:   nfsDirAttr,
	}, nil
}

// DecodeCreateRequest decodes an XDR-encoded CREATE request.
//
// The request format (RFC 1813 Section 3.3.8):
//
//	struct CREATE3args {
//	    diropargs3   where;
//	    createhow3   how;
//	};
//
// Returns an error if the request cannot be decoded.
func DecodeCreateRequest(data []byte) (*CreateRequest, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: %d bytes", len(data))
	}

	reader := bytes.NewReader(data)

	// Decode directory handle (opaque data)
	dirHandle, err := decodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// Decode filename (string)
	filename, err := decodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode filename: %w", err)
	}

	// Decode create mode
	var mode uint32
	if err := binary.Read(reader, binary.BigEndian, &mode); err != nil {
		return nil, fmt.Errorf("decode mode: %w", err)
	}

	req := &CreateRequest{
		DirHandle: dirHandle,
		Filename:  filename,
		Mode:      mode,
	}

	// Decode mode-specific data
	switch mode {
	case CreateExclusive:
		// Decode verifier (8 bytes)
		var verf uint64
		if err := binary.Read(reader, binary.BigEndian, &verf); err != nil {
			return nil, fmt.Errorf("decode verifier: %w", err)
		}
		req.Verf = verf

	case CreateUnchecked, CreateGuarded:
		// Decode sattr3 (set attributes)
		attr, err := decodeSetAttrs(reader)
		if err != nil {
			return nil, fmt.Errorf("decode attributes: %w", err)
		}
		req.Attr = attr

	default:
		return nil, fmt.Errorf("invalid create mode: %d", mode)
	}

	return req, nil
}

// validateCreateRequest validates the CREATE request parameters.
func validateCreateRequest(req *CreateRequest) error {
	if len(req.DirHandle) == 0 {
		return fmt.Errorf("empty directory handle")
	}

	if req.Filename == "" {
		return fmt.Errorf("empty filename")
	}

	if len(req.Filename) > 255 {
		return fmt.Errorf("filename too long: %d bytes", len(req.Filename))
	}

	// Check for invalid characters (basic check)
	if bytes.ContainsAny([]byte(req.Filename), "/\x00") {
		return fmt.Errorf("filename contains invalid characters")
	}

	if req.Mode > CreateExclusive {
		return fmt.Errorf("invalid create mode: %d", req.Mode)
	}

	return nil
}

// createNewFile creates a new file with the requested attributes.
func createNewFile(
	metadataRepo metadata.Repository,
	req *CreateRequest,
) (metadata.FileHandle, *metadata.FileAttr, error) {
	now := time.Now()

	// Initialize file attributes with defaults
	fileAttr := &metadata.FileAttr{
		Type:      metadata.FileTypeRegular,
		Mode:      0644, // Default: rw-r--r--
		UID:       0,
		GID:       0,
		Size:      0,
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Will be set on first write
	}

	// Apply requested attributes
	applySetAttrs(fileAttr, req.Attr)

	// Generate file handle
	fileHandle := generateFileHandle(req.DirHandle, req.Filename, now)

	// Create file in metadata repository
	if err := metadataRepo.CreateFile(fileHandle, fileAttr); err != nil {
		return nil, nil, fmt.Errorf("create file metadata: %w", err)
	}

	return fileHandle, fileAttr, nil
}

// truncateExistingFile truncates an existing file to the specified size and updates its attributes.
// If targetSize is not provided or req.Attr.SetSize is true, uses the size from req.Attr.
// Otherwise truncates to 0 (empty file).
func truncateExistingFile(
	contentRepo content.Repository,
	metadataRepo metadata.Repository,
	fileHandle metadata.FileHandle,
	req *CreateRequest,
) (*metadata.FileAttr, error) {
	// Get current attributes
	fileAttr, err := metadataRepo.GetFile(fileHandle)
	if err != nil {
		return nil, fmt.Errorf("get file attributes: %w", err)
	}

	now := time.Now()

	// Determine target size
	targetSize := uint64(0) // Default: truncate to empty
	if req.Attr != nil && req.Attr.SetSize {
		targetSize = req.Attr.Size
	}

	// Update file metadata
	fileAttr.Size = targetSize
	fileAttr.Mtime = now
	fileAttr.Ctime = now

	// Apply other requested attributes (mode, uid, gid, times)
	applySetAttrs(fileAttr, req.Attr)

	// Update metadata
	if err := metadataRepo.UpdateFile(fileHandle, fileAttr); err != nil {
		return nil, fmt.Errorf("update file metadata: %w", err)
	}

	// Truncate content if it exists
	if fileAttr.ContentID != "" {
		if writeRepo, ok := contentRepo.(content.WriteRepository); ok {
			if err := writeRepo.Truncate(fileAttr.ContentID, targetSize); err != nil {
				logger.Warn("Failed to truncate content to %d bytes: %v", targetSize, err)
				// Non-fatal, metadata is already updated
			}
		}
	}

	return fileAttr, nil
}

// linkFileToParent links a file to its parent directory.
func linkFileToParent(
	metadataRepo metadata.Repository,
	fileHandle metadata.FileHandle,
	parentHandle metadata.FileHandle,
	filename string,
) error {
	// Add as child to parent directory
	if err := metadataRepo.AddChild(parentHandle, filename, fileHandle); err != nil {
		return fmt.Errorf("add child: %w", err)
	}

	// Set parent relationship
	if err := metadataRepo.SetParent(fileHandle, parentHandle); err != nil {
		// Cleanup child link
		metadataRepo.DeleteChild(parentHandle, filename)
		return fmt.Errorf("set parent: %w", err)
	}

	return nil
}

// generateFileHandle creates a deterministic file handle.
func generateFileHandle(parentHandle []byte, filename string, timestamp time.Time) metadata.FileHandle {
	data := fmt.Sprintf("%x-%s-%d", parentHandle, filename, timestamp.UnixNano())
	hash := sha256.Sum256([]byte(data))
	return metadata.FileHandle(hash[:])
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
