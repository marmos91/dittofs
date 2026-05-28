# Phase 27: NFS Adapter Restructuring - Research

**Researched:** 2026-02-25
**Domain:** Go package restructuring, NFS protocol directory layout, dispatch consolidation
**Confidence:** HIGH

## Summary

Phase 27 is a large-scale directory rename and file reorganization with zero behavioral changes. The core task is renaming `internal/protocol/` to `internal/adapter/`, consolidating NFS ecosystem protocols (NLM, NSM, portmapper) under `internal/adapter/nfs/`, reorganizing `pkg/adapter/nfs/` file names, splitting v4.1 into a nested hierarchy, consolidating dispatch into a single entry point, and adding handler documentation.

The primary risk is import path breakage across the 312 Go files that reference `internal/protocol`. The work must be done in carefully ordered waves: rename first, reorganize second, consolidate dispatch third, test throughout. Go's tooling (`go build ./...`, `go test ./...`, `go vet ./...`) provides immediate feedback after each step.

**Primary recommendation:** Execute as a series of atomic git-mv + sed-based import rewrites, verifying `go build ./...` after each move. Never have a broken build state between commits.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- `internal/protocol/` renamed to `internal/adapter/`
- SMB code moves with the rename (`internal/adapter/smb/`) but is NOT restructured (Phase 28)
- `internal/auth/` (NTLM+SPNEGO) stays where it is, deferred to Phase 28
- NLM, NSM, portmapper move under `internal/adapter/nfs/` as peers of v3/v4/mount
- Each ecosystem protocol keeps its own `xdr/` subdirectory for protocol-specific types
- Generic XDR primitives (current `internal/protocol/xdr/`) move to `internal/adapter/nfs/xdr/core/`
- RPC + GSS stays under `internal/adapter/nfs/rpc/`
- v4.1 split: `internal/adapter/nfs/v4/v41/` (no underscore) for v4.1-only handlers and types
- Shared NFS types remain at `nfs/types/` if truly shared across versions; version-specific types live in `v3/types/` and `v4/types/`. Audit during research to determine what's actually shared.
- Mount stays as peer of v3/v4 under `nfs/mount/`
- Buffer pool (`bufpool.go`) moves to `internal/adapter/pool/` (generic utility shared across adapters)
- Auth extraction moves to `internal/adapter/nfs/middleware/` (dedicated middleware package)
- Remove `nfs_` prefix from all files in `pkg/adapter/nfs/`
- Drop prefix AND flatten where possible
- `nlm_service.go` -> `service.go` inside `nlm/`; `nfs_adapter_portmap.go` -> `service.go` inside `portmap/`
- Keep `settings.go` and `shutdown.go` as separate files
- One handler file per NFS procedure (maintained)
- Drop `_handler` suffix from v4 handler files (align with v3 pattern)
- Remove `.disabled` test files entirely (stale)
- Split files by concern first, then by version
- Aim for ~500 lines max per file
- `identity/` package placement: Claude's discretion based on import graph analysis
- Handler documentation: 5-line Godoc comment blocks above each handler function
- Dispatch consolidation: single `nfs.Dispatch()` entry point with program -> version -> procedure chain
- Each dispatch layer handles errors at its own scope (PROG_UNAVAIL, PROG_MISMATCH, PROC_UNAVAIL)
- Auth context extraction moved out of dispatch into `middleware/`
- Dispatch accepts `context.Context` as parameter
- Connection code split: common in `nfs/connection.go`, version-specific in `v3/connection.go` and `v4/connection.go`
- PROG_MISMATCH reply for unsupported NFS versions (v2, v5) with range Low=3, High=4
- NFS4ERR_MINOR_VERS_MISMATCH for unsupported minor versions (e.g., minor=2)
- PROG_UNAVAIL for unknown RPC programs
- Table-driven subtests for version negotiation
- Function-level dispatch tests (call Dispatch() with parsed structs, not wire bytes)
- Import dependency: `pkg/adapter/nfs/` imports `internal/adapter/nfs/` (not the reverse)
- Full test coverage: existing tests pass + new tests for all restructured code
- E2E test suite as final verification gate

### Claude's Discretion
- Reply/response building placement (shared nfs/ level vs per-version)
- `identity/` package location based on import graph analysis
- RFC reference format (section number vs full URL)
- Whether v3 needs its own `connection.go` (depends on how much v3-specific connection logic exists)

