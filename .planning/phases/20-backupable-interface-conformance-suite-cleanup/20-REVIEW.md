---
phase: 20-backupable-interface-conformance-suite-cleanup
reviewed: 2026-05-27T10:30:41Z
depth: standard
files_reviewed: 7
files_reviewed_list:
  - pkg/blockstore/hashset.go
  - pkg/blockstore/hashset_test.go
  - pkg/metadata/backupable.go
  - pkg/metadata/backupable_test.go
  - pkg/metadata/backup/envelope.go
  - pkg/metadata/backup/envelope_test.go
  - pkg/metadata/storetest/backup_conformance.go
findings:
  critical: 0
  warning: 2
  info: 2
  total: 4
status: issues_found
---

# Phase 20: Code Review Report (Pass 2)

**Reviewed:** 2026-05-27T10:30:41Z
**Depth:** standard
**Files Reviewed:** 7
**Status:** issues_found

## Summary

Second review pass after fixes from the first review. All five original findings (CR-01, CR-02, WR-01, WR-02, WR-03) are resolved:

- **CR-01** (CRC before write): `Writer.Write` now correctly accumulates `p[:n]` only after the write succeeds (lines 111-114 of `envelope.go`).
- **CR-02** (t.Fatal from goroutine): The `ConcurrentWriter` test now uses direct store API calls with error returns instead of `createTestFile`, with a clear comment explaining why (lines 246-247).
- **WR-01** (dead first attempt in envelope test): Removed; `envelope_test.go` is clean with no dead code.
- **WR-02** (weak ConcurrentWriter): Accepted as-is with non-fatal `t.Errorf` for the hash count check.
- **WR-03** (unchecked errors in tests): The `writeValidEnvelope` helper now properly checks all error returns.

Two new warnings remain from deeper analysis of the API design and test correctness. Two informational items carry over from the first pass as acknowledged low-priority items.

## Warnings

### WR-01: ReadHeader API has undocumented payload-length-framing requirement

**File:** `pkg/metadata/backup/envelope.go:132-177`
**Issue:** `ReadHeader` returns a `payloadReader` (tee reader wrapping the original `r`) but the envelope wire format has no payload length field. If a caller uses `io.ReadAll(payloadReader)` -- the natural pattern for variable-length streams -- the trailing 4-byte CRC gets read through the tee reader and accumulated into the CRC hash, corrupting `VerifyCRC`. The conformance suite's own `WrongEngineTag` test at `backup_conformance.go:394-402` demonstrates this footgun: it calls `io.ReadAll(payloadReader)` and must manually strip the last 4 bytes, with a comment acknowledging the issue.

This means every engine driver that implements `Backupable.Backup`/`Restore` must encode its own payload-length framing so the reader knows exactly how many bytes to consume before calling `VerifyCRC`. This contract is not documented on `ReadHeader` or anywhere in the package doc. A future engine implementer will hit this silently -- the CRC will just fail with `ErrCRCMismatch` and the root cause will not be obvious.

**Fix:** Add explicit documentation to `ReadHeader`'s godoc:

```go
// IMPORTANT: The envelope wire format does not encode payload length.
// The caller (engine driver) MUST embed its own length framing within
// the payload so it knows exactly how many bytes to read through
// payloadReader before calling VerifyCRC. Using io.ReadAll(payloadReader)
// will read the trailing CRC bytes through the tee, corrupting the
// CRC accumulator.
```

Alternatively, redesign the API to include a payload length field in the wire format, which would allow `ReadHeader` to return an `io.LimitedReader` over the payload.

### WR-02: ConcurrentWriter hash-count assertion cannot fail regardless of snapshot isolation

**File:** `pkg/metadata/storetest/backup_conformance.go:309-315`
**Issue:** The `ConcurrentWriter` test asserts `hashes.Len() != 3` at line 313 to verify that hashes from concurrently-written files do not appear in the backup. However, the concurrent file `concurrent-new.bin` is created with an empty `File` struct (lines 263-271) that has no `Blocks` field set. An empty `Blocks` slice contributes zero hashes to the HashSet. This means `hashes.Len()` will always be 3 (from the two initial files) regardless of whether the backup captured the concurrent file or not. The assertion provides no signal about snapshot isolation.

A conformance test that cannot detect the violation it claims to guard against gives false confidence to engine implementers.

**Fix:** Either (a) add block refs to the concurrent file so its hashes would be visible if captured:

```go
f.Blocks = []blockstore.BlockRef{
    {Hash: hashOfSeed("concurrent-only"), Offset: 0, Size: 1 << 20},
}
```

Then the assertion `hashes.Len() != 3` would actually fail (producing 4) if snapshot isolation is broken. Or (b) acknowledge in a comment that the hash-count check is not a strong isolation test and that the real contract being verified is "backup completes without data races under concurrent writes."

## Info

### IN-01: HashSet.Hashes() exposes internal map with zero production callers

**File:** `pkg/blockstore/hashset.go:66-70`
**Issue:** `Hashes()` returns the internal map by reference. A grep across the codebase shows it is only called in `hashset_test.go:123`. Since `ForEach`, `Sorted`, `Contains`, and `Len` cover all read access patterns, `Hashes()` is dead API surface that increases the risk of accidental mutation. The method is documented as returning a non-copy, so this is intentional, but it may be worth removing if no callers materialize.

**Fix:** Consider removing `Hashes()` or deferring it until a concrete caller needs direct map access.

### IN-02: Empty engine tag accepted by envelope NewWriter

**File:** `pkg/metadata/backup/envelope.go:76-78`
**Issue:** `NewWriter` validates the maximum length (65535 bytes) but accepts an empty string. An empty engine tag produces a valid envelope with `engine_len=0`. While technically parseable, no engine should have an empty identifier. A `Restore` receiving an empty tag would fail with a confusing `ErrEngineMismatch` rather than a clear "empty tag" error.

**Fix:** Add a guard: `if len(engineTag) == 0 { return nil, fmt.Errorf("backup: engine tag must not be empty") }`

---

_Reviewed: 2026-05-27T10:30:41Z_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_
