# DittoFS SMB2 Protocol Implementation Plan

## Implementation Status

**Last Updated**: December 2024

| Phase | Status | Description |
|-------|--------|-------------|
| **Phase 1** | **COMPLETED** | Basic SMB2 Infrastructure with mock data |
| **Phase 2** | Pending | Identity Abstraction (SID-based) |
| **Phase 3** | Pending | Connect to Metadata/Content Stores |
| **Phase 4** | Pending | Permission Abstraction & Interoperability |

### Phase 1 Testing Results

**macOS Finder mount**: ✅ Working
```bash
mount -t smbfs //guest@localhost:12445/export /tmp/smb
ls /tmp/smb  # Lists directory contents successfully
```

**Key fixes implemented**:
- Compound request handling (race condition fixed)
- FileID injection for related operations (QUERY_INFO, QUERY_DIRECTORY, CLOSE)
- Credit-based flow control (grants 256 credits)
- Directory enumeration state tracking (prevents infinite loops)

### Phase 1 Completed Files

```
internal/auth/                     # Shared authentication (SMB + NFSv4)
├── ntlm/
│   ├── ntlm.go                    # NTLM message types, building, parsing
│   └── ntlm_test.go               # NTLM tests
└── spnego/
    ├── spnego.go                  # SPNEGO token parsing (gokrb5-based)
    └── spnego_test.go             # SPNEGO tests

internal/protocol/smb/
├── dispatch.go                    # Command dispatch table
├── types/
│   ├── constants.go               # Typed enums (Command, HeaderFlags, Dialect, etc.)
│   ├── status.go                  # NT_STATUS type with severity levels
│   └── filetime.go                # Windows FILETIME conversion
├── header/
│   ├── header.go                  # SMB2Header struct with documentation
│   ├── header_test.go             # Header tests
│   ├── parser.go                  # Parse from wire format
│   └── encoder.go                 # Encode to wire format
├── session/                       # Unified session management
│   ├── session.go                 # Session struct (identity + credits)
│   ├── credits.go                 # Credit configuration and strategy
│   ├── manager.go                 # SessionManager with lifecycle management
│   └── manager_test.go            # Session manager tests
└── v2/handlers/
    ├── handler.go                 # Main Handler struct (uses SessionManager)
    ├── handler_test.go            # Handler tests
    ├── context.go                 # SMBHandlerContext
    ├── result.go                  # HandlerResult type
    ├── mock_data.go               # Mock filesystem
    ├── negotiate.go               # SMB2 NEGOTIATE
    ├── session_setup.go           # SMB2 SESSION_SETUP (uses internal/auth)
    ├── session_setup_test.go      # SESSION_SETUP tests
    ├── logoff.go                  # SMB2 LOGOFF
    ├── tree_connect.go            # SMB2 TREE_CONNECT
    ├── tree_disconnect.go         # SMB2 TREE_DISCONNECT
    ├── echo.go                    # SMB2 ECHO
    ├── create.go                  # SMB2 CREATE
    ├── close.go                   # SMB2 CLOSE
    ├── read.go                    # SMB2 READ
    ├── write.go                   # SMB2 WRITE
    ├── flush.go                   # SMB2 FLUSH
    ├── query_directory.go         # SMB2 QUERY_DIRECTORY
    ├── query_info.go              # SMB2 QUERY_INFO
    └── set_info.go                # SMB2 SET_INFO

pkg/adapter/smb/
├── config.go                      # SMBConfig struct
├── smb_adapter.go                 # Adapter implementation (uses SessionManager)
└── smb_connection.go              # Per-connection handler

pkg/config/
├── config.go                      # Added SMB to AdaptersConfig
├── adapters.go                    # CreateAdapters includes SMB
└── defaults.go                    # SMB defaults
```

### Shared Authentication Package

The `internal/auth/` package provides shared authentication components for both SMB and future NFSv4:

