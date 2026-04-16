---
phase: 02-per-engine-backup-drivers
plan: 03
subsystem: database
tags: [badger, backup, restore, snapshot, crc32, metadata-store, eng-01]

# Dependency graph
requires:
  - phase: 02-per-engine-backup-drivers
    provides: metadata.Backupable interface, Phase-2 error sentinels, shared storetest.RunBackupConformanceSuite (plan 02-01)
provides:
  - BadgerMetadataStore.Backup producing a consistent snapshot inside one db.View txn (D-02 + D-03)
  - BadgerMetadataStore.Restore decoding the framed archive into an empty destination (D-06)
  - Length-prefixed wire format with JSON header, 0xFF EOF marker, CRC32/IEEE trailer (D-09)
  - allBackupPrefixes — authoritative catalogue of 25 Badger key prefixes
affects: [03-destination-drivers, 05-restore-orchestration, 07-chaos-testing]

# Tech tracking
tech-stack:
  added: [hash/crc32 (stdlib), encoding/binary framing]
  patterns:
    - "Same-snapshot PayloadIDSet + key/value stream inside one s.db.View closure (D-02 + D-03)"
    - "Disambiguate EOF marker from prefix_idx via reserved 0xFF byte — no ambiguity parsing"
    - "Post-restore recomputation of in-memory derived state (usedBytes counter) to avoid restart"

key-files:
  created:
    - pkg/metadata/store/badger/backup.go
    - pkg/metadata/store/badger/backup_test.go
  modified: []

key-decisions:
  - "Drive streaming inside s.db.View(...) and bypass badger's DB-level streaming wrapper so the PayloadID scan and key/value emission share one read-ts (D-02 + D-03 binding)"
  - "Place EOF marker (0xFF) before the CRC trailer so the decoder can disambiguate it from any prefix_idx with a single byte-compare (allBackupPrefixes has 25 entries, far below 255)"
  - "Compute CRC32/IEEE over all frame bytes — deviation from the plan's baseline design, required to make SingleByteFlip conformance subtest deterministic (bit flip in a value would otherwise pass silently)"
  - "Rebuild usedBytes counter inside Restore via initUsedBytesCounter so stats reporting is correct without requiring a process restart"
  - "allBackupPrefixes is a const-ordered slice; the index in this slice becomes prefix_idx, so any future prefix MUST be appended at the end to preserve wire-format compatibility with archives produced by older binaries"

patterns-established:
  - "Pattern: storage driver exposing Backupable implements it in its own file (backup.go) with a compile-time assertion `var _ metadata.Backupable = (*XxxStore)(nil)`"
  - "Pattern: restore empty-dest check runs as a read-only probe BEFORE touching the reader — so a bug invoking Restore against a live store fails with a recognisable sentinel instead of consuming bytes"
  - "Pattern: CRC32 trailer over frame bytes for integrity; cheap per-frame via io.MultiWriter feeding a crc32 hasher in parallel with the archive writer"

requirements-completed: [ENG-01]

# Metrics
duration: ~20min
completed: 2026-04-16
---

# Phase 02 Plan 03: Badger Backup Driver Summary

**Badger metadata-store Backup/Restore driver with custom length-prefixed framing, CRC32 trailer, and same-snapshot PayloadIDSet inside a single s.db.View txn (ENG-01).**

## Performance

- **Duration:** ~20 min
- **Started:** 2026-04-16T08:02:00Z (worktree reset)
- **Completed:** 2026-04-16T08:23:13Z
- **Tasks:** 2
- **Files created:** 2

## Accomplishments

- `*BadgerMetadataStore` now satisfies `metadata.Backupable` at compile time via `var _ metadata.Backupable = (*BadgerMetadataStore)(nil)`.
- Backup streams a consistent snapshot of the entire database inside a single `s.db.View(...)` closure. Both the PayloadIDSet scan (over `prefixFile`) and the key/value emission loop share one read-timestamp — satisfying D-02's same-snapshot invariant without invoking badger's DB-level streaming wrapper (D-03 prohibition).
- Restore rejects non-empty destinations with `metadata.ErrRestoreDestinationNotEmpty` (D-06), rejects archives citing unknown prefixes or unsupported format versions with `metadata.ErrRestoreCorrupt` (D-09), and enforces per-frame size bounds (1 MiB keys / 1 GiB values) to prevent oversize-allocation DoS (T-02-03-04).
- All 5 shared conformance subtests pass under `-tags=integration`, including three `Corruption` variants (HeaderTruncated, BodyTruncated, SingleByteFlip).

