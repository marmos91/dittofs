# Phase 20: Backupable Interface + Conformance Suite + Cleanup - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-27
**Phase:** 20-Backupable Interface + Conformance Suite + Cleanup
**Areas discussed:** Serialization contract, HashSet placement + semantics, Conformance suite scope, Archive strategy, Code structure

---

## Serialization Contract

### Q1: Shared envelope vs opaque per-engine

| Option | Description | Selected |
|--------|-------------|----------|
| Opaque per-engine | Each driver owns its format. ErrRestoreCorrupt detection is driver-internal. | |
| Shared envelope + opaque payload | Small shared header wrapping engine-specific payload. Restore reads header first. | ✓ |
| You decide | Claude picks based on 'less is more' principle. | |

**User's choice:** Shared envelope + opaque payload

### Q2: Envelope minimality

| Option | Description | Selected |
|--------|-------------|----------|
| Magic + version + engine tag only | ~8 bytes, no CRC in envelope | |
| Magic + version + engine + trailing CRC32 | Uniform corruption detection at envelope level | ✓ |
| You decide | Claude picks simplest satisfying ENG-03 | |

**User's choice:** Magic + version + engine + trailing CRC32

### Q3: HashSet in stream or separate

| Option | Description | Selected |
|--------|-------------|----------|
| Two-section stream | Envelope = header + metadata payload + hash manifest section | |
| Metadata-only stream, HashSet returned separately | Backup writes only metadata to w, returns HashSet in-memory | |
| You decide | Claude picks | ✓ |

**User's choice:** You decide → Claude chose metadata-only stream (D-03)

### Q4: Engine tag format

| Option | Description | Selected |
|--------|-------------|----------|
| Byte enum (1=memory, 2=badger, 3=postgres) | Simple, compact, 255 slots | |
| String tag (variable-length engine name) | More readable, self-documenting | ✓ |
| You decide | Claude picks simpler | |

**User's choice:** String tag

### Q5: Version semantics

| Option | Description | Selected |
|--------|-------------|----------|
| Envelope-only version | Version = envelope framing. Engine schema version in payload. | ✓ |
| Dual version (envelope + engine schema) | Both in header. | |
| You decide | Claude picks based on layering | |

**User's choice:** Envelope-only version

### Q6: Envelope code location

| Option | Description | Selected |
|--------|-------------|----------|
| pkg/metadata/backup/ subpackage | Dedicated subpackage for envelope code | ✓ |
| Inline in pkg/metadata/backupable.go | Helpers in same file as interface | |
| You decide | Claude picks | |

**User's choice:** pkg/metadata/backup/ subpackage

---

## HashSet Package Placement + Semantics

### Q1: Package location

| Option | Description | Selected |
|--------|-------------|----------|
| pkg/blockstore/hashset.go (as planned) | Alongside ContentHash in types.go | ✓ |
| pkg/metadata/hashset.go | Near Backupable interface | |
| pkg/metadata/backup/hashset.go | In new backup subpackage | |

**User's choice:** Asked "what is the best location?" → Claude recommended pkg/blockstore/hashset.go

### Q2: Implementation

| Option | Description | Selected |
|--------|-------------|----------|
| In-memory map[ContentHash]struct{} | Simple, O(1), ~3.2 MB for 100k hashes | ✓ |
| Sorted slice (binary search) | Compact, O(log n) Contains | |
| You decide | Claude picks simplest | |

**User's choice:** In-memory map

### Q3: Concrete vs interface

| Option | Description | Selected |
|--------|-------------|----------|
| Concrete struct (exported) | Simple, no abstraction overhead | ✓ |
| Interface with map-backed default | Future disk-backed impl without API break | |
| You decide | Claude picks per 'less is more' | |

**User's choice:** Asked "what does software architect skill suggest?" → Claude recommended concrete struct

### Q4: Thread safety

| Option | Description | Selected |
|--------|-------------|----------|
| Caller-synchronized (no internal lock) | All usage patterns single-goroutine | |
| Thread-safe (internal RWMutex) | Defensive, ~2 ns/op overhead | |
| You decide | Claude picks | ✓ |

**User's choice:** You decide → Claude chose caller-synchronized (D-10)

### Q5: Sorted() method

