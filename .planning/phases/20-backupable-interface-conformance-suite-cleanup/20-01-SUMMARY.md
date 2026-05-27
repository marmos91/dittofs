---
phase: 20-backupable-interface-conformance-suite-cleanup
plan: 01
subsystem: metadata-backup
tags: [backup, types, interface, envelope, crc32]
dependency_graph:
  requires: []
  provides: [blockstore.HashSet, metadata.Backupable, backup.envelope]
  affects: [pkg/blockstore, pkg/metadata, pkg/metadata/backup]
tech_stack:
  added: []
  patterns: [optional-capability-interface, streaming-crc32, error-sentinels]
key_files:
  created:
    - pkg/blockstore/hashset.go
    - pkg/blockstore/hashset_test.go
    - pkg/metadata/backupable.go
    - pkg/metadata/backupable_test.go
    - pkg/metadata/backup/envelope.go
    - pkg/metadata/backup/envelope_test.go
  modified: []
decisions:
  - "HashSet is a concrete struct with map[ContentHash]struct{}, caller-synchronized, no MarshalBinary"
  - "Backupable is standalone interface, not embedded in MetadataStore — discovered via type assertion"
  - "Envelope package in pkg/metadata/backup/ with zero imports of pkg/metadata — avoids cycle"
  - "Castagnoli CRC32 for hw acceleration on arm64/amd64, matching appendlog.go pattern"
metrics:
  duration_seconds: 215
  completed: 2026-05-27T10:00:08Z
  tasks_completed: 2
  tasks_total: 2
  files_created: 6
  files_modified: 0
---

# Phase 20 Plan 01: Foundational Backup Types Summary

HashSet collection, Backupable interface with 4 error sentinels, and streaming CRC32 envelope format for metadata backup streams.

## What Was Done

### Task 1: HashSet type + Backupable interface + error sentinels (a4512fae)

Created `pkg/blockstore/hashset.go` with the `HashSet` concrete struct wrapping `map[ContentHash]struct{}`. Six methods: `Add`, `Contains`, `Len`, `ForEach`, `Sorted` (via `slices.SortFunc` + `bytes.Compare`), and `Hashes` (returns internal map). Caller-synchronized per D-10, no MarshalBinary per D-12.

Created `pkg/metadata/backupable.go` with the `Backupable` interface (standalone, NOT embedded in `MetadataStore`). Two methods: `Backup(ctx, w) (*blockstore.HashSet, error)` and `Restore(ctx, r) error`. Four error sentinels using `errors.New` with `metadata:` prefix matching the `rollup_store.go` convention.

Tests: 6 HashSet test functions + 5 sentinel tests (4 wrap-through + 1 cross-match exclusion).

### Task 2: Envelope binary format package (0bcc4c44)

Created `pkg/metadata/backup/envelope.go` with the shared binary envelope format. Wire format: 4-byte magic "DFBK" + uint32 LE version + uint16 LE engine tag length + engine tag bytes + variable payload + trailing uint32 LE CRC32 (Castagnoli).

Streaming API: `NewWriter` writes header and returns a `Writer` that accumulates CRC; `ReadHeader` validates header and returns a tee reader that accumulates CRC; `VerifyCRC` checks trailing checksum; `VerifyEngine` validates engine tag match.

Five error sentinels: `ErrBadMagic`, `ErrUnsupportedVersion`, `ErrEngineMismatch`, `ErrTruncated`, `ErrCRCMismatch`. Zero imports of `pkg/metadata` — no import cycle.

Tests: 6 test functions covering round-trip, bad magic, bad version, truncation, bit-flip corruption detection, and engine mismatch.

## Deviations from Plan

None - plan executed exactly as written.

## Verification Results

| Check | Result |
|-------|--------|
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test ./pkg/blockstore/ -run HashSet` | 6/6 PASS |
| `go test ./pkg/metadata/ -run Err` | 5/5 PASS |
| `go test ./pkg/metadata/backup/` | 6/6 PASS |
| Backupable NOT in MetadataStore | Confirmed (0 matches) |
| No import cycle | Confirmed (envelope.go has 0 imports of pkg/metadata) |

## Commit Log

| Task | Commit | Message |
|------|--------|---------|
| 1 | a4512fae | feat(20-01): add HashSet type, Backupable interface, and error sentinels |
| 2 | 0bcc4c44 | feat(20-01): add envelope binary format for backup streams |
