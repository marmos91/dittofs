---
phase: 11-cas-write-path-gc-rewrite-a2
plan: 03
subsystem: blockstore
tags: [cas, blake3, verifier, dual-read, cache-bifurcation, inv-06, bscas-06, error-mapping]

requires:
  - phase: 11-cas-write-path-gc-rewrite-a2
    provides: "FormatCASKey/ParseCASKey, RemoteStore.WriteBlockWithHash + GetObjectMetadata, ErrCASContentMismatch + ErrCASKeyMalformed sentinels (plan 11-01); engine.Syncer/uploadOne PUT-then-meta ordering and FileBlock.State (plan 11-02)"
provides:
  - "RemoteStore.ReadBlockVerified interface method (s3 + memory impls)"
  - "s3.verifyingReader streaming BLAKE3 io.Reader wrapper (D-18, zero extra body alloc)"
  - "s3.Store.ReadBlockVerified with x-amz-meta-content-hash pre-check (D-19) + body recompute"
  - "memory.Store.ReadBlockVerified (in-process tests)"
  - "Engine dual-read resolver: dispatchRemoteFetch routes by FileBlock.Hash.IsZero() (D-21)"
  - "ReadBuffer cache key bifurcation: keyKindCoord/keyKindCAS/keyKindLegacy with shared LRU budget (D-22)"
  - "GetCAS/PutCAS/GetLegacy/PutLegacy/InvalidateCAS/InvalidateLegacy/ContainsCAS/ContainsLegacy"
  - "ErrCASContentMismatch + ErrCASKeyMalformed wired into MapContentToNFS3/4/SMB"
  - "remotetest.RunReadBlockVerifiedSuite conformance sub-suite"
affects: [11-04-flush-eviction, 11-05-restart-recovery, 11-06-gc-mark-sweep, 11-08-e2e]

tech-stack:
  added: []
  patterns:
    - "Streaming io.Reader wrapper with hash on EOF (verifier pattern carried from Phase 10 chunker)"
    - "Discriminated key type for coexisting cache spaces (Approach A from plan)"
    - "Spying RemoteStore wrapper with per-method counter + recorded keys (reusable in plan 11-06 GC tests)"
    - "Wrapped sentinel mapping in adapter common (errors.Is chain, no string match)"

key-files:
  created:
    - "pkg/blockstore/remote/s3/verifier.go"
    - "pkg/blockstore/remote/s3/verifier_test.go"
    - "pkg/blockstore/engine/engine_dualread_test.go"
    - "pkg/blockstore/engine/cache_bifurcation_test.go"
    - "internal/adapter/common/content_errmap_test.go"
  modified:
    - "pkg/blockstore/remote/remote.go (interface + ReadBlockVerified contract)"
    - "pkg/blockstore/remote/s3/store.go (ReadBlockVerified)"
    - "pkg/blockstore/remote/memory/store.go (ReadBlockVerified mirror)"
    - "pkg/blockstore/remote/remotetest/suite.go (RunReadBlockVerifiedSuite)"
    - "pkg/blockstore/engine/fetch.go (resolveFileBlock + dispatchRemoteFetch + wired callers)"
    - "pkg/blockstore/engine/cache.go (discriminated blockKey + bifurcated API)"
    - "internal/adapter/common/content_errmap.go (CAS sentinel mapping)"
    - "pkg/controlplane/runtime/blockgc_test.go (fakeRemoteStore.ReadBlockVerified stub)"