### Deferred Ideas (OUT OF SCOPE)
- `internal/auth/` (NTLM+SPNEGO) restructuring -> Phase 28
- SMB internal restructuring -> Phase 28
- BaseAdapter extraction -> Phase 28
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| REF-03.1 | `internal/protocol/` renamed to `internal/adapter/` (all imports updated) | Rename is a bulk `git mv` + `sed` on 312 files referencing `internal/protocol`. Current directory has 435 Go files. |
| REF-03.2 | Generic XDR moved to `internal/adapter/nfs/xdr/core/` | Current `internal/protocol/xdr/` has 4 files (decode.go, encode.go, types.go, union.go). Imported by 89+ files in v4/handlers, v4/attrs, v4/types, v4/state, nlm/xdr, nsm/xdr, nlm/handlers. |
| REF-03.3 | NLM, NSM, portmapper consolidated under `internal/adapter/nfs/` | NLM: 16 files in 5 dirs. NSM: 12 files in 5 dirs. Portmap: 11 files in 4 dirs. Currently peers of `nfs/` under `internal/protocol/`. Move to be children of `nfs/`. |
| REF-03.4 | `internal/auth/` moved to `internal/adapter/smb/auth/` | **DEFERRED per CONTEXT.md** -- `internal/auth/` stays where it is. This is a Phase 28 task. |
| REF-03.5 | `pkg/adapter/nfs/` files renamed (remove `nfs_` prefix, split mixed-concern files) | 10 files with `nfs_` prefix + 1 `nlm_service.go`. Also move NLM/portmap service files. |
| REF-03.6 | v4.1 split into nested hierarchy (`v4/v41/`) | v4.1-specific handlers: 11 `*_handler.go` files. v4.1-specific types: ~25 files in `v4/types/`. v4.1 state: `v41_client.go`. Need to identify which are v4.1-only vs shared. |
| REF-03.7 | Dispatch consolidated: single `nfs.Dispatch()` entry point | Current dispatch is split across: `internal/protocol/nfs/dispatch.go` (types + auth), `dispatch_nfs.go` (v3 table), `dispatch_mount.go` (mount table), and `pkg/adapter/nfs/nfs_connection_dispatch.go` (program switch). Consolidate into single entry in `internal/adapter/nfs/dispatch.go`. |
| REF-03.8 | Connection code split by version concern | Current: `pkg/adapter/nfs/nfs_connection.go` (388 lines) has v4 backchannel demux. `nfs_connection_handlers.go` (644 lines) has v3/v4/NLM/NSM handler dispatch. Split common TCP/connection logic from version-specific handling. |
| REF-03.9 | Shared handler helpers extracted | Current: `utils.go` in dispatch package has `handleRequest` generic helper + type unions. v3 has `utils.go`, `auth_helper.go`, `nfs_context.go`, `nfs_response.go`, `verf.go`. v4 has `helpers.go`, `context.go`. Extract shared patterns to `internal/adapter/nfs/helpers.go`. |
| REF-03.10 | Handler documentation added (3-5 lines each, all v3/v4/mount handlers) | v3: 22 handlers. v4: ~40+ handlers. Mount: 6 handlers. NLM: ~8 handlers. NSM: ~6 handlers. Total: ~80+ handler functions need documentation. |
| REF-03.11 | Version negotiation tests added | Current test coverage: `dispatch_test.go` tests auth extraction + table completeness. Need: v2 reject, v5 reject, minor=2 reject, unknown program tests. `MakeProgMismatchReply` and `MakeErrorReply` already exist in `rpc/parser.go`. |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go | 1.25.0 | Language runtime | Project's go.mod specifies 1.25.0 |
| `go build ./...` | - | Build verification after each move | Catches all import errors immediately |
| `go test ./...` | - | Test verification after each move | Ensures behavior unchanged |
| `go vet ./...` | - | Static analysis | Catches common mistakes |

### Supporting
| Tool | Purpose | When to Use |
|------|---------|-------------|
| `git mv` | Rename files/directories preserving git history | Every file move |
| `sed -i '' 's/old/new/g'` | Bulk import path rewriting | After each directory rename |
| `goimports` | Fix import formatting after sed | After sed rewrites |
| `grep -r` | Find remaining old import paths | Verification after each wave |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `sed` import rewrite | `gofmt -r` | `sed` is more reliable for import path changes; `gofmt -r` is for expression rewrites |
| Manual file-by-file | `gorename` | `gorename` works for identifiers, not import paths |

