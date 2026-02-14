# Phase 6: NFSv4 Protocol Foundation - Context

**Gathered:** 2026-02-12
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement the NFSv4 compound operation dispatcher, pseudo-filesystem, XDR types, and error mapping. This is the wire protocol backbone that all subsequent NFSv4 phases build on. Phase 6 ships an incrementally testable v4 endpoint where COMPOUND works but most ops return NFS4ERR_NOTSUPP — real file operations come in Phase 7.

</domain>

<decisions>
## Implementation Decisions

### Port & Adapter Strategy
- Extend the existing NFS adapter (not a separate adapter). Single adapter handles v3+v4 on the same port
- Keep port 12049 as default (no change to standard port)
- Version routing happens in `dispatch.go` — check RPC version field, route to v3 or v4 dispatch table
- NLM/NSM/MOUNT programs stay always active regardless of NFS version (they're identified by program number, independent of NFS version)
- Shared HandlerResult type between v3 and v4 (add v4-specific fields if needed)
- Minimal connection state struct for v4 (placeholder with client address) — Phase 9 will extend it
- Separate v4 handler struct in `internal/protocol/nfs/v4/handlers/` (not shared with v3)

### Pseudo-Filesystem Design
- Direct mapping: share `/export` appears at pseudo-fs path `/export`
- Dynamic pseudo-fs: reflects runtime share additions/removals immediately
- Separate handle space for pseudo-fs: in-memory `map[path]handle`, runtime resolves handle source before dispatch
- Show all shares in READDIR on pseudo-fs root — access control enforced at LOOKUP/operation time (matches Linux nfsd, Synology)
- PUTPUBFH = PUTROOTFH = pseudo-fs root (same handle, most common approach)
- NFS4ERR_STALE for handles pointing to removed shares (not NFS4ERR_MOVED)
- Pseudo-fs lives in `internal/protocol/nfs/v4/pseudofs/` (NFS v4-specific, no reason to extract)
- Claude's Discretion: virtual directory visibility in READDIR, filesystem boundary crossing mechanism (follow RFC 7530 Section 7.4)

### Version Negotiation
- Both v3 and v4 active simultaneously by default (different clients or same client can use either)
- Ship incrementally: Phase 6 enables v4 dispatch, COMPOUND works, most ops return NFS4ERR_NOTSUPP
- Validate minor version now: accept minor=0 only, reject 1/2 with NFS4ERR_MINOR_VERS_MISMATCH
- Version range (min/max) configured via control plane API at runtime (NOT in static config file)
- Default: min=3, max=4 (maximum compatibility)
- Log at INFO level the first time a client uses v3 or v4, subsequent calls at DEBUG
- Claude's Discretion: RPC-level response when client sends disallowed version

### COMPOUND Operation Model
- Mutable CompoundContext struct passed by pointer through handlers (CurrentFH, SavedFH, auth context)
- Map-based dispatch table for COMPOUND sub-ops (same pattern as existing v3 NfsDispatchTable)
- Op handlers receive raw XDR bytes + CompoundContext, return HandlerResult (same as v3 pattern)
- Tag field: echo as-is in response, no interpretation
- Define all NFSv4 attribute bits, return only supported ones in response bitmask (unsupported bits simply not set)
- Dynamic response buffer growth (bytes.Buffer approach, not pre-allocation)
- Claude's Discretion: COMPOUND op count limit, error tracking (op index in response)

### Code Structure
- Mirror v3 structure: `internal/protocol/nfs/v4/handlers/` with types in `v4/types/`, attrs in `v4/attrs/`
- Common NFS errors package: `internal/protocol/nfs/errors/` for shared error interface, specialized in v3/types and v4/types
- Common package: `internal/protocol/nfs/common/` for shared types, auth context, error mapping
- Use shared `internal/protocol/xdr/` for XDR primitives, add v4 compound-specific helpers
- One handler per file: `putfh.go`, `lookup.go`, `getattr.go` (matches v3 pattern)
- Unit tests only for Phase 6 (no RPC-level integration, no NFS mount tests)
- Defer observability (metrics/tracing) to Phase 7+ when real operations exist

</decisions>

<specifics>
## Specific Ideas

- V4 dispatch should follow the exact same pattern as existing `NfsDispatchTable` (map of procedure → handler struct)
- CompoundContext is the new concept — mutable struct with CurrentFH/SavedFH, single-goroutine execution (no concurrency concern)
- The pseudo-fs is a small in-memory tree — `map[string]uint64` (path → handle) is sufficient
- Error code structure: `nfs/errors/` (common interface) → `v3/types/` (v3-specific) → `v4/types/` (v4-specific)
- Attribute bitmask approach: define all ~80 bits, server returns only supported ones. Client infers unsupported features. Standard NFSv4 behavior (how Linux nfsd handles it)

</specifics>

<deferred>
## Deferred Ideas

- Portmapper auto-registration — deferred to Phase 28.1
- NFSv4 observability (metrics/tracing) — add when real operations exist (Phase 7+)
- Connection state for v4 sessions — Phase 9 (State Management) extends the placeholder

</deferred>

---

*Phase: 06-nfsv4-protocol-foundation*
*Context gathered: 2026-02-12*
