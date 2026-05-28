---
phase: 09-adapter-layer-cleanup-adapt
plan: 03
subsystem: adapter
tags: [error-mapping, consolidation, common, nfs, smb, adapt-03]

# Dependency graph
requires:
  - phase: 09-adapter-layer-cleanup-adapt
    plan: 01
    provides: "internal/adapter/common package (ADAPT-01) to host the new errmap tables"
provides:
  - "internal/adapter/common/errmap.go — struct-per-code table (NFS3/NFS4/SMB columns) covering all 25 merrs.ErrorCode values; MapToNFS3 / MapToNFS4 / MapToSMB accessors; goerrors.As unwrap on every call"
  - "internal/adapter/common/content_errmap.go — narrow content/block-store error table (D-08 §2) with MapContentToNFS3/NFS4/SMB"
  - "internal/adapter/common/lock_errmap.go — lock-context table (D-08 §3) with MapLockToNFS3/NFS4/SMB; lock-vs-general divergence for ErrLocked is explicit and tested"
  - "TestErrorMapCoverage: coverage test iterates an enumerated allErrorCodes() list and asserts every code has an errorMap row; adding a new merrs.ErrorCode without a row fails CI"
  - "Latent SMB unwrap bug fixed: common.MapToSMB uses goerrors.As so wrapped StoreErrors (fmt.Errorf %w) unwrap correctly — replaces converters.go:364's pre-consolidation type-assertion path which did not unwrap"
affects:
  - 09-04 (ADAPT-04 call-site refactor can rely on the common/ API as the single mapping seam)
  - 09-05 (ADAPT-05 cross-protocol conformance test drives the same table — one row per code is enforced by coverage test)
  - 12 (Phase 12 META-01/API-01 adapters land new error codes; one-edit contract extends naturally to future codes)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Struct-per-code mapping table keyed on merrs.ErrorCode with per-protocol columns — Go's struct literal rules guarantee completeness at compile time"
    - "Lock-vs-general context divergence via a separate lockErrorMap that falls back to errorMap, not a separate switch — consistent mental model"
    - "goerrors.As uniformly used in every accessor (not type assertion) — wrapped errors unwrap correctly across the entire adapter layer"

key-files:
  created:
    - internal/adapter/common/errmap.go
    - internal/adapter/common/content_errmap.go
    - internal/adapter/common/lock_errmap.go
    - internal/adapter/common/errmap_test.go
  modified:
    - internal/adapter/nfs/v3/handlers/create.go
    - internal/adapter/nfs/v3/handlers/link.go
    - internal/adapter/nfs/v3/handlers/lookup.go
    - internal/adapter/nfs/v3/handlers/mkdir.go
    - internal/adapter/nfs/v3/handlers/mknod.go
    - internal/adapter/nfs/v3/handlers/readdir.go
    - internal/adapter/nfs/v3/handlers/readdirplus.go
    - internal/adapter/nfs/v3/handlers/readlink.go
    - internal/adapter/nfs/v3/handlers/rmdir.go
    - internal/adapter/nfs/v3/handlers/write.go
    - internal/adapter/nfs/xdr/errors.go
    - internal/adapter/nfs/v4/types/errors.go
    - internal/adapter/nfs/v4/types/constants_test.go
    - internal/adapter/nfs/v4/handlers/access.go
    - internal/adapter/nfs/v4/handlers/commit.go
    - internal/adapter/nfs/v4/handlers/create.go
    - internal/adapter/nfs/v4/handlers/getattr.go
    - internal/adapter/nfs/v4/handlers/link.go
    - internal/adapter/nfs/v4/handlers/lookup.go
    - internal/adapter/nfs/v4/handlers/lookupp.go
    - internal/adapter/nfs/v4/handlers/open.go
    - internal/adapter/nfs/v4/handlers/read.go
    - internal/adapter/nfs/v4/handlers/readdir.go
    - internal/adapter/nfs/v4/handlers/readlink.go
    - internal/adapter/nfs/v4/handlers/remove.go
    - internal/adapter/nfs/v4/handlers/rename.go
    - internal/adapter/nfs/v4/handlers/setattr.go
    - internal/adapter/nfs/v4/handlers/verify.go
    - internal/adapter/nfs/v4/handlers/write.go
    - internal/adapter/smb/v2/handlers/converters.go
    - internal/adapter/smb/v2/handlers/converters_test.go
    - internal/adapter/smb/v2/handlers/create.go
    - internal/adapter/smb/v2/handlers/write.go
    - internal/adapter/smb/v2/handlers/read.go
    - internal/adapter/smb/v2/handlers/close.go
    - internal/adapter/smb/v2/handlers/flush.go
    - internal/adapter/smb/v2/handlers/lock.go
    - internal/adapter/smb/v2/handlers/set_info.go
    - internal/adapter/smb/v2/handlers/query_directory.go
    - internal/adapter/smb/v2/handlers/query_info.go
    - internal/adapter/smb/v2/handlers/stub_handlers.go
    - internal/adapter/smb/v2/handlers/ioctl_copychunk.go
    - internal/adapter/smb/v2/handlers/ioctl_fsctl.go

