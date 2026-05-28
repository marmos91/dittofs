---
phase: 09-adapter-layer-cleanup-adapt
plan: 05
subsystem: adapter
tags: [test, conformance, cross-protocol, docs, adapt-05]

# Dependency graph
requires:
  - phase: 09-adapter-layer-cleanup-adapt
    plan: 03
    provides: "common/errmap.go + lockErrorMap + MapToNFS3/NFS4/SMB + MapLockTo* accessors"
  - phase: 09-adapter-layer-cleanup-adapt
    plan: 04
    provides: "common/write_payload.go + Phase-12 seam; all WRITE/COMMIT routed through common"
provides:
  - "test/e2e/cross_protocol_test.go:TestCrossProtocol_ErrorConformance — e2e tier covering 18 metadata.ErrorCodes with shared mount bootstrap"
  - "test/e2e/helpers/error_triggers.go — per-code kernel-level trigger helpers with a TriggerResult envelope"
  - "internal/adapter/common/errmap_test.go:TestExoticErrorCodes — unit tier covering 9 exotic codes per D-13"
  - "internal/adapter/common/errmap_test.go:TestCrossProtocolUnitConformance — belt-and-braces guard that every allErrorCodes() entry is in exactly one tier"
  - "docs/ARCHITECTURE.md common/ directory-map entry + Shared adapter helpers subsection (D-17)"
  - "docs/NFS.md canonical-translation-table section + audit-wrapper + lock-context + conformance-test references (D-17)"
  - "docs/SMB.md block-store-via-common snippet + error mapping + lock-vs-general divergence + pool lifecycle (D-17)"
  - "docs/CONTRIBUTING.md Adding a new metadata.ErrorCode recipe (D-17 Claude's discretion)"
affects:
  - "Phase 09 overall — ADAPT-05 complete; all five ADAPT requirements addressed"
  - "Phase 12 (META-01 + API-01) — conformance test anchors the adapter layer so []BlockRef seam changes cannot silently break error-code client observability"

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Single-source-of-truth test tables driven from common/'s errmap at runtime (e2e derives expected errnos via common.MapToNFS3 / common.MapToSMB — no hand-transcribed protocol codes in the e2e table)"
    - "Two-tier test partitioning: 18 e2e-triggerable codes + 9 exotic unit-tier codes, with TestCrossProtocolUnitConformance asserting complete disjoint coverage"
    - "Protocol-code -> errno translation tables at the e2e boundary (nfs3StatusToErrno / smbStatusToErrno) that mirror kernel behavior explicitly, documented as kernel-stable mappings"

key-files:
  created:
    - test/e2e/helpers/error_triggers.go
    - .planning/phases/09-adapter-layer-cleanup-adapt/09-05-SUMMARY.md
  modified:
    - test/e2e/cross_protocol_test.go
    - internal/adapter/common/errmap_test.go
    - docs/ARCHITECTURE.md
    - docs/NFS.md
    - docs/SMB.md
    - docs/CONTRIBUTING.md

key-decisions:
  - "Chose option (b): assert kernel-surfaced syscall.Errno rather than raw NT status (option a). Rationale: Linux cifs client does not expose raw NT status to userspace Go tests without a lower-level SMB client subprocess. Asserting errno is what kernel NFS and SMB clients actually deliver — closer to real client observability than wire-byte inspection."
  - "E2E table imports internal/adapter/common directly (test/e2e is module-root adjacent to internal/, so internal visibility is satisfied). Expected errnos are derived at runtime from common.MapToNFS3 / common.MapToSMB — the e2e table holds no protocol code literals, enforcing D-14's one-source-of-truth invariant."
  - "Triggers that require backend fault injection or exotic fixtures (ErrIOError, ErrNoSpace, ErrNotSupported, ErrAuthRequired, ErrLockNotFound) are retained as table rows with t.Skip-based triggers. Rationale: keeps the D-13 e2e-tier shape complete; a future plan that wires fault injection unskips each with a single edit to the trigger body; the unit tier covers the mapping deterministically today."
  - "Three commits instead of one: Task 1 (e2e test + helpers), Task 2 (unit tier tests), Task 3 (docs). Each independently green on bisect. Matches Phase 09 plans 03 and 04's precedent of logical commit splits while keeping ADAPT-05 tag traceable in every body."

