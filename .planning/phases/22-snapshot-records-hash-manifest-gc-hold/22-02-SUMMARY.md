---
phase: 22-snapshot-records-hash-manifest-gc-hold
plan: 02
subsystem: snapshot
tags: [snapshot, manifest, cas, atomic-write]
requires:
  - pkg/blockstore.HashSet
  - pkg/blockstore.ContentHash
  - pkg/blockstore.ParseContentHash
provides:
  - pkg/snapshot.WriteManifest
  - pkg/snapshot.WriteManifestAtomic
  - pkg/snapshot.ReadManifest
  - pkg/snapshot.ErrInvalidManifestLine
affects: []
tech-stack:
  added: []
  patterns:
    - temp-file + fsync + rename for atomic on-disk materialization
    - bufio.Scanner with explicit Buffer cap to bound parser memory
decisions:
  - "Wire format pinned in pkg/snapshot/doc.go: plain ASCII, one 64-hex line per ContentHash, LF terminator, sorted ascending (HashSet.Sorted order)"
  - "ReadManifest tolerates CRLF on input; WriteManifest always emits LF"
  - "Duplicate lines collapse silently — matches HashSet.Add semantics (D-16)"
  - "Parent-directory fsync intentionally NOT performed in WriteManifestAtomic; deferred to the snapshot orchestrator if power-loss durability of the rename itself becomes load-bearing"
  - "Scanner buffer capped at 1 MiB; valid lines are ~65 bytes so headroom catches accidental concatenation without unbounded allocation"
key-files:
  created:
    - pkg/snapshot/doc.go
    - pkg/snapshot/manifest.go
    - pkg/snapshot/manifest_test.go
  modified: []
metrics:
  completed: 2026-05-28
  tasks: 3
  files_created: 3
  files_modified: 0
---

# Phase 22 Plan 02: Hash Manifest Reader / Writer Summary

Brand-new `pkg/snapshot` package owning the on-disk hash manifest wire format. Three public functions (`WriteManifest`, `WriteManifestAtomic`, `ReadManifest`) plus the typed `ErrInvalidManifestLine` sentinel. Package godoc pins the format (plain ASCII, sorted hex, LF-terminated) so downstream consumers in Phase 22 (`SnapshotHoldProvider`) and Phase 23 (snapshot orchestrator, sync gate) can rely on byte-stable semantics.

## What Was Built

**`pkg/snapshot/doc.go`** — Package godoc declaring the wire format as the contract: one 64-char lowercase hex ContentHash per line, LF-terminated, sorted in ascending byte order (matching `HashSet.Sorted`), no header/footer/comments/blanks. Documents the atomic-write contract (temp + fsync + rename, same-filesystem rename) and explicitly notes parent-directory fsync is out of scope per Less-is-More.

**`pkg/snapshot/manifest.go`** —

- `ErrInvalidManifestLine` package-level sentinel created via `errors.New`; wrapped by ReadManifest with `fmt.Errorf("%w: line %d: %v", …)` so callers match with `errors.Is`.
- `WriteManifest(io.Writer, *blockstore.HashSet) error` — wraps the writer in `bufio.Writer`, iterates `hs.Sorted()` (D-16: consume the canonical order verbatim, do not re-sort), emits `h.String() + "\n"` per entry via direct `WriteString`/`WriteByte` (no `fmt.Fprintf` allocations on the hot path), flushes before returning. Empty HashSet → zero bytes, nil error.
- `WriteManifestAtomic(path string, hs *blockstore.HashSet) error` — opens `<path>.tmp` in the same directory (rename atomicity requires same filesystem), runs `WriteManifest` against the file, `f.Sync()`, `f.Close()`, `os.Rename(tmp, path)`. Best-effort `os.Remove(tmp)` on any error before rename so failures leave no debris. Helper `writeAndSync` factored out so the main function reads as the three-step contract.
- `ReadManifest(io.Reader) (*blockstore.HashSet, error)` — `bufio.Scanner` with explicit `Buffer(make([]byte, 0, 128), 1<<20)` (1 MiB cap, headroom over the ~65-byte valid-line size for malformed-but-bounded input). Strips trailing CR per line, delegates parsing to `blockstore.ParseContentHash` (canonical helper enforces 32-byte length + hex), wraps mismatches with `ErrInvalidManifestLine` and the 1-based line number. Returns `sc.Err()` wrapped if non-nil. Duplicate lines collapse via `HashSet.Add` semantics (no error raised).

**`pkg/snapshot/manifest_test.go`** — black-box tests (`package snapshot_test`) covering:

| Test                                       | Pins                                                                |
| ------------------------------------------ | ------------------------------------------------------------------- |
| TestSkeleton_SentinelExists                | Sentinel non-nil after Task 1                                       |
| TestWriteRead_RoundTrip (n=0,1,1000)       | Lossless round-trip across empty, singleton, mid-size sets          |
| TestWriteRead_SortedAscending              | Wire bytes strictly ascending regardless of insertion order         |
| TestRead_RejectsMalformed (4 sub-cases)    | 63/65/non-hex/embedded-space → `errors.Is(err, sentinel)` + line N  |
| TestRead_EmptyInput_ReturnsEmptySet        | Zero-block snapshot is valid                                        |
| TestRead_ToleratesCRLF                     | Round-trips CRLF input cleanly                                      |
| TestRead_LargeBuffer_Handles100k           | 100k hashes (~6.4 MiB) — scanner buffer regression guard            |
| TestWriteAtomic_CompleteFileOnly           | `.tmp` sidecar absent post-success; canonical file readable         |
| TestWriteAtomic_NoPartialOnError           | Read-only parent dir → no canonical file left behind (skips Windows + root) |
| TestWriteAtomic_OverwritesExisting         | Second write fully replaces the first                               |
| TestWriteRead_DuplicateLinesCollapse       | Documented HashSet de-dup semantics for hand-edited manifests       |

11 tests total, all green under `go test -race`.

## Deviations from Plan

None. Plan executed as written.

One signature-style clarification: `WriteManifestAtomic` initially used a named return (`(retErr error)`) to drive the defer-cleanup; rewritten to a plain `error` return with the writeAndSync helper to satisfy the strict acceptance grep `func WriteManifestAtomic(path string, hs *blockstore.HashSet) error`. No behavioral change.

## Verification Results

- `go build ./pkg/snapshot/...` — exit 0
- `go vet ./pkg/snapshot/...` — exit 0
- `go test ./pkg/snapshot/... -count=1 -race` — `ok` 1.679s, 11 tests
- `gofmt -l pkg/snapshot/` — empty (clean)
- All Task 1/2/3 acceptance grep checks pass.

## Commits

| Task | Hash      | Type | Subject                                                                  |
| ---- | --------- | ---- | ------------------------------------------------------------------------ |
| 1    | 8947254c  | feat | snapshot package skeleton + ErrInvalidManifestLine sentinel              |
| 2    | 08d13fd1  | feat | implement WriteManifest/WriteManifestAtomic/ReadManifest                 |
| 3    | 004204c1  | test | round-trip + atomicity + malformed + 100k-set tests                      |

## Self-Check

```
FOUND: pkg/snapshot/doc.go
FOUND: pkg/snapshot/manifest.go
FOUND: pkg/snapshot/manifest_test.go
FOUND: 8947254c
FOUND: 08d13fd1
FOUND: 004204c1
```

## Self-Check: PASSED