| Option | Description | Selected |
|--------|-------------|----------|
| HashSet.Sorted() returns sorted slice | Convenience for manifest writer | |
| ForEach only, caller sorts | HashSet stays minimal | |
| You decide | Claude picks simplest | ✓ |

**User's choice:** You decide → Claude chose Sorted() (D-11)

### Q6: Serialization methods

| Option | Description | Selected |
|--------|-------------|----------|
| No serialization on HashSet | Pure RAM type, Phase 22 owns disk format | ✓ |
| MarshalBinary / UnmarshalBinary | HashSet knows how to serialize | |
| You decide | Claude picks | |

**User's choice:** No serialization on HashSet

---

## Conformance Suite Scope

### Q1: Suite function shape

| Option | Description | Selected |
|--------|-------------|----------|
| Separate RunBackupConformanceSuite | New function, separate factory type | |
| Extend RunConformanceSuite | Add BackupOps section with type assertion | |
| You decide | Claude picks based on patterns | ✓ |

**User's choice:** You decide → Claude chose separate function (D-14)

### Q2: Corruption subtest scope

| Option | Description | Selected |
|--------|-------------|----------|
| Truncated stream only | Simple, covers most common failure | |
| Truncated + bit-flip + wrong engine | Three corruption scenarios | ✓ |
| You decide | Claude picks coverage level | |

**User's choice:** Truncated + bit-flip + wrong engine

### Q3: ConcurrentWriter isolation

| Option | Description | Selected |
|--------|-------------|----------|
| Verify snapshot isolation | Assert concurrent writes NOT in restored data | ✓ |
| No-crash + no-deadlock only | Just verify no errors | |
| You decide | Claude picks | |

**User's choice:** Verify snapshot isolation

### Q4: HashSetCorrectness scope

| Option | Description | Selected |
|--------|-------------|----------|
| Exact hash match against manual scan | Cross-reference with store walk | |
| Dedup verification (shared hashes counted once) | Prove dedup-awareness | |
| Both | Exact match + dedup in same subtest | ✓ |

**User's choice:** Both

---

## Archive Strategy for 01-07

### Q1: Destination

| Option | Description | Selected |
|--------|-------------|----------|
| Move to v0.13.0 archive | .planning/milestones/v0.13.0-archive/phases/ | |
| Delete entirely | Git history preserves if needed | ✓ |
| You decide | Claude picks lower-effort | |

**User's choice:** Delete entirely

---

## Code Structure and Design

### Q1: File layout

| Option | Description | Selected |
|--------|-------------|----------|
| Proposed layout (backupable.go + backup/ + hashset.go + backup_conformance.go) | Errors in backupable.go | ✓ |
| Errors in backup/ subpackage | Groups all backup types in one place | |
| Different layout | User specifies | |

**User's choice:** Looks right (proposed layout confirmed)

### Q2: Backupable interface shape

| Option | Description | Selected |
|--------|-------------|----------|
| Standalone interface | Type assertion at call sites | |
| Extend MetadataStore | All stores must implement | |
| You decide | Claude picks | ✓ |

**User's choice:** You decide → Claude chose standalone (D-18)

### Q3: Error sentinel design

| Option | Description | Selected |
|--------|-------------|----------|
| Plain sentinels (var + errors.New) | Matches existing ExportError pattern | |
| Typed errors with context | Richer diagnostics, errors.As needed | |
| You decide | Claude picks per existing patterns | ✓ |

**User's choice:** You decide → Claude chose plain sentinels (D-19)

### Q4: PR shape

| Option | Description | Selected |
|--------|-------------|----------|
| Single PR, staged commits | One PR, 4 ordered commits. Matches recent phase pattern. | ✓ |
| Two PRs: code + cleanup | Separates new code from deletions | |
| You decide | Claude picks | |

**User's choice:** Single PR, staged commits

---

## Claude's Discretion

- D-03: HashSet returned separately (metadata-only stream)
- D-09: Concrete struct over interface
- D-10: Caller-synchronized (no internal lock)
- D-11: Sorted() convenience method
- D-14: Separate RunBackupConformanceSuite
- D-18: Standalone Backupable interface
- D-19: Plain sentinel vars

## Deferred Ideas

None — discussion stayed within phase scope.