## Architecture Patterns

### Recommended Final Structure

```
internal/adapter/                          # Renamed from internal/protocol/
├── nfs/
│   ├── dispatch.go                        # Top-level program switch (100003, 100005, 100021, 100024, 100000)
│   ├── connection.go                      # Shared connection logic (TCP read/write, fragment handling)
│   ├── helpers.go                         # Cross-version shared helpers (handleRequest generic)
│   ├── middleware/                         # Auth extraction (from current ExtractHandlerContext)
│   │   └── auth.go
│   ├── rpc/                               # RPC layer + GSS (unchanged internal structure)
│   │   ├── auth.go, constants.go, message.go, parser.go
│   │   └── gss/                           # GSS-API (context, framework, integrity, privacy, etc.)
│   ├── xdr/
│   │   ├── core/                          # Generic XDR primitives (from internal/protocol/xdr/)
│   │   │   ├── decode.go, encode.go, types.go, union.go
│   │   ├── attributes.go                  # NFS-specific XDR (file attrs)
│   │   ├── decode.go, encode.go           # NFS-specific XDR helpers
│   │   ├── decode_handle.go, filehandle.go
│   │   ├── errors.go, network.go, pointers.go, time.go, utils.go
│   ├── types/                             # Shared NFS types (v3 constants, TimeVal, etc.)
│   │   ├── constants.go, types.go
│   ├── v3/
│   │   ├── handlers/                      # One file per handler (22 handlers)
│   │   │   └── testing/                   # Test fixtures
│   │   └── connection.go                  # v3-specific connection logic (if needed)
│   ├── v4/
│   │   ├── handlers/                      # v4.0 + shared handlers (COMPOUND, per-op handlers)
│   │   ├── attrs/                         # Attribute encoding/decoding
│   │   ├── pseudofs/                      # Pseudo-filesystem
│   │   ├── state/                         # State management (clients, sessions, leases)
│   │   ├── types/                         # v4.0 types (compound structs, stateid, etc.)
│   │   ├── connection.go                  # v4-specific connection logic
│   │   └── v41/
│   │       ├── handlers/                  # v4.1-only handlers (moved from v4/handlers/*_handler.go)
│   │       └── types/                     # v4.1-only types (moved from v4/types/)
│   ├── mount/
│   │   └── handlers/                      # Mount protocol handlers (6 handlers)
│   ├── nlm/
│   │   ├── service.go                     # NLM service lifecycle (from pkg/adapter/nfs/nlm_service.go)
│   │   ├── handlers/
│   │   ├── blocking/
│   │   ├── callback/
│   │   ├── types/
│   │   └── xdr/
│   ├── nsm/
│   │   ├── handlers/
│   │   ├── callback/
│   │   ├── types/
│   │   └── xdr/
│   └── portmap/
│       ├── service.go                     # Portmap service lifecycle (from pkg/adapter/nfs/nfs_adapter_portmap.go)
│       ├── handlers/
│       ├── types/
│       └── xdr/
├── smb/                                   # Renamed path only, no internal restructuring
│   └── (existing structure unchanged)
└── pool/
    └── bufpool.go                         # Shared buffer pool (from internal/bufpool/)

pkg/adapter/nfs/                           # After rename (remove nfs_ prefix)
├── adapter.go                             # Was nfs_adapter.go
├── connection.go                          # Was nfs_connection.go
├── dispatch.go                            # Was nfs_connection_dispatch.go
├── handlers.go                            # Was nfs_connection_handlers.go
├── reply.go                               # Was nfs_connection_reply.go
├── settings.go                            # Was nfs_adapter_settings.go
├── shutdown.go                            # Was nfs_adapter_shutdown.go
├── identity/                              # Stays here (see identity analysis below)
│   ├── cache.go, convention.go, mapper.go, static.go, table.go
│   └── *_test.go
└── (nlm_service.go and nfs_adapter_nlm.go, nfs_adapter_portmap.go -> moved)
```

### Pattern 1: Atomic Rename + Import Rewrite

**What:** Rename a directory with `git mv`, then `sed` all imports in one pass, then verify with `go build`.
**When to use:** Every directory move in this phase.
**Example:**
```bash
# Step 1: Move the directory
git mv internal/protocol internal/adapter

# Step 2: Rewrite ALL imports in the entire repo
find . -name '*.go' -exec sed -i '' \
  's|github.com/marmos91/dittofs/internal/protocol|github.com/marmos91/dittofs/internal/adapter|g' {} +

# Step 3: Verify
go build ./...
go test ./...
```