## Task Commits

1. **Task 1: Implement Backup/Restore in backup.go** — `dc6b4294` (feat)
2. **Task 2: Wire backup_test.go under integration tag** — `5babf128` (test)

## Files Created

- `pkg/metadata/store/badger/backup.go` (525 lines) — `Backup`, `Restore`, wire-format constants, prefix catalogue, helpers.
- `pkg/metadata/store/badger/backup_test.go` (35 lines) — `//go:build integration` test that plumbs `storetest.RunBackupConformanceSuite` into a fresh Badger store per sub-test.

## Authoritative Prefix Catalogue (D-01)

`allBackupPrefixes` enumerates 25 prefixes, audited by grep at execution time across every `pkg/metadata/store/badger/*.go`:

| # | Prefix           | Source file          | Notes                                      |
|---|------------------|----------------------|--------------------------------------------|
| 0 | `f:`             | encoding.go          | File (JSON)                                |
| 1 | `p:`             | encoding.go          | parent UUID index                          |
| 2 | `c:`             | encoding.go          | directory child map                        |
| 3 | `s:`             | encoding.go          | share root handle (JSON)                   |
| 4 | `l:`             | encoding.go          | link count (uint32 BE)                     |
| 5 | `d:`             | encoding.go          | device number (JSON)                       |
| 6 | `cfg:`           | encoding.go          | server config singleton                    |
| 7 | `cap:`           | encoding.go          | filesystem capabilities singleton          |
| 8 | `lock:`          | locks.go             | LockStore primary records                  |
| 9 | `lkfile:`        | locks.go             | index: file → locks                        |
|10 | `lkowner:`       | locks.go             | index: owner → locks                       |
|11 | `lkclient:`      | locks.go             | index: client → locks                      |
|12 | `srvepoch`       | locks.go             | singleton; no separator                    |
|13 | `nsm:client:`    | clients.go           | NSM client registrations                   |
|14 | `nsm:monname:`   | clients.go           | index: monitor name → client               |
|15 | `dh:id:`         | durable_handles.go   | SMB3 durable handle primary                |
|16 | `dh:cguid:`      | durable_handles.go   | index: create-guid → id                    |
|17 | `dh:appid:`      | durable_handles.go   | index: app-instance-id → id                |
|18 | `dh:fid:`        | durable_handles.go   | index: file-id → id                        |
|19 | `dh:fh:`         | durable_handles.go   | index: file-handle → id                    |
|20 | `dh:share:`      | durable_handles.go   | index: share → id                          |
|21 | `fb:`            | objects.go           | FileBlock primary                          |
|22 | `fb-hash:`       | objects.go           | index: content hash → id                   |
|23 | `fb-local:`      | objects.go           | index: local cache key → id                |
|24 | `fb-file:`       | objects.go           | index: file → block(s)                     |
|25 | `fsmeta:`        | transaction.go       | per-share filesystem meta (seeded lazily)  |

**Prefixes in code but excluded from backup:** none. Every `prefix*` constant and every `fileBlock*Prefix` constant declared in `pkg/metadata/store/badger/*.go` at audit time is captured.

**Prefixes mentioned in D-01's CONTEXT list but not in code:** `fsmeta:` was listed in CONTEXT.md D-01 but exists only in `transaction.go:592` as `prefixFilesystemMeta` — it IS captured in `allBackupPrefixes`. There is no drift between D-01 and the implementation.

## Wire Format Specification

```
┌────────────────────────────────────────────────────────────────────┐
│ header_len: uint32 BE                                              │
│ header:     JSON (badgerBackupHeader)                              │
│ frames:     repeated {                                             │
│                 prefix_idx: uint8 (0..254; 0xFF reserved as EOF)   │
│                 key_len:    uint32 BE (bounded ≤ 1 MiB)            │
│                 key:        [key_len]byte                          │
│                 value_len:  uint32 BE (bounded ≤ 1 GiB)            │
│                 value:      [value_len]byte                        │
│             }                                                      │
│ eof_marker: uint8 = 0xFF                                           │
│ trailer:    uint32 BE CRC-32/IEEE over all frame bytes             │
└────────────────────────────────────────────────────────────────────┘
```

