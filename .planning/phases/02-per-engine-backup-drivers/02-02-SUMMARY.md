---
phase: 02-per-engine-backup-drivers
plan: 02
subsystem: backup
tags: [gob, crc32, metadata, memory-store, backup, restore]

# Dependency graph
requires:
  - phase: 01-foundations-models-manifest-capability-interface
    provides: "metadata.Backupable interface, Phase-2 sentinel errors, storetest.BackupTestStore/BackupStoreFactory, RunBackupConformanceSuite"
provides:
  - "*MemoryMetadataStore implements metadata.Backupable"
  - "memoryBackupRoot — versioned on-disk format for memory-store snapshots"
  - "MDFS envelope format (magic + version + length + CRC32) detecting single-byte corruption"
  - "Conformance wiring proving memory store passes 5-subtest Phase-2 contract"
affects: [02-03-badger-driver, 02-04-postgres-driver, 03-destination-drivers, 05-restore-orchestration]

# Tech tracking
tech-stack:
  added: [encoding/gob, hash/crc32]
  patterns:
    - "Framed envelope (magic + version + length + CRC) around gob payloads for corruption detection"
    - "Shadow-field capture of lazy sub-stores in a single root struct (avoids GobEncoder/GobDecoder on every sub-type)"
    - "Restore recomputes usedBytes defensively rather than trusting archive value"
    - "Compile-time interface assertions lock the Backupable + BackupTestStore contracts in the test file"

key-files:
  created:
    - pkg/metadata/store/memory/backup.go
    - pkg/metadata/store/memory/backup_test.go
  modified:
    - pkg/metadata/store/memory/shares.go

key-decisions:
  - "Envelope over raw gob: CRC32 catches single-byte flips that gob's self-describing framing tolerates (needed for Corruption conformance subtest T-02-02-01)"
  - "Shadow-field capture of inner sub-store state instead of per-type GobEncoder: keeps backup logic colocated in backup.go and preserves the lazy-init contract"
  - "1 GiB payload cap on Restore: bounded allocation for T-02-02-04 DoS mitigation; memory store is not expected to approach this"
  - "Recompute usedBytes from restored Files (not trust the archive): T-02-02-06 defense against quota-evasion via tampered archive"

patterns-established:
  - "Envelope framing: 20-byte LE header (magic 'MDFS' + version + uint64 length + CRC32 IEEE) precedes any gob/tar payload produced by Phase-2 drivers — future engines may adopt or define their own envelope"
  - "nilSafeMap / nilSafeChildrenMap helpers reconstitute post-gob maps that decoded as nil"

requirements-completed: [ENG-03]

# Metrics
duration: 10min
completed: 2026-04-16
---

# Phase 02 Plan 02: Memory store backup driver Summary

**In-memory metadata store gains full Backup/Restore via a length-framed, CRC32-protected gob envelope that passes all five Phase-2 conformance subtests (RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, PayloadIDSet)**

## Performance

- **Duration:** ~10 min
- **Started:** 2026-04-16T08:16:02Z
- **Completed:** 2026-04-16T08:26:00Z (approx)
- **Tasks:** 3 (Task 1 was a no-op — `Close()` already existed)
- **Files created:** 2 (`backup.go`, `backup_test.go`)
- **Files modified:** 1 (`shares.go` — Rule 1 bug fix exposed by conformance suite)

## Accomplishments

