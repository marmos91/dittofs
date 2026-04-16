---
phase: 05-restore-orchestration-safety-rails
plan: 03
subsystem: backup
tags: [destination, manifest, s3, filesystem, backup, restore]

# Dependency graph
requires:
  - phase: 03-destination-drivers-encryption
    provides: "Destination interface with GetBackup/List/Stat/Delete; fs and s3 drivers; manifest.yaml two-file layout; manifest.ReadFrom with MaxManifestBytes cap; isNotFound/classifyS3Error helpers; readManifest helper"
provides:
  - "Destination.GetManifestOnly(ctx, id) interface method"
  - "fs.Store.GetManifestOnly implementation (wraps existing readManifest helper)"
  - "s3.Store.GetManifestOnly implementation (single GetObject on manifestKey)"
  - "s3.Store.GetBackup refactored to delegate manifest-fetch prologue to GetManifestOnly"
  - "Cross-driver conformance scenario testGetManifestOnly in destinationtest.Run"
affects: ["05-04 restore pre-flight (consumes GetManifestOnly to validate store_kind/store_id before payload download)", "05-05 and later block-GC hold provider (unions PayloadIDSet across retained manifests via GetManifestOnly)"]

# Tech tracking
tech-stack:
  added: []
  patterns: ["Interface extension with driver-refactor (S3 GetBackup delegates to GetManifestOnly so the manifest-read code path is shared between callers)", "ErrManifestMissing sentinel reused for cheap-fetch misses"]

key-files:
  created: []
  modified:
    - "pkg/backup/destination/destination.go"
    - "pkg/backup/destination/fs/store.go"
    - "pkg/backup/destination/s3/store.go"
    - "pkg/backup/destination/destinationtest/roundtrip.go"
    - "pkg/backup/destination/fs/store_test.go"
    - "pkg/backup/destination/s3/store_integration_test.go"
    - "pkg/backup/destination/registry_test.go"
    - "pkg/controlplane/runtime/storebackups/service_test.go"
    - "pkg/controlplane/runtime/storebackups/retention_test.go"

key-decisions:
  - "Return parsed *manifest.Manifest directly (not raw bytes) — all callers need the parsed form and both drivers already parse internally."
  - "Insert GetManifestOnly BEFORE GetBackup in the interface declaration so readers see the cheap path first (D-12 rationale)."
  - "S3 GetBackup delegates to GetManifestOnly rather than duplicating the GetObject+ReadFrom prologue — eliminates code duplication and ensures both callers share the same error-shape."

patterns-established:
  - "Cheap-fetch sibling to a streaming method: when a composite read (manifest + payload) has an independently-useful cheap prefix, extract the prefix into its own method and have the composite delegate to it."
  - "Interface mock stubs live in three files (registry_test.go, storebackups/service_test.go, storebackups/retention_test.go) — all require synchronized updates when Destination gains a method."

requirements-completed: [REST-03, SAFETY-01]

# Metrics
duration: 4min
completed: 2026-04-16
---

# Phase 05 Plan 03: Destination.GetManifestOnly + fs/s3 Implementations Summary

**Added a lightweight `GetManifestOnly(ctx, id)` method to the Destination interface so restore pre-flight and the block-GC hold provider can fetch manifests without payload bandwidth; both fs and s3 drivers implement it and S3's GetBackup now delegates the manifest prologue to the new method.**

## Performance

- **Duration:** ~4 min
- **Started:** 2026-04-16T22:03:36Z
- **Completed:** 2026-04-16T22:07:28Z
- **Tasks:** 2 (TDD: RED + GREEN per task)
- **Files modified:** 9

## Accomplishments

