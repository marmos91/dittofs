---
status: resolved
trigger: "Admin user cannot access shares by default - gets permission denied when writing to mounted NFS share"
created: 2026-02-08T10:00:00Z
updated: 2026-02-08T10:20:00Z
---

## Current Focus

hypothesis: CONFIRMED - The fix correctly sets ShareWritable ONLY for admin users
test: All unit tests pass, fix committed
expecting: Admin users can access shares, regular users still respect Unix permissions
next_action: PR created and ready for review

## Symptoms

expected: Admin users should have read/write permissions on all shares by default. They should be able to access and manage all shares without explicit permission grants.
actual: When an admin user mounts a share via NFS and tries to write a file (e.g., `echo "test" > /mnt/share/test.txt`), they get "permission denied".
errors: Permission denied when attempting write operations on NFS-mounted shares as admin user.
reproduction:
1. Start an empty dittofs instance
2. Create a share through the controlplane API (any store is ok)
3. Mount the share using the admin user credentials
4. Run `echo "test" > /mnt/share/test.txt`
5. Permission denied error occurs
started: Related to recent revert of ShareWritable permission bypass

## Eliminated

- hypothesis: ShareWritable should be set for read-write users
  evidence: This was the original fix that broke 19 POSIX tests - bypassing Unix permissions for regular users violates POSIX semantics
  timestamp: 2026-02-08T10:08:00Z

## Evidence

- timestamp: 2026-02-08T10:02:00Z
  checked: auth_helper.go BuildAuthContextWithMapping
  found: permResult.permission contains admin permission from ResolveSharePermission (when UID lookup finds admin user), but this is only used to set ShareReadOnly flag. The permission level itself is not passed to the metadata layer.
  implication: The metadata layer has no way to know the user is an admin - only has UID/GID

- timestamp: 2026-02-08T10:03:00Z
  checked: permissions.go ResolveSharePermission
  found: Correctly identifies admin users (IsAdmin() or HasGroup("admins")) and returns PermissionAdmin
  implication: Admin detection works at controlplane level

- timestamp: 2026-02-08T10:04:00Z
  checked: authentication.go calculatePermissions
  found: Only has UID=0 (root) bypass. No concept of "DittoFS admin" - only checks Unix identity
  implication: Admin users who don't connect as root (UID=0) get no special treatment

- timestamp: 2026-02-08T10:05:00Z
  checked: Commits ae9f9d8 and e73b458
  found: ShareWritable was added then reverted. Setting ShareWritable=true for admin/read-write users bypassed ALL Unix permissions, breaking POSIX tests
  implication: ShareWritable approach is too broad - bypasses permissions for ALL operations, not just for admin users

- timestamp: 2026-02-08T10:10:00Z
  checked: Original fix in ae9f9d8 - auth_helper.go line 104
  found: `ShareWritable: permResult.permission == models.PermissionReadWrite || permResult.permission == models.PermissionAdmin`
  implication: Fix was setting ShareWritable for BOTH read-write AND admin users. The issue is read-write users - they should NOT bypass Unix permissions. Only admin users should.

## Resolution

root_cause: The original fix (ae9f9d8) set ShareWritable=true for BOTH read-write and admin users. This caused regular read-write users to bypass Unix file permissions, breaking POSIX compliance. When this was reverted (e73b458), it removed admin bypass as well, leaving admin users without access.

fix: Set ShareWritable=true ONLY for admin users (models.PermissionAdmin), not for regular read-write users. Admin users now bypass Unix permissions (like root does), while regular users still respect file permissions.

Changes made:
1. auth_helper.go: Set ShareWritable only for admin users: `ShareWritable: permResult.permission == models.PermissionAdmin`
2. authentication.go: Simplified admin bypass to grant all requested permissions (like root), not just write permissions

verification: All unit tests pass (go test ./...)
files_changed:
- internal/protocol/nfs/v3/handlers/auth_helper.go
- pkg/metadata/authentication.go

commit: 6c6a1cb
