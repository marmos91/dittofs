# pkg/metadata/acl

NFSv4 Access Control List implementation per RFC 7530 Section 6.

## Package Purpose

Protocol-agnostic ACL types, evaluation, validation, mode synchronization, and inheritance. This package has **no dependencies** on NFS/SMB wire formats or internal protocol packages.

## Key Files

| File | Purpose |
|------|---------|
| `types.go` | ACE/ACL types, all constants (4 ACE types, 7 flags, 16 mask bits, 3 special identifiers) |
| `evaluate.go` | ACL evaluation engine (process-first-match algorithm) |
| `validate.go` | Canonical ordering validation and 128 ACE limit |
| `mode.go` | ACL to/from Unix mode bits (DeriveMode, AdjustACLForMode) |
| `inherit.go` | Inheritance computation for new files/directories |

## Evaluation Algorithm

Process-first-match per RFC 7530 Section 6.2.1:

1. Process ACEs sequentially
2. Skip INHERIT_ONLY ACEs (they only apply to children)
3. For each matching ACE: set undecided bits as allowed or denied
4. AUDIT/ALARM ACEs are store-only (skipped during evaluation)
5. Early termination when all requested bits are decided
6. Return true only if ALL requested bits are in allowedBits

## Canonical Ordering Rules

Strict Windows canonical order enforced on validation:
- Bucket 1: Explicit DENY (no INHERITED_ACE flag)
- Bucket 2: Explicit ALLOW (no INHERITED_ACE flag)
- Bucket 3: Inherited DENY (INHERITED_ACE flag set)
- Bucket 4: Inherited ALLOW (INHERITED_ACE flag set)
- AUDIT/ALARM ACEs can appear anywhere

## Critical Semantics

### nil ACL vs Empty ACL
- `nil` ACL = no ACL set, use classic Unix permission check
- `&ACL{ACEs: []}` = explicit empty ACL, denies ALL access

### OWNER@/GROUP@/EVERYONE@ Are Dynamic
These are resolved at evaluation time against the file's current owner/group. Never store resolved UIDs in place of these identifiers.

### Mode Sync (chmod)
`AdjustACLForMode` only modifies OWNER@/GROUP@/EVERYONE@ ACEs. All other ACEs (explicit user/group) are preserved unchanged.

## Common Gotchas

1. **INHERIT_ONLY skip**: Must be skipped during evaluation but inherited by children
2. **First-match wins**: Once a bit is decided (allowed or denied), later ACEs cannot change it
3. **chmod does not replace ACL**: Only adjusts special identifier ACEs
4. **Non-rwx bits preserved**: AdjustACLForMode preserves READ_ACL, WRITE_ACL, DELETE, etc.
5. **File inheritance clears ALL flags**: Inherited file ACEs have no inheritance flags (can't propagate)
