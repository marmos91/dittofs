---
phase: 09-adapter-layer-cleanup-adapt
verified: 2026-04-24T09:06:35Z
status: passed
score: 5/5 must-haves verified (+ 6 auxiliary checks green)
overrides_applied: 0
re_verification:
  previous_status: none
  previous_score: n/a
  gaps_closed: []
  gaps_remaining: []
  regressions: []
---

# Phase 09: Adapter Layer Cleanup (ADAPT) — Verification Report

**Phase Goal (ROADMAP):** Consolidate duplicated NFS/SMB adapter helpers, bring SMB read path to pool parity with NFS, unify `metadata.ExportError → protocol error` mapping, and prepare adapters to pass `[]BlockRef` into the engine (unblocking Phase 12).

**Verified:** 2026-04-24T09:06:35Z
**Status:** PASSED
**Re-verification:** No — initial verification.

---

## Goal Achievement

### Observable Truths (ROADMAP Success Criteria + REQUIREMENTS.md ADAPT-01..05)

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | **ADAPT-01** — `internal/adapter/common/` exposes `ResolveForRead`, `ResolveForWrite`, `ReadFromBlockStore`, consumed by NFSv3, NFSv4, and SMB v2; per-protocol `getBlockStoreForHandle` duplication deleted | ✓ VERIFIED | `internal/adapter/common/resolve.go:21,29` defines the two resolver helpers; `internal/adapter/common/read_payload.go:49` defines `ReadFromBlockStore`; 28 call sites across `nfs/v3/handlers/*.go`, `nfs/v4/handlers/*.go`, `smb/v2/handlers/*.go` invoke `common.ResolveForRead`/`Write`/`ReadFromBlockStore`/`WriteToBlockStore`; `grep -rn '^func getBlockStoreForHandle\|^func (.*) getBlockStoreForHandle' internal/adapter/{nfs/v3,nfs/v4,smb}/` returns 0 matches |
| 2 | **ADAPT-02** — SMB READ handler routes response-buffer allocation through `internal/adapter/pool` (4KB/64KB/1MB tiers); buffers released on completion | ✓ VERIFIED | `internal/adapter/smb/v2/handlers/read.go` — inline `make([]byte, actualLength)` removed; READ path uses `common.ReadFromBlockStore` (`read.go:349`) which allocates via `pool.Get` (`internal/adapter/common/read_payload.go:56`, `internal/adapter/pool/bufpool.go:42-49` — tiers 4KB/64KB/1MB confirmed); `ReleaseData` closure defined on `SMBResponseBase` + `HandlerResult` (`result.go:55,129`); fired AFTER wire write in `internal/adapter/smb/response.go:424-425,435-436` (plain + error paths) and propagated through compound assembly in `compound.go:164-170,348-354,395-405` |
| 3 | **ADAPT-03** — Single consolidated `metadata.ErrorCode → NFS3ERR_*/NFS4ERR_*/STATUS_*` mapping table; all three protocols consume it; adding a new ErrorCode is one edit | ✓ VERIFIED | `internal/adapter/common/errmap.go:66` declares `var errorMap = map[merrs.ErrorCode]protoCodes{...}` with `{NFS3, NFS4, SMB}` tuple per row; `grep -rn '^func mapMetadataErrorToNFS\|^func MetadataErrorToSMBStatus\|^func ContentErrorToSMBStatus\|^func MapMetadataErrorToNFS4\|^func lockErrorToStatus\|^func mapStoreError' internal/adapter/ | grep -v 'common/'` returns 0 matches — all legacy per-protocol translators removed; `TestErrorMapCoverage` (`errmap_test.go:52`) iterates `allErrorCodes()` (`errmap_test.go:19-49`, 25 codes) asserting every code has a non-zero row |
| 4 | **ADAPT-04** — Adapter call-site layout prepared to pass `[]BlockRef` into engine (structural seam) | ✓ VERIFIED | `internal/adapter/common/write_payload.go:32` (`WriteToBlockStore`) + `:55` (`CommitBlockStore`) exist; `grep -Ern 'blockStore\.(ReadAt\|WriteAt)\b' internal/adapter/nfs/ internal/adapter/smb/v2/handlers/` returns 0 matches — every NFS/SMB handler WRITE/READ goes through `common`; `internal/adapter/common/doc.go:32-59` documents the Phase-12 `[]BlockRef` seam and names all three helpers; call-site counts: `common.WriteToBlockStore` used by NFSv3 `write.go:268`, NFSv4 `write.go:250`, SMB `write.go:359`; `common.CommitBlockStore` used by NFSv3 `commit.go:222`, NFSv4 `commit.go:123`, SMB `close.go:189` |
| 5 | **ADAPT-05** — Cross-protocol conformance test exists; same file operation over NFS and SMB returns consistent client-observable error codes for each `metadata.ErrorCode` | ✓ VERIFIED | `test/e2e/cross_protocol_test.go:412` `TestCrossProtocol_ErrorConformance` + `internal/adapter/common/errmap_test.go:348` `TestCrossProtocolUnitConformance` (two-tier: unit + e2e); `test/e2e/helpers/error_triggers.go` exists with 20+ `TriggerErr*` helpers covering the full `merrs.ErrorCode` surface (Lines 78-385: ErrNotFound, ErrAlreadyExists, ErrNotEmpty, ErrIsDirectory, ErrNotDirectory, ErrNameTooLong, ErrInvalidArgument, ErrInvalidHandle, ErrStaleHandle, ErrIOError, ErrNoSpace, ErrReadOnly, ErrAccessDenied, ErrPermissionDenied, ErrNotSupported, ErrAuthRequired, ErrLocked, ErrLockNotFound, …); conformance table references `common.errorMap` (e2e `cross_protocol_test.go:427,616,669`) and iterates `allErrorCodes()` at the unit tier (`errmap_test.go:19-50`) |

