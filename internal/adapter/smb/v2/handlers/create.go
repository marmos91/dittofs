// Package handlers provides SMB2 command handlers and session management.
//
// This file implements the SMB2 CREATE command handler [MS-SMB2] 2.2.13, 2.2.14.
// CREATE is one of the most complex SMB2 commands, used for both opening
// existing files and creating new files/directories.
package handlers

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// CreateRequest represents an SMB2 CREATE request from a client [MS-SMB2] 2.2.13.
//
// CREATE is the primary mechanism for clients to:
//   - Open existing files or directories
//   - Create new files or directories
//   - Supersede or overwrite existing files
//
// The request specifies the desired file/directory path and various options
// controlling how the operation should be performed.
//
// **Wire format (56 bytes fixed part):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 57 (includes 1 byte of buffer)
//	2       1     SecurityFlags      Security flags (reserved)
//	3       1     RequestedOplockLevel  Requested oplock level
//	4       4     ImpersonationLevel    Impersonation level
//	8       8     SmbCreateFlags     SMB create flags (reserved)
//	16      8     Reserved           Reserved (must be 0)
//	24      4     DesiredAccess      Requested access mask
//	28      4     FileAttributes     Initial file attributes
//	32      4     ShareAccess        Sharing mode
//	36      4     CreateDisposition  Create action disposition
//	40      4     CreateOptions      Create options flags
//	44      2     NameOffset         Offset to filename from header
//	46      2     NameLength         Length of filename in bytes
//	48      4     CreateContextsOffset   Offset to create contexts
//	52      4     CreateContextsLength   Length of create contexts
//	56+     var   Buffer             Filename (UTF-16LE) and contexts
//
// **Example:**
//
//	req := &CreateRequest{
//	    FileName:          "documents\\report.txt",
//	    CreateDisposition: types.FileOpenIf,
//	    CreateOptions:     types.FileNonDirectoryFile,
//	    DesiredAccess:     0x12019F, // Generic read/write
//	}
type CreateRequest struct {
	// OplockLevel is the requested opportunistic lock level.
	// Valid values: 0x00 (None), 0x01 (Level II), 0x08 (Batch), 0xFF (Lease)
	OplockLevel uint8

	// ImpersonationLevel specifies the impersonation level requested.
	// Valid values: 0-3 (Anonymous, Identification, Impersonation, Delegation)
	ImpersonationLevel uint32

	// DesiredAccess is a bit mask of requested access rights.
	// Common values:
	//   - 0x12019F: Generic read/write
	//   - 0x120089: Generic read
	//   - 0x120116: Generic write
	DesiredAccess uint32

	// FileAttributes specifies initial file attributes for new files.
	// Common values:
	//   - FileAttributeNormal (0x80): Normal file
	//   - FileAttributeDirectory (0x10): Directory
	//   - FileAttributeHidden (0x02): Hidden file
	FileAttributes types.FileAttributes

	// ShareAccess specifies the sharing mode for the file.
	// Bit mask: 0x01 (Read), 0x02 (Write), 0x04 (Delete)
	ShareAccess uint32

	// CreateDisposition specifies the action to take on file existence.
	// See types.CreateDisposition for values.
	CreateDisposition types.CreateDisposition

	// CreateOptions specifies options for the create operation.
	// See types.CreateOptions for bit flags.
	CreateOptions types.CreateOptions

	// FileName is the name/path of the file to create or open.
	// Uses backslash separators (Windows-style).
	FileName string

	// CreateContexts contains optional create context structures.
	// Used for extended operations like requesting lease, etc.
	CreateContexts []CreateContext
}

// CreateContext represents an SMB2 Create Context [MS-SMB2] 2.2.13.2.
//
// Create contexts provide extensibility for the CREATE command,
// allowing clients to request additional functionality.
type CreateContext struct {
	// Name identifies the type of create context.
	// Standard names: "MxAc", "QFid", "RqLs", etc.
	Name string

	// Data contains the context-specific data.
	Data []byte
}

