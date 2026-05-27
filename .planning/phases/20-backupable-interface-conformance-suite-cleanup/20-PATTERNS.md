# Phase 20: Backupable Interface + Conformance Suite + Cleanup - Pattern Map

**Mapped:** 2026-05-27
**Files analyzed:** 7 new/modified files + 2 deletion targets
**Analogs found:** 7 / 7

## File Classification

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|-------------------|------|-----------|----------------|---------------|
| `pkg/blockstore/hashset.go` | model | transform | `pkg/blockstore/types.go` | exact |
| `pkg/blockstore/hashset_test.go` | test | transform | `pkg/blockstore/types_test.go` | exact |
| `pkg/metadata/backupable.go` | model (interface + sentinels) | request-response | `pkg/metadata/store.go` + `pkg/blockstore/errors.go` | exact |
| `pkg/metadata/backupable_test.go` | test | request-response | `pkg/blockstore/errors_test.go` | exact |
| `pkg/metadata/backup/envelope.go` | utility (serialization) | file-I/O | `pkg/blockstore/local/fs/appendlog.go` | exact |
| `pkg/metadata/backup/envelope_test.go` | test | file-I/O | `pkg/blockstore/local/fs/appendlog.go` (inline pattern) | role-match |
| `pkg/metadata/storetest/backup_conformance.go` | test (conformance suite) | CRUD | `pkg/metadata/storetest/suite.go` + `pkg/metadata/storetest/blockref_roundtrip.go` | exact |
| `internal/cli/backupfmt/` (DELETE) | -- | -- | -- | N/A |
| `.planning/phases/01-* through 07-*` (DELETE) | -- | -- | -- | N/A |

## Pattern Assignments

### `pkg/blockstore/hashset.go` (model, transform)

**Analog:** `pkg/blockstore/types.go`

**Imports pattern** (lines 1-11):
```go
package blockstore

import (
    "bytes"
    "slices"
)
```

The file lives in the `blockstore` package alongside `types.go` (which defines `ContentHash`). No external imports needed -- only `bytes` for `Compare` in `Sorted()` and `slices` for `SortFunc`. Follows the same package-level import style as `types.go`.

**Core type pattern** (derived from `types.go` lines 22-23, 26-34):
```go
// ContentHash type definition that HashSet wraps:
// types.go:22-23
const HashSize = 32
type ContentHash [HashSize]byte

// types.go:26-28 -- method style for value types
func (h ContentHash) String() string {
    return hex.EncodeToString(h[:])
}
```

HashSet should follow the same receiver-method style: concrete struct with pointer receiver methods matching the documented method set (Add, Contains, Len, ForEach, Sorted, Hashes). Constructor follows `NewFileBlock` pattern (types.go line 207-216).

**Constructor pattern** (types.go lines 207-216):
```go
func NewFileBlock(id string, localPath string) *FileBlock {
    now := time.Now()
    return &FileBlock{
        ID:         id,
        LocalPath:  localPath,
        RefCount:   1,
        LastAccess: now,
        CreatedAt:  now,
    }
}
```

HashSet constructor: `NewHashSet(sizeHint int) *HashSet` returns pointer, mirrors naming convention.

---

### `pkg/blockstore/hashset_test.go` (test, transform)

**Analog:** `pkg/blockstore/types_test.go`

**Imports pattern** (lines 1-9):
```go
package blockstore

import (
    "bytes"
    "encoding/json"
    "errors"
    "strings"
    "testing"
)
```

Note: `types_test.go` uses the **same package** (`package blockstore`, not `package blockstore_test`). HashSet tests should follow this since HashSet is in the same package and needs direct access to the struct fields.

**Test naming pattern** (lines 16-32):
```go
func TestContentHash_CASKey_Format(t *testing.T) {
    var h ContentHash
    for i := range h {
        h[i] = byte(i) // 00 01 02 ... 1F
    }
    got := h.CASKey()
    want := "blake3:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
    if got != want {
        t.Fatalf("CASKey() = %q, want %q", got, want)
    }
}
```

Test naming: `TestHashSet_Add`, `TestHashSet_Contains`, `TestHashSet_Sorted`, etc. Use `t.Fatalf` for assertions, not testify.

---

### `pkg/metadata/backupable.go` (model -- interface + sentinels)

**Analog:** `pkg/metadata/store.go` (interface) + `pkg/blockstore/errors.go` (sentinels) + `pkg/metadata/rollup_store.go` (metadata-package sentinel)