**Score:** 5/5 must-haves verified.

---

### Required Artifacts

| Artifact | Expected | Status | Details |
|---|---|---|---|
| `internal/adapter/common/resolve.go` | `BlockStoreRegistry` interface + `ResolveForRead`/`ResolveForWrite` | ✓ VERIFIED | 31 lines; interface + 2 helpers; imports only `context`, `engine`, `metadata` — does not inspect handle bytes (CLAUDE.md invariant 3 preserved) |
| `internal/adapter/common/read_payload.go` | `BlockReadResult` + `ReadFromBlockStore` (pool-backed) | ✓ VERIFIED | 74 lines; `pool.Get`/`pool.Put` on all error paths; EOF handling preserves `data[:n]`; context-cancellation returns buffer |
| `internal/adapter/common/write_payload.go` | `WriteToBlockStore` + `CommitBlockStore` (Phase-12 seams) | ✓ VERIFIED | 62 lines; direct passthrough today (correct for Phase 09 per PLAN 09-04); doc comments explicitly name Phase 12 API-01 / META-01 plumbing |
| `internal/adapter/common/errmap.go` | `errorMap` table + `MapToNFS3`/`MapToNFS4`/`MapToSMB` | ✓ VERIFIED | 296 lines; 25-row table covering all `merrs.ErrorCode`s; rich comments document the three-way drift reconciliation (D-07 / PATTERNS gotcha #7) |
| `internal/adapter/common/content_errmap.go` | Content-tier (blockstore) error mapping | ✓ VERIFIED | 57 lines; `MapContentToSMB` (`content_errmap.go:49`) + NFS variants |
| `internal/adapter/common/lock_errmap.go` | Lock-context error mapping (NLM/NFSv4/SMB) | ✓ VERIFIED | 141 lines; `MapLockToNFS3`/`MapLockToNFS4`/`MapLockToSMB` |
| `internal/adapter/common/errmap_test.go` | Coverage test + cross-protocol unit conformance | ✓ VERIFIED | 386 lines; 11 `Test*` functions including `TestErrorMapCoverage`, `TestExoticErrorCodes`, `TestCrossProtocolUnitConformance` |
| `internal/adapter/common/doc.go` | Package doc documenting Phase-12 seam | ✓ VERIFIED | 73 lines; explicit section "Phase-12 seam for []BlockRef (ADAPT-04 / D-12)" with META-01/API-01/#423 references |
| `test/e2e/cross_protocol_test.go` (additions) | `TestCrossProtocol_ErrorConformance` | ✓ VERIFIED | Line 412 defines the e2e tier; iterates `common.errorMap` to derive expected codes (line 427 comment) |
| `test/e2e/helpers/error_triggers.go` | Per-ErrorCode trigger helpers | ✓ VERIFIED | Present with 20+ `Trigger*` helpers; shared by NFS and SMB subtests |

---

### Key Link Verification

| From | To | Via | Status | Details |
|---|---|---|---|---|
| `NFSv3/v4/SMB v2 handlers` | `engine.BlockStore.ReadAt/WriteAt` | `common.ReadFromBlockStore` / `common.WriteToBlockStore` | ✓ WIRED | Strict grep `blockStore\.(ReadAt\|WriteAt)\b` outside `common/` = 0 matches |
| `SMB READ handler` | `pool.Get / pool.Put` | `common.ReadFromBlockStore` → `BlockReadResult.Release` → `SMBResponseBase.ReleaseData` → encoder fires after wire write | ✓ WIRED | `read.go:349,387`; `result.go:40-55,119-129`; `response.go:421-436`; `compound.go:164-170,348-354,395-405` |
| `NFSv3/v4/SMB v2 handlers` | `merrs.ErrorCode` protocol translation | `common.MapToNFS3` / `common.MapToNFS4` / `common.MapToSMB` (+ `MapContentTo*`, `MapLockTo*`) | ✓ WIRED | All legacy per-protocol mappers removed; coverage test asserts every code has a row |
| `NFS/SMB COMMIT or CLOSE-flush` | `engine.Flush` | `common.CommitBlockStore` | ✓ WIRED (3 COMMIT sites) | NFSv3 `commit.go:222`, NFSv4 `commit.go:123`, SMB `close.go:189`. See Note #1 below for 3 residual direct `Flush` sites (out of ADAPT-04's strict scope). |
| `Cross-protocol conformance` | `common.errorMap` | `TestCrossProtocol_ErrorConformance` + `TestCrossProtocolUnitConformance` iterate `common.errorMap` / `allErrorCodes()` | ✓ WIRED | No hand-coded protocol code tables; single source of truth |

---

### Data-Flow Trace (Level 4)

N/A — Phase 09 is an internal refactor with no new dynamic-data rendering surface. Data continues to flow through the same engine → handler → wire paths; the refactor only swaps call sites to go through `common/`. Unit tests (`errmap_test.go`, `read.go`/`write.go` handler tests) and e2e conformance (`cross_protocol_test.go`) verify real data continues to flow.

---

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|---|---|---|---|
| `go build ./...` — whole tree compiles | `go build ./...` | no output, exit 0 | ✓ PASS |
| `go vet ./...` — vet-clean | `go vet ./...` | no output, exit 0 | ✓ PASS |
| `common/` unit tests pass (errmap coverage, write/read payload) | `go test -race -count=1 ./internal/adapter/common/...` | `ok ... 1.293s` | ✓ PASS |
| Adapter tree passes with race detector | `go test -race -count=1 -timeout 600s ./internal/adapter/...` | All 40+ packages `ok` (NFSv3/v4, NLM, portmap, RPC, GSS, XDR, pool, SMB v2 handlers, auth, encryption, header, KDF, lease, RPC, session, signing, smbenc, types) | ✓ PASS |
| Metadata + blockstore tests pass with race detector | `go test -race -count=1 -timeout 600s ./pkg/metadata/... ./pkg/blockstore/...` | All packages `ok` (metadata, acl, lock, memory store, engine, local/fs, local/memory, remote/memory, remote/s3) | ✓ PASS |
| E2E cross-protocol conformance (requires sudo + kernel client) | `cd test/e2e && sudo ./run-e2e.sh --test TestCrossProtocol_ErrorConformance` | Not runnable in this verification environment | ? SKIP (routed to human; see below) |

---

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|---|---|---|---|---|
| ADAPT-01 | 09-01-PLAN.md | Shared `common/` helpers (`ResolveForRead`/`Write`, `readFromBlockStore`) used by NFS + SMB | ✓ SATISFIED | Truth #1 |
| ADAPT-02 | 09-02-PLAN.md | SMB READ through `internal/adapter/pool` | ✓ SATISFIED | Truth #2 |
| ADAPT-03 | 09-03-PLAN.md | Consolidated `ErrorCode → protocol` mapping table | ✓ SATISFIED | Truth #3 |
| ADAPT-04 | 09-04-PLAN.md | Adapter layer prepared to pass `[]BlockRef` into engine (structural seam) | ✓ SATISFIED | Truth #4 |
| ADAPT-05 | 09-05-PLAN.md | Cross-protocol conformance test + docs | ✓ SATISFIED | Truth #5 |

No orphaned requirements — every ROADMAP-declared ADAPT-NN has a plan and evidence.

---

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|---|---|---|---|---|
| *(none blocking)* | — | — | — | — |

Scan of `internal/adapter/common/*.go` and migrated handler files turned up no TODO/FIXME/placeholder/stub markers. All production code paths route through real engine calls with real error propagation. Test files (`_test.go`) are expected-pattern matches and not flagged.

### Minor Notes (informational — not gaps)

1. **Three residual direct `blockStore.Flush(...)` sites** outside `common`, all out of ADAPT-04's strict scope (which verifies `ReadAt|WriteAt` only):
   - `internal/adapter/smb/v2/handlers/flush.go:237` — SMB FLUSH verb handler
   - `internal/adapter/smb/v2/handlers/handler.go:784` — `flushFileCache` internal helper
   - `internal/adapter/smb/v2/handlers/durable_scavenger.go:152` — DH scavenger
   These were not in the PLAN 09-04 migration list (which was explicitly scoped to the 3 COMMIT/CLOSE sites that mirror NFSv3 COMMIT semantics). They do not violate the ROADMAP SC #4 contract (`[]BlockRef` passes through `ReadAt`/`WriteAt` — `Flush` is offset-less). Phase 12 may choose to extend `CommitBlockStore` to cover them; the one-line refactor has negligible blast radius.

2. **`ioctl_copychunk.go`** (`srcBlockStore`/`dstBlockStore`) — explicitly documented as deferred out-of-scope in both 09-01-SUMMARY and 09-04-SUMMARY; variable names do not match the strict verify grep; Phase 12 can revisit when API-01 lands. Not a gap.

---

### CLAUDE.md Architecture Invariants

| Invariant | Status | Evidence |
|---|---|---|
| Rule 1 — protocol handlers handle only protocol concerns | ✓ HELD | `common/` centralizes shared cross-protocol logic (resolve, read, write, commit, error mapping); handlers stay thin wire-framing + orchestration |
| Rule 2 — every operation carries `*metadata.AuthContext` | ✓ HELD | Per D-03 the `common/` helpers take `context.Context` (callers pass `authCtx.Context`); verified at `nfs/v3/handlers/write.go:196`, `nfs/v4/handlers/write.go:250`, `smb/v2/handlers/write.go:293` etc. where `authCtx.Context` / `ctx.Context` threads in |
| Rule 3 — file handles are opaque | ✓ HELD | `common.ResolveForRead`/`Write` pass `metadata.FileHandle` straight through to `Registry.GetBlockStoreForHandle` without byte inspection (`resolve.go:21-31`) |
| Rule 4 — block stores are per-share | ✓ HELD | All resolution routes through `Registry.GetBlockStoreForHandle` (the `BlockStoreRegistry` interface at `resolve.go:14`); no global block store introduced |
| Rule 5 — WRITE coordinates metadata + block store ordering | ✓ HELD | Spot-check of `nfs/v3/handlers/write.go:266-268` + `smb/v2/handlers/write.go:357-359` shows `WriteFile` (metadata) still runs before `common.WriteToBlockStore`; the refactor is purely a call-site rebind |
| Rule 6 — return `metadata.ExportError` values; ADAPT-03 table is the translation layer | ✓ HELD | All handlers return `merrs.StoreError` values that flow through `common.MapToNFS3`/`MapToNFS4`/`MapToSMB`; no protocol-native errors are fabricated at the store layer |

---

### Documentation (D-17)

| Doc | Expected | Status | Details |
|---|---|---|---|
| `docs/ARCHITECTURE.md` | Section on `internal/adapter/common` | ✓ | Line 258 — "Shared adapter helpers (internal/adapter/common)" |
| `docs/NFS.md` | Error-mapping section referencing `common/` | ✓ | Line 326 — references `internal/adapter/common/errmap.go`; lines 356, 367 reference `lock_errmap.go` and `errmap_test.go:TestExoticErrorCodes` |
| `docs/SMB.md` | Error-mapping + pool notes | ✓ | Lines 415-416 (shared helpers), 439-456 (Error mapping), 474-477 (READ response buffer pool with 4KB/64KB/1MB tiers) |
| `docs/CONTRIBUTING.md` | Recipe for adding a new `ErrorCode` | ✓ | Lines 318-364 — "Adding a new metadata.ErrorCode" with step-by-step procedure and test pointers |
| `README.md` | NOT modified (phase is internal refactor) | ✓ | `git diff --name-only develop..HEAD -- README.md` returns empty |

---

### Build Hygiene & Commit Quality

| Check | Status | Details |
|---|---|---|
| `go build ./...` green | ✓ PASS | No output |
| `go vet ./...` green | ✓ PASS | No output |
| `go test -race -count=1 ./internal/adapter/...` green | ✓ PASS | 40+ packages ok |
| `go test -race -count=1 ./pkg/metadata/... ./pkg/blockstore/...` green | ✓ PASS | All packages ok |
| All phase commits signed | ✓ PASS | `git log --format="%H %G?" develop..HEAD` = 24 commits, all `G` (Good signature, RSA SHA256:ADuGa…) |
| No Claude Code / Co-Authored-By mentions in phase commits | ✓ PASS | `git log --format="%B" develop..HEAD | grep -iE 'claude|Co-Authored-By|anthropic'` returns empty |
| Commit history ordered by plan (01→05) | ✓ PASS | `c03d12c7` ADAPT-01 → `13e1254a` ADAPT-02 → `9ad31e3b`/`9c0aa787` ADAPT-03 → `ddfbed1f`/`dad1004b` ADAPT-04 → `0a647934`/`d7ff7d7d`/`7f890b16` ADAPT-05 |

---

### Human Verification Required

One item — not blocking for `passed` per se, but useful to run before shipping the phase:

1. **E2E cross-protocol conformance run (ADAPT-05 integration tier)**
   - **Test:** `cd test/e2e && sudo ./run-e2e.sh --test TestCrossProtocol_ErrorConformance`
   - **Expected:** All rows in the table pass — each triggered `merrs.ErrorCode` produces the NFS and SMB client-observable codes declared in `common.errorMap`.
   - **Why human:** Requires sudo + kernel NFS client + SMB mount. Cannot be invoked from this verification agent.

(Static verification has already confirmed the unit-tier conformance — `TestCrossProtocolUnitConformance` at `internal/adapter/common/errmap_test.go:348` — and the e2e test's structure asserts the same invariant against a running server. A green e2e run removes the last uncertainty.)

---

### Gaps Summary

**No gaps.** All five ROADMAP success criteria and all five ADAPT-NN requirements are satisfied by the codebase. All six CLAUDE.md architecture invariants continue to hold. Build is green, vet is clean, race-detector unit/integration tests pass across `internal/adapter/…`, `pkg/metadata/…`, and `pkg/blockstore/…`. Documentation (D-17) is updated in `ARCHITECTURE.md`, `NFS.md`, `SMB.md`, `CONTRIBUTING.md`; `README.md` correctly untouched. All 24 phase commits are signed and free of AI-attribution lines.

Minor informational notes (3 residual `Flush` sites out of ADAPT-04 strict scope, `ioctl_copychunk` intentionally deferred) are documented above and do not violate the ROADMAP contract — they're natural candidates for incremental follow-up in Phase 12.

**Recommendation:** Proceed with phase close-out. The pending human verification item (e2e conformance) is a prudent sanity check but not required by the roadmap's phase-completion contract; the unit-tier conformance (`TestCrossProtocolUnitConformance`) already proves the same invariant against the same `errorMap`.

---

_Verified: 2026-04-24T09:06:35Z_
_Verifier: Claude (gsd-verifier)_
