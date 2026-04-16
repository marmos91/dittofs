---
status: issues_found
phase: 02-per-engine-backup-drivers
reviewed: 2026-04-16
depth: standard
files_reviewed: 10
findings:
  critical: 2
  warning: 8
  info: 7
  total: 17
---

# Phase 2: Code Review Report

## Summary

Reviewed the Phase 2 per-engine metadata backup drivers (memory, badger, postgres) plus the shared conformance suite and the top-level `metadata.Backupable` contract. Overall quality is high: D-02 (same-snapshot PayloadIDSet), D-03 (custom badger streaming instead of `DB.Backup`), D-04 (tar+manifest for postgres), D-06 (empty-destination rejection), and D-07 (typed sentinels with `%w` wrapping) are honored consistently. Envelope framing in memory and badger drivers is defensive (magic/CRC32/length bounds).

The most important finding is a **data race in the memory driver**: `Backup` reads lazy sub-store internal state (`lockStore.locks`, `clientStore.registrations`, `durableStore.handles`, plus the sub-stores' `serverEpoch`) while holding only the outer `store.mu.RLock()`, but each of those sub-stores has its own independent `sync.RWMutex`. A writer racing against a backup (the scenario the conformance suite's `ConcurrentWriter` test is designed to exercise) can cause (a) torn reads of those maps, (b) a fatal "concurrent map iteration and map write" panic once gob iterates the aliased map. Other findings are lower-severity — DoS-potential unbounded tar entry read in postgres restore, duplicate-entry tolerance in tar parse, inconsistent error wrapping for some postgres error paths, and cosmetic issues.

## Critical Issues

### CR-01: Memory Backup reads lazy sub-store state without sub-store locks (data race + map-iter panic)

**File:** `pkg/metadata/store/memory/backup.go:178-195`

`Backup` accesses lazy sub-store internals under only the outer `store.mu.RLock()`. Every sub-store has an independent `sync.RWMutex`; mutation methods like `memoryLockStore.PutLock` only acquire `lockStore.mu.Lock()` — they never touch `MemoryMetadataStore.mu`.

Consequences:
1. Data race under `-race` when a concurrent `PutLock` / `PutClientRegistration` / `PutDurableHandle` runs while Backup is running.
2. `root.LockLocks = store.lockStore.locks` aliases the live map; gob iterates it during `Encode`. Concurrent insert during encoding raises a fatal runtime error ("concurrent map iteration and map write").
3. D-02 violation: maps can change between outer RLock acquisition and the gob encode, so the snapshot is not a consistent point-in-time view of lock/client/durable state.

The current `ConcurrentWriter` conformance test only exercises `CreateFile` paths (outer-lock-protected), so CI is green today.

**Fix:** acquire each sub-store's read lock before reading and shallow-clone the maps into the root struct.

### CR-02: Postgres `readBackupArchive` performs unbounded `io.ReadAll` per tar entry (DoS)

**File:** `pkg/metadata/store/postgres/backup.go:487`

`data, err := io.ReadAll(tr)` reads each tar entry fully into memory with no size cap. A malformed archive with a crafted `Size` field (e.g., `1 << 62`) causes up to header-declared-size allocation. Unlike the badger driver (which bounds `headerLen`, `keyLen`, `valLen`), postgres has no upper bound. The whole archive is materialized in `tableBlobs map[string][]byte` before the D-04/D-06 gates run, so a tampered archive can OOM the process.

**Fix:** use `io.LimitReader` with a per-entry cap (e.g., 8 GiB) and reject oversize entries.

## Warnings

### WR-01: Postgres restore silently accepts duplicate tar entries for the same table

**File:** `pkg/metadata/store/postgres/backup.go:498-500`

A tampered archive containing two `tables/files.bin` entries causes the second to silently replace the first. Same concern for `manifest.yaml`. Combined with CR-02, this offers attackers a second-order opportunity.

**Fix:** error on duplicates, wrap with `ErrRestoreCorrupt`.

### WR-02: Postgres restore wraps several corruption errors without `ErrRestoreCorrupt`

**File:** `pkg/metadata/store/postgres/backup.go:267, 270, 273`

Three corruption-class conditions use plain `fmt.Errorf` without wrapping `metadata.ErrRestoreCorrupt`:
- line 267: read-archive error
- line 270-271: unsupported format_version
- line 273-275: engine_kind mismatch

