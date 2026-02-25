package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
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
	Attr *metadata.SetAttrs

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
	NFSResponseBase // Embeds Status field and GetStatus() method

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

// ============================================================================
// Protocol Handler
// ============================================================================

// Mknod handles NFS MKNOD (RFC 1813 Section 3.3.11).
// Creates a special file: character/block device, socket, or FIFO.
// Delegates to MetadataService.CreateSpecialFile with device numbers and type.
// Creates special file metadata and parent entry; returns handle and parent WCC data.
// Errors: NFS3ErrExist, NFS3ErrNotDir, NFS3ErrPerm (privilege required), NFS3ErrIO.
func (h *Handler) Mknod(
	ctx *NFSHandlerContext,
	req *MknodRequest,
) (*MknodResponse, error) {
	// ========================================================================
	// Context Cancellation Check - Entry Point
	// ========================================================================
	// Check if the client has disconnected or the request has timed out
	// before we start processing. While MKNOD is typically fast (metadata only),
	// we should still respect cancellation to avoid wasted work.
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "MKNOD: request cancelled at entry", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	var mode uint32 = 0644 // Default
	if req.Attr != nil && req.Attr.Mode != nil {
		mode = *req.Attr.Mode
	}

	logger.InfoCtx(ctx.Context, "MKNOD", "name", req.Name, "type", specialFileTypeName(req.Type), "handle", fmt.Sprintf("%x", req.DirHandle), "mode", fmt.Sprintf("%o", mode), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateMknodRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "MKNOD validation failed", "name", req.Name, "type", req.Type, "client", clientIP, "error", err)
		return &MknodResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Decode share name from directory file handle
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "MKNOD failed: metadata service not initialized", "client", clientIP, "error", err)
		return &MknodResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	parentHandle := metadata.FileHandle(req.DirHandle)

	logger.DebugCtx(ctx.Context, "MKNOD", "share", ctx.Share, "name", req.Name, "type", req.Type)

	// ========================================================================
	// Step 3: Verify parent directory exists and is valid
	// ========================================================================

	parentFile, status, err := h.getFileOrError(ctx, parentHandle, "MKNOD", req.DirHandle)
	if parentFile == nil {
		return &MknodResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&parentFile.FileAttr)

	// Verify parent is actually a directory
	if parentFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "MKNOD failed: parent not a directory", "handle", fmt.Sprintf("%x", req.DirHandle), "type", parentFile.Type, "client", clientIP)

		// Get current parent state for WCC
		wccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &MknodResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 3: Build AuthContext for permission checking
	// ========================================================================

	authCtx, wccAfter, err := h.buildAuthContextWithWCCError(ctx, parentHandle, &parentFile.FileAttr, "MKNOD", req.Name, req.DirHandle)
	if authCtx == nil {
		return &MknodResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    wccAfter,
		}, err
	}

	// ========================================================================
	// Context Cancellation Check - After Parent Lookup
	// ========================================================================
	// Check again after parent verification, before child operations
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "MKNOD: request cancelled after parent lookup", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 4: Check if special file name already exists using Lookup
	// ========================================================================

	_, err = metaSvc.Lookup(authCtx, parentHandle, req.Name)
	if err == nil {
		// Child exists (no error from Lookup)
		logger.DebugCtx(ctx.Context, "MKNOD failed: file already exists", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

		// Get updated parent attributes for WCC data
		updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

		return &MknodResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrExist},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    wccAfter,
		}, nil
	}
	// If error from Lookup, file doesn't exist (good) - continue

	// ========================================================================
	// Step 5: Create special file via store.CreateSpecialFile()
	// ========================================================================
	// The store is responsible for:
	// - Converting NFS file type to metadata file type
	// - Building complete file attributes with defaults
	// - Checking write permission on parent directory
	// - Checking privilege requirements (e.g., root-only for devices)
	// - Creating the special file metadata
	// - Storing device numbers (for block/char devices)
	// - Linking it to the parent
	// - Updating parent directory timestamps
	// - Respecting context cancellation

	// Build special file attributes
	fileAttr := &metadata.FileAttr{
		Type: nfsTypeToMetadataType(req.Type),
		Mode: 0644, // Default: rw-r--r--
		UID:  0,
		GID:  0,
	}

	// Apply context defaults (authenticated user's UID/GID)
	if authCtx.Identity.UID != nil {
		fileAttr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		fileAttr.GID = *authCtx.Identity.GID
	}

	// Apply explicit attributes from request
	if req.Attr != nil {
		if req.Attr.Mode != nil {
			fileAttr.Mode = *req.Attr.Mode
		}
		if req.Attr.UID != nil {
			fileAttr.UID = *req.Attr.UID
		}
		if req.Attr.GID != nil {
			fileAttr.GID = *req.Attr.GID
		}
	}

	// Create the special file
	newFile, err := metaSvc.CreateSpecialFile(
		authCtx,
		parentHandle,
		req.Name,
		nfsTypeToMetadataType(req.Type), // FileType parameter
		fileAttr,
		req.Spec.SpecData1, // Major device number (or 0 for non-devices)
		req.Spec.SpecData2, // Minor device number (or 0 for non-devices)
	)
	if err != nil {
		// Check if error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "MKNOD: creation cancelled", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)
			return nil, ctx.Context.Err()
		}

		logError(ctx.Context, err, "MKNOD failed: store error", "name", req.Name, "type", req.Type, "client", clientIP)

		// Get updated parent attributes for WCC data
		updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

		// Map store errors to NFS status codes
		status := mapMetadataErrorToNFS(err)

		return &MknodResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response with new file attributes
	// ========================================================================

	// Encode the file handle for the new special file
	newHandle, err := metadata.EncodeFileHandle(newFile)
	if err != nil {
		logError(ctx.Context, err, "MKNOD: failed to encode file handle")
		return &MknodResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Generate file ID from handle for NFS attributes
	nfsAttr := h.convertFileAttrToNFS(newHandle, &newFile.FileAttr)

	// Get updated parent attributes for WCC data
	updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
	wccAfter = h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

	logger.InfoCtx(ctx.Context, "MKNOD successful", "name", req.Name, "type", specialFileTypeName(req.Type), "handle", fmt.Sprintf("%x", newHandle), "mode", fmt.Sprintf("%o", newFile.Mode), "major", req.Spec.SpecData1, "minor", req.Spec.SpecData2, "client", clientIP)

	logger.DebugCtx(ctx.Context, "MKNOD details", "handle", fmt.Sprintf("%x", newHandle), "uid", newFile.UID, "gid", newFile.GID, "parent", fmt.Sprintf("%x", parentHandle))

	return &MknodResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		FileHandle:      newHandle,
		Attr:            nfsAttr,
		DirAttrBefore:   wccBefore,
		DirAttrAfter:    wccAfter,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// nfsTypeToMetadataType converts NFS file type to metadata file type.
//
// Note: The metadata interface uses more descriptive names:
//   - FileTypeCharDevice (not FileTypeChar)
//   - FileTypeBlockDevice (not FileTypeBlock)
func nfsTypeToMetadataType(nfsType uint32) metadata.FileType {
	switch nfsType {
	case types.NF3CHR:
		return metadata.FileTypeCharDevice
	case types.NF3BLK:
		return metadata.FileTypeBlockDevice
	case types.NF3SOCK:
		return metadata.FileTypeSocket
	case types.NF3FIFO:
		return metadata.FileTypeFIFO
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
// Request Validation
// ============================================================================

// validateMknodRequest validates MKNOD request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Special file name is valid (not empty, not "." or "..", length, characters)
//   - File type is valid for MKNOD (CHR, BLK, SOCK, FIFO only)
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validateMknodRequest(req *MknodRequest) *validationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &validationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &validationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate special file name
	if req.Name == "" {
		return &validationError{
			message:   "empty special file name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &validationError{
			message:   fmt.Sprintf("special file name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &validationError{
			message:   fmt.Sprintf("special file name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if bytes.Contains([]byte(req.Name), []byte{0}) {
		return &validationError{
			message:   "special file name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if bytes.Contains([]byte(req.Name), []byte{'/'}) {
		return &validationError{
			message:   "special file name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &validationError{
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
		return &validationError{
			message:   "use CREATE procedure for regular files, not MKNOD",
			nfsStatus: types.NFS3ErrInval,
		}
	case types.NF3DIR:
		return &validationError{
			message:   "use MKDIR procedure for directories, not MKNOD",
			nfsStatus: types.NFS3ErrInval,
		}
	case types.NF3LNK:
		return &validationError{
			message:   "use SYMLINK procedure for symbolic links, not MKNOD",
			nfsStatus: types.NFS3ErrInval,
		}
	default:
		return &validationError{
			message:   fmt.Sprintf("invalid file type for MKNOD: %d", req.Type),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate device numbers for block/char devices
	// Linux uses 8-bit major and 8-bit minor on older kernels, 12-bit major and 20-bit minor on newer.
	// We enforce reasonable limits to catch obvious errors and match Linux behavior.
	// Linux kernel fs/nfsd/nfs3proc.c validates: MAJOR(rdev) != specdata1 || MINOR(rdev) != specdata2
	if req.Type == types.NF3CHR || req.Type == types.NF3BLK {
		// Major device number should be in reasonable range (0-4095 covers most systems)
		// This matches Linux's 12-bit major number on modern systems
		if req.Spec.SpecData1 > 0xFFF {
			return &validationError{
				message:   fmt.Sprintf("major device number out of range: %d (max 4095)", req.Spec.SpecData1),
				nfsStatus: types.NFS3ErrInval,
			}
		}
		// Minor device number should be in reasonable range (0-1048575 for 20-bit minor)
		// This matches Linux's 20-bit minor number on modern systems
		if req.Spec.SpecData2 > 0xFFFFF {
			return &validationError{
				message:   fmt.Sprintf("minor device number out of range: %d (max 1048575)", req.Spec.SpecData2),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}