// CreateResponse represents an SMB2 CREATE response to a client [MS-SMB2] 2.2.14.
//
// The response contains the outcome of the create operation including
// the file handle (FileID) and file attributes.
//
// **Wire format (88 bytes fixed part):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 89 (includes 1 byte of buffer)
//	2       1     OplockLevel        Granted oplock level
//	3       1     Flags              Response flags
//	4       4     CreateAction       Action taken (opened, created, etc.)
//	8       8     CreationTime       File creation time (FILETIME)
//	16      8     LastAccessTime     Last access time (FILETIME)
//	24      8     LastWriteTime      Last write time (FILETIME)
//	32      8     ChangeTime         Change time (FILETIME)
//	40      8     AllocationSize     Allocated size in bytes
//	48      8     EndOfFile          Actual file size in bytes
//	56      4     FileAttributes     File attributes
//	60      4     Reserved2          Reserved
//	64      16    FileId             SMB2 file identifier
//	80      4     CreateContextsOffset   Offset to response contexts
//	84      4     CreateContextsLength   Length of response contexts
//	88+     var   Buffer             Response contexts (if any)
type CreateResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// OplockLevel is the granted opportunistic lock level.
	OplockLevel uint8

	// Flags contains response flags.
	Flags uint8

	// CreateAction indicates what action was performed.
	// See types.CreateAction for values.
	CreateAction types.CreateAction

	// CreationTime is when the file was created (Windows FILETIME).
	CreationTime time.Time

	// LastAccessTime is when the file was last accessed.
	LastAccessTime time.Time

	// LastWriteTime is when the file was last written.
	LastWriteTime time.Time

	// ChangeTime is when the file metadata last changed.
	ChangeTime time.Time

	// AllocationSize is the allocated size in bytes (cluster-aligned).
	AllocationSize uint64

	// EndOfFile is the actual file size in bytes.
	EndOfFile uint64

	// FileAttributes contains the file's attributes.
	FileAttributes types.FileAttributes

	// FileID is the SMB2 file identifier used for subsequent operations.
	// This is a 16-byte value (8 bytes persistent + 8 bytes volatile).
	FileID [16]byte

	// CreateContexts contains response create contexts (if any).
	CreateContexts []CreateContext
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeCreateRequest parses an SMB2 CREATE request body [MS-SMB2] 2.2.13.
//
// This function extracts the fixed header fields and the variable-length
// filename from the request body.
//
// **Parameters:**
//   - body: Request body starting after the SMB2 header (64 bytes)
//
// **Returns:**
//   - *CreateRequest: Parsed request structure
//   - error: Decoding error if body is malformed
//
// **Example:**
//
//	req, err := DecodeCreateRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
//	// Use req.FileName, req.CreateDisposition, etc.
func DecodeCreateRequest(body []byte) (*CreateRequest, error) {
	if len(body) < 56 {
		return nil, fmt.Errorf("CREATE request too short: %d bytes", len(body))
	}

	req := &CreateRequest{
		OplockLevel:        body[3],
		ImpersonationLevel: binary.LittleEndian.Uint32(body[4:8]),
		DesiredAccess:      binary.LittleEndian.Uint32(body[24:28]),
		FileAttributes:     types.FileAttributes(binary.LittleEndian.Uint32(body[28:32])),
		ShareAccess:        binary.LittleEndian.Uint32(body[32:36]),
		CreateDisposition:  types.CreateDisposition(binary.LittleEndian.Uint32(body[36:40])),
		CreateOptions:      types.CreateOptions(binary.LittleEndian.Uint32(body[40:44])),
	}

	nameOffset := binary.LittleEndian.Uint16(body[44:46])
	nameLength := binary.LittleEndian.Uint16(body[46:48])

	// Extract filename (UTF-16LE encoded)
	// nameOffset is relative to the start of SMB2 header (64 bytes)
	// body starts after the header, so:
	//   body offset = nameOffset - 64
	// Typical nameOffset is 120 (64 header + 56 fixed part), giving body offset 56

	if nameLength > 0 {
		// Calculate where the name starts in our body buffer
		bodyOffset := int(nameOffset) - 64

		// Clamp to valid range (name can't start before the Buffer field at byte 56)
		if bodyOffset < 56 {
			bodyOffset = 56
		}

		// Extract the filename
		if bodyOffset+int(nameLength) <= len(body) {
			req.FileName = decodeUTF16LE(body[bodyOffset : bodyOffset+int(nameLength)])
		}
	}

	return req, nil
}