### Pattern 2: Dispatch Consolidation (Program -> Version -> Procedure)

**What:** Single entry point that routes hierarchically.
**When to use:** The consolidated `dispatch.go` in `internal/adapter/nfs/`.
**Example:**
```go
// Dispatch routes an RPC call to the appropriate NFS ecosystem handler.
// Top-level switch on program number, each program delegates to version-specific dispatch.
func Dispatch(ctx context.Context, call *rpc.RPCCallMessage, data []byte, deps *DispatchDeps) (*HandlerResult, error) {
    switch call.Program {
    case rpc.ProgramNFS:    // 100003
        return dispatchNFS(ctx, call, data, deps)
    case rpc.ProgramMount:  // 100005
        return dispatchMount(ctx, call, data, deps)
    case rpc.ProgramNLM:    // 100021
        return dispatchNLM(ctx, call, data, deps)
    case rpc.ProgramNSM:    // 100024
        return dispatchNSM(ctx, call, data, deps)
    case rpc.ProgramPortmap: // 100000
        return dispatchPortmap(ctx, call, data, deps)
    default:
        return nil, ErrProgUnavail
    }
}

func dispatchNFS(ctx context.Context, call *rpc.RPCCallMessage, data []byte, deps *DispatchDeps) (*HandlerResult, error) {
    switch call.Version {
    case rpc.NFSVersion3:
        return v3.Dispatch(ctx, call, data, deps.V3Handler)
    case rpc.NFSVersion4:
        return v4.Dispatch(ctx, call, data, deps.V4Handler)
    default:
        return nil, &ProgMismatchError{Low: 3, High: 4}
    }
}
```

### Pattern 3: Handler Documentation Template

**What:** 5-line Godoc block above each handler.
**When to use:** All handler functions.
**Example:**
```go
// HandleRead handles NFS READ (RFC 1813 Section 3.3.6).
// Reads up to `count` bytes from file at `offset`.
// Delegates to MetadataService for attr lookup, BlockService for data.
// Returns file data + post-op attrs. Updates atime on success.
// Errors: NFS3ERR_STALE (bad handle), NFS3ERR_ACCES (no read perm).
func (h *Handler) Read(ctx *NFSHandlerContext, req *ReadRequest) (*ReadResponse, error) {
```

### Anti-Patterns to Avoid
- **Big bang rename**: Moving everything at once and trying to fix imports later. Do it in waves with build verification between each.
- **Changing behavior during restructuring**: This phase is ONLY about moving files and renaming imports. Zero logic changes.
- **Circular imports**: The constraint `pkg/adapter/nfs/ imports internal/adapter/nfs/` (not reverse) must be maintained. Moving NLM/portmap service files from `pkg/` to `internal/` requires care.
- **Losing git history**: Use `git mv` rather than delete + create to preserve file history.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Import path rewriting | Manual edit of 312 files | `sed -i '' 's/old/new/g'` + `goimports` | Error-prone and slow manually |
| Build verification | Spot checks | `go build ./...` after every move | Must catch all breakage |
| Test verification | Run individual tests | `go test ./...` | Must verify no behavioral regression |

**Key insight:** This is a purely mechanical refactoring. The risk is only in missing an import rewrite or creating a circular dependency. Go's compiler catches both immediately.

## Common Pitfalls

### Pitfall 1: Import Alias Conflicts After Rename
**What goes wrong:** After renaming `internal/protocol/nfs` to `internal/adapter/nfs`, files that already import `pkg/adapter/nfs` will have two packages named `nfs`. Go requires explicit aliasing.
**Why it happens:** The `pkg/adapter/nfs/` package currently uses `package nfs`. After the rename, `internal/adapter/nfs/` also uses `package nfs`. Files importing both need aliases.
**How to avoid:** In files that import both packages, use explicit aliases. The current codebase already does this: `nfs "github.com/marmos91/dittofs/internal/protocol/nfs"` aliased as `nfs` while `pkg/adapter/nfs` is the default package. After rename, continue this pattern: e.g., `nfs_internal "github.com/marmos91/dittofs/internal/adapter/nfs"`.
**Warning signs:** `go build` error about "imported and not used" or "ambiguous import".

