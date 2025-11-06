package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ============================================================================
// Request/Response Structures
// ============================================================================

// MkdirRequest represents a MKDIR request from an NFS client.
// The client provides the parent directory handle, the name for the new directory,
// and optional attributes to set on the newly created directory.
//
// RFC 1813 Section 3.3.9 specifies the MKDIR procedure as:
//
//	MKDIR3res NFSPROC3_MKDIR(MKDIR3args) = 9;
//
// The request includes:
//   - Parent directory handle (where to create the new directory)
//   - Directory name (must be valid and not already exist)
//   - Attributes (mode, uid, gid - other attributes like size and times are ignored)
type MkdirRequest struct {
	// DirHandle is the file handle of the parent directory where the new directory
	// will be created. This must be a valid directory handle.
	DirHandle []byte

	// Name is the name of the directory to create.
	// Must follow NFS naming conventions:
	//   - Cannot be empty or "." or ".."
	//   - Maximum length is typically 255 bytes
	//   - Should not contain null bytes or path separators
	Name string

	// Attr contains the attributes to set on the new directory.
	// Only certain fields are relevant for MKDIR:
	//   - Mode: Directory permissions (e.g., 0755)
	//   - UID: Owner user ID
	//   - GID: Owner group ID
	// Other fields (size, times) are ignored and set by the server.
	Attr SetAttrs
}