key-decisions:
  - "Cache approach: A (discriminated key type). Rationale — existing blockKey was already a typed struct (not a string), and adding a kind discriminator keeps Approach A's type-safety while preserving the existing Get/Put coordinate-space API for engine.ReadAt callers. Approach B (prefixed strings) would have required reshaping the entire entries map and offered no practical advantage."
  - "Verifier filename is verifier.go (per plan). Companion test file verifier_test.go covers happy/tampered/early-close/partial-read/double-close + readAllVerified pre-sized and fallback paths."
  - "verifyingReader.Close before EOF returns ErrCASContentMismatch — caller-side abandon is treated as untrusted bytes (we cannot prove the hash matched)."
  - "Header pre-check uses resp.Metadata[\"content-hash\"] (lower-case) because the AWS SDK normalizes user metadata keys before exposing them. The wire header is x-amz-meta-content-hash."
  - "readAllVerified pre-sized path forces a one-byte trailing read after io.ReadFull so the verifier observes io.EOF and runs the hash check; without this the hasher would be silent on exact-fill responses."
  - "On verification failure readAllVerified returns nil for the buffer (not the partial bytes). INV-06 strictly forbids surfacing bytes that failed verification."
  - "dispatchRemoteFetch returns (storeKey, data, err) so the caller can both diagnose and (where applicable) cache by storeKey for the legacy path."
  - "Spying RemoteStore wrapper exposes readCalls / readVerifiedCalls atomic counters and recorded keys; reusable in plan 11-06 for GC sweep assertions."
  - "Cache byFile secondary index remains coord-only; CAS entries are not file-scoped (cross-file dedup) and legacy entries are time-bounded by Phase 14 migration."
  - "SMB CAS-mismatch maps to StatusUnexpectedIOError because the smbtypes table does not include a StatusDataError constant. The plan called for StatusDataError but that does not exist in our types; StatusUnexpectedIOError is the closest analog and is already the fallback for opaque content errors."

requirements-completed: [INV-06, BSCAS-06]

duration: 21min
completed: 2026-04-25
---

# Phase 11 Plan 03: Streaming BLAKE3 Verifier + Dual-Read Engine + Cache Bifurcation

**Every CAS GET is BLAKE3-verified end-to-end via a streaming io.Reader wrapper (INV-06); the engine routes per-block reads between the new CAS path and the legacy {payloadID}/block-N path based on metadata key shape (D-21); the read buffer's key space is bifurcated so CAS reads and legacy reads coexist without collision through the dual-read window (D-22); CAS verification errors are mapped to per-protocol I/O codes in adapter common.**

## Performance

- **Duration:** ~21 min
- **Started:** 2026-04-25T15:48:00Z (worktree base)
- **Completed:** 2026-04-25T16:09:46Z
- **Tasks:** 4 (all TDD, 4 commits)
- **Files created:** 5 / **modified:** 8

## Accomplishments

### Task 1 — Streaming BLAKE3 verifier + ReadBlockVerified (commit `efe57e39`)

- New `pkg/blockstore/remote/s3/verifier.go` with `verifyingReader` (single-use, not goroutine-safe) wrapping any `io.ReadCloser` and feeding bytes through `blake3.Hasher` as the caller reads them. On `io.EOF`, the accumulated hash is compared to expected; mismatch returns `ErrCASContentMismatch` instead of `io.EOF`. Close-before-EOF is also surfaced as mismatch — we cannot prove the bytes were correct.
- New `readAllVerified` helper handles both pre-sized (`ContentLength` known) and fallback (`io.ReadAll`-style) paths, discarding the buffer on mismatch so corrupt bytes never escape.
- `s3.Store.ReadBlockVerified(ctx, blockKey, expected)`:
  - GETs the object via the existing AWS SDK path
  - **Header pre-check (D-19):** if `resp.Metadata["content-hash"]` is non-empty and != `expected.CASKey()`, returns `ErrCASContentMismatch` BEFORE reading any body bytes.
  - **Body recompute (D-18):** wraps `resp.Body` in `verifyingReader`, reads via `readAllVerified`. Both fail-closed.
