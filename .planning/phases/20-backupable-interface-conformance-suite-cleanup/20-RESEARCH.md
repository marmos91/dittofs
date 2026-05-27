# Phase 20: Backupable Interface + Conformance Suite + Cleanup - Research

**Researched:** 2026-05-27
**Domain:** Go interface design, conformance testing, binary envelope format, codebase cleanup
**Confidence:** HIGH

## Summary

Phase 20 is a pure foundation phase: no runtime behavior changes, no external dependencies, no database migrations. It establishes the `Backupable` interface contract, the `HashSet` collection type, a shared binary envelope format, error sentinels, and a conformance test suite that Phase 21 drivers will wire into. It also cleans up orphaned v0.13.0 backup artifacts.

All decisions are locked in CONTEXT.md (D-01 through D-21). The technical domain is exclusively Go standard library (`hash/crc32`, `encoding/binary`, `io`, `errors`, `slices`). No external packages are needed. The codebase already has well-established patterns for every construct this phase introduces (optional interfaces via type assertion, error sentinels via `errors.New`, conformance suites via `Run*Suite` functions, CRC32 Castagnoli checksums, little-endian binary framing).

**Primary recommendation:** Follow the codebase's existing patterns exactly. The append-log envelope in `pkg/blockstore/local/fs/appendlog.go` is the direct template for the backup envelope's magic/version/CRC32 structure. The `storetest/objectid_roundtrip.go` pattern is the direct template for the conformance suite's factory and type-assertion gating.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- D-01: Shared envelope + opaque payload. Small shared header wrapping engine-specific bytes.
- D-02: Envelope includes trailing CRC32 for uniform corruption detection.
- D-03: HashSet returned separately from the stream. `Backup(ctx, w)` writes only metadata bytes to `w` and returns `HashSet` in-memory.
- D-04: Engine tag is a variable-length string (e.g., `"badger"`, `"postgres"`, `"memory"`), not a byte enum.
- D-05: Envelope version tracks envelope framing only.
- D-06: Envelope code lives in `pkg/metadata/backup/` subpackage. Interface and error sentinels stay in `pkg/metadata/backupable.go`.
- D-07: HashSet lives in `pkg/blockstore/hashset.go` alongside `ContentHash` in `types.go`.
- D-08: Implementation: in-memory `map[ContentHash]struct{}`. O(1) Add/Contains.
- D-09: Concrete struct, not interface.
- D-10: Caller-synchronized (no internal lock).
- D-11: Exposes `Sorted()` returning `[]ContentHash`.
- D-12: No `MarshalBinary`/`UnmarshalBinary`.
- D-13: Methods: `Add`, `Contains`, `Len`, `ForEach`, `Sorted`, `Hashes`.
- D-14: Separate `RunBackupConformanceSuite` function in `pkg/metadata/storetest/backup_conformance.go`.
- D-15: Corruption subtest: 3 scenarios (truncated, bit-flip, wrong engine tag).
- D-16: ConcurrentWriter subtest: verifies snapshot isolation.
- D-17: HashSetCorrectness subtest: exact hash match + dedup verification.
- D-18: `Backupable` is a standalone interface, not embedded in `MetadataStore`. Call sites use type assertion.
- D-19: Error sentinels are plain `var + errors.New`.
- D-20: Delete `.planning/phases/01-*` through `07-*` entirely.
- D-21: Single PR against develop, staged commits: (1) HashSet, (2) Backupable + errors + envelope, (3) conformance suite, (4) backupfmt deletion + planning cleanup.

### Claude's Discretion
- D-03: HashSet returned separately (keeps Backup stream metadata-only)
- D-09: Concrete struct over interface ("less is more")
- D-10: Caller-synchronized (no concurrent usage pattern)
- D-11: Sorted() method (avoids duplicate sort logic)
- D-14: Separate RunBackupConformanceSuite (existing suite has 7 sections)
- D-18: Standalone Backupable interface (matches existing patterns)
- D-19: Plain sentinel vars (matches existing patterns)

