package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

const CreateContextTagAllocationSize = "AlSi"

// CreateContextTagExtendedAttributes is the SMB2_CREATE_EA_BUFFER create
// context tag ("ExtA") [MS-SMB2] 2.2.13.2.1. Its Data is a
// FILE_FULL_EA_INFORMATION chain (MS-FSCC §2.4.16 ("FileFullEaInformation")) of extended attributes the
// client wants attached to the file at creation time.

const CreateContextTagExtendedAttributes = "ExtA"

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

	// GenericDerivedAccess holds the file-specific bits that DesiredAccess
	// gained purely from GENERIC_* expansion at decode time (see
	// acl.GenericDerivedBits). Under SEC_FLAG_MAXIMUM_ALLOWED these bits are
	// best-effort and excluded from the strict explicit-bit enforcement gate,
	// matching Samba (smb2.maximum_allowed vs smb2.acls.MXAC-NOT-GRANTED).
	GenericDerivedAccess uint32

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

	// RequestedAllocSize is the client-requested initial allocation size in
	// bytes, decoded from the SMB2_CREATE_ALLOCATION_SIZE ("AlSi") create
	// context [MS-SMB2] 2.2.13.2.2. SMB2 CREATE has no fixed AllocationSize
	// request field — the 8-byte field at request offset 16 is Reserved — so
	// the client conveys the reservation via this context. DittoFS does not
	// preallocate; the value only raises the (cluster-aligned) AllocationSize
	// reported in the CREATE response (smb2.durable-open.alloc-size).
	RequestedAllocSize uint64

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
	r.Skip(2)                            // StructureSize (2)
	r.Skip(1)                            // SecurityFlags (1)
	oplockLevel := r.ReadUint8()         // OplockLevel (1)
	impersonationLevel := r.ReadUint32() // ImpersonationLevel (4)
	r.Skip(8)                            // SmbCreateFlags (8)
	r.Skip(8)                            // Reserved (8)
	desiredAccess := r.ReadUint32()      // DesiredAccess (4)
	// Per MS-DTYP §2.4.3 / §2.5.3, GENERIC_* bits in DesiredAccess MUST be
	// expanded to file-object-specific rights before access-check
	// evaluation. Do it at decode time so every downstream consumer
	// (permission checks, share-mode conflict, MxAc, OpenFile bookkeeping)
	// observes resolved bits.
	//
	// Capture the bits introduced by GENERIC_* expansion BEFORE stripping the
	// generic flags, so the MAXIMUM_ALLOWED access-check can treat them as
	// best-effort (excluded from strict explicit-bit enforcement) per Samba.
	genericDerivedAccess := acl.GenericDerivedBits(desiredAccess)
	desiredAccess = acl.ExpandGenericMask(desiredAccess)
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
		OplockLevel:          oplockLevel,
		ImpersonationLevel:   impersonationLevel,
		DesiredAccess:        desiredAccess,
		GenericDerivedAccess: genericDerivedAccess,
		FileAttributes:       fileAttributes,
		ShareAccess:          shareAccess,
		CreateDisposition:    createDisposition,
		CreateOptions:        createOptions,
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

	// SMB2_CREATE_ALLOCATION_SIZE ("AlSi") create context [MS-SMB2] 2.2.13.2.2:
	// the client conveys its requested initial allocation as an 8-byte
	// little-endian value in the context Data. SMB2 has no fixed AllocationSize
	// request field (request offset 16 is Reserved), so this is the only place
	// the reservation is carried. Required by smb2.durable-open.alloc-size.
	if alsi := FindCreateContext(req.CreateContexts, CreateContextTagAllocationSize); alsi != nil && len(alsi.Data) >= 8 {
		req.RequestedAllocSize = smbenc.NewReader(alsi.Data).ReadUint64()
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
	// MUST is not stated in §3.3.5.9. Track the first tag in firstSeen and
	// only allocate the map on the second context to keep the common 0/1-tag
	// path allocation-free.
	var firstSeen string
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
			switch {
			case firstSeen == "":
				firstSeen = name
			case seen == nil:
				if name == firstSeen {
					return nil, fmt.Errorf("duplicate create context tag: %q", name)
				}
				seen = map[string]struct{}{firstSeen: {}, name: {}}
			default:
				if _, dup := seen[name]; dup {
					return nil, fmt.Errorf("duplicate create context tag: %q", name)
				}
				seen[name] = struct{}{}
			}
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
	w.WriteUint16(89)                                        // StructureSize (always 89)
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
		"fileAttributes", fmt.Sprintf("0x%x", req.FileAttributes),
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
	h.primeAuthContext(ctx, ctx.TreeID, ctx.SessionID)

	// Validate durable-handle create context combinations per MS-SMB2
	// §3.3.5.9.6/7/11/12. Mutually-exclusive pairs of DHnQ/DHnC/DH2Q/DH2C
	// MUST be rejected with STATUS_INVALID_PARAMETER (smbtorture
	// smb2.durable-v2-open.create-blob).
	if status := ValidateDurableContexts(req.CreateContexts); status != types.StatusSuccess {
		logger.Debug("CREATE: invalid durable context combination", "status", status)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: status}}, nil
	}

	// SMB3 DH2Q replay protection (MS-SMB2 §3.3.5.9; §2.2.1.2
	// REPLAY_OPERATION). A CREATE whose DH2Q CreateGuid matches a live
	// open is resolved here — replayed (with the original FileId and the
	// open's current lease/oplock state), rejected as a duplicate, or
	// failed fast while the original is still parked. A miss falls through
	// to the normal CREATE path, which records the response on success.
	// See resolveCreateReplay for the full per-case contract.
	if resp, handled := h.resolveCreateReplay(ctx, req); handled {
		return resp, nil
	}

	// TWrp (SMB2_CREATE_TIMEWARP_TOKEN, MS-SMB2 §2.2.13.2.7) requests a
	// previous-version (snapshot) view of the share. DittoFS has no VSS-style
	// snapshot backend wired into CREATE, so any non-zero snapshot timestamp
	// MUST return STATUS_OBJECT_NAME_NOT_FOUND — the requested snapshot does
	// not exist. Required by smbtorture smb2.create.blob ("Testing timewarp").
	if twrp := FindCreateContext(req.CreateContexts, "TWrp"); twrp != nil {
		logger.Debug("CREATE: TWrp (previous version) requested — no snapshot backend",
			"filename", req.FileName,
			"dataLen", len(twrp.Data))
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameNotFound}}, nil
	}

	// MS-SMB2 §3.3.5.9.7/12: a DHnC/DH2C reconnect is keyed solely by the
	// reconnect blob (FileId / CreateGuid). The server ignores the wire-format
	// CREATE fields validated below — ImpersonationLevel, CreateOptions reserved
	// bits, FileAttributes — AND ignores DesiredAccess / ShareAccess: the
	// reconnect path restores the ORIGINAL granted access from the persisted
	// handle and never re-validates the requested access (confirmed against
	// Samba source3/smbd/smb2_create.c — no desired/share-access compare on
	// reconnect), so no privilege escalation is possible. smbtorture
	// smb2.durable-open.reopen2 reconnects with every such field set to garbage
	// (0x12345678 / 0x78) to prove the wire gates are skipped; running them
	// against that garbage would wrongly return STATUS_INVALID_PARAMETER /
	// STATUS_ACCESS_DENIED / STATUS_BAD_IMPERSONATION_LEVEL / STATUS_NOT_SUPPORTED
	// before the reconnect path (Step 4b) is reached.
	//
	// Gated on a configured DurableStore so the bypass only applies on the path
	// that reaches the reconnect handler, and excluded on IPC$ — durable handles
	// are disk-share only, so a stray reconnect blob on a named pipe must still
	// run full wire validation rather than fall into handlePipeCreate unchecked.
	isDurableReconnect := h.DurableStore != nil &&
		tree.ShareName != "/ipc$" &&
		(FindCreateContext(req.CreateContexts, DurableHandleV1ReconnectTag) != nil ||
			FindCreateContext(req.CreateContexts, DurableHandleV2ReconnectTag) != nil)

	// Validate ImpersonationLevel per MS-SMB2 §2.2.13: only the four defined
	// values are accepted (0=Anonymous, 1=Identification, 2=Impersonation,
	// 3=Delegate). Anything else MUST be rejected with
	// STATUS_BAD_IMPERSONATION_LEVEL. Required by smbtorture
	// smb2.create.impersonation.
	if !isDurableReconnect && req.ImpersonationLevel > 3 {
		logger.Debug("CREATE: invalid impersonation level",
			"level", fmt.Sprintf("0x%08x", req.ImpersonationLevel))
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusBadImpersonationLevel}}, nil
	}

	// Validate reserved bits in CreateOptions per MS-SMB2 §2.2.13. The upper
	// byte (bits 24-31) is reserved and MUST be zero; any non-zero reserved
	// bit returns STATUS_INVALID_PARAMETER. Samba sets `invalid_parameter_mask
	// = 0xff000000` in source3/smbd/smb2_create.c for these probes (smbtorture
	// smb2.create.gentest).
	if !isDurableReconnect && uint32(req.CreateOptions)&0xff000000 != 0 {
		logger.Debug("CREATE: reserved CreateOptions bits set",
			"options", fmt.Sprintf("0x%08x", uint32(req.CreateOptions)))
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	// Per MS-SMB2 §2.2.13 ("SMB2 CREATE Request"), which is where CreateOptions
	// is defined, three CreateOptions bits are defined
	// in Samba but unimplemented in DittoFS and MUST return STATUS_NOT_SUPPORTED
	// rather than silently succeed. Samba sets `not_supported_mask = 0x00102080`
	// for these probes (smbtorture smb2.create.gentest):
	//   bit 7  (0x00000080) NTCREATEX_OPTIONS_TREE_CONNECTION — reserved on the
	//                       wire per MS-SMB2; Samba legacy name retained.
	//   bit 13 (0x00002000) FILE_OPEN_BY_FILE_ID — open by FileId reference
	//                       (matches types.FileOpenByFileId).
	//   bit 20 (0x00100000) NTCREATEX_OPTIONS_OPFILTER — reserved on the wire
	//                       per MS-SMB2; Samba legacy name retained.
	// Note: FILE_COMPLETE_IF_OPLOCKED (0x00000100, types.FileCompleteIfOplocked)
	// is NOT in this mask — Samba's `ok_mask` accepts it.
	const unsupportedCreateOptionsMask uint32 = 0x00000080 | // bit 7  (TREE_CONNECTION)
		uint32(types.FileOpenByFileId) | // bit 13 (0x00002000)
		0x00100000 // bit 20 (OPFILTER)
	if !isDurableReconnect && uint32(req.CreateOptions)&unsupportedCreateOptionsMask != 0 {
		logger.Debug("CREATE: unsupported CreateOptions bits set",
			"options", fmt.Sprintf("0x%08x", uint32(req.CreateOptions)))
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil
	}

	// Validate FileAttributes per MS-FSCC §2.6 and MS-SMB2 §2.2.13. Only the
	// attributes defined for files/directories are accepted on CREATE; the
	// reserved/volume/device bits MUST be rejected with STATUS_INVALID_PARAMETER.
	//
	// Legal attributes on CREATE (MS-FSCC §2.6):
	//   READONLY (0x1)         HIDDEN (0x2)          SYSTEM (0x4)
	//   DIRECTORY (0x10)       ARCHIVE (0x20)        NORMAL (0x80)
	//   TEMPORARY (0x100)      SPARSE_FILE (0x200)   REPARSE_POINT (0x400)
	//   COMPRESSED (0x800)     OFFLINE (0x1000)      NOT_CONTENT_INDEXED (0x2000)
	//   ENCRYPTED (0x4000)     INTEGRITY_STREAM (0x8000)
	//
	// Bit 0x8 (FILE_ATTRIBUTE_VOLUME) and 0x40 (FILE_ATTRIBUTE_DEVICE) are
	// reserved-on-the-wire and rejected. INTEGRITY_STREAM (0x8000) is silently
	// accepted (no-op on DittoFS) — WPTS FSA tests probe basic-info on a file
	// created with that attribute and require CREATE to succeed.
	const validFileAttributesMask uint32 = 0x0000FFB7
	if !isDurableReconnect && uint32(req.FileAttributes)&^validFileAttributesMask != 0 {
		logger.Debug("CREATE: invalid FileAttributes bits set",
			"attrs", fmt.Sprintf("0x%08x", uint32(req.FileAttributes)))
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	// ========================================================================
	// Step 2: Check for IPC$ named pipe operations
	// ========================================================================

	if tree.ShareName == "/ipc$" {
		resp, err := h.handlePipeCreate(ctx, req, tree)
		h.storeCreateReplayIfApplicable(ctx, req, resp)
		return resp, err
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

	// Normalize NTFS stream syntax. Stream names may contain characters
	// like "/" which path.Dir/path.Base would misinterpret as path
	// separators, so extract the stream portion BEFORE any path operations.
	//
	// After normalization, named ADS are stored as "file.txt:StreamName"
	// (no type suffix). FileStreamInformation re-appends ":$DATA" on wire.
	var streamSuffix string // e.g., ":StreamName" — empty for non-ADS
	var explicitDataStream bool
	if idx := strings.Index(filename, ":"); idx >= 0 {
		colonPart := filename[idx:]
		basePart := filename[:idx]
		upper := strings.ToUpper(colonPart)
		switch upper {
		case "::$DATA":
			explicitDataStream = true
			filename = basePart
		case "::$INDEX_ALLOCATION", ":$I30:$INDEX_ALLOCATION":
			// Both forms refer to the directory's default index stream.
			// ::$INDEX_ALLOCATION is the unnamed variant; :$I30:$INDEX_ALLOCATION
			// names the $I30 index explicitly (NTFS directory listing index).
			// Strip the suffix so the open resolves to the directory itself.
			filename = basePart
		default:
			// Named ADS: strip trailing :$DATA type indicator, isolate
			// stream suffix so it doesn't pollute path.Dir/path.Base.
			if strings.HasSuffix(upper, ":$DATA") {
				colonPart = colonPart[:len(colonPart)-len(":$DATA")]
			}
			// `file:$DATA` (empty stream name, $DATA type with single
			// colon) resolves to a stream entity distinct from the
			// base file. Per Samba/WPTS semantics, opens on this
			// path are independent of the base file's DOC and share
			// state. Preserve the literal suffix so the metadata
			// lookup resolves to a separate child entry rather than
			// collapsing onto the base file (which would inherit
			// the base's pending-delete state and reject the open).
			if colonPart == "" {
				streamSuffix = ":$DATA"
			} else {
				streamSuffix = colonPart
			}
			filename = basePart
		}
	}

	// A stream without a base file (":streamname") is invalid.
	if streamSuffix != "" && filename == "" {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameInvalid}}, nil
	}

	// Per smb2.create_no_streams.no_stream (source4/torture/smb2/create.c):
	// shares with StreamsDisabled reject CREATEs that name a data stream.
	// The upstream extractor surfaces every stream syntax through one of
	// these two predicates:
	//
	//   - streamSuffix != ""    — the default arm of the switch above. Set
	//                             for named ADS ("file:stream"), named-ADS
	//                             with type suffix ("file:stream:$DATA"),
	//                             arbitrary stream-type suffix ("file::foo"
	//                             or "file:foo:bar"), and the literal
	//                             "file:$DATA" stream entity.
	//   - explicitDataStream    — set only for the bare default data
	//                             stream ("file::$DATA"); the extractor
	//                             strips the suffix into filename so
	//                             streamSuffix is empty.
	//
	// Directory-index syntaxes (`::$INDEX_ALLOCATION`, `:$I30:$INDEX_ALLOCATION`)
	// are intentionally normalized away by the extractor and resolve to the
	// directory itself, so they are not stream references and remain
	// allowed. Mirrors Samba `smbd:streams = no`.
	if tree.StreamsDisabled && (streamSuffix != "" || explicitDataStream) {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameInvalid}}, nil
	}

	// Wildcard characters are invalid in path components: MS-FSA 2.1.5.1 Phase 1
	// defers name validity to MS-FSCC 2.1.5 ("Pathname"), and MS-FSA 2.1.4.3
	// ("Algorithm for Determining If a Character Is a Wildcard") names the five
	// characters tested here. Note: wildcards ARE valid inside stream names (e.g.,
	// "file:?Stream*" is legal), so this check applies only to the base
	// file path (stream suffix already stripped above).
	if strings.ContainsAny(filename, "*?<>\"") {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameInvalid}}, nil
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

	// Build the full normalized filename (including stream suffix) for
	// durable reconnect and logging. The base `filename` has the stream
	// suffix stripped for safe path.Dir/path.Base; `fullFilename` preserves it.
	fullFilename := filename
	if streamSuffix != "" {
		fullFilename = filename + streamSuffix
	}

	// Handle root directory case. A durable-reconnect request (DHnC/DH2C) that
	// carries an empty filename must NOT open the share root — it has to fall
	// through to Step 4b so the reconnect path validates it (and rejects a stale
	// or mismatched reconnect rather than silently opening the root). smbtorture
	// durable-open.reopen2_lease / _lease_v2 and the V2 reopen2 family send an
	// empty-fname reconnect as a negative case.
	hasReconnectCtx := h.DurableStore != nil &&
		(FindCreateContext(req.CreateContexts, DurableHandleV1ReconnectTag) != nil ||
			FindCreateContext(req.CreateContexts, DurableHandleV2ReconnectTag) != nil)
	if filename == "" && streamSuffix == "" && !hasReconnectCtx {
		resp, err := h.handleOpenRootCreate(ctx, req, authCtx, rootHandle, tree)
		h.storeCreateReplayIfApplicable(ctx, req, resp)
		return resp, err
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
				tree.ShareName, fullFilename, connClientGUID(ctx),
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
			// Per MS-SMB2 3.3.5.9.7: the server returns the original Open.FileId.
			// We restore both halves (persistent + volatile) from the persisted
			// OriginalFileID so the OpenID derived from FileID matches any
			// byte-range locks taken before disconnect — UNLOCK after reconnect
			// on the same volatile would otherwise fail with RANGE_NOT_LOCKED
			// (smb2.durable-open.lock-{oplock,lease}). For older persisted
			// handles written before OriginalFileID existed, the restore path
			// already falls back to the volatile-zeroed FileID; regenerate the
			// volatile half in that fallback case so the FileID is still unique
			// on this connection.
			restored.TreeID = ctx.TreeID
			restored.SessionID = ctx.SessionID
			smbFileID := restored.FileID
			if reconnResult.OriginalFileID == ([16]byte{}) {
				_, _ = rand.Read(smbFileID[8:16])
				restored.FileID = smbFileID
			}

			// Re-grant durability on reconnect. The handle was durable before
			// disconnect, and the successful reconnect implicitly restores it.
			restored.IsDurable = true
			restored.DurableTimeoutMs = h.DurableTimeoutMs

			// Per MS-SMB2 §3.3.5.9.7 / §3.3.5.9.12 and Samba
			// `smbd_smb2_create_send` (which only emits the DH2Q response
			// blob when the request actually carried a DH2Q request context),
			// a reconnect (DHnC / DH2C) response MUST NOT include a DHnQ or
			// DH2Q grant blob: `out.durable_open` and `out.durable_open_v2`
			// are expected to be false and `out.timeout` zero. The handle
			// stays durable for *future* disconnects via the server-side
			// IsDurable flag set above — no wire signaling is required.
			// smbtorture smb2.durable-v2-open.{reopen1a,reopen2,reopen2b,...}
			// assert "no dh2q response blob" on every successful reconnect.
			var reconnectContexts []CreateContext

			// Re-register lease/oplock in the LeaseManager. During disconnect,
			// ReleaseSessionLeases cleared the lease state from the LeaseManager.
			// We must re-request it so that break notifications work and the
			// oplock/lease is visible to other opens.
			if h.LeaseManager != nil && len(restored.MetadataHandle) > 0 {
				lockFileHandle := lock.FileHandle(restored.MetadataHandle)

				// Capture the oplock/lease level persisted with the durable
				// handle BEFORE re-registration mutates restored.OplockLevel.
				// MS-SMB2 §3.3.5.9.7/§3.3.5.9.12: a successful reconnect re-grants
				// the SAME oplock/lease the client held before disconnect — the
				// FileID/OpenID continuity keeps any byte-range locks valid. If
				// LeaseManager re-registration under-delivers (returns a weaker or
				// zero state because the prior synthetic key wasn't fully cleared
				// or best-grantable transiently returns 0), reporting the degraded
				// level would make the client believe its oplock/lease was lost,
				// failing the io.out.oplock_level assertion in
				// smb2.durable-v2-open.lock-{oplock,lease}. Fall back to the
				// persisted level so the reconnect response always reflects what
				// the handle actually still holds.
				persistedOplockLevel := restored.OplockLevel
				persistedLeaseState := reconnResult.PersistedLease
				persistedLeaseEpoch := reconnResult.PersistedEpoch

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
						connClientGUID(ctx),
						ownerID,
						clientID,
						tree.ShareName,
						requestedState,
						restored.IsDirectory,
					)
					// The reconnected handle still holds the lease it had before
					// disconnect (OpenID continuity preserves its byte-range
					// locks). If re-registration under-delivers (re-request error
					// or a granted state weaker than what was persisted), report
					// the persisted lease state rather than the degraded grant so
					// the client sees its lease preserved
					// (smb2.durable-v2-open.lock-lease io.out.lease.lease_state).
					reportedLeaseState := grantedState
					if persistedLeaseState != lock.LeaseStateNone &&
						leaseStateRank(persistedLeaseState) > leaseStateRank(reportedLeaseState) {
						reportedLeaseState = persistedLeaseState
					}
					// Mirror the lease-state fallback for the epoch. The lease-V2
					// epoch is monotonic across the handle's life; a fresh
					// re-registration on reconnect restarts it at 0, which would
					// make the reconnect response report Epoch=0 and leak a stale
					// NewEpoch on the next break. Restore the persisted epoch when
					// the re-grant under-delivers (returns 0 or an older value)
					// (smb2.durable-v2-open.lock-lease asserts lease_epoch==1).
					reportedLeaseEpoch := epoch
					if persistedLeaseEpoch > reportedLeaseEpoch {
						reportedLeaseEpoch = persistedLeaseEpoch
					}
					if leaseErr != nil {
						logger.Debug("CREATE: durable reconnect lease re-request failed", "error", leaseErr)
					}
					if reportedLeaseState != lock.LeaseStateNone {
						// Push the restored epoch back into the lock layer so
						// subsequent break notifications carry the correct
						// NewEpoch rather than the re-registration's reset value.
						if reportedLeaseEpoch != epoch {
							h.LeaseManager.SetLeaseEpoch(lockFileHandle, tree.ShareName, restored.LeaseKey, reportedLeaseEpoch)
						}
						// Build lease response context
						if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
							isV1 := len(leaseCtx.Data) < LeaseV2ContextSize
							leaseResp := &LeaseResponseContext{
								LeaseKey:   restored.LeaseKey,
								LeaseState: reportedLeaseState,
								Epoch:      reportedLeaseEpoch,
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
						//
						// Keyed on reportedLeaseState (not grantedState): when the
						// re-register under-delivers but the handle still holds a
						// persisted lease, the open remains lease-backed and must be
						// re-marked V2 so its OplockLevel stays OplockLevelLease.
						// Lease-backed durable handles require SMB 3.0+, so the
						// reconnect path always re-establishes a V2 lease.
						// MarkLeaseVersionIfUnset preserves any pre-existing
						// version recorded by the original grant; an absent
						// mark (e.g. cleared by intervening release) is set to
						// V2 here.
						h.LeaseManager.MarkLeaseVersionIfUnset(lockFileHandle, tree.ShareName, restored.LeaseKey, true)
						restored.OplockLevel = OplockLevelLease
					}

					// Update session mapping for break notification routing
					h.LeaseManager.UpdateSessionForLease(clientID, tree.ShareName, restored.LeaseKey, ctx.SessionID)
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
						// Reconnect path for a previously-granted traditional
						// oplock. Re-tag IsTraditionalOplock so the cross-tier
						// rules in bestGrantableState continue to apply for
						// concurrent real-lease grants (matches Samba's
						// post-reclaim share_mode_entry op_type semantics).
						grantedState, _, leaseErr := h.LeaseManager.RequestLeaseAsOplock(
							authCtx.Context,
							lockFileHandle,
							syntheticKey,
							[16]byte{},
							ctx.SessionID,
							connClientGUID(ctx),
							ownerID,
							clientID,
							tree.ShareName,
							requestedState,
							false,
						)
						restored.LeaseKey = syntheticKey
						regrantedLevel := leaseStateToOplockLevel(grantedState)
						if leaseErr != nil {
							logger.Debug("CREATE: durable reconnect oplock re-request failed", "error", leaseErr)
						}
						// MS-SMB2 §3.3.5.9.7: the reconnect re-grants the SAME
						// oplock the handle held before disconnect. If the
						// re-register under-delivers (error, or a weaker level
						// because best-grantable transiently returns 0 / the prior
						// synthetic key wasn't fully cleared), report the persisted
						// level rather than the degraded one — the FileID/OpenID
						// continuity keeps the handle's byte-range locks valid, so
						// the client must still see its Batch/Exclusive/II oplock
						// (smb2.durable-v2-open.lock-oplock io.out.oplock_level).
						if oplockLevelRank(regrantedLevel) >= oplockLevelRank(persistedOplockLevel) {
							restored.OplockLevel = regrantedLevel
						} else {
							restored.OplockLevel = persistedOplockLevel
						}
						if restored.OplockLevel != OplockLevelNone {
							h.LeaseManager.RegisterOplockFileID(syntheticKey, smbFileID)
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
					// Echo the per-handle requested AllocationSize restored
					// from the persisted durable handle so the reconnect
					// response reports the same cluster-aligned reservation as
					// the original open ([MS-SMB2] 2.2.13.2.2). Directories
					// never carry a reservation (dropped at grant via
					// allocReservationFor), so this collapses to the file's
					// cluster-aligned size for them
					// (smb2.durable-open.alloc-size).
					AllocationSize: effectiveAllocationSize(size, restored.RequestedAllocSize),
					EndOfFile:      size,
					FileAttributes: FileAttrToSMBAttributes(&respFile.FileAttr),
					FileID:         smbFileID,
					CreateContexts: reconnectContexts,
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

	// Split path into directory and name components. The stream suffix was
	// already stripped from filename above, so path.Dir/path.Base operate
	// only on the base-file path and won't split on "/" inside stream names.
	dirPath := path.Dir(filename)
	baseName := path.Base(filename)
	isADS := streamSuffix != ""
	var adsBaseFileName string // base file entry name (e.g., "file.txt")
	if isADS {
		adsBaseFileName = baseName
		baseName = baseName + streamSuffix // e.g., "file.txt:StreamName"
		filename = filename + streamSuffix // restore full filename for logging
	}

	// Walk to parent directory
	parentHandle := rootHandle
	if dirPath != "." && dirPath != "" {
		parentHandle, err = h.walkPath(authCtx, rootHandle, dirPath)
		if err != nil {
			logger.Debug("CREATE: parent path not found", "path", dirPath, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectPathNotFound}}, nil
		}
	}

	// ADS early validation (before any metadata ops, including share mode
	// checks). Per MS-FSA, name validation precedes access checks.
	if isADS {
		// Streams are always non-directory objects per MS-FSA.
		if req.CreateOptions&types.FileDirectoryFile != 0 {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotADirectory}}, nil
		}
		adsStreamName := streamSuffix[1:] // strip leading ":"
		// Per Samba/Windows: "/", "\", and ":" are OBJECT_NAME_INVALID in
		// stream names. "/" and "\" are path separators; ":" is the stream
		// type delimiter — an extra colon means a malformed type suffix
		// (e.g., trailing ":", ":$FOO", ":?D*a").
		// An empty stream name (just ":") is also invalid.
		// Control characters (0x01-0x1F) are permitted — Samba allows them
		// and smbtorture creates streams containing \x05, \n, etc.
		if adsStreamName == "" || strings.ContainsAny(adsStreamName, "/\\:") {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusObjectNameInvalid}}, nil
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

	// Pre-disposition parent-DACL gate: any non-pure-OPEN disposition mutates
	// the parent directory's contents (creating, overwriting, or superseding a
	// child). MS-FSA orders these the other way round — 2.1.5.1.2 fails
	// FILE_CREATE with OBJECT_NAME_COLLISION before any parent-DACL check — so
	// this follows Samba's open_file_ntcreate, which evaluates parent-create
	// permission first so
	// a deny ACE on the parent surfaces as ACCESS_DENIED rather than
	// OBJECT_NAME_COLLISION / DELETE_PENDING. FILE_OPEN is the only pure-open
	// disposition; everything else (CREATE, CREATE_IF, OVERWRITE,
	// OVERWRITE_IF, SUPERSEDE) is gated.
	//
	// The check evaluates the precise ACL bit for the child kind (ADD_FILE
	// vs ADD_SUBDIRECTORY) so a parent DACL that denies only one variant
	// does not also block the other — required by smbtorture
	// smb2.create.mkdir-visible (deny-WORLD-ADD_FILE inherited ACE must
	// not block subdirectory creation).
	//
	// Load the parent inode exactly once here and reuse it for both the
	// permission gate and the later compression-attribute inheritance in
	// createNewFile — a single GetFile replaces the separate reloads each
	// path used to perform. The compression read only runs on a create
	// disposition, which is precisely when this block loads the parent.
	var parentFile *metadata.File
	if req.CreateDisposition != types.FileOpen {
		var pErr error
		parentFile, pErr = metaSvc.GetFile(authCtx.Context, parentHandle)
		if pErr != nil {
			logger.Debug("CREATE: parent lookup failed",
				"path", filename,
				"disposition", req.CreateDisposition,
				"error", pErr)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(pErr)}}, nil
		}
		isDirCreate := req.CreateOptions&types.FileDirectoryFile != 0
		if err := metaSvc.CheckParentCreateAccessFile(authCtx, parentHandle, parentFile, isDirCreate); err != nil {
			logger.Debug("CREATE: parent DACL denies create",
				"path", filename,
				"disposition", req.CreateDisposition,
				"isDirectory", isDirCreate,
				"error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(err)}}, nil
		}
	}

	// For ADS opens, existence is based on the STREAM entry, and the base
	// file is auto-created when the disposition would create.
	//
	// NTFS/SMB names are case-preserving but case-insensitive.
	// First try a general case-insensitive lookup for the baseName (covers
	// both regular files and ADS entries whose exact case differs).
	// For ADS, also fall back to the stream-specific scanner which matches
	// on the stream suffix within the parent directory.
	existingFile, foundName, lookupErr := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, baseName)
	if lookupErr != nil {
		logger.Debug("CREATE: parent lookup failed", "name", baseName, "error", lookupErr)
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(lookupErr)}}, nil
	}
	fileExists := (existingFile != nil)
	if fileExists && foundName != baseName {
		baseName = foundName // use the canonical stored name
	}
	// lookupCaseInsensitive already does a full EqualFold scan of all
	// children — no separate stream-specific fallback needed.

	// Per MS-FSA: directories do not have a default data stream. When the
	// client explicitly requests "dir::$DATA", the open must fail even if
	// the directory itself exists. FILE_DIRECTORY_FILE → NOT_A_DIRECTORY;
	// otherwise → FILE_IS_A_DIRECTORY.
	if explicitDataStream && fileExists && existingFile.Type == metadata.FileTypeDirectory {
		if req.CreateOptions&types.FileDirectoryFile != 0 {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotADirectory}}, nil
		}
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileIsADirectory}}, nil
	}

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
	// Per MS-FSA 2.1.5.1 Phase 7 ("Type of stream to open"): Disposition check (e.g., FILE_CREATE failing with
	// OBJECT_NAME_COLLISION when the name exists) takes priority over type
	// constraint checks (NOT_A_DIRECTORY / FILE_IS_A_DIRECTORY).
	// For ADS, this also takes priority over base-file share mode checks:
	// if the stream doesn't exist and the disposition is FILE_OPEN, we must
	// return OBJECT_NAME_NOT_FOUND, not SHARING_VIOLATION (per smbtorture
	// smb2.streams.names2). The per-stream share mode check in
	// checkShareModeConflict still runs later for existing streams.

	createAction, dispErr := ResolveCreateDisposition(req.CreateDisposition, fileExists)
	if dispErr != nil {
		return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(dispErr)}}, nil
	}

	// ADS auto-create: when the disposition would create the stream,
	// ensure the base file exists first. Per MS-FSA, creating a named
	// stream implicitly creates the base file.
	var adsBaseCreatedByUs bool
	if isADS && (createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded) {
		adsBase, _, adsLookupErr := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, adsBaseFileName)
		// A real lookup failure (permission denied, store error) must surface
		// as the create status, not be masked by the auto-create path: treating
		// the base as missing would attempt a CreateFile the caller may not be
		// allowed to perform, and the eventual failure would be the wrong error.
		if adsLookupErr != nil && adsBase == nil {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(adsLookupErr)}}, nil
		}
		if adsBase == nil {
			baseAttr := &metadata.FileAttr{
				Mode: SMBModeFromAttrs(0, false),
				Type: metadata.FileTypeRegular,
			}
			if authCtx.Identity.UID != nil {
				baseAttr.UID = *authCtx.Identity.UID
			}
			if authCtx.Identity.GID != nil {
				baseAttr.GID = *authCtx.Identity.GID
			}
			if _, _, createBaseErr := metaSvc.CreateFile(authCtx, parentHandle, adsBaseFileName, baseAttr); createBaseErr != nil {
				// Concurrent stream CREATE may race on base-file creation.
				// ErrAlreadyExists means another goroutine won — proceed.
				var storeErr *metadata.StoreError
				if errors.As(createBaseErr, &storeErr) && storeErr.Code == metadata.ErrAlreadyExists {
					logger.Debug("CREATE: ADS base file created concurrently", "base", adsBaseFileName)
				} else {
					logger.Debug("CREATE: ADS auto-create base file failed",
						"base", adsBaseFileName, "error", createBaseErr)
					return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(createBaseErr)}}, nil
				}
			} else {
				// We exclusively created the base file; record ownership so
				// the lease-rejection rollback path can clean it up.
				adsBaseCreatedByUs = true
				logger.Debug("CREATE: ADS auto-created base file", "base", adsBaseFileName)
			}
		}
	}

	// Validate directory vs file constraints for existing files.
	// Only applies when the disposition opens or overwrites (not FILE_CREATE,
	// which already failed above if the file existed).
	if fileExists && !isADS {
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

	// Per MS-FSA 2.1.5.1.2 ("Open of an Existing File"): Overwrite/supersede attribute mismatch rules.
	// READONLY on the existing file denies the overwrite entirely.
	// HIDDEN and SYSTEM must be present in the request if set on the existing file.
	if fileExists && existingFile.Type != metadata.FileTypeDirectory {
		if createAction == types.FileOverwritten || createAction == types.FileSuperseded {
			existingAttrs := FileAttrToSMBAttributesWithName(&existingFile.FileAttr, baseName)
			if existingAttrs&types.FileAttributeReadonly != 0 {
				logger.Debug("CREATE: cannot overwrite read-only file",
					"path", filename,
					"action", createAction)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
			}
			// Per MS-FSA 2.1.5.1.2 ("Open of an Existing File"): Hidden and System flags must be preserved.
			if existingAttrs&types.FileAttributeHidden != 0 && req.FileAttributes&types.FileAttributeHidden == 0 {
				logger.Debug("CREATE: attribute mismatch — existing hidden, request not hidden",
					"path", filename)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
			}
			if existingAttrs&types.FileAttributeSystem != 0 && req.FileAttributes&types.FileAttributeSystem == 0 {
				logger.Debug("CREATE: attribute mismatch — existing system, request not system",
					"path", filename)
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
	// wait and the async park-on-break goroutine (MS-SMB2 §3.3.4.2 (interim async response)
	// and §3.3.4.7 "Object Store Indicates a Lease Break").

	// Cross-stream share-mode check for new files (no lease break needed).
	if !fileExists {
		if shareConflict := h.checkShareModeConflict(nil, req.DesiredAccess, req.ShareAccess, parentHandle, baseName); shareConflict {
			logger.Debug("CREATE: sharing violation on new file",
				"path", filename,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess),
				"shareAccess", fmt.Sprintf("0x%x", req.ShareAccess))
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSharingViolation}}, nil
		}
	}

	// Step 6c-pre: Check delete-pending state before oplock break dispatch.
	//
	// If an existing open on the file has set disposition delete-on-close,
	// subsequent opens MUST fail with STATUS_DELETE_PENDING without triggering an
	// oplock break. MS-FSA 2.1.5.1 Phase 6 ("Location of file") states this for
	// intermediate path components; for the final component it follows
	// MS-SMB2 3.3.5.9 and Samba. The
	// check runs BEFORE breakAndMaybeParkCreate so the holder's oplock
	// remains intact. Required by smbtorture smb2.oplock.doc.
	//
	// Also checks cross-stream delete-pending: when a base file has been
	// unlinked while a stream handle is still open, opens on both the base
	// file and any of its streams MUST return STATUS_DELETE_PENDING
	// (smbtorture smb2.streams.delete).
	if fileExists {
		if existingHandle, encErr := metadata.EncodeFileHandle(existingFile); encErr == nil {
			if h.isFileOrBaseDeletePending(existingHandle, parentHandle, baseName) {
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusDeletePending}}, nil
			}
		}
	}
	// Even when the file does not exist in the metadata store (already
	// removed by the unlink), a deferred base-file delete may still be
	// pending on an open stream handle. Match those handles by the parent
	// directory and the name within it, there being no metadata handle left
	// to compare against.
	if !fileExists {
		if h.isFileOrBaseDeletePending(nil, parentHandle, baseName) {
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusDeletePending}}, nil
		}
	}

	// Step 6d: AppInstanceId failover (MS-SMB2 §3.3.5.9.13). When this CREATE
	// carries an AppInstanceId matching an existing open (live or disconnected),
	// the existing open MUST be force-closed to claim the file. This runs BEFORE
	// the oplock/lease break dispatch below so the displaced open is gone before
	// any break is computed — the failover is silent (no oplock break on the
	// old open). smbtorture smb2.durable-v2-open.app-instance asserts
	// break_info.count == 0 after a same-AppInstanceId open on a second tree.
	var appInstanceId [16]byte
	if h.DurableStore != nil {
		appInstanceId = ProcessAppInstanceId(
			authCtx.Context, h.DurableStore, h, req.CreateContexts,
		)
	}

	draft := (&createDraft{
		req:                  req,
		tree:                 tree,
		authCtx:              authCtx,
		filename:             filename,
		baseName:             baseName,
		parentHandle:         parentHandle,
		parentFile:           parentFile,
		existingFile:         existingFile,
		fileExists:           fileExists,
		createAction:         createAction,
		isDirectoryRequest:   isDirectoryRequest,
		excludeOwner:         excludeOwner,
		appInstanceProcessed: true,
		appInstanceId:        appInstanceId,
		adsBaseFileName:      adsBaseFileName,
		adsBaseCreatedByUs:   adsBaseCreatedByUs,
	}).finalize()

	// SMB3 replay protection (MS-SMB2 §3.3.5.9): reserve the DH2Q CreateGuid for
	// the entire window during which this second open is blocked/pending on a
	// share-mode conflict — covering BOTH the async lease-park path AND the
	// inline no-oplock sharing-violation/sync-wait path. A replay
	// (FLAGS_REPLAY_OPERATION) arriving while the original is still in this
	// window must return STATUS_FILE_NOT_AVAILABLE (resolveCreateReplay), which
	// requires the guid to be reserved. The no-oplock "n" variant (conflicting
	// holder with no Write/Handle lease bits) takes the inline path and so was
	// previously never reserved (smbtorture replay-dhv2-pending1n-*-sane). The
	// reservation is held only across the break+complete window below and is
	// always released:
	//   - async park (asyncId != 0): the parked resume goroutine owns the
	//     Release (parkCreateOnLeaseBreak), so we MUST NOT release here.
	//   - inline (asyncId == 0): released after completeCreateAfterBreak.
	// Reserve is the single site (parkCreateOnLeaseBreak no longer reserves); a
	// zero CreateGuid or no share conflict is left unreserved (Reserve is a
	// no-op on a zero guid). Gated on fileExists since a conflict requires a
	// pre-existing open to contend with.
	replayGuid := dh2qCreateGuid(req)
	reservedReplay := false
	if h.CreateReplayCache != nil && fileExists && replayGuid != ([16]byte{}) {
		h.CreateReplayCache.Reserve(ctx.SessionID, replayGuid)
		reservedReplay = true
	}

	// Dispatch lease break and either park the CREATE async (emit interim
	// STATUS_PENDING) or wait for the break to drain inline. Mid-chain
	// (NextCommand != 0) parking is the MS-SMB2 §3.3.4.2 interim-async
	// contract: the compound processor defers trailing commands behind the
	// parked CREATE via ReplaceCallback, so the interim PENDING is safe and
	// smbtorture compound_async.getinfo_middle requires it.
	if asyncId := h.breakAndMaybeParkCreate(ctx, draft); asyncId != 0 {
		// Parked: the resume goroutine releases the reservation on completion.
		return &CreateResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusPending},
			AsyncId:         asyncId,
		}, nil
	}

	resp := h.completeCreateAfterBreak(ctx, draft)
	if reservedReplay {
		h.CreateReplayCache.Release(ctx.SessionID, replayGuid)
	}
	return resp, nil
}

