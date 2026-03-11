---
status: complete
phase: 30-smb-bug-fixes
source:
  - 30-01-SUMMARY.md
  - 30-02-SUMMARY.md
  - 30-03-SUMMARY.md
  - 30-04-SUMMARY.md
started: 2026-02-27T15:30:00Z
updated: 2026-02-27T16:10:00Z
---

## Current Test

[testing complete]

## Tests

### 1. Sparse file reads return zeros instead of errors
expected: Reading unwritten regions of a sparse file returns zero-filled data instead of ErrBlockNotFound. Unit tests pass: `go test ./pkg/payload/offloader/ ./pkg/payload/io/`
result: pass

### 2. Directory rename propagates paths to all descendants
expected: Renaming a directory updates the Path field of all descendant files/directories via BFS traversal. Memory store persists Path correctly. Unit tests pass: `go test ./pkg/metadata/ -run "Move|Path"`
result: pass

### 3. Parent directory navigation (..) works in SMB CREATE
expected: walkPath resolves ".." path segments via metaSvc.Lookup, enabling correct parent directory navigation. Unit tests pass for walkPath.
result: pass

### 4. NumberOfLinks uses actual attr.Nlink value
expected: FileStandardInfo.NumberOfLinks reflects the actual link count (minimum 1) instead of hardcoded 1. Unit tests pass for converters.
result: pass

### 5. Cross-protocol oplock breaks from NFS to SMB
expected: NFS v3 READ/WRITE/REMOVE/RENAME handlers trigger SMB oplock breaks via OplockBreaker interface. Fire-and-forget pattern. Code compiles and all NFS handler tests pass.
result: pass

### 6. Pipe share list caching with invalidation
expected: SMB Handler caches share list for IPC$ pipe CREATE with RWMutex. Cache invalidated via Runtime.OnShareChange(). pkg/adapter and pkg/adapter/smb build successfully.
result: pass

## Summary

total: 6
passed: 6
issues: 0
pending: 0
skipped: 0

## Gaps

[none]