### Deferred Ideas (OUT OF SCOPE)
None.
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|------------------|
| ENG-01 | `Backupable` interface in `pkg/metadata/` with `Backup(ctx, w) (HashSet, error)` and `Restore(ctx, r) error` signatures | Interface pattern verified against existing `MetadataStore` optional-capability conventions (ObjectIDIndexAccessor, FileBlockRefsAccessor). Type assertion at call site is the established pattern. |
| ENG-02 | `HashSet` type captures all unique `ContentHash` values from `FileAttr.Blocks` inside the same atomic snapshot transaction | `ContentHash` is `[32]byte` in `pkg/blockstore/types.go`. `FileAttr.Blocks` is `[]blockstore.BlockRef` (line 89 of file_types.go). `map[ContentHash]struct{}` is correct for O(1) dedup. |
| ENG-03 | Four typed error sentinels: `ErrRestoreDestinationNotEmpty`, `ErrRestoreCorrupt`, `ErrSchemaVersionMismatch`, `ErrBackupAborted` | Existing pattern verified: `var ErrX = errors.New(...)` in `pkg/blockstore/errors.go` (18 sentinels), `pkg/metadata/types.go` (ErrInvalidBlockLayout), `pkg/metadata/rollup_store.go` (ErrRollupOffsetRegression). All pass `errors.Is` via standard `fmt.Errorf("%w", ...)` wrapping. |
| ENG-04 | Shared conformance suite in `pkg/metadata/storetest/` with 5 subtests | Conformance pattern verified: `RunConformanceSuite` in `suite.go` dispatches to per-file test functions. `StoreFactory func(t *testing.T) metadata.MetadataStore` is the factory type. `BackupableStoreFactory` will be analogous. Memory conformance test at `store/memory/memory_conformance_test.go` shows the wiring pattern. |
| CLN-01 | Delete orphaned `internal/cli/backupfmt/` package | Confirmed orphaned: `grep -rn 'backupfmt' --include='*.go'` returns zero hits outside the package itself. Contains `format.go` (67 lines) + `format_test.go` (55 lines). Safe to delete. |
| CLN-02 | Archive old backup planning phases (01-07) to milestones directory | D-20 says "delete entirely" (git history is the archive). Seven directories exist under `.planning/phases/`: `01-*` through `07-*`, totaling ~2.6 MB. No milestones directory creation needed per D-20. |
</phase_requirements>

## Architectural Responsibility Map

| Capability | Primary Tier | Secondary Tier | Rationale |
|------------|-------------|----------------|-----------|
| Backupable interface definition | API / Contract | -- | Pure Go interface in `pkg/metadata/`; no runtime tier involved |
| HashSet type | Data Structure | -- | In-memory collection in `pkg/blockstore/`; no persistence tier |
| Envelope binary format | Serialization | -- | `pkg/metadata/backup/` subpackage; write/read helpers for io.Writer/io.Reader |
| Error sentinels | API / Contract | -- | `pkg/metadata/backupable.go`; standard errors.New values |
| Conformance suite | Testing | -- | `pkg/metadata/storetest/`; test-only code, no production runtime |
| Cleanup (backupfmt + phases) | Build / Codebase | -- | File deletion; no runtime impact |

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `hash/crc32` | stdlib | Envelope CRC32 Castagnoli | Already used in `pkg/blockstore/local/fs/appendlog.go` with hardware acceleration on arm64/amd64 [VERIFIED: codebase grep] |
| `encoding/binary` | stdlib | Little-endian wire encoding for envelope header | Already used in appendlog.go for the same purpose [VERIFIED: codebase grep] |
| `io` | stdlib | `io.Writer` / `io.Reader` for Backup/Restore stream | Standard Go streaming interface [VERIFIED: codebase grep] |
| `errors` | stdlib | Sentinel errors via `errors.New` | Project-wide pattern for all error sentinels [VERIFIED: codebase grep] |
| `slices` | stdlib | `slices.SortFunc` for HashSet.Sorted() | Already used in `pkg/metadata/lock/manager.go` and `pkg/bench/stats.go` [VERIFIED: codebase grep] |
| `bytes` | stdlib | `bytes.Compare` for ContentHash ordering in Sorted() | ContentHash is `[32]byte`; `bytes.Compare` gives lexicographic order [VERIFIED: codebase grep] |