- `memory.Store.ReadBlockVerified` mirrors the same fail-closed semantics for in-process tests; uses recorded `metadata["content-hash"]` for the header pre-check and re-hashes stored bytes for the body recompute.
- `RemoteStore` interface gains `ReadBlockVerified`. `pkg/controlplane/runtime/blockgc_test.go` fakeRemoteStore stub updated.
- `remotetest` gains `RunReadBlockVerifiedSuite` — every backend gets HappyPath / BodyMismatch / HeaderPreCheck / NotFound coverage.
- Verifier unit tests (`verifier_test.go`): HappyPath, BodyTampered, EarlyClose, PartialReads (one byte at a time), DoubleClose; `readAllVerified` ContentLengthExact, ContentLengthMismatch, NoContentLength.

### Task 2 — Dual-read engine resolver (commit `c47ffd60`)

- New `Syncer.resolveFileBlock(ctx, payloadID, blockIdx) (*FileBlock, error)` returns the row so callers can decide CAS vs legacy.
- New `Syncer.dispatchRemoteFetch(ctx, fb) (storeKey, data, err)`:
  - `fb.Hash != zero` → CAS path. Uses `fb.BlockStoreKey` if set, else falls back to `FormatCASKey(fb.Hash)`. Calls `ReadBlockVerified`.
  - `fb.BlockStoreKey != ""` (legacy, hash zero) → calls plain `ReadBlock` with no verification.
  - Otherwise sparse → returns `("", nil, nil)`.
- `fetchBlock` and `inlineFetchOrWait` in `pkg/blockstore/engine/fetch.go` now call `dispatchRemoteFetch` instead of constructing the storeKey manually and calling `ReadBlock`. The legacy `resolveStoreKey` is retained for the in-flight dedup map keying (it now also falls back to `FormatCASKey` when only `Hash` is set, defensively).
- New `engine_dualread_test.go` introduces `spyingRemoteStore` (atomic counters + recorded keys per method) and four tests:
  - `CASRowRoutesToVerified`: a row with non-zero Hash routes through `ReadBlockVerified(FormatCASKey(Hash), Hash)`.
  - `LegacyRowRoutesToReadBlock`: a row with zero Hash + legacy BlockStoreKey routes through `ReadBlock(BlockStoreKey)`.
  - `NoFileBlockReturnsNil`: missing metadata row → no remote call, nil result.
  - `CASRowMismatchSurfacesError`: corrupt bytes at the CAS key surface `ErrCASContentMismatch` end-to-end.

### Task 3 — Cache key bifurcation (commit `176e6d07`)

- **Approach A** (discriminated key type) chosen — `blockKey` gains a `kind keyKind` discriminator with `keyKindCoord` / `keyKindCAS` / `keyKindLegacy`. Cross-space collision is impossible by construction since `blockKey` is value-compared and the `kind` byte differs.
- Three constructor helpers: `coordKey(payloadID, blockIdx)`, `casKey(hash)`, `legacyKey(storeKey)`.
- Refactored `Put` and `Get` into shared `putAt(blockKey, data, dataSize)` and `getAt(blockKey, dest, offset)` so eviction / LRU promotion is identical across spaces.
- New API: `GetCAS / PutCAS` (keyed by `ContentHash`), `GetLegacy / PutLegacy` (keyed by legacy storeKey string), `InvalidateCAS`, `InvalidateLegacy`, `ContainsCAS`, `ContainsLegacy`.
- `byFile` secondary index is gated to `keyKindCoord` only (CAS spans files; legacy entries are time-bounded by Phase 14 migration).
- Existing `Get`/`Put`/`Invalidate` API unchanged for current callers — they delegate to the coord space.
- All three spaces share the same `maxBytes` budget; eviction is single-policy LRU.
- New `cache_bifurcation_test.go` covers:
  - CAS round-trip (`PutCAS` then `GetCAS` returns identical bytes).
  - Legacy round-trip.
  - **NoCrossSpaceCollision (LOAD-BEARING)**: hash X is stored via `PutCAS`; the same string `FormatCASKey(hash)` is also stored via `PutLegacy`; coord `Put("share/file", 0, ...)` is also stored. All three Get methods return their own bytes — none leaks.
  - `BifurcatedSpacesShareBudget`: 3 CAS entries in a 2-block budget evicts the oldest (LRU works across spaces).
  - `InvalidateFile_OnlyCoordEntries`: a CAS entry that "happens to share a hex prefix" with the invalidated payloadID survives.
  - `InvalidateCAS` / `InvalidateLegacy` return present-and-removed booleans.

