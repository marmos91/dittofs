# DittoFS SMB2 Protocol Implementation Plan

## Implementation Status

**Last Updated**: December 2024

| Phase | Status | Description |
|-------|--------|-------------|
| **Phase 1** | **COMPLETED** | Basic SMB2 Infrastructure with mock data |
| **Phase 2** | **COMPLETED** | Identity System (unified with NFS via `pkg/identity`) |
| **Phase 3** | **COMPLETED** | Connect to Metadata/Content Stores |
| **Phase 4** | Pending | Permission Abstraction & Interoperability |

### Current Capabilities

**Protocols Supported:**
- SMB2 dialect 0x0202 (SMB 2.0.2)
- NTLM authentication with SPNEGO wrapping
- Guest access support

**File Operations:**
- CREATE (open/create files and directories)
- READ (with cache integration)
- WRITE (with cache integration)
- CLOSE
- FLUSH (shared flush logic with NFS COMMIT)
- QUERY_INFO (FILE_BASIC_INFO, FILE_STANDARD_INFO, FILE_NETWORK_OPEN_INFO)
- SET_INFO (timestamps, attributes, rename, delete, truncate)
- QUERY_DIRECTORY (FileBothDirectoryInformation, FileIdBothDirectoryInformation)

**Session Management:**
- Adaptive credit flow control (configurable strategies)
- Compound request handling (CREATE+QUERY_INFO+CLOSE)
- Parallel request processing with configurable concurrency

### Testing Results

**macOS Finder mount**: ✅ Working
```bash
mount -t smbfs //guest@localhost:12445/export /tmp/smb
ls /tmp/smb  # Lists directory contents from real stores
```

**smbclient**: ✅ Working
```bash
smbclient //127.0.0.1/export -p 12445 -N -c "ls"
smbclient //127.0.0.1/export -p 12445 -N -c "get readme.txt -"
```

**Key fixes implemented**:
- Compound request handling (race condition fixed)
- FileID injection for related operations (QUERY_INFO, QUERY_DIRECTORY, CLOSE)
- Credit-based flow control (configurable via credits strategy)
- Directory enumeration state tracking (prevents infinite loops)
- Real metadata/content store integration (removed mock data)
- Cache integration for READ and WRITE operations
- Shared flush logic with NFS (via `pkg/ops`)

### Implementation Files

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
    ├── requests.go                # Typed request/response structs
    ├── encoding.go                # Request/response encoding/decoding
    ├── converters.go              # SMB ↔ metadata type converters
    ├── auth_helper.go             # SMB auth context builder
    ├── negotiate.go               # SMB2 NEGOTIATE
    ├── session_setup.go           # SMB2 SESSION_SETUP (uses internal/auth)
    ├── session_setup_test.go      # SESSION_SETUP tests
    ├── logoff.go                  # SMB2 LOGOFF
    ├── tree_connect.go            # SMB2 TREE_CONNECT (with permission checking)
    ├── tree_disconnect.go         # SMB2 TREE_DISCONNECT
    ├── echo.go                    # SMB2 ECHO
    ├── create.go                  # SMB2 CREATE (real store integration)
    ├── close.go                   # SMB2 CLOSE
    ├── read.go                    # SMB2 READ (with cache support)
    ├── write.go                   # SMB2 WRITE (with cache support)
    ├── flush.go                   # SMB2 FLUSH (uses shared ops.FlushCacheToContentStore)
    ├── query_directory.go         # SMB2 QUERY_DIRECTORY (real store integration)
    ├── query_info.go              # SMB2 QUERY_INFO
    └── set_info.go                # SMB2 SET_INFO

pkg/adapter/smb/
├── config.go                      # SMBConfig struct
├── smb_adapter.go                 # Adapter implementation (uses SessionManager)
└── smb_connection.go              # Per-connection handler (parallel requests)

pkg/identity/                      # Shared identity system (NFS + SMB)
├── user.go                        # User struct with SID, UID/GID
├── group.go                       # Group struct with permissions
├── store.go                       # UserStore interface
└── memory.go                      # In-memory user store

pkg/ops/                           # Shared operations (NFS + SMB)
└── flush.go                       # FlushCacheToContentStore (used by COMMIT/FLUSH)

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

## Phase 2: Identity System

### 2.1 Overview

**Status**: **COMPLETED**

A unified identity system was implemented in `pkg/identity/` that supports both NFS and SMB.

**Implementation Approach**: Instead of SID-based identity mapping, we implemented a unified user management system with:
- Username/password authentication (bcrypt hashed)
- UID/GID for Unix compatibility
- SID for Windows compatibility (auto-generated)
- Share-level permission resolution