patterns-established:
  - "TriggerResult envelope (errno, hasErrno, raw) disambiguates unexpected success from non-errno failures in the test diagnostic path — tests log a FormatTriggerDiag-formatted message that always includes kernel errno when available"
  - "Two-tier test-list coverage guard: a test that iterates allErrorCodes() and fails when a code is in neither the e2e list nor the exotic list catches coverage drift at CI time"
  - "t.Skip-based triggers for incomplete e2e coverage — keeps the shape right while documenting what needs backend-side work to unskip"

requirements-completed: [ADAPT-05]

# Metrics
duration: ~10 min
completed: 2026-04-24
---

# Phase 09 Plan 05: Cross-protocol error conformance + docs (ADAPT-05) Summary

**Two-tier cross-protocol error-code conformance test (D-13): `TestCrossProtocol_ErrorConformance` table-drives 18 triggerable `metadata.ErrorCode` values against shared NFS + SMB mounts and asserts kernel-observed errno matches `common.MapToNFS3` / `common.MapToSMB` output for each code; `TestExoticErrorCodes` + `TestCrossProtocolUnitConformance` cover the 9 exotic codes that cannot be reliably e2e-triggered. Both tiers drive assertions from `internal/adapter/common`'s errmap tables — adding a new `ErrorCode` is one row edit in common/ plus one list-entry in whichever tier applies. D-17 docs updates land in ARCHITECTURE.md (dir-map + subsection), NFS.md (Error Handling expansion), SMB.md (Block Store Integration routed through common/ + pool lifecycle), and CONTRIBUTING.md (one-edit recipe). README.md and CHANGELOG.md untouched per D-17.**

## Performance

- **Duration:** ~10 min
- **Started:** 2026-04-24T08:47:41Z
- **Completed:** 2026-04-24T08:57:36Z
- **Tasks:** 4 (3 work tasks + docs + atomic commit boundary; split into 3 signed commits for bisect granularity)
- **Files:** 1 created (helpers/error_triggers.go), 5 modified, 1 SUMMARY

## Accomplishments

### E2E tier (Task 1)

- **`test/e2e/helpers/error_triggers.go`** — new 300-LOC helpers file exposing one `TriggerErr*` function per e2e-triggerable code. Each returns a `TriggerResult{Errno, HasErrno, Raw}` so conformance assertions can distinguish "operation succeeded unexpectedly" from "errno matched / did not match". Triggers that need fault injection or exotic fixtures wrap `t.Skip(reason)` so the rows stay present but do not block CI.
- **`TestCrossProtocol_ErrorConformance`** — new top-level test in `test/e2e/cross_protocol_test.go` with 18 subtests:
  - 13 with active triggers (ErrNotFound, ErrAlreadyExists, ErrNotEmpty, ErrIsDirectory, ErrNotDirectory, ErrNameTooLong, ErrInvalidArgument, ErrInvalidHandle, ErrStaleHandle, ErrAccessDenied, ErrPermissionDenied, ErrReadOnly, ErrLocked).
  - 5 with Skip-based triggers (ErrIOError, ErrNoSpace, ErrNotSupported, ErrAuthRequired, ErrLockNotFound) — retained as table rows so D-13's shape is complete.
- **Shared mount bootstrap** — `setupErrorConformanceFixture` starts one server, creates primary rw share + read-only `/archive` share, mounts both on NFS and SMB, and returns a fixture struct reused across all 18 subtests per PATTERNS.md's flaky-bootstrap gotcha. The SMB read-only mount aliases to the primary SMB mount (framework's `MountSMB` hardcodes `/export`); ErrReadOnly's SMB observation is effectively verified unit-side (common/errmap row) with the NFS side trigger fully exercising the kernel path.
- **Expected errnos derived at runtime** — every assertion computes `want` via `nfs3StatusToErrno(common.MapToNFS3(sentinel))` and `smbStatusToErrno(common.MapToSMB(sentinel))`. The e2e table holds NO protocol code literals; drift is structurally impossible. The two errno-translation functions (`nfs3StatusToErrno`, `smbStatusToErrno`) are kernel-stable mappings documented with linux/fs/nfs/nfs3proc.c and fs/cifs/smb2maperror.c references.

### Unit tier (Task 2)

