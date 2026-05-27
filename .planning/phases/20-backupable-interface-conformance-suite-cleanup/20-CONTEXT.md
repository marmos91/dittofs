# Phase 20: Backupable Interface + Conformance Suite + Cleanup - Context

**Gathered:** 2026-05-27
**Status:** Ready for planning
**GH issue:** [#643](https://github.com/marmos91/dittofs/issues/643)
**Milestone:** v0.16.0 Share Snapshots — Phase 1 of 6
**Depends on:** Nothing (foundation phase)

<domain>
## Phase Boundary

Define the `Backupable` interface contract that all 3 metadata engines will implement (Phase 21), the `HashSet` type consumed by snapshot orchestration and GC hold (Phases 22-23), the shared backup envelope format, and the conformance suite. Clean up orphaned backup code from the never-released v0.13.0 backup system.

**In scope:**
- `Backupable` interface (`Backup`/`Restore` methods)
- `HashSet` concrete type for content hash collection
- Shared envelope format (magic, version, engine tag, CRC32)
- 4 error sentinels
- Conformance suite with 5 subtests
- Delete `internal/cli/backupfmt/` (orphaned)
- Delete planning phases 01-07 (stale v0.13.0 artifacts)

**Out of scope:**
- Per-engine driver implementations (Phase 21)
- Snapshot records, manifest I/O, GC hold (Phase 22)
- Snapshot orchestration and sync gate (Phase 23)
- Restore flow (Phase 24)
- CLI/REST API (Phase 25)

</domain>

<decisions>
## Implementation Decisions

### Serialization contract
- **D-01:** Shared envelope + opaque payload. Small shared header wrapping engine-specific bytes. Each driver writes its own format inside the envelope. Restore reads the header first to detect wrong-engine and version mismatches before touching driver code.
- **D-02:** Envelope includes trailing CRC32 for uniform corruption detection at the envelope level. `ErrRestoreCorrupt` triggers on CRC mismatch or truncation without each driver reimplementing validation.
- **D-03:** HashSet returned separately from the stream. `Backup(ctx, w)` writes only metadata bytes to `w` and returns `HashSet` in-memory. Phase 22 manifest writer owns the on-disk hash format. Clean separation: backup stream is metadata-only.
- **D-04:** Engine tag is a variable-length string (e.g., `"badger"`, `"postgres"`, `"memory"`), not a byte enum. Self-documenting, no registry needed.
- **D-05:** Envelope version tracks envelope framing only (header layout, CRC placement). Engine schema version lives inside the driver's payload. `ErrSchemaVersionMismatch` is returned by the driver, not the envelope layer.
- **D-06:** Envelope code lives in `pkg/metadata/backup/` subpackage. Interface and error sentinels stay in `pkg/metadata/backupable.go`. Clean boundary between contract and wire format.

### HashSet type
- **D-07:** HashSet lives in `pkg/blockstore/hashset.go` alongside `ContentHash` in `types.go`. Logical — operates on the type it wraps. GC hold imports blockstore already.
- **D-08:** Implementation: in-memory `map[ContentHash]struct{}`. O(1) Add/Contains. ~3.2 MB for 100k hashes (well within RAM for VM workloads).
- **D-09:** Concrete struct, not interface. "Less is more" — no current consumer needs polymorphism. Refactor cost near-zero (no prod users).
- **D-10:** Caller-synchronized (no internal lock). All usage patterns are single-goroutine: backup under engine's transactional lock, GC hold reads, manifest writes.
- **D-11:** Exposes `Sorted()` returning `[]ContentHash` — convenience for Phase 22 manifest writer. Single `slices.SortFunc` call; allocates new slice each call, no caching.
- **D-12:** No `MarshalBinary`/`UnmarshalBinary`. HashSet is pure RAM type. Phase 22 owns the on-disk manifest format.
- **D-13:** Methods: `Add`, `Contains`, `Len`, `ForEach`, `Sorted`, `Hashes` (returns underlying map for direct iteration).

### Conformance suite
- **D-14:** Separate `RunBackupConformanceSuite` function in `pkg/metadata/storetest/backup_conformance.go`. Not extending existing `RunConformanceSuite`. Takes a `BackupableStoreFactory` returning stores that implement both `MetadataStore` and `Backupable`.
- **D-15:** Corruption subtest: 3 scenarios — truncated stream (`ErrRestoreCorrupt`), single bit-flip (`ErrRestoreCorrupt` via CRC mismatch), wrong engine tag in header.
- **D-16:** ConcurrentWriter subtest: verifies snapshot isolation. Concurrent writes during backup must NOT appear in restored data. Proves ENG-02 "same atomic snapshot transaction" is real.
- **D-17:** HashSetCorrectness subtest: two scenarios — exact hash match against manual store walk + dedup verification (shared blocks counted once in HashSet).

### Interface shape
- **D-18:** `Backupable` is a standalone interface, not embedded in or extending `MetadataStore`. Drivers implement both interfaces. Call sites use type assertion (`store.(Backupable)`). Matches existing optional-capability pattern (`ObjectIDIndexAccessor`, `FileBlockRefsAccessor`).
- **D-19:** Error sentinels are plain `var + errors.New` — matches existing `metadata.ExportError` patterns. Drivers wrap with `fmt.Errorf` for context.

### Archive strategy
- **D-20:** Delete `.planning/phases/01-*` through `07-*` entirely. They're stale v0.13.0 backup planning artifacts from a never-released feature. Git history is the archive.

### PR shape
- **D-21:** Single PR against develop, staged commits: (1) HashSet type, (2) Backupable interface + error sentinels + envelope package, (3) conformance suite, (4) backupfmt deletion + planning phase cleanup. Each commit builds independently.

### Claude's Discretion
- D-03 (HashSet returned separately, not in stream) — keeps Backup stream metadata-only; Phase 22 owns manifest format
- D-09 (concrete struct over interface) — "less is more" principle
- D-10 (caller-synchronized) — no concurrent usage pattern exists
- D-11 (Sorted() method) — avoids duplicate sort logic in every consumer
- D-14 (separate RunBackupConformanceSuite) — existing suite has 7 sections, type assertion skip is fragile
- D-18 (standalone Backupable interface) — matches existing optional-capability patterns
- D-19 (plain sentinel vars) — matches existing error patterns

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Requirements and design
- `.planning/REQUIREMENTS.md` §v0.16.0 Requirements — ENG-01..04, CLN-01..02 are Phase 20's requirements
- `.planning/ROADMAP.md` §Phase 20 — success criteria and files-to-touch list

### Existing code patterns
- `pkg/metadata/storetest/suite.go` — existing conformance suite pattern (StoreFactory, RunConformanceSuite structure)
- `pkg/metadata/store.go` — MetadataStore interface and sub-interfaces; Backupable must NOT extend these
- `pkg/blockstore/types.go` — `ContentHash` type definition; HashSet lives alongside this
- `pkg/metadata/storetest/file_block_ops.go` — example of optional-capability conformance (FileBlockRefsAccessor type assertion)

### GC integration (consumed by Phase 22)
- `pkg/blockstore/engine/gc.go` — mark-sweep GC; Phase 22 adds SnapshotHoldProvider here
- `pkg/blockstore/engine/gc_test.go:396` — `TestGCMarkSweep_NoBackupHoldProvider` verifies no BackupHoldProvider leak

### Cleanup targets
- `internal/cli/backupfmt/` — orphaned package to delete (format.go + format_test.go)
- `.planning/phases/01-*` through `07-*` — stale v0.13.0 planning artifacts to delete

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/metadata/storetest/suite.go` — `StoreFactory` pattern reusable for `BackupableStoreFactory`
- `pkg/blockstore/types.go` — `ContentHash` type with `String()`, `CASKey()` methods already defined
- `pkg/metadata/storetest/file_block_ops.go` — type-assertion-based capability probe pattern for optional interfaces

### Established Patterns
- Error sentinels as `var ErrX = errors.New(...)` in `pkg/metadata/` — Backupable errors follow same pattern
- Optional interfaces with type assertion at call sites (`ObjectIDIndexAccessor`, `FileBlockRefsAccessor`)
- Conformance suites as exported `Run*Suite` functions in `storetest/`

### Integration Points
- `pkg/metadata/backupable.go` (new) — interface consumed by all 3 metadata store implementations in Phase 21
- `pkg/metadata/backup/` (new subpackage) — envelope code imported by all drivers for header/CRC operations
- `pkg/blockstore/hashset.go` (new) — consumed by Phase 22 GC hold + Phase 23 sync gate + Phase 22 manifest writer
- `pkg/metadata/storetest/backup_conformance.go` (new) — called by each driver's test file in Phase 21

</code_context>

<specifics>
## Specific Ideas

No specific requirements — open to standard approaches following established patterns.

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 20-Backupable Interface + Conformance Suite + Cleanup*
*Context gathered: 2026-05-27*
