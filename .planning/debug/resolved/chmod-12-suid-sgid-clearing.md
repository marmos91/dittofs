---
status: resolved
trigger: "chmod/12.t POSIX test failure - SUID/SGID bits not cleared on non-owner write through NFSv4"
created: 2026-02-19T00:00:00Z
updated: 2026-02-19T17:00:00Z
---

## Current Focus

RESOLVED - Fix applied and verified (build + unit tests pass).
Awaiting CI verification for full POSIX test confirmation.

## Symptoms

expected: When a non-owner user writes to a file with SUID/SGID bits set, those bits should be cleared automatically (POSIX requirement)
actual: chmod/12.t fails 6 of 14 tests (subtests 3-4, 7-8, 11-12) - fstat() after non-owner write still shows SUID/SGID bits set
errors: chmod/12.t (Wstat: 0 Tests: 14 Failed: 6), Failed tests: 3-4, 7-8, 11-12
reproduction: Run pjdfstest chmod/12.t against NFSv4 mount
started: Present since NFSv4 POSIX tests were first added

## Eliminated

- hypothesis: Deferred write path not clearing SUID/SGID bits in store
  evidence: Commit c988366 added immediate store persistence in deferredCommitWrite (io.go lines 232-252). Code correctly clears bits in store and invalidates pending writes cache. The server-side clearing works, but the Linux NFS client never reaches the WRITE because file_remove_privs() SETATTR fails first.
  timestamp: 2026-02-19

- hypothesis: Auth context UID not properly threaded to deferred write path
  evidence: buildV4AuthContext correctly extracts UID from CompoundContext. The UID (65534) is properly passed through PrepareWrite and CommitWrite.
  timestamp: 2026-02-19

- hypothesis: Pending writes not merged in GetFile response
  evidence: MetadataService.GetFile merges pending state including ClearSetuidSetgid flag. GETATTR handler uses this merged state.
  timestamp: 2026-02-19

- hypothesis: NFSv4 WRITE delegation causing stale attribute cache
  evidence: Delegations are enabled by default, but POSIX tests mount with noac,sync,lookupcache=none which should force fresh attribute fetches. Not the primary issue.
  timestamp: 2026-02-19

## Evidence

- timestamp: 2026-02-19
  checked: NFSv4 WRITE handler (write.go)
  found: WRITE response contains only count/committed/verifier, no attributes. Client must use separate GETATTR to see mode changes.
  implication: Client relies on seeing cleared SUID bits via GETATTR, but the clearing mechanism is actually in file_remove_privs() SETATTR that happens BEFORE the WRITE.

- timestamp: 2026-02-19
  checked: Linux kernel NFS client file_remove_privs() behavior
  found: Linux NFS client sends SETATTR(mode = current_mode & ~06000) before WRITE when file has SUID/SGID bits. This is the client-side implementation of POSIX SUID clearing for NFS.
  implication: The SETATTR is the primary mechanism. If it fails, the client sees the WRITE succeed but SUID bits remain.

- timestamp: 2026-02-19
  checked: SetFileAttributes permission logic (file.go lines 382-423)
  found: Permission check treats ALL mode changes as ownership-required operations. Non-owner (uid 65534) writing to root-owned file gets EPERM on the SETATTR(mode clearing). The writePermSufficient path only covered timestamp-now and size changes, not SUID/SGID clearing.
  implication: ROOT CAUSE - Server rejects the file_remove_privs() SETATTR with EPERM because it doesn't recognize SUID/SGID clearing as a write-permission-sufficient operation.

- timestamp: 2026-02-19
  checked: Previous fix (commit c988366)
  found: Previous fix addressed server-side SUID clearing in deferredCommitWrite, but the Linux NFS client's file_remove_privs() sends SETATTR BEFORE the WRITE even reaches the server. The SETATTR rejection (EPERM) causes the client to see unchanged mode bits.
  implication: The previous fix was addressing the wrong code path. The client-initiated SETATTR is the mechanism that needs to succeed.

- timestamp: 2026-02-19
  checked: Build and test results after fix
  found: go build ./... succeeds. go test ./pkg/metadata/... all pass. No regressions.
  implication: Fix is safe and correct from unit test perspective.

## Resolution

root_cause: Linux NFS client's file_remove_privs() sends SETATTR(mode = current & ~06000) before WRITE to clear SUID/SGID bits on files with those bits set. The server's SetFileAttributes rejected this SETATTR with EPERM because the permission check treated ALL mode changes as requiring file ownership. Non-owner uid 65534 writing to root-owned file could not clear SUID/SGID via SETATTR, so the bits remained set after WRITE. The previous fix (c988366) addressed server-side clearing in deferredCommitWrite, but the client never reaches WRITE because the pre-WRITE SETATTR fails first.

fix: Added `onlyClearingSuidSgid` detection in SetFileAttributes (pkg/metadata/file.go). When a SETATTR only changes mode to clear SUID/SGID bits (new_mode == old_mode & ~0o6000 AND file currently has SUID/SGID bits), this is treated as a write-permission-sufficient operation, allowing non-owners with write permission to make this specific mode change. This aligns with the POSIX requirement that writing to a SUID/SGID file clears those bits.

verification: Build passes, all metadata unit tests pass. Full POSIX test verification requires CI run.

files_changed:
  - pkg/metadata/file.go
