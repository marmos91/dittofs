# Phase 21: Per-Engine Backup Drivers - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-27
**Phase:** 21-per-engine-backup-drivers
**Areas discussed:** Serialization format per driver, Hash extraction strategy, Empty-store detection for Restore, Schema versioning inside payload, Code structure and design

---

## Serialization Format — Memory

| Option | Description | Selected |
|--------|-------------|----------|
| Full state dump | Gob-encode entire in-memory state: shares, files, parents, children, linkCounts, symlinkTargets, payloadIDs, deviceNumbers. Single gob.Encode under mu.RLock(). | ✓ |
| Files + tree only | Only shares, files, parent/child, link counts. Skip transient state. | |
| You decide | Let Claude pick based on conformance suite. | |

**User's choice:** Full state dump (Recommended)
**Notes:** None

## Serialization Format — Badger

| Option | Description | Selected |
|--------|-------------|----------|
| Custom KV stream | Iterate all prefixed keys inside db.View(), write length-prefixed key+value pairs. Portable, no Badger version coupling. | |
| Badger built-in Backup/Stream | Use db.Backup() or db.Stream(). Faster but couples to Badger wire format. | |
| You decide | Let Claude pick based on portability vs. performance. | ✓ |

**User's choice:** You decide
**Notes:** Claude recommends custom KV stream for portability.

## Serialization Format — Postgres

| Option | Description | Selected |
|--------|-------------|----------|
| COPY all metadata tables as CSV | COPY TO STDOUT per table inside REPEATABLE READ txn. Table name + row count header per section. Restore via COPY FROM STDIN in dependency order. | ✓ |
| Row-at-a-time JSON | SELECT as JSON, write JSON-lines per table. Simpler but slower. | |
| You decide | Let Claude pick based on pgx capabilities. | |

**User's choice:** COPY all metadata tables as CSV (Recommended)
**Notes:** None

## Hash Extraction Strategy

| Option | Description | Selected |
|--------|-------------|----------|
| Inline during serialization | Extract BlockRef hashes as each file/row is serialized, Add() to HashSet. Single pass inside atomic snapshot. | ✓ |
| Separate pass after serialization | Serialize all data first, then second read pass for hashes. Doubles work inside snapshot window. | |
| You decide | Let Claude pick based on snapshot window concerns. | |

**User's choice:** Inline during serialization (Recommended)
**Notes:** None

### Postgres Hash Extraction Detail

| Option | Description | Selected |
|--------|-------------|----------|
| Separate COPY on file_block_refs | COPY (SELECT DISTINCT hash FROM file_block_refs) inside same REPEATABLE READ txn. Clean single-purpose query. | ✓ |
| JOIN during files COPY | Join files with file_block_refs. Fewer round-trips but mixes concerns. | |
| You decide | Let Claude pick based on pgx COPY ergonomics. | |

**User's choice:** Separate COPY on file_block_refs (Recommended)
**Notes:** None

## Empty-Store Detection for Restore

| Option | Description | Selected |
|--------|-------------|----------|
| Shares count > 0 | All drivers check if any shares exist. Memory: len(shares)>0. Badger: seek s: prefix. Postgres: SELECT EXISTS. Simple, fast, uniform. | ✓ |
| Per-engine deepest check | Memory: len(files)>0. Badger: any key. Postgres: EXISTS across tables. More thorough but unnecessary. | |
| You decide | Let Claude pick simplest correct check. | |

**User's choice:** Shares count > 0 (Recommended)
**Notes:** None

## Schema Versioning Inside Payload

| Option | Description | Selected |
|--------|-------------|----------|
| uint32 at payload start | First 4 bytes LE uint32, start at 1. Each driver manages independently. | ✓ |
| JSON header object | First line JSON {"version": 1}. More self-documenting but adds parsing overhead. | |
| You decide | Let Claude pick based on envelope design consistency. | |

**User's choice:** uint32 at payload start (Recommended)
**Notes:** None

## Code Structure and Design

| Option | Description | Selected |
|--------|-------------|----------|
| Self-contained per driver | Each backup.go independent. Import envelope + HashSet, no shared driver helpers. | ✓ |
| Shared backup helpers subpackage | Common patterns in pkg/metadata/backup/. Reduces duplication but adds coupling. | |
| You decide | Let Claude pick based on DRY vs. coupling. | |

**User's choice:** Self-contained per driver (Recommended)
**Notes:** None

### PR Shape

| Option | Description | Selected |
|--------|-------------|----------|
| Single PR, staged commits | One PR, 4 commits: memory, badger, postgres, conformance wiring. Each builds independently. | ✓ |
| Three separate PRs | One per driver. Independent review/merge. More overhead. | |
| You decide | Let Claude pick based on complexity. | |

**User's choice:** Single PR, staged commits (Recommended)
**Notes:** None

---

## Claude's Discretion

- D-02: Badger serialization format — user deferred to Claude. Recommendation: custom KV stream for portability.

## Deferred Ideas

None — discussion stayed within phase scope.