### Pitfall 2: Circular Dependencies When Moving NLM/Portmap Services
**What goes wrong:** `pkg/adapter/nfs/nlm_service.go` and `nfs_adapter_portmap.go` are in `pkg/adapter/nfs/` and contain service lifecycle code. Moving them to `internal/adapter/nfs/nlm/` and `internal/adapter/nfs/portmap/` may create circular imports because they reference `NFSAdapter` struct from `pkg/adapter/nfs/`.
**Why it happens:** The NLM service references the adapter's runtime, and the adapter references the NLM service. Tight coupling.
**How to avoid:** Keep the service integration code in `pkg/adapter/nfs/` (just rename files to drop prefix). Move only the protocol-level service registration to `internal/adapter/nfs/{nlm,portmap}/service.go`. Or define interfaces to break the cycle.
**Warning signs:** `go build` error about import cycles.

### Pitfall 3: v4.1 Split - Shared vs v4.1-Only Code
**What goes wrong:** Moving a handler that's used by both v4.0 and v4.1 to `v4/v41/` breaks v4.0 dispatch.
**Why it happens:** Some v4.1 handlers are registered in `v41DispatchTable` but call shared helpers from `v4/handlers/`.
**How to avoid:** The v4.1-only handlers are clearly identified by the `_handler.go` suffix and `V41OpHandler` signature. These are the ones to move: `exchange_id_handler.go`, `create_session_handler.go`, `destroy_session_handler.go`, `destroy_clientid_handler.go`, `bind_conn_to_session_handler.go`, `backchannel_ctl_handler.go`, `sequence_handler.go`, `reclaim_complete_handler.go`, `free_stateid_handler.go`, `get_dir_delegation_handler.go`, `test_stateid_handler.go`. Shared handlers (access.go, read.go, write.go, etc.) stay in `v4/handlers/`.
**Warning signs:** `go build` error about undefined references.

### Pitfall 4: Generic XDR Package Path Change Breaks Many Files
**What goes wrong:** `internal/protocol/xdr` is imported by 89+ files across v4/handlers, v4/attrs, v4/types, v4/state, nlm/xdr, nsm/xdr. Moving to `internal/adapter/nfs/xdr/core/` changes the import path AND the package alias.
**Why it happens:** Current import is `"github.com/marmos91/dittofs/internal/protocol/xdr"` with package name `xdr`. New path will have package name `core` (since the directory is `core/`). All call sites using `xdr.DecodeUint32()` would need to change to `core.DecodeUint32()` OR the package declaration must remain `package xdr`.
**How to avoid:** In the `core/` directory, keep `package xdr` as the package declaration (Go allows package name to differ from directory name). This way, all existing call sites (`xdr.DecodeUint32()`) continue to work with just the import path change.
**Warning signs:** "undefined: xdr" errors after move.

### Pitfall 5: Test Files With Build Tags or Fixtures
**What goes wrong:** Test files in `testing/` subdirectory or with build constraints may not be found after move.
**Why it happens:** `v3/handlers/testing/fixtures.go` uses a specific test package. Moving the parent changes the import path.
**How to avoid:** Move test helper packages together with their parent. Verify with `go test ./...` after each move.

### Pitfall 6: Dispatch State Currently on Connection Struct
**What goes wrong:** The dispatch consolidation goal is to have a single `nfs.Dispatch()` entry point, but the current dispatch logic is methods on `NFSConnection` struct (in `pkg/adapter/nfs/`), which has access to `c.server` (adapter state), `c.conn` (TCP connection), `c.connectionID`, etc.
**Why it happens:** The dispatch is tightly coupled to the connection lifecycle (GSS context, reply writing, metrics).
**How to avoid:** The consolidated `Dispatch()` in `internal/adapter/nfs/` should accept a dependency struct (or interface) rather than coupling to `NFSConnection`. The `NFSConnection.handleRPCCall()` method becomes a thin wrapper that: (1) does GSS interception, (2) calls `nfs.Dispatch()`, (3) writes the reply. The actual program/version/procedure routing moves to the internal dispatch package.

## Code Examples

### Import Rewrite Pattern (sed)
```bash
# After git mv internal/protocol internal/adapter
# Rewrite all imports across the codebase
find . -name '*.go' -not -path './vendor/*' -exec sed -i '' \
  's|github.com/marmos91/dittofs/internal/protocol/|github.com/marmos91/dittofs/internal/adapter/|g' {} +

# After moving nlm from internal/adapter/nlm to internal/adapter/nfs/nlm
find . -name '*.go' -not -path './vendor/*' -exec sed -i '' \
  's|github.com/marmos91/dittofs/internal/adapter/nlm|github.com/marmos91/dittofs/internal/adapter/nfs/nlm|g' {} +
```