**Imports pattern** (store.go lines 1-9):
```go
package metadata

import (
    "context"

    "github.com/marmos91/dittofs/pkg/blockstore"
    "github.com/marmos91/dittofs/pkg/health"
    "github.com/marmos91/dittofs/pkg/metadata/lock"
)
```

`backupable.go` imports: `context`, `errors`, `io`, and `github.com/marmos91/dittofs/pkg/blockstore`. The `pkg/metadata` -> `pkg/blockstore` import direction is already established (store.go line 6).

**Interface pattern** (store.go lines 313-401):
```go
// MetadataStore is the main interface for metadata operations.
//
// It combines five interfaces:
//   - Files: File CRUD operations
//   ...
//
// Design Principles:
//   - Protocol-agnostic: No NFS/SMB/FTP-specific types or values
//   ...
type MetadataStore interface {
    Files
    Shares
    ...
}
```

Backupable is standalone (D-18), NOT embedded in MetadataStore. Matches the optional-capability pattern from `storetest/blockref_roundtrip.go` (lines 22-26):
```go
type FileBlockRefsAccessor interface {
    CountFileBlockRefs(ctx context.Context, fileID uuid.UUID) (int, error)
}
```

**Error sentinel pattern** (blockstore/errors.go lines 11-18):
```go
var (
    ErrContentNotFound = errors.New("content not found")
    ErrContentExists   = errors.New("content already exists")
    ...
)
```

And metadata-package sentinel from rollup_store.go (line 42):
```go
var ErrRollupOffsetRegression = errors.New("metadata: rollup offset regression rejected")
```

The metadata-package sentinels use the `metadata:` prefix in their messages. Follow this for the 4 new sentinels.

---

### `pkg/metadata/backupable_test.go` (test)

**Analog:** `pkg/blockstore/errors_test.go`

**Imports pattern** (lines 1-9):
```go
package blockstore_test

import (
    "errors"
    "fmt"
    "testing"

    "github.com/marmos91/dittofs/pkg/blockstore"
)
```

Note: `errors_test.go` uses the **external test package** (`package blockstore_test`). The metadata sentinel tests should use `package metadata_test` for consistency and to verify exported sentinel visibility.

**Sentinel test pattern** (errors_test.go lines 14-22):
```go
func TestErrStopWalk_DetectsThroughWrap(t *testing.T) {
    wrapped := fmt.Errorf("gc found target deadbeef: %w", blockstore.ErrStopWalk)
    if !errors.Is(wrapped, blockstore.ErrStopWalk) {
        t.Fatalf("errors.Is should detect ErrStopWalk through fmt.Errorf wrap; got %v", wrapped)
    }
    if errors.Is(wrapped, blockstore.ErrLegacyLayoutDetected) {
        t.Fatalf("errors.Is must not cross-match unrelated sentinels")
    }
}
```

For each of the 4 new sentinels (`ErrRestoreDestinationNotEmpty`, `ErrRestoreCorrupt`, `ErrSchemaVersionMismatch`, `ErrBackupAborted`): verify `errors.Is` detection through `fmt.Errorf` wrapping AND verify no cross-match with other sentinels.

---

### `pkg/metadata/backup/envelope.go` (utility, file-I/O)

**Analog:** `pkg/blockstore/local/fs/appendlog.go`

**Imports pattern** (lines 1-10):
```go
package fs

import (
    "encoding/binary"
    "errors"
    "fmt"
    "hash/crc32"
    "io"
    "os"
)
```

Envelope package: `package backup`. Imports: `encoding/binary`, `fmt`, `hash/crc32`, `io`. Does NOT import `os` (pure stream, no files). Does NOT import `pkg/metadata` (avoids import cycle per pitfall 1 in RESEARCH.md).

**Magic constant pattern** (appendlog.go line 43):
```go
var logMagic = [4]byte{'D', 'F', 'L', 'G'}
```

Envelope magic: `var envelopeMagic = [4]byte{'D', 'F', 'B', 'K'}` -- confirmed no collision (appendlog uses "DFLG").

**CRC table pattern** (appendlog.go line 48):
```go
var crcTable = crc32.MakeTable(crc32.Castagnoli)
```

Reuse the exact same Castagnoli table initialization. Hardware-accelerated on arm64/amd64.

