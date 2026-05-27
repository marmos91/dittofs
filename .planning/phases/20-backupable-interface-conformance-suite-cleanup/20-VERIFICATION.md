---
phase: 20-backupable-interface-conformance-suite-cleanup
verified: 2026-05-27T10:36:12Z
status: passed
score: 6/6
overrides_applied: 0
---

# Phase 20: Backupable Interface + Conformance Suite + Cleanup Verification Report

**Phase Goal:** Define the Backupable interface, error taxonomy, HashSet type, shared conformance suite, and clean up legacy backup artifacts.
**Verified:** 2026-05-27T10:36:12Z
**Status:** PASSED
**Re-verification:** No -- initial verification

## Goal Achievement

### Observable Truths (Roadmap Success Criteria)

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| SC-1 | Backupable interface exists in pkg/metadata/backupable.go with Backup(ctx, w) (HashSet, error) and Restore(ctx, r) error | VERIFIED | File exists (64 lines), exports Backupable interface with `Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error)` and `Restore(ctx context.Context, r io.Reader) error`. NOT embedded in MetadataStore (grep returns 0 matches). |
| SC-2 | HashSet type exists in pkg/blockstore/hashset.go with Add/Contains/Len/ForEach methods | VERIFIED | File exists (71 lines), concrete struct with `map[ContentHash]struct{}`. Exports: NewHashSet, Add, Contains, Len, ForEach, Sorted, Hashes. Uses `slices.SortFunc` + `bytes.Compare` for Sorted(). All 6 tests PASS. |
| SC-3 | Four error sentinels pass errors.Is round-trip tests | VERIFIED | ErrRestoreDestinationNotEmpty, ErrRestoreCorrupt, ErrSchemaVersionMismatch, ErrBackupAborted all defined via `errors.New` with `metadata:` prefix. `TestBackupSentinels_DetectThroughWrap` passes all 4 through `fmt.Errorf("%w")` wrapping. `TestBackupSentinels_NoCrossMatch` verifies mutual exclusion. 5/5 tests PASS. |
| SC-4 | Shared conformance suite with 5 subtests compiles and is ready for engine drivers | VERIFIED | backup_conformance.go (607 lines) exports BackupableStoreFactory and RunBackupConformanceSuite. 5 top-level t.Run calls: RoundTrip, ConcurrentWriter, Corruption (3 sub-scenarios), NonEmptyDest, HashSetCorrectness (2 sub-scenarios). Type assertion to metadata.Backupable at entry. `go build ./pkg/metadata/storetest/...` exits 0. |
| SC-5 | internal/cli/backupfmt/ deleted (orphaned, zero imports) | VERIFIED | Directory does not exist. `grep -rn 'backupfmt' --include='*.go'` returns 0 matches. `go build ./...` exits 0 after deletion. Commit 231f4c00 confirms deletion. |
| SC-6 | Old backup planning phases (01-07) archived/deleted | VERIFIED | `.planning/phases/0[1-7]-*` returns "no matches". Phases 08+ and 20+ preserved. D-20 decision explicitly chose deletion over milestones archive ("git history is the archive"). Commit db6dd776 confirms deletion of 105 files across 7 directories. |

