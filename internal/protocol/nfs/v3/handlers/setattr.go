package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// SetAttrRequest represents a SETATTR request from an NFS client.
// The client provides a file handle and a set of attributes to modify.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.2 specifies the SETATTR procedure as:
//
//	SETATTR3res NFSPROC3_SETATTR(SETATTR3args) = 2;
//
// The SETATTR procedure changes one or more attributes of a file system object.
// It is one of the fundamental operations for file management, allowing changes
// to permissions, ownership, size, and timestamps.
type SetAttrRequest struct {
	// Handle is the file handle of the object whose attributes should be changed.
	// Must be a valid file handle obtained from MOUNT, LOOKUP, CREATE, etc.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// NewAttr contains the attributes to set.
	// Only the attributes with their corresponding Set* flags set to true
	// will be modified. Other attributes remain unchanged.
	NewAttr metadata.SetAttrs

	// Guard provides a conditional update mechanism based on ctime.
	// If Guard.Check is true, the server only proceeds with the update
	// if the current ctime matches Guard.Time. This prevents lost updates
	// when multiple clients modify the same file.
	//
	// The guard mechanism implements optimistic concurrency control:
	//   - Client reads file attributes (including ctime)
	//   - Client performs local operations
	//   - Client sends SETATTR with ctime guard
	//   - Server checks if ctime has changed
	//   - If unchanged: Apply updates (success)
	//   - If changed: Reject updates (NFS3ErrNotSync)
	Guard types.TimeGuard
}

// SetAttrResponse represents the response to a SETATTR request.
// It contains the status of the operation and WCC (Weak Cache Consistency)
// data for the modified object.
//
// The response is encoded in XDR format before being sent back to the client.
type SetAttrResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// AttrBefore contains pre-operation weak cache consistency data.
	// Used for cache consistency to help clients detect changes.
	// May be nil if attributes could not be captured.
	AttrBefore *types.WccAttr

	// AttrAfter contains post-operation attributes of the object.
	// Used for cache consistency to provide updated object state.
	// May be nil on error, but should be present on success.
	AttrAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// SetAttr sets the attributes of a file system object.
