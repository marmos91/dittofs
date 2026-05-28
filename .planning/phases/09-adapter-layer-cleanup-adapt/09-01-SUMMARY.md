---
phase: 09-adapter-layer-cleanup-adapt
plan: 01
subsystem: adapter
tags: [nfs, smb, block-store, buffer-pool, refactor]

# Dependency graph
requires:
  - phase: 08-pre-refactor-cleanup-a0
    provides: "TD-01/02/03/04 cleanup of pre-refactor debt; v0.13.0 backup scaffolding removed"
provides:
  - "internal/adapter/common/ package: BlockStoreRegistry narrow interface + ResolveForRead/ResolveForWrite"
  - "internal/adapter/common/ReadFromBlockStore: pooled read helper with Release() lifecycle"
  - "NFSv3, NFSv4, SMB v2 adapters unified on common.ResolveFor* for block-store resolution"
  - "NFSv4 READ now pool-backed for parity with NFSv3 (perf delta: per-request alloc -> pool reuse)"
  - "Phase-12 seam in one place (common/) for []BlockRef landing (META-01 + API-01)"
affects:
  - 09-02 (SMB READ pool integration on top of this foundation)
  - 09-03 (error-mapping consolidation using same narrow-interface pattern)
  - 09-04 (call-site seam ready for Phase-12 []BlockRef data flow)
  - 12 (engine API signature change lands in common/, handlers untouched)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Narrow-interface decoupling: common/ declares BlockStoreRegistry, *runtime.Runtime satisfies implicitly"
    - "Exported BlockReadResult field names (Data/EOF) at package boundary"
    - "Inline nil-Registry guard at NFSv4 call sites (keeps common/ minimal)"

key-files:
  created:
    - internal/adapter/common/resolve.go
    - internal/adapter/common/read_payload.go
    - internal/adapter/common/doc.go
  modified:
    - internal/adapter/nfs/v3/handlers/utils.go
    - internal/adapter/nfs/v3/handlers/read.go
    - internal/adapter/nfs/v3/handlers/commit.go
    - internal/adapter/nfs/v4/handlers/helpers.go
    - internal/adapter/nfs/v4/handlers/read.go
    - internal/adapter/nfs/v4/handlers/write.go
    - internal/adapter/nfs/v4/handlers/commit.go
    - internal/adapter/smb/v2/handlers/read.go
    - internal/adapter/smb/v2/handlers/write.go
    - internal/adapter/smb/v2/handlers/close.go
  deleted:
    - internal/adapter/nfs/v3/handlers/read_payload.go

key-decisions:
  - "Kept nil-Registry guard inline at each NFSv4 call site rather than folding into common.ResolveFor* (avoids leaking NFSv4-ism into common/)"
  - "NFSv3 write.go keeps getServicesForHandle wrapper; wrapper body now calls common.ResolveForWrite (single edit point, callers unchanged)"
  - "Preserved per-protocol structured logging at call sites (clientIP, handle bytes) — common/ cannot couple to protocol-specific log fields (D-03)"

patterns-established:
  - "Narrow-interface-over-runtime: common/ never imports pkg/controlplane/runtime; satisfied implicitly"
  - "Pooled-read lifecycle: BlockReadResult.Release() fires after response has been encoded (NFSv3 existing Releaser path, NFSv4 defer in handler)"
  - "Phase-12 seam documented in common/doc.go so successor phases have the contract in-source"

requirements-completed: [ADAPT-01]

# Metrics
duration: ~25 min
completed: 2026-04-24
---

# Phase 09 Plan 01: Shared adapter helpers (ADAPT-01) Summary

**Shared `internal/adapter/common/` package exposing narrow BlockStoreRegistry + ResolveForRead/Write + pooled ReadFromBlockStore; NFSv3/v4 + SMB v2 handlers unified on it; NFSv3 read_payload.go and both getBlockStoreForHandle duplicates deleted; NFSv4 READ gains pool parity with NFSv3.**

## Performance

- **Duration:** ~25 min
- **Started:** 2026-04-24T07:35:08Z
- **Completed:** 2026-04-24T07:35:33Z
- **Tasks:** 3
- **Files modified:** 11 (3 created, 10 modified, 1 deleted)

## Accomplishments