// ============================================================================
// Named Pipe Handling (IPC$)
// ============================================================================

// storeCreateReplayIfApplicable mirrors the cache.Store call at the
// bottom of completeCreateAfterBreak for CREATE return paths that
// bypass it (notably handlePipeCreate and handleOpenRootCreate). It is
// the single seam used by those bypass paths so a successful CREATE
// carrying a DH2Q CreateGuid is recorded into the replay cache; a
// FLAGS_REPLAY_OPERATION retry within the replay window can then return
// the cached result. Cache.Store is itself a no-op when CreateGuid is
// zero or when resp.Status != StatusSuccess, so it is safe to call
// unconditionally here (MS-SMB2 §3.3.5.9).

func isStatOnlyOpen(desiredAccess uint32) bool {
	const statOpenBits uint32 = 0x00000080 | // FILE_READ_ATTRIBUTES
		0x00000100 | // FILE_WRITE_ATTRIBUTES
		0x00020000 | // READ_CONTROL
		0x00100000 //  SYNCHRONIZE
	return desiredAccess&statOpenBits != 0 && desiredAccess&^statOpenBits == 0
}

// isOplockStatOpen mirrors Samba `is_oplock_stat_open` (source3/smbd/open.c).
// It is the *narrower* sibling of isStatOnlyOpen: it excludes READ_CONTROL,
// because Samba's traditional-oplock path treats a READ_CONTROL-bearing open
// as a non-stat opener that must break a traditional-oplock holder.
//
// At the new-opener side, this gate is used to strip a *requested* traditional
// oplock down to NONE when the new opener's access mask is oplock-stat-only:
// Samba `open_file_ntcreate`, source3/smbd/open.c line 4000:
//
//	if (is_oplock_stat_open(open_access_mask) && lease == NULL) {
//	    oplock_request &= SAMBA_PRIVATE_OPLOCK_MASK;
//	}
//
// smbtorture smb2.oplock.batch8 / exclusive4 cover this: the second open is
// FILE_READ_ATTRIBUTES|FILE_WRITE_ATTRIBUTES|SYNCHRONIZE asking for BATCH /
// EXCLUSIVE — expected grant is NO_OPLOCK_RETURN (LEVEL_NONE) with NO break
// dispatched to the original holder.

func isOplockStatOpen(desiredAccess uint32) bool {
	const oplockStatBits uint32 = 0x00000080 | // FILE_READ_ATTRIBUTES
		0x00000100 | // FILE_WRITE_ATTRIBUTES
		0x00100000 //  SYNCHRONIZE
	return desiredAccess&oplockStatBits != 0 && desiredAccess&^oplockStatBits == 0
}

// computeSessionKeyHash computes the SHA-256 hash of the session's signing key.
// This is used for durable handle security validation during reconnect.
// Returns zero hash if the session has no crypto state or signing key.

func computeSessionKeyHash(sess *session.Session) [32]byte {
	cs := sess.GetCryptoState()
	if cs == nil || len(cs.SigningKey) == 0 {
		return [32]byte{}
	}
	return sha256.Sum256(cs.SigningKey)
}
