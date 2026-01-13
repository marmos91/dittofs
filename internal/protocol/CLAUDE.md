# internal/protocol

Low-level protocol implementations - NFS and SMB wire formats and handlers.

## Layer Responsibilities

**This layer handles ONLY protocol concerns:**
- XDR/SMB2 encoding/decoding
- RPC message framing
- Procedure dispatch
- Wire type ↔ internal type conversion

**Business logic belongs in pkg/metadata and pkg/blocks.**

## NFS (`nfs/`)

### Directory Structure
```
dispatch.go     - RPC routing, auth extraction
rpc/            - RPC call/reply handling
xdr/            - XDR encoding/decoding
types/          - NFS constants and error codes
mount/handlers/ - MOUNT protocol (MNT, UMNT, EXPORT, DUMP)
v3/handlers/    - NFSv3 procedures (READ, WRITE, LOOKUP, etc.)
```

### Auth Context Threading
```
RPC Call → ExtractAuthContext() → Handler → Service → Store
```
- Created in `dispatch.go:ExtractHandlerContext()`
- Export-level squashing (AllSquash, RootSquash) applied at mount time

### Two-Phase Write Pattern
```
PrepareWrite() → [content store write] → CommitWrite()
```
- `PrepareWrite`: Validates, returns intent (no metadata changes)
- `CommitWrite`: Applies size/time updates atomically

### Buffer Pooling
Three-tier pools (4KB, 64KB, 1MB) in `pkg/bufpool/`. Reduces GC ~90%.

## SMB (`smb/`)

### Directory Structure
```
header/   - SMB2 header parsing
rpc/      - SMB-RPC handling
session/  - Session state machine
signing/  - Message signing
types/    - SMB2 constants
v2/       - SMB2 command handlers
```

## Common Mistakes

1. **Business logic in handlers** - permissions, validation belong in service layer
2. **Parsing file handles** - they're opaque, just pass through
3. **Wrong log level** - DEBUG for expected errors (not found), ERROR for unexpected
4. **Not using buffer pools** - significant GC pressure under load
5. **Forgetting WCC data** - pre-operation attributes required for client cache coherency
