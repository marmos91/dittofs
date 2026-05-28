---
status: resolved
trigger: "NFSv4 Touch Still Not Updating mtime"
created: 2026-02-17T21:00:00Z
updated: 2026-02-17T22:15:00Z
---

## Current Focus

hypothesis: CONFIRMED - SupportedAttrs() did not advertise FATTR4_TIME_MODIFY_SET (bit 54) or FATTR4_TIME_ACCESS_SET (bit 48)
test: Add these bits to SupportedAttrs() and verify Touch test passes
expecting: Linux NFSv4 client will send time attributes in SETATTR once server advertises support
next_action: COMPLETE

## Symptoms

expected: After os.Chtimes() call, file mtime should be updated to the specified time
actual: Mtime remains unchanged - "1771355481" is not greater than "1771355481"
errors: Test assertion failure, no NFS errors returned
reproduction: Run TestNFSv4BasicOperations/v4.0/Touch E2E test
started: After decode.go fix was applied

## Eliminated

- hypothesis: SET_TO_SERVER_TIME4 not setting Mtime pointer
  evidence: decode.go has the fix applied, binary rebuilt
  timestamp: 2026-02-17T21:05:00Z

- hypothesis: Unit tests failing
  evidence: All decode and setattr unit tests pass
  timestamp: 2026-02-17T21:08:00Z

- hypothesis: GetFile returning stale data from cache
  evidence: GetFile reads directly from store, only merges pending writes
  timestamp: 2026-02-17T21:10:00Z

- hypothesis: SET_TO_CLIENT_TIME4 not setting Mtime
  evidence: Code path at line 286-291 correctly sets setAttrs.Mtime = &t
  timestamp: 2026-02-17T21:25:00Z

- hypothesis: Permission check blocking timestamp update
  evidence: Test runs as root (sudo), isRoot=true bypasses permission checks
  timestamp: 2026-02-17T21:20:00Z

- hypothesis: Auth context UID not being set correctly
  evidence: Linux NFSv4 client sends AUTH_UNIX with correct UID/GID
  timestamp: 2026-02-17T22:12:00Z

## Evidence

- timestamp: 2026-02-17T21:05:00Z
  checked: decode.go SET_TO_CLIENT_TIME4 path
  found: Line 286-291 correctly sets setAttrs.Mtime = &t
  implication: Client time decoding is correct

- timestamp: 2026-02-17T21:07:00Z
  checked: SetFileAttributes permission logic
  found: onlySettingTimesToNow requires AtimeNow || MtimeNow to be true
  implication: When using SET_TO_CLIENT_TIME4, AtimeNow/MtimeNow are false, but root bypasses

- timestamp: 2026-02-17T21:10:00Z
  checked: Permission check logic at file.go:384-404
  found: If not onlySettingTimesToNow and not owner and not root, returns permission denied
  implication: Root user passes (isRoot=true), so this isn't the blocker

- timestamp: 2026-02-17T21:12:00Z
  checked: NFSv3 vs NFSv4 difference
  found: Both use same SetFileAttributes, NFSv3 passes test
  implication: Issue is specific to NFSv4 request handling, not metadata layer

- timestamp: 2026-02-17T21:25:00Z
  checked: All unit tests for decode, handler, and SetFileAttributes
  found: All pass, including TestHandleSetAttr_TimeClientTime
  implication: Issue is at integration level, not unit level

- timestamp: 2026-02-17T22:12:00Z
  checked: Server logs with debug logging enabled
  found: SETATTR called with bitmap=[], all has_* flags false (empty request)
  implication: Linux NFSv4 client not sending time attributes in SETATTR

- timestamp: 2026-02-17T22:13:00Z
  checked: SupportedAttrs() function in encode.go
  found: Missing FATTR4_TIME_ACCESS_SET (bit 48) and FATTR4_TIME_MODIFY_SET (bit 54)
  implication: Server doesn't advertise writable time support, client won't send them

## Resolution

root_cause: The SupportedAttrs() function in internal/protocol/nfs/v4/attrs/encode.go did not include FATTR4_TIME_ACCESS_SET (bit 48) and FATTR4_TIME_MODIFY_SET (bit 54). Without these bits in the supported_attrs response, the Linux NFSv4 client doesn't attempt to set timestamps via SETATTR - it sends an empty attribute bitmap instead.

fix: Added FATTR4_TIME_ACCESS_SET and FATTR4_TIME_MODIFY_SET to SupportedAttrs() function:
```go
SetBit(&bitmap, FATTR4_TIME_ACCESS_SET)  // Writable: allows clients to set atime via SETATTR
SetBit(&bitmap, FATTR4_TIME_MODIFY_SET)  // Writable: allows clients to set mtime via SETATTR
```

verification:
- TestNFSv4AdvancedFileOps/v4.0/Touch now passes
- All NFSv4 AdvancedFileOps tests pass (v3 and v4.0)
- All attrs unit tests pass

files_changed:
- internal/protocol/nfs/v4/attrs/encode.go (SupportedAttrs function)
