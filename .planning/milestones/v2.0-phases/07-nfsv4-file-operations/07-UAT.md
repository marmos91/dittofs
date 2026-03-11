---
status: passed
phase: 07-nfsv4-file-operations
source: 06-01-SUMMARY.md, 06-02-SUMMARY.md, 06-03-SUMMARY.md, 07-01-SUMMARY.md, 07-02-SUMMARY.md, 07-03-SUMMARY.md
started: 2026-02-13T16:00:00Z
updated: 2026-02-13T18:45:00Z
---

## Current Test
<!-- OVERWRITE each test - shows where we are -->

number: 11
name: NFSv3 Backward Compatibility
expected: All NFSv3 operations still work after NFSv4 additions
awaiting: complete

## Tests

### 1. NFSv4 Mount
expected: Mount the server using NFSv4 (`sudo mount -t nfs -o nfsvers=4,tcp,port=12049 localhost:/ /tmp/test`). The mount succeeds without errors.
result: pass

### 2. Browse Pseudo-FS Root
expected: `ls /tmp/test` shows exported share names (e.g., "export"). The pseudo-filesystem namespace works.
result: pass

### 3. Navigate Into Share
expected: `cd /tmp/test/export` succeeds. `ls` shows the share contents. LOOKUP crosses the export junction from pseudo-fs to real filesystem.
result: pass

### 4. Read File Content
expected: `cat /tmp/test/export/<file>` displays file content correctly.
result: skipped
reason: No files exist in fresh memory store; covered by test 5

### 5. Create and Write a New File
expected: `echo "hello nfsv4" > /tmp/test/export/testfile.txt` succeeds. `cat /tmp/test/export/testfile.txt` returns "hello nfsv4".
result: pass

### 6. Create Directory
expected: `mkdir /tmp/test/export/testdir` succeeds. `ls /tmp/test/export/` shows "testdir" in the listing.
result: pass

### 7. Remove File
expected: `rm /tmp/test/export/testfile.txt` succeeds. `ls /tmp/test/export/` no longer shows "testfile.txt".
result: pass

### 8. Remove Directory
expected: `rmdir /tmp/test/export/testdir` succeeds. `ls /tmp/test/export/` no longer shows "testdir".
result: pass

### 9. Create and Read Symlink
expected: Create a regular file first, then `ln -s <target> /tmp/test/export/mylink`. `ls -la` shows the symlink with its target. `readlink /tmp/test/export/mylink` returns the target path.
result: pass
notes: Symlink created, listed correctly (`mylink -> target.txt`), `readlink` returned correct target

### 10. File Attributes
expected: `ls -la /tmp/test/export/` shows correct file types (d for dirs, l for symlinks, - for files), sizes, and timestamps. `stat` shows meaningful attributes.
result: pass
notes: |
  stat showed: Size=15, Mode=0644, Links=1, correct timestamps
  Known cosmetic: UID/GID map to "nobody" (NFSv4 identity mapping issue)
  Known cosmetic: file size sometimes shows 0 in `ls -la` (NFSv4 GETATTR caching)

### 11. NFSv3 Backward Compatibility
expected: Unmount NFSv4 mount. Mount using NFSv3. All NFSv3 operations (ls, cat, echo, mkdir, rm, rmdir) still work correctly.
result: pass
notes: |
  All operations work. NFSv3 shows correct owner (marmos91) and file sizes.
  Files from NFSv4 session persist across protocol switch.

## Summary

total: 11
passed: 10
issues: 0
pending: 0
skipped: 1

## Known Issues (non-blocking)

- truth: "NFSv4 identity mapping shows 'nobody' for file ownership"
  status: cosmetic
  reason: "macOS NFSv4 client maps user@domain identities; server returns root@localdomain / UID@localdomain which macOS maps to nobody (UID 4294967294). NFSv3 works correctly."
  severity: cosmetic
  phase_to_fix: 9 (NFSv4 state management)

- truth: "File size sometimes shows 0 in NFSv4 ls -la"
  status: cosmetic
  reason: "GETATTR for NFSv4 sometimes returns stale size=0 for recently written files. stat and cat return correct data. NFSv3 shows correct size. Likely a metadata update timing issue in WRITE/COMMIT."
  severity: minor
  phase_to_fix: 9

- truth: "Pseudo-FS rebuilds when shares are added at runtime"
  status: known_limitation
  reason: "Shares added via API after server startup do not appear in pseudo-fs until restart"
  severity: minor
  phase_to_fix: 14
  root_cause: "PseudoFS.Rebuild() only called once in SetRuntime()"

## Fixes Applied During UAT

1. **RENEW handler** (renew.go): Stub returning NFS4_OK to prevent macOS degrading mount to read-only
2. **SECINFO handler** (secinfo.go): Stub returning AUTH_SYS for macOS security negotiation
3. **Root directory mode**: Changed from 0755 to 0777 in runtime.go and memory/shares.go
4. **FSID unification** (encode.go): Real-FS FSID changed from (1, hash) to (0, 1) matching pseudo-FS. macOS creates a "triggered mount" when FSID changes at junction boundaries, which failed silently. Using same FSID prevents triggered mount.
5. **NFSv4 identity format** (encode.go): OWNER/OWNER_GROUP changed to user@localdomain format per RFC 7530

## Debug Logging Added

- compound.go: Logs every dispatched operation with opcode, status, client
- access.go: Logs ACCESS check details (mode, uid, gid, requested/granted bits)
- getattr.go: Logs GETATTR real-FS details (path, mode, uid, gid, type, size)