**Header marshal/unmarshal pattern** (appendlog.go lines 65-105):
```go
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

func unmarshalHeader(buf []byte) (logHeader, error) {
    var h logHeader
    if len(buf) < logHeaderSize {
        return h, fmt.Errorf("append log: header short: %d < %d", len(buf), logHeaderSize)
    }
    copy(h.Magic[:], buf[0:4])
    if h.Magic != logMagic {
        return h, ErrLogBadMagic
    }
    h.Version = binary.LittleEndian.Uint32(buf[4:8])
    if h.Version != logVersion {
        return h, ErrLogBadVersion
    }
    wantCRC := binary.LittleEndian.Uint32(buf[28:32])
    gotCRC := crc32.Checksum(buf[0:28], crcTable)
    if wantCRC != gotCRC {
        return h, ErrLogBadHeaderCRC
    }
    ...
}
```

Envelope header is **variable-length** (D-04 engine tag is variable string), unlike appendlog's fixed 64 bytes. The marshal/unmarshal pattern applies but the header writes to `io.Writer` / reads from `io.Reader` rather than a fixed-size byte array. CRC is **trailing** (after the engine payload) rather than inline in the header.

**Error pattern within the package** (appendlog.go unmarshalHeader returns package-level errors):
```go
return h, ErrLogBadMagic
return h, ErrLogBadVersion
return h, ErrLogBadHeaderCRC
```

Envelope should define its own error sentinels for wire-format issues (bad magic, bad version). These are **distinct from** the metadata-level sentinels in `backupable.go` -- the envelope package must NOT import `pkg/metadata`.

**Streaming CRC pattern** (appendlog.go lines 122-146, specifically the running CRC computation):
```go
crc := crc32.Update(0, crcTable, offBuf[:])
crc = crc32.Update(crc, crcTable, payload)
```

For the envelope's trailing CRC over the full stream (header + payload), use `io.MultiWriter(dest, crc32.New(crcTable))` to accumulate the checksum while writing. On read, use `io.TeeReader(src, crc32.New(crcTable))` to verify while reading.

---

### `pkg/metadata/backup/envelope_test.go` (test, file-I/O)

**Analog:** `pkg/blockstore/local/fs/appendlog.go` (the appendlog tests are embedded in the FSStore tests; no standalone appendlog_test.go exists -- but the `blockstore/errors_test.go` pattern applies for unit tests)

**Package and imports pattern:**
```go
package backup

import (
    "bytes"
    "hash/crc32"
    "testing"
)
```

Use same-package tests (`package backup`, not `package backup_test`) since envelope internals (magic, version constants) need testing. Test CRC round-trip, truncation detection, bit-flip detection, and wrong-engine-tag detection.

---

### `pkg/metadata/storetest/backup_conformance.go` (test, conformance suite)

**Analog:** `pkg/metadata/storetest/suite.go` (factory + dispatcher) + `pkg/metadata/storetest/blockref_roundtrip.go` (optional capability type assertion)

**Imports pattern** (suite.go lines 1-7):
```go
package storetest

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
)
```

Backup conformance will also need: `context`, `bytes`, `io`, `github.com/marmos91/dittofs/pkg/blockstore`, `github.com/marmos91/dittofs/pkg/metadata/backup`.

**Factory type pattern** (suite.go lines 9-12):
```go
type StoreFactory func(t *testing.T) metadata.MetadataStore
```

Backup factory: `type BackupableStoreFactory func(t *testing.T) metadata.MetadataStore` -- same signature (D-14). The suite type-asserts to `metadata.Backupable` inside.

**Suite dispatcher pattern** (suite.go lines 21-76):
```go
func RunConformanceSuite(t *testing.T, factory StoreFactory) {
    t.Helper()

    t.Run("FileOps", func(t *testing.T) {
        runFileOpsTests(t, factory)
    })

    t.Run("DirOps", func(t *testing.T) {
        runDirOpsTests(t, factory)
    })
    ...
}
```

Backup suite: `RunBackupConformanceSuite(t, factory)` dispatches 5 subtests via `t.Run`.

**Type assertion gating pattern** (blockref_roundtrip.go lines 22-27):
```go
type FileBlockRefsAccessor interface {
    CountFileBlockRefs(ctx context.Context, fileID uuid.UUID) (int, error)
}

// In the test body:
accessor, ok := store.(FileBlockRefsAccessor)
if !ok {
    t.Skip("backend does not implement FileBlockRefsAccessor")
}
```