// Encode serializes the CreateResponse into SMB2 wire format [MS-SMB2] 2.2.14.
//
// **Returns:**
//   - []byte: 89-byte response body (no create contexts)
//   - error: Encoding error (currently always nil)
//
// **Example:**
//
//	resp := &CreateResponse{
//	    CreateAction:   types.FileCreated,
//	    FileID:         fileID,
//	    EndOfFile:      1024,
//	    FileAttributes: types.FileAttributeNormal,
//	}
//	data, err := resp.Encode()
func (resp *CreateResponse) Encode() ([]byte, error) {
	// Build response (89 bytes)
	buf := make([]byte, 89)
	binary.LittleEndian.PutUint16(buf[0:2], 89)                                          // StructureSize
	buf[2] = resp.OplockLevel                                                            // OplockLevel
	buf[3] = resp.Flags                                                                  // Flags
	binary.LittleEndian.PutUint32(buf[4:8], uint32(resp.CreateAction))                   // CreateAction
	binary.LittleEndian.PutUint64(buf[8:16], types.TimeToFiletime(resp.CreationTime))    // CreationTime
	binary.LittleEndian.PutUint64(buf[16:24], types.TimeToFiletime(resp.LastAccessTime)) // LastAccessTime
	binary.LittleEndian.PutUint64(buf[24:32], types.TimeToFiletime(resp.LastWriteTime))  // LastWriteTime
	binary.LittleEndian.PutUint64(buf[32:40], types.TimeToFiletime(resp.ChangeTime))     // ChangeTime
	binary.LittleEndian.PutUint64(buf[40:48], resp.AllocationSize)                       // AllocationSize
	binary.LittleEndian.PutUint64(buf[48:56], resp.EndOfFile)                            // EndOfFile
	binary.LittleEndian.PutUint32(buf[56:60], uint32(resp.FileAttributes))               // FileAttributes
	binary.LittleEndian.PutUint32(buf[60:64], 0)                                         // Reserved2
	copy(buf[64:80], resp.FileID[:])                                                     // FileId (persistent + volatile)
	binary.LittleEndian.PutUint32(buf[80:84], 0)                                         // CreateContextsOffset
	binary.LittleEndian.PutUint32(buf[84:88], 0)                                         // CreateContextsLength

	return buf, nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Create handles SMB2 CREATE command [MS-SMB2] 2.2.13, 2.2.14.
//
// CREATE is the fundamental operation for accessing files and directories.
// It handles both opening existing files and creating new ones.
//
// **Purpose:**
//
// The CREATE command allows clients to:
//   - Open existing files for reading or writing
//   - Create new files or directories
//   - Replace or supersede existing files
//   - Request opportunistic locks (oplocks)
//
// **Process:**
//
//  1. Validate tree connection and session
//  2. Resolve the file path from the share root
//  3. Check if the target file/directory exists
//  4. Apply create disposition rules:
//     - FILE_SUPERSEDE (0): Replace or create
//     - FILE_OPEN (1): Open only, fail if not exists
//     - FILE_CREATE (2): Create only, fail if exists
//     - FILE_OPEN_IF (3): Open or create
//     - FILE_OVERWRITE (4): Overwrite only, fail if not exists
//     - FILE_OVERWRITE_IF (5): Overwrite or create
//  5. Create or open the file as appropriate
//  6. Store open file handle in handler state
//  7. Return file handle and attributes
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidParameter: Malformed request
//   - StatusInvalidHandle: Invalid tree ID
//   - StatusUserSessionDeleted: Invalid session ID
//   - StatusBadNetworkName: Share not found
//   - StatusAccessDenied: Permission denied
//   - StatusObjectPathNotFound: Parent directory not found
//   - StatusObjectNameNotFound: File not found (for FILE_OPEN)
//   - StatusObjectNameCollision: File exists (for FILE_CREATE)
//   - StatusNotADirectory: Expected directory, got file
//   - StatusFileIsADirectory: Expected file, got directory
//
// **Parameters:**
//   - ctx: SMB handler context with session and tree information
//   - req: Parsed CREATE request
//
// **Returns:**
//   - *CreateResponse: Response with file handle and attributes
//   - error: Internal error (rare, most errors returned via response status)
func (h *Handler) Create(ctx *SMBHandlerContext, req *CreateRequest) (*CreateResponse, error) {
	logger.Debug("CREATE request",
		"filename", req.FileName,
		"disposition", req.CreateDisposition,
		"options", req.CreateOptions,
		"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess))

	// ========================================================================
	// Step 1: Get tree connection and validate session
	// ========================================================================

	tree, ok := h.GetTree(ctx.TreeID)
	if !ok {
		logger.Debug("CREATE: invalid tree ID", "treeID", ctx.TreeID)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	sess, ok := h.GetSession(ctx.SessionID)
	if !ok {
		logger.Debug("CREATE: invalid session ID", "sessionID", ctx.SessionID)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusUserSessionDeleted}}, nil
	}

	// Update context with session info
	ctx.ShareName = tree.ShareName
	ctx.User = sess.User
	ctx.IsGuest = sess.IsGuest
	ctx.Permission = tree.Permission

	// ========================================================================
	// Step 2: Check for IPC$ named pipe operations
	// ========================================================================

	if tree.ShareName == "/ipc$" {
		return h.handlePipeCreate(ctx, req, tree)
	}

	// ========================================================================
	// Step 3: Build AuthContext from SMB session
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("CREATE: failed to build auth context", "error", err)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 4: Resolve path and get parent directory
	// ========================================================================

	// Normalize the filename (convert backslashes to forward slashes)
	filename := strings.ReplaceAll(req.FileName, "\\", "/")
	filename = strings.TrimPrefix(filename, "/")

	// Get root handle for the share
	rootHandle, err := h.Registry.GetRootHandle(tree.ShareName)
	if err != nil {
		logger.Warn("CREATE: failed to get root handle", "share", tree.ShareName, "error", err)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusBadNetworkName}}, nil
	}

	// Handle root directory case
	if filename == "" {
		return h.handleOpenRootCreate(ctx, req, authCtx, rootHandle, tree)
	}

	// Split path into directory and name components
	dirPath := path.Dir(filename)
	baseName := path.Base(filename)

	// Walk to parent directory
	parentHandle := rootHandle
	if dirPath != "." && dirPath != "" {
		parentHandle, err = h.walkPath(authCtx, rootHandle, dirPath)
		if err != nil {
			logger.Debug("CREATE: parent path not found", "path", dirPath, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectPathNotFound}}, nil
		}
	}

	// ========================================================================
	// Step 5: Check if file exists
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	existingFile, lookupErr := metaSvc.Lookup(authCtx, parentHandle, baseName)
	fileExists := (lookupErr == nil)

	// Debug logging to trace file type issues in Lookup
	if fileExists {
		logger.Debug("CREATE Lookup result",
			"filename", filename,
			"fileType", int(existingFile.Type),
			"fileSize", existingFile.Size,
			"fileID", existingFile.ID.String(),
			"filePath", existingFile.Path)
	}

	// Check create options constraints
	isDirectoryRequest := req.CreateOptions&types.FileDirectoryFile != 0
	isNonDirectoryRequest := req.CreateOptions&types.FileNonDirectoryFile != 0

	// Validate directory vs file constraints for existing files
	if fileExists {
		if isDirectoryRequest && existingFile.Type != metadata.FileTypeDirectory {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotADirectory}}, nil
		}
		if isNonDirectoryRequest && existingFile.Type == metadata.FileTypeDirectory {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileIsADirectory}}, nil
		}
	}

	// ========================================================================
	// Step 6: Handle create disposition
	// ========================================================================

	createAction, dispErr := ResolveCreateDisposition(req.CreateDisposition, fileExists)
	if dispErr != nil {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(dispErr)}}, nil
	}

	// ========================================================================
	// Step 6b: Check write permission for create/overwrite operations
	// ========================================================================
	//
	// Write permission is required for:
	// - FileCreated: Creating new files or directories
	// - FileOverwritten: Truncating existing files
	// - FileSuperseded: Replacing existing files
	//
	// Read permission is sufficient for:
	// - FileOpened: Opening existing files for read

	if createAction != types.FileOpened && !HasWritePermission(ctx) {
		logger.Debug("CREATE: access denied (no write permission)",
			"path", filename,
			"action", createAction,
			"permission", ctx.Permission)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 7: Perform create/open operation
	// ========================================================================

	var file *metadata.File
	var fileHandle metadata.FileHandle

	switch createAction {
	case types.FileOpened:
		// Open existing file
		file = existingFile
		fileHandle, err = metadata.EncodeFileHandle(file)
		if err != nil {
			logger.Warn("CREATE: failed to encode handle", "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
		}

	case types.FileCreated:
		// Create new file or directory
		file, fileHandle, err = h.createNewFile(authCtx, parentHandle, baseName, req, isDirectoryRequest)
		if err != nil {
			logger.Warn("CREATE: failed to create file", "name", baseName, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}

	case types.FileOverwritten, types.FileSuperseded:
		// Open and truncate/replace existing file
		file, fileHandle, err = h.overwriteFile(authCtx, existingFile, req)
		if err != nil {
			logger.Warn("CREATE: failed to overwrite file", "name", baseName, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
		}
	}

	// ========================================================================
	// Step 8: Store open file with metadata handle
	// ========================================================================

	smbFileID := h.GenerateFileID()

	// ========================================================================
	// Step 8b: Request oplock or lease if applicable
	// ========================================================================
	//
	// Oplocks/leases are only granted for regular files, not directories.
	// The client's requested oplock level may be downgraded if there
	// are conflicting opens.
	//
	// SMB2.1+ clients prefer leases over oplocks (indicated by OplockLevel=0xFF).
	// When OplockLevel=0xFF, look for RqLs create context.

	var grantedOplock uint8
	var leaseResponse *LeaseResponseContext

	// Check for lease request (SMB2.1+)
	if req.OplockLevel == OplockLevelLease {
		// Look for RqLs create context
		if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
			// Use metadata handle as lock file handle
			lockFileHandle := lock.FileHandle(fileHandle)

			// Process lease request
			var err error
			leaseResponse, err = ProcessLeaseCreateContext(
				h.OplockManager,
				leaseCtx.Data,
				lockFileHandle,
				ctx.SessionID,
				fmt.Sprintf("smb:%d", ctx.SessionID), // Client ID
				tree.ShareName,
				file.Type == metadata.FileTypeDirectory,
			)
			if err != nil {
				logger.Debug("CREATE: lease context processing failed", "error", err)
			}

			// Set oplock level to lease if lease was granted
			if leaseResponse != nil && leaseResponse.LeaseState != lock.LeaseStateNone {
				grantedOplock = OplockLevelLease
			} else if leaseResponse != nil {
				// ================================================================
				// Cross-Protocol Handling: Lease denied
				// ================================================================
				//
				// When LeaseState=None and the client requested R/W lease, it may
				// be due to an NLM byte-range lock conflict. Per CONTEXT.md:
				// "NFS lock vs SMB Write lease: Deny SMB immediately"
				//
				// The CREATE still succeeds (file is opened), but without a lease.
				// The response includes LeaseState=0 in the RsLs context to inform
				// the client. The client may retry or proceed without caching.
				//
				// Note: We DON'T return STATUS_LOCK_NOT_GRANTED for the CREATE
				// because the file open succeeded - only the lease was denied.
				// STATUS_LOCK_NOT_GRANTED is for SMB LOCK requests, not CREATE.

				// Parse the original request to get the requested state
				if leaseReq, parseErr := DecodeLeaseCreateContext(leaseCtx.Data); parseErr == nil {
					requestedState := leaseReq.LeaseState
					if requestedState&(lock.LeaseStateRead|lock.LeaseStateWrite) != 0 {
						// Client requested Read or Write lease but got None
						// This could be due to NLM lock conflict or SMB lease conflict
						logger.Debug("CREATE: lease denied",
							"requestedState", lock.LeaseStateToString(requestedState),
							"grantedState", lock.LeaseStateToString(leaseResponse.LeaseState),
							"reason", "possibly NLM lock or SMB lease conflict")
					}
				}

				// Ensure OplockLevel=None when lease denied
				grantedOplock = OplockLevelNone
			}
		}
	}

	// Fall back to traditional oplocks if no lease was requested/granted
	if grantedOplock == 0 && file.Type == metadata.FileTypeRegular && req.OplockLevel != OplockLevelNone && req.OplockLevel != OplockLevelLease {
		// Build the oplock path using consistent helper
		oplockPath := BuildOplockPath(tree.ShareName, filename)
		grantedOplock = h.OplockManager.RequestOplock(
			oplockPath,
			smbFileID,
			ctx.SessionID,
			req.OplockLevel,
		)
	}

	openFile := &OpenFile{
		FileID:         smbFileID,
		TreeID:         ctx.TreeID,
		SessionID:      ctx.SessionID,
		Path:           filename,
		ShareName:      tree.ShareName,
		OpenTime:       time.Now(),
		DesiredAccess:  req.DesiredAccess,
		IsDirectory:    file.Type == metadata.FileTypeDirectory,
		MetadataHandle: fileHandle,
		PayloadID:      file.PayloadID,
		// Store parent info for delete-on-close support
		ParentHandle: parentHandle,
		FileName:     baseName,
		// Store oplock level
		OplockLevel: grantedOplock,
		// Set delete-on-close from create options
		DeletePending: req.CreateOptions&types.FileDeleteOnClose != 0,
	}
	h.StoreOpenFile(openFile)

	logger.Debug("CREATE successful",
		"fileID", fmt.Sprintf("%x", smbFileID),
		"filename", filename,
		"action", createAction,
		"isDirectory", openFile.IsDirectory,
		"fileType", int(file.Type),
		"fileSize", file.Size,
		"oplock", oplockLevelName(grantedOplock))

	// ========================================================================
	// Step 9: Notify change watchers
	// ========================================================================

	// Notify CHANGE_NOTIFY watchers about file system changes
	if h.NotifyRegistry != nil {
		parentPath := GetParentPath(filename)

		switch createAction {
		case types.FileCreated:
			// New file or directory created
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionAdded)
		case types.FileOverwritten, types.FileSuperseded:
			// Existing file was modified/replaced
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionModified)
		}
	}

	// ========================================================================
	// Step 10: Build success response
	// ========================================================================

	creation, access, write, change := FileAttrToSMBTimes(&file.FileAttr)
	// Use MFsymlink size for symlinks
	size := getSMBSize(&file.FileAttr)
	allocationSize := ((size + 4095) / 4096) * 4096

	resp := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     grantedOplock,
		CreateAction:    createAction,
		CreationTime:    creation,
		LastAccessTime:  access,
		LastWriteTime:   write,
		ChangeTime:      change,
		AllocationSize:  allocationSize,
		EndOfFile:       size,
		FileAttributes:  FileAttrToSMBAttributes(&file.FileAttr),
		FileID:          smbFileID,
	}

	// ========================================================================
	// Step 10b: Add lease response context if lease was granted
	// ========================================================================

	if leaseResponse != nil {
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: LeaseContextTagResponse,
			Data: leaseResponse.Encode(),
		})

		logger.Debug("CREATE: lease granted in response",
			"leaseKey", fmt.Sprintf("%x", leaseResponse.LeaseKey),
			"grantedState", lock.LeaseStateToString(leaseResponse.LeaseState),
			"epoch", leaseResponse.Epoch)
	}

	return resp, nil
}