- Created `internal/adapter/common/` (3 files): narrow `BlockStoreRegistry` interface + `ResolveForRead` / `ResolveForWrite` resolvers + verbatim-moved NFSv3 pooled `ReadFromBlockStore` with exported `BlockReadResult{Data, EOF}`.
- Deleted both duplicate `getBlockStoreForHandle` bodies (NFSv3 `utils.go` and NFSv4 `helpers.go`) and the entire NFSv3 `read_payload.go` file.
- Migrated every in-scope call site across three protocol adapters:
  - NFSv3: `read.go` (resolution + pooled read), `commit.go` (resolution), `utils.go`'s two internal helpers (`getServicesForHandle`, `readMFsymlinkContentForNFS`).
  - NFSv4: `read.go` (resolution + pool adoption), `write.go` (resolution), `commit.go` (resolution).
  - SMB v2: `read.go`, `write.go`, `close.go` (three sites incl. MFsymlink read + delete paths).
- NFSv4 READ now allocates its response buffer through `internal/adapter/pool` (via `common.ReadFromBlockStore`) — behavior change noted in commit message. Releases via `defer readResult.Release()` after `encodeRead4resok` (which copies the bytes into a fresh `bytes.Buffer`).
- `common/doc.go` documents the Phase-12 `[]BlockRef` seam (D-12) and the pool over-cap fallback (D-11) so later phases have the contract in-source.
- Full `go build ./...`, `go vet ./...`, and `go test -race -count=1 ./...` all green (76 test packages pass).

## Task Commits

All three tasks merged into a single atomic commit per plan D-16:

1. **Tasks 1+2+3 (atomic per-ADAPT-NN)** — `c03d12c7` (adapter/refactor) — `adapter(common): extract shared block-store helpers (ADAPT-01)`

Signed commit; no Claude Code mentions; no Co-Authored-By lines.

## Files Created/Modified

**Created:**
- `internal/adapter/common/resolve.go` — BlockStoreRegistry narrow interface + ResolveForRead/ResolveForWrite.
- `internal/adapter/common/read_payload.go` — BlockReadResult{Data,EOF} + Release() + ReadFromBlockStore (verbatim NFSv3 move, exported field names).
- `internal/adapter/common/doc.go` — Package Godoc covering D-11 pool over-cap fallback and D-12 Phase-12 []BlockRef seam.

**Modified:**
- `internal/adapter/nfs/v3/handlers/utils.go` — Deleted getBlockStoreForHandle body; added common import; migrated two internal callers (getServicesForHandle → common.ResolveForWrite; readMFsymlinkContentForNFS → common.ResolveForRead).
- `internal/adapter/nfs/v3/handlers/read.go` — Migrated line 144 resolution to common.ResolveForRead and line 237 read to common.ReadFromBlockStore; renamed fields Data/EOF; preserved call-site structured logging (clientIP, handle).
- `internal/adapter/nfs/v3/handlers/commit.go` — Migrated line 198 to common.ResolveForWrite; added common import.
- `internal/adapter/nfs/v4/handlers/helpers.go` — Deleted getBlockStoreForHandle body; trimmed now-unused imports (context, engine).
- `internal/adapter/nfs/v4/handlers/read.go` — Migrated resolution to common.ResolveForRead (with inline nil-Registry guard per D-03); adopted pool via common.ReadFromBlockStore; added defer readResult.Release() after encode; imported fmt for log formatting.
- `internal/adapter/nfs/v4/handlers/write.go` — Migrated both resolution call sites to common.ResolveForWrite (inline nil-Registry guard).
- `internal/adapter/nfs/v4/handlers/commit.go` — Migrated resolution to common.ResolveForWrite (inline nil-Registry guard).
- `internal/adapter/smb/v2/handlers/read.go` — Migrated line 234 to common.ResolveForRead (pool integration on line 342 deferred to plan 02 per scope).
- `internal/adapter/smb/v2/handlers/write.go` — Migrated line 292 to common.ResolveForWrite.
- `internal/adapter/smb/v2/handlers/close.go` — Migrated three sites: line 183 flush (Write), line 518 readMFsymlinkContent (Read), line 558 delete-on-close (Write).

**Deleted:**
- `internal/adapter/nfs/v3/handlers/read_payload.go` — Body moved verbatim into common/read_payload.go; only one external caller (v3 read.go) updated.