- `*MemoryMetadataStore` satisfies `metadata.Backupable` (compile-time assertion in `backup.go`).
- Backup emits a gob-encoded `memoryBackupRoot` wrapped in a 20-byte MDFS envelope (magic + version + payload-length + CRC32); same-snapshot PayloadIDSet computed under the shared `mu.RLock()` per D-02.
- Restore rejects non-empty destination (`ErrRestoreDestinationNotEmpty`), validates envelope magic/version/length/CRC and gob schema version (`ErrRestoreCorrupt`), atomically replaces internals, and recomputes `usedBytes` defensively.
- Conformance suite wired through `pkg/metadata/storetest.RunBackupConformanceSuite` — all 5 subtests PASS. 4 memory-specific direct tests additionally cover: restore-into-self rejection, pre-cancelled ctx behaviour, empty-store round-trip, and envelope structural shape.
- Surfaced and fixed a pre-existing memory-store bug in `CreateRootDirectory` that caused `GetRootHandle` to point at an empty subtree (handle mismatch with the tree's real root).

## Task Commits

1. **Task 1: Verify Close() exists on MemoryMetadataStore** — no-op; `Close()` was already defined at `pkg/metadata/store/memory/shares.go:295`. No commit required.
2. **Task 2a: Implement Backup/Restore with gob serialization** — `41cd21bb` (feat)
3. **Task 2b: CreateRootDirectory handle-reuse fix (Rule 1 deviation)** — `34764786` (fix)
4. **Task 2c: Add MDFS envelope + CRC32 corruption detection** — `1f249058` (feat; completes Task 2)
5. **Task 3: Wire backup_test.go conformance + direct tests** — `7113bf3a` (test)

## Files Created/Modified

- `pkg/metadata/store/memory/backup.go` — `memoryBackupRoot` struct (wire format), `Backup(ctx, w)`, `Restore(ctx, r)`, `nilSafeMap` helpers, envelope constants, compile-time `var _ metadata.Backupable = (*MemoryMetadataStore)(nil)`.
- `pkg/metadata/store/memory/backup_test.go` — `TestBackupConformance` (shared suite) + 4 memory-specific direct tests + compile-time `var _ storetest.BackupTestStore = (*memory.MemoryMetadataStore)(nil)`.
- `pkg/metadata/store/memory/shares.go` — `CreateRootDirectory` now reuses `store.shares[shareName].RootHandle` when the share exists (Rule 1 fix).

## Final `memoryBackupRoot` Field List (Wire Format v1)

Field ordering is LOCKED; reordering/removing requires bumping `memoryGobSchemaVersion`.

Header (D-09):
- `FormatVersion uint32` — Phase-2-internal, currently 1
- `GobSchemaVersion uint32` — bumped when struct layout changes
- `GoVersion string` — `runtime.Version()` at backup time; advisory only

Core maps (D-01):
- `Shares map[string]*shareData`
- `Files map[string]*fileData`
- `Parents map[string]metadata.FileHandle`
- `Children map[string]map[string]metadata.FileHandle`
- `LinkCounts map[string]uint32`
- `DeviceNumbers map[string]*deviceNumber`
- `PendingWrites map[string]*metadata.WriteOperation`

Value fields:
- `ServerConfig metadata.MetadataServerConfig`
- `Capabilities metadata.FilesystemCapabilities`

Sessions:
- `Sessions map[string]*metadata.ShareSession`

Lazy sub-store shadow fields (inner state captured directly; avoids per-type GobEncoder):
- `HasFileBlockData bool` + `FileBlockBlocks map[string]*metadata.FileBlock` + `FileBlockHashIndex map[metadata.ContentHash]string`
- `HasLockStore bool` + `LockLocks map[string]*lock.PersistedLock` + `LockServerEpoch uint64`
- `HasClientStore bool` + `ClientRegistrations map[string]*lock.PersistedClientRegistration`
- `HasDurableStore bool` + `DurableHandles map[string]*lock.PersistedDurableHandle`

Footer:
- `UsedBytes int64` — captured for audit; Restore recomputes from Files (T-02-02-06)

## MDFS Envelope Layout

```
offset  size   field
------  ----   -----
0       4      magic 'MDFS' (little-endian 0x4d444653)
4       4      envelope FormatVersion (LE uint32)
8       8      payload length in bytes (LE uint64, capped at 1 GiB on Restore)
16      4      payload CRC32 IEEE (LE uint32)
20      N      gob-encoded memoryBackupRoot
```

## `gob.Register` Calls Added

None. `MetadataServerConfig.CustomSettings map[string]any` is the only interface-typed payload that could require registration, but no memory-store code paths put structured values there (tests + production callers use primitives only). A comment in `backup.go:init()` marks the hook point for future additions.

## Struct Definitions That Changed Shape

None. The shadow-field approach captured inner state of `fileBlockStoreData`, `memoryLockStore`, `memoryClientStore`, `memoryDurableStore` into exported fields on `memoryBackupRoot`, leaving the sub-store structs untouched. No `GobEncoder`/`GobDecoder` methods added, no field-export changes.

## Conformance Subtest Outcomes

All 5 subtests PASS (`go test ./pkg/metadata/store/memory/... -count=10 -run TestBackup -race`):

| Subtest | Outcome | Notes |
|---------|---------|-------|
| RoundTrip | PASS | Populated 2 shares × 3 dirs × 2 files = 12 files with distinct PayloadIDs; round-trip preserves handles, attrs, tree, and PayloadIDSet |
| ConcurrentWriter | PASS | `mu.RLock` serialises writer behind Backup; snapshot is consistent with post-restore enumeration |
| Corruption/HeaderTruncated | PASS | `io.ReadFull` on envelope header returns `ErrUnexpectedEOF` → wrapped as `ErrRestoreCorrupt` |
| Corruption/BodyTruncated | PASS | Envelope length check catches short payload, wrapped as `ErrRestoreCorrupt` |
| Corruption/SingleByteFlip | PASS | CRC32 mismatch on payload detected, wrapped as `ErrRestoreCorrupt` (envelope was added specifically to close this gap — raw gob tolerated the flip) |
| NonEmptyDest | PASS | Empty-dest guard triggers before envelope read; pre-existing data untouched |
| PayloadIDSet | PASS | Returned set exactly equals post-restore enumerated set |

Memory-specific direct tests (all PASS):
- `TestBackupMemory_RestoreIntoSelfRejected`
- `TestBackupMemory_CtxCancelBeforeBackup`
- `TestBackupMemory_EmptyStoreRoundTrip`
- `TestBackupMemory_EnvelopeShape`

Total run time: ~300 ms per invocation; stable across `-count=10 -race`.

## Decisions Made

- **Envelope over raw gob stream** — added after discovering that single-byte flips in the middle of a gob stream decode successfully (producing structurally-valid but semantically-wrong data), which would violate T-02-02-01 (Corruption conformance). The MDFS envelope with CRC32 closes this gap deterministically. A small format deviation from the plan's "single gob encoding" characterisation in D-05, but preserves the gob payload inside and is a strict superset (callers can ignore the envelope if they want raw gob — which is exactly what `TestBackupMemory_EnvelopeShape` verifies).
- **Shadow-field capture of sub-store inner state** — chose option (c)-ish (capture inner state directly in the root struct as exported fields) over option (b) (per-type GobEncoder/GobDecoder). Rationale: keeps all backup-related logic in `backup.go`, avoids scattering concerns across 4 sub-store files, and preserves the lazy-init contract unchanged.
- **Payload length cap at 1 GiB** — defense-in-depth bound on `Restore` allocation. Memory-store working sets are not expected to approach this; a malicious / corrupt archive declaring a 16 EiB length is rejected without allocation.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] CreateRootDirectory generated a second UUID instead of reusing the share's pre-assigned RootHandle**
- **Found during:** Task 3 (conformance suite RoundTrip / PayloadIDSet subtests failed — enumeration from `GetRootHandle(shareName)` returned empty trees)
- **Issue:** `CreateShare` stored `shares[name].RootHandle = generateFileHandle(name, "/")` (UUID H1). Then `CreateRootDirectory` regenerated a **different** UUID H2 and keyed `files[H2]`, `children[H2]`. Result: `GetRootHandle` returned H1 but the tree lived under H2; `ListChildren(H1)` always returned empty. A pre-existing bug exposed by the new conformance suite (which is the first test to exercise enumerate-via-GetRootHandle on memory).
- **Fix:** `CreateRootDirectory` now checks `store.shares[shareName]` under the write lock; if the share exists it reuses the stored `RootHandle`, otherwise it falls back to generation.
- **Files modified:** `pkg/metadata/store/memory/shares.go`
- **Verification:** All 5 conformance subtests + existing `TestConformance` + existing memory unit tests pass cleanly after the fix, including under `-race -count=3`.
- **Committed in:** `34764786` (fix commit)