// ============================================================================
// Named Pipe Handling (IPC$)
// ============================================================================

// handlePipeCreate handles CREATE on IPC$ for named pipes.
// Named pipes are used for DCE/RPC communication, e.g., srvsvc for share enumeration.
func (h *Handler) handlePipeCreate(ctx *SMBHandlerContext, req *CreateRequest, tree *TreeConnection) (*CreateResponse, error) {
	// Normalize pipe name (remove leading backslashes and "pipe\" prefix)
	pipeName := strings.ReplaceAll(req.FileName, "\\", "/")
	pipeName = strings.TrimPrefix(pipeName, "/")
	pipeName = strings.TrimPrefix(pipeName, "pipe/")
	pipeName = strings.ToLower(pipeName)

	logger.Debug("CREATE on IPC$ named pipe",
		"originalName", req.FileName,
		"normalizedName", pipeName)

	// Check if this is a supported pipe
	if !rpc.IsSupportedPipe(pipeName) {
		logger.Debug("CREATE: unsupported pipe", "pipeName", pipeName)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameNotFound}}, nil
	}

	// Update pipe metaSvc with current shares from registry.
	// TODO: This is called on every pipe CREATE which is inefficient under high load.
	// Consider caching the share list and invalidating on share add/remove events.
	if h.Registry != nil {
		shareNames := h.Registry.ListShares()
		shares := make([]rpc.ShareInfo1, 0, len(shareNames))
		for _, name := range shareNames {
			// Skip IPC$ virtual share
			if strings.EqualFold(name, "/ipc$") {
				continue
			}
			// Convert to share info
			displayName := strings.TrimPrefix(name, "/")
			shares = append(shares, rpc.ShareInfo1{
				Name:    displayName,
				Type:    rpc.STYPE_DISKTREE,
				Comment: "DittoFS share",
			})
		}
		h.PipeManager.SetShares(shares)
	}

	// Generate file ID for the pipe
	smbFileID := h.GenerateFileID()

	// Create pipe state
	h.PipeManager.CreatePipe(smbFileID, pipeName)

	// Store open file entry for the pipe
	openFile := &OpenFile{
		FileID:        smbFileID,
		TreeID:        ctx.TreeID,
		SessionID:     ctx.SessionID,
		Path:          req.FileName,
		ShareName:     tree.ShareName,
		OpenTime:      time.Now(),
		DesiredAccess: req.DesiredAccess,
		IsDirectory:   false,
		IsPipe:        true,
		PipeName:      pipeName,
	}
	h.StoreOpenFile(openFile)

	logger.Debug("CREATE pipe successful",
		"fileID", fmt.Sprintf("%x", smbFileID),
		"pipeName", pipeName)

	// Build success response
	now := time.Now()
	return &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     0,
		CreateAction:    types.FileOpened,
		CreationTime:    now,
		LastAccessTime:  now,
		LastWriteTime:   now,
		ChangeTime:      now,
		AllocationSize:  0,
		EndOfFile:       0,
		FileAttributes:  types.FileAttributeNormal,
		FileID:          smbFileID,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// handleOpenRootCreate handles opening the root directory of a share.