- Added `Destination.GetManifestOnly(ctx, id) (*manifest.Manifest, error)` to the driver interface, positioned immediately before `GetBackup` with documented error taxonomy (`ErrManifestMissing`, `ErrDestinationUnavailable`).
- FS driver: thin wrapper around the existing private `readManifest` helper — `<root>/<id>/manifest.yaml` read with zero payload open/close overhead.
- S3 driver: single `GetObject` call against `manifestKey(id)` — no multipart upload path, no payload bandwidth. Error classification reuses the existing `isNotFound` and `classifyS3Error` helpers.
- `s3.Store.GetBackup` refactored to delegate the manifest prologue to `GetManifestOnly`, removing the duplicated `GetObject` + `manifest.ReadFrom` block and keeping error shape consistent between callers.
- Cross-driver conformance: `destinationtest.Run` now covers a `GetManifestOnly` scenario that validates round-trip of `BackupID`, `SHA256`, `StoreID`, `StoreKind`, and `PayloadIDSet`, plus the `ErrManifestMissing` sentinel on unknown id.
- Three additional mock `Destination` implementations (in `registry_test.go`, `service_test.go`, `retention_test.go`) updated so the project builds cleanly under the new interface surface.

## Task Commits

Each task was committed atomically via TDD cycle:

1. **Task 1 (RED): add failing tests for fs.GetManifestOnly** — `ddb2c405` (test)
2. **Task 1 (GREEN): add Destination.GetManifestOnly + fs/s3 implementations** — `f1cb3bbf` (feat)
3. **Task 2: add GetManifestOnly conformance + S3 integration tests** — `e308ebfd` (test)

_Note: Task 1 and Task 2 were tightly coupled — the conformance suite addition in Task 2 is what drives multi-driver coverage; per-driver tests shipped in Task 1 to satisfy the RED gate before implementation landed. No REFACTOR commit was needed — the S3 GetBackup delegation refactor happened as part of the GREEN commit and the existing GetBackup conformance scenario re-verified it._

## Files Created/Modified

### Modified
- `pkg/backup/destination/destination.go` — added `GetManifestOnly` method to the `Destination` interface with D-12 doc comment explaining restore pre-flight + block-GC hold use cases.
- `pkg/backup/destination/fs/store.go` — added `func (s *Store) GetManifestOnly(ctx, id)` wrapping the existing `readManifest(dir)` helper with a ctx cancellation check.
- `pkg/backup/destination/s3/store.go` — added `func (s *Store) GetManifestOnly(ctx, id)` using `s.client.GetObject` on `s.manifestKey(id)`; refactored `GetBackup` to delegate the manifest prologue to the new method.
- `pkg/backup/destination/destinationtest/roundtrip.go` — registered `GetManifestOnly` subtest in `Run`; added `testGetManifestOnly` scenario asserting round-trip of identifying fields + `ErrManifestMissing` on unknown id.
- `pkg/backup/destination/fs/store_test.go` — added `TestFSStore_GetManifestOnly_Roundtrip`, `TestFSStore_GetManifestOnly_MissingReturnsSentinel`, and `TestFSStore_GetManifestOnly_DoesNotOpenPayload` (the last asserts the D-12 "payload untouched" promise by removing payload.bin post-publish and still fetching the manifest).
- `pkg/backup/destination/s3/store_integration_test.go` — added `TestIntegration_S3_GetManifestOnly_Roundtrip` and `TestIntegration_S3_GetManifestOnly_MissingReturnsSentinel` under the `integration` build tag (Localstack-backed).
- `pkg/backup/destination/registry_test.go` — `stubDest` gained a `GetManifestOnly` method so the compile-time `var _ Destination = (*stubDest)(nil)` assertion still holds.
- `pkg/controlplane/runtime/storebackups/service_test.go` — `controlledDestination` gained a `GetManifestOnly` method (returns "not implemented") so the `var _ destination.Destination` assertion still holds.
- `pkg/controlplane/runtime/storebackups/retention_test.go` — `fakeDst` gained a `GetManifestOnly` method (returns "not implemented") so the `var _ destination.Destination` assertion still holds.

## Decisions Made