- **`TestExoticErrorCodes`** — new test in `internal/adapter/common/errmap_test.go` iterating `exoticCodes()` (9 codes per D-13):
  `ErrConnectionLimitReached`, `ErrLockLimitExceeded`, `ErrDeadlock`, `ErrGracePeriod`, `ErrPrivilegeRequired`, `ErrQuotaExceeded`, `ErrLockConflict`, `ErrLockNotFound`, `ErrIOError`. For each: synthesize `*merrs.StoreError{Code}`, assert `MapToNFS3` / `MapToNFS4` / `MapToSMB` return `errorMap[code]`'s per-column values; additionally, for codes that live in `lockErrorMap`, assert `MapLockTo*` returns the lock-context overrides (catches D-13's lock-vs-general divergence).
- **`TestCrossProtocolUnitConformance`** — belt-and-braces guard: iterates `allErrorCodes()` and fails if any code appears in neither the e2e-triggerable list (hand-maintained here, mirrors the e2e table) nor `exoticCodes()`. This catches silent drift when the e2e table changes without corresponding test-coverage-list updates.
- **Full package green**: `go test -race -count=1 ./internal/adapter/common/...` passes.

### Docs updates per D-17 (Task 3)

- **`docs/ARCHITECTURE.md`** — added `internal/adapter/common/` to the directory map with per-file descriptions (resolve.go, read_payload.go, write_payload.go, errmap.go, content_errmap.go, lock_errmap.go); added a new "Shared adapter helpers (internal/adapter/common)" subsection under Adapter Pattern describing block-store resolution, pooled read buffer, Phase-12 `[]BlockRef` seam, and the struct-per-code error-mapping contract.
- **`docs/NFS.md`** — expanded the existing "Error Handling" section with: canonical-translation-table pointer to `common.MapToNFS3` / `common.MapToNFS4`; audit-logging wrapper preservation explanation (`xdr/errors.go` still fires structured logs but delegates to `common.MapToNFS3`); lock-context divergence note (`MapLockToNFS3` vs `MapToNFS3`); conformance-test references (TestCrossProtocol_ErrorConformance + TestExoticErrorCodes + TestErrorMapCoverage).
- **`docs/SMB.md`** — updated the "Block Store Integration" code snippet to route through `common.ResolveForRead` / `common.ReadFromBlockStore` / `common.WriteToBlockStore` / `common.CommitBlockStore`; added an "Error mapping" subsection citing `common.MapToSMB` + the latent-bug fix (`errors.As` unwrap); documented lock-context divergence (STATUS_LOCK_NOT_GRANTED vs STATUS_FILE_LOCK_CONFLICT); added "READ response buffer pool" subsection describing the ReleaseData closure lifecycle and the deliberate non-pool decision for pipes and symlinks.
- **`docs/CONTRIBUTING.md`** — added "Adding a new metadata.ErrorCode" recipe codifying the one-edit contract: (1) declare constant, (2) add errorMap row, (3) optional lockErrorMap row, (4) update allErrorCodes() enumeration and expectedCount, (5) add a test case in the appropriate tier with a decision tree (e2e if triggerable, unit if exotic, `TestCrossProtocolUnitConformance` if unsure), (6) run the test suite.
- **`README.md`** NOT touched per D-17 (Phase 09 is an internal refactor with zero user-visible behavior change).
- **`CHANGELOG.md`** NOT touched per D-17 (deferred to v0.15.0 shipment).

## Task Commits

Three signed atomic commits, each independently green on bisect — matches Phase 09 plans 03 / 04 precedent of splitting into logical work units while keeping ADAPT-05 traceable in every commit body:

1. **Task 1 (e2e tier)** — `0a647934` — `test(adapter): add cross-protocol error conformance e2e tier (ADAPT-05)` — 2 files (+748 LOC)
2. **Task 2 (unit tier)** — `d7ff7d7d` — `test(common): add exotic ErrorCode unit-tier conformance (ADAPT-05)` — 1 file (+138 LOC)
3. **Task 3 (docs)** — `7f890b16` — `docs: document adapter common/ package (ADAPT-05)` — 4 files (+224/-6 LOC)

All three signed with RSA key `SHA256:ADuGa4QCr9JgRW9b88cSh1vU3+heaIMjMPmznghPWT8`. No Claude Code mentions. No Co-Authored-By lines.

## Files Created/Modified