The shared conformance `Corruption` sub-test asserts `errors.Is(err, metadata.ErrRestoreCorrupt)` — postgres integration-tagged runs risk mismatch.

**Fix:** wrap every corruption-class path with `%w: %v` and `metadata.ErrRestoreCorrupt`.

### WR-03: Badger/Postgres restore context-cancellation convention inconsistent with memory

**File:** `pkg/metadata/store/badger/backup.go:355-357, 396-398`

Drivers return raw `ctx.Err()` at start-of-op but wrap mid-stream cancellations with `ErrBackupAborted` inconsistently across drivers. Memory has explicit tests; postgres and badger don't. Document the convention in the `Backupable` interface godoc.

### WR-04: Badger `allBackupPrefixes` has no compile-time guard against >254 entries

**File:** `pkg/metadata/store/badger/backup.go:114-152, 203`

Frame format uses `uint8` for `prefix_idx` with 0xFF reserved as EOF. `streamPrefixForBackup` does `uint8(idx)` with no bounds check. If `allBackupPrefixes` grows past 254 entries, wrap-around or collision with EOF corrupts archives.

**Fix:** `init()` guard panicking if `len(allBackupPrefixes) >= 255`.

### WR-05: Badger Restore partial-write comment slightly overstates hazard

**File:** `pkg/metadata/store/badger/backup.go:351-353`

Current code correctly cancels the batch via `defer wb.Cancel()` on any pre-flush error, so the destination remains empty on CRC mismatch / truncation — matching the conformance test's assertion. Comment can be tightened.

### WR-06: Postgres `scanPayloadIDsTx` relies on convention `content_id` matches `PayloadID`

**File:** `pkg/metadata/store/postgres/backup.go:406-425`

Column name is string-literal; a future migration rename/retype would silently produce wrong PayloadIDSet (violating SAFETY-01). Centralize in a schema constant.

### WR-07: Postgres `readSchemaVersion` called outside restore transaction (TOCTOU)

**File:** `pkg/metadata/store/postgres/backup.go:279-286, 380-388`

Schema-version check runs on a new pool connection before the restore tx begins. A concurrent migration could change the version between check and tx. Phase 5 is expected to manage quiescence; low severity.

**Fix:** move the version read inside the restore transaction for single-snapshot semantics.

### WR-08: Memory `Backup` aliases internal maps into gob encoder

**File:** `pkg/metadata/store/memory/backup.go:161-173`

Safe today under `mu.RLock`; defensive risk if a future refactor adds a sub-lock-only mutation path. Consider shallow-clone via `maps.Clone` for isolation.

## Info

### IN-01: Envelope magic comment shows bytes in big-endian but layout is little-endian

**File:** `pkg/metadata/store/memory/backup.go:18-22`

Code is correct; doc comment shows `0x4d 0x44 0x46 0x53` (big-endian) for a little-endian layout. Update to `0x4d444653 LE` notation.

### IN-02: Badger header uses hardcoded `"v4"` for `BadgerVersion`

**File:** `pkg/metadata/store/badger/backup.go:235`

Static string instead of `debug.ReadBuildInfo` lookup. D-09 intent was build-time derivation.

### IN-03: Postgres TRUNCATE comment is mildly confusing

**File:** `pkg/metadata/store/postgres/backup.go:338-346`

Comment implies D-06 gate is narrowly scoped to `files`; clarify that singleton tables (server_config etc.) still need TRUNCATE.

### IN-04: Memory backup test asserts magic with hardcoded bytes

**File:** `pkg/metadata/store/memory/backup_test.go:139-141`

Derive expected bytes from `binary.LittleEndian.PutUint32` of the magic constant instead.

### IN-05: Postgres backup test `seedShareWithFiles` uses classic for-loop

**File:** `pkg/metadata/store/postgres/backup_test.go:214`

Could migrate to `for i := range n` (Go 1.22+).

### IN-06: Test-package manifest struct mirrors production struct

**File:** `pkg/metadata/store/postgres/backup_test.go:273-285`

Exporting `BackupManifest` would avoid drift.

### IN-07: Badger EOF-marker CRC handling could use a clarifying comment

**File:** `pkg/metadata/store/badger/backup.go:411`

Confirm comment: "EOF marker byte is NOT fed to the CRC (matches writer)".

---

_Reviewed: 2026-04-16_
_Depth: standard_
