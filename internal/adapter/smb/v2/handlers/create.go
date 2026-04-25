package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/rpc"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// CreateRequest represents an SMB2 CREATE request from a client [MS-SMB2] 2.2.13.
//
// CREATE is the primary mechanism for clients to open existing files or
// directories, create new ones, or supersede/overwrite existing files.
// The request specifies the desired path and various options controlling
// how the operation should be performed. The fixed wire format is 56 bytes.
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
// The response contains the file handle (FileID), file attributes, timestamps,
// and the action taken (opened, created, overwritten). The fixed wire format
// is 88 bytes plus optional create context data.
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

	// AsyncId, when non-zero together with Status == StatusPending, signals the
	// dispatch layer to emit an interim async response (FlagAsync + AsyncId) for
	// this CREATE. The handler must have already registered a resume goroutine
	// in PendingCreateRegistry that will deliver the final response via
	// AsyncCreateCompleteCallback. See create.go park-on-lease-break path.
	AsyncId uint64
}

// GetAsyncId satisfies the asyncIdCarrier interface in helpers.go so the
// generic handleRequest wrapper forwards AsyncId to the wire-level
// HandlerResult when Status == StatusPending.
func (resp *CreateResponse) GetAsyncId() uint64 {
	return resp.AsyncId
}

// ============================================================================
// Path Normalization
// ============================================================================