### 2.2 What Was Implemented

The `pkg/identity/` package provides:

```go
type User struct {
    Username         string
    PasswordHash     string            // bcrypt
    UID              uint32
    GID              uint32
    SID              string            // Auto-generated Windows SID
    Groups           []string
    SharePermissions map[string]SharePermission
}

type Group struct {
    Name             string
    GID              uint32
    SID              string
    SharePermissions map[string]SharePermission
}

type SharePermission int  // None, Read, ReadWrite, Admin

type UserStore interface {
    GetUser(username string) (*User, error)
    AuthenticateUser(username, password string) (*User, error)
    ResolveSharePermission(user *User, shareName string, defaultPerm SharePermission) SharePermission
}
```

**CLI tools for user management**:
```bash
./dittofs user add alice          # Add user
./dittofs user grant alice /export read-write
./dittofs group add editors
./dittofs user join alice editors
```

### 2.3 Files Created

- `pkg/identity/user.go` - User struct with SID, UID/GID
- `pkg/identity/group.go` - Group struct with permissions
- `pkg/identity/store.go` - UserStore interface
- `pkg/identity/memory.go` - In-memory user store implementation
- `cmd/dittofs/commands/user.go` - User CLI commands
- `cmd/dittofs/commands/group.go` - Group CLI commands

### 2.4 Success Criteria

- [x] Unified identity system for NFS and SMB
- [x] Users with bcrypt password hashing
- [x] Groups with share-level permissions
- [x] Permission resolution: user → group → share default
- [x] CLI tools for user/group management
- [x] SMB sessions linked to DittoFS users

---

## Phase 3: Connect to Stores

### 3.1 Overview

**Status**: **COMPLETED**

All SMB handlers now use real metadata and content stores through the registry.

**Key Achievement**: SMB operations work against real DittoFS stores with:
- Full CREATE/READ/WRITE/CLOSE file operations
- Directory listing via QUERY_DIRECTORY
- Metadata operations via QUERY_INFO/SET_INFO
- Cache integration for performance
- Cross-protocol file visibility (NFS ↔ SMB)

### 3.2 Command to Store Mappings (Implemented)

| SMB Command | Metadata Store Method | Content Store Method |
|-------------|----------------------|---------------------|
| TREE_CONNECT | `Registry.GetShare()` | - |
| CREATE | `Lookup()`, `Create()`, `MkDir()` | `Create()` |
| READ | `PrepareRead()` | `ReadAt()` or cache |
| WRITE | `PrepareWrite()`, `CommitWrite()` | `WriteAt()` or cache |
| CLOSE | - | - |
| FLUSH | `GetFile()` | `ops.FlushCacheToContentStore()` |
| QUERY_INFO | `GetFile()` | - |
| SET_INFO | `SetFileAttributes()`, `Rename()`, `Remove()` | `Truncate()` |
| QUERY_DIRECTORY | `ReadDirectory()` | - |

### 3.3 Key Implementation Details

**Two-Phase Write Pattern** (same as NFS):
```go
// 1. PrepareWrite - validates permissions, gets ContentID
writeOp, err := metadataStore.PrepareWrite(authCtx, handle, newSize)

// 2. Write data to cache or content store
if cache != nil {
    err = cache.WriteAt(ctx, writeOp.ContentID, data, offset)
} else {
    err = contentStore.WriteAt(ctx, writeOp.ContentID, data, offset)
}

// 3. CommitWrite - updates metadata (size, mtime)
_, err = metadataStore.CommitWrite(authCtx, writeOp)
```

**Cache Integration**:
- READ: Tries cache first (dirty data → cached data → content store)
- WRITE: Writes to cache if available (async mode), else direct to content store
- FLUSH: Uses shared `ops.FlushCacheToContentStore()` function

**Path Conversion**:
- SMB backslashes (`\path\to\file`) converted to forward slashes (`/path/to/file`)
- Root path is `/` (share root)

**Attribute Conversion** (in `converters.go`):
- `FileAttrToSMBAttributes()` - Unix mode → SMB file attributes
- `SMBAttributesToFileType()` - SMB attributes → metadata FileType
- `FileAttrToSMBTimes()` - Unix times → SMB FILETIME
- `MetadataErrorToSMBStatus()` - Store errors → NT_STATUS codes

### 3.4 Files Created/Modified