### Supporting
No external libraries needed. This phase is 100% Go standard library.

### Alternatives Considered
None. All decisions are locked and require only stdlib.

**Installation:**
```bash
# No installation needed -- all stdlib
```

## Package Legitimacy Audit

No external packages are installed in this phase. All code uses Go standard library only.

| Package | Registry | Age | Downloads | Source Repo | slopcheck | Disposition |
|---------|----------|-----|-----------|-------------|-----------|-------------|
| (none) | -- | -- | -- | -- | -- | N/A |

**Packages removed due to slopcheck [SLOP] verdict:** none
**Packages flagged as suspicious [SUS]:** none

## Architecture Patterns

### System Architecture Diagram

```
Phase 20 introduces contracts only -- no runtime data flow.
Phase 21+ implements the flow:

   MetadataStore (Badger/Memory/Postgres)
         |
         | implements Backupable
         v
   store.(Backupable).Backup(ctx, w)
         |
         +---> io.Writer: envelope header + engine payload + CRC32
         |
         +---> HashSet (in-memory): all ContentHash from FileAttr.Blocks
         |
         v
   (consumed by Phase 22 manifest writer + Phase 23 GC hold)

Restore flow:
   io.Reader ---> envelope.ReadHeader() ---> engine tag check
         |                                       |
         |                              ErrSchemaVersionMismatch
         |                              ErrRestoreCorrupt (CRC)
         v
   store.(Backupable).Restore(ctx, r)
         |
         +---> ErrRestoreDestinationNotEmpty (pre-check)
         +---> ErrRestoreCorrupt (payload corruption)
```

### Component Responsibilities

| File | Purpose | Content |
|------|---------|---------|
| `pkg/blockstore/hashset.go` | HashSet type | Concrete struct wrapping `map[ContentHash]struct{}` with Add/Contains/Len/ForEach/Sorted/Hashes |
| `pkg/metadata/backupable.go` | Interface + errors | `Backupable` interface (Backup/Restore) + 4 error sentinels |
| `pkg/metadata/backup/envelope.go` | Envelope format | WriteHeader/ReadHeader/WriteCRC/VerifyCRC helpers |
| `pkg/metadata/storetest/backup_conformance.go` | Conformance suite | `RunBackupConformanceSuite` with 5 subtests |

### Recommended Project Structure
```
pkg/
├── blockstore/
│   ├── types.go              # existing ContentHash [32]byte
│   └── hashset.go            # NEW: HashSet concrete type
├── metadata/
│   ├── backupable.go         # NEW: Backupable interface + 4 error sentinels
│   ├── backup/               # NEW subpackage
│   │   └── envelope.go       # WriteHeader, ReadHeader, WriteCRC, VerifyCRC
│   └── storetest/
│       └── backup_conformance.go  # NEW: RunBackupConformanceSuite
```