**`internal/auth/ntlm/`** - NTLM authentication:
- Message type detection and parsing
- Challenge (Type 2) message building
- Negotiate flags and AV_PAIR handling
- TODO: Encryption support (advertised but not implemented)
- TODO: Challenge verification for production auth

**`internal/auth/spnego/`** - SPNEGO token handling:
- Uses `github.com/jcmturner/gokrb5/v8` for proper ASN.1 parsing
- NegTokenInit/NegTokenResp parsing and building
- Mechanism OID detection (NTLM, Kerberos)
- Foundation for future Kerberos support

**Why shared?** NFSv4 with RPCSEC_GSS will use the same Kerberos/SPNEGO infrastructure.
The gokrb5 library provides unified support for both protocols.

### Testing Phase 1

```bash
# Enable SMB in config
adapters:
  smb:
    enabled: true
    port: 12445  # Use non-privileged port

# Start server
./dittofs start

# Test with smbclient
smbclient //127.0.0.1/export -p 12445 -N -c "ls"
smbclient //127.0.0.1/export -p 12445 -N -c "get readme.txt -"
```

---

## Document Purpose

This document provides a comprehensive implementation plan for adding SMB2 protocol support to DittoFS. Each phase is broken down into detailed steps that can be implemented independently.

**Target**: SMB2 dialect only (0x0202 - SMB 2.0.2)
**Approach**: From scratch implementation (like NFS)
**Identity Strategy**: Implement abstraction before connecting stores

---

## Table of Contents