### Task 4 — Adapter common error map (commit `269b90cf`, fixes Warning 3 from review)

- `MapContentToNFS3/4/SMB` extended via `errors.Is` chains to recognize:
  - `blockstore.ErrCASContentMismatch` → `NFS3ErrIO` / `NFS4ERR_IO` / `StatusUnexpectedIOError`. The streaming verifier already discarded the bytes; the protocol surfaces this as an I/O error.
  - `blockstore.ErrCASKeyMalformed` → `NFS3ErrInval` / `NFS4ERR_INVAL` / `StatusInvalidParameter`. CAS key parse failure indicates corrupted metadata; surfaced as invalid argument.
- `internal/adapter/common/content_errmap_test.go` covers each sentinel both wrapped (`fmt.Errorf("...: %w", sentinel)`) and direct, plus a no-regression assertion on `ErrRemoteUnavailable` + opaque errors.
- SMB caveat: the plan called for `StatusDataError` but our `smbtypes` table does not expose that constant. `StatusUnexpectedIOError` is the closest analog and is also what the existing fallback uses for opaque I/O failures. Documented inline.

## Design Decisions

### Approach A vs B for cache bifurcation

The plan offered two approaches: A (discriminated key type) or B (prefixed string keys). The existing `ReadBuffer` keys by a typed struct `blockKey{payloadID, blockIdx}`, not by a string — so Approach A is the natural extension. Approach B would have required reshaping the `entries map[blockKey]*list.Element` to `map[string]*list.Element`, breaking secondary-index logic, and offered no practical advantage (collision protection is guaranteed by construction in both approaches). Picked A.

### Spying RemoteStore wrapper reusable in plan 06 GC tests

The `spyingRemoteStore` introduced in `engine_dualread_test.go` exposes `readCalls / readVerifiedCalls` atomic counters plus per-method recorded keys (`readKeys`, `readVerifiedKeys`). Plan 11-06 (GC mark-sweep) needs to assert that the sweep phase walks `cas/XX/YY/*` prefixes and issues `DeleteByPrefix` / `DeleteBlock` calls in the expected order. The same wrapper pattern can be extended with `deleteCalls` / `listCalls` fields and reused there. The pattern is documented in this Summary so plan 06's planner can find it.

### Header pre-check vs body recompute layering

Per D-19, both checks are mandatory and the system is fail-closed twice. The header pre-check is a cheap optimization: if S3 metadata says the object hash is X and we asked for Y, we know they cannot match without reading the body. But the header alone is NOT sufficient (would trust S3 metadata layer). The body recompute is the actual integrity guarantee. Implementation order: `_ = resp.Body.Close()` is called explicitly before returning the header-mismatch error so we don't leak the underlying connection.

### Cache byFile gating

The pre-existing `byFile map[string]map[uint64]struct{}` secondary index supports `InvalidateFile(payloadID)` in O(blocks-for-file) time. CAS entries cannot be tracked in this index — they have no payloadID association (cross-file dedup). Legacy entries technically have a payloadID embedded in the storeKey string, but parsing it on every `Put` would defeat the purpose. The clean solution: gate index maintenance to `keyKindCoord`. CAS and legacy entries will be naturally evicted by LRU; explicit `InvalidateCAS / InvalidateLegacy` are provided for callers that need targeted removal.

## Deviations from Plan

### Auto-fixed issues

**1. [Rule 3 — Blocking issue] fakeRemoteStore in pkg/controlplane/runtime/blockgc_test.go missing ReadBlockVerified**