### Pattern 1: HashSet (D-07 through D-13)
**What:** Concrete struct wrapping `map[ContentHash]struct{}` with 6 methods.
**When to use:** Collecting unique content hashes during backup for GC hold and manifest writing.
**Example:**
```go
// Source: codebase pattern analysis
package blockstore

import (
    "bytes"
    "slices"
)

// HashSet is an in-memory collection of unique ContentHash values.
// Caller-synchronized: no internal locking. All usage is single-goroutine
// (backup under engine lock, GC hold reads, manifest writes).
type HashSet struct {
    m map[ContentHash]struct{}
}

// NewHashSet creates an empty HashSet with optional initial capacity hint.
func NewHashSet(sizeHint int) *HashSet {
    return &HashSet{m: make(map[ContentHash]struct{}, sizeHint)}
}

func (hs *HashSet) Add(h ContentHash)             { hs.m[h] = struct{}{} }
func (hs *HashSet) Contains(h ContentHash) bool    { _, ok := hs.m[h]; return ok }
func (hs *HashSet) Len() int                       { return len(hs.m) }
func (hs *HashSet) Hashes() map[ContentHash]struct{} { return hs.m }

func (hs *HashSet) ForEach(fn func(ContentHash) error) error {
    for h := range hs.m {
        if err := fn(h); err != nil {
            return err
        }
    }
    return nil
}

func (hs *HashSet) Sorted() []ContentHash {
    out := make([]ContentHash, 0, len(hs.m))
    for h := range hs.m {
        out = append(out, h)
    }
    slices.SortFunc(out, func(a, b ContentHash) int {
        return bytes.Compare(a[:], b[:])
    })
    return out
}
```

### Pattern 2: Backupable Interface (D-18)
**What:** Standalone optional interface, not embedded in MetadataStore.
**When to use:** Call sites use type assertion `store.(Backupable)` to probe capability.
**Example:**
```go
// Source: matches ObjectIDIndexAccessor pattern in storetest/objectid_roundtrip.go
package metadata

import (
    "context"
    "errors"
    "io"

    "github.com/marmos91/dittofs/pkg/blockstore"
)

// Backupable is an optional capability interface for metadata stores that
// support atomic backup and restore. Drivers implement both MetadataStore
// and Backupable. Call sites probe via type assertion:
//
//     b, ok := store.(Backupable)
//     if !ok { return ErrNotSupported }
//
// Matches existing optional-capability patterns (ObjectIDIndexAccessor,
// FileBlockRefsAccessor) -- standalone interface, not embedded.
type Backupable interface {
    // Backup writes a complete metadata snapshot to w and returns the set
    // of all unique ContentHash values from FileAttr.Blocks within the
    // same atomic snapshot transaction. The stream contains only metadata
    // bytes; hash manifest format is owned by the caller (Phase 22).
    Backup(ctx context.Context, w io.Writer) (*blockstore.HashSet, error)

    // Restore reads a metadata snapshot from r and populates an empty
    // store. Returns ErrRestoreDestinationNotEmpty if the store already
    // contains data.
    Restore(ctx context.Context, r io.Reader) error
}
```

### Pattern 3: Error Sentinels (D-19)
**What:** Plain `var + errors.New` matching existing codebase convention.
**Example:**
```go
// Source: matches pkg/blockstore/errors.go pattern
var (
    ErrRestoreDestinationNotEmpty = errors.New("metadata: restore destination is not empty")
    ErrRestoreCorrupt             = errors.New("metadata: restore data is corrupt")
    ErrSchemaVersionMismatch      = errors.New("metadata: schema version mismatch")
    ErrBackupAborted              = errors.New("metadata: backup aborted")
)
```

### Pattern 4: Envelope Format (D-01, D-02, D-04, D-05, D-06)
**What:** Binary header + opaque payload + trailing CRC32. Mirrors appendlog.go framing conventions.
**When to use:** All backup/restore operations wrap engine payloads in this envelope.
**Example:**
```go
// Source: derived from pkg/blockstore/local/fs/appendlog.go CRC/binary patterns
package backup

import (
    "encoding/binary"
    "errors"
    "fmt"
    "hash/crc32"
    "io"
)

// Envelope wire format:
//   magic        [4]byte   "DFBK"
//   version      uint32 LE (1 = current)
//   engine_len   uint16 LE
//   engine_tag   [engine_len]byte (e.g. "badger", "memory", "postgres")
//   payload      [...]byte (engine-specific, variable length)
//   crc32c       uint32 LE (Castagnoli, over everything before this field)

var envelopeMagic = [4]byte{'D', 'F', 'B', 'K'}
var crcTable = crc32.MakeTable(crc32.Castagnoli)

const envelopeVersion = uint32(1)
```