- **Return type: `*manifest.Manifest`, not raw bytes.** Both drivers already parse internally; forcing callers to re-parse would duplicate work. Discretionary choice called out in CONTEXT.md D-12.
- **S3 GetBackup delegates to GetManifestOnly.** Rather than leave two copies of the "GetObject + ReadFrom + close" prologue, the composite method now calls the cheap one — guarantees identical error-shape across the two callers and shrinks the code path that Phase 5 restore depends on.
- **Interface method placement: before GetBackup.** Readers scanning the interface see the cheap shortcut first; matches the PLAN's explicit guidance and the documentation flow.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Updated three existing Destination mock stubs**
- **Found during:** Task 1 (GREEN gate — `go vet` after adding the interface method failed on three test files that declare `var _ destination.Destination = (*mock)(nil)`)
- **Issue:** `pkg/backup/destination/registry_test.go` (`stubDest`), `pkg/controlplane/runtime/storebackups/service_test.go` (`controlledDestination`), and `pkg/controlplane/runtime/storebackups/retention_test.go` (`fakeDst`) all had compile-time assertions that the mock satisfies `Destination`. Adding a new method to the interface broke those assertions.
- **Fix:** Added a minimal no-op `GetManifestOnly` method to each stub (returning `nil` or `errors.New("not implemented")`). This is the standard "interface extension → sync mock stubs" pattern the codebase already uses.
- **Files modified:** `pkg/backup/destination/registry_test.go`, `pkg/controlplane/runtime/storebackups/service_test.go`, `pkg/controlplane/runtime/storebackups/retention_test.go`.
- **Verification:** `go vet ./pkg/backup/destination/... ./pkg/controlplane/runtime/storebackups/...` and `go test -count=1 ./pkg/backup/destination/... ./pkg/controlplane/runtime/storebackups/...` both clean.
- **Committed in:** `f1cb3bbf` (part of the GREEN commit).

---

**Total deviations:** 1 auto-fixed (1 blocking).
**Impact on plan:** Zero scope creep — mock-stub updates are the mandatory mechanical consequence of adding a method to a published interface.

## Issues Encountered

None.

## Self-Check

All acceptance criteria verified:
- Interface method added: `GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error)` in `pkg/backup/destination/destination.go` line 41.
- `func (s *Store) GetManifestOnly` in `pkg/backup/destination/fs/store.go` line 331.
- `func (s *Store) GetManifestOnly` in `pkg/backup/destination/s3/store.go` line 469.
- `var _ destination.Destination = (*Store)(nil)` present in both `fs/store.go` (line 33) and `s3/store.go` (line 40).
- `go build ./...` exits 0.
- `go vet ./pkg/backup/destination/...` exits 0 (also under `-tags=integration`).
- `go test ./pkg/backup/destination/...` exits 0 across all 5 packages.
- `go test -v -run TestFSStore_GetManifestOnly ./pkg/backup/destination/fs/...` exits 0 (3 subtests pass).
- `go test -v -run TestConformance_FSDriver/GetManifestOnly ./pkg/backup/destination/destinationtest/` exits 0.

## Self-Check: PASSED

## Helper Functions Referenced

- `readManifest(dir string) (*manifest.Manifest, error)` (pkg/backup/destination/fs/store.go:378) — existing private helper; `fs.Store.GetManifestOnly` is a ctx-aware wrapper around it.
- `isNotFound(err error) bool` (pkg/backup/destination/s3/errors.go:18) — maps SDK 404-equivalents to `true`; `s3.Store.GetManifestOnly` wraps the not-found path in `ErrManifestMissing`.
- `classifyS3Error(err error) error` (pkg/backup/destination/s3/errors.go:56) — maps SDK errors to D-07 sentinels; `s3.Store.GetManifestOnly` uses it for transient / permission failures.
- `manifest.ReadFrom(r io.Reader) (*manifest.Manifest, error)` (pkg/backup/manifest/manifest.go:93) — reused unchanged; size-capped at `MaxManifestBytes = 1 MiB` so the cheap-fetch path cannot be weaponized for memory exhaustion.

## Next Phase Readiness

- `Destination.GetManifestOnly` is ready for consumption by 05-04 restore pre-flight and later plans building the block-GC hold provider.
- No follow-up deferred items — the contract is fully delivered, both drivers satisfy it, and the conformance suite guards future drivers.
- Integration tests for S3 are gated by `//go:build integration` and will run under the existing Localstack harness in CI; no new CI wiring required.

---
*Phase: 05-restore-orchestration-safety-rails*
*Completed: 2026-04-16*