**Created**:
- `internal/protocol/smb/v2/handlers/requests.go` - Typed request/response structs
- `internal/protocol/smb/v2/handlers/encoding.go` - Encoding/decoding functions
- `internal/protocol/smb/v2/handlers/converters.go` - Type conversion helpers
- `internal/protocol/smb/v2/handlers/auth_helper.go` - SMB auth context builder
- `pkg/ops/flush.go` - Shared flush logic for NFS COMMIT and SMB FLUSH

**Deleted**:
- `internal/protocol/smb/v2/handlers/mock_data.go` - No longer needed

**Modified**:
- All handler files (create.go, read.go, write.go, etc.) to use real stores

### 3.5 Success Criteria

- [x] TREE_CONNECT validates against registry shares
- [x] CREATE creates real files in metadata/content stores
- [x] READ returns real file content from content store
- [x] READ uses cache for dirty/cached data
- [x] WRITE persists data to content store or cache
- [x] FLUSH flushes cache to content store
- [x] QUERY_DIRECTORY lists real directory contents
- [x] Files created via SMB are visible via NFS
- [x] Files created via NFS are visible via SMB

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

# Share configuration with permissions
shares:
  - name: /export
    metadata_store: default
    content_store: default
    allow_guest: true
    default_permission: read-write

# User management
users:
  - username: alice
    password_hash: "$2a$10$..."  # bcrypt hash
    uid: 1001
    gid: 1001
    share_permissions:
      /export: read-write
```

---

## SMB Roadmap

The following features are planned for future releases:

### 1. SMBv3 Support

**Priority**: Medium
**Complexity**: High

SMB3 adds enterprise features:
- **Encryption**: End-to-end encryption (AES-128-CCM/GCM)
- **Multichannel**: Multiple network connections for throughput
- **Transparent failover**: Seamless reconnection
- **Secure dialect negotiation**: Prevents downgrade attacks

**Implementation Notes**:
- Requires signing and encryption infrastructure
- Session binding for multichannel
- Persistent handles for failover

### 2. File Locking

**Priority**: High
**Complexity**: Medium

SMB2 supports two locking mechanisms:
- **Opportunistic Locks (Oplocks)**: Client-side caching permissions
- **Byte-Range Locks**: Exclusive or shared locks on file regions

**Implementation Notes**:
- Need to add `SMB2_LOCK` command handler
- Oplock break notifications for cache coherency
- Integration with NFS locking (if NLM is added)

### 3. Security Descriptors and Windows ACLs

**Priority**: Medium
**Complexity**: High

Full Windows security model:
- **Security Descriptors (SD)**: Owner, group, DACL, SACL
- **Access Control Entries (ACE)**: Allow/deny permissions per SID
- **Inheritance**: ACL propagation to child objects

**Implementation Notes**:
- Add `SecurityDescriptor` to `FileAttr` struct
- Implement SD ↔ Unix mode bidirectional conversion
- Schema migration for persistent metadata stores
- `QUERY_INFO` / `SET_INFO` for `FileSecurityInformation`

### 4. Extended Attributes (XAttrs) Support

**Priority**: Low
**Complexity**: Medium

Windows extended attributes and streams:
- **Named Streams**: Alternate data streams (ADS)
- **Extended Attributes**: `FILE_FULL_EA_INFORMATION`
- **Reparse Points**: Symbolic links, junctions

**Implementation Notes**:
- Need metadata store schema changes
- `QUERY_INFO` / `SET_INFO` for EA classes
- Stream enumeration for Finder compatibility

### 5. Kerberos/LDAP/Active Directory Integration

**Priority**: Medium
**Complexity**: High

Enterprise authentication:
- **Kerberos**: SPNEGO with Kerberos (currently NTLM only)
- **LDAP**: User/group lookup from directory
- **Active Directory**: Domain join, GPO support

**Implementation Notes**:
- gokrb5 library already in use for SPNEGO parsing
- Need KDC configuration and keytab support
- LDAP user store implementation
- Service principal name (SPN) registration

---

## Testing Roadmap

### Immediate (E2E Test Suite)

- [ ] Set up SMB E2E test infrastructure (smbclient or CIFS mount)
- [ ] Basic file operations (create, read, write, delete)
- [ ] Directory operations (mkdir, rmdir, readdir)
- [ ] Cross-protocol tests (NFS create → SMB read, vice versa)
- [ ] Permission enforcement tests
- [ ] Large file tests (>4GB)

### Future

- [ ] Windows client compatibility testing
- [ ] macOS Finder edge cases
- [ ] Performance benchmarks vs native SMB
- [ ] Stress testing (concurrent connections)
- [ ] Fuzzing for protocol compliance