`badgerBackupHeader` JSON fields:

```json
{
  "format_version": 1,
  "badger_version": "v4",
  "key_prefix_list": ["f:", "p:", ...],
  "created_at": "2026-04-16T08:22:47.219Z"
}
```

`format_version == 1` is the only version emitted by this driver. `Restore` accepts `format_version` up to `badgerFormatVersion` and rejects unknown values with `ErrRestoreCorrupt`. New fields added to `badgerBackupHeader` MUST carry the `omitempty` tag to remain compatible with archives produced by older binaries in the same major wire-format version.

## Conformance Subtest Results

All 5 conformance scenarios pass with `-tags=integration`:

| Subtest                     | Duration | Outcome                                                                   |
|-----------------------------|----------|---------------------------------------------------------------------------|
| RoundTrip                   | 0.08s    | PASS — populated tree round-trips with full PayloadIDSet + share tree     |
| ConcurrentWriter            | 0.08s    | PASS — parallel `db.Update` during `db.View` honours SSI snapshot          |
| Corruption/HeaderTruncated  | 0.04s    | PASS — short archive surfaces `ErrRestoreCorrupt`, destination untouched  |
| Corruption/BodyTruncated    | 0.04s    | PASS — mid-frame truncation surfaces `ErrRestoreCorrupt`                  |
| Corruption/SingleByteFlip   | 0.05s    | PASS — flipped byte detected by CRC32 trailer → `ErrRestoreCorrupt`       |
| NonEmptyDest                | 0.09s    | PASS — populated destination rejects Restore with `ErrRestoreDestinationNotEmpty`, pre-existing data intact |
| PayloadIDSet                | 0.08s    | PASS — set returned by Backup equals set enumerable after Restore          |

Total suite time: 0.905s. Real concurrent writer commits during `ConcurrentWriter` are observable in the writer goroutine's `i` counter growing past zero before backup completes; SSI ensures they land with a later read-ts and stay out of the archive.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing Critical] Added CRC32/IEEE trailer over frame bytes**

- **Found during:** Task 1 design review (before writing test file)
- **Issue:** The plan's wire format (header + frames + `0xFF` EOF marker) contained no integrity check. The conformance `Corruption/SingleByteFlip` subtest XORs one byte at `len(good)/2`; that byte can land in a frame's value bytes, which the plan's decoder would accept silently because raw bytes are not re-validated against any schema — restore would "succeed" on a corrupt archive.
- **Fix:** Added a 4-byte CRC-32/IEEE trailer after the EOF marker. Encoder feeds frame bytes through an `io.MultiWriter` into both `w` and a running `hash/crc32.NewIEEE()`; trailer emits `crc.Sum32()` right after the 0xFF marker. Decoder reads the trailer after consuming the marker and validates.
- **Files modified:** `pkg/metadata/store/badger/backup.go` — added `hash/crc32` import; trailer layout; decoder CRC check with `ErrRestoreCorrupt` on mismatch.
- **Verification:** `Corruption/SingleByteFlip` subtest passes deterministically (tested via conformance suite).
- **Committed in:** `dc6b4294` (Task 1 commit)

**2. [Rule 2 - Missing Critical] Rebuild usedBytes counter after Restore**

- **Found during:** Task 1 implementation, reviewing `BadgerMetadataStore` struct fields
- **Issue:** The store's `usedBytes atomic.Int64` is initialised once at construction via `initUsedBytesCounter()` (a full `prefixFile` scan). Restore writes raw key/value pairs straight through `NewWriteBatch` — the in-memory counter never updates. Post-restore `GetUsedBytes()` would return 0 until a server restart, breaking FSSTAT and any quota accounting.
- **Fix:** Call `s.initUsedBytesCounter()` after `wb.Flush()` succeeds; mirrors the constructor's invocation path.
- **Files modified:** `pkg/metadata/store/badger/backup.go` — final block of `Restore`.
- **Verification:** Not directly asserted by the Phase-2 conformance suite (which does not query `GetUsedBytes`), but the call is cheap and closes an observable behaviour gap Phase 5's orchestration will rely on.
- **Committed in:** `dc6b4294` (Task 1 commit)

**3. [Rule 2 - Missing Critical] Bounded `valLen == 0` as valid; rejected `keyLen == 0`**