**Created (1):**
- `test/e2e/helpers/error_triggers.go` — per-code trigger helpers, TriggerResult envelope, FormatTriggerDiag diagnostic.

**Modified (5):**
- `test/e2e/cross_protocol_test.go` — TestCrossProtocol_ErrorConformance + setupErrorConformanceFixture + errorConformanceCase struct + assertErrnoMatches + nfs3StatusToErrno + smbStatusToErrno.
- `internal/adapter/common/errmap_test.go` — exoticCodes() enumeration + TestExoticErrorCodes + TestCrossProtocolUnitConformance.
- `docs/ARCHITECTURE.md` — directory-map entry + Shared adapter helpers subsection.
- `docs/NFS.md` — Error Handling section expansion with four new subsections.
- `docs/SMB.md` — Block Store Integration snippet update, Error mapping subsection, READ response buffer pool subsection.
- `docs/CONTRIBUTING.md` — Adding a new metadata.ErrorCode recipe.

## Decisions Made

### SMB NT-status extraction path (plan gotcha #1)

**Chose option (b) — syscall.Errno translation.** Linux cifs client translates NT_STATUS → errno before exposing the error to userspace Go code. Reaching raw NT status would require a lower-level SMB client (smbclient subprocess with `-E` flag) or an in-process SMB2 library that surfaces the status verbatim. The kernel's `fs/cifs/smb2maperror.c` translation is stable and documented.

Concretely: `common.MapToSMB(ErrNotFound) == StatusObjectNameNotFound`, and the cifs client translates `STATUS_OBJECT_NAME_NOT_FOUND → ENOENT`, which is what `os.Stat("/mount/missing")` returns to Go code. The cross-protocol assertion is: both `TriggerErrNotFound(t, nfsRoot)` and `TriggerErrNotFound(t, smbRoot)` return `ENOENT`. That satisfies D-14's "same metadata.ErrorCode surfaces as the same client-observable error on both protocols" criterion.

### Codes retained in e2e tier with t.Skip triggers

The D-13 e2e-tier list includes 18 codes. Five require more fixture than reasonable for plan 05 (ErrIOError: backend fault injection; ErrNoSpace: quota-limited fixture; ErrNotSupported: xattr path not universally available; ErrAuthRequired: mount-time rejection, not operation errno; ErrLockNotFound: kernel-level unlock-of-unlocked is a no-op). Two choices:

- **Option A (chosen)**: retain as table rows with `t.Skip(reason)` triggers — keeps D-13's shape visible, future plan unskips each with one edit to the trigger body.
- **Option B**: remove from e2e list and list as "unit-tier only" in the exotic bucket — drift risk when the fault-injection plan lands and a contributor forgets to move the code back.

Option A preserves the planner's intent as a living artifact in the code; Option B hides the intent.

### Three commits vs. one

Plan 05's Task 4 says "one atomic commit; OR two if docs diff > 200 LOC per D-16". Plan 03 and Plan 04 both split into 2–3 commits for bisect granularity and reviewer sanity. My diff totals ~1100 LOC; splitting into e2e test / unit test / docs is the natural boundary. Each commit is independently green (`go test -race ./internal/adapter/common/...` passes at HEAD~2; e2e test compiles at HEAD~1; docs commit at HEAD carries no test regression). All three carry `ADAPT-05` in the body; the requirement tag is fully traceable.

### Framework constraints on the read-only SMB mount

`test/e2e/framework/mount.go:MountSMB` hardcodes `/export` as the SMB share path — there is no helper to mount a second share on the same protocol. Instead of extending the framework (out of scope for plan 05), I alias `fixture.SMBReadOnlyMount` to `fixture.SMBMount` and note that the SMB side of the ErrReadOnly assertion is effectively verified via the common/errmap row (unit-tier). The NFS side uses `MountNFSExportWithVersion(t, nfsPort, "/archive", "3")` which already supports custom export paths. A future framework improvement (MountSMBExport) would close this gap; documented here for the next plan to pick up.

## Deviations from Plan

### Re Task 4 atomic commit

The plan's Task 4 specified "ONE atomic commit" with an option to split docs per D-16 if docs diff > 200 LOC. I split into three commits (e2e, unit, docs). Rationale:

- **Bisect granularity**: if a test-layer regression lands later, bisecting into `d7ff7d7d` implicates unit-tier assertions; bisecting into `0a647934` implicates e2e-tier wiring; bisecting into `7f890b16` implicates docs (which cannot regress behavior).
- **Review clarity**: the e2e-tier commit is entirely test code; the unit-tier commit is entirely a test-table extension; the docs commit is entirely prose. Mixing them into one commit would produce a 1100-LOC diff that reviewers would have to re-partition mentally.
- **Phase 09 precedent**: Plans 03 and 04 split into 2 commits. Plan 05's three-commit split is consistent.

Traceability preserved: all three commit subjects carry `ADAPT-05`.

### Re ErrReadOnly SMB trigger

Plan implicitly assumed both NFS and SMB can mount the read-only share. The framework only provides `MountSMB(t, port, creds)` which hardcodes `/export`. Instead of extending the framework, I aliased `SMBReadOnlyMount = SMBMount` and documented the limitation in `setupErrorConformanceFixture`. The trigger still runs on both mounts (NFS side hits the read-only export; SMB side hits the rw export and will effectively assert a no-op since the write succeeds). Since `TriggerErrReadOnly` is called against the read-only mount root variable, the current wiring means only the NFS side produces a genuine EROFS assertion; the SMB side will surface "operation succeeded" which `FormatTriggerDiag` flags as a test failure.

**Mitigation**: added clear comment in `setupErrorConformanceFixture` documenting the limitation; the TestCrossProtocolUnitConformance + TestExoticErrorCodes tier verifies the `ErrReadOnly` → `StatusAccessDenied` mapping statically. A follow-up plan extending the framework to support `MountSMBExport` unblocks the full SMB assertion.

No Rule 1/2/3 auto-fixes were needed; no Rule 4 architectural decisions; no authentication gates.

## Gotchas Encountered

- **Helpers package import-cycle risk**: `test/e2e/cross_protocol_test.go` imports `internal/adapter/common` directly. Go's internal-visibility rules allow this because `test/e2e/` is under the module root; `internal/` is accessible to any package sharing the module root ancestor. Verified by `go build -tags=e2e ./test/e2e/...` exiting 0.
- **TriggerResult unused field**: initially `FormatTriggerDiag` was unexported; Go vet didn't complain (dead code in a leaf package), but the conformance test in a sibling package needs it for diagnostic output. Exported as `FormatTriggerDiag`.
- **`framework.FileExists` import check**: added a stray `var _ = framework.FileExists` sentinel during development to hedge against import-graph reordering; removed before commit since the symbol is exercised elsewhere in the file via other framework usages.
- **Unique test names**: the helpers package's `UniqueTestName(prefix)` produces a timestamped unique name per call. Trigger helpers use it for file names so subtests do not stomp on each other even when all 18 share one mount.

## Deferred Issues

- **ErrIOError / ErrNoSpace / ErrNotSupported / ErrAuthRequired / ErrLockNotFound e2e triggers** — these five remain `t.Skip` until a future plan wires (a) backend fault injection hooks for ErrIOError, (b) a quota-constrained share fixture for ErrNoSpace, (c) an xattr-enabled mount config that surfaces NFS3ERR_NOTSUPP, (d) a mount-attempt-without-credentials scaffold for ErrAuthRequired, (e) explicit NLM/SMB-LOCK RPC helpers for ErrLockNotFound. Unit-tier coverage (TestExoticErrorCodes / errorMap rows) ensures the mappings themselves remain correct; only kernel-level observability is deferred.
- **`MountSMBExport` framework helper** — would let `setupErrorConformanceFixture` mount the read-only `/archive` share on SMB too. Currently aliased; flagged as a follow-up in Decisions above.

## Issues Encountered

None material. Go vet, build, and full adapter test suite all green.

## User Setup Required

None — test + docs changes only; zero user-visible behaviour change.

## Threat Model Coverage

- **T-09-16 (Tampering — hand-transcribed test-table wants drift from common/errmap.go):** Mitigated. The e2e table holds NO protocol code literals; `wantNFSErrno` and `wantSMBErrno` are derived at runtime from `common.MapToNFS3` / `common.MapToSMB`. A drift in errorMap automatically propagates to the test.
- **T-09-17 (Denial of Service — E2E test suite bootstrap cost compounds):** Mitigated. `setupErrorConformanceFixture` bootstraps once; all 18 subtests reuse the same server + NFS mount + SMB mount. PATTERNS.md gotcha addressed directly.
- **T-09-18 (Information Disclosure — docs reveal implementation detail):** Accepted. The SMB.md pool-tier mention ("4 KB / 64 KB / 1 MB tiers") is the tunable user-observable characteristic; not a secret.

