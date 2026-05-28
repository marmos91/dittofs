# SMB2.1 WPTS Fix Plan

## Status
- Test suite confirmed running (11 previous tests still pass based on negotiate/session tests seen)
- New failures identified with specific root causes below

## Issues Found (from code review + test output)

### 1. Missing ARCHIVE attribute on regular files
**File**: `internal/adapter/smb/v2/handlers/converters.go:77-111`
**Fix**: In `fileAttrToSMBAttributesInternal()`, add `FileAttributeArchive` for regular files.
WPTS error: "Assert.IsTrue failed. The FileAttributes of the entry should contain ARCHIVE."

```go
// After the switch block, before hidden check:
case metadata.FileTypeRegular:
    attrs |= types.FileAttributeArchive  // ADD THIS LINE
    if attr.Size == 0 {
        attrs |= types.FileAttributeNormal
    }
```

Note: `FileAttributeNormal` MUST NOT be combined with other attributes per MS-FSCC.
So when ARCHIVE is set, don't also set NORMAL. Fix: remove the `FileAttributeNormal` for empty files
since ARCHIVE alone is sufficient. Only set NORMAL when NO other attributes are set.

### 2. CREATE response doesn't encode create contexts
**File**: `internal/adapter/smb/v2/handlers/create.go:182-202`
**Fix**: `CreateResponse.Encode()` always returns 89 bytes ignoring `resp.CreateContexts`.
When lease response contexts exist, they must be serialized and appended.
Need to encode create contexts chain and update CreateContextsOffset/Length fields.

### 3. QueryDirectory DOS wildcard patterns not handled
**File**: `internal/adapter/smb/v2/handlers/query_directory.go:848-888`
**Fix**: `matchSMBPattern()` uses `filepath.Match` which doesn't handle DOS wildcards:
- `<` (DOS_STAR) = match zero or more chars, not matching extension
- `>` (DOS_QM) = match any single char or end of name
- `"` (DOS_DOT) = match period or end of name
These cause "unexpected end of channel stream" because the pattern fails to match,
returning empty results, and possibly truncating the response.
Need to implement proper DOS wildcard translation per MS-FSCC 2.1.4.4.

### 4. QueryDirectory response truncation
**File**: `internal/adapter/smb/v2/handlers/query_directory.go:334-336`
**Fix**: When `entries` exceeds `OutputBufferLength`, we naively truncate:
```go
entries = entries[:req.OutputBufferLength]
```
This corrupts the last entry (cuts it in the middle). Must truncate at entry boundaries
by checking `NextEntryOffset` fields.

### 5. AllocationSize should differ from EndOfFile in dir entries
**File**: Multiple dir entry builders in `query_directory.go`
**Fix**: `AllocationSize` is set to `size` (same as EndOfFile) but should be cluster-aligned:
```go
binary.LittleEndian.PutUint64(entry[48:56], size)  // AllocationSize - WRONG
```
Should use `calculateAllocationSize(size)` instead.

### 6. FileId expected to be 0 in some tests
WPTS error: "FileId of the entry should be 0"
The WPTS test `BVT_QueryDirectory_FileIdFullDirectoryInformation` creates a file and
expects FileId=0. Our code uses `entries[i].ID` from metadata store. Need to investigate
if the test expects 0 because the server hasn't assigned a proper file reference number,
or if we need to use the FILE_INTERNAL_INFORMATION IndexNumber instead.

## Tests Already in KNOWN_FAILURES (Phase 39+ / SMB3)
All 49 tests in KNOWN_FAILURES.md are correctly categorized as needing SMB3+ features:
- SMB 3.1.1 negotiate/preauthentication (9 tests)
- Encryption (7 tests)
- Signing (1 test - BVT_Signing)
- DFS (7 tests)
- SWN (6 tests)
- VSS (4 tests)
- Directory leasing (1 test)

These are covered by Phase 39 (SMB3 Security & Encryption) and Phase 44 (SMB3 Conformance).

## GSD Coverage
The ROADMAP.md already has:
- Phase 30: SMB Bug Fixes (sparse file READ, renamed dir listing)
- Phase 39-44: Full SMB3 upgrade including all 3.x features
- Phase 44: SMB3 Conformance Testing (extends Phase 29.8 WPTS infrastructure)

The SMB2.1 fixes above should be done as part of this PR (fix/smb-conformance branch).

## Fix Priority (for this PR)
1. ARCHIVE attribute (trivial fix, unlocks many QueryDirectory tests)
2. AllocationSize cluster alignment in dir entries (trivial)
3. CREATE context encoding (medium - needed for lease tests)
4. DOS wildcard patterns (medium - needed for search pattern tests)
5. QueryDirectory truncation at entry boundaries (medium)
6. FileId investigation (needs more research)