1. [Phase Overview](#phase-overview)
2. [Phase 1: Basic SMB2 Infrastructure](#phase-1-basic-smb2-infrastructure) - COMPLETED
3. [Phase 2: Identity Abstraction](#phase-2-identity-abstraction)
4. [Phase 3: Connect to Stores](#phase-3-connect-to-stores)
5. [Phase 4: Permission Abstraction & Interoperability](#phase-4-permission-abstraction--interoperability)
6. [Appendix: SMB2 Protocol Reference](#appendix-smb2-protocol-reference)

---

## Phase Overview

| Phase | Goal | Dependencies | Deliverable |
|-------|------|--------------|-------------|
| **1** | Basic SMB2 Infrastructure | None | Mountable share with mock data |
| **2** | Identity Abstraction | Phase 1 | SID-based identity system |
| **3** | Connect to Stores | Phase 1, 2 | Full store integration |
| **4** | Permission & Interop | Phase 1, 2, 3 | Windows ACLs, cross-protocol |

---

## Phase 1: Basic SMB2 Infrastructure

### 1.1 Overview

**Status**: COMPLETED

**Goal**: Create a mountable SMB2 share that accepts connections and responds with mock/stub data.

**Success Criteria**:
```bash
# Should work after Phase 1 completion
smbclient //localhost/export -p 12445 -N -c "ls"
smbclient //localhost/export -p 12445 -N -c "get readme.txt -"
```

### 1.2 Directory Structure

```
dittofs/
├── internal/auth/                        # Shared authentication (SMB + NFSv4)
│   ├── ntlm/                             # NTLM authentication
│   │   ├── ntlm.go                       # Message types, building, parsing
│   │   └── ntlm_test.go                  # Tests
│   └── spnego/                           # SPNEGO token handling
│       ├── spnego.go                     # gokrb5-based parsing
│       └── spnego_test.go                # Tests
│
├── pkg/adapter/smb/                      # Public adapter
│   ├── smb_adapter.go                    # Adapter interface implementation
│   ├── smb_connection.go                 # Per-connection handler
│   └── config.go                         # SMBConfig struct
│
├── internal/protocol/smb/                # Protocol implementation
│   ├── dispatch.go                       # Command dispatch table
│   │
│   ├── types/                            # Constants and types
│   │   ├── constants.go                  # Command codes, flags, dialects
│   │   ├── status.go                     # NT_STATUS codes
│   │   └── filetime.go                   # Windows FILETIME conversion
│   │
│   ├── header/                           # Message header handling
│   │   ├── header.go                     # Header structs
│   │   ├── parser.go                     # Parse from wire format
│   │   └── encoder.go                    # Encode to wire format
│   │
│   └── v2/handlers/                      # SMB2 command handlers
│       ├── handler.go                    # Main Handler struct
│       ├── context.go                    # SMBHandlerContext
│       ├── mock_data.go                  # Mock files/directories
│       ├── negotiate.go                  # NEGOTIATE
│       ├── session_setup.go              # SESSION_SETUP (imports internal/auth)
│       ├── session_setup_test.go         # SESSION_SETUP tests
│       ├── logoff.go                     # LOGOFF
│       ├── tree_connect.go               # TREE_CONNECT
│       ├── tree_disconnect.go            # TREE_DISCONNECT
│       ├── create.go                     # CREATE (open/create file)
│       ├── close.go                      # CLOSE
│       ├── read.go                       # READ
│       ├── write.go                      # WRITE
│       ├── flush.go                      # FLUSH
│       ├── query_info.go                 # QUERY_INFO
│       ├── set_info.go                   # SET_INFO
│       ├── query_directory.go            # QUERY_DIRECTORY
│       └── echo.go                       # ECHO
│
├── pkg/config/                           # Configuration (modified)
│   ├── config.go                         # Add SMB to AdaptersConfig
│   ├── adapters.go                       # Add SMB to CreateAdapters()
│   └── defaults.go                       # Add SMB defaults
│
└── test/                                 # SMB tests (TODO)
    ├── integration/smb/                  # Integration tests
    └── e2e/smb_test.go                   # E2E tests (build tag)
```

### 1.3 Implementation Details

Phase 1 implements:
- SMB2 dialect 0x0202 negotiation
- Guest authentication (no real auth)
- Tree connections to `/export` share
- Mock filesystem with `readme.txt` and `subdir`
- File operations: CREATE, READ, WRITE, CLOSE, FLUSH
- Directory operations: QUERY_DIRECTORY
- Metadata operations: QUERY_INFO, SET_INFO
- Session management: LOGOFF, ECHO

### 1.4 Testing Strategy

#### Unit Tests
```bash
go test ./internal/protocol/smb/...
go test ./pkg/adapter/smb/...
```

#### Manual Testing
```bash
# Start server (use non-privileged port for testing)
DITTOFS_ADAPTERS_SMB_PORT=12445 ./dittofs start

# Test with macOS Finder (VERIFIED WORKING)
mount -t smbfs //guest@localhost:12445/export /tmp/smb
ls /tmp/smb
cat /tmp/smb/readme.txt
umount /tmp/smb

# Test with smbclient
smbclient //127.0.0.1/export -p 12445 -N -c "ls"
smbclient //127.0.0.1/export -p 12445 -N -c "get readme.txt -"

# Test with Windows
net use X: \\127.0.0.1\export /user:guest "" /port:12445
```

### 1.5 Success Criteria Checklist

- [x] SMB2 NEGOTIATE succeeds (dialect 0x0202 selected)
- [x] SMB2 SESSION_SETUP succeeds (guest session)
- [x] SMB2 TREE_CONNECT succeeds for `/export`
- [x] SMB2 TREE_CONNECT fails for unknown shares
- [x] SMB2 CREATE opens mock files
- [x] SMB2 READ returns mock file content
- [x] SMB2 QUERY_DIRECTORY lists mock directory
- [x] SMB2 CLOSE works correctly
- [x] macOS Finder can mount share
- [x] macOS Finder can list directory (`ls /tmp/smb`)
- [ ] smbclient can list directory (needs testing)
- [ ] smbclient can read readme.txt (needs testing)
- [x] Graceful shutdown works

---

## Phase 2: Identity Abstraction

### 2.1 Overview

**Status**: Pending

Enable SID-based identity alongside UID/GID before connecting to stores.

**Goal**: Create an identity abstraction layer that supports both:
- Unix identity (UID/GID) for NFS
- Windows identity (SID) for SMB

### 2.2 Key Changes

1. **Activate Identity.SID and Identity.GroupSIDs fields**
   - Currently these fields exist in `pkg/store/metadata/authentication.go` but are unused
   - Need to populate them from SMB session authentication

2. **Add identity type detection methods**
   ```go
   type Identity struct {
       // Existing
       UID      uint32
       GID      uint32
       GIDs     []uint32

       // Windows identity (activate these)
       SID      string   // Primary SID
       GroupSIDs []string // Group SIDs
   }

   func (i *Identity) IsUnixIdentity() bool
   func (i *Identity) IsWindowsIdentity() bool
   func (i *Identity) HasValidIdentity() bool
   ```

3. **Create SID ↔ UID/GID mapping infrastructure**
   ```go
   type IdentityMapper interface {
       // Map Windows SID to Unix UID/GID
       SIDToUnix(sid string) (uid, gid uint32, err error)

       // Map Unix UID/GID to Windows SID
       UnixToSID(uid, gid uint32) (sid string, err error)

       // Check if mapping exists
       HasMapping(sid string) bool
   }
   ```

4. **Update share configuration for SID mapping**
   ```yaml
   shares:
     - name: /export
       identity_mapping:
         # Existing Unix mapping
         map_all_to_anonymous: false
         anonymous_uid: 65534
         anonymous_gid: 65534

         # New Windows mapping
         sid_mapping:
           enabled: true
           default_sid: "S-1-5-21-..."  # Default SID for unmapped users
           mappings:
             - sid: "S-1-5-21-...-1000"
               uid: 1000
               gid: 1000
   ```

### 2.3 Files to Modify

- `pkg/store/metadata/authentication.go` - Activate SID fields
- `pkg/registry/share.go` - Add identity mapping configuration
- `pkg/registry/access.go` - Add SID ↔ UID/GID mapping logic
- `internal/protocol/smb/v2/handlers/session_setup.go` - Extract SID from auth

### 2.4 Implementation Steps

1. **Define IdentityMapper interface** in `pkg/registry/identity.go`
2. **Implement StaticIdentityMapper** for configuration-based mapping
3. **Update ShareConfig** to include SID mapping configuration
4. **Modify SESSION_SETUP** to create proper Identity with SID
5. **Update handler context** to carry resolved identity
6. **Add identity resolution** before store operations

### 2.5 Success Criteria

- [ ] Identity struct supports both Unix and Windows identities
- [ ] Static SID ↔ UID/GID mapping works from configuration
- [ ] SMB sessions have valid Identity with SID
- [ ] Identity can be converted between formats for store operations

---

## Phase 3: Connect to Stores

### 3.1 Overview

**Status**: Pending

Replace mock data with actual metadata and content store integration.

**Goal**: SMB operations work against real DittoFS stores, not mock data.

### 3.2 Key Mappings

| SMB Command | Metadata Store Method | Content Store Method |
|-------------|----------------------|---------------------|
| TREE_CONNECT | `GetRootHandle(shareName)` | - |
| CREATE | `GetChild()`, `CreateFile()`, `CreateDirectory()` | `Create()` |
| READ | `GetFile()` | `ReadAt()` |
| WRITE | `WriteFile()` | `WriteAt()` |
| CLOSE | - | - |
| FLUSH | - | `Sync()` |
| QUERY_INFO | `GetFile()` | `Stat()` |
| SET_INFO | `SetAttr()` | `Truncate()` |
| QUERY_DIRECTORY | `ListDirectory()` | - |

### 3.3 Implementation Steps

1. **Remove mock_data.go** - Replace with real store calls

2. **Update Handler struct**
   ```go
   type Handler struct {
       Registry *registry.Registry  // Already exists
       // Remove mock data fields
   }
   ```

3. **Implement store integration for each command**:

   **TREE_CONNECT**:
   ```go
   func (h *Handler) TreeConnect(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
       // Parse share path
       shareName := parseSharePath(sharePath)

       // Get share from registry
       share, err := h.Registry.GetShare(shareName)
       if err != nil {
           return NewErrorResult(types.StatusBadNetworkName), nil
       }

       // Get root handle
       rootHandle, err := share.MetadataStore.GetRootHandle(shareName)
       // ...
   }
   ```

   **CREATE**:
   ```go
   func (h *Handler) Create(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
       share := h.Registry.GetShareByTreeID(ctx.TreeID)

       // Convert path (backslash to forward slash)
       path := convertPath(filename)

       // Resolve to file handle
       parentHandle, name := splitPath(path)

       switch createDisposition {
       case types.FileOpen:
           handle, err := share.MetadataStore.GetChild(parentHandle, name, identity)
       case types.FileCreate:
           handle, err := share.MetadataStore.CreateFile(parentHandle, name, mode, identity)
           contentID, err := share.ContentStore.Create()
       // ...
       }
   }
   ```

4. **Handle path conversion**:
   - SMB uses backslashes: `\path\to\file`
   - DittoFS stores use forward slashes: `/path/to/file`
   - Need conversion layer in handlers

5. **Handle attribute conversion**:
   - SMB uses Windows file attributes (DIRECTORY, HIDDEN, READONLY, etc.)
   - DittoFS uses Unix mode bits
   - Need bidirectional conversion

### 3.4 Files to Create/Modify

- `internal/protocol/smb/v2/handlers/store.go` - Store integration helpers
- `internal/protocol/smb/v2/handlers/path.go` - Path conversion utilities
- `internal/protocol/smb/v2/handlers/attributes.go` - Attribute conversion
- Modify all handler files to use stores instead of mock data

### 3.5 Success Criteria

- [ ] TREE_CONNECT validates against registry shares
- [ ] CREATE creates real files in metadata/content stores
- [ ] READ returns real file content from content store
- [ ] WRITE persists data to content store
- [ ] QUERY_DIRECTORY lists real directory contents
- [ ] Files created via SMB are visible via NFS
- [ ] Files created via NFS are visible via SMB

---

## Phase 4: Permission Abstraction & Interoperability

### 4.1 Overview

**Status**: Pending

Add Windows ACL support and ensure cross-protocol file access works correctly.

**Goal**:
- Files created via SMB have proper Windows security descriptors
- Files created via NFS have proper Unix permissions
- Both can be converted/mapped for cross-protocol access

### 4.2 Key Changes

1. **Add SecurityDescriptor to FileAttr**
   ```go
   type FileAttr struct {
       // Existing Unix attributes
       Mode  uint32
       UID   uint32
       GID   uint32
       // ...

       // New Windows attributes
       SecurityDescriptor []byte  // Raw SD in self-relative format
       DosAttributes      uint32  // FILE_ATTRIBUTE_* flags
   }
   ```

2. **Implement dual permission checking**
   ```go
   func CheckAccess(identity *Identity, attr *FileAttr, access AccessMask) error {
       if identity.IsWindowsIdentity() {
           return checkWindowsAccess(identity, attr.SecurityDescriptor, access)
       }
       return checkUnixAccess(identity, attr.Mode, attr.UID, attr.GID, access)
   }
   ```

3. **Handle cross-protocol file creation**
   - SMB creates file → Generate both SD and Unix mode
   - NFS creates file → Generate both Unix mode and default SD

4. **Schema migrations for persistent stores**
   - Add SecurityDescriptor column to BadgerDB/PostgreSQL
   - Migration scripts for existing data

### 4.3 Implementation Steps

1. **Define SecurityDescriptor types** in `pkg/store/metadata/security.go`
   ```go
   type SecurityDescriptor struct {
       Owner    SID
       Group    SID
       DACL     []ACE
       SACL     []ACE
   }

   type ACE struct {
       Type       ACEType
       Flags      ACEFlags
       AccessMask AccessMask
       SID        SID
   }
   ```

2. **Implement SD ↔ Mode conversion**
   ```go
   func ModeToSecurityDescriptor(mode uint32, uid, gid uint32) *SecurityDescriptor
   func SecurityDescriptorToMode(sd *SecurityDescriptor) uint32
   ```

3. **Update metadata stores** to persist SecurityDescriptor

4. **Update SMB handlers** to use SecurityDescriptor for access checks

5. **Update NFS handlers** to generate/use SecurityDescriptor

### 4.4 Files to Create/Modify

- `pkg/store/metadata/security.go` - Security descriptor types
- `pkg/store/metadata/security_convert.go` - SD ↔ Mode conversion
- `pkg/store/metadata/file.go` - Add SecurityDescriptor to FileAttr
- `pkg/store/metadata/badger/migration.go` - Schema migration
- `pkg/store/metadata/postgres/migration.go` - Schema migration

### 4.5 Success Criteria

- [ ] Files created via SMB have valid SecurityDescriptor
- [ ] Files created via NFS have valid SecurityDescriptor (generated)
- [ ] SMB access checks use SecurityDescriptor
- [ ] NFS access checks use Unix mode (with SD as fallback)
- [ ] Cross-protocol access works correctly
- [ ] Existing data migrates successfully

---

## Appendix: SMB2 Protocol Reference

### Message Format

```
+------------------+
| NetBIOS Header   | 4 bytes (type + 24-bit length)
+------------------+
| SMB2 Header      | 64 bytes
+------------------+
| Command Body     | Variable
+------------------+
```

### SMB2 Header Structure (64 bytes)

| Offset | Size | Field |
|--------|------|-------|
| 0 | 4 | ProtocolID (0xFE534D42) |
| 4 | 2 | StructureSize (64) |
| 6 | 2 | CreditCharge |
| 8 | 4 | Status (response) / ChannelSequence (request) |
| 12 | 2 | Command |
| 14 | 2 | CreditRequest/Response |
| 16 | 4 | Flags |
| 20 | 4 | NextCommand |
| 24 | 8 | MessageID |
| 32 | 4 | Reserved / ProcessID |
| 36 | 4 | TreeID |
| 40 | 8 | SessionID |
| 48 | 16 | Signature |

### Key NT_STATUS Codes

| Code | Name | Description |
|------|------|-------------|
| 0x00000000 | STATUS_SUCCESS | Operation successful |
| 0xC0000022 | STATUS_ACCESS_DENIED | Access denied |
| 0xC0000034 | STATUS_OBJECT_NAME_NOT_FOUND | File not found |
| 0xC0000035 | STATUS_OBJECT_NAME_COLLISION | File already exists |
| 0xC00000BA | STATUS_FILE_IS_A_DIRECTORY | Cannot read directory as file |
| 0xC0000103 | STATUS_NOT_A_DIRECTORY | Not a directory |
| 0xC00000CC | STATUS_BAD_NETWORK_NAME | Share not found |
| 0x80000006 | STATUS_NO_MORE_FILES | End of directory listing |

### Key References

- [MS-SMB2]: SMB2 Protocol Specification
- [MS-FSCC]: File System Control Codes
- [MS-ERREF]: Windows Error Codes
- [MS-DTYP]: Windows Data Types (for SID, ACL)

---

## Configuration Example

```yaml
# Enable SMB adapter
adapters:
  nfs:
    enabled: true
    port: 2049
  smb:
    enabled: true
    port: 445        # Standard SMB port (requires root)
    # port: 12445    # Non-privileged port for testing
    timeouts:
      read: 5m
      write: 30s
      idle: 5m
      shutdown: 30s

# Share configuration with identity mapping
shares:
  - name: /export
    metadata_store: default
    content_store: default
    identity_mapping:
      map_all_to_anonymous: false
      anonymous_uid: 65534
      anonymous_gid: 65534
```