// normalizeCreatePath converts backslashes to forward slashes and strips
// leading slashes from an SMB CREATE request filename. Per MS-SMB2 2.2.13,
// the Buffer field contains the path relative to the share root. Windows
// clients may send paths with leading backslashes (e.g., "\foo\bar" or
// "\\foo") which must be normalized to "foo/bar" and "foo" respectively.
func normalizeCreatePath(rawPath string) string {
	filename := strings.ReplaceAll(rawPath, "\\", "/")
	// Use TrimLeft to handle multiple leading slashes (e.g. "//foo" from "\\foo")
	return strings.TrimLeft(filename, "/")
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeCreateRequest parses an SMB2 CREATE request body [MS-SMB2] 2.2.13.
// It extracts the fixed header fields and the variable-length filename
// from the request body starting after the SMB2 header (64 bytes).
// Returns an error if the body is malformed or too short.
func DecodeCreateRequest(body []byte) (*CreateRequest, error) {
	if len(body) < 56 {
		return nil, fmt.Errorf("CREATE request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	r.Skip(2)                                                    // StructureSize (2)
	r.Skip(1)                                                    // SecurityFlags (1)
	oplockLevel := r.ReadUint8()                                 // OplockLevel (1)
	impersonationLevel := r.ReadUint32()                         // ImpersonationLevel (4)
	r.Skip(8)                                                    // SmbCreateFlags (8)
	r.Skip(8)                                                    // Reserved (8)
	desiredAccess := r.ReadUint32()                              // DesiredAccess (4)
	fileAttributes := types.FileAttributes(r.ReadUint32())       // FileAttributes (4)
	shareAccess := r.ReadUint32()                                // ShareAccess (4)
	createDisposition := types.CreateDisposition(r.ReadUint32()) // CreateDisposition (4)
	createOptions := types.CreateOptions(r.ReadUint32())         // CreateOptions (4)
	nameOffset := r.ReadUint16()                                 // NameOffset (2)
	nameLength := r.ReadUint16()                                 // NameLength (2)
	if r.Err() != nil {
		return nil, fmt.Errorf("CREATE request parse error: %w", r.Err())
	}

	req := &CreateRequest{
		OplockLevel:        oplockLevel,
		ImpersonationLevel: impersonationLevel,
		DesiredAccess:      desiredAccess,
		FileAttributes:     fileAttributes,
		ShareAccess:        shareAccess,
		CreateDisposition:  createDisposition,
		CreateOptions:      createOptions,
	}

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

	// ====================================================================
	// Parse Create Contexts [MS-SMB2] 2.2.13.2
	// ====================================================================
	//
	// CreateContextsOffset (4 bytes at offset 48) is relative to the start
	// of the SMB2 header. CreateContextsLength (4 bytes at offset 52) is
	// the total length of all chained create context structures.
	//
	// Windows 11 sends MxAc (Maximal Access), QFid (Query on Disk ID), and
	// RqLs (Request Lease) create contexts with every CREATE request. The
	// server MUST parse these to return the corresponding response contexts.
	// Without MxAc in the response, the Windows SMB redirector cannot
	// determine access rights and may refuse to open the file.

	if len(body) >= 56 {
		ctxR := smbenc.NewReader(body[48:56])
		ctxOffset := ctxR.ReadUint32()
		ctxLength := ctxR.ReadUint32()

		if ctxOffset > 0 && ctxLength > 0 {
			// ctxOffset is relative to the start of the SMB2 header (64 bytes)
			ctxBodyOffset := int(ctxOffset) - 64
			ctxEnd := ctxBodyOffset + int(ctxLength)

			if ctxBodyOffset >= 0 && ctxEnd <= len(body) {
				ctxs, ctxErr := decodeCreateContexts(body[ctxBodyOffset:ctxEnd])
				if ctxErr != nil {
					return nil, fmt.Errorf("invalid create context blob: %w", ctxErr)
				}
				req.CreateContexts = ctxs
			}
		}
	}

	return req, nil
}

// decodeCreateContexts parses a chain of SMB2 CREATE context structures
// [MS-SMB2] 2.2.13.2 from the given buffer.
//
// Each create context has the following wire format:
//
//	Offset  Size  Field
//	------  ----  -----------
//	0       4     Next            Offset to next context (0 if last)
//	4       2     NameOffset      Offset to Name from start of this context
//	6       2     NameLength      Length of Name in bytes
//	8       2     Reserved
//	10      2     DataOffset      Offset to Data from start of this context
//	12      4     DataLength      Length of Data in bytes
//	16      var   Buffer          Name (padded) + Data
func decodeCreateContexts(buf []byte) ([]CreateContext, error) {
	var contexts []CreateContext
	// Windows rejects duplicate create-context tags with STATUS_INVALID_PARAMETER
	// (smbtorture smb2.lease.duplicate_create / duplicate_open). MS-SMB2
	// §2.2.13.2 implies each tag is single-occurrence by design; the explicit
	// MUST is not stated in §3.3.5.9.
	var seen map[string]struct{}
	offset := 0

	for offset < len(buf) {
		remaining := len(buf) - offset

		// Need at least 16 bytes for the context header
		if remaining < 16 {
			return nil, fmt.Errorf("create context truncated: need 16 bytes, have %d", remaining)
		}

		ctx := buf[offset:]
		ctxR := smbenc.NewReader(ctx)
		next := ctxR.ReadUint32()    // Next
		nameOff := ctxR.ReadUint16() // NameOffset
		nameLen := ctxR.ReadUint16() // NameLength
		ctxR.Skip(2)                 // Reserved
		dataOff := ctxR.ReadUint16() // DataOffset
		dataLen := ctxR.ReadUint32() // DataLength

		// Validate offsets per MS-SMB2 2.2.13.2:
		// Tag name must be at least 4 bytes per MS-SMB2 2.2.13.2 (all defined
		// context names are 4+ chars). smbtorture smb2.create.blob tests this.
		if nameLen < 4 {
			return nil, fmt.Errorf("create context name too short: %d (must be >= 4)", nameLen)
		}
		// NameOffset must be at least 16 (past the fixed header) per MS-SMB2 2.2.13.2
		if nameOff < 16 {
			return nil, fmt.Errorf("create context nameOff too small: %d (must be >= 16)", nameOff)
		}
		// NameOffset must point within the context buffer (at least past header)
		if nameLen > 0 && int(nameOff)+int(nameLen) > remaining {
			return nil, fmt.Errorf("create context name overflows buffer: off=%d len=%d remaining=%d", nameOff, nameLen, remaining)
		}
		// DataOffset + DataLength must be within bounds
		if dataLen > 0 && int(dataOff)+int(dataLen) > remaining {
			return nil, fmt.Errorf("create context data overflows buffer: off=%d len=%d remaining=%d", dataOff, dataLen, remaining)
		}
		// Next must be 8-byte aligned and within buffer (or 0 for last)
		if next != 0 {
			if next%8 != 0 {
				return nil, fmt.Errorf("create context Next not 8-byte aligned: %d", next)
			}
			if int(next) > remaining {
				return nil, fmt.Errorf("create context Next overflows buffer: next=%d remaining=%d", next, remaining)
			}
		}

		// Limit parsing to the current context record to prevent reading
		// across boundaries into subsequent contexts [MS-SMB2 2.2.13.2]
		ctxLen := remaining
		if next > 0 && int(next) < ctxLen {
			ctxLen = int(next)
		}

		// Extract the context name (ASCII, e.g. "MxAc", "QFid", "RqLs")
		var name string
		if nameLen > 0 && int(nameOff)+int(nameLen) <= ctxLen {
			name = string(ctx[nameOff : nameOff+nameLen])
		}

		// Extract the context data (slice into existing buffer to avoid allocation)
		var data []byte
		if dataLen > 0 && int(dataOff)+int(dataLen) <= ctxLen {
			data = ctx[dataOff : int(dataOff)+int(dataLen)]
		}

		if name != "" {
			if _, dup := seen[name]; dup {
				return nil, fmt.Errorf("duplicate create context tag: %q", name)
			}
			if seen == nil {
				seen = make(map[string]struct{})
			}
			seen[name] = struct{}{}
			contexts = append(contexts, CreateContext{
				Name: name,
				Data: data,
			})
		}

		// Move to next context or stop
		if next == 0 {
			break
		}
		offset += int(next)
	}

	return contexts, nil
}

// Encode serializes the CreateResponse into SMB2 wire format [MS-SMB2] 2.2.14.
// The fixed header is 89 bytes. If CreateContexts are present, they are appended
// and the offset/length fields are set accordingly.
func (resp *CreateResponse) Encode() ([]byte, error) {
	// Encode create contexts if present
	ctxBuf, _, ctxLength := EncodeCreateContexts(resp.CreateContexts)

	// The CREATE response fixed fields are 88 bytes (StructureSize 89 includes
	// 1 byte of the variable buffer per MS-SMB2 convention). When create
	// contexts are present they follow immediately at offset 88 since 88 is
	// already 8-byte aligned. Without contexts we emit 89 bytes (88 fixed +
	// 1 zero byte for the variable-buffer placeholder).
	const fixedSize = 88 // actual wire bytes for fixed fields

	w := smbenc.NewWriter(fixedSize + len(ctxBuf))
	w.WriteUint16(89)                                        // StructureSize (always 89 per spec)
	w.WriteUint8(resp.OplockLevel)                           // OplockLevel
	w.WriteUint8(resp.Flags)                                 // Flags
	w.WriteUint32(uint32(resp.CreateAction))                 // CreateAction
	w.WriteUint64(types.TimeToFiletime(resp.CreationTime))   // CreationTime
	w.WriteUint64(types.TimeToFiletime(resp.LastAccessTime)) // LastAccessTime
	w.WriteUint64(types.TimeToFiletime(resp.LastWriteTime))  // LastWriteTime
	w.WriteUint64(types.TimeToFiletime(resp.ChangeTime))     // ChangeTime
	w.WriteUint64(resp.AllocationSize)                       // AllocationSize
	w.WriteUint64(resp.EndOfFile)                            // EndOfFile
	w.WriteUint32(uint32(resp.FileAttributes))               // FileAttributes
	w.WriteUint32(0)                                         // Reserved2
	w.WriteBytes(resp.FileID[:])                             // FileId (persistent + volatile)

	if len(ctxBuf) > 0 {
		// Contexts follow immediately at offset 64 (header) + 88 (fixed) = 152
		w.WriteUint32(uint32(64 + fixedSize)) // CreateContextsOffset
		w.WriteUint32(ctxLength)              // CreateContextsLength
	} else {
		w.WriteUint32(0) // CreateContextsOffset
		w.WriteUint32(0) // CreateContextsLength
	}

	buf := w.Bytes() // exactly 88 bytes

	if len(ctxBuf) > 0 {
		// Append context data directly (88 is already 8-byte aligned)
		buf = append(buf, ctxBuf...)
	} else {
		// No contexts: add 1 zero byte for the variable-buffer placeholder
		// so the body is 89 bytes matching StructureSize.
		buf = append(buf, 0)
	}

	return buf, nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Create handles SMB2 CREATE command [MS-SMB2] 2.2.13, 2.2.14.
//
// CREATE is the fundamental operation for accessing files and directories.
// It handles opening existing files, creating new ones, and replacing or
// superseding existing files based on the CreateDisposition. The response
// includes the file handle (FileID) and current file attributes.
//
// Oplock and lease requests are processed for regular files. Named pipe
// operations on IPC$ are delegated to handlePipeCreate.
func (h *Handler) Create(ctx *SMBHandlerContext, req *CreateRequest) (*CreateResponse, error) {
	logger.Debug("CREATE request",
		"filename", req.FileName,
		"disposition", req.CreateDisposition,
		"options", req.CreateOptions,
		"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess),
		"oplockLevel", fmt.Sprintf("0x%02x", req.OplockLevel),
		"createContexts", len(req.CreateContexts))

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
	filename := normalizeCreatePath(req.FileName)

	// Per MS-SMB2 2.2.13, the filename is relative to the share root and MUST NOT
	// start with a path separator. A leading backslash (e.g., "\foo") is invalid.
	// Return STATUS_INVALID_PARAMETER per smbtorture smb2.create.leading-slash.
	if strings.HasPrefix(req.FileName, "\\") || strings.HasPrefix(req.FileName, "/") {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	// Strip NTFS stream suffixes. The default data stream (::$DATA) and
	// directory index (::$INDEX_ALLOCATION) are implicit and should not be
	// part of the actual filename stored in the metadata store.
	if idx := strings.Index(filename, ":"); idx >= 0 {
		streamSuffix := filename[idx:]
		basePath := filename[:idx]
		upperSuffix := strings.ToUpper(streamSuffix)
		if upperSuffix == "::$DATA" || upperSuffix == "::$INDEX_ALLOCATION" {
			filename = basePath
		}
		// Other stream names (alternate data streams) are kept as-is
	}

	// Reject paths that traverse above the share root (e.g., "../../..")
	// Per MS-SMB2: return STATUS_OBJECT_PATH_SYNTAX_BAD for invalid path syntax.
	cleaned := path.Clean(filename)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectPathSyntaxBad}}, nil
	}

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

	// ========================================================================
	// Step 4b: Durable handle reconnect (DHnC/DH2C) [MS-SMB2] 3.3.5.9.7/12
	// ========================================================================
	//
	// If the CREATE request contains a reconnect context (V1 DHnC or V2 DH2C),
	// the client is attempting to reconnect a previously durable handle after a
	// network interruption. This path skips normal file creation and restores
	// the persisted handle state instead.

	if h.DurableStore != nil {
		hasDHnC := FindCreateContext(req.CreateContexts, DurableHandleV1ReconnectTag) != nil
		hasDH2C := FindCreateContext(req.CreateContexts, DurableHandleV2ReconnectTag) != nil

		if hasDHnC || hasDH2C {
			// Compute session key hash for security validation
			sessionKeyHash := computeSessionKeyHash(sess)

			metaSvc := h.Registry.GetMetadataService()
			reconnResult, status, reconnErr := ProcessDurableReconnectContext(
				authCtx.Context, h.DurableStore, metaSvc, req.CreateContexts,
				ctx.SessionID, sess.Username, sessionKeyHash,
				tree.ShareName, filename,
			)
			if reconnErr != nil {
				logger.Warn("CREATE: durable reconnect error", "error", reconnErr)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
			}
			if status != types.StatusSuccess {
				logger.Debug("CREATE: durable reconnect failed",
					"status", status, "filename", filename)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: status}}, nil
			}

			restored := reconnResult.OpenFile

			// Reconnect succeeded: register the restored OpenFile.
			// Per MS-SMB2 3.3.5.9.7: keep the persistent FileId (bytes 0-7)
			// but regenerate the volatile FileId (bytes 8-15) so it is unique
			// on this connection.  The old volatile part is stale after the
			// server restart / disconnect.
			restored.TreeID = ctx.TreeID
			restored.SessionID = ctx.SessionID
			smbFileID := restored.FileID
			// Regenerate volatile part (bytes 8-15)
			_, _ = rand.Read(smbFileID[8:16])
			restored.FileID = smbFileID

			// Re-grant durability on reconnect. The handle was durable before
			// disconnect, and the successful reconnect implicitly restores it.
			restored.IsDurable = true
			restored.DurableTimeoutMs = h.DurableTimeoutMs

			// Build response create contexts: DH2Q/DHnQ response + lease response
			var reconnectContexts []CreateContext

			// Per MS-SMB2 3.3.5.9.12/7: return DH2Q or DHnQ response on reconnect
			if reconnResult.IsV2 {
				resp := EncodeDH2QResponse(h.DurableTimeoutMs, 0)
				reconnectContexts = append(reconnectContexts, resp)
			} else {
				resp := EncodeDHnQResponse()
				reconnectContexts = append(reconnectContexts, resp)
			}

			// Re-register lease/oplock in the LeaseManager. During disconnect,
			// ReleaseSessionLeases cleared the lease state from the LeaseManager.
			// We must re-request it so that break notifications work and the
			// oplock/lease is visible to other opens.
			if h.LeaseManager != nil && len(restored.MetadataHandle) > 0 {
				lockFileHandle := lock.FileHandle(restored.MetadataHandle)

				if restored.OplockLevel == OplockLevelLease && restored.LeaseKey != ([16]byte{}) {
					// Use persisted lease state for re-request. This preserves the
					// exact lease state the client had before disconnect rather than
					// always requesting full RWH which may conflict or be rejected.
					requestedState := reconnResult.PersistedLease
					if requestedState == 0 {
						// Fallback to RWH if lease state was not persisted
						requestedState = uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle)
					}

					ownerID := fmt.Sprintf("smb:lease:%x", restored.LeaseKey)
					clientID := fmt.Sprintf("smb:%d", ctx.SessionID)
					grantedState, epoch, leaseErr := h.LeaseManager.RequestLease(
						authCtx.Context,
						lockFileHandle,
						restored.LeaseKey,
						[16]byte{}, // No parent lease key on reconnect
						ctx.SessionID,
						ownerID,
						clientID,
						tree.ShareName,
						requestedState,
						restored.IsDirectory,
					)
					if leaseErr != nil {
						logger.Debug("CREATE: durable reconnect lease re-request failed", "error", leaseErr)
					} else {
						// Build lease response context
						if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
							isV1 := len(leaseCtx.Data) < LeaseV2ContextSize
							leaseResp := &LeaseResponseContext{
								LeaseKey:   restored.LeaseKey,
								LeaseState: grantedState,
								Epoch:      epoch,
								IsV1:       isV1,
							}
							reconnectContexts = append(reconnectContexts, CreateContext{
								Name: LeaseContextTagResponse,
								Data: leaseResp.Encode(),
							})
						}
						// Re-mark V2 regardless of whether the client repeated
						// RqLs on this reconnect. Lease-backed durable handles
						// require SMB 3.0+ (MS-SMB2 §3.3.5.9.7 / §3.3.5.9.12),
						// and SMB 3.x leases are always V2-context. Without
						// this, an omitted-RqLs reconnect would leave the
						// lease untracked and subsequent breaks would send
						// NewEpoch = 0 even though the lease is V2 (#417).
						if grantedState != lock.LeaseStateNone {
							h.LeaseManager.MarkLeaseV2(restored.LeaseKey)
							restored.OplockLevel = OplockLevelLease
						}
					}

					// Update session mapping for break notification routing
					h.LeaseManager.UpdateSessionForLease(restored.LeaseKey, ctx.SessionID)
				} else if restored.OplockLevel != OplockLevelNone && restored.OplockLevel != OplockLevelLease {
					// Traditional oplock: re-request via synthetic lease key
					var requestedState uint32
					switch restored.OplockLevel {
					case OplockLevelBatch:
						requestedState = lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
					case OplockLevelExclusive:
						requestedState = lock.LeaseStateRead | lock.LeaseStateWrite
					case OplockLevelII:
						requestedState = lock.LeaseStateRead
					}

					if requestedState != 0 {
						syntheticKey := generateSyntheticLeaseKey(smbFileID)
						ownerID := fmt.Sprintf("smb:oplock:%x", smbFileID)
						clientID := fmt.Sprintf("smb:%d", ctx.SessionID)
						grantedState, _, leaseErr := h.LeaseManager.RequestLease(
							authCtx.Context,
							lockFileHandle,
							syntheticKey,
							[16]byte{},
							ctx.SessionID,
							ownerID,
							clientID,
							tree.ShareName,
							requestedState,
							false,
						)
						if leaseErr != nil {
							logger.Debug("CREATE: durable reconnect oplock re-request failed", "error", leaseErr)
						} else {
							restored.OplockLevel = leaseStateToOplockLevel(grantedState)
							restored.LeaseKey = syntheticKey
							if restored.OplockLevel != OplockLevelNone {
								h.LeaseManager.RegisterOplockFileID(syntheticKey, smbFileID)
							}
						}
					}
				}
			}

			// Per MS-SMB2 3.3.5.9.7/12: A successful reconnect implicitly
			// re-grants durability. Do NOT process DHnQ/DH2Q create contexts
			// during reconnect -- the handle is already durable.

			h.StoreOpenFile(restored)

			// Get current file attributes for the response
			var respFile *metadata.File
			if len(restored.MetadataHandle) > 0 {
				respFile, _ = metaSvc.GetFile(authCtx.Context, restored.MetadataHandle)
			}

			logger.Debug("CREATE: durable reconnect successful",
				"fileID", fmt.Sprintf("%x", smbFileID),
				"filename", filename,
				"oplock", oplockLevelName(restored.OplockLevel))

			if respFile != nil {
				creation, access, write, change := FileAttrToSMBTimes(&respFile.FileAttr)
				size := getSMBSize(&respFile.FileAttr)
				return &CreateResponse{
					SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
					OplockLevel:     restored.OplockLevel,
					CreateAction:    types.FileOpened,
					CreationTime:    creation,
					LastAccessTime:  access,
					LastWriteTime:   write,
					ChangeTime:      change,
					AllocationSize:  calculateAllocationSize(size),
					EndOfFile:       size,
					FileAttributes:  FileAttrToSMBAttributes(&respFile.FileAttr),
					FileID:          smbFileID,
					CreateContexts:  reconnectContexts,
				}, nil
			}

			// Fallback if we can't get file attributes (shouldn't happen)
			now := time.Now()
			return &CreateResponse{
				SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
				OplockLevel:     restored.OplockLevel,
				CreateAction:    types.FileOpened,
				CreationTime:    now,
				LastAccessTime:  now,
				LastWriteTime:   now,
				ChangeTime:      now,
				FileAttributes:  types.FileAttributeNormal,
				FileID:          smbFileID,
				CreateContexts:  reconnectContexts,
			}, nil
		}
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

	// Extract the opener's lease key from RqLs context (if present).
	// Used to prevent breaking our own lease on same-key opens (nobreakself).
	var openerLeaseKey [16]byte
	if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
		if lcc, lccErr := DecodeLeaseCreateContext(leaseCtx.Data); lccErr == nil {
			openerLeaseKey = lcc.LeaseKey
		}
	}

	// Build excludeOwner once for lease break calls below.
	// Prevents breaking our own lease on same-key opens (MS-SMB2 nobreakself).
	var excludeOwner *lock.LockOwner
	if openerLeaseKey != ([16]byte{}) {
		excludeOwner = &lock.LockOwner{ExcludeLeaseKey: openerLeaseKey}
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

	// ========================================================================
	// Step 6: Handle create disposition
	// ========================================================================
	//
	// Per MS-FSA 2.1.5.1.1: Disposition check (e.g., FILE_CREATE failing with
	// OBJECT_NAME_COLLISION when the name exists) takes priority over type
	// constraint checks (NOT_A_DIRECTORY / FILE_IS_A_DIRECTORY).

	createAction, dispErr := ResolveCreateDisposition(req.CreateDisposition, fileExists)
	if dispErr != nil {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(dispErr)}}, nil
	}

	// Validate directory vs file constraints for existing files.
	// Only applies when the disposition opens or overwrites (not FILE_CREATE,
	// which already failed above if the file existed).
	if fileExists {
		if isDirectoryRequest && existingFile.Type != metadata.FileTypeDirectory {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotADirectory}}, nil
		}
		if isNonDirectoryRequest && existingFile.Type == metadata.FileTypeDirectory {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileIsADirectory}}, nil
		}
	}

	// Per MS-SMB2 3.3.5.9: Overwrite/supersede operations are not valid for
	// directories. If the file exists and is a directory, only open is allowed.
	if fileExists && existingFile.Type == metadata.FileTypeDirectory {
		if createAction == types.FileOverwritten || createAction == types.FileSuperseded {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
		}
	}

	// Per MS-FSA 2.1.5.1.2.1: Overwrite/supersede of a read-only file is
	// not allowed. Return STATUS_ACCESS_DENIED.
	if fileExists && existingFile.Type != metadata.FileTypeDirectory {
		if createAction == types.FileOverwritten || createAction == types.FileSuperseded {
			attrs := FileAttrToSMBAttributes(&existingFile.FileAttr)
			if attrs&types.FileAttributeReadonly != 0 {
				logger.Debug("CREATE: cannot overwrite read-only file",
					"path", filename,
					"action", createAction)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
			}
		}
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

	// Step 6c: Break Handle leases and check share-mode conflicts.
	//
	// Per MS-SMB2 3.3.5.9 Step 10 and MS-FSA 2.1.5.1.2: when opening an
	// existing file with a conflicting Handle lease, the server MUST break
	// that lease BEFORE the final share-mode check so holders can release
	// cached handles. The post-break flow (recheck + create + response) runs
	// in completeCreateAfterBreak so the same code path serves both the sync
	// wait and the async park-on-break goroutine (MS-SMB2 §3.3.4.4/§3.3.4.7).

	// Cross-stream share-mode check for new files (no lease break needed).
	if !fileExists {
		if shareConflict := h.checkShareModeConflict(nil, req.DesiredAccess, req.ShareAccess, filename); shareConflict {
			logger.Debug("CREATE: cross-stream sharing violation",
				"path", filename,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess),
				"shareAccess", fmt.Sprintf("0x%x", req.ShareAccess))
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSharingViolation}}, nil
		}
	}

	draft := (&createDraft{
		req:                req,
		tree:               tree,
		authCtx:            authCtx,
		filename:           filename,
		baseName:           baseName,
		parentHandle:       parentHandle,
		existingFile:       existingFile,
		fileExists:         fileExists,
		createAction:       createAction,
		isDirectoryRequest: isDirectoryRequest,
		excludeOwner:       excludeOwner,
	}).finalize()

	// Dispatch lease break and either park the CREATE async (emit interim
	// STATUS_PENDING) or wait for the break to drain inline.
	if asyncId := h.breakAndMaybeParkCreate(ctx, draft); asyncId != 0 {
		return &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusPending},
			AsyncId:         asyncId,
		}, nil
	}

	return h.completeCreateAfterBreak(ctx, draft), nil
}