**2. [Rule 2 - Missing Critical] Envelope framing + CRC32 for corruption detection**
- **Found during:** Task 3 (Corruption/SingleByteFlip conformance subtest failed — gob silently tolerated byte flips)
- **Issue:** A raw gob stream is self-describing but far from bit-tight: a single byte flip inside a UUID string or an interior map value decodes as valid data with wrong semantics. The Corruption conformance subtest demands `errors.Is(err, metadata.ErrRestoreCorrupt)` on any single byte flip; gob-only cannot meet this.
- **Fix:** Added a 20-byte MDFS envelope (magic + version + uint64 length + CRC32 IEEE) wrapping the gob payload. Restore validates magic, version, length bound (1 GiB cap), and CRC before invoking the gob decoder.
- **Files modified:** `pkg/metadata/store/memory/backup.go`, `pkg/metadata/store/memory/backup_test.go` (test updated to skip past envelope header)
- **Verification:** 10 consecutive runs of `TestBackup` pass clean including all 3 Corruption variants.
- **Committed in:** `1f249058` (envelope commit)

---

**Total deviations:** 2 auto-fixed (1 pre-existing bug, 1 missing critical correctness feature)
**Impact on plan:** Both fixes were necessary for the conformance contract from Plan 02-01 to hold on memory. The CreateRootDirectory fix is general-purpose and benefits every memory-store consumer. The envelope adds one forward-lookable property (`format_version` + checksum) future engines can study. No scope creep.

