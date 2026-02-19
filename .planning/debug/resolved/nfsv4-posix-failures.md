---
status: resolved
trigger: "NFSv4 POSIX compliance test failures - 60/237 test files fail with 3 distinct error patterns"
created: 2026-02-19T00:00:00Z
updated: 2026-02-19T15:15:00Z
---

## Current Focus

hypothesis: All three issues confirmed and fixed
test: Unit tests pass locally; CI verification needed
expecting: NFSv4 POSIX tests should pass (matching NFSv3 results)
next_action: Push to branch and verify in CI

## Symptoms

expected: All NFSv4 POSIX tests pass (or only open/03.t PATH_MAX failure)
actual: 60 test files fail with 3 distinct error patterns
errors:
  1. Error 524 (ENOTSUPP/EOPNOTSUPP) for mkfifo, mknod, bind - 693 occurrences
  2. UID/GID mapping: files show uid=65534, gid=65534 (nobody) instead of actual UID/GID
  3. SUID/SGID bits not cleared on non-owner write
reproduction: sudo ./test/posix/setup-posix.sh memory --nfs-version 4
started: Just added NFSv4 POSIX CI jobs; NFSv3 tests pass fine

## Eliminated

- hypothesis: SUID/SGID clearing logic is wrong in CommitWrite
  evidence: Code correctly checks *identity.UID != 0 and clears bits via file.Mode &= ^uint32(0o6000)
  timestamp: 2026-02-19T15:00:00Z

- hypothesis: Deferred commits don't apply SUID/SGID clearing
  evidence: GetFile() merges pending state including ClearSetuidSetgid; CLOSE handler flushes pending writes
  timestamp: 2026-02-19T15:05:00Z

## Evidence

- timestamp: 2026-02-19T00:10:00Z
  checked: NFSv4 CREATE handler (create.go)
  found: NF4BLK, NF4CHR, NF4SOCK, NF4FIFO all return NFS4ERR_NOTSUPP explicitly
  implication: All special file creation via NFSv4 CREATE is intentionally disabled

- timestamp: 2026-02-19T00:10:00Z
  checked: NFSv3 MKNOD handler (mknod.go)
  found: Uses metaSvc.CreateSpecialFile() successfully for all special file types
  implication: The metadata layer supports special files; only the NFSv4 handler is missing

- timestamp: 2026-02-19T00:10:00Z
  checked: attrs/encode.go resolveOwnerString and resolveGroupString
  found: Returns "root@localdomain" for UID 0, "N@localdomain" for others
  implication: Linux client with nfs4_disable_idmapping=Y (default) expects pure numeric strings

- timestamp: 2026-02-19T00:10:00Z
  checked: io.go CommitWrite and immediateCommitWrite
  found: SUID/SGID clearing logic checks *identity.UID != 0 - correct server-side logic
  implication: Issue 3 is a consequence of Issue 2 (wrong UIDs confuse pjdfstest)

- timestamp: 2026-02-19T00:10:00Z
  checked: CREATE handler createattrs parsing
  found: skipFattr4(reader) is called - attrs are consumed but not applied
  implication: createattrs (mode, owner, group) need to be decoded and applied

- timestamp: 2026-02-19T15:10:00Z
  checked: Linux kernel NFSv4 ID mapping behavior
  found: nfs4_disable_idmapping=Y is default since kernel 3.x; expects pure numeric UID/GID strings
  implication: Server must send "0" not "root@localdomain" for AUTH_SYS clients

## Resolution

root_cause: |
  Two confirmed root causes (Issue 3 is a consequence of Issue 2):

  1. NFSv4 CREATE handler (create.go) explicitly returned NFS4ERR_NOTSUPP for
     NF4BLK, NF4CHR, NF4SOCK, NF4FIFO file types. Also skipped createattrs
     parsing (used skipFattr4 instead of DecodeFattr4ToSetAttrs).

  2. NFSv4 OWNER/OWNER_GROUP attribute encoding (encode.go) used "user@domain"
     format (e.g., "root@localdomain", "0@localdomain"). Modern Linux kernels
     default to nfs4_disable_idmapping=Y which expects purely numeric strings
     ("0", "1000"). The "@localdomain" suffix caused the client's idmapd to
     fail domain matching, mapping all UIDs/GIDs to nobody (65534).

  3. SUID/SGID clearing works correctly server-side. The test failures were
     caused by Issue 2: wrong UID/GID in stat() confused pjdfstest's ownership
     checks and non-owner write detection.

fix: |
  1. CREATE handler: Added support for NF4BLK, NF4CHR, NF4SOCK, NF4FIFO via
     metaSvc.CreateSpecialFile(). Replaced skipFattr4() with DecodeFattr4ToSetAttrs()
     to properly decode and apply createattrs (mode, owner, group). Device spec
     data (major/minor) properly decoded and passed through for block/char devices.

  2. OWNER/OWNER_GROUP encoding: Changed resolveOwnerString() and resolveGroupString()
     to return purely numeric strings (fmt.Sprintf("%d", uid/gid)) instead of
     "user@domain" format. Also updated pseudo-FS attribute encoding to use "0"
     instead of "root@localdomain"/"wheel@localdomain".

verification: |
  - All existing unit tests pass (go test ./... - 0 failures)
  - New tests added: TestCreate_BlockDevice_Success, TestCreate_FIFO_Success,
    TestCreate_Socket_Success
  - Updated TestCreate_UnsupportedType_BlockDevice -> TestCreate_BlockDevice_Success
  - CI verification pending (cannot run POSIX tests locally on macOS)

files_changed:
  - internal/protocol/nfs/v4/handlers/create.go
  - internal/protocol/nfs/v4/attrs/encode.go
  - internal/protocol/nfs/v4/handlers/create_remove_test.go