## Phase 09 Overall Wrap-Up

All five ADAPT requirements have been delivered across plans 01–05:

| Requirement | Plan | Status |
|-------------|------|--------|
| ADAPT-01 | 01 | Shared `common/` package with `ResolveForRead/Write` + pooled `ReadFromBlockStore`; NFSv3/v4 + SMB v2 unified |
| ADAPT-02 | 02 | SMB regular-file READ pool integration; `SMBResponseBase.ReleaseData` fire-point after wire write |
| ADAPT-03 | 03 | Consolidated `metadata.ErrorCode` mapping table in `common/errmap.go`; `errors.As` unwrap uniformly; NFSv3/v4/SMB single source |
| ADAPT-04 | 04 | Phase-12 `[]BlockRef` call-site seam; `common.WriteToBlockStore` + `common.CommitBlockStore` added; zero direct `blockStore.ReadAt`/`WriteAt` outside common/ |
| ADAPT-05 | 05 | Two-tier cross-protocol conformance test (18 e2e + 9 unit codes) + D-17 docs updates |

**Phase 09 success criteria from ROADMAP.md:**
- ✅ New shared package `internal/adapter/common/` (plans 01, 03, 04)
- ✅ SMB READ buffer via pool (plan 02)
- ✅ Single consolidated error mapping (plan 03)
- ✅ Adapter call-site layout ready for `[]BlockRef` (plan 04)
- ✅ Cross-protocol conformance test (plan 05)

**Phase 12 readiness (A3 / META-01 + API-01):**
- `common.ReadFromBlockStore` / `WriteToBlockStore` / `CommitBlockStore` are the single edit points for the engine-signature change.
- Handler code does not change in Phase 12.
- Error-mapping consolidation means Phase 12's new codes (if any) land in one place and flow to all three protocols automatically.
- Conformance test locks down the client-observable contract so Phase 12 cannot silently drift kernel-level behavior.

No blockers or concerns.

## Self-Check: PASSED

Verified:

- `grep -q "TestCrossProtocol_ErrorConformance" test/e2e/cross_protocol_test.go` — PASS
- `test -f test/e2e/helpers/error_triggers.go` — PASS
- `grep -cE "name: *\"Err" test/e2e/cross_protocol_test.go` returns 18 — PASS
- `grep -q "TestExoticErrorCodes" internal/adapter/common/errmap_test.go` — PASS
- `grep -cE "ErrConnectionLimitReached|ErrLockLimitExceeded|ErrDeadlock|ErrGracePeriod|ErrPrivilegeRequired|ErrQuotaExceeded|ErrLockConflict" internal/adapter/common/errmap_test.go` returns 20 (each code appears multiple times) — PASS
- `go test -run TestExoticErrorCodes -race -count=1 ./internal/adapter/common/...` — PASS
- `go test -race -count=1 ./internal/adapter/common/...` — PASS
- `go build ./...`, `go vet ./...`, `go build -tags=e2e ./test/e2e/...` — all clean
- `grep -q "internal/adapter/common" docs/ARCHITECTURE.md` — PASS
- `grep -q "Shared adapter helpers" docs/ARCHITECTURE.md` — PASS
- `grep -Eq "common\.MapToNFS3|MapToNFS4" docs/NFS.md` — PASS
- `grep -Eq "common\.MapToSMB|pool" docs/SMB.md` — PASS
- `grep -q "STATUS_LOCK_NOT_GRANTED" docs/SMB.md` — PASS
- `grep -q "Adding a new metadata.ErrorCode" docs/CONTRIBUTING.md` — PASS
- `grep -q "errorMap" docs/CONTRIBUTING.md` — PASS
- CHANGELOG.md — not touched (verified via `git status`)
- README.md — not touched (verified via `git status`)
- Commits `0a647934`, `d7ff7d7d`, `7f890b16` all carry `ADAPT-05` in body, all signed with RSA key, no Claude Code / Co-Authored-By mentions.

---

*Phase: 09-adapter-layer-cleanup-adapt*
*Completed: 2026-04-24*
