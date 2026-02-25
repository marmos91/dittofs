package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// PathConfRequest represents a PATHCONF request from an NFS client.
// The client provides a file handle to query POSIX-compatible information
// about the filesystem.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.20 specifies the PATHCONF procedure as:
//
//	PATHCONF3res NFSPROC3_PATHCONF(PATHCONF3args) = 20;
//
// Where PATHCONF3args contains:
//   - object: File handle for a filesystem object
//
// The PATHCONF procedure retrieves POSIX-style filesystem information
// that may vary per file or directory. This complements FSINFO which
// returns server-wide capabilities.
type PathConfRequest struct {
	// Handle is the file handle for which to retrieve PATHCONF information.
	// Can be the handle of any file or directory within the filesystem.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// PathConfResponse represents the response to a PATHCONF request.
// It contains POSIX-compatible filesystem properties that help clients
// understand filesystem behavior and limitations.
//
// The response is encoded in XDR format before being sent back to the client.
type PathConfResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains post-operation attributes of the file system object.
	// Present when Status == types.NFS3OK. May be nil if attributes are unavailable.
	// These attributes help clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Linkmax is the maximum number of hard links to a file.
	// Only present when Status == types.NFS3OK.
	// Typical value: 32767 or higher for modern filesystems.
	Linkmax uint32

	// NameMax is the maximum length of a filename component in bytes.
	// Only present when Status == types.NFS3OK.
	// Typical value: 255 bytes for most modern filesystems.
	NameMax uint32

	// NoTrunc indicates whether the server rejects names longer than NameMax.
	// Only present when Status == types.NFS3OK.
	//   - true: Server rejects long names (returns NFS3ErrNameTooLong)
	//   - false: Server silently truncates long names
	// Modern servers typically set this to true.
	NoTrunc bool

	// ChownRestricted indicates if chown is restricted to the superuser.
	// Only present when Status == types.NFS3OK.
	//   - true: Only root/superuser can change file ownership
	//   - false: File owner can give away ownership
	// POSIX-compliant systems typically set this to true for security.
	ChownRestricted bool

	// CaseInsensitive indicates if filename comparisons are case-insensitive.
	// Only present when Status == types.NFS3OK.
	//   - true: "File.txt" and "file.txt" refer to the same file
	//   - false: Filenames are case-sensitive (POSIX standard)
	// Unix/Linux systems typically set this to false.
	CaseInsensitive bool

	// CasePreserving indicates if the filesystem preserves filename case.
	// Only present when Status == types.NFS3OK.
	//   - true: Filenames maintain their original case
	//   - false: Case information may be lost
	// Most modern filesystems set this to true.
	CasePreserving bool
}

// ============================================================================
// Protocol Handler
// ============================================================================

// PathConf handles NFS PATHCONF (RFC 1813 Section 3.3.20).
// Returns POSIX filesystem properties: max links, name length, case sensitivity, chown restrictions.
// Delegates to MetadataService.GetFile and GetFilesystemCapabilities for attribute and capability data.
// No side effects; read-only, low-frequency operation (typically called once at mount time).
// Errors: NFS3ErrBadHandle (invalid handle), NFS3ErrStale (not found), NFS3ErrIO.
func (h *Handler) PathConf(
	ctx *NFSHandlerContext,
	req *PathConfRequest,
) (*PathConfResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "PATHCONF", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "PATHCONF cancelled", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "error", ctx.Context.Err())
		return &PathConfResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Validate file handle
	// ========================================================================

	if err := validatePathConfHandle(req.Handle); err != nil {
		logger.WarnCtx(ctx.Context, "PATHCONF validation failed", "client", clientIP, "error", err)
		return &PathConfResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Verify the file handle exists and is valid in the store
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "PATHCONF cancelled before GetFile", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "error", ctx.Context.Err())
		return &PathConfResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "PATHCONF failed: metadata service not initialized", "client", clientIP, "error", err)
		return &PathConfResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	logger.DebugCtx(ctx.Context, "PATHCONF", "share", ctx.Share)

	file, status, err := h.getFileOrError(ctx, fileHandle, "PATHCONF", req.Handle)
	if file == nil {
		return &PathConfResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// ========================================================================
	// Step 4: Retrieve filesystem capabilities from the store
	// ========================================================================
	// FilesystemCapabilities contains POSIX-compatible properties
	// that map directly to PATHCONF response fields

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "PATHCONF cancelled before GetCapabilities", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "error", ctx.Context.Err())
		return &PathConfResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	caps, err := metaSvc.GetFilesystemCapabilities(ctx.Context, fileHandle)
	if err != nil {
		logError(ctx.Context, err, "PATHCONF failed: could not get filesystem capabilities", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP)
		return &PathConfResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 5: Generate file attributes for cache consistency
	// ========================================================================

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

	logger.InfoCtx(ctx.Context, "PATHCONF successful", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP)
	logger.DebugCtx(ctx.Context, "PATHCONF properties", "linkmax", caps.MaxHardLinkCount, "namemax", caps.MaxFilenameLen, "no_trunc", !caps.TruncatesLongNames, "chown_restricted", caps.ChownRestricted, "case_insensitive", !caps.CaseSensitive, "case_preserving", caps.CasePreserving)

	// ========================================================================
	// Step 6: Build response with filesystem capabilities
	// ========================================================================
	// Map FilesystemCapabilities fields to PATHCONF response fields

	return &PathConfResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Linkmax:         caps.MaxHardLinkCount,
		NameMax:         caps.MaxFilenameLen,
		NoTrunc:         !caps.TruncatesLongNames,
		ChownRestricted: caps.ChownRestricted,
		CaseInsensitive: !caps.CaseSensitive,
		CasePreserving:  caps.CasePreserving,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// validatePathConfHandle validates a PATHCONF file handle.
//
// Checks performed:
//   - Handle is not empty
//   - Handle length is within RFC 1813 limits (max 64 bytes)
//   - Handle is long enough for file ID extraction (min 8 bytes)
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validatePathConfHandle(handle []byte) *validationError {
	if len(handle) == 0 {
		return &validationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(handle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(handle) < 8 {
		return &validationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	return nil
}