key-decisions:
  - "Deleted MapMetadataErrorToNFS4 from internal/adapter/nfs/v4/types/errors.go instead of reducing it to a common.MapToNFS4 one-liner (plan Task 2 step 3c). Rationale: common/ imports nfs/v4/types for the NFS4ERR_* constants; keeping MapMetadataErrorToNFS4 in v4/types would require v4/types to import common/, creating a cycle. The clean resolution is migrate every caller (16 files in v4/handlers) directly to common.MapToNFS4 and remove the wrapper. This is a Rule 3 deviation (blocking issue: import cycle) and documented inline in v4/types/errors.go with a note pointing readers to common/"
  - "Moved TestMapMetadataErrorToNFS4 coverage from v4/types/constants_test.go into common/errmap_test.go. The new TestMapToNFS4 iterates every errorMap row and asserts the NFS4 column, covering the same behavior in a place that matches where the code now lives"
  - "Preserved xdr/errors.go:MapStoreErrorToNFSStatus as an audit-logging wrapper (D-07, PATTERNS.md gotcha #6). Its switch body shrinks to common.MapToNFS3(err) + a condensed log.Warn / log.Error dispatch based on the code severity class. Per-code audit fidelity preserved"
  - "ErrInvalidArgument → NFS3ErrInval (not NFS3ErrIO). The old xdr/errors.go wrapper coarsened EINVAL to EIO while create.go's in-handler mapper correctly returned NFS3ErrInval. The existing TestReadLink_NotSymlink test codifies NFS3ErrInval as the correct behavior; common/errmap.go follows the handler-path authority (create.go) rather than the audit wrapper"

patterns-established:
  - "All per-protocol error translators consolidated in internal/adapter/common — future ErrorCodes go in exactly one place; the struct literal forces NFS3/NFS4/SMB to be populated together"
  - "Coverage test pattern: enumerate the canonical list of codes at the top of the test file, assert len() against an expected constant, iterate and check every row exists — catches drift in both directions (missing enumeration entry or missing map row)"
  - "Lock-context vs general-context mapping: separate lockErrorMap with fallback to errorMap, tested explicitly for ErrLocked divergence (StatusFileLockConflict general vs StatusLockNotGranted lock)"

requirements-completed: [ADAPT-03]

# Metrics
duration: ~40 min
completed: 2026-04-24
---

# Phase 09 Plan 03: Consolidated metadata error mapping (ADAPT-03) Summary

**Every `metadata.ErrorCode → protocol-code` translator (NFSv3 `mapMetadataErrorToNFS`, NFSv4 `MapMetadataErrorToNFS4`, SMB `MetadataErrorToSMBStatus` + `ContentErrorToSMBStatus` + `lockErrorToStatus`) consolidated into a single struct-per-code table in `internal/adapter/common/errmap.go` with NFS3/NFS4/SMB columns, plus parallel tables for content errors (`content_errmap.go`, D-08 §2) and lock-context errors (`lock_errmap.go`, D-08 §3). Coverage test (`TestErrorMapCoverage`) fails CI when a new `merrs.ErrorCode` is added without a row. Latent SMB unwrap bug (`converters.go:364` type assertion) fixed as a side effect — common/ uniformly uses `goerrors.As`. 25 `merrs.ErrorCode` values, 25 rows in `errorMap`, 9 rows in `lockErrorMap`, two signed commits, 44 files modified, full `go test -race ./...` green.**