- **Found during:** Task 1 (after extending the `RemoteStore` interface)
- **Issue:** `go vet ./...` reported the existing fake didn't implement the new method.
- **Fix:** Added a no-op `ReadBlockVerified` stub matching the interface.
- **Files modified:** `pkg/controlplane/runtime/blockgc_test.go`
- **Commit:** `efe57e39` (rolled into Task 1)

**2. [Rule 3 — Cache file location] Plan referenced pkg/blockstore/cache/cache.go which does not exist**

- **Found during:** Task 3 reading
- **Issue:** Plan 11-03 referenced `pkg/blockstore/cache/cache.go` and `pkg/blockstore/cache/cache_test.go`. The actual cache lives at `pkg/blockstore/engine/cache.go` (the `ReadBuffer` type). There is no `pkg/blockstore/cache/` directory.
- **Fix:** Adapted Task 3 to operate on `pkg/blockstore/engine/cache.go` directly. Tests live at `pkg/blockstore/engine/cache_bifurcation_test.go`.
- **Files modified:** None additional (this is a plan-vs-tree path discrepancy, not a code bug).
- **Commit:** `176e6d07`

**3. [Rule 1 — SMB constant absence] StatusDataError does not exist in smbtypes**

- **Found during:** Task 4 reading
- **Issue:** Plan called for SMB mapping `ErrCASContentMismatch → StatusDataError`. The `smbtypes` table does not export that constant.
- **Fix:** Used `StatusUnexpectedIOError` (already the fallback for opaque content errors). Documented inline and in this Summary.
- **Files modified:** `internal/adapter/common/content_errmap.go`
- **Commit:** `269b90cf`

### Required architectural changes (Rule 4)

None — the plan was implementable as written, modulo the path/constant discrepancies above which are cosmetic.

## Authentication Gates

None — this plan is pure code with no external auth required.

## Testing

- 11 new test functions (5 verifier unit + 4 dual-read + 7 cache-bifurcation + 5 errmap = 21 sub-tests, several with multiple sub-cases)
- All `pkg/blockstore/...` and `internal/adapter/common/...` short tests pass with `-race`
- `go vet ./...` clean
- `go build ./...` clean

## Risks Surfaced for Downstream Plans

- **Plan 11-04 (flush + LSL-07 + LSL-08):** the bifurcated cache adds CAS/legacy entries to the same byte budget. Plan 04's eviction work (LSL-08 in-process LRU keyed by ContentHash on disk) should be aware of this — it operates on the disk-side, and the read-buffer here is the in-memory side. They are separate budgets.
- **Plan 11-05 / 11-06 (GC):** `spyingRemoteStore` pattern in `engine_dualread_test.go` is reusable for GC sweep assertions. See Design Decisions above.
- **Plan 11-08 (E2E):** `TestBlockStoreImmutableOverwrites` should now exercise the verified read path because every block uploaded under the CAS write path (plan 11-02) carries `x-amz-meta-content-hash`, and the dual-read resolver in plan 11-03 routes through `ReadBlockVerified` for those blocks.

## Self-Check

- `pkg/blockstore/remote/s3/verifier.go`: FOUND
- `pkg/blockstore/remote/s3/verifier_test.go`: FOUND
- `pkg/blockstore/engine/engine_dualread_test.go`: FOUND
- `pkg/blockstore/engine/cache_bifurcation_test.go`: FOUND
- `internal/adapter/common/content_errmap_test.go`: FOUND
- Commit `efe57e39` (Task 1): FOUND
- Commit `c47ffd60` (Task 2): FOUND
- Commit `176e6d07` (Task 3): FOUND
- Commit `269b90cf` (Task 4): FOUND
- `grep ErrCASContentMismatch internal/adapter/common/content_errmap.go`: 3 hits
- `grep ErrCASKeyMalformed internal/adapter/common/content_errmap.go`: 3 hits

## Self-Check: PASSED