### Pattern 5: Conformance Suite Factory (D-14)
**What:** Separate `BackupableStoreFactory` analogous to `StoreFactory`, with `RunBackupConformanceSuite`.
**Example:**
```go
// Source: matches pkg/metadata/storetest/suite.go StoreFactory pattern
package storetest

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
)

// BackupableStoreFactory creates a fresh store that implements both
// MetadataStore and Backupable. Each test gets a fresh instance.
type BackupableStoreFactory func(t *testing.T) metadata.MetadataStore

// RunBackupConformanceSuite runs the 5-subtest backup conformance suite.
// If the store does not implement Backupable, the entire suite is skipped.
func RunBackupConformanceSuite(t *testing.T, factory BackupableStoreFactory) {
    t.Helper()
    t.Run("RoundTrip", func(t *testing.T) { ... })
    t.Run("ConcurrentWriter", func(t *testing.T) { ... })
    t.Run("Corruption", func(t *testing.T) { ... })
    t.Run("NonEmptyDest", func(t *testing.T) { ... })
    t.Run("HashSetCorrectness", func(t *testing.T) { ... })
}
```

### Anti-Patterns to Avoid
- **Embedding Backupable in MetadataStore:** D-18 explicitly forbids this. Backupable is an optional capability. Embedding would force all stores to implement it immediately.
- **Adding internal locking to HashSet:** D-10 says caller-synchronized. All usage is single-goroutine. A mutex adds overhead and false safety signals.
- **Putting MarshalBinary on HashSet:** D-12 explicitly forbids this. Phase 22 owns the on-disk manifest format.
- **Using gob/JSON for the envelope:** D-01/D-02 specify a binary envelope with CRC32. The append-log pattern is the model.
- **Making HashSet an interface:** D-09 says concrete struct. "Less is more" -- refactor cost near-zero with no prod users.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| CRC32 checksum | Custom checksum algorithm | `hash/crc32` with Castagnoli table | Hardware-accelerated on arm64/amd64; already used in appendlog.go |
| Binary encoding | Custom byte packing | `encoding/binary.LittleEndian` | Standard, endian-safe, already used in appendlog.go |
| Sorted hash output | Custom sort | `slices.SortFunc` + `bytes.Compare` | Standard library, already used in the codebase |

**Key insight:** This phase needs zero external dependencies. Every construct has a direct codebase precedent.

## Common Pitfalls

### Pitfall 1: Import Cycle Between backup/ and metadata/
**What goes wrong:** `pkg/metadata/backup/envelope.go` imports `pkg/metadata` for error sentinels, creating a circular import.
**Why it happens:** Error sentinels are defined in `pkg/metadata/backupable.go` but the envelope package needs them.
**How to avoid:** The envelope package (`pkg/metadata/backup/`) must NOT import `pkg/metadata`. The envelope functions should return their own errors or use plain `fmt.Errorf`. The `Backupable` interface and error sentinels in `pkg/metadata/backupable.go` are consumed by drivers (which live under `pkg/metadata/store/`), not by the envelope package. The envelope package defines write/read helpers that operate on `io.Writer`/`io.Reader` and return errors about wire format -- it does not need the metadata-level sentinels.
**Warning signs:** `go build` fails with "import cycle not allowed."

### Pitfall 2: HashSet Import Direction
**What goes wrong:** `pkg/metadata/backupable.go` declares `Backup(ctx, w) (HashSet, error)` but `HashSet` lives in `pkg/blockstore/hashset.go`. This requires `pkg/metadata` to import `pkg/blockstore`.
**Why it happens:** The method signature couples the two packages.
**How to avoid:** `pkg/metadata` already imports `pkg/blockstore` (see `store.go` line 7: `"github.com/marmos91/dittofs/pkg/blockstore"`). The import direction is correct and does not create a cycle. The return type should be `*blockstore.HashSet`. [VERIFIED: codebase grep confirms existing import]
**Warning signs:** None -- this is the correct direction.