## Performance

- **Duration:** ~40 min
- **Started:** 2026-04-24T10:00:00Z (approx)
- **Completed:** 2026-04-24T10:22:00Z (approx)
- **Tasks:** 3 (TDD for Task 1 RED/GREEN, mechanical migration for Task 2, atomic commits for Task 3)
- **Files modified:** 44 (4 created, 40 modified)

## Accomplishments

- **`internal/adapter/common/errmap.go` (NEW)**: `protoCodes` struct + `errorMap` map covering all 25 `merrs.ErrorCode` values × NFS3/NFS4/SMB columns. Three thin accessors (`MapToNFS3`, `MapToNFS4`, `MapToSMB`) use `goerrors.As` uniformly. Inline comments document three-way drift findings per code.
- **`internal/adapter/common/content_errmap.go` (NEW)**: D-08 §2 parallel table for block-store content errors. `MapContentToNFS3`, `MapContentToNFS4`, `MapContentToSMB` — all typed errors (`blockstore.ErrRemoteUnavailable`) and the unknown-fallback map to I/O-class codes per protocol. The string-matching "cache full" heuristic is intentionally kept OUT of common/ — it lives at the NFSv3 call site as a transition fallback per PATTERNS.md gotcha.
- **`internal/adapter/common/lock_errmap.go` (NEW)**: D-08 §3 lock-context table. `lockErrorMap` maps the same `merrs.ErrorCode` values to lock-operation codes (`NFS4ERR_DENIED`, `StatusLockNotGranted`, `NFS3ErrJukebox`) that differ from general-context mappings. `MapLockToNFS3/NFS4/SMB` chain: `lockErrorMap` → `errorMap` → `defaultCodes`, so lock-context callers get correct codes for lock-specific errors and still get consistent mappings for general errors (`ErrNotFound` → `StatusFileClosed` in lock context; `ErrLocked` → `StatusLockNotGranted` in lock context, `StatusFileLockConflict` in general context).
- **`internal/adapter/common/errmap_test.go` (NEW)**: 10 tests covering all three tables. `TestErrorMapCoverage` enumerates every `merrs.ErrorCode` at the top of the file, asserts length matches 25 (catches drift if a new code is added without updating enumeration), and asserts every code has a row. `TestMapToNFS3/NFS4/SMB` are table-driven over every row in `errorMap`. `TestMapToSMB` includes the wrapped-StoreError Test D that exercises the `goerrors.As` unwrap path (fixes the pre-consolidation SMB type-assertion bug). `TestMapLockToSMB` includes Test H (lock-vs-general divergence for `ErrLocked`).
- **NFSv3 migration**: `mapMetadataErrorToNFS` at `internal/adapter/nfs/v3/handlers/create.go:577-621` deleted. 9 call sites across create/link/lookup/mkdir/mknod/readdir/readdirplus/readlink/rmdir/write migrated to `common.MapToNFS3`. Unused `"errors"` import removed from `create.go` after deletion.
- **NFSv3 audit wrapper**: `xdr/errors.go:MapStoreErrorToNFSStatus` preserved (D-07, PATTERNS.md gotcha #6) with its switch body replaced by `common.MapToNFS3(err)` + a compact per-severity log dispatch (Error for NoSpace/IOError server-side faults; Warn for everything else). Per-code log fields (`code`, `message`, `path`, `client`) preserved. `MapContentErrorToNFSStatus` unchanged — the string-matching heuristic stays at the NFSv3 call site per plan.
- **NFSv4 migration**: `MapMetadataErrorToNFS4` deleted from `internal/adapter/nfs/v4/types/errors.go` (not reduced to a wrapper — see "Deviations" below). 16 files in `v4/handlers/*` migrated from `types.MapMetadataErrorToNFS4(err)` to `common.MapToNFS4(err)`. `TestMapMetadataErrorToNFS4` removed from `constants_test.go` (coverage moved to `common/errmap_test.go:TestMapToNFS4`); unused `goerrors` and `errors` imports trimmed from the test file.
- **SMB migration**: `MetadataErrorToSMBStatus` and `ContentErrorToSMBStatus` deleted from `internal/adapter/smb/v2/handlers/converters.go`. `lockErrorToStatus` deleted from `lock.go`. 13 files in `smb/v2/handlers/*` migrated to `common.MapToSMB` / `common.MapContentToSMB` / `common.MapLockToSMB`. Leftover note in `read.go` about "plan 03 swaps the function name" cleaned up. `TestMetadataErrorToSMBStatus_NilError` removed from `converters_test.go` (nil handling now covered in `common/errmap_test.go`).
- **Latent SMB bug fixed**: `converters.go:364`'s `err.(*metadata.StoreError)` type assertion did not unwrap wrapped errors. `common.MapToSMB` uses `goerrors.As` — Test D in `common/errmap_test.go` explicitly exercises `fmt.Errorf("wrap: %w", &merrs.StoreError{Code: merrs.ErrNotFound})` and asserts `StatusObjectNameNotFound`. This is a behavior-positive change (more wrapped errors now unwrap correctly) and is called out in the commit message.
- **Tests all green**: `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...` all pass. Specifically verified: `./internal/adapter/common/...`, `./internal/adapter/nfs/...`, `./internal/adapter/smb/...`, `./pkg/metadata/...`.

## Task Commits

Two signed commits (plan D-16 suggested 1-3 commits; I split into 2 for clean review — table creation + migration are separately bisectable):

1. **Task 1 (common/ tables + tests)** — `9ad31e3b` — `adapter(common): add errmap tables (ADAPT-03)`
2. **Task 2 + 3 (call-site migration + atomic commit)** — `9c0aa787` — `adapter: migrate error mapping to common (ADAPT-03)`

Both signed with RSA key SHA256:ADuGa4QCr9JgRW9b88cSh1vU3+heaIMjMPmznghPWT8. No Claude Code mentions. No Co-Authored-By lines.

## Files Created/Modified

**Created (4):**
- `internal/adapter/common/errmap.go` — struct-per-code table + MapTo* accessors (25 rows)
- `internal/adapter/common/content_errmap.go` — content-error table (D-08 §2)
- `internal/adapter/common/lock_errmap.go` — lock-error table (D-08 §3; 9 rows)
- `internal/adapter/common/errmap_test.go` — coverage + table-driven tests (10 tests, all table-driven from the maps themselves)

**Modified (40):**
- NFSv3 (10 files): `v3/handlers/{create,link,lookup,mkdir,mknod,readdir,readdirplus,readlink,rmdir,write}.go` — all callers of `mapMetadataErrorToNFS` switched; function deleted from create.go.
- NFSv3 audit wrapper (1): `nfs/xdr/errors.go` — `MapStoreErrorToNFSStatus` body reduced to `common.MapToNFS3` + severity-based log dispatch.
- NFSv4 types (2): `v4/types/errors.go` (MapMetadataErrorToNFS4 deleted with migration note), `v4/types/constants_test.go` (TestMapMetadataErrorToNFS4 removed).
- NFSv4 handlers (16): `v4/handlers/{access,commit,create,getattr,link,lookup,lookupp,open,read,readdir,readlink,remove,rename,setattr,verify,write}.go` — all callers of `types.MapMetadataErrorToNFS4` switched to `common.MapToNFS4`.
- SMB handlers (13): `smb/v2/handlers/{converters,converters_test,create,write,read,close,flush,lock,set_info,query_directory,query_info,stub_handlers,ioctl_copychunk,ioctl_fsctl}.go` — all callers migrated.

## Decisions Made

- **Delete `MapMetadataErrorToNFS4` instead of reducing it to a one-liner wrapper.** The plan (Task 2 step 3c) specified reducing the body to `return common.MapToNFS4(err)` and keeping the function for backward compatibility. This is impossible without creating an import cycle: `common/errmap.go` imports `nfs/v4/types` for the `NFS4ERR_*` constants, so `nfs/v4/types/errors.go` cannot import `common/`. The cleanest resolution is to delete the wrapper and migrate every caller (16 files in `v4/handlers`) directly to `common.MapToNFS4`. Documented inline in `v4/types/errors.go` with a migration note for future readers.
- **`ErrInvalidArgument` row → `NFS3ErrInval`, not `NFS3ErrIO`.** The pre-consolidation `xdr/errors.go` audit wrapper mapped `ErrInvalidArgument` to `NFS3ErrIO` (coarsening EINVAL → EIO); `create.go:mapMetadataErrorToNFS` correctly mapped to `NFS3ErrInval`. The handler-path mapper is the authority for client-visible behavior, and `TestReadLink_NotSymlink` in `v3/handlers/readlink_test.go` codifies `NFS3ErrInval` as the expected code. I fixed my initial errmap.go row to follow handler-path authority; the xdr wrapper's existing callers now also get `NFS3ErrInval` — a behavior-positive change because EINVAL is the correct POSIX mapping.
- **Two commits, not one.** Plan D-16 permitted 1-3 commits; I split into (1) create tables + tests, (2) migrate call sites. Each commit is independently green (`go test -race ./...` passes at both HEAD and HEAD~1). This makes `git bisect` cleaner if a behavior drift is later discovered — bisecting into commit 1 implicates the table values, bisecting into commit 2 implicates the call-site migration. A single commit was plausible but would have been ~750 lines of diff; the two-commit split keeps each reviewable.
- **Preserve xdr/errors.go as an audit wrapper.** Per D-07 and PATTERNS.md gotcha #6, `MapStoreErrorToNFSStatus` adds per-code logging. I shrank its switch body to `common.MapToNFS3(err)` + a compact severity dispatch (`logger.Error` for NoSpace/IOError server-side failures; `logger.Warn` for everything else). All fields (`operation`, `code`, `message`, `path`, `client`) preserved. Callers that want raw mapping without logging use `common.MapToNFS3` directly; callers that want audit use the wrapper.
- **Keep the NFSv3 "cache full" string-match heuristic at the xdr layer.** Per PATTERNS.md gotcha: string matching on error messages is intentionally NOT in common/. `MapContentErrorToNFSStatus` in `xdr/errors.go` retains its string switches as a transition fallback; the typed-error path (which is what common/ covers) fires first via the delegation to `MapStoreErrorToNFSStatus`.

## Deviations from Plan

### Rule 3 (blocking issue): NFSv4 MapMetadataErrorToNFS4 deletion instead of wrapper shrink

- **Found during:** Task 2 (planning phase before editing)
- **Issue:** The plan specifies reducing `v4/types/errors.go:MapMetadataErrorToNFS4` body to `return common.MapToNFS4(err)`. This requires `nfs/v4/types` to import `internal/adapter/common`. But `common/errmap.go` already imports `nfs/v4/types` for the `NFS4ERR_*` constants — creating an import cycle.
- **Fix:** Deleted `MapMetadataErrorToNFS4` entirely. Migrated all 16 callers in `v4/handlers/*` directly to `common.MapToNFS4`. Migrated the existing `TestMapMetadataErrorToNFS4` test coverage into `common/errmap_test.go:TestMapToNFS4` (which iterates every row in `errorMap` — strictly more coverage than the hand-curated 22-case test).
- **Files modified:** `v4/types/errors.go` (function deleted, migration note added), `v4/types/constants_test.go` (test removed, unused imports trimmed), 16 files in `v4/handlers/*`
- **Commit:** `9c0aa787`

### Planning-time three-way drift findings (per D-07)

Each row in `errorMap` explicitly resolves the drift inline. The full list surfaced during consolidation:

- **NFSv3 omissions** (codes NFSv3 pre-consolidation switch did not handle): `ErrDeadlock`, `ErrGracePeriod`, `ErrLockLimitExceeded`, `ErrLockConflict`, `ErrLockNotFound`, `ErrConnectionLimitReached`. Resolution: each gets `NFS3ErrJukebox` (transient retry semantic) or `NFS3ErrIO` (generic I/O) fallback — conservative and consistent with how NFSv3 clients handle unknown transient errors.
- **NFSv3 inconsistency between handler path and audit wrapper**: `ErrInvalidArgument` returned `NFS3ErrInval` from `create.go` but `NFS3ErrIO` from `xdr/errors.go`. Resolution: `NFS3ErrInval` (handler path authority; test-codified).
- **NFSv3 inconsistency for handle errors**: `ErrInvalidHandle` returned `NFS3ErrStale` from `create.go` but `NFS3ErrBadHandle` from `xdr/errors.go`. Resolution: `NFS3ErrBadHandle` (audit wrapper authority per plan). `ErrStaleHandle` unambiguously → `NFS3ErrStale`.
- **SMB omissions** (codes SMB pre-consolidation switch did not handle): `ErrPermissionDenied`, `ErrAuthRequired`, `ErrReadOnly`, `ErrNameTooLong`, `ErrStaleHandle`, `ErrPrivilegeRequired`, `ErrQuotaExceeded`, `ErrLocked`, `ErrLockNotFound`, `ErrDeadlock`, `ErrGracePeriod`, `ErrLockLimitExceeded`, `ErrLockConflict`, `ErrConnectionLimitReached`. Resolution: `StatusAccessDenied` for permission-class (MS-ERREF — SMB has no EPERM distinction), `StatusFileClosed` for `ErrStaleHandle`, `StatusDiskFull` for `ErrQuotaExceeded`, `StatusObjectNameInvalid` for `ErrNameTooLong`, `StatusFileLockConflict` for general-context lock errors (lock-operation context uses `lock_errmap.go`), `StatusInsufficientResources` for limit-class, `StatusInternalError` for `ErrGracePeriod` (no SMB equivalent of NFSv4 grace-period).

## Gotchas Encountered

- **`goimports` aliased `internal/adapter/nfs/xdr/core` as `xdr` in every v4 handler** where it added the `common` import. This is technically correct (the package declares `package xdr`, not `package core`), but it's cosmetic noise unrelated to ADAPT-03. I reverted the alias-only changes in 24 test files that didn't receive any common/ migration so the diff stays focused on functional changes. 15 handler files that did get functional migration kept their alias adjustment because they were already being edited.
- **Test run output initially obscured the one failing test.** `go test -v ./...` produces ~2000 lines of log output for NFSv3 handlers, and the shell grep alias in this environment was using ugrep which treats the log as binary. I had to use `/usr/bin/grep -a` to find the one `--- FAIL` line. The failure was `TestReadLink_NotSymlink` expecting `NFS3ErrInval` — this flagged the `ErrInvalidArgument` drift (see Decisions) and I fixed the errmap row to follow the handler-path authority.
- **`MapContentErrorToNFSStatus` delegates through `MapStoreErrorToNFSStatus`**. This means once `MapStoreErrorToNFSStatus`'s body was shrunk to delegate to `common.MapToNFS3`, the typed-error path in `MapContentErrorToNFSStatus` automatically picks up the common/ mapping. No additional edit needed in `MapContentErrorToNFSStatus` — its string-matching fallback for untyped errors (e.g., "cache full") continues to work.
- **SMB `read.go`'s error branch was already using plan-03-pattern code in plan 02.** Plan 02 installed `return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: common.MapContentToSMB(err)}}, nil` pre-emptively (because plan 02 ran first with `ContentErrorToSMBStatus` as a transition placeholder, and subsequent sed-replace did the mechanical rename). Good: no duplicate work. I did update the surrounding comment to remove the now-stale "plan 03 lands this" note.

## Deferred Issues

None. All planned work completed. No out-of-scope issues discovered.

## Issues Encountered

None material beyond the import-cycle constraint documented as a Rule 3 deviation.

## User Setup Required

None — internal refactor, zero user-visible behavior change. The single latent-bug fix (`goerrors.As` uniformly used in common/) is behavior-positive: wrapped StoreErrors now unwrap correctly in SMB paths. No flags, no configuration, no migration.

## Threat Model Coverage

- **T-09-09 (Tampering / silent drift):** Mitigated by table-driven `TestMapToNFS3/NFS4/SMB` — every row asserts the protocol code matches the expected value. The `xdr/errors.go` preservation also means the existing per-code audit log dispatch continues to fire at the same `logger.Warn` / `logger.Error` severities as before consolidation.
- **T-09-10 (Information Disclosure / wrapped error unwrap):** Mitigated. Every accessor uses `goerrors.As`. Test D in `common/errmap_test.go` explicitly exercises `fmt.Errorf("wrap: %w", ...)` across all three protocols.
- **T-09-11 (DoS / missing row):** Mitigated. `TestErrorMapCoverage` enumerates every `merrs.ErrorCode` and asserts a row exists; the `len(allErrorCodes()) == 25` assertion catches drift in the enumeration itself.
- **T-09-12 (Repudiation / lost audit logs):** Accepted and addressed. `xdr/errors.go:MapStoreErrorToNFSStatus` audit wrapper preserved with compact per-severity log dispatch; `setattr.go`, `rename.go`, `symlink.go`, `remove.go` callers that used it continue to get audit lines with operation name, code, message, path, and client IP.

## Next Phase Readiness

- **Plan 04 (ADAPT-04 / []BlockRef seam)** can layer call-site refactor atop the common/ error seam. Plan 04 touches `common/readFromBlockStore` and `common/writeToBlockStore` (plan 01 deliverables); the error-mapping consolidation here means plan 04's error branches already use `common.MapContentToSMB` / `common.MapContentToNFS3` / `common.MapContentToNFS4` — one consistent mapping per context.
- **Plan 05 (ADAPT-05 / cross-protocol conformance)** can drive from the same `errorMap` table. The test tier split (18 e2e-triggerable + 9 unit-tier per D-13) maps directly onto rows — a table-driven subtest over `errorMap` keys generates both tiers.
- **Phase 12 (META-01 / API-01)** future codes: Go's struct literal enforces three-column population; the coverage test enforces the enumeration; future adapter-layer work is confined to adding a single map row per new code.

## Self-Check: PASSED

Verified:
- `test -f internal/adapter/common/errmap.go` succeeds.
- `test -f internal/adapter/common/content_errmap.go` succeeds.
- `test -f internal/adapter/common/lock_errmap.go` succeeds.
- `test -f internal/adapter/common/errmap_test.go` succeeds.
- `! grep -rqE "^func (mapMetadataErrorToNFS|MetadataErrorToSMBStatus|ContentErrorToSMBStatus|lockErrorToStatus|MapMetadataErrorToNFS4)\b" internal/adapter/` succeeds (all five translator functions deleted).
- `grep -q "common\.MapToNFS3" internal/adapter/nfs/xdr/errors.go` succeeds.
- `grep -rq "common\.MapToNFS3" internal/adapter/nfs/v3/handlers/` succeeds.
- `grep -rq "common\.MapToNFS4" internal/adapter/nfs/v4/handlers/` succeeds.
- `grep -rq "common\.MapToSMB" internal/adapter/smb/v2/handlers/` succeeds.
- `grep -rq "common\.MapContentToSMB" internal/adapter/smb/v2/handlers/` succeeds.
- `grep -q "common\.MapLockToSMB" internal/adapter/smb/v2/handlers/lock.go` succeeds.
- `! grep -Eq "\berrors\.(StoreError|ErrorCode)\b" internal/adapter/common/*.go` succeeds (alias-consistent).
- `git log --oneline -3 | grep -q "ADAPT-03"` succeeds; `git log -2 --show-signature` shows Good RSA signature on both `9ad31e3b` and `9c0aa787`; no Claude mentions anywhere in commit messages.
- `go build ./...` green; `go vet ./...` green; `go test -race -count=1 ./internal/adapter/... ./pkg/metadata/...` all green (38 packages); full-repo `go test -race -count=1 ./...` green (no FAIL entries).

---
*Phase: 09-adapter-layer-cleanup-adapt*
*Completed: 2026-04-24*