//
// This implements the NFS SETATTR procedure as defined in RFC 1813 Section 3.3.2.
//
// **Purpose:**
//
// SETATTR changes one or more attributes of a file system object. This includes:
//   - Permissions (mode)
//   - Ownership (UID/GID)
//   - File size (truncation/extension)
//   - Access time (atime)
//   - Modification time (mtime)
//
// Common use cases:
//   - chmod: Change file permissions
//   - chown: Change file ownership
//   - truncate: Change file size
//   - touch: Update timestamps
//   - utimes: Set specific timestamps
//
// **Process:**
//
//  1. Check for context cancellation (client disconnect, timeout)
//  2. Validate request parameters (handle format)
//  3. Extract client IP and authentication credentials from context
//  4. Verify file handle exists (via store)
//  5. Check guard condition if specified (optimistic concurrency control)
//  6. Delegate attribute updates to store.SetFileAttributes()
//  7. Return updated WCC data
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (attribute modification, validation, access control) delegated to store
//   - File handle validation performed by store.GetFile()
//   - Context cancellation respected throughout the operation
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the store, which implements:
//   - Ownership checks (only owner or root can chown)
//   - Permission checks (owner can chmod, write permission for size/time)
//   - Access control based on UID/GID
//   - Read-only filesystem checks
//
// **Guard Mechanism:**
//
// The guard provides optimistic concurrency control:
//  1. Client reads file attributes including ctime
//  2. Client performs local operations
//  3. Client sends SETATTR with ctime guard
//  4. Server checks if ctime changed (another client modified file)
//  5. If unchanged: Apply updates (success)
//  6. If changed: Reject updates (NFS3ErrNotSync - client should retry)
//
// This prevents lost updates when multiple clients modify the same file.
//
// **Attribute Selection:**
//
// Attributes are selectively updated based on Set* flags in NewAttr:
//   - SetMode: Update permission bits
//   - SetUID: Update owner user ID
//   - SetGID: Update owner group ID
//   - SetSize: Truncate or extend file
//   - SetAtime: Update access time
//   - SetMtime: Update modification time
//
// Unset attributes remain unchanged. This allows atomic multi-attribute
// updates without race conditions.
//
// **Size Changes:**
//
// Size changes require coordination with the content store:
//   - Truncation (new size < old size): Remove trailing content
//   - Extension (new size > old size): Pad with zeros
//   - Zero size: Delete all content
//
// The metadata store delegates content operations to the content store.
// Size changes can be time-consuming for large files, so context cancellation
// is particularly important.
//
// **Timestamp Semantics:**
//
// Per RFC 1813:
//   - SetAtime: Set to specified time or SET_TO_SERVER_TIME (current time)
//   - SetMtime: Set to specified time or SET_TO_SERVER_TIME (current time)
//   - Ctime: Always set to current time when any attribute changes
//
// Ctime (change time) is automatically updated by the server and cannot
// be set by the client - it's a server-managed metadata field.
//
// **Context Cancellation:**
//
// SETATTR can involve multiple operations (metadata update, content truncation).
// Context cancellation is checked at key points:
//   - Before starting the operation (client disconnect detection)
//   - During metadata lookup (before and after guard check)
//   - store operations respect context (passed through in AuthContext)
//
// Cancellation scenarios include:
//   - Client disconnects before completion
//   - Client timeout expires (especially for large truncations)
//   - Server shutdown initiated
//   - Network connection lost
//
// Size changes (truncation/extension) can be particularly time-consuming
// for large files, making cancellation support critical.
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Guard check failed → types.NFS3ErrNotSync
//   - Permission denied → NFS3ErrAcces
//   - Not owner → NFS3ErrPerm
//   - Read-only filesystem → NFS3ErrRoFs
//   - Invalid size → NFS3ErrInval
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → returns context error (client disconnect)
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients maintain cache coherency:
//  1. Capture file attributes before the operation (WccBefore)
//  2. Perform the attribute updates
//  3. Capture file attributes after the operation (WccAfter)
//
// Clients use this to:
//   - Detect if file changed during the operation
//   - Update their cached file attributes
//   - Invalidate stale cached data
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store enforces ownership and permission checks
//   - Only owner or root can change ownership
//   - Only owner can change permissions
//   - Size/time changes require write permission
//   - Guard prevents lost updates in concurrent scenarios
//   - Client context enables audit logging
//   - Cancellation prevents resource exhaustion
//
// **Parameters:**
//   - ctx: Context with client address, authentication, and cancellation support
//   - metadataStore: The metadata store for file operations
//   - req: The setattr request containing handle and new attributes
//
// **Returns:**
//   - *SetAttrResponse: Response with status and WCC data
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.2: SETATTR Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &SetAttrRequest{
//	    Handle: fileHandle,
//	    NewAttr: SetAttrs{
//	        SetMode: true,
//	        Mode:    0644, // rw-r--r--
//	    },
//	    Guard: TimeGuard{Check: false}, // No guard
//	}
//	ctx := &SetAttrContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.SetAttr(ctx, store, req)
//	if err == context.Canceled {
//	    // Client disconnected during setattr
//	    return nil, err
//	}
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Attributes updated successfully
//	}
func (h *Handler) SetAttr(
	ctx *NFSHandlerContext,
	req *SetAttrRequest,
) (*SetAttrResponse, error) {
	// ========================================================================
	// Context Cancellation Check - Entry Point
	// ========================================================================
	// Check if the client has disconnected or the request has timed out
	// before we start any operations. This is especially important for
	// SETATTR operations that may involve expensive content truncation.
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "SETATTR: request cancelled at entry",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", ctx.ClientAddr,
			"error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "SETATTR",
		"handle", fmt.Sprintf("%x", req.Handle),
		"guard", req.Guard.Check,
		"client", clientIP,
		"auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateSetAttrRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "SETATTR validation failed",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP,
			"error", err)
		return &SetAttrResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store from context
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(ctx.Share)
	if err != nil {
		logger.WarnCtx(ctx.Context, "SETATTR failed",
			"error", err,
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP)
		return &SetAttrResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	logger.DebugCtx(ctx.Context, "SETATTR",
		"share", ctx.Share)

	// ========================================================================
	// Step 3: Get current file attributes for WCC and guard check
	// ========================================================================

	currentFile, status, err := h.getFileOrError(ctx, metadataStore, fileHandle, "SETATTR", req.Handle)
	if currentFile == nil {
		return &SetAttrResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&currentFile.FileAttr)

	// ========================================================================
	// Handle Empty SETATTR (No-Op)
	// ========================================================================
	// If no attributes are specified, return success immediately with current
	// attributes. This is valid NFS behavior - macOS Finder and other clients
	// sometimes send empty SETATTR requests (possibly for access verification).
	// Note: This is a true no-op; ctime is NOT updated for empty SETATTR.

	if req.NewAttr.Mode == nil && req.NewAttr.UID == nil && req.NewAttr.GID == nil &&
		req.NewAttr.Size == nil && req.NewAttr.Atime == nil && req.NewAttr.Mtime == nil {

		logger.DebugCtx(ctx.Context, "SETATTR: no attributes specified (no-op)",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP)

		// Return current attributes without modification
		wccAfter := h.convertFileAttrToNFS(fileHandle, &currentFile.FileAttr)

		return &SetAttrResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// ========================================================================
	// Context Cancellation Check - After Metadata Lookup
	// ========================================================================
	// Check again after metadata lookup, before guard check and attribute update
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "SETATTR: request cancelled after metadata lookup",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP)
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 3: Check guard condition if specified
	// ========================================================================
	// The guard implements optimistic concurrency control by checking if
	// the file's ctime has changed since the client last read it.
	// If it has changed, another client has modified the file and this
	// operation should be rejected to prevent lost updates.

	if req.Guard.Check {
		currentCtime := xdr.TimeToTimeVal(currentFile.Ctime)

		// Compare ctime from guard with current ctime
		if currentCtime.Seconds != req.Guard.Time.Seconds ||
			currentCtime.Nseconds != req.Guard.Time.Nseconds {
			logger.DebugCtx(ctx.Context, "SETATTR guard check failed",
				"handle", fmt.Sprintf("%x", req.Handle),
				"expected", fmt.Sprintf("%d.%d", req.Guard.Time.Seconds, req.Guard.Time.Nseconds),
				"got", fmt.Sprintf("%d.%d", currentCtime.Seconds, currentCtime.Nseconds),
				"client", clientIP)

			// Get updated attributes for WCC data (best effort)
			var wccAfter *types.NFSFileAttr
			if file, err := metadataStore.GetFile(ctx.Context, fileHandle); err == nil {
				wccAfter = h.convertFileAttrToNFS(fileHandle, &file.FileAttr)
			}

			return &SetAttrResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotSync},
				AttrBefore:      wccBefore,
				AttrAfter:       wccAfter,
			}, nil
		}

		logger.DebugCtx(ctx.Context, "SETATTR guard check passed",
			"handle", fmt.Sprintf("%x", req.Handle),
			"ctime", fmt.Sprintf("%d.%d", currentCtime.Seconds, currentCtime.Nseconds))
	}

	// ========================================================================
	// Step 4: Build authentication context with share-level identity mapping
	// ========================================================================

	authCtx, wccAfter, err := h.buildAuthContextWithWCCError(ctx, fileHandle, &currentFile.FileAttr, "SETATTR", "", req.Handle)
	if authCtx == nil {
		return &SetAttrResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, err
	}

	// ========================================================================
	// Step 5: Apply attribute updates via store
	// ========================================================================
	// The store is responsible for:
	// - Checking ownership (for chown/chmod)
	// - Checking write permission (for size/time changes)
	// - Validating attribute values (e.g., invalid size)
	// - Coordinating with content store for size changes
	// - Updating ctime automatically
	// - Ensuring atomicity of updates
	// - Respecting context cancellation (especially for size changes)
	//
	// IMPORTANT: Per RFC 5661 Section 18.30.4, size changes should be applied
	// separately from other attribute changes because filesystems may not
	// expect size mixed with other attributes. This matches Linux kernel
	// behavior in fs/nfsd/vfs.c (nfsd_setattr).
	//
	// If both size and other attributes are being set:
	//   1. First apply size change (truncation/extension)
	//   2. Then apply other attributes (mode, uid, gid, atime, mtime)

	// Log which attributes are being set (for debugging)
	logSetAttrRequest(req, clientIP)

	// Check if we have both size and other attributes to set
	hasSize := req.NewAttr.Size != nil
	hasOtherAttrs := req.NewAttr.Mode != nil || req.NewAttr.UID != nil ||
		req.NewAttr.GID != nil || req.NewAttr.Atime != nil || req.NewAttr.Mtime != nil

	if hasSize && hasOtherAttrs {
		// Apply size change first (separate call per RFC 5661)
		sizeOnlyAttrs := metadata.SetAttrs{Size: req.NewAttr.Size}
		err = metadataStore.SetFileAttributes(authCtx, fileHandle, &sizeOnlyAttrs)
		if err != nil {
			// Handle size change error
			if err == context.Canceled || err == context.DeadlineExceeded {
				logger.DebugCtx(ctx.Context, "SETATTR: size change cancelled",
					"handle", fmt.Sprintf("%x", req.Handle),
					"client", clientIP)
				return nil, err
			}

			traceError(ctx.Context, err, "SETATTR failed: size change error",
				"handle", fmt.Sprintf("%x", req.Handle),
				"client", clientIP)

			var wccAfter *types.NFSFileAttr
			if file, getErr := metadataStore.GetFile(ctx.Context, fileHandle); getErr == nil {
				wccAfter = h.convertFileAttrToNFS(fileHandle, &file.FileAttr)
			}

			status := xdr.MapStoreErrorToNFSStatus(err, clientIP, "SETATTR")
			return &SetAttrResponse{
				NFSResponseBase: NFSResponseBase{Status: status},
				AttrBefore:      wccBefore,
				AttrAfter:       wccAfter,
			}, nil
		}

		// Now apply other attributes (without size)
		otherAttrs := metadata.SetAttrs{
			Mode:  req.NewAttr.Mode,
			UID:   req.NewAttr.UID,
			GID:   req.NewAttr.GID,
			Atime: req.NewAttr.Atime,
			Mtime: req.NewAttr.Mtime,
		}
		err = metadataStore.SetFileAttributes(authCtx, fileHandle, &otherAttrs)
	} else {
		// Only size or only other attributes - single call is fine
		err = metadataStore.SetFileAttributes(authCtx, fileHandle, &req.NewAttr)
	}
	if err != nil {
		// Check if error is due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.DebugCtx(ctx.Context, "SETATTR: store operation cancelled",
				"handle", fmt.Sprintf("%x", req.Handle),
				"client", clientIP)
			return nil, err
		}

		traceError(ctx.Context, err, "SETATTR failed: store error",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP)

		// Get updated attributes for WCC data (best effort)
		var wccAfter *types.NFSFileAttr
		if file, getErr := metadataStore.GetFile(ctx.Context, fileHandle); getErr == nil {
			wccAfter = h.convertFileAttrToNFS(fileHandle, &file.FileAttr)
		}

		// Map store errors to NFS status codes
		status := xdr.MapStoreErrorToNFSStatus(err, clientIP, "SETATTR")

		return &SetAttrResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			AttrBefore:      wccBefore,
			AttrAfter:       wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response with updated attributes
	// ========================================================================

	// Get updated file attributes for WCC data
	updatedFile, _, err := h.getFileOrError(ctx, metadataStore, fileHandle, "SETATTR", req.Handle)
	if updatedFile == nil && err != nil {
		// Context cancelled
		return nil, err
	}
	if updatedFile == nil {
		logger.WarnCtx(ctx.Context, "SETATTR: attributes updated but cannot get new attributes",
			"handle", fmt.Sprintf("%x", req.Handle))
		// Continue with nil WccAfter rather than failing the entire operation
		wccAfter = nil
	} else {
		wccAfter = h.convertFileAttrToNFS(fileHandle, &updatedFile.FileAttr)
	}

	logger.InfoCtx(ctx.Context, "SETATTR successful",
		"handle", fmt.Sprintf("%x", req.Handle),
		"client", clientIP)

	if updatedFile != nil {
		logger.DebugCtx(ctx.Context, "SETATTR details",
			"old_size", currentFile.Size,
			"new_size", updatedFile.Size,
			"old_mode", fmt.Sprintf("%o", currentFile.Mode),
			"new_mode", fmt.Sprintf("%o", updatedFile.Mode))
	} else {
		logger.DebugCtx(ctx.Context, "SETATTR details",
			"old_size", currentFile.Size,
			"old_mode", fmt.Sprintf("%o", currentFile.Mode))
	}

	return &SetAttrResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		AttrBefore:      wccBefore,
		AttrAfter:       wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// setAttrValidationError represents a SETATTR request validation error.
type setAttrValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *setAttrValidationError) Error() string {
	return e.message
}

// validateSetAttrRequest validates SETATTR request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - At least one attribute is being set
//   - Size value is valid (if being set)
//   - Mode value is valid (if being set)
//
// Returns:
//   - nil if valid
//   - *setAttrValidationError with NFS status if invalid
func validateSetAttrRequest(req *SetAttrRequest) *setAttrValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &setAttrValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.Handle) > 64 {
		return &setAttrValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &setAttrValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Note: Empty SETATTR (no attributes specified) is valid per NFS RFC.
	// Some clients (like macOS Finder) send empty SETATTR to verify access
	// or update ctime. We allow this and handle it as a no-op later in the
	// handler, returning current attributes without modification.

	// Validate and normalize mode value if being set
	// NOTE: This function modifies req.NewAttr.Mode in place for normalization.
	// This mutation is intentional and normalizes client-provided modes to a
	// canonical form before further processing.
	if req.NewAttr.Mode != nil {
		// Some clients (like macOS Finder) send mode values that include file type bits
		// in the upper bits (e.g., 0100644 for regular file, 040755 for directory).
		// We only use the permission bits (lower 12 bits) and ignore file type bits.
		// This is standard behavior - SETATTR cannot change file type, only permissions.

		// Strip file type bits and keep only permission bits (lower 12 bits)
		// This modifies the request in place to normalize the value for downstream processing.
		*req.NewAttr.Mode = *req.NewAttr.Mode & 0o7777
	}

	// Size validation is done by the store as it depends on file type
	// (e.g., cannot set size on directories)

	return nil
}