**Score:** 6/6 truths verified

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/blockstore/hashset.go` | HashSet concrete struct with map[ContentHash]struct{} | VERIFIED | 71 lines, exports NewHashSet + 6 methods (Add, Contains, Len, ForEach, Sorted, Hashes). No TODO/FIXME markers. |
| `pkg/blockstore/hashset_test.go` | 6 test functions covering all HashSet methods | VERIFIED | 129 lines, 6 test functions: Add_Contains, Len, ForEach, ForEach_ErrorPropagation, Sorted, Hashes. All PASS. |
| `pkg/metadata/backupable.go` | Backupable interface + 4 error sentinels | VERIFIED | 64 lines, standalone interface with godoc. 4 sentinels with errors.New. No import of MetadataStore embedding. |
| `pkg/metadata/backupable_test.go` | Sentinel errors.Is round-trip tests | VERIFIED | 48 lines, external test package (metadata_test). Tests wrapping + cross-match exclusion. |
| `pkg/metadata/backup/envelope.go` | Envelope wire format helpers | VERIFIED | 207 lines. Exports: NewWriter, ReadHeader (with accumulated hash.Hash32 return), VerifyCRC, VerifyEngine, EnvelopeVersion. 5 error sentinels. CRC32 Castagnoli via crc32.MakeTable. Zero imports of pkg/metadata. |
| `pkg/metadata/backup/envelope_test.go` | 6 test functions covering envelope scenarios | VERIFIED | 124 lines, 6 test functions: RoundTrip, BadMagic, BadVersion, Truncated, BitFlip, EngineMismatch. All PASS. |
| `pkg/metadata/storetest/backup_conformance.go` | RunBackupConformanceSuite with 5 subtests | VERIFIED | 607 lines, exports BackupableStoreFactory and RunBackupConformanceSuite. 5 top-level + 5 sub-level t.Run calls. Compiles without any driver. |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| pkg/metadata/backupable.go | pkg/blockstore/hashset.go | Backup return type *blockstore.HashSet | WIRED | Line 34: `Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error)` |
| pkg/metadata/backup/envelope.go | hash/crc32 | Castagnoli CRC32 table | WIRED | Line 34: `crcTable = crc32.MakeTable(crc32.Castagnoli)` |
| backup_conformance.go | metadata.Backupable | type assertion store.(metadata.Backupable) | WIRED | Lines 40, 67-69: type assertion at suite entry and in asBackupable helper |
| backup_conformance.go | backup.NewWriter/ReadHeader | envelope write/read for corruption tests | WIRED | Lines 379, 387: used in WrongEngineTag corruption subtest |
| backup_conformance.go | blockstore.HashSet | HashSet verification in subtests | WIRED | Lines 234, 542-551: HashSet.Len() and Contains() checked in RoundTrip and HashSetCorrectness |
| pkg/metadata/backup/envelope.go | pkg/metadata (NO import) | must NOT import to avoid cycle | VERIFIED | grep returns 0 matches for pkg/metadata import in envelope.go. `go build ./...` confirms no cycle. |

### Data-Flow Trace (Level 4)

Not applicable -- this phase creates types, interfaces, and test infrastructure. No dynamic data rendering or user-visible output.

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|----------|---------|--------|--------|
| HashSet all tests pass | `go test ./pkg/blockstore/ -run HashSet -count=1` | 6/6 PASS (0.238s) | PASS |
| Sentinel errors.Is tests pass | `go test ./pkg/metadata/ -run Err -count=1` | All PASS (0.215s) | PASS |
| Envelope all tests pass | `go test ./pkg/metadata/backup/ -count=1` | 6/6 PASS (0.195s) | PASS |
| Full project builds | `go build ./...` | exit 0 | PASS |
| go vet clean | `go vet ./pkg/blockstore/ ./pkg/metadata/ ./pkg/metadata/backup/ ./pkg/metadata/storetest/` | exit 0 | PASS |
| Conformance suite compiles | `go build ./pkg/metadata/storetest/...` | exit 0 | PASS |
| backupfmt directory absent | `ls internal/cli/backupfmt/` | No such file or directory | PASS |
| Phases 01-07 absent | `ls .planning/phases/0[1-7]-*` | no matches | PASS |
| Phases 08+ preserved | `ls .planning/phases/08-*` | exists | PASS |

### Probe Execution

Step 7c: SKIPPED (no probes declared or found for Phase 20)

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|------------|-------------|--------|----------|
| ENG-01 | 20-01 | Backupable interface with Backup/Restore signatures | SATISFIED | pkg/metadata/backupable.go exports Backupable with correct signatures |
| ENG-02 | 20-01 | HashSet type captures unique ContentHash values from Blocks inside snapshot transaction | SATISFIED | pkg/blockstore/hashset.go implements HashSet with map[ContentHash]struct{}. Conformance suite RoundTrip/HashSetCorrectness subtests verify correctness. Actual atomic transaction behavior is per-driver (Phase 21). |
| ENG-03 | 20-01 | Four typed error sentinels | SATISFIED | All 4 sentinels exported from backupable.go. errors.Is round-trip tests pass. |
| ENG-04 | 20-02 | Shared conformance suite with 5 subtests | SATISFIED | backup_conformance.go has 5 subtests (RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest, HashSetCorrectness). Compiles independently. |
| CLN-01 | 20-03 | Delete orphaned backupfmt package | SATISFIED | Directory deleted, zero remaining Go references, build clean. |
| CLN-02 | 20-03 | Archive old backup planning phases (01-07) | SATISFIED | Phases deleted per D-20 decision ("git history is the archive"). All 7 directories removed. Active phases preserved. REQUIREMENTS.md says "archive to milestones directory" but research decision D-20 explicitly chose deletion. |

No orphaned requirements found -- all 6 Phase 20 requirement IDs (ENG-01 through ENG-04, CLN-01, CLN-02) are accounted for.

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| (none) | - | - | - | All 4 production files and conformance suite are clean of TBD/FIXME/XXX/TODO/HACK/PLACEHOLDER markers. No empty returns or stub patterns detected. |

### Human Verification Required

No items require human verification. All deliverables are verifiable programmatically:
- Types and interfaces are checked via `go build`
- Test correctness is checked via `go test`
- Deletions are checked via file system inspection
- No UI, visual, or real-time behavior to verify

### Gaps Summary

No gaps found. All 6 roadmap success criteria are met. All 6 requirement IDs are satisfied. All artifacts exist, are substantive, and are properly wired. All tests pass. Full project build is clean.

---

_Verified: 2026-05-27T10:36:12Z_
_Verifier: Claude (gsd-verifier)_
