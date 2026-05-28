# Phase 27: NFS Adapter Restructuring - Context

**Gathered:** 2026-02-25
**Status:** Ready for planning

<domain>
## Phase Boundary

Restructure NFS adapter directory layout and consolidate dispatch. Rename `internal/protocol/` to `internal/adapter/`, reorganize NFS ecosystem protocols under `nfs/`, split v4/v4.1, consolidate dispatch into a single entry point, and add handler documentation. No new features, no behavior changes.

</domain>

<decisions>
## Implementation Decisions

### Target Directory Layout
- `internal/protocol/` renamed to `internal/adapter/`
- SMB code moves with the rename (`internal/adapter/smb/`) but is NOT restructured (that's Phase 28)
- `internal/auth/` (NTLM+SPNEGO) stays where it is, deferred to Phase 28
- NLM, NSM, portmapper move under `internal/adapter/nfs/` as peers of v3/v4/mount
- Each ecosystem protocol keeps its own `xdr/` subdirectory for protocol-specific types
- Generic XDR primitives (current `internal/protocol/xdr/`) move to `internal/adapter/nfs/xdr/core/`
- RPC + GSS stays under `internal/adapter/nfs/rpc/`
- v4.1 split: `internal/adapter/nfs/v4/v41/` (no underscore) for v4.1-only handlers and types
- Shared NFS types remain at `nfs/types/` if truly shared across versions; version-specific types live in `v3/types/` and `v4/types/`. Audit during research to determine what's actually shared.
- Mount stays as peer of v3/v4 under `nfs/mount/` (separate RPC program 100005)
- Buffer pool (`bufpool.go`) moves to `internal/adapter/pool/` — generic utility shared across adapters
- Auth extraction moves to `internal/adapter/nfs/middleware/` — dedicated middleware package

Target layout:
```
internal/adapter/
├── nfs/
│   ├── dispatch.go          # Top-level program switch
│   ├── connection.go        # Shared connection logic
│   ├── helpers.go           # Cross-version shared helpers
│   ├── middleware/           # Auth extraction, future middleware
│   ├── rpc/                 # RPC layer + GSS
│   │   └── gss/
│   ├── xdr/
│   │   └── core/            # Generic XDR primitives
│   ├── types/               # Shared NFS types (if any after audit)
│   ├── v3/
│   │   ├── handlers/        # One file per handler
│   │   └── connection.go    # v3-specific connection logic (if needed)
│   ├── v4/
│   │   ├── handlers/        # One file per handler
│   │   ├── attrs/
│   │   ├── pseudofs/
│   │   ├── state/
│   │   ├── types/
│   │   ├── connection.go    # v4-specific connection logic
│   │   └── v41/
│   │       ├── handlers/    # v4.1-only handlers
│   │       └── types/       # v4.1-only types
│   ├── mount/
│   │   └── handlers/
│   ├── nlm/
│   │   ├── service.go       # NLM service lifecycle (from pkg/adapter/nfs/)
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
│       ├── service.go       # Portmap service lifecycle (from pkg/adapter/nfs/)
│       ├── handlers/
│       ├── types/
│       └── xdr/
├── smb/                     # Renamed path only, no internal restructuring
│   └── (existing structure)
└── pool/
    └── bufpool.go           # Shared buffer pool utility
```

### File Naming After Cleanup
- Remove `nfs_` prefix from all files in `pkg/adapter/nfs/`
- Drop prefix AND flatten where possible (e.g., `nfs_connection_dispatch.go` -> `dispatch.go`)
- If a file would be too large, create a subpackage — the package name serves as the prefix
- `nlm_service.go` -> `service.go` inside `nlm/` (module name is the prefix)
- `nfs_adapter_portmap.go` -> `service.go` inside `portmap/`
- Keep `settings.go` and `shutdown.go` as separate files (don't fold into adapter.go)
- One handler file per NFS procedure (current pattern, maintained)
- Drop `_handler` suffix from v4 handler files (align with v3 pattern)
- Remove `.disabled` test files entirely — they're stale
- Split files by concern first, then by version where a file would be too large
- Aim for ~500 lines max per file; split via helpers. Okay to exceed if single concern and clean code.
- `identity/` package placement: Claude's discretion based on import graph analysis

### Handler Documentation Style
- Godoc comment block above each handler function
- 5 lines covering: RFC reference, protocol semantics, delegation path, side effects, key error conditions
- Example format:
  ```go
  // HandleRead handles NFS READ (RFC 1813 §3.3.6).
  // Reads up to `count` bytes from file at `offset`.
  // Delegates to MetadataService for attr lookup, BlockService for data.
  // Returns file data + post-op attrs. Updates atime on success.
  // Errors: NFS3ERR_STALE (bad handle), NFS3ERR_ACCES (no read perm).
  ```
- v4.1 handlers explicitly mark minor version: "handles NFSv4.1 CREATE_SESSION"
- Stub/unimplemented handlers get full docs + TODO line
- Keep docs concise and maintainable — no over-documentation
- Section number only for RFC references (not full URLs) — Claude's discretion

### Dispatch Consolidation
- Single `nfs.Dispatch()` entry point in `internal/adapter/nfs/dispatch.go`
- Top-level switch on RPC program number (100003=NFS, 100005=Mount, 100021=NLM, 100024=NSM, 100000=Portmap)
- Each program delegates to its own dispatch function (e.g., `nfs.dispatchNFS()`)
- Inside each program dispatcher, switch on version (e.g., v3 -> `v3.Dispatch()`, v4 -> `v4.Dispatch()`)
- Version-specific dispatchers switch on procedure and call handlers
- Each dispatch layer handles errors at its own scope:
  - Top-level: unknown program -> RPC PROG_UNAVAIL
  - Program-level: unknown version -> RPC PROG_MISMATCH
  - Version-level: unknown procedure -> RPC PROC_UNAVAIL
- Auth context extraction moved out of dispatch into `middleware/`
- Dispatch accepts `context.Context` as parameter (explicit context threading)
- Connection code split: common in `nfs/connection.go`, version-specific in `v3/connection.go` and `v4/connection.go`
- Reply/response building: Claude's discretion on placement (shared vs per-version)

### Version Negotiation
- PROG_MISMATCH reply for unsupported NFS versions (v2, v5) with range Low=3, High=4
- NFS4ERR_MINOR_VERS_MISMATCH for unsupported minor versions (e.g., minor=2)
- PROG_UNAVAIL for unknown RPC programs
- Negotiation logic lives in the dispatch chain (each layer validates its scope)
- No separate negotiation module — dispatch IS negotiation

### Testing Approach
- Full test coverage: existing tests pass + new tests for all restructured code
- Table-driven subtests for version negotiation (v2 reject, v5 reject, minor=2 reject, unknown program)
- Function-level dispatch tests (call Dispatch() with parsed structs, not wire bytes)
- Unit tests for middleware, helpers, and split files
- E2E test suite (real NFS mounts) as final verification gate
- Import dependency: `pkg/adapter/nfs/` imports `internal/adapter/nfs/` (not the reverse)

### Claude's Discretion
- Reply/response building placement (shared nfs/ level vs per-version)
- `identity/` package location based on import graph analysis
- RFC reference format (section number vs full URL)
- Whether v3 needs its own `connection.go` (depends on how much v3-specific connection logic exists)

</decisions>

<specifics>
## Specific Ideas

- Dispatch chain mirrors the protocol hierarchy: program -> version -> procedure, each level self-contained
- "Module name is the prefix" principle for file naming (e.g., `nlm/service.go` not `nlm/nlm_service.go`)
- Buffer pool should be generic enough for both NFS and SMB adapters
- Hybrid XDR approach: generic primitives in `xdr/core/`, protocol-specific types stay co-located with their protocol

</specifics>

<deferred>
## Deferred Ideas

- `internal/auth/` (NTLM+SPNEGO) restructuring -> Phase 28
- SMB internal restructuring -> Phase 28
- BaseAdapter extraction -> Phase 28

</deferred>

---

*Phase: 27-nfs-adapter-restructuring*
*Context gathered: 2026-02-25*
