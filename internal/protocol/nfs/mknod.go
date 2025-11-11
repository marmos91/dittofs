package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// MknodRequest represents a MKNOD request from an NFS client.
// The MKNOD procedure creates a special file (device, socket, or FIFO).
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.11 specifies the MKNOD procedure as:
//
//	MKNOD3res NFSPROC3_MKNOD(MKNOD3args) = 11;
//
// Special files include:
//   - Character devices (terminals, serial ports)
//   - Block devices (disks, partitions)
//   - Sockets (Unix domain sockets)
//   - FIFOs (named pipes)
//
// Regular files, directories, and symbolic links use CREATE, MKDIR, and SYMLINK instead.
type MknodRequest struct {
	// DirHandle is the file handle of the parent directory where the special file
	// will be created. Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name of the special file to create within the parent directory.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	//   - Should not contain control characters
	Name string

	// Type specifies the type of special file to create.
	// Valid values:
	//   - NF3CHR (4): Character special device
	//   - NF3BLK (3): Block special device
	//   - NF3SOCK (6): Unix domain socket
	//   - NF3FIFO (7): Named pipe (FIFO)
	// Note: NF3REG, NF3DIR, and NF3LNK are invalid for MKNOD
	Type uint32

	// Attr contains the attributes to set on the new special file.
	// Only certain fields are meaningful for MKNOD:
	//   - Mode: File permissions (e.g., 0644)
	//   - UID: Owner user ID
	//   - GID: Owner group ID
	// Other fields (size, times) are ignored and set by the server.
	Attr metadata.SetAttrs

	// Spec contains device-specific data (only for block/char devices).
	// For NF3CHR and NF3BLK:
	//   - SpecData1: Major device number
	//   - SpecData2: Minor device number
	// For NF3SOCK and NF3FIFO:
	//   - Ignored (should be zero)
	Spec DeviceSpec
}

// DeviceSpec contains device-specific data for block and character devices.
// This follows the Unix convention of major/minor device numbers.
//
// Device numbers identify which device driver handles the device:
//   - Major number: Identifies the device driver
//   - Minor number: Identifies the specific device instance
//
// Example: For /dev/sda1, major might be 8 (SCSI disk), minor might be 1 (first partition)
type DeviceSpec struct {
	// SpecData1 is the major device number.
	// Identifies the device driver or device type.
	SpecData1 uint32

	// SpecData2 is the minor device number.
	// Identifies the specific device instance.
	SpecData2 uint32
}

// MknodResponse represents the response to a MKNOD request.
// On success, it returns the new special file's handle and attributes,
// plus WCC (Weak Cache Consistency) data for the parent directory.
//
// The response is encoded in XDR format before being sent back to the client.
type MknodResponse struct {
	// Status indicates the result of the mknod operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - NFS3ErrExist (17): File already exists
	//   - types.NFS3ErrNoEnt (2): Parent directory not found
	//   - types.NFS3ErrNotDir (20): Parent handle is not a directory
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrInval (22): Invalid file type or argument
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrNameTooLong (63): Name too long
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// FileHandle is the handle of the newly created special file.
	// Only present when Status == types.NFS3OK.
	// The handle can be used in subsequent NFS operations.
	FileHandle []byte

	// Attr contains the attributes of the newly created special file.
	// Only present when Status == types.NFS3OK.
	// Includes mode, ownership, timestamps, etc.
	Attr *types.NFSFileAttr

	// DirAttrBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency. May be nil.
	DirAttrBefore *types.WccAttr

	// DirAttrAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency. May be nil on error.
	DirAttrAfter *types.NFSFileAttr
}