### Pitfall 3: Conformance Suite Compiles But Cannot Run
**What goes wrong:** The conformance suite in `backup_conformance.go` references `Backupable` but no driver implements it yet (Phase 21). If the suite tries to compile test assertions against concrete store types, it will fail.
**Why it happens:** The suite is designed to be called from driver test files that don't exist yet.
**How to avoid:** The suite must compile independently (it only references the `Backupable` interface and `MetadataStore` interface via the factory). It should use type assertion inside the test body: `b, ok := store.(metadata.Backupable)`. If `!ok`, skip. But since Phase 20 doesn't create driver implementations, no driver test file will call `RunBackupConformanceSuite` yet -- the suite just needs to compile cleanly. Verify with `go build ./pkg/metadata/storetest/...`.
**Warning signs:** `go build` or `go vet` fails on the storetest package.

### Pitfall 4: CRC32 Scope for Trailing Checksum
**What goes wrong:** The CRC32 is computed over the wrong byte range, making corruption detection unreliable.
**Why it happens:** Ambiguity about whether CRC covers the header, just the payload, or everything before the CRC field.
**How to avoid:** Per D-02, the CRC covers the entire stream (header + engine payload) -- everything written before the 4-byte CRC trailer. This matches the appendlog.go pattern where `crc32.Checksum(buf[0:28], crcTable)` covers all header bytes before the CRC field. For streaming writes, use a `hash/crc32.New(crcTable)` running hash via `io.MultiWriter(w, crcWriter)`.
**Warning signs:** Corruption subtest (D-15) passes despite injected bit-flips.

### Pitfall 5: Deletion of backupfmt Breaks Build
**What goes wrong:** Deleting `internal/cli/backupfmt/` causes a build failure because some file still imports it.
**Why it happens:** Stale import not caught during code review.
**How to avoid:** Already verified: `grep -rn 'backupfmt' --include='*.go'` returns zero hits outside the package itself. The package is confirmed orphaned with zero external imports. [VERIFIED: codebase grep 2026-05-27]
**Warning signs:** `go build ./...` fails after deletion.

### Pitfall 6: Planning Phase Deletion Scope
**What goes wrong:** Accidentally deleting phases 08-19 instead of just 01-07.
**Why it happens:** Glob pattern too broad.
**How to avoid:** D-20 is explicit: delete `.planning/phases/01-*` through `07-*`. The seven directories that exist are: `01-foundations-models-manifest-capability-interface`, `02-per-engine-backup-drivers`, `03-destination-drivers-encryption`, `04-scheduler-retention`, `05-restore-orchestration-safety-rails`, `06-cli-rest-api-surface`, `07-testing-hardening`. [VERIFIED: codebase ls]
**Warning signs:** Missing phase directories for active work.

## Code Examples

Verified patterns from the codebase:

### Existing CRC32 Castagnoli Pattern
```go
// Source: pkg/blockstore/local/fs/appendlog.go:48
var crcTable = crc32.MakeTable(crc32.Castagnoli)
```

### Existing Binary Header Pattern
```go
// Source: pkg/blockstore/local/fs/appendlog.go:65-76
func marshalHeader(h logHeader) [logHeaderSize]byte {
    var buf [logHeaderSize]byte
    copy(buf[0:4], h.Magic[:])
    binary.LittleEndian.PutUint32(buf[4:8], h.Version)
    binary.LittleEndian.PutUint64(buf[8:16], h.RollupOffset)
    binary.LittleEndian.PutUint32(buf[16:20], h.Flags)
    binary.LittleEndian.PutUint64(buf[20:28], uint64(h.CreatedAt))
    crc := crc32.Checksum(buf[0:28], crcTable)
    binary.LittleEndian.PutUint32(buf[28:32], crc)
    return buf
}
```

