# Phase 09: Adapter layer cleanup (ADAPT) — Context

**Gathered:** 2026-04-24
**Status:** Ready for planning
**Milestone:** v0.15.0 Block Store + Core-Flow Refactor
**GH issue:** [#427](https://github.com/marmos91/dittofs/issues/427)
**Requirements:** ADAPT-01, ADAPT-02, ADAPT-03, ADAPT-04, ADAPT-05

<domain>
## Phase Boundary

Consolidate duplicated NFS/SMB adapter helpers, bring SMB READ to pool parity with NFS, unify `metadata.ExportError → NFS3ERR_* / NFS4ERR_* / STATUS_*` mapping, and prepare the call-site seam where Phase 12 (API-01) will feed `[]BlockRef` into the engine.

**Core work (from ROADMAP.md):**
1. **ADAPT-01**: New shared package `internal/adapter/common/` exposing `ResolveForRead`, `ResolveForWrite`, `readFromBlockStore` — used by NFS v3/v4 and SMB v2 handlers; per-protocol `getBlockStoreForHandle` duplication deleted.
2. **ADAPT-02**: SMB READ handler routes response-buffer allocation through `internal/adapter/pool` (4KB/64KB/1MB tiers) — no more inline `make([]byte, actualLength)`; buffers released on completion.
3. **ADAPT-03**: Single consolidated `metadata.ExportError → protocol-code` mapping table; NFS v3, NFS v4, and SMB all consume it; adding a new export error requires one edit.
4. **ADAPT-04**: Adapter call-site layout prepared to pass `[]BlockRef` into engine (structural seam; actual data flow wired in Phase 12 when `FileAttr.Blocks` is reintroduced as `[]BlockRef`).
5. **ADAPT-05**: Cross-protocol conformance test — same file operation over NFS and SMB returns consistent client-observable error codes for each `metadata.ErrorCode`.

**Out of scope for Phase 09:**
- The engine signature change itself (`engine.BlockStore.ReadAt(ctx, blocks []BlockRef, dest, offset)`) — that is API-01 in Phase 12 (A3).
- The `FileAttr.Blocks []BlockRef` schema reintroduction — META-01 in Phase 12 (A3).
- CAS/FastCDC/BLAKE3 behaviors — Phases 10–11 (A1–A2).
- Auth-flow refactor (signing, session setup, credential routing) — not related to error/block-store plumbing.
- User-facing docs beyond internal architecture notes (no README.md change; CHANGELOG deferred to v0.15.0 shipment per Phase 08 D-24).

</domain>

<decisions>
## Implementation Decisions

### Shared `common/` package (ADAPT-01)

- **D-01:** **Narrow interfaces, not `*runtime.Runtime`.** Define `BlockStoreRegistry interface { GetBlockStoreForHandle(ctx, metadata.FileHandle) (*engine.BlockStore, error) }` and `MetadataService` (narrowed to the methods helpers actually use) inside `internal/adapter/common/`. Helpers take the interface. Why: testable without booting the runtime, explicit contract, no circular-import risk between `common/` and `runtime/`. The small plumbing cost (interface declarations in common/, satisfied implicitly by `*runtime.Runtime`) is worth the decoupling.
- **D-02:** **`ResolveForRead` / `ResolveForWrite` are resolve-only, not permission-bundlers.** Signatures:
  - `ResolveForRead(ctx context.Context, reg BlockStoreRegistry, handle metadata.FileHandle) (*engine.BlockStore, error)`
  - `ResolveForWrite(ctx context.Context, reg BlockStoreRegistry, handle metadata.FileHandle) (*engine.BlockStore, error)`

  `metaSvc.PrepareRead` / `PrepareWrite` stay at the call site because they return protocol-specific shapes (`ReadMetadata.Attr` for SMB EOF logic, `WriteOperation` with pre/post attrs for NFSv3 WCC) that each protocol consumes differently. Bundling them would force a widest-common return shape that adds friction.
- **D-03:** **`AuthContext` is an explicit helper parameter.** When a helper needs auth (e.g., the bundled `readFromBlockStore` takes auth-scoped context for quota/permission echo), the signature takes `authCtx *metadata.AuthContext`. Callers (NFS `dispatch.go:ExtractAuthContext` per-request; SMB session layer) build it themselves. Honors the CLAUDE.md invariant "every operation carries an *metadata.AuthContext" without coupling `common/` to protocol-specific `NFSHandlerContext` / `SMBHandlerContext` types (which would defeat D-01).
- **D-04:** **`readFromBlockStore` keeps the `Release()` pattern verbatim.** Move the existing NFSv3 `blockReadResult` struct with `Release()` method from `internal/adapter/nfs/v3/handlers/read_payload.go:16-28` to `internal/adapter/common/` unchanged. Callers `defer result.Release()` (when they own the lifetime) or hand the release closure off to the response encoder (SMB — see D-11). Proven pattern, already coupled to `pool.Put` correctly. SMB READ adopts it as-is.
- **D-05:** **All three protocols migrate in Phase 09: NFSv3 + NFSv4 + SMB v2.** Matches the ROADMAP success criterion "used by both NFS v3/v4 and SMB v2 handlers". NFSv4's local `getBlockStoreForHandle` (`internal/adapter/nfs/v4/handlers/helpers.go:102`) is a 3-line duplicate — easy migration — and doing it now prevents Phase 12 (which touches all adapters for META-01/API-01) from having to do adapter dedup work as side scope.

### Consolidated error-mapping table (ADAPT-03)

- **D-06:** **Struct-per-code table with NFS3/NFS4/SMB columns.**
  ```go
  var errorMap = map[metadata.ErrorCode]struct {
      NFS3 uint32
      NFS4 uint32
      SMB  types.Status
  }{
      metadata.ErrNotFound:      {NFS3: NFS3ErrNoEnt,  NFS4: NFS4ErrNoEnt,  SMB: StatusObjectNameNotFound},
      metadata.ErrAccessDenied:  {NFS3: NFS3ErrAccess, NFS4: NFS4ErrAccess, SMB: StatusAccessDenied},
      ...
  }
  ```
  Exposes three mappers as thin accessors: `MapToNFS3(err) uint32`, `MapToNFS4(err) uint32`, `MapToSMB(err) types.Status`. Adding a new `metadata.ErrorCode` = one new map entry populating all three columns at once. Literally satisfies ADAPT-03's "one edit, not two" criterion.
- **D-07:** **NFSv4 codes included in the table in Phase 09.** Since D-05 has all three protocols adopting `common/` helpers, NFSv4's `NFS4ERR_*` codes fold into the same table as single-source-of-truth. Audit current NFSv4 error mapping location during planning (likely in `internal/adapter/nfs/v4/handlers/` or types package) and migrate. Risk: the NFSv4 mapping may surface inconsistencies vs. NFSv3 (e.g., NFSv4 has finer-grained error codes for delegation/state); those are planning-time findings documented in the plan, not live bugs.
- **D-08:** **Consolidation scope = meta + content + lock translators.** The three translators that move into `common/`:
  1. **Metadata errors**: `MetadataErrorToSMBStatus` (`internal/adapter/smb/v2/handlers/converters.go:358`) + `mapMetadataErrorToNFS` (`internal/adapter/nfs/v3/handlers/create.go:579`) + NFSv4 equivalent — consolidate into the struct table above.
  2. **Content errors**: `ContentErrorToSMBStatus` (`internal/adapter/smb/v2/handlers/converters.go:401`) and any NFS content-error mapper — consolidate into a parallel smaller table keyed on block-store error type.
  3. **Lock errors**: `ErrLocked`, `ErrLockNotFound`, `ErrLockConflict`, `ErrDeadlock`, `ErrGracePeriod`, `ErrLockLimitExceeded` → protocol codes. SMB's richer lock-error taxonomy (`STATUS_FILE_LOCK_CONFLICT` vs `STATUS_LOCK_NOT_GRANTED`) documented in table comments.

  **Out of scope:** auth/signing errors (e.g., SMB's `STATUS_ACCESS_DENIED` for bad MAC in `internal/adapter/smb/framing.go:360`, `compound.go:616`). Those are protocol-wire failures, not error translations — pulling them in muddies the abstraction.

### SMB READ pool integration (ADAPT-02)

- **D-09:** **Release after response encoding, not defer in `Read()`.** The handler returns a `ReadResponse` whose `Data` field carries the pooled slice. The framing/encoding layer (`internal/adapter/smb/response.go`) returns the buffer to the pool AFTER writing to the socket. Matches NFSv3's existing pattern. Correct by construction — buffer stays valid until wire-write completes, including through any compound-response assembly.

  Implementation hint for planner: `SMBResponseBase` grows an optional `ReleaseData func()` field; the encoder calls it after `SendResponse`. Non-pooled responses leave it nil — no behavior change. Protocol dispatch doesn't need to know about pool semantics.
- **D-10:** **All READ variants route through pool: regular file, pipe, symlink.** Uniform handler shape; future maintainers don't need to remember "file yes, pipe no". Yes, pipe and symlink buffers are small (pool rounds up to the 4KB small tier), but:
  - The overhead of `pool.Get(4KB) / pool.Put` on a cold path is negligible.
  - Consistency wins: one release mechanism for all READ responses.
  - Prevents future contributors from accidentally reintroducing `make([]byte, n)` on pipe/symlink paths thinking they're exempt.
- **D-11:** **Over-cap reads use direct allocation via `pool.Get` (no handler code change).** `pool.Get` already falls through to `make([]byte, size)` when `size > LargeSize` (bufpool.go:157-162); `pool.Put` silently ignores undersized/oversized buffers. Current DittoFS advertises `MaxReadSize = 1048576` (1 MB — `internal/adapter/smb/v2/handlers/handler.go:361`), which is exactly the pool's LargeSize tier — so over-cap path is dormant today, but the fallback is the safety net if a future phase raises `MaxReadSize` to SMB 3.1.1's 8 MB ceiling.

  **Do not** bump LargeSize to 8 MB speculatively — `sync.Pool` is per-P, meaning a 16-core machine would pin ~128 MB of idle pool memory for an optimization that doesn't fire at current negotiated cap. If a future perf profile shows large reads dominating, revisit then with data.

  Document this behavior in `common/`'s package Godoc so the contract is explicit.

### `[]BlockRef` groundwork (ADAPT-04)

- **D-12:** **Phase 09 ships call-site refactor only; Phase 12 wires actual `[]BlockRef` data.** Rationale: Phase 08 deleted `FileAttr.Blocks []string` (TD-03); Phase 12 (META-01) reintroduces it as `[]BlockRef`; Phase 12 (API-01) changes `engine.BlockStore.ReadAt` / `WriteAt` signatures to accept `[]BlockRef`. In Phase 09, the field doesn't exist and the engine signature hasn't changed — nothing concrete to pass.

  **What Phase 09 delivers for ADAPT-04:**
  - Extract NFS/SMB READ and WRITE call sites through `common/` helpers so the "fetch `FileAttr.Blocks` → slice to [offset, offset+len) → hand to engine" logic will land in exactly one place (`common/readFromBlockStore` and its write twin) in Phase 12.
  - Call sites continue to speak the current `engine.BlockStore.ReadAt(ctx, payloadID, dest, offset)` contract — which matches what the wire protocols actually carry (SMB2/NFS3/NFS4 READ all communicate `offset + length`, never block identity). Protocol fidelity preserved.
  - Phase 12 changes `common/`'s helper body; all protocol handler code stays untouched.

  Why not the `ReadAtV2` shim: adding a second engine method taking `[]BlockRef` with `nil` placeholder leaks internal abstraction up into handler code prematurely, and every call site would change twice (once here with `nil`, again in Phase 12 with real blocks). One edit > two edits.

  Why not defer ADAPT-04 entirely to Phase 12: the call-site refactor *is* the groundwork; doing it now sets up Phase 12 to touch exactly the internals of `common/` and nothing else. Losing it means Phase 12 does both the adapter-layer dedup and the signature change in one phase, widening blast radius.

### Cross-protocol conformance test (ADAPT-05)

- **D-13:** **Full 27-code coverage, split across two test tiers.**
  - **E2E tier (~18 triggerable codes):** Extend `test/e2e/cross_protocol_test.go` — starts real dfs server, mounts NFS and SMB shares, triggers each error condition, asserts kernel errno / NT status matches the mapping table. Codes triggerable without exotic fixtures:
    `ErrNotFound`, `ErrAccessDenied`, `ErrAlreadyExists`, `ErrNotEmpty`, `ErrIsDirectory`, `ErrNotDirectory`, `ErrInvalidArgument`, `ErrNoSpace`, `ErrReadOnly`, `ErrStaleHandle`, `ErrNameTooLong`, `ErrIOError`, `ErrInvalidHandle`, `ErrNotSupported`, `ErrAuthRequired`, `ErrPermissionDenied`, `ErrLocked`, `ErrLockNotFound`.
  - **Unit tier (~9 exotic codes):** Handler-level unit test that injects the error via a mocked `MetadataService`, asserts the handler's response carries the mapped code. Covers: `ErrConnectionLimitReached`, `ErrLockLimitExceeded`, `ErrDeadlock`, `ErrGracePeriod`, `ErrPrivilegeRequired`, `ErrQuotaExceeded`, `ErrLockConflict`, `ErrNotSupported` edge cases, and any residual codes not exercised in e2e. Fast, runs in `go test ./...`, no fixture complexity.

  Both tiers are table-driven from the same `common/` error table. Adding a new `metadata.ErrorCode` = one new row in `common/` + one new test case in the appropriate tier (e2e if triggerable, unit otherwise). The one-edit contract extends into test maintenance.
- **D-14:** **"Consistent" = each protocol returns the code specified for that protocol in the common/ table.** Not "semantic category matches" (too loose — masks real bugs), not "byte-for-byte identical wire bytes" (impossible — NFS and SMB are different protocols). The assertion is: `MapToNFS3(ErrNotFound) == NFS3ErrNoEnt` AND `MapToSMB(ErrNotFound) == StatusObjectNameNotFound` AND both are actually what kernels receive when the scenario fires.

### Structure & process

- **D-15:** **Two-PR split: helpers+pool+errors, then []BlockRef seam + conformance test.**
  - **PR-A (ADAPT-01/02/03):** New `common/` package; migrate NFSv3 + NFSv4 + SMB helpers to it; consolidated error-mapping table with three-column struct; SMB READ pool integration; response-encoding release plumbing. Low-risk mechanical consolidation — no semantic behavior change, purely dedup + one new allocator path.
  - **PR-B (ADAPT-04/05):** Call-site refactor through `common/readFromBlockStore` / `writeToBlockStore` for all READ/WRITE paths (the seam Phase 12 will feed); cross-protocol conformance test (27-code coverage in two tiers); docs updates (D-17).

  Each PR ships green independently. PR-A unblocks Phase 10/11 work that touches error mapping. PR-B can be delayed if Phase 12 timing shifts, without stranding PR-A. Matches Phase 08's PR-A/B/C discipline but scaled to Phase 09's smaller surface.
- **D-16:** **Atomic per-ADAPT-NN commits within each PR.** PR-A has 3 logical commits (ADAPT-01 helpers, ADAPT-02 pool, ADAPT-03 errors) plus any necessary setup commits; PR-B has 2 (ADAPT-04 call-site refactor, ADAPT-05 conformance test). Each commit independently green so `git bisect` works cleanly. Planner may subdivide further if reviewability demands (e.g., split ADAPT-03 into "table + NFSv3 migration" / "NFSv4 migration" / "SMB migration" if one commit grows large).
- **D-17:** **Docs updated: ARCHITECTURE, NFS, SMB.**
  - `docs/ARCHITECTURE.md` — add `internal/adapter/common/` to the directory map with a one-line role description ("Shared NFS/SMB adapter helpers: block-store resolution, pooled read buffer, consolidated `metadata.ExportError → protocol-code` mapping"); brief subsection under "Adapter layer" explaining the consolidation.
  - `docs/NFS.md` — add/update "Error mapping" section pointing to `common/` as the canonical translator; note NFSv3 and NFSv4 both consume the same table.
  - `docs/SMB.md` — same for SMB; mention the pool integration for READ responses.
  - `docs/CONTRIBUTING.md` — **Claude's discretion** (not explicitly scoped, but a "Adding a new metadata.ErrorCode" recipe pointing at common/ + conformance test would codify the one-edit contract; planner may include as a small addition).
  - `README.md` — no change (Phase 09 is internal refactor, zero user-visible behavior change).
  - CHANGELOG — deferred to v0.15.0 shipment (matches Phase 08 D-24).

### Claude's Discretion

- **Exact narrow-interface definition in `common/`** (D-01) — the planner chooses which metadata-service methods get narrowed (probably `PrepareRead`, `PrepareWrite`, `GetFile`, `CheckLockForIO`) based on actual call-site usage audit.
- **Where `common/` lives within `internal/adapter/`** (D-01) — flat package vs `common/errmap/` + `common/resolve/` subfolders. Planner decides based on file count after migration.
- **Exact `ReleaseData func()` field placement in `SMBResponseBase`** (D-09) — or an alternative encoder-level release registry if that turns out cleaner.
- **Whether `common/readFromBlockStore` wraps or inlines the existing NFSv3 body** (D-04) — planner picks based on whether any protocol-specific logger calls need to stay at the call site.
- **Commit subdivision inside PR-A and PR-B** (D-16) — above the per-ADAPT-NN floor; planner splits further only where it improves reviewability.
- **`docs/CONTRIBUTING.md` "Adding a new metadata.ErrorCode" recipe** (D-17) — include if the touched-docs diff stays small; otherwise defer.

### Folded Todos

None — no pending todos from the backlog matched Phase 09 scope.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents (researcher, planner, executor) MUST read these before acting.**

### Roadmap & Requirements
- `.planning/ROADMAP.md` §"Phase 09: Adapter layer cleanup (ADAPT)" (lines 75-97) — success criteria, files to touch, key risks
- `.planning/REQUIREMENTS.md` §"Adapter cleanup (ADAPT)" (lines 100-106) — ADAPT-01..ADAPT-05 text
- `.planning/REQUIREMENTS.md` §"Traceability" — ADAPT-01..05 all mapped to Phase 09 / GH #427
- `.planning/PROJECT.md` §"Current Milestone: v0.15.0" — milestone core value
- `.planning/STATE.md` §"v0.15.0 Decisions" — parallel-track rule (Phase 08 ∥ Phase 09)

### Predecessor / Successor Phase Context
- `.planning/phases/08-pre-refactor-cleanup-a0/08-CONTEXT.md` — Phase 08 is the parallel track; sets the pattern for per-requirement atomic commits (D-11 in that doc), the scaffolding-removal posture, and the v0.13.0 backup-paths already deleted (so Phase 09 never has to coordinate with backup code).
- `.planning/ROADMAP.md` §"Phase 12" (lines 151-176) — consumes Phase 09's groundwork via META-01 (`FileAttr.Blocks []BlockRef`) + API-01 (engine signature) + adapter call-site consumption at line 170.

### Project invariants (architectural constraints that bind Phase 09)
- `CLAUDE.md` §"Architecture invariants" — rules 1, 2, 3, 4, 6 directly apply:
  - Rule 1: "Protocol handlers handle only protocol concerns" — `common/` centralizes what's shared, handlers stay thin.
  - Rule 2: "Every operation carries an `*metadata.AuthContext`" — D-03 formalizes this at the helper boundary.
  - Rule 3: "File handles are opaque" — `ResolveForRead/Write` treats handles as opaque input.
  - Rule 4: "Block stores are per-share" — helpers route via `Registry.GetBlockStoreForHandle`.
  - Rule 6: "Error codes: return `metadata.ExportError` values" — ADAPT-03's table is the translation layer.

### Source files to read (Phase 09 work)

**ADAPT-01 migration targets (helpers to consolidate):**
- `internal/adapter/nfs/v3/handlers/utils.go:56-60` — `getBlockStoreForHandle(reg, ctx, handle)` (NFSv3 duplicate)
- `internal/adapter/nfs/v3/handlers/read_payload.go:16-70` — `blockReadResult` + `readFromBlockStore` (canonical source for common/)
- `internal/adapter/nfs/v4/handlers/helpers.go:100-106` — `getBlockStoreForHandle` (NFSv4 duplicate, 3-line wrapper)
- `internal/adapter/smb/v2/handlers/read.go:234` — inline `h.Registry.GetBlockStoreForHandle` (SMB call site)
- `internal/adapter/smb/v2/handlers/write.go:292`, `close.go:183`, `close.go:518`, `close.go:558` — additional SMB call sites

**ADAPT-02 target (SMB READ pool):**
- `internal/adapter/smb/v2/handlers/read.go:342` — `data := make([]byte, actualLength)` (the allocation to pool)
- `internal/adapter/smb/response.go` — framing/encoding path where release fires (D-09)
- `internal/adapter/pool/bufpool.go` — existing pool API; `pool.Get` fallback at lines 157-162 (D-11 relies on this)

**ADAPT-03 mapping consolidation:**
- `internal/adapter/nfs/v3/handlers/create.go:577-621` — `mapMetadataErrorToNFS` (full NFSv3 switch)
- `internal/adapter/smb/v2/handlers/converters.go:354-406` — `MetadataErrorToSMBStatus` + `ContentErrorToSMBStatus`
- `internal/adapter/nfs/status_string.go` — NFSv3 code names (for test assertions)
- `internal/adapter/smb/types/status.go` — SMB status codes
- `pkg/metadata/errors.go:22-50` — canonical `metadata.Err*` enum (what the table keys on)
- `pkg/metadata/errors/errors.go` — underlying error package
- NFSv4 error code location — **to be audited during planning**; likely `internal/adapter/nfs/v4/handlers/` or `internal/adapter/nfs/v4/types/`

**ADAPT-04 call-site seam:**
- `internal/adapter/nfs/v3/handlers/read.go`, `write.go`, `commit.go` — READ/WRITE/COMMIT call sites
- `internal/adapter/nfs/v4/handlers/read.go`, `write.go`, `commit.go` — same for NFSv4
- `internal/adapter/smb/v2/handlers/read.go`, `write.go`, `close.go` (for flush) — SMB call sites
- `pkg/blockstore/engine/engine.go` — current `ReadAt`/`WriteAt` signatures (unchanged in Phase 09; changed in Phase 12)

**ADAPT-05 test target:**
- `test/e2e/cross_protocol_test.go` — existing XPR-01..06 interop framework (pattern to extend)
- `test/e2e/framework/`, `test/e2e/helpers/` — fixture utilities
- `pkg/metadata/errors.go` — enumerate 27 codes to drive table-driven test cases

### Docs affected
- `docs/ARCHITECTURE.md` — directory map + "Adapter layer" subsection (D-17)
- `docs/NFS.md` — error mapping section (D-17)
- `docs/SMB.md` — error mapping + pool notes (D-17)
- `docs/CONTRIBUTING.md` — Claude's discretion per D-17

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- **`internal/adapter/pool/bufpool.go`** — Tiered 4KB/64KB/1MB pool with transparent large-alloc fallback. `pool.Get(size)` + `pool.Put(buf)`. Already used by NFSv3 `readFromBlockStore`. SMB READ integration (ADAPT-02) is just switching `make([]byte, n)` to `pool.Get(int(n))` and wiring `pool.Put` on the response path.
- **`internal/adapter/nfs/v3/handlers/read_payload.go:16-70`** — Canonical `blockReadResult{data, eof}` + `Release()` + `readFromBlockStore` pattern. Moves to `common/` unchanged (D-04).
- **`pkg/metadata/errors.go:22-50`** — 27 `metadata.Err*` codes; the enum `common/` error table keys on. Stable surface.
- **`test/e2e/cross_protocol_test.go`** — Real-server + NFS/SMB kernel-client cross-protocol test. XPR-01..06 pattern; ADAPT-05 extends with error-code conformance cases.
- **`pkg/controlplane/runtime/runtime.go` `*Runtime.GetBlockStoreForHandle`** — The method the new `BlockStoreRegistry` interface wraps (D-01). Already the single entrypoint all protocols hit.

### Established Patterns
- **Per-share block store resolution via handle** (CLAUDE.md invariant 4) — `Registry.GetBlockStoreForHandle(ctx, handle)` is the canonical entrypoint. Both protocols already use it; Phase 09 just stops duplicating the wrapper.
- **Error translation via `errors.As(err, &*metadata.StoreError)`** — both NFSv3 and SMB do this identically; the struct-per-code table in `common/` encapsulates this pattern.
- **`SMBResponseBase` / `NFSResponseBase` as response envelopes** — carry `Status` field + payload. Adding a `ReleaseData func()` to `SMBResponseBase` (D-09) fits the existing envelope pattern.
- **Table-driven e2e tests** — `test/e2e/cross_protocol_test.go` uses subtests; ADAPT-05 extends the existing pattern.
- **Atomic per-requirement commits** — established by Phase 08's D-11 / D-31; Phase 09 D-16 follows suit.

### Integration Points
- **Phase 12 (A3) consumes this phase**: `common/readFromBlockStore` / `writeToBlockStore` become where `FileAttr.Blocks` fetch + slice lands (META-01 + API-01). Phase 09's call-site refactor is explicitly structured to make that Phase 12 change a one-package edit.
- **Runtime layer is untouched**: `*runtime.Runtime` gains no new methods; Phase 09 is pure adapter-layer consolidation. The narrow interface in `common/` is satisfied implicitly by the existing `*Runtime`.
- **NFSv4 state/lease code is untouched**: Phase 09 consolidates READ/WRITE helpers and error mapping only; delegation, locking, recall paths stay as-is.

</code_context>

<specifics>
## Specific Ideas

- **Align with "real SMB 3.1.1 and NFS3/4 behaviour"** (user guidance on ADAPT-04, 2026-04-24): Wire protocols communicate `(offset, length)` — they do not know about blocks. `[]BlockRef` is internal DittoFS plumbing between adapter and engine. The chosen call-site-refactor-only approach preserves protocol fidelity: NFS/SMB handlers continue to receive `(offset, length)` from the wire and hand `(offset, length)` to the engine in Phase 09; Phase 12 changes `common/`'s internals (fetch `FileAttr.Blocks`, slice to the requested range, pass resolved refs) without any protocol handler code change.

- **"One edit" contract is literal**: ADAPT-03's success criterion "adding a new export error requires one edit, not two" is taken seriously by D-06's struct-per-code table. All three protocols are columns of the same row — you cannot add the key and forget a protocol, because the type system requires all three columns.

- **Pool over-cap fallback is intentional non-work**: D-11 explicitly says "no handler code change" for over-cap reads. `pool.Get` already does the right thing. Documenting it in `common/`'s Godoc is the deliverable, not code.

</specifics>

<deferred>
## Deferred Ideas

- **Metrics / observability for adapter error translation and pool hit/miss**: User declined to scope this into Phase 09. Revisit in a later observability-focused phase if adapter error translation or pool utilization becomes a performance or debugging concern.
- **Auth-flow refactor (signing, session setup, credential routing)**: Not raised as a gray area; out of scope. Any future cleanup of SMB auth/signing lives in a distinct phase.
- **`pool.LargeSize` bump to 8 MB to match SMB 3.1.1 MaxReadSize ceiling**: D-11 defers this. Revisit only if perf metrics show large reads (> 1 MB per request) dominating a real workload.
- **Bumping `MaxReadSize` / `MaxWriteSize` / `MaxTransactSize` beyond 1 MB**: Separate tuning decision; out of scope for Phase 09 (pure refactor).
- **Consolidating auth/signing error paths into the `common/` table**: D-08 explicitly excludes these. They are protocol-wire failures, not metadata translations.
- **Widening ADAPT-03 to handle non-`metadata.ErrorCode` errors (generic Go errors from standard library, etc.)**: Out of scope; the table keys on `metadata.ErrorCode` exclusively.

</deferred>

---

*Phase: 09-adapter-layer-cleanup-adapt*
*Context gathered: 2026-04-24*