// MknodContext contains the context information needed to process a MKNOD request.
// This includes client identification and authentication details for access control.
type MknodContext struct {
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	AuthFlavor uint32

	// UID is the authenticated user ID (from AUTH_UNIX).
	// Used for default file ownership if not specified in Attr.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for default file ownership if not specified in Attr.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Mknod creates a special file (device, socket, or FIFO).
//
// This implements the NFS MKNOD procedure as defined in RFC 1813 Section 3.3.11.
//
// **Purpose:**
//
// MKNOD creates special files that represent devices, communication endpoints,
// or inter-process communication mechanisms. Common use cases include:
//   - Device files for hardware access (/dev/sda, /dev/tty)
//   - Unix domain sockets for IPC
//   - Named pipes (FIFOs) for process communication
//
// **Process:**
//
//  1. Check for context cancellation (client disconnect, timeout)
//  2. Validate request parameters (handle format, name syntax, file type)
//  3. Extract client IP and authentication credentials from context
//  4. Verify parent directory exists and is a directory (via repository)
//  5. Capture pre-operation parent state (for WCC)
//  6. Check for name conflicts
//  7. Delegate special file creation to repository.CreateSpecialFile()
//  8. Return new file handle and attributes with WCC data
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (creation, validation, access control) delegated to repository
//   - File handle validation performed by repository.GetFile()
//   - Context cancellation respected throughout the operation
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the repository, which can implement:
//   - Write permission checking on the parent directory
//   - Access control based on UID/GID
//   - Default ownership assignment for new special files
//   - Privilege checking (device creation often requires root)
//
// **Special File Types:**
//
// Valid file types for MKNOD:
//   - NF3CHR (4): Character device (e.g., /dev/tty, /dev/null)
//   - NF3BLK (3): Block device (e.g., /dev/sda, /dev/loop0)
//   - NF3SOCK (6): Unix domain socket
//   - NF3FIFO (7): Named pipe (FIFO)
//
// Invalid types (use other procedures instead):
//   - NF3REG (1): Regular file → use CREATE
//   - NF3DIR (2): Directory → use MKDIR
//   - NF3LNK (5): Symbolic link → use SYMLINK
//
// **Device Numbers:**
//
// For character and block devices (NF3CHR, NF3BLK):
//   - SpecData1 contains the major device number
//   - SpecData2 contains the minor device number
//   - These identify which kernel driver handles the device
//
// For sockets and FIFOs (NF3SOCK, NF3FIFO):
//   - Device numbers are ignored
//   - The file is a communication endpoint, not a device
//
// **Context Cancellation:**
//
// MKNOD is a metadata operation that typically completes quickly.
// Context cancellation is checked at key points:
//   - Before starting the operation (client disconnect detection)
//   - After parent directory lookup
//   - Repository operations respect context (passed through in AuthContext)
//
// Cancellation scenarios include:
//   - Client disconnects before completion
//   - Client timeout expires
//   - Server shutdown initiated
//   - Network connection lost
//
// **Security Considerations:**
//
// Creating special files has security implications:
//   - Device files provide direct hardware access
//   - Sockets can be used for privilege escalation
//   - FIFOs can be used for DoS attacks
//   - Most systems restrict device creation to root/admin
//
// The repository should enforce:
//   - Proper permission checking (typically root-only for devices)
//   - Write access to parent directory
//   - Quota enforcement
//   - Device number validation
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Parent not found → types.NFS3ErrNoEnt
//   - Parent not directory → types.NFS3ErrNotDir
//   - Name already exists → NFS3ErrExist
//   - Invalid type → NFS3ErrInval
//   - Invalid name → NFS3ErrInval
//   - Name too long → NFS3ErrNameTooLong
//   - Permission denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → returns context error (client disconnect)
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients detect if the parent directory changed:
//  1. Capture parent attributes before the operation (WccBefore)
//  2. Perform the special file creation
//  3. Capture parent attributes after the operation (WccAfter)
//
// Clients use this to maintain cache consistency and detect concurrent modifications.
//
// **Parameters:**
//   - ctx: Context with client address, authentication, and cancellation support
//   - repository: The metadata repository for file operations
//   - req: The mknod request containing parent handle, name, type, and attributes
//
// **Returns:**
//   - *MknodResponse: Response with status, new file handle (if successful),
//     and WCC data for the parent directory
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.11: MKNOD Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &MknodRequest{
//	    DirHandle: parentHandle,
//	    Name:      "my-device",
//	    Type:      NF3CHR,
//	    Attr:      SetAttrs{SetMode: true, Mode: 0644},
//	    Spec:      DeviceSpec{SpecData1: 1, SpecData2: 3}, // major=1, minor=3
//	}
//	ctx := &MknodContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Mknod(ctx, repository, req)
//	if err == context.Canceled {
//	    // Client disconnected during mknod
//	    return nil, err
//	}
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Special file created successfully, use resp.FileHandle
//	}
func (h *DefaultNFSHandler) Mknod(
	ctx *MknodContext,
	repository metadata.Repository,
	req *MknodRequest,
) (*MknodResponse, error) {
	// ========================================================================
	// Context Cancellation Check - Entry Point
	// ========================================================================
	// Check if the client has disconnected or the request has timed out
	// before we start processing. While MKNOD is typically fast (metadata only),
	// we should still respect cancellation to avoid wasted work.
	select {
	case <-ctx.Context.Done():
		logger.Debug("MKNOD: request cancelled at entry: name='%s' dir=%x client=%s error=%v",
			req.Name, req.DirHandle, ctx.ClientAddr, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
		// Context not cancelled, continue processing
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("MKNOD: name='%s' type=%s dir=%x mode=%o client=%s auth=%d",
		req.Name, specialFileTypeName(req.Type), req.DirHandle, req.Attr.Mode, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateMknodRequest(req); err != nil {
		logger.Warn("MKNOD validation failed: name='%s' type=%d client=%s error=%v",
			req.Name, req.Type, clientIP, err)
		return &MknodResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify parent directory exists and is valid
	// ========================================================================

	parentHandle := metadata.FileHandle(req.DirHandle)
	parentAttr, err := repository.GetFile(ctx.Context, parentHandle)
	if err != nil {
		// Check if error is due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("MKNOD: parent lookup cancelled: name='%s' dir=%x client=%s",
				req.Name, req.DirHandle, clientIP)
			return nil, err
		}

		logger.Warn("MKNOD failed: parent not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &MknodResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(parentAttr)

	// Verify parent is actually a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("MKNOD failed: parent not a directory: dir=%x type=%d client=%s",
			req.DirHandle, parentAttr.Type, clientIP)

		// Get current parent state for WCC
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MknodResponse{
			Status:        types.NFS3ErrNotDir,
			DirAttrBefore: wccBefore,
			DirAttrAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Context Cancellation Check - After Parent Lookup
	// ========================================================================
	// Check again after parent verification, before child operations
	select {
	case <-ctx.Context.Done():
		logger.Debug("MKNOD: request cancelled after parent lookup: name='%s' dir=%x client=%s",
			req.Name, req.DirHandle, clientIP)
		return nil, ctx.Context.Err()
	default:
		// Context not cancelled, continue processing
	}

	// ========================================================================
	// Step 3: Check if special file name already exists
	// ========================================================================

	_, err = repository.GetChild(ctx.Context, parentHandle, req.Name)
	if err == nil {
		// Child exists
		logger.Debug("MKNOD failed: file '%s' already exists: dir=%x client=%s",
			req.Name, req.DirHandle, clientIP)

		// Get updated parent attributes for WCC data
		parentAttr, _ = repository.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MknodResponse{
			Status:        types.NFS3ErrExist,
			DirAttrBefore: wccBefore,
			DirAttrAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Create special file via repository
	// ========================================================================
	// The repository is responsible for:
	// - Converting NFS file type to metadata file type
	// - Building complete file attributes with defaults
	// - Checking write permission on parent directory
	// - Checking privilege requirements (e.g., root-only for devices)
	// - Creating the special file metadata
	// - Storing device numbers (for block/char devices)
	// - Linking it to the parent
	// - Updating parent directory timestamps
	// - Respecting context cancellation

	// Build authentication context for repository
	authCtx := &metadata.AuthContext{
		Context:    ctx.Context, // Pass context through for cancellation
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	// Convert SetAttrs to metadata format for repository
	fileAttr := xdr.ConvertSetAttrsToMetadata(nfsTypeToMetadataType(req.Type), &req.Attr, authCtx)
	fileAttr.Type = nfsTypeToMetadataType(req.Type)

	// Create the special file
	newHandle, err := repository.CreateSpecialFile(
		authCtx,
		parentHandle,
		req.Name,
		fileAttr,
		req.Spec.SpecData1, // Major device number (or 0 for non-devices)
		req.Spec.SpecData2, // Minor device number (or 0 for non-devices)
	)
	if err != nil {
		// Check if error is due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("MKNOD: creation cancelled: name='%s' dir=%x client=%s",
				req.Name, req.DirHandle, clientIP)
			return nil, err
		}

		logger.Error("MKNOD failed: repository error: name='%s' type=%d client=%s error=%v",
			req.Name, req.Type, clientIP, err)

		// Get updated parent attributes for WCC data
		parentAttr, _ = repository.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		// Map repository errors to NFS status codes
		status := xdr.MapRepositoryErrorToNFSStatus(err, clientIP, "mknod")

		return &MknodResponse{
			Status:        status,
			DirAttrBefore: wccBefore,
			DirAttrAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 5: Build success response with new file attributes
	// ========================================================================

	// Get the newly created file's attributes
	newFileAttr, err := repository.GetFile(ctx.Context, newHandle)
	if err != nil {
		// Check if error is due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("MKNOD: final attribute lookup cancelled: name='%s' handle=%x client=%s",
				req.Name, newHandle, clientIP)
			return nil, err
		}

		logger.Error("MKNOD: failed to get new file attributes: handle=%x error=%v",
			newHandle, err)
		// This shouldn't happen, but handle gracefully
		return &MknodResponse{Status: types.NFS3ErrIO}, nil
	}

	// Generate file ID from handle for NFS attributes
	fileid := xdr.ExtractFileID(newHandle)
	nfsAttr := xdr.MetadataToNFS(newFileAttr, fileid)

	// Get updated parent attributes for WCC data
	parentAttr, _ = repository.GetFile(ctx.Context, parentHandle)
	parentFileid := xdr.ExtractFileID(parentHandle)
	wccAfter := xdr.MetadataToNFS(parentAttr, parentFileid)

	logger.Info("MKNOD successful: name='%s' type=%s handle=%x mode=%o major=%d minor=%d client=%s",
		req.Name, specialFileTypeName(req.Type), newHandle, newFileAttr.Mode,
		req.Spec.SpecData1, req.Spec.SpecData2, clientIP)

	logger.Debug("MKNOD details: fileid=%d uid=%d gid=%d parent=%d",
		fileid, newFileAttr.UID, newFileAttr.GID, parentFileid)

	return &MknodResponse{
		Status:        types.NFS3OK,
		FileHandle:    newHandle,
		Attr:          nfsAttr,
		DirAttrBefore: wccBefore,
		DirAttrAfter:  wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// mknodValidationError represents a MKNOD request validation error.
type mknodValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *mknodValidationError) Error() string {
	return e.message
}

// validateMknodRequest validates MKNOD request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Special file name is valid (not empty, not "." or "..", length, characters)
//   - File type is valid for MKNOD (CHR, BLK, SOCK, FIFO only)
//
// Returns:
//   - nil if valid
//   - *mknodValidationError with NFS status if invalid
func validateMknodRequest(req *MknodRequest) *mknodValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &mknodValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &mknodValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &mknodValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate special file name
	if req.Name == "" {
		return &mknodValidationError{
			message:   "empty special file name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &mknodValidationError{
			message:   fmt.Sprintf("special file name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &mknodValidationError{
			message:   fmt.Sprintf("special file name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if bytes.Contains([]byte(req.Name), []byte{0}) {
		return &mknodValidationError{
			message:   "special file name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if bytes.Contains([]byte(req.Name), []byte{'/'}) {
		return &mknodValidationError{
			message:   "special file name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &mknodValidationError{
				message:   fmt.Sprintf("special file name contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	// Validate file type - only special files are allowed
	// Regular files, directories, and symlinks use other procedures
	switch req.Type {
	case types.NF3CHR, types.NF3BLK, types.NF3SOCK, types.NF3FIFO:
		// Valid special file types
	case types.NF3REG:
		return &mknodValidationError{
			message:   "use CREATE procedure for regular files, not MKNOD",
			nfsStatus: types.NFS3ErrInval,
		}
	case types.NF3DIR:
		return &mknodValidationError{
			message:   "use MKDIR procedure for directories, not MKNOD",
			nfsStatus: types.NFS3ErrInval,
		}
	case types.NF3LNK:
		return &mknodValidationError{
			message:   "use SYMLINK procedure for symbolic links, not MKNOD",
			nfsStatus: types.NFS3ErrInval,
		}
	default:
		return &mknodValidationError{
			message:   fmt.Sprintf("invalid file type for MKNOD: %d", req.Type),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// nfsTypeToMetadataType converts NFS file type to metadata file type.
func nfsTypeToMetadataType(nfsType uint32) metadata.FileType {
	switch nfsType {
	case types.NF3CHR:
		return metadata.FileTypeChar
	case types.NF3BLK:
		return metadata.FileTypeBlock
	case types.NF3SOCK:
		return metadata.FileTypeSocket
	case types.NF3FIFO:
		return metadata.FileTypeFifo
	default:
		// This shouldn't happen due to validation, but handle gracefully
		return metadata.FileTypeRegular
	}
}

// specialFileTypeName returns a human-readable name for a special file type.
func specialFileTypeName(fileType uint32) string {
	switch fileType {
	case types.NF3CHR:
		return "CHARACTER_DEVICE"
	case types.NF3BLK:
		return "BLOCK_DEVICE"
	case types.NF3SOCK:
		return "SOCKET"
	case types.NF3FIFO:
		return "FIFO"
	case types.NF3REG:
		return "REGULAR_FILE"
	case types.NF3DIR:
		return "DIRECTORY"
	case types.NF3LNK:
		return "SYMLINK"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", fileType)
	}
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeMknodRequest decodes a MKNOD request from XDR-encoded bytes.
//
// The MKNOD request has the following XDR structure (RFC 1813 Section 3.3.11):
//
//	struct MKNOD3args {
//	    diropargs3   where;      // Parent dir handle + name
//	    mknoddata3   what;       // Type + attributes + device spec
//	};
//
//	union mknoddata3 switch (ftype3 type) {
//	case NF3CHR:
//	case NF3BLK:
//	    struct {
//	        sattr3     dev_attributes;
//	        specdata3  spec;
//	    } device;
//	case NF3SOCK:
//	case NF3FIFO:
//	    sattr3  pipe_attributes;
//	default:
//	    void;
//	};
//
// Decoding process:
//  1. Read parent directory handle (variable length with padding)
//  2. Read special file name (variable length string with padding)
//  3. Read file type (discriminated union)
//  4. Based on type:
//     - For CHR/BLK: Read attributes + device spec (major/minor)
//     - For SOCK/FIFO: Read attributes only
//
// XDR encoding details:
//   - All integers are 4-byte aligned (32-bit)
//   - Variable-length data (handles, strings) are length-prefixed
//   - Padding is added to maintain 4-byte alignment
//   - Discriminated unions use type field to determine structure
//
// Parameters:
//   - data: XDR-encoded bytes containing the mknod request
//
// Returns:
//   - *MknodRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
//
// Example:
//
//	data := []byte{...} // XDR-encoded MKNOD request from network
//	req, err := DecodeMknodRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Name, req.Type, req.Attr, req.Spec in MKNOD procedure
func DecodeMknodRequest(data []byte) (*MknodRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)
	req := &MknodRequest{}

	// ========================================================================
	// Decode parent directory handle
	// ========================================================================

	handle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}
	req.DirHandle = handle

	// ========================================================================
	// Decode special file name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	req.Name = name

	// ========================================================================
	// Decode file type (discriminated union)
	// ========================================================================

	var fileType uint32
	if err := binary.Read(reader, binary.BigEndian, &fileType); err != nil {
		return nil, fmt.Errorf("decode file type: %w", err)
	}
	req.Type = fileType

	// ========================================================================
	// Decode type-specific data based on discriminated union
	// ========================================================================

	switch fileType {
	case types.NF3CHR, types.NF3BLK:
		// Character and block devices: attributes + device spec
		attr, err := xdr.DecodeSetAttrs(reader)
		if err != nil {
			return nil, fmt.Errorf("decode device attributes: %w", err)
		}
		req.Attr = *attr

		// Decode device spec (major/minor numbers)
		if err := binary.Read(reader, binary.BigEndian, &req.Spec.SpecData1); err != nil {
			return nil, fmt.Errorf("decode major device number: %w", err)
		}
		if err := binary.Read(reader, binary.BigEndian, &req.Spec.SpecData2); err != nil {
			return nil, fmt.Errorf("decode minor device number: %w", err)
		}

	case types.NF3SOCK, types.NF3FIFO:
		// Sockets and FIFOs: attributes only (no device spec)
		attr, err := xdr.DecodeSetAttrs(reader)
		if err != nil {
			return nil, fmt.Errorf("decode pipe attributes: %w", err)
		}
		req.Attr = *attr

		// Device spec is not present for sockets and FIFOs
		req.Spec = DeviceSpec{SpecData1: 0, SpecData2: 0}

	default:
		// Invalid file type - this should have been caught by validation
		// but handle gracefully during decoding
		return nil, fmt.Errorf("invalid file type for MKNOD: %d (expected CHR/BLK/SOCK/FIFO)", fileType)
	}

	logger.Debug("Decoded MKNOD request: handle_len=%d name='%s' type=%d mode=%o major=%d minor=%d",
		len(handle), name, fileType, req.Attr.Mode, req.Spec.SpecData1, req.Spec.SpecData2)

	return req, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the MknodResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The MKNOD response has the following XDR structure (RFC 1813 Section 3.3.11):
//
//	struct MKNOD3resok {
//	    post_op_fh3   obj;           // New file handle
//	    post_op_attr  obj_attributes;
//	    wcc_data      dir_wcc;       // Parent directory WCC
//	};
//
//	struct MKNOD3resfail {
//	    wcc_data      dir_wcc;
//	};
//
// Encoding process:
//  1. Write status code (4 bytes)
//  2. If success (NFS3OK):
//     a. Write optional new file handle
//     b. Write optional new file attributes
//     c. Write WCC data for parent directory
//  3. If failure:
//     a. Write WCC data for parent directory (best effort)
//
// XDR encoding requires all data to be in big-endian format and aligned
// to 4-byte boundaries.
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
//
// Example:
//
//	resp := &MknodResponse{
//	    Status:        types.NFS3OK,
//	    FileHandle:    newFileHandle,
//	    Attr:          fileAttr,
//	    DirAttrBefore: wccBefore,
//	    DirAttrAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *MknodResponse) Encode() ([]byte, error) {
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

	if resp.Status == types.NFS3OK {
		// Write new file handle (post_op_fh3 - optional)
		if err := xdr.EncodeOptionalOpaque(&buf, resp.FileHandle); err != nil {
			return nil, fmt.Errorf("encode file handle: %w", err)
		}

		// Write new file attributes (post_op_attr - optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode file attributes: %w", err)
		}
	}

	// ========================================================================
	// Write WCC data for parent directory (both success and failure)
	// ========================================================================

	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.
	if err := xdr.EncodeWccData(&buf, resp.DirAttrBefore, resp.DirAttrAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded MKNOD response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}