### NLM/NSM/Portmap Move Into NFS
```bash
# Move peer protocols under nfs/
git mv internal/adapter/nlm internal/adapter/nfs/nlm
git mv internal/adapter/nsm internal/adapter/nfs/nsm
git mv internal/adapter/portmap internal/adapter/nfs/portmap

# Rewrite imports
find . -name '*.go' -exec sed -i '' \
  's|internal/adapter/nlm|internal/adapter/nfs/nlm|g; s|internal/adapter/nsm|internal/adapter/nfs/nsm|g; s|internal/adapter/portmap|internal/adapter/nfs/portmap|g' {} +
```

### Generic XDR Move With Package Name Preservation
```bash
# Create target directory
mkdir -p internal/adapter/nfs/xdr/core

# Move files
git mv internal/adapter/xdr/decode.go internal/adapter/nfs/xdr/core/decode.go
git mv internal/adapter/xdr/encode.go internal/adapter/nfs/xdr/core/encode.go
git mv internal/adapter/xdr/types.go internal/adapter/nfs/xdr/core/types.go
git mv internal/adapter/xdr/union.go internal/adapter/nfs/xdr/core/union.go

# IMPORTANT: Keep package declaration as `package xdr` in moved files
# (Go allows package name != directory name)

# Rewrite imports
find . -name '*.go' -exec sed -i '' \
  's|github.com/marmos91/dittofs/internal/adapter/xdr|github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core|g' {} +
```

### Version Negotiation Test Pattern
```go
func TestDispatch_VersionNegotiation(t *testing.T) {
    tests := []struct {
        name       string
        program    uint32
        version    uint32
        wantStatus uint32 // Expected RPC accept_stat
        wantLow    uint32 // For PROG_MISMATCH: minimum supported version
        wantHigh   uint32 // For PROG_MISMATCH: maximum supported version
    }{
        {
            name:       "NFS v2 rejected with PROG_MISMATCH",
            program:    rpc.ProgramNFS,
            version:    2,
            wantStatus: rpc.RPCProgMismatch,
            wantLow:    3,
            wantHigh:   4,
        },
        {
            name:       "NFS v5 rejected with PROG_MISMATCH",
            program:    rpc.ProgramNFS,
            version:    5,
            wantStatus: rpc.RPCProgMismatch,
            wantLow:    3,
            wantHigh:   4,
        },
        {
            name:       "Unknown program rejected with PROG_UNAVAIL",
            program:    999999,
            version:    1,
            wantStatus: rpc.RPCProgUnavail,
        },
        {
            name:       "NFS v3 accepted",
            program:    rpc.ProgramNFS,
            version:    3,
            wantStatus: rpc.RPCSuccess,
        },
        {
            name:       "NFS v4 accepted",
            program:    rpc.ProgramNFS,
            version:    4,
            wantStatus: rpc.RPCSuccess,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            call := &rpc.RPCCallMessage{
                Program:   tt.program,
                Version:   tt.version,
                Procedure: 0, // NULL
            }
            result, err := Dispatch(ctx, call, nil, deps)
            // Assert based on wantStatus...
        })
    }
}
```

### Handler Documentation Example (v3)
```go
// Read handles NFS READ (RFC 1813 Section 3.3.6).
// Reads up to `count` bytes from file at `offset`.
// Delegates to MetadataService for attr lookup, BlockService for data.
// Returns file data + post-op attrs. Updates atime on success.
// Errors: NFS3ERR_STALE (bad handle), NFS3ERR_ACCES (no read perm).
func (h *Handler) Read(ctx *NFSHandlerContext, req *ReadRequest) (*ReadResponse, error) {

// Mkdir handles NFS MKDIR (RFC 1813 Section 3.3.9).
// Creates a new directory in the parent identified by `dir` handle.
// Delegates to MetadataService for directory creation with initial attrs.
// Returns new dir handle + attrs + dir WCC data.
// Errors: NFS3ERR_EXIST (name taken), NFS3ERR_ACCES (no write perm on parent).
func (h *Handler) Mkdir(ctx *NFSHandlerContext, req *MkdirRequest) (*MkdirResponse, error) {
```