- **Found during:** Task 1 implementation (frame decoder)
- **Issue:** The plan's bounds check (`keyLen > 1<<20 || valLen > 1<<30`) did not guard against `keyLen == 0`. A zero-length key would pass validation and then `bytes.HasPrefix(key, expectedPrefix)` would return `false` only if `len(expectedPrefix) > 0` — but `prefixServerEpoch == "srvepoch"` has `len > 0`, so the check would catch it. Still, an explicit `keyLen == 0` rejection is clearer and defensive. `valLen == 0` IS valid (some index entries store only keys), so we accept it but skip the `io.ReadFull` of the empty value.
- **Fix:** Added `keyLen == 0` to the bounds rejection; wrapped the value read in `if valLen > 0`.
- **Files modified:** `pkg/metadata/store/badger/backup.go` — `Restore` frame decode loop.
- **Verification:** Covered indirectly by `Corruption/SingleByteFlip` (a flip that reduces `keyLen` to zero would trip this branch).
- **Committed in:** `dc6b4294` (Task 1 commit)

---

**Total deviations:** 3 auto-fixed (all Rule 2 — missing critical functionality for correctness).
**Impact on plan:** Every auto-fix is a correctness requirement. CRC trailer is load-bearing for `SingleByteFlip` conformance; usedBytes recompute is load-bearing for Phase 5's orchestration; zero-length guard is defence-in-depth. No scope creep.

## Issues Encountered

- **Comment text tripping the D-03 acceptance grep.** My initial docstring described the prohibition by naming the banned function call pattern (`s.db.Backup(w, since)`), which caused the `! grep -nE 's\.db\.Backup\(|s\.db\.Load\(' pkg/metadata/store/badger/backup.go` acceptance gate to flag the comment as a violation. Resolved by rewording the docstring to describe the prohibition without emitting the literal call syntax. No behaviour change.

## User Setup Required

None — no external service configuration required.

## Recommendation for a Future Milestone Plan

Add a `go vet`-style or `go generate`-style sanity test that fails when a new `prefix*` constant (or `fileBlock*Prefix` constant) is added anywhere under `pkg/metadata/store/badger/` without being appended to `allBackupPrefixes` in `backup.go`. Shape:

```go
func TestAllBackupPrefixes_IsExhaustive(t *testing.T) {
    // 1. AST-parse every *.go file in the package
    // 2. Collect every top-level const decl whose value matches /^[a-z][a-z-]*:?$/
    //    (the prefix pattern — colon-terminated string literal or "srvepoch")
    // 3. Assert every collected value appears in allBackupPrefixes
}
```

Defence against the D-09 prefix-completeness drift risk (T-02-03-08 in the threat model). Cheap, catches drift at CI time rather than at a DR-drill time. Ship it in a future hygiene milestone.

## Next Phase Readiness

- Plan 02-04 (Postgres driver) and plan 02-02 (Memory driver) can now rely on the same shared `storetest.RunBackupConformanceSuite` — the Badger implementation has served as the first real exercise of that suite, confirming all 5 subtests are well-formed.
- Phase 3 (destination drivers) can now invoke `BadgerMetadataStore.Backup(w)` and wrap the resulting stream in tar/S3/local-FS envelopes. The archive is self-contained (header + frames + CRC + EOF marker) and byte-identical across runs modulo `created_at`.
- Phase 5 (restore orchestration) has a deterministic `ErrRestoreDestinationNotEmpty` gate to lean on.

## Self-Check: PASSED

All created files exist on disk:
- `pkg/metadata/store/badger/backup.go` — FOUND (525 lines)
- `pkg/metadata/store/badger/backup_test.go` — FOUND (35 lines)

Both task commits exist in git log:
- `dc6b4294` — FOUND (feat(02-03): badger Backup/Restore driver (ENG-01))
- `5babf128` — FOUND (test(02-03): wire badger backup conformance suite under integration tag)

D-03 prohibition verified (zero matches):
```
grep -nE 's\.db\.Backup\(|s\.db\.Load\(' pkg/metadata/store/badger/backup.go
```

All 5 conformance subtests PASS under `-tags=integration`; default `go test ./...` reports "no test files" for the package (build tag correctly gates).

---
*Phase: 02-per-engine-backup-drivers*
*Plan: 03 (Badger driver)*
*Completed: 2026-04-16*