## Decisions Made

- **Inline nil-Registry guard at each NFSv4 call site** — The existing NFSv4 `getBlockStoreForHandle` had a `h.Registry == nil` guard; the plan explicitly called out preferring inline at call site over folding into `common.ResolveFor*`. Kept inline at NFSv4 `read.go`, `write.go`, `commit.go` with a short `logger.Debug` and NFS4ERR_SERVERFAULT return. Rationale: keeps common/ minimal and avoids leaking a per-protocol concern into shared code.
- **NFSv3 `write.go` write-path resolution via `getServicesForHandle` wrapper** — The plan suggested migrating direct call sites in write.go, but in the current codebase the two resolution points are both routed through `getServicesForHandle` (utils.go). Migrating the wrapper body to `common.ResolveForWrite` gives a single edit point; the callers (write.go, create.go, etc.) are unchanged. Satisfies the acceptance grep `common.ResolveForWrite` at the directory level without duplicating imports across leaf files.
- **NFSv4 READ pool release timing** — Used `defer readResult.Release()` immediately after the error branch (so it fires on the return path). `encodeRead4resok` writes `readResult.Data` into a fresh `bytes.Buffer` via `xdr.WriteXDROpaque`, so by the time the defer runs the bytes have been copied into the response envelope; release is race-free and correct.
- **Out-of-scope SMB call sites preserved** — `handler.go:775`, `ioctl_copychunk.go:362/375`, `flush.go:213`, `durable_scavenger.go:151` still call `h.Registry.GetBlockStoreForHandle` directly. These files are not in the plan's `files_modified` list; per scope-boundary rules they were left alone. Plan 02 or a later plan may migrate them.

## Deviations from Plan

None material — the plan executed as written. One minor drift worth noting:

**Plan line references vs actual file state**
- Plan mentioned NFSv3 `write.go` call sites at lines 163 and 243, and `commit.go` line 116. Actual positions in the current `develop`-based source: `write.go` line 163 (single site, routed through `getServicesForHandle`); `commit.go` line 198. No new call sites found — the file evolved between planning and execution. The migration was performed on the real call sites; the acceptance grep (`common.ResolveForWrite`) passes at the directory level.
- Plan mentioned SMB `write.go` call sites at lines 292, 349, 371 — lines 349 and 371 are `MetadataErrorToSMBStatus` calls (error mapping, ADAPT-03 territory — plan 03), not block-store resolution. Only line 292 was migrated, correctly matching plan 01 scope.

No Rule 1/2/3 auto-fixes were needed; no Rule 4 architectural decisions; no authentication gates.

## Issues Encountered

None.

## User Setup Required

None — internal refactor, zero user-visible behavior change.

## Next Phase Readiness

- Plan 02 (ADAPT-02) can now layer SMB READ pool integration on top: `h.Registry.GetBlockStoreForHandle` on SMB `read.go:234` is already `common.ResolveForRead`; plan 02 adds the `make([]byte, actualLength)` → `common.ReadFromBlockStore` switch plus the `SMBResponseBase.ReleaseData` encoder hook (D-09). The foundation is in place.
- Plan 03 (ADAPT-03) will introduce the `common/errmap.go` struct-per-code table following the same narrow-interface pattern.
- Plan 04 (ADAPT-04) Phase-12 seam is documented in `common/doc.go`; no further handler work needed until Phase 12 (META-01 + API-01) changes `common.ReadFromBlockStore`'s body to fetch `FileAttr.Blocks` and pass `[]BlockRef`.
- Phase 12's blast radius is now strictly internal to `common/`; handler code stays untouched.

No blockers or concerns.

## Self-Check: PASSED

Verified:
- `internal/adapter/common/resolve.go` exists (commit c03d12c7).
- `internal/adapter/common/read_payload.go` exists (commit c03d12c7).
- `internal/adapter/common/doc.go` exists (commit c03d12c7).
- `internal/adapter/nfs/v3/handlers/read_payload.go` deleted (commit c03d12c7).
- Commit `c03d12c7` in `git log`; signed; no Claude mentions.
- `go build ./...` green; `go vet ./...` green; `go test -race -count=1 ./...` green (76 packages).

---
*Phase: 09-adapter-layer-cleanup-adapt*
*Completed: 2026-04-24*
