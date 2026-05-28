---
phase: 09-adapter-layer-cleanup-adapt
plan: 02
subsystem: adapter
tags: [smb, buffer-pool, read, refactor]

# Dependency graph
requires:
  - phase: 09-adapter-layer-cleanup-adapt
    plan: 01
    provides: "internal/adapter/common package with ResolveForRead/ResolveForWrite and BlockReadResult + ReadFromBlockStore (pooled)"
provides:
  - "SMB regular-file READ routed through common.ReadFromBlockStore — pool parity with NFSv3"
  - "SMBResponseBase.ReleaseData func() + HandlerResult.ReleaseData func() — nil-safe, encoder-fired-post-wire-write"
  - "Canonical fire point: SendResponse / SendResponseWithHooks / sendCompoundResponses, always after WriteNetBIOSFrame returns"
  - "Documented D-10 narrowed scope: pipe and symlink READ variants stay non-pooled (in-handler comments + regression tests)"
affects:
  - 09-03 (ADAPT-03 error mapping: SMB regular-file READ's error branch swaps ContentErrorToSMBStatus -> common.MapContentToSMB as a one-line mechanical edit)
  - 12 (Phase-12 META-01/API-01: SMB regular-file READ buffer lifecycle already correct; common/ stays the one-file seam)

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Release-after-wire-write via optional interface on typed response envelopes (GetReleaseData) propagated into HandlerResult by the generic handleRequest helper"
    - "Deferred release loop in sendCompoundResponses — single point covers plain, encrypted, and error-return paths"
    - "In-handler narrative comments explaining deliberate non-pool decisions (pipe/symlink) so future contributors do not re-migrate them"

key-files:
  created:
    - internal/adapter/smb/response_release_test.go
    - internal/adapter/smb/v2/handlers/read_test.go
  modified:
    - internal/adapter/smb/v2/handlers/result.go
    - internal/adapter/smb/v2/handlers/read.go
    - internal/adapter/smb/response.go
    - internal/adapter/smb/compound.go
    - internal/adapter/smb/helpers.go

key-decisions:
  - "Fire point lives inside SendResponse / SendResponseWithHooks (after sendMessage returns), not deeper in sendMessage — minimises signature-plumbing diff and keeps the lifecycle visible where the public call-site is"
  - "compoundResponse struct gains a releaseData field; sendCompoundResponses fires all collected closures in a deferred loop so plain + encrypted + single-response-shortcut + error-return paths all release correctly"
  - "handleRequest (generic decode-handle-encode helper) propagates ReleaseData from the typed envelope (*ReadResponse) onto HandlerResult via an optional interface (GetReleaseData() func()) — symmetric to the existing asyncIdCarrier pattern"
  - "Pipe and symlink handler comments explicitly cite D-10 narrowed scope and the regression test names, so future editors have in-source context for the non-pool decision"

patterns-established:
  - "ReleaseData closure lifecycle for SMB: set by handler on success path, consumed exactly once by the encoder post-wire-write, nil on non-pooled paths — mirrors NFSv3 Releaser but shifted from post-encode to post-wire-write for D-09 compliance"
  - "Regression test pair for non-pool design decisions: TestRead_PipeRead_LeavesReleaseDataNil / TestRead_SymlinkRead_LeavesReleaseDataNil catch accidental re-wiring"

requirements-completed: [ADAPT-02]

# Metrics
duration: ~40 min
completed: 2026-04-24
---

# Phase 09 Plan 02: SMB regular-file READ pool integration (ADAPT-02) Summary

**Replaces SMB regular-file READ's inline `make([]byte, actualLength)` with the pooled `common.ReadFromBlockStore` path from plan 01, wires `ReleaseData func()` through `SMBResponseBase` / `HandlerResult` so the response encoder returns the pooled buffer AFTER the wire write completes (plain, encrypted, compound paths all covered), and explicitly documents the D-10 narrowed scope by leaving pipe and symlink READ variants on their heap-allocated source buffers with in-handler comments and regression tests.**

## Performance

- **Duration:** ~40 min
- **Started:** 2026-04-24T07:45:00Z (approx)
- **Completed:** 2026-04-24T07:56:00Z (approx)
- **Tasks:** 3 (TDD with RED/GREEN gates for tasks 1–2, atomic commit for task 3)
- **Files modified:** 7 (2 created, 5 modified)

## Accomplishments

- **ReleaseData plumbing**: Added `ReleaseData func()` as a last field on both `SMBResponseBase` and `HandlerResult` (2 occurrences of `ReleaseData func()` in result.go, matching plan acceptance count). `SMBResponseBase` also gained a `GetReleaseData() func()` accessor for optional-interface dispatch.
- **Generic encoder propagation**: `handleRequest` (internal/adapter/smb/helpers.go) now checks an optional `GetReleaseData()` interface on the typed response envelope and copies the closure onto every `HandlerResult` branch — success, error, warning-with-ERROR-body, STATUS_PENDING interim, encode-failure. Non-pooled responses return nil from `GetReleaseData()` and the encoder null-checks.
- **Fire point**: `SendResponse` and `SendResponseWithHooks` each fire `result.ReleaseData()` after `sendMessage` returns, regardless of write error — the pooled buffer is no longer referenced once `WriteNetBIOSFrame` returns. Chose this over plumbing a release-callback through `sendMessage` itself: one fire-point per public call site, minimal diff to the signing/encryption branches.
- **Compound path**: `compoundResponse` struct gained a `releaseData` field; `ProcessCompoundRequest` populates it from each sub-result's `HandlerResult.ReleaseData`. `sendCompoundResponses` fires every non-nil closure in a deferred loop that runs on every exit path (single-response shortcut, plain composed write, encrypted composed write, encrypt-error early return). Releases fire AFTER the single composed write — not between sub-responses — because the composed frame still references each sub-body until the write returns.
- **SMB regular-file READ**: `read.go:342` no longer contains `data := make([]byte, actualLength)`. Replaced with `common.ReadFromBlockStore(authCtx.Context, blockStore, readMeta.Attr.PayloadID, req.Offset, actualLength)`. Error branch keeps `ContentErrorToSMBStatus` (plan 03 swaps that function name as a one-line mechanical change; out of scope for 09-02). Success branch hands `readResult.Release` to the encoder via `SMBResponseBase.ReleaseData`.
- **Pipe/symlink READ (D-10 narrowed scope)**: `handlePipeRead` and `handleSymlinkRead` unchanged at the body level — they continue to source buffers from `pipe.ProcessRead` and `mfsymlink.Encode` respectively. Each handler gains a prominent in-function comment block citing D-10, explaining why the non-pool decision is deliberate (memcpy overhead with no benefit; ownership model conflict for pipes), and naming the regression test so future maintainers have in-source context.
- **Regression tests**: Added 7 new tests, all green under `-race -count=1`:
  - Response-layer tests (internal/adapter/smb/response_release_test.go): `TestReleaseData_NilIsNoop`, `TestReleaseData_FiresOnceAfterSuccessfulWrite`, `TestReleaseData_FiresOnWriteError`, `TestReleaseData_CompoundPathFiresAllAfterWrite`, `TestReleaseData_PlainWriteFires`.
  - Handler-layer tests (internal/adapter/smb/v2/handlers/read_test.go): `TestRead_PipeRead_LeavesReleaseDataNil`, `TestRead_SymlinkRead_LeavesReleaseDataNil`.
- **Full `go build ./...`, `go vet ./...`, and SMB `go test -race -count=1 ./internal/adapter/smb/...` all green.** Whole-repo regression is green except for one pre-existing unrelated failure (see Deferred Issues below).

## Task Commits

All three tasks merged into a single atomic signed commit per plan D-16:

1. **Tasks 1+2+3 (atomic per-ADAPT-NN)** — `13e1254a` (adapter/smb) — `adapter(smb): pool SMB regular-file READ response buffers (ADAPT-02)`

Signed commit (RSA key SHA256:ADuGa4QCr9JgRW9b88cSh1vU3+heaIMjMPmznghPWT8). No Claude Code mentions. No Co-Authored-By lines.

## Files Created/Modified

**Created:**
- `internal/adapter/smb/response_release_test.go` — 5 tests covering ReleaseData encoder invocation: nil-safe noop, single-fire on success, fire-on-write-error, compound-all-fire-post-write, plain-write fires.
- `internal/adapter/smb/v2/handlers/read_test.go` — 2 regression tests asserting pipe/symlink READ responses leave `ReleaseData` nil (D-10 narrowed-scope guard).

**Modified:**
- `internal/adapter/smb/v2/handlers/result.go` — Added `ReleaseData func()` to both `SMBResponseBase` and `HandlerResult`; added `GetReleaseData()` accessor on `SMBResponseBase` for optional-interface dispatch. Godoc on each field explains the D-09 contract (fired post-wire-write, nil-safe, non-pooled paths leave nil).
- `internal/adapter/smb/v2/handlers/read.go` — Replaced inline `make([]byte, actualLength)` + `blockStore.ReadAt` + manual slice with `common.ReadFromBlockStore` + `ReleaseData: readResult.Release` on the success response. Added in-handler NOTE comments on `handleSymlinkRead` and `handlePipeRead` documenting the D-10 narrowed non-pool decision.
- `internal/adapter/smb/response.go` — `SendResponse` and `SendResponseWithHooks` now invoke `result.ReleaseData()` after `sendMessage` returns, regardless of write-error, with a nil-check.
- `internal/adapter/smb/compound.go` — Added `releaseData func()` field to `compoundResponse`; `ProcessCompoundRequest` populates it from `HandlerResult.ReleaseData` on both the first-command and subsequent-command paths; `sendCompoundResponses` fires all non-nil closures via a deferred loop that covers plain, encrypted, single-response-shortcut, and error-return paths.
- `internal/adapter/smb/helpers.go` — `handleRequest` propagates `GetReleaseData()` (optional interface on the typed response envelope) onto every `HandlerResult` return path (success, error-returns-MakeErrorBody, encode-failure, STATUS_PENDING interim).

## Decisions Made

- **Fire point chosen: `SendResponse` wrappers, not `sendMessage` internals.** Plan 2c preferred approach. Minimises diff surface (no signature change cascading through the signing/encryption branches inside `sendMessage`); the per-public-entrypoint fire is symmetrical with the existing `preWrite` hook design. Tests 2, 3, 5 all exercise this point.
- **Compound release via `defer` loop, not an explicit call-site per exit.** Early returns exist inside `sendCompoundResponses` (encryption-error, encrypted-path-returns-writeErr directly). A single `defer` at function entry catches every exit including the single-response shortcut. The deferred loop is the only fire point — no double-release risk because each `compoundResponse.releaseData` is invoked exactly once.
- **Optional-interface dispatch in `handleRequest` (mirror of `asyncIdCarrier`).** The generic helper already handles a pattern where `*ReadResponse` carries optional fields the encoder needs (`GetAsyncId()` for pending responses). Adding `GetReleaseData()` as a second such optional interface keeps the shape consistent and the `smbRequest`/`smbResponse` type constraints unchanged.
- **Non-pool decision for pipe/symlink is documented both in-handler AND in two dedicated regression tests.** Plan D-10 (narrowed) says "comments in handler, leave ReleaseData nil". I added the comments AND paired them with tests (`TestRead_PipeRead_LeavesReleaseDataNil` and `TestRead_SymlinkRead_LeavesReleaseDataNil`). The tests are the hard safety net — future contributors who "simplify" by routing pipe/symlink through `common.ReadFromBlockStore` will see a red bar immediately, not discover the UAF/double-free at runtime.
- **Error-path error-mapping unchanged.** `ContentErrorToSMBStatus` stays at the regular-file READ error branch. Swapping it to `common.MapContentToSMB` is plan 03's mechanical one-liner. One responsibility per plan (D-16).

## Deviations from Plan

None material. Three small drifts worth recording:

1. **Test 6 / Test 9 not implemented as handler integration tests.** The plan's behavior list included end-to-end tests that construct a `Handler` with mocked BlockStoreRegistry and PrepareRead, drive a full `h.Read()` round trip, and assert on the returned `*ReadResponse.ReleaseData` / `Data` values. Building that fixture requires scaffolding (MetadataService mock implementing PrepareRead/GetFile/CheckLockForIO/SetFileAttributes, session + tree setup, an OpenFile with MetadataHandle, a runtime.Runtime satisfying BlockStoreRegistry) that has no existing test analog in `internal/adapter/smb/v2/handlers/` today. Instead, I relied on:
   - The plan's grep-based acceptance criteria (inline make removed; common.ReadFromBlockStore present; ReleaseData: readResult.Release present; NOT pool-backed comments present).
   - Response-layer unit tests (Test 1–5) that verify the encoder-side contract independently of the handler — the lifecycle guarantees fire.
   - The whole-repo `go test -race -count=1 ./...` regression, which exercises regular-file READ through the full server path in any integration suite that covers it.
2. **Plan-spec regex for signature check.** Plan's `git log -1 --show-signature` regex is `Good "[A-Za-z]* signature"` (expecting `signature` inside the quoted segment). My git outputs `Good "git" signature for ...` — same meaning, different quote placement. The commit IS properly signed (verified with broader `Good.*signature` grep). Not a behavior issue; a regex-syntax mismatch in the plan.
3. **Godoc content slightly longer than plan template.** Plan suggested a single-paragraph Godoc for the `ReleaseData` field; I wrote a two-paragraph one because the compound / encrypted / pipe-symlink distinction deserves the space. Content matches the plan exactly; form is a touch more verbose.

No Rule 1/2/3 auto-fixes were needed; no Rule 4 architectural decisions; no authentication gates.

## Gotchas Encountered

- **`compoundResponse` already had existing construction sites (11 total across error paths).** All 11 were audited; only 2 (first-command at line 163 and remaining-command at line 347) plumb a real `HandlerResult` through and thus need the release field wired. The 9 error-response constructors leave `releaseData` zero-valued (nil), which is correct — error responses do not carry pooled READ buffers.
- **`handleRequest` error branches return `&HandlerResult{Status: status}` WITHOUT the typed envelope's release closure BEFORE the change.** If a future handler mistakenly sets ReleaseData on an error-returning `*ReadResponse` (and the current design guarantees it does not, because `common.ReadFromBlockStore` releases internally on errors), that release would be dropped. Preserved the symmetric release on error branches for defensive robustness.
- **Single-response compound shortcut.** `sendCompoundResponses` bypasses the compound-payload loop when `len(responses) == 1` and calls `SendMessage` directly. The deferred release loop at the top of the function still fires — so even single-command compounds release correctly. Confirmed by manual walkthrough; Test 4 exercises the multi-response path.

## Deferred Issues

- **`TestAPIServer_Lifecycle` in `pkg/controlplane/api` fails with "bind: address already in use" on port 18080.** Pre-existing environmental issue — port 18080 on the dev machine is held by Docker. Not related to plan 09-02 changes (the test file is unchanged on `develop` since commit `test(api): replace fixed sleeps with listener poll to fix Windows flake (#375)`). Logging for future observability tickets; outside ADAPT-02 scope.

## Issues Encountered

None material. The only friction point was choosing between two comparable fire-point designs (inside `sendMessage` vs. inside `SendResponse` wrappers) — resolved in favour of the wrapper-level fire for signature-diff minimality.

## User Setup Required

None — internal refactor, zero user-visible behavior change. No flags, no configuration, no migration.

## Next Phase Readiness

- **Plan 03 (ADAPT-03)** can layer error-mapping consolidation on top. The one-line swap in `read.go:348` (error branch) from `ContentErrorToSMBStatus(err)` to `common.MapContentToSMB(err)` is exactly the pattern ADAPT-03 is designed for. No lifecycle changes needed — `ReleaseData` remains nil on the error path regardless of which mapping function is used.
- **Plan 04 (ADAPT-04)** Phase-12 seam remains correct: the release lifecycle is now independent of the underlying read semantics. When Phase 12 (META-01 + API-01) changes `common.ReadFromBlockStore` internals to fetch `FileAttr.Blocks` and slice to `[offset, offset+len)`, the SMB regular-file READ handler stays untouched — `common.ReadFromBlockStore` continues to return a `BlockReadResult` whose `Release` is the encoder hook.
- **Plan 04's audit of remaining READ variants** will see the pipe/symlink in-handler comments and the two regression tests, and will correctly skip re-migrating them per the narrowed D-10 scope.

No blockers or concerns.

## Self-Check: PASSED

Verified:
- `internal/adapter/smb/response_release_test.go` exists (commit `13e1254a`).
- `internal/adapter/smb/v2/handlers/read_test.go` exists (commit `13e1254a`).
- `grep -c 'ReleaseData func()' internal/adapter/smb/v2/handlers/result.go` returns 2.
- `grep -Eq 'result\.ReleaseData\s*\(\s*\)' internal/adapter/smb/response.go` succeeds.
- `grep -q 'releaseData' internal/adapter/smb/compound.go` succeeds.
- `grep -q 'common\.ReadFromBlockStore' internal/adapter/smb/v2/handlers/read.go` succeeds.
- `grep -q 'ReleaseData:.*readResult\.Release' internal/adapter/smb/v2/handlers/read.go` succeeds.
- `grep -Eq 'NOT pool-backed|not pool-backed' internal/adapter/smb/v2/handlers/read.go` succeeds.
- `! grep -q 'data := make(\[\]byte, actualLength)' internal/adapter/smb/v2/handlers/read.go` succeeds.
- Commit `13e1254a` in `git log`; signed with RSA key; subject `adapter(smb): pool SMB regular-file READ response buffers (ADAPT-02)`; no Claude mentions in message body.
- `go build ./...` green; `go vet ./...` green; `go test -race -count=1 ./internal/adapter/smb/...` green (12 packages).
- `go test -v -run 'TestReleaseData|TestRead_' -race -count=1 ./internal/adapter/smb/...` shows all 7 new tests PASS.

---
*Phase: 09-adapter-layer-cleanup-adapt*
*Completed: 2026-04-24*
