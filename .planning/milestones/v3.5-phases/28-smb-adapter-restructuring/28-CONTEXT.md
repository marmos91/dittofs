# Phase 28: SMB Adapter Restructuring - Context

**Gathered:** 2026-02-25
**Status:** Ready for planning

<domain>
## Phase Boundary

Restructure the SMB adapter to mirror the NFS adapter pattern from Phase 27. Extract shared BaseAdapter, reorganize files, define clean Authenticator interface, slim down connection.go. No new protocol features — pure code organization.

</domain>

<decisions>
## Implementation Decisions

### BaseAdapter Extraction
- **Struct embedding**: BaseAdapter is an embedded struct in both NFS and SMB adapters
- **Full lifecycle scope**: Listener management, shutdown orchestration (graceful + force-close), connection tracking (sync.Map + WaitGroup + atomic counter), connection semaphore, metrics logging
- **Runtime reference**: SetRuntime() lives in BaseAdapter. Protocol adapters can override for additional setup (e.g., SMB applies live settings, NFS sets up portmap)
- **Both NFS + SMB**: Extract BaseAdapter and immediately refactor BOTH adapters to use it in this phase. Proves the abstraction is correct
- **Port() in base, Protocol() per-adapter**: Port comes from config (identical logic). Protocol() returns a constant string, stays per-adapter
- **ConnectionFactory interface**: Protocol adapters implement `ConnectionFactory` with `NewConnection(net.Conn) ConnectionHandler`. BaseAdapter calls it in the accept loop. More explicit and testable than callbacks
- **TCP_NODELAY in base**: Universally wanted. Protocol-specific pre-accept checks (e.g., SMB live max_connections) stay per-adapter

### Authenticator Interface
- **Unified interface in `pkg/adapter/auth.go`**: Single `Authenticator` interface used by all three auth paths:
  - NFSv3 AUTH_UNIX: called per-RPC, always single-round
  - NFSv4 RPCSEC_GSS: called during GSS context establishment, multi-round
  - SMB SPNEGO: called during SESSION_SETUP, multi-round (NTLM 2-round, Kerberos 1-round)
- **Full bridge pattern**: Authenticator validates token, looks up DittoFS user in control plane, returns `models.User` + session key. Same pattern as NFS where auth context extraction produces a ready-to-use identity
- **SPNEGO handled inside Authenticator**: SMB Authenticator receives raw SPNEGO tokens, internally detects mechanism (NTLM vs Kerberos), delegates. Session_setup just passes the security blob
- **SMB implementations in `internal/adapter/smb/auth/`**: Single package with authenticator.go, ntlm.go, spnego.go. Interface + implementations together
- **NFS implementations in `internal/adapter/nfs/auth/`**: For symmetry, create AUTH_UNIX authenticator. Extract from dispatch.go and v3/v4 auth helpers

### Connection Slimming (SMB)
- **Mirror NFS structure exactly**: SMB should mirror NFS as closely as possible
- **4-way split**:
  - `pkg/adapter/smb/connection.go` (~150 lines): Thin Serve loop, NewConnection, track/untrack session, cleanup, panic recovery
  - `internal/adapter/smb/dispatch.go`: Request routing, processRequest, processRequestWithFileID, sendResponse, sendErrorResponse, SMB1 negotiate handler, async change notify response
  - `internal/adapter/smb/framing.go`: NetBIOS framing (readRequest, writeNetBIOSFrame, sendMessage, sendRawMessage)
  - `internal/adapter/smb/compound.go`: Compound request handling (processCompoundRequest, injectFileID)
- **NFS-style dispatch chain**: Single dispatch.go for now (only SMB2). Split into dispatch_smb2.go/dispatch_smb3.go when SMB3 is added in Phase 39+
- **Signing verification** moves to existing `internal/adapter/smb/signing/` package (crypto concerns isolated)
- **Rename utils.go to helpers.go**: Align naming with NFS's helpers.go

### File Naming & Auth Move
- **Drop smb_ prefix**: smb_adapter.go -> adapter.go, smb_connection.go -> connection.go (mirrors NFS Phase 27 pattern)
- **Rename structs**: SMBAdapter -> Adapter, SMBConnection -> Connection (package provides context, mirrors NFS)
- **git mv for auth move**: `internal/auth/` -> `internal/adapter/smb/auth/` using git mv to preserve history
- **Delete old internal/auth/ entirely**: Clean break, no stale directories, all imports updated in same commit
- **Config stays as-is**: pkg/adapter/smb/config.go is already well-structured, no changes needed

### Handler Documentation
- **Separate final pass**: Dedicated documentation plan after all restructuring is done (mirrors NFS Phase 27 Plan 04). Ensures docs reflect final structure, not intermediate states
- **3-5 lines per handler**: Consistent with NFS handler documentation style

### Claude's Discretion
- Exact BaseAdapter field names and helper method signatures
- Internal organization of dispatch.go (function ordering, helper grouping)
- How to handle import cycle avoidance during the auth move
- Test file organization (which tests move with which code)

</decisions>

<specifics>
## Specific Ideas

- "SMB should mirror NFS as closely as possible as structure" — the NFS adapter post-Phase 27 is the gold standard for how SMB should look
- "Make sure to create the same dispatch chain as NFS" — dispatch entry point pattern must match
- "Authenticator should translate dittofs authentication to smb authentication exactly as nfs translates from dittofs users to uid" — full bridge pattern, not partial

</specifics>

<deferred>
## Deferred Ideas

- SMB3 dialect dispatch split (dispatch_smb2.go / dispatch_smb3.go) — Phase 39+
- NFS AUTH_UNIX Authenticator is extracted in this phase, but full NFS auth refactoring may need Phase 29

</deferred>

---

*Phase: 28-smb-adapter-restructuring*
*Context gathered: 2026-02-25*