### Handler Documentation Example (v4.1)
```go
// handleCreateSession handles NFSv4.1 CREATE_SESSION (RFC 8881 Section 18.36).
// Creates a new session for the client, establishing slot table and back-channel.
// Delegates to StateManager for session creation and seqid validation.
// Caches response bytes for replay detection. Auto-binds originating connection.
// Errors: NFS4ERR_STALE_CLIENTID, NFS4ERR_SEQ_MISORDERED, NFS4ERR_RESOURCE.
func (h *Handler) handleCreateSession(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
```

## State of the Art

| Current State | Target State | Impact |
|--------------|--------------|--------|
| `internal/protocol/` with flat NLM/NSM/portmap peers | `internal/adapter/nfs/` with nested NFS ecosystem | Clean hierarchy reflecting protocol relationships |
| Dispatch split across 4+ files in 2 packages | Single `Dispatch()` entry point | Easier to understand request routing |
| `nfs_` prefix on all pkg/adapter/nfs files | Package name serves as prefix | Idiomatic Go naming |
| v4.1 handlers mixed with v4.0 handlers | Separate `v4/v41/handlers/` directory | Clear version boundaries |
| No handler documentation | 5-line Godoc on every handler | Better maintainability |
| No version negotiation tests | Table-driven tests for all edge cases | Prevents regression |

## Research Findings: Identity Package Placement

**Analysis:** `pkg/adapter/nfs/identity/` is currently imported by:
- `internal/protocol/nfs/v4/handlers/handler.go` (Handler struct field `IdentityMapper`)
- Various v4 handler files that use `identity.IdentityMapper`

The import direction is `internal/protocol/ -> pkg/adapter/nfs/identity/`. After rename, this becomes `internal/adapter/nfs/v4/handlers/ -> pkg/adapter/nfs/identity/`. This is `internal/ -> pkg/` which is fine (internal can import public packages).

**Recommendation:** Keep `identity/` in `pkg/adapter/nfs/identity/` (no move needed). It's a public API that internal code uses, and the current import direction is correct. Moving it into `internal/adapter/nfs/` would mean `pkg/adapter/nfs/` needs to import `internal/adapter/nfs/identity/`, but `pkg/ -> internal/` imports are already the established pattern here.

## Research Findings: v3 Connection File

**Analysis:** Current `pkg/adapter/nfs/nfs_connection.go` (388 lines) contains:
- `NFSConnection` struct definition with v4 backchannel fields (`pendingCBReplies *state.PendingCBReplies`)
- `Serve()` method with v4 backchannel demux logic
- `readRequest()` with v4 REPLY message routing
- `processRequest()` (generic, handles all programs)
- `readFragmentHeader()`, `readRPCMessage()` (generic RPC framing)
- `handleUnsupportedVersion()` (generic)
- `handleConnectionClose()` (generic with v4.1 connection unbinding)

The v3-specific connection logic is minimal -- there's no v3-specific connection state beyond what's shared. v4 has significant connection state (backchannel, connection ID, pending replies).

**Recommendation:** Split into:
- `connection.go` (~250 lines): Shared NFSConnection struct (without v4 fields), Serve(), readFragmentHeader(), readRPCMessage(), handleUnsupportedVersion(), basic processRequest()
- `connection_v4.go` (~150 lines): v4 backchannel demux, SetPendingCBReplies(), connection close v4.1 unbinding, v4-specific Serve() modifications

A separate `v3/connection.go` is NOT needed -- v3 has no version-specific connection logic.

## Research Findings: NFS Types Audit

**Analysis:** `internal/protocol/nfs/types/` contains:
- `constants.go`: NFSv3 procedure numbers (NFSProcNull through NFSProcCommit), NFS3 error codes (NFS3OK through NFS3ErrBadType), NFSv3 file type constants
- `types.go`: NFSv3 wire format types (TimeVal, FileAttributes, WccData, etc.)

These are exclusively v3 types. The v4 types are in `v4/types/`. There are no types shared between v3 and v4 in this package.

**Recommendation:** The `types/` package at `nfs/types/` can stay as-is since it's only imported by v3 code and the dispatch layer. It could be moved to `v3/types/` for consistency, but that would break the existing dispatch_nfs.go imports that reference `types.NFSProcNull`. The user decision says "audit during research" -- the audit shows these are v3-only but used at the dispatch level. Keeping at `nfs/types/` is simplest.

## Research Findings: bufpool Consumers