## Issues Encountered

- Gob tolerance of single-byte flips was not anticipated in the plan text (which characterises gob as "trivial round-trip" in D-05). The plan's §Behavior 2 explicitly requires the Corruption subtest to pass. Resolution: added the envelope (see Deviation 2).
- The pre-existing `CreateRootDirectory` / `CreateShare` handle-mismatch bug in memory store is not caused by backup work but surfaces only when GetRootHandle is used for enumeration after population, which the new conformance suite is the first caller to do. Fixed via Deviation 1.

## User Setup Required

None — no external service configuration required.

## Next Phase Readiness

- Backup conformance suite now has a known-good reference implementation; Badger (02-03) and Postgres (02-04) drivers can develop against the same test suite with confidence.
- The envelope pattern (magic + version + length + CRC) is available as a pattern for those engines if they want byte-tight corruption detection. Badger's framed KV stream already self-describes; Postgres's tar-of-COPYs has its own tar-level checksum potential. Both engines can choose whether to layer an envelope.
- The `CreateRootDirectory` fix makes Memory store's handle semantics consistent with Badger/Postgres (both of which derive root handles deterministically from share name), improving cross-engine behavioural parity.

## Self-Check: PASSED

- `pkg/metadata/store/memory/backup.go` — FOUND ✓
- `pkg/metadata/store/memory/backup_test.go` — FOUND ✓
- `pkg/metadata/store/memory/shares.go` — modified (Rule 1 fix) ✓
- Commits: `41cd21bb` (Task 2a), `34764786` (Task 2b fix), `1f249058` (Task 2c envelope), `7113bf3a` (Task 3) — all present in `git log --oneline` ✓
- `go build ./...` — clean ✓
- `go vet ./...` — clean ✓
- `go test ./pkg/metadata/store/memory/... -count=10 -race -run TestBackup` — PASS ✓

---
*Phase: 02-per-engine-backup-drivers*
*Completed: 2026-04-16*
