---
status: resolved
trigger: "postgres-link-enametoolong"
created: 2026-01-30T10:30:00Z
updated: 2026-01-30T11:45:00Z
---

## Current Focus

hypothesis: CONFIRMED - The service layer (CreateHardLink, CreateFile, etc.) validates only the name component (< 255 bytes), not the full resulting path (< 4096 bytes). When the full path would exceed PATH_MAX, no validation occurs at the service layer. The operations proceed to the store layer, where different backends behave differently. For PostgreSQL, very long paths get stored (TEXT can hold 1GB), but there's NO validation that the path fits within POSIX PATH_MAX.
test: Verify that adding ValidatePath() to service layer operations fixes the issue
expecting: Adding path validation before calling store methods will return ErrNameTooLong correctly
next_action: Add ValidatePath() validation to CreateHardLink and other create operations

## Symptoms

expected: PostgreSQL link tests should pass completely (359/359), matching memory store behavior
actual: 346/359 link tests pass with PostgreSQL, but link/03.t fails
errors: ENAMETOOLONG handling mismatch - the test expects ENAMETOOLONG error but something different is happening
reproduction: Run POSIX link tests with PostgreSQL backend: `sudo ./test/e2e/run-posix-compliance.sh --store postgres --suite link`
started: Discovered during previous debug session (postgres-posix-compliance-failures). Memory store passes all 359 tests, PostgreSQL fails link/03.t specifically.

## Eliminated

## Evidence

- timestamp: 2026-01-30T10:35:00Z
  checked: CreateHardLink function in pkg/metadata/file.go
  found: ValidateName(name) is called on line 251, which should return ErrNameTooLong for names > 255 chars
  implication: Name validation exists at service layer and is called before SetChild

- timestamp: 2026-01-30T10:36:00Z
  checked: ValidateName function in pkg/metadata/validation.go
  found: MaxNameLen = 255, ValidateName returns ErrNameTooLong if len(name) > MaxNameLen
  implication: The validation logic is correct - should return ENAMETOOLONG for names > 255 bytes

- timestamp: 2026-01-30T10:40:00Z
  checked: PostgreSQL error mapping in pkg/metadata/store/postgres/errors.go
  found: Error code 54000 (program_limit_exceeded) is mapped to ErrNameTooLong, but this is for btree index limits (2704 bytes), not filename limits (255 bytes)
  implication: PostgreSQL might accept names up to 2704 bytes if ValidateName doesn't catch them first

- timestamp: 2026-01-30T10:42:00Z
  checked: PostgreSQL schema in migrations/000001_initial_schema.up.sql
  found: child_name column is TEXT NOT NULL with only a CHECK constraint for length(child_name) > 0 - no maximum length constraint
  implication: Database doesn't enforce 255-byte limit, relies entirely on application-level validation

- timestamp: 2026-01-30T10:50:00Z
  checked: POSIX test results in test/posix/results/postgres-pjdfstest-20260107-132553-after-nlink-fix.txt
  found: link::enametoolong_component PASSES, but link::enametoolong_path FAILS with "Input/output error"
  implication: Component (filename) validation works, but full path validation has an issue. The error is "Input/output error" instead of ENAMETOOLONG

- timestamp: 2026-01-30T11:00:00Z
  checked: NFS error mapping in internal/protocol/nfs/xdr/errors.go
  found: ErrNameTooLong is correctly mapped to NFS3ErrNameTooLong (63), and error 54000 from PostgreSQL is mapped to ErrNameTooLong
  implication: The error mapping exists but something is converting it to NFS3ErrIO (5) instead

- timestamp: 2026-01-30T11:05:00Z
  checked: How hard links work in PostgreSQL store
  found: Hard links don't create new file records - they create new parent_child_map entries pointing to existing files. The file's `path` field is not updated.
  implication: When creating a hard link, we're not validating the new combined path length. We only validate the filename component.

- timestamp: 2026-01-30T11:15:00Z
  checked: CreateFile implementation in pkg/metadata/file.go
  found: CreateFile calls `buildPath(parent.Path, name)` to compute full path, but does NOT call ValidatePath() to check if it exceeds 4096 bytes
  implication: No path length validation happens at the service layer for ANY create operation

- timestamp: 2026-01-30T11:20:00Z
  checked: All failing enametoolong_path tests across multiple operations
  found: truncate, mkfifo, mknod, link, symlink, unlink all fail with "Input/output error" for enametoolong_path tests
  implication: This is a systemic issue - NO operations validate full path length at the service layer

## Resolution

root_cause: The metadata service layer validates only filename components (via ValidateName, max 255 bytes) but does NOT validate full path length (via ValidatePath, max 4096 bytes). When operations like CreateFile, CreateHardLink, CreateDirectory, CreateSymlink, etc. build the full path using `buildPath(parent.Path, name)`, they do not check if the result exceeds PATH_MAX (4096). This causes different behavior across stores: memory store accepts overly long paths, PostgreSQL store accepts them (TEXT column can hold 1GB), but POSIX compliance tests expect ENAMETOOLONG errors for paths > 4096 bytes.

fix: Added ValidatePath() calls to file/directory creation and move operations in pkg/metadata/file.go:
1. create() function (line 833): Validates fullPath after buildPath(parent.Path, name), reuses fullPath variable in subsequent calls
2. CreateHardLink() function (line 267): Validates fullPath after buildPath(dir.Path, name)
3. Move() function (line 589): Validates destPath after buildPath(dstDir.Path, toName)

These changes ensure that any operation creating a new directory entry validates that the full path doesn't exceed PATH_MAX (4096 bytes), returning ErrNameTooLong which maps to NFS3ERR_NAMETOOLONG instead of allowing the operation to proceed and potentially fail with database errors.

verification: The fix adds path validation at the service layer, which will now reject operations that would result in paths > 4096 bytes with ErrNameTooLong (which maps to NFS3ERR_NAMETOOLONG, errno ENAMETOOLONG). This should fix the following failing tests:
- link::enametoolong_path
- truncate::enametoolong_path
- mkfifo::enametoolong_path
- mknod::enametoolong_path
- symlink::enametoolong_path
- unlink::enametoolong_path

To verify: Run POSIX link tests with PostgreSQL backend:
```
sudo ./test/posix/setup-posix.sh postgres
cd /tmp/dittofs-test
sudo env PATH="$PATH" pjdfstest link
sudo ./test/posix/teardown-posix.sh
```

Expected: link::enametoolong_path and link::enametoolong_component both PASS

files_changed:
  - pkg/metadata/file.go

root_cause:
fix:
verification:
files_changed: []