**Analysis:** `internal/bufpool/` is imported by 4 files:
- `internal/protocol/nfs/v3/handlers/read_payload.go`
- `internal/protocol/nfs/v3/handlers/read.go`
- `pkg/adapter/nfs/nfs_connection.go`
- `pkg/adapter/smb/smb_connection.go`

Both NFS and SMB import it. The user decided to move it to `internal/adapter/pool/`. This is correct -- it's used by both adapters and should be a shared utility.

## Research Findings: File Count Impact

| Directory | Files | Lines (approx) | Action |
|-----------|-------|-----------------|--------|
| `internal/protocol/nfs/` (core) | 8 | ~1200 | Rename path + reorganize |
| `internal/protocol/nfs/mount/` | 11 | ~600 | Rename path |
| `internal/protocol/nfs/rpc/` | 18 | ~2000 | Rename path |
| `internal/protocol/nfs/types/` | 2 | ~200 | Rename path |
| `internal/protocol/nfs/v3/` | 70 | ~6000 | Rename path + add docs |
| `internal/protocol/nfs/v4/` | 100 | ~15000 | Rename path + v4.1 split + add docs |
| `internal/protocol/nfs/xdr/` | 13 | ~800 | Rename path |
| `internal/protocol/nlm/` | 17 | ~1500 | Move under nfs/ |
| `internal/protocol/nsm/` | 12 | ~800 | Move under nfs/ |
| `internal/protocol/portmap/` | 11 | ~800 | Move under nfs/ |
| `internal/protocol/smb/` | 37 | ~4000 | Rename path only |
| `internal/protocol/xdr/` | 4 | ~300 | Move to nfs/xdr/core/ |
| `internal/bufpool/` | 2 | ~200 | Move to adapter/pool/ |
| `pkg/adapter/nfs/` | 11 | ~3350 | Rename files, move NLM/portmap service |
| **External importers** | **18** | - | Update import paths |
| **Total files needing import updates** | **~312** | - | sed rewrite |

## Open Questions

1. **Dispatch dependency injection pattern**
   - What we know: The consolidated `Dispatch()` needs access to v3 handler, v4 handler, mount handler, NLM handler, NSM handler, metrics, rate limiter. Currently these are fields on `NFSAdapter` and `NFSConnection`.
   - What's unclear: Whether to use a struct of dependencies (`DispatchDeps`) or individual parameters or an interface.
   - Recommendation: Use a `DispatchDeps` struct. It's the simplest approach and avoids interface ceremony for what's an internal API.

2. **Reply building placement**
   - What we know: `handleRequest` generic helper + `rpc.MakeReply()` are used for response construction. v4 has its own COMPOUND encoding. User marked this as Claude's discretion.
   - Recommendation: Keep reply building in a shared `helpers.go` at `internal/adapter/nfs/` level. The generic `handleRequest[Req, Resp]` template works for v3 and mount. v4 has its own compound encoding. No need to split.

3. **v4.1 handler registration after split**
   - What we know: v4.1 handlers are registered in `v41DispatchTable` inside `handler.go:NewHandler()`. After moving to `v4/v41/handlers/`, the registration must still happen in the v4 handler init.
   - Recommendation: `v4/v41/handlers/` exports handler functions. `v4/handlers/handler.go:NewHandler()` imports them and registers in `v41DispatchTable`. This maintains the current registration pattern with just a different import path.

## Sources

### Primary (HIGH confidence)
- Codebase analysis: `internal/protocol/` directory structure (435 Go files)
- Codebase analysis: `pkg/adapter/nfs/` directory structure (11 Go files)
- Codebase analysis: Import graph (312 files reference `internal/protocol`)
- `internal/protocol/CLAUDE.md` - Protocol layer documentation
- `pkg/adapter/CLAUDE.md` - Adapter layer documentation
- `docs/CORE_REFACTORING_PLAN.md` - Core refactoring plan (Phase 1 relevant)

### Secondary (MEDIUM confidence)
- Go documentation on package naming conventions (package name can differ from directory name)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - standard Go tooling, no external dependencies
- Architecture: HIGH - codebase fully audited, all import paths mapped
- Pitfalls: HIGH - all pitfalls verified against actual import graph and file structure
- v4.1 split: MEDIUM - handler identification is clear, but type file split between v4.0/v4.1 needs per-file audit during implementation

**Research date:** 2026-02-25
**Valid until:** 2026-03-27 (stable codebase, no external dependency changes)
