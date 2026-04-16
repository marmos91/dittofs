# Phase 2: Per-Engine Backup Drivers - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in 02-CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-04-16
**Phase:** 02-per-engine-backup-drivers
**Areas discussed:** Backup scope, PayloadIDSet computation, Per-engine serialization format, Restore preconditions, Error taxonomy, Testing coverage

---

## Gray Area Selection

**Prompt:** Which gray areas do you want to discuss for Phase 2?

| Option | Description | Selected |
|--------|-------------|----------|
| Backup scope per engine | Files only vs full schema (incl. locks/clients/durable handles) | ✓ |
| PayloadIDSet computation | Same-snapshot vs stream-tap vs post-scan | ✓ |
| Per-engine serialization format | PG (tar/concat/framed), Memory (gob/JSON), Badger native | ✓ |
| Restore preconditions & wipe | Require empty vs wipe-then-restore vs merge | ✓ |

---

## Backup Scope

| Option | Description | Selected |
|--------|-------------|----------|
| Full DB / all tables | Native engine APIs verbatim; stale session state treated as post-crash by clients | ✓ |
| Core metadata only | Exclude locks/clients/durable handles; custom iteration required | |
| Core + locks, exclude clients/DH | Half-measure; value marginal | |

**User's choice:** Full DB / all tables
**Notes:** Aligns with "reliable and safe" — no custom exclusion logic that could drop data.

---

## PayloadIDSet Computation

| Option | Description | Selected |
|--------|-------------|----------|
| Second pass inside same snapshot | Same MVCC/RLock for both payload and PayloadIDSet | ✓ |
| Stream decoder (tap backup stream) | Decode payload to extract IDs; brittle for Badger's opaque framing | |
| Post-backup scan (outside snapshot) | Simplest code but UNSAFE — race window = data loss on concurrent deletes | |

**User's choice:** Second pass inside same snapshot
**Notes:** User flagged zero-downtime concern for concurrent uploads. Walked through worst-case: a file deleted mid-backup, if payload-ref scan is post-backup, its PayloadID would be missed → block-GC could free a block referenced by the manifest → restore fails with IO error. Same-snapshot scan closes the window. No option is unsafe for WRITES (all three are zero-downtime); only post-backup scan is unsafe for the PayloadIDSet correctness invariant.

---

## Badger: DB.Backup vs Custom Stream (follow-up)

| Option | Description | Selected |
|--------|-------------|----------|
| Custom stream in single db.View | Preserves SSI primitive, no race window, bypasses DB.Backup wrapper | ✓ |
| Use DB.Backup + accept over-hold race | Faster to code; widens data-loss window under concurrent deletes | |
| Use DB.Backup + sync.Mutex full-backup lock | Violates zero-downtime requirement | |

**User's choice:** Custom stream in single db.View
**Notes:** REQ ENG-01's intent (SSI snapshot, safe under concurrent writes) preserved; the literal DB.Backup call is not.

---

## Per-Engine Serialization Format

### Postgres

| Option | Description | Selected |
|--------|-------------|----------|
| Framed tar of per-table COPYs with sidecar manifest | Clear table boundaries, tolerates schema evolution, standard tooling compatible | ✓ |
| Concatenated binary COPYs with length prefixes | Smaller but custom decoder | |
| Single pg_dump -Fc file | Defeats pgx-native requirement, adds runtime binary dep | |

**User's choice:** Framed tar + sidecar manifest

### Memory

| Option | Description | Selected |
|--------|-------------|----------|
| encoding/gob | Go-native, fast, handles typed structs + maps directly | ✓ |
| JSON | Human-inspectable but larger/slower, needs explicit type tags | |
| Framed tar matching PG | Pointless for in-process memory store | |

**User's choice:** encoding/gob

---

## Restore Preconditions & Wipe

### First pass

| Option | Description | Selected |
|--------|-------------|----------|
| Require empty destination | Safest; caller (Phase 5) must prepare fresh store | (user asked for guidance) |
| Wipe-then-restore | Simpler caller; destructive — industry's #1 DR failure mode | |
| Merge/upsert | Ill-defined for full snapshots; rejected | |

**User's notes:** "Help me choose here, considering that this feature will be used by enterprises/companies to backup their edge nas"

Reasoning walked through: Phase 2 is plumbing below Phase 5 orchestrator. Phase 5 owns share-disable / quiesce / swap-under-temp-path per REST-01/02/03. At Phase 2 layer, "require empty" gives an unambiguous contract, preserves defense in depth, matches pg_restore / etcdctl / restic precedent.

### Confirmation

| Option | Description | Selected |
|--------|-------------|----------|
| Require empty destination | Phase 2 errors on non-empty; Phase 5 handles destruction explicitly | ✓ |
| Wipe-then-restore | Destruction hidden inside engine = bug/test = silent data loss | |

**User's choice:** Require empty destination
**Notes:** User reinforced after the choice: "WE need something reliable and safe" — confirming all downstream decisions lean this direction.

---

## Error Taxonomy

| Option | Description | Selected |
|--------|-------------|----------|
| Fail-fast + typed sentinels | Abort on any error, typed errors for Phase 7 chaos tests, no silent recovery | ✓ |
| Best-effort with partial-success reporting | Encourages silent corruption tolerance; rejected for DR subsystem | |

**User's choice:** Fail-fast + typed sentinels

---

## Testing Coverage

| Option | Description | Selected |
|--------|-------------|----------|
| Round-trip + concurrent-writes + corruption | Shared storetest/backup_conformance.go covering all 5 safety invariants | ✓ |
| Round-trip only | Defers safety tests to Phase 7 where they typically slip | |

**User's choice:** Round-trip + concurrent-writes + corruption

---

## Claude's Discretion

- Exact gob root-struct shape for Memory store
- Badger stream framing (length-prefixed vs tar)
- Order of tables within the PG tar (must be deterministic)
- Whether to add progress callback signature or skip

## Deferred Ideas

- Incremental backups (INCR-01)
- Cross-engine restore (XENG-01)
- Progress reporting per backup (possibly Phase 6)
- Encryption at backup-producer layer (owned by Phase 3)
- External KMS (post-v0.13.0)
- Block payload data backup (out of milestone scope)