For backup conformance, the type assertion is at the top of `RunBackupConformanceSuite`:
```go
store := factory(t)
b, ok := store.(metadata.Backupable)
if !ok {
    t.Fatal("factory must return a store implementing Backupable")
}
```

Use `t.Fatal` not `t.Skip` because the factory explicitly opts in (D-14 / RESEARCH.md open question 2).

**Conformance test wiring pattern** (memory_conformance_test.go lines 1-15):
```go
package memory_test

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata"
    "github.com/marmos91/dittofs/pkg/metadata/store/memory"
    "github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
    storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
        return memory.NewMemoryMetadataStoreWithDefaults()
    })
}
```

Phase 21 will add a parallel `TestBackupConformance` in each driver's test file. Phase 20 just establishes the suite -- it must compile independently without any driver calling it.

**Test helper pattern** (suite.go lines 80-111):
```go
func createTestShare(t *testing.T, store metadata.MetadataStore, shareName string) metadata.FileHandle {
    t.Helper()
    ctx := t.Context()
    share := &metadata.Share{Name: shareName}
    if err := store.CreateShare(ctx, share); err != nil {
        t.Fatalf("CreateShare(%q) failed: %v", shareName, err)
    }
    ...
}
```

Backup conformance helpers should follow the same `t.Helper()` + `t.Fatalf` pattern for setup operations (creating shares, populating files, etc.) within the suite.

---

## Shared Patterns

### Error Sentinels
**Source:** `pkg/blockstore/errors.go` lines 11-18, `pkg/metadata/rollup_store.go` line 42
**Apply to:** `pkg/metadata/backupable.go`
```go
// blockstore/errors.go pattern:
var (
    ErrContentNotFound = errors.New("content not found")
)

// metadata/rollup_store.go pattern (metadata-prefixed):
var ErrRollupOffsetRegression = errors.New("metadata: rollup offset regression rejected")
```
New sentinels should use the `metadata:` prefix: `errors.New("metadata: restore destination is not empty")`.

### CRC32 Castagnoli
**Source:** `pkg/blockstore/local/fs/appendlog.go` line 48
**Apply to:** `pkg/metadata/backup/envelope.go`
```go
var crcTable = crc32.MakeTable(crc32.Castagnoli)
```

### Binary Encoding (Little-Endian)
**Source:** `pkg/blockstore/local/fs/appendlog.go` lines 65-76
**Apply to:** `pkg/metadata/backup/envelope.go`
```go
binary.LittleEndian.PutUint32(buf[4:8], h.Version)
binary.LittleEndian.PutUint64(buf[8:16], h.RollupOffset)
```

### Optional Interface Type Assertion
**Source:** `pkg/metadata/storetest/blockref_roundtrip.go` lines 22-26, `pkg/metadata/storetest/objectid_roundtrip.go` lines 23-30
**Apply to:** `pkg/metadata/storetest/backup_conformance.go`
```go
// blockref_roundtrip.go:22-26
type FileBlockRefsAccessor interface {
    CountFileBlockRefs(ctx context.Context, fileID uuid.UUID) (int, error)
}
```

### Conformance Factory + Dispatch
**Source:** `pkg/metadata/storetest/suite.go` lines 9-76
**Apply to:** `pkg/metadata/storetest/backup_conformance.go`
```go
type StoreFactory func(t *testing.T) metadata.MetadataStore

func RunConformanceSuite(t *testing.T, factory StoreFactory) {
    t.Helper()
    t.Run("...", func(t *testing.T) { ... })
}
```

### Test Style (t.Fatalf, no testify)
**Source:** `pkg/blockstore/types_test.go` lines 16-32, `pkg/blockstore/errors_test.go` lines 14-22
**Apply to:** All new test files
```go
if got != want {
    t.Fatalf("Foo() = %q, want %q", got, want)
}
```

## No Analog Found

| File | Role | Data Flow | Reason |
|------|------|-----------|--------|
| (none) | -- | -- | All files have exact or role-match analogs in the codebase |

## Metadata

**Analog search scope:** `pkg/blockstore/`, `pkg/metadata/`, `pkg/metadata/storetest/`, `pkg/blockstore/local/fs/`, `internal/cli/backupfmt/`
**Files scanned:** 12 analog files read, 7 new files classified
**Pattern extraction date:** 2026-05-27
