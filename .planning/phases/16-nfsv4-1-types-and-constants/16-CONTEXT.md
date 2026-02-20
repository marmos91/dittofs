# Phase 16: NFSv4.1 Types and Constants - Context

**Gathered:** 2026-02-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Define all NFSv4.1 wire types, operation numbers, error codes, and XDR structures (ops 40-58, CB ops 5-14) per RFC 8881. Add v4.1 detection to the COMPOUND dispatcher (routing to an empty v4.1 dispatch table that returns NFS4ERR_NOTSUPP for all ops). No handler implementations — those come in Phases 17-24.

</domain>

<decisions>
## Implementation Decisions

### File Organization
- Add v4.1 types to the **existing `internal/protocol/nfs/v4/types/` package** — no separate v41/ package
- **Per-operation files** for types: `exchange_id.go`, `create_session.go`, `sequence.go`, `destroy_session.go`, etc. Each file contains request + response XDR structs and their encode/decode methods
- **Shared sub-types** (ChannelAttrs, StateProtect, ServerOwner, NfsImplId, etc.) go in `session_common.go`
- **Constants** (op numbers, error codes) added to the **existing `constants.go` and `errors.go`** files, separated by a `// --- NFSv4.1 ... ---` comment block
- v4.1 handlers (in later phases) go in the **existing `v4/handlers/` tree** — no separate v41/handlers/ directory
- Update `internal/protocol/CLAUDE.md` with a section on v4.0/v4.1 coexistence conventions

### XDR Codec Style
- **Hand-written** encode/decode (same as v4.0), not code-generated
- **Struct methods**: `func (a *ExchangeIdArgs) Encode(w) error` / `func (a *ExchangeIdArgs) Decode(r) error`
- Types and encode/decode **in the same file** (e.g., `exchange_id.go` has both struct definitions and codec methods)
- **Add union helpers** to the `internal/protocol/xdr/` package for discriminated unions (common in v4.1 XDR)
- Union helper abstraction level: Claude's discretion (minimal helpers vs interface pattern, based on number of unions)
- Field validation in codecs: Claude's discretion (based on where v4.0 validates)
- Each type gets **per-type unit tests** with encode/decode round-trip verification

### Type Naming
- **No prefix/suffix** for v4.1-specific types: `SessionId4`, `ChannelAttrs`, `ExchangeIdArgs`, `CreateSessionRes` — plain names
- **No version number** in operation arg/result types: `ExchangeIdArgs` not `ExchangeId41Args` (these ops only exist in v4.1)
- Operation constants follow **same OP_ pattern**: `OP_EXCHANGE_ID`, `OP_CREATE_SESSION`, `OP_SEQUENCE`
- Callback constants follow **same CB_ pattern**: `CB_LAYOUTRECALL`, `CB_NOTIFY`, `CB_SEQUENCE`
- Error codes follow **same NFS4ERR_ pattern**: `NFS4ERR_BACK_CHAN_BUSY`, `NFS4ERR_CONN_NOT_BOUND_TO_SESSION`
- Flag/bitmask constants use **RFC names directly**: `EXCHGID4_FLAG_USE_NON_PNFS`, `SP4_NONE`, `CREATE_SESSION4_FLAG_PERSIST`

### Scope Boundary
- **All 19 v4.1 ops + 10 CB ops** get full XDR types upfront (not phased/deferred)
- **Full pNFS types** included (LAYOUTCOMMIT, LAYOUTGET, LAYOUTRETURN, GETDEVICEINFO, GETDEVICELIST) — ready for future pNFS support
- **Define v4.1 dispatch table** with placeholder handlers (each returns NFS4ERR_NOTSUPP)
- **Add v4.1 detection** to COMPOUND dispatcher: minorversion=1 routes to v4.1 dispatch table (empty for now)
- **Add NFS4_MINOR_VERSION_1** constant and **update version negotiation** to accept minorversion 0 and 1
- v4.0 type compatibility audit: Claude's discretion (assess what existing types need for v4.1 readiness)
- Regression testing approach: Claude's discretion (existing tests vs explicit regression test)

### Code Structure and Design
- **Per-operation test files**: `exchange_id_test.go`, `create_session_test.go`, etc.
- **Test fixtures file** (`testutil_test.go` or `fixtures_test.go`) with helpers for common test data (valid SessionId, ChannelAttrs with reasonable defaults) — reusable by later phases
- Golden test vs round-trip: Claude's discretion (depends on whether RFC provides usable byte examples)
- Types implement **`fmt.Stringer`** for debug/log readability
- Types implement **`XdrEncoder`/`XdrDecoder` interfaces** — enables generic codec helpers. Interface signatures: Claude's discretion based on existing xdr package
- **New v4.1 handler signature** (distinct from v4.0): includes session context
- Session context passed via **`V41RequestContext` struct** (bundled session, slot, sequence info) — extensible, clean signature

### Claude's Discretion
- XDR union helper abstraction level (minimal functions vs interface pattern)
- Whether to validate field constraints in codecs or at handler level
- Golden tests vs round-trip-only (based on RFC byte examples availability)
- v4.0 type compatibility audit (whether any existing types need modification)
- Regression test strategy (existing tests sufficient vs explicit regression test)
- XdrEncoder/XdrDecoder interface method signatures (based on existing xdr package)

</decisions>

<specifics>
## Specific Ideas

- Constants should use RFC names directly (EXCHGID4_FLAG_USE_NON_PNFS, not Go-ified equivalents) for easy cross-reference with RFC 8881
- Per-operation files should be self-contained: struct + encode/decode + sub-types specific to that operation
- session_common.go for types used by 2+ operations (ChannelAttrs, StateProtect, ServerOwner, NfsImplId, etc.)
- V41RequestContext struct should be defined in this phase even though handlers come later — it's part of the type foundation

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 16-nfsv4-1-types-and-constants*
*Context gathered: 2026-02-20*