### Existing Optional Interface Type Assertion Pattern
```go
// Source: pkg/metadata/storetest/blockref_roundtrip.go:210-212
accessor, ok := store.(FileBlockRefsAccessor)
if !ok {
    t.Skip("backend does not implement FileBlockRefsAccessor")
}
```

### Existing Error Sentinel Pattern
```go
// Source: pkg/blockstore/errors.go:18-19
ErrContentNotFound = errors.New("content not found")
```

### Existing Conformance Suite Factory Pattern
```go
// Source: pkg/metadata/storetest/suite.go:12
type StoreFactory func(t *testing.T) metadata.MetadataStore

// Source: pkg/metadata/store/memory/memory_conformance_test.go:11-15
func TestConformance(t *testing.T) {
    storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        return memory.NewMemoryMetadataStoreWithDefaults()
    })
}
```

### Existing ContentHash Type
```go
// Source: pkg/blockstore/types.go:23-24
const HashSize = 32
type ContentHash [HashSize]byte
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| v0.13.0 BackupHoldProvider in GC | No backup hold (symbol guard test) | Phase 08/11 (Apr 2026) | GC has clean integration point for Phase 22 SnapshotHoldProvider |
| v0.13.0 `internal/cli/backupfmt/` | Orphaned (zero imports) | Phase 08+ cleanup | Safe to delete entirely |
| v0.13.0 planning phases 01-07 | Stale, never released | v0.15.0 shipped instead | Delete; git history is archive |

**Deprecated/outdated:**
- `internal/cli/backupfmt/`: Orphaned package from v0.13.0 backup CLI that was never released. Contains `ShortULID`, `TimeAgo`, `RenderProgressBar` helpers. Zero external imports -- safe to delete.
- `.planning/phases/01-07`: v0.13.0 backup planning artifacts. The v0.16.0 snapshot system (this milestone) replaces the v0.13.0 design entirely.
- `BackupHoldProvider` symbol: Explicitly guarded against reintroduction by `TestGCMarkSweep_NoBackupHoldProvider` in `gc_test.go:396`. Phase 22 will introduce `SnapshotHoldProvider` as the replacement.

## Project Constraints (from CLAUDE.md)

- Never mention Claude Code, AI tools, or Co-Authored-By in commits/PRs
- Keep commit messages concise
- Sign commits with `git commit -S`
- Default NFS port is 12049
- Error codes: return `metadata.ExportError` values
- Protocol handlers handle only protocol concerns
- Every operation carries an `*metadata.AuthContext`

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | none needed (stdlib) |
| Quick run command | `go test ./pkg/blockstore/ ./pkg/metadata/... -run Backup -count=1` |
| Full suite command | `go test ./... -count=1` |

### Phase Requirements to Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| ENG-01 | Backupable interface compiles | build | `go build ./pkg/metadata/...` | Wave 0 (backupable.go) |
| ENG-02 | HashSet captures unique ContentHash | unit | `go test ./pkg/blockstore/ -run HashSet -count=1` | Wave 0 (hashset_test.go) |
| ENG-03 | Four error sentinels pass errors.Is | unit | `go test ./pkg/metadata/ -run Backup -count=1` | Wave 0 (backupable_test.go) |
| ENG-04 | Conformance suite compiles with 5 subtests | build | `go build ./pkg/metadata/storetest/...` | Wave 0 (backup_conformance.go) |
| CLN-01 | backupfmt deleted, zero build impact | build | `go build ./...` | N/A (deletion) |
| CLN-02 | Phases 01-07 deleted | verify | `ls .planning/phases/0[1-7]-* 2>/dev/null \|\| echo PASS` | N/A (deletion) |

### Sampling Rate
- **Per task commit:** `go build ./... && go vet ./...`
- **Per wave merge:** `go test ./pkg/blockstore/ ./pkg/metadata/... -count=1`
- **Phase gate:** `go test ./... -count=1` green

### Wave 0 Gaps
- [ ] `pkg/blockstore/hashset_test.go` -- covers ENG-02 (Add/Contains/Len/ForEach/Sorted/Hashes)
- [ ] `pkg/metadata/backupable_test.go` -- covers ENG-03 (errors.Is round-trip for 4 sentinels)
- [ ] `pkg/metadata/backup/envelope_test.go` -- covers D-02 (CRC32 write/read/verify round-trip)

## Assumptions Log

| # | Claim | Section | Risk if Wrong |
|---|-------|---------|---------------|
| A1 | `bytes.Compare` provides correct lexicographic ordering for `[32]byte` ContentHash in `Sorted()` | Architecture Patterns / Pattern 1 | Low -- `bytes.Compare` is documented to compare byte slices lexicographically; ContentHash is `[32]byte` which converts to `[]byte` via `[:]` |
| A2 | Envelope magic "DFBK" does not conflict with any existing magic in the codebase | Architecture Patterns / Pattern 4 | Low -- appendlog uses "DFLG"; no other magic constants found via grep [VERIFIED: codebase grep] |
| A3 | `slices.SortFunc` is available in Go 1.25+ stdlib | Standard Stack | Low -- Go 1.21+ added `slices` to stdlib; project uses Go 1.25.0 [VERIFIED: go.mod] |

**All claims above are low-risk.** A1 and A3 are well-documented Go stdlib behavior. A2 was verified by codebase grep.

## Open Questions (RESOLVED)

1. **Envelope header fixed size vs variable**
   - What we know: D-04 specifies variable-length engine tag string. This means the header is variable-length (unlike appendlog's fixed 64-byte header).
   - RESOLVED: Cap engine tag at 255 bytes. Use uint16 LE for the tag length to keep alignment simple and allow headroom. In practice, tags are "badger" (6), "memory" (6), "postgres" (8).

2. **Whether the conformance suite should include a "SkipIfNotBackupable" guard**
   - What we know: D-14 creates a separate `RunBackupConformanceSuite` with its own `BackupableStoreFactory`. The factory type guarantees the store is Backupable.
   - RESOLVED: Single type assertion at the top of `RunBackupConformanceSuite` is sufficient. The factory contract guarantees Backupable capability. If assertion fails, `t.Fatal` — don't skip, because the caller explicitly opted in.

## Sources

### Primary (HIGH confidence)
- Codebase grep of `pkg/blockstore/types.go` -- ContentHash type definition, HashSize constant
- Codebase grep of `pkg/metadata/store.go` -- MetadataStore interface, import graph
- Codebase grep of `pkg/metadata/storetest/suite.go` -- StoreFactory pattern, RunConformanceSuite
- Codebase grep of `pkg/metadata/storetest/objectid_roundtrip.go` -- ObjectIDIndexAccessor optional interface pattern
- Codebase grep of `pkg/metadata/storetest/blockref_roundtrip.go` -- FileBlockRefsAccessor type assertion pattern
- Codebase grep of `pkg/blockstore/errors.go` -- error sentinel pattern (18 sentinels)
- Codebase grep of `pkg/blockstore/local/fs/appendlog.go` -- CRC32 Castagnoli, binary.LittleEndian, magic bytes pattern
- Codebase grep of `pkg/metadata/store/memory/memory_conformance_test.go` -- conformance test wiring pattern
- Codebase grep of `internal/cli/backupfmt/` -- confirmed orphaned (zero external imports)
- Codebase grep of `pkg/blockstore/engine/gc_test.go:396` -- BackupHoldProvider guard test

### Secondary (MEDIUM confidence)
- None needed -- all findings verified from codebase

### Tertiary (LOW confidence)
- None

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all Go stdlib, verified in go.mod (Go 1.25.0) and existing codebase usage
- Architecture: HIGH -- every pattern has a direct codebase precedent
- Pitfalls: HIGH -- import cycle risk verified by analyzing existing import graph; all deletion targets confirmed orphaned

**Research date:** 2026-05-27
**Valid until:** 2026-06-27 (stable -- Go stdlib and codebase patterns)