// computeMaximalAccess computes the maximal access mask for a file based on
// POSIX permissions and the requesting user's identity.
//
// For the file owner, GENERIC_ALL (0x001F01FF) is granted.
// For other users, access is computed from the file's mode bits:
//   - Read permission:    0x00120089 (FILE_READ_DATA | FILE_READ_EA | FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE)
//   - Write permission:   0x00120116 (FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_WRITE_EA | FILE_WRITE_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE)
//   - Execute permission: 0x001200A0 (FILE_EXECUTE | FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE)
func computeMaximalAccess(file *metadata.File, authCtx *metadata.AuthContext) uint32 {
	const (
		genericAll    uint32 = 0x001F01FF
		readAccess    uint32 = 0x00120089
		writeAccess   uint32 = 0x00120116
		executeAccess uint32 = 0x001200A0
	)

	// Check if the requesting user is the file owner
	if authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == file.UID {
		return genericAll
	}

	// Compute from POSIX mode bits for non-owner
	// Use "other" permission bits (lowest 3 bits of mode)
	mode := file.Mode
	var access uint32

	// Check group membership for group permissions
	isGroupMember := authCtx.Identity != nil && authCtx.Identity.GID != nil && *authCtx.Identity.GID == file.GID
	if !isGroupMember && authCtx.Identity != nil {
		for _, gid := range authCtx.Identity.GIDs {
			if gid == file.GID {
				isGroupMember = true
				break
			}
		}
	}

	var permBits uint32
	if isGroupMember {
		// Use group permission bits (bits 3-5)
		permBits = uint32((mode >> 3) & 0x7)
	} else {
		// Use other permission bits (bits 0-2)
		permBits = uint32(mode & 0x7)
	}

	if permBits&0x4 != 0 { // read
		access |= readAccess
	}
	if permBits&0x2 != 0 { // write
		access |= writeAccess
	}
	if permBits&0x1 != 0 { // execute
		access |= executeAccess
	}

	// Ensure at minimum READ_CONTROL | SYNCHRONIZE for any authenticated user
	if access == 0 {
		access = 0x00100000 | 0x00020000 // SYNCHRONIZE | READ_CONTROL
	}

	return access
}