func (h *Handler) handleOpenRootCreate(
	ctx *SMBHandlerContext,
	req *CreateRequest,
	authCtx *metadata.AuthContext,
	rootHandle metadata.FileHandle,
	tree *TreeConnection,
) (*CreateResponse, error) {
	// Root can only be opened with FILE_OPEN disposition
	if req.CreateDisposition != types.FileOpen && req.CreateDisposition != types.FileOpenIf {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameCollision}}, nil
	}

	// Get root file attributes
	metaSvc := h.Registry.GetMetadataService()
	rootFile, err := metaSvc.GetFile(authCtx.Context, rootHandle)
	if err != nil {
		logger.Warn("CREATE: failed to get root file", "error", err)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameNotFound}}, nil
	}

	// Store open file
	smbFileID := h.GenerateFileID()
	openFile := &OpenFile{
		FileID:         smbFileID,
		TreeID:         ctx.TreeID,
		SessionID:      ctx.SessionID,
		Path:           "",
		ShareName:      tree.ShareName,
		OpenTime:       time.Now(),
		DesiredAccess:  req.DesiredAccess,
		IsDirectory:    true,
		MetadataHandle: rootHandle,
	}
	h.StoreOpenFile(openFile)

	creation, access, write, change := FileAttrToSMBTimes(&rootFile.FileAttr)

	return &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     0,
		CreateAction:    types.FileOpened,
		CreationTime:    creation,
		LastAccessTime:  access,
		LastWriteTime:   write,
		ChangeTime:      change,
		AllocationSize:  0,
		EndOfFile:       0,
		FileAttributes:  types.FileAttributeDirectory,
		FileID:          smbFileID,
	}, nil
}

