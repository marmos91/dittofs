---
phase: 05
plan: 05
subsystem: nfs-v4-handlers
tags: [nfs, v4, restore, safety-rails, atomic, boot-verifier]
requires:
  - Phase 1 manifest (unchanged)
  - Phase 4 storebackups.Service skeleton (downstream caller)
provides:
  - atomic.Pointer-backed serverBootVerifier
  - exported BumpBootVerifier() for Phase 5 RunRestore (D-09)
  - internal bootVerifierBytes() snapshot accessor
affects:
  - internal/adapter/nfs/v4/handlers/write.go
  - internal/adapter/nfs/v4/handlers/commit.go
  - internal/adapter/nfs/v4/handlers/io_test.go
  - internal/adapter/nfs/v4/handlers/verifier_test.go (new)
tech-stack:
  added:
    - sync/atomic.Pointer[[8]byte] (standard library, Go 1.19+)
  patterns:
    - lock-free atomic swap for hot-path read, cold-path write
key-files:
  created:
    - internal/adapter/nfs/v4/handlers/verifier_test.go
  modified:
    - internal/adapter/nfs/v4/handlers/write.go
    - internal/adapter/nfs/v4/handlers/commit.go
    - internal/adapter/nfs/v4/handlers/io_test.go
decisions:
  - Used atomic.Pointer[[8]byte] over sync.Mutex-guarded slice — lock-free reads on every WRITE/COMMIT hot path.
  - bootVerifierBytes() returns a copy ([8]byte) rather than a pointer to prevent callers from mutating the live stored value.
  - Test placed in new verifier_test.go (not io_test.go or write_test.go) — isolates the new concurrency contract in its own file.
  - Added a second test (TestBumpBootVerifier_ConcurrentReadsAreConsistent) beyond the plan's single test to exercise the atomic swap under -race with multiple readers and bumpers.
metrics:
  duration_minutes: ~5
  completed: 2026-04-16
---

# Phase 5 Plan 05: Atomic serverBootVerifier + BumpBootVerifier Summary

Hoisted NFSv4 `serverBootVerifier` from a package-level `[8]byte` set once in `init()` to an `atomic.Pointer[[8]byte]`, and exported `BumpBootVerifier()` so Phase-5 `RunRestore` can force NFSv4 clients reconnecting after a restore into the reclaim-grace path (D-09 belt-and-suspenders).

## What Changed

### `internal/adapter/nfs/v4/handlers/write.go`

- Added `sync/atomic` import.
- Replaced the package-level `var serverBootVerifier [8]byte` + its `init()` with:
  - `var serverBootVerifier atomic.Pointer[[8]byte]` (line 30)
  - `init()` at line 32: generates 8 time-derived bytes and `Store()`s a pointer.
  - `BumpBootVerifier()` at line 44: exported; same generation logic, swaps atomically.
  - `bootVerifierBytes()` at line 53: internal snapshot accessor, returns a `[8]byte` copy (callers cannot mutate the live value).
- Updated the single WRITE response call site at **line 280** (was line 243 pre-edit):
  ```go
  verf := bootVerifierBytes()
  buf.Write(verf[:])
  ```

### `internal/adapter/nfs/v4/handlers/commit.go`

- Updated the single COMMIT response call site at **line 161**:
  ```go
  verf := bootVerifierBytes()
  buf.Write(verf[:])
  ```

### `internal/adapter/nfs/v4/handlers/io_test.go`

- Updated `TestWrite_Success` verifier assertion at **line 977**:
  ```go
  want := bootVerifierBytes()
  if !bytes.Equal(verf, want[:]) { ... }
  ```
- Updated `TestCommit_WriteThenCommit` verifier assertion at **line 1101**:
  ```go
  want := bootVerifierBytes()
  if !bytes.Equal(verf, want[:]) { ... }
  ```

### `internal/adapter/nfs/v4/handlers/verifier_test.go` (new)

- `TestBumpBootVerifier_ChangesValue` — exactly the test specified in the plan. Snapshot → sleep 1ms → `BumpBootVerifier()` → snapshot → assert different.
- `TestBumpBootVerifier_ConcurrentReadsAreConsistent` — extra concurrency test. 8 reader goroutines + 2 bumper goroutines × 200 iterations each, running under `-race`. Asserts the final loaded verifier is non-zero (sanity) and the race detector finds nothing.

## Verification

**Acceptance criteria** (all from plan `<acceptance_criteria>`):

| Criterion | Result |
|-----------|--------|
| `grep 'serverBootVerifier atomic.Pointer\[\[8\]byte\]' write.go` | 1 match (line 30) |
| `grep 'func BumpBootVerifier' write.go` | 1 match (line 44) |
| `grep 'func bootVerifierBytes' write.go` | 1 match (line 53) |
| `grep -rn 'serverBootVerifier\[:\]' handlers/` | **0 matches** |
| `grep -rn 'bootVerifierBytes()' handlers/` | 10 call sites across 4 files (write.go, commit.go, io_test.go, verifier_test.go) |
| `grep 'func TestBumpBootVerifier_ChangesValue' handlers/` | 1 match (verifier_test.go:11) |
| `go build ./internal/adapter/nfs/v4/handlers/...` | clean (exit 0) |
| `go vet ./internal/adapter/nfs/v4/handlers/...` | clean (exit 0) |
| `go test ./internal/adapter/nfs/v4/handlers/... -count=1 -race` | **PASS** (1.902s) |
| `go build ./...` (full tree) | clean |

**Race test result:** `ok github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers 1.902s` — race detector found no data races across 8 reader × 2 bumper concurrent goroutines.

## Deviations from Plan

None — plan executed exactly as written. One additive change: added `TestBumpBootVerifier_ConcurrentReadsAreConsistent` as a second test in `verifier_test.go` to exercise the atomic swap under the race detector. This strengthens the "concurrent reads are race-free" `<behavior>` assertion beyond the plan's single test without changing scope.

## Commits

| Hash      | Type  | Description                                                |
|-----------|-------|------------------------------------------------------------|
| e8393b7a  | test  | add failing test for BumpBootVerifier (RED)                |
| 10b6b76f  | feat  | atomic serverBootVerifier with BumpBootVerifier export (GREEN) |

## Downstream Hook

`storebackups.Service.RunRestore` (to be implemented in a later Phase-5 plan) can now invoke `handlers.BumpBootVerifier()` after a successful metadata swap. The atomic pointer swap is lock-free and safe to call concurrently with in-flight WRITE/COMMIT handlers; hot-path readers pay only an atomic load + dereference + struct copy (8 bytes, negligible).

## Self-Check: PASSED

- `internal/adapter/nfs/v4/handlers/verifier_test.go` — exists
- Commit `e8393b7a` — found in log
- Commit `10b6b76f` — found in log