// ============================================================================
// Named Pipe Handling (IPC$)
// ============================================================================

// handlePipeCreate handles CREATE on IPC$ for named pipes.
// Named pipes are used for DCE/RPC communication, e.g., srvsvc for share enumeration.
func (h *Handler) handlePipeCreate(ctx *SMBHandlerContext, req *CreateRequest, tree *TreeConnection) (*CreateResponse, error) {
	// Normalize pipe name (remove leading/backslashes and "pipe\" prefix)
	pipeName := normalizeCreatePath(req.FileName)
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

	// Update pipe manager with cached share list.
	// Cache is invalidated via Runtime.OnShareChange() callback.
	if shares := h.getCachedShares(); shares != nil {
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
			// Navigate to parent directory using Lookup which handles ".." natively
			parentFile, err := metaSvc.Lookup(authCtx, currentHandle, "..")
			if err != nil {
				return nil, fmt.Errorf("walkPath: lookup parent '..': %w", err)
			}
			currentHandle, err = metadata.EncodeFileHandle(parentFile)
			if err != nil {
				return nil, fmt.Errorf("encode parent handle: %w", err)
			}
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

	// Inherit compression state from parent directory.
	// Per MS-FSA 2.1.5.1.1: if the parent directory has FILE_ATTRIBUTE_COMPRESSED,
	// the new file/directory inherits the compression attribute.
	// Per MS-SMB2 2.2.13: FILE_NO_COMPRESSION in CreateOptions suppresses inheritance.
	metaSvc := h.Registry.GetMetadataService()
	if req.CreateOptions&types.FileNoCompression == 0 {
		parentFile, pErr := metaSvc.GetFile(authCtx.Context, parentHandle)
		if pErr == nil && parentFile.Mode&modeDOSCompressed != 0 {
			fileAttr.Mode |= modeDOSCompressed
		}
	}

	// Create appropriate file type based on fileAttr.Type
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

	// Truncate to zero size and apply requested attributes
	zeroSize := uint64(0)
	setAttrs := &metadata.SetAttrs{
		Size: &zeroSize,
	}

	// Per MS-FSA 2.1.5.1.1: OVERWRITE/SUPERSEDE should apply FileAttributes
	// from the request. Preserve modeDOSCompressed from existing metadata since
	// compression state is controlled only via FSCTL_SET_COMPRESSION.
	if req.FileAttributes != 0 {
		mode := SMBModeFromAttrs(req.FileAttributes, existingFile.Type == metadata.FileTypeDirectory)
		mode |= existingFile.Mode & modeDOSCompressed
		setAttrs.Mode = &mode
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

// updateBaseObjectCtime updates the ChangeTime of the base file or directory
// that hosts an ADS. Per MS-FSA / NTFS semantics, creating or modifying an
// alternate data stream propagates a ChangeTime update to the base object.
func (h *Handler) updateBaseObjectCtime(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.MetadataService,
	parentHandle metadata.FileHandle,
	baseObjectName string,
) {
	baseFile, err := metaSvc.Lookup(authCtx, parentHandle, baseObjectName)
	if err != nil {
		return
	}
	baseHandle, err := metadata.EncodeFileHandle(baseFile)
	if err != nil {
		return
	}
	now := time.Now()
	if updateErr := metaSvc.SetFileAttributes(authCtx, baseHandle, &metadata.SetAttrs{Ctime: &now}); updateErr != nil {
		logger.Debug("updateBaseObjectCtime: failed",
			"baseObject", baseObjectName, "error", updateErr)
	}
}

// updateBaseObjectTimestampsForADSWrite updates the ChangeTime and LastWriteTime
// of the base file or directory that hosts an ADS after a WRITE to the stream.
// Per MS-FSA / NTFS semantics, data writes to an alternate data stream propagate
// Mtime and Ctime changes to the base object, unless the corresponding timestamp
// is frozen on the ADS handle.
func (h *Handler) updateBaseObjectTimestampsForADSWrite(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.MetadataService,
	openFile *OpenFile,
	baseObjectName string,
) {
	baseFile, err := metaSvc.Lookup(authCtx, openFile.ParentHandle, baseObjectName)
	if err != nil {
		return
	}
	baseHandle, err := metadata.EncodeFileHandle(baseFile)
	if err != nil {
		return
	}
	now := time.Now()
	setAttrs := &metadata.SetAttrs{}
	if !openFile.CtimeFrozen {
		setAttrs.Ctime = &now
	}
	if !openFile.MtimeFrozen {
		setAttrs.Mtime = &now
	}
	if setAttrs.Ctime != nil || setAttrs.Mtime != nil {
		_ = metaSvc.SetFileAttributes(authCtx, baseHandle, setAttrs)
	}
}

// isStatOnlyOpen returns true when DesiredAccess contains only
// FILE_READ_ATTRIBUTES, optionally combined with SYNCHRONIZE and/or READ_CONTROL.
// Per MS-SMB2 3.3.5.9.8, stat-only opens must NOT break existing leases.
func isStatOnlyOpen(desiredAccess uint32) bool {
	const (
		fileReadAttributes = 0x00000080
		readControl        = 0x00020000
		synchronize        = 0x00100000
	)
	// Strip allowed non-conflicting bits, check nothing else is requested.
	masked := desiredAccess &^ (fileReadAttributes | readControl | synchronize)
	return masked == 0 && desiredAccess&fileReadAttributes != 0
}

// computeSessionKeyHash computes the SHA-256 hash of the session's signing key.
// This is used for durable handle security validation during reconnect.
// Returns zero hash if the session has no crypto state or signing key.
func computeSessionKeyHash(sess *session.Session) [32]byte {
	if sess == nil || sess.CryptoState == nil || len(sess.CryptoState.SigningKey) == 0 {
		return [32]byte{}
	}
	return sha256.Sum256(sess.CryptoState.SigningKey)
}