// MkdirResponse represents the response to a MKDIR request.
// On success, it returns the new directory's file handle and attributes,
// plus WCC (Weak Cache Consistency) data for the parent directory.
//
// The WCC data helps clients maintain cache coherency by providing
// before-and-after attributes of the parent directory.
type MkdirResponse struct {
	// Status indicates the result of the mkdir operation.
	// Common values:
	//   - NFS3OK (0): Success
	//   - NFS3ErrExist (17): Directory already exists
	//   - NFS3ErrNoEnt (2): Parent directory not found
	//   - NFS3ErrNotDir (20): Parent handle is not a directory
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrNoSpc (28): No space left on device
	//   - NFS3ErrIO (5): I/O error
	//   - NFS3ErrInval (22): Invalid argument (e.g., bad name)
	//   - NFS3ErrNameTooLong (63): Directory name too long
	Status uint32

	// Handle is the file handle of the newly created directory.
	// Only present when Status == NFS3OK.
	// The handle can be used in subsequent NFS operations to access the directory.
	Handle []byte

	// Attr contains the attributes of the newly created directory.
	// Only present when Status == NFS3OK.
	// Includes mode, ownership, timestamps, etc.
	Attr *FileAttr

	// WccBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect if the parent
	// directory changed during the operation.
	WccBefore *WccAttr

	// WccAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide the updated parent state.
	WccAfter *FileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Mkdir creates a new directory within a parent directory.
//
// The mkdir process follows these steps:
//  1. Validate the parent directory handle exists and is a directory
//  2. Validate the directory name (length, characters, not "." or "..")
//  3. Check if a child with the same name already exists
//  4. Construct directory attributes (mode, ownership, timestamps)
//  5. Create the directory in the metadata repository
//  6. Log the operation and return the new directory handle
//
// Security and validation:
//   - Validates parent is a directory (not a file or symlink)
//   - Prevents creating "." or ".." directories
//   - Checks for duplicate names before creation
//   - Enforces maximum name length (255 bytes)
//   - Rejects names with invalid characters (null bytes, path separators)
//   - Creates directories with safe default permissions (0755) if not specified
//
// Error handling:
//   - Returns NFS3ErrNoEnt if parent directory doesn't exist
//   - Returns NFS3ErrNotDir if parent handle is not a directory
//   - Returns NFS3ErrExist if directory name already exists
//   - Returns NFS3ErrInval for invalid directory names
//   - Returns NFS3ErrNameTooLong for names exceeding 255 bytes
//   - Returns NFS3ErrIO for repository/internal errors
//
// Parameters:
//   - repository: The metadata repository for directory operations
//   - req: The mkdir request containing parent handle, name, and attributes
//
// Returns:
//   - *MkdirResponse: Response with status, new directory handle (if successful)
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// RFC 1813 Section 3.3.9: MKDIR Procedure
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &MkdirRequest{
//	    DirHandle: parentHandle,
//	    Name:      "documents",
//	    Attr:      SetAttrs{SetMode: true, Mode: 0755},
//	}
//	resp, err := handler.Mkdir(repository, req)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == NFS3OK {
//	    // Directory created successfully, use resp.Handle
//	}
func (h *DefaultNFSHandler) Mkdir(repository metadata.Repository, req *MkdirRequest) (*MkdirResponse, error) {
	logger.Debug("MKDIR: name='%s' parent=%x mode=%o", req.Name, req.DirHandle, req.Attr.Mode)

	// ========================================================================
	// Step 1: Validate directory name
	// ========================================================================

	if err := validateDirectoryName(req.Name); err != nil {
		logger.Warn("MKDIR failed: invalid directory name '%s': %v", req.Name, err)
		return &MkdirResponse{Status: err.(*directoryNameError).NFSStatus()}, nil
	}

	// ========================================================================
	// Step 2: Validate parent directory
	// ========================================================================

	parentHandle := metadata.FileHandle(req.DirHandle)
	parentAttr, err := repository.GetFile(parentHandle)
	if err != nil {
		logger.Warn("MKDIR failed: parent directory not found: handle=%x error=%v", req.DirHandle, err)
		return &MkdirResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := captureWccAttr(parentAttr)

	// Verify parent is actually a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("MKDIR failed: parent handle is not a directory: handle=%x type=%d", req.DirHandle, parentAttr.Type)
		return &MkdirResponse{
			Status:    NFS3ErrNotDir,
			WccBefore: wccBefore,
		}, nil
	}

	// ========================================================================
	// Step 3: Check for existing child with same name
	// ========================================================================

	_, err = repository.GetChild(parentHandle, req.Name)
	if err == nil {
		// Child exists
		logger.Debug("MKDIR failed: directory '%s' already exists in parent %x", req.Name, req.DirHandle)

		// Get updated parent attributes for WCC data
		parentAttr, _ = repository.GetFile(parentHandle)
		fileid := extractFileID(parentHandle)
		wccAfter := MetadataToNFSAttr(parentAttr, fileid)

		return &MkdirResponse{
			Status:    NFS3ErrExist,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Construct directory attributes
	// ========================================================================

	dirAttr := buildDirectoryAttributes(&req.Attr)

	logger.Debug("MKDIR: creating directory with mode=%o uid=%d gid=%d",
		dirAttr.Mode, dirAttr.UID, dirAttr.GID)

	// ========================================================================
	// Step 5: Create directory in repository
	// ========================================================================

	newHandle, err := repository.AddFileToDirectory(parentHandle, req.Name, dirAttr)
	if err != nil {
		logger.Error("MKDIR failed: repository error: name='%s' parent=%x error=%v",
			req.Name, req.DirHandle, err)

		// Get updated parent attributes for WCC data
		parentAttr, _ = repository.GetFile(parentHandle)
		fileid := extractFileID(parentHandle)
		wccAfter := MetadataToNFSAttr(parentAttr, fileid)

		return &MkdirResponse{
			Status:    NFS3ErrIO,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build response with new directory attributes
	// ========================================================================

	// Generate file ID from handle for NFS attributes
	fileid := binary.BigEndian.Uint64(newHandle[:8])
	nfsAttr := MetadataToNFSAttr(dirAttr, fileid)

	// Get updated parent attributes for WCC data
	parentAttr, _ = repository.GetFile(parentHandle)
	parentFileid := extractFileID(parentHandle)
	wccAfter := MetadataToNFSAttr(parentAttr, parentFileid)

	logger.Info("MKDIR successful: name='%s' parent=%x new_handle=%x mode=%o",
		req.Name, req.DirHandle, newHandle, dirAttr.Mode)

	return &MkdirResponse{
		Status:    NFS3OK,
		Handle:    newHandle,
		Attr:      nfsAttr,
		WccBefore: wccBefore,
		WccAfter:  wccAfter,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// buildDirectoryAttributes constructs directory attributes from the request,
// applying defaults for unspecified fields.
//
// Directories have specific requirements:
//   - Type must be FileTypeDirectory
//   - Size is typically 4096 (standard directory block size)
//   - ContentID is empty (directories don't have content blobs)
//   - Timestamps are set to current time
//
// Parameters:
//   - setAttrs: Requested attributes from the client (may be partial)
//
// Returns:
//   - *metadata.FileAttr: Complete directory attributes with defaults applied
func buildDirectoryAttributes(setAttrs *SetAttrs) *metadata.FileAttr {
	now := time.Now()

	// Default directory permissions: rwxr-xr-x (0755)
	mode := uint32(0755)
	if setAttrs.SetMode {
		// Ensure directory bit is set if client provided a mode
		mode = setAttrs.Mode | 0040000 // Add directory type bit
	}

	// Default ownership: root (UID 0, GID 0)
	// In production, you might want to use the authenticated user's credentials
	uid := uint32(0)
	if setAttrs.SetUID {
		uid = setAttrs.UID
	}

	gid := uint32(0)
	if setAttrs.SetGID {
		gid = setAttrs.GID
	}

	return &metadata.FileAttr{
		Type:      metadata.FileTypeDirectory,
		Mode:      mode,
		UID:       uid,
		GID:       gid,
		Size:      4096, // Standard directory size
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Directories don't have content blobs
	}
}

// ============================================================================
// Directory Name Validation
// ============================================================================

// directoryNameError represents an invalid directory name error with
// the corresponding NFS error status.
type directoryNameError struct {
	message   string
	nfsStatus uint32
}

func (e *directoryNameError) Error() string {
	return e.message
}

func (e *directoryNameError) NFSStatus() uint32 {
	return e.nfsStatus
}

// validateDirectoryName checks if a directory name is valid according to
// NFS and POSIX conventions.
//
// Validation rules:
//   - Must not be empty
//   - Must not be "." (current directory)
//   - Must not be ".." (parent directory)
//   - Must not exceed 255 bytes (NFS filename limit)
//   - Must not contain null bytes (string terminator)
//   - Must not contain '/' (path separator)
//   - Should not contain control characters (0x00-0x1F, 0x7F)
//
// Parameters:
//   - name: The directory name to validate
//
// Returns:
//   - error: directoryNameError with appropriate NFS status code if invalid, nil if valid
func validateDirectoryName(name string) error {
	// Check for empty name
	if name == "" {
		return &directoryNameError{
			message:   "directory name cannot be empty",
			nfsStatus: NFS3ErrInval,
		}
	}

	// Check for reserved names
	if name == "." || name == ".." {
		return &directoryNameError{
			message:   fmt.Sprintf("directory name cannot be '%s'", name),
			nfsStatus: NFS3ErrInval,
		}
	}

	// Check name length (NFS limit is typically 255 bytes)
	if len(name) > 255 {
		return &directoryNameError{
			message:   fmt.Sprintf("directory name too long: %d bytes (max 255)", len(name)),
			nfsStatus: NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes
	if strings.Contains(name, "\x00") {
		return &directoryNameError{
			message:   "directory name cannot contain null bytes",
			nfsStatus: NFS3ErrInval,
		}
	}

	// Check for path separators
	if strings.Contains(name, "/") {
		return &directoryNameError{
			message:   "directory name cannot contain path separators",
			nfsStatus: NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	// This prevents potential issues with terminal output and logs
	for i, r := range name {
		if r < 0x20 || r == 0x7F {
			return &directoryNameError{
				message:   fmt.Sprintf("directory name contains control character at position %d", i),
				nfsStatus: NFS3ErrInval,
			}
		}
	}

	return nil
}

// ============================================================================
// Request Decoder
// ============================================================================

// DecodeMkdirRequest decodes a MKDIR request from XDR-encoded bytes.
//
// The MKDIR request has the following XDR structure (RFC 1813 Section 3.3.9):
//
//	struct MKDIR3args {
//	    diropargs3   where;     // Parent dir handle + name
//	    sattr3       attributes; // Directory attributes to set
//	};
//
// Decoding process:
//  1. Read parent directory handle (variable length with padding)
//  2. Read directory name (variable length string with padding)
//  3. Read attributes structure (sattr3)
//
// XDR encoding details:
//   - All integers are 4-byte aligned (32-bit)
//   - Variable-length data (handles, strings) are length-prefixed
//   - Padding is added to maintain 4-byte alignment
//
// Parameters:
//   - data: XDR-encoded bytes containing the mkdir request
//
// Returns:
//   - *MkdirRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
func DecodeMkdirRequest(data []byte) (*MkdirRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)
	req := &MkdirRequest{}

	// ========================================================================
	// Decode parent directory handle
	// ========================================================================

	handle, err := decodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode handle: %w", err)
	}
	req.DirHandle = handle

	// ========================================================================
	// Decode directory name
	// ========================================================================

	name, err := decodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	req.Name = name

	// ========================================================================
	// Decode sattr3 attributes structure
	// ========================================================================

	attr, err := decodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}
	req.Attr = *attr

	return req, nil
}

// ============================================================================
// Response Encoder
// ============================================================================

// Encode serializes the MkdirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The MKDIR response has the following XDR structure (RFC 1813 Section 3.3.9):
//
//	struct MKDIR3res {
//	    nfsstat3    status;
//	    union switch (status) {
//	    case NFS3_OK:
//	        struct {
//	            post_op_fh3   obj;        // New directory handle
//	            post_op_attr  obj_attributes;
//	            wcc_data      dir_wcc;    // Parent directory WCC
//	        } resok;
//	    default:
//	        wcc_data      dir_wcc;
//	    } resfail;
//	};
//
// Encoding process:
//  1. Write status code (4 bytes)
//  2. If success (NFS3OK):
//     a. Write optional new directory handle
//     b. Write optional new directory attributes
//     c. Write WCC data for parent directory
//  3. If failure:
//     a. Write WCC data for parent directory (best effort)
//
// Parameters:
//   - resp: The response structure to encode
//
// Returns:
//   - []byte: XDR-encoded response bytes
//   - error: Encoding error (should be rare)
func (resp *MkdirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Success case: Write handle and attributes
	// ========================================================================

	if resp.Status == NFS3OK {
		// Write new directory handle (post_op_fh3 - optional)
		if err := encodeOptionalOpaque(&buf, resp.Handle); err != nil {
			return nil, fmt.Errorf("encode handle: %w", err)
		}

		// Write new directory attributes (post_op_attr - optional)
		if err := encodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode attributes: %w", err)
		}
	}

	// ========================================================================
	// Write WCC data for parent directory (both success and failure)
	// ========================================================================

	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.
	if err := encodeWccData(&buf, resp.WccBefore, resp.WccAfter); err != nil {
		return nil, fmt.Errorf("encode wcc data: %w", err)
	}

	return buf.Bytes(), nil
}