// ============================================================================
// Utility Functions
// ============================================================================

// logSetAttrRequest logs which attributes are being set in a SETATTR request.
// This provides detailed debugging information about the operation.
func logSetAttrRequest(req *SetAttrRequest, clientIP string) {
	attrs := make([]string, 0, 6)

	if req.NewAttr.Mode != nil {
		attrs = append(attrs, fmt.Sprintf("mode=0%o", *req.NewAttr.Mode))
	}
	if req.NewAttr.UID != nil {
		attrs = append(attrs, fmt.Sprintf("uid=%d", *req.NewAttr.UID))
	}
	if req.NewAttr.GID != nil {
		attrs = append(attrs, fmt.Sprintf("gid=%d", *req.NewAttr.GID))
	}
	if req.NewAttr.Size != nil {
		attrs = append(attrs, fmt.Sprintf("size=%d", *req.NewAttr.Size))
	}
	if req.NewAttr.Atime != nil {
		attrs = append(attrs, fmt.Sprintf("atime=%v", *req.NewAttr.Atime))
	}
	if req.NewAttr.Mtime != nil {
		attrs = append(attrs, fmt.Sprintf("mtime=%v", *req.NewAttr.Mtime))
	}

	if len(attrs) > 0 {
		logger.Debug("SETATTR attributes",
			"attrs", attrs,
			"client", clientIP)
	}
}