// walkPath walks a path from a starting handle, returning the final handle.
func (h *Handler) walkPath(
	authCtx *metadata.AuthContext,
	startHandle metadata.FileHandle,
	pathStr string,
) (metadata.FileHandle, error) {
	currentHandle := startHandle
	metaSvc := h.Registry.GetMetadataService()

	// Split path into components
	parts := strings.Split(pathStr, "/")
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			// TODO: Handle parent directory navigation
			continue
		}

		file, err := metaSvc.Lookup(authCtx, currentHandle, part)
		if err != nil {
			return nil, err
		}

		if file.Type != metadata.FileTypeDirectory {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: fmt.Sprintf("%s is not a directory", part),
			}
		}

		currentHandle, err = metadata.EncodeFileHandle(file)
		if err != nil {
			return nil, err
		}
	}

	return currentHandle, nil
}

// createNewFile creates a new file or directory in the metadata store.
func (h *Handler) createNewFile(
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	req *CreateRequest,
	isDirectory bool,
) (*metadata.File, metadata.FileHandle, error) {
	// Build file attributes
	fileAttr := &metadata.FileAttr{
		Mode: SMBModeFromAttrs(req.FileAttributes, isDirectory),
	}

	// Set owner from auth context
	if authCtx.Identity.UID != nil {
		fileAttr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		fileAttr.GID = *authCtx.Identity.GID
	}

	if isDirectory {
		fileAttr.Type = metadata.FileTypeDirectory
	} else {
		fileAttr.Type = metadata.FileTypeRegular
		fileAttr.Size = 0
	}

	// Create appropriate file type based on fileAttr.Type
	metaSvc := h.Registry.GetMetadataService()
	var file *metadata.File
	var err error
	if isDirectory {
		file, err = metaSvc.CreateDirectory(authCtx, parentHandle, name, fileAttr)
	} else {
		file, err = metaSvc.CreateFile(authCtx, parentHandle, name, fileAttr)
	}

	if err != nil {
		return nil, nil, err
	}

	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		return nil, nil, err
	}

	return file, fileHandle, nil
}

// overwriteFile truncates an existing file for OVERWRITE/SUPERSEDE operations.
func (h *Handler) overwriteFile(
	authCtx *metadata.AuthContext,
	existingFile *metadata.File,
	req *CreateRequest,
) (*metadata.File, metadata.FileHandle, error) {
	fileHandle, err := metadata.EncodeFileHandle(existingFile)
	if err != nil {
		return nil, nil, err
	}

	// Truncate to zero size
	zeroSize := uint64(0)
	setAttrs := &metadata.SetAttrs{
		Size: &zeroSize,
	}

	metaSvc := h.Registry.GetMetadataService()
	err = metaSvc.SetFileAttributes(authCtx, fileHandle, setAttrs)
	if err != nil {
		return nil, nil, err
	}

	// Get updated file
	updatedFile, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		return nil, nil, err
	}

	return updatedFile, fileHandle, nil
}
