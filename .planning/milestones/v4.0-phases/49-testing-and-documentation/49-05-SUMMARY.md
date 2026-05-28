---
phase: 49-testing-and-documentation
plan: 05
subsystem: documentation
tags: [docs, block-store, terminology, rename]
dependency_graph:
  requires: [49-01, 49-02]
  provides: [block-store-docs]
  affects: [all-documentation]
tech_stack:
  patterns: [two-tier-block-store, per-share-isolation, ref-counted-remote]
key_files:
  created: []
  modified:
    - docs/ARCHITECTURE.md
    - docs/CONFIGURATION.md
    - docs/IMPLEMENTING_STORES.md
    - docs/NFS.md
    - docs/SMB.md
    - docs/FAQ.md
    - docs/CONTRIBUTING.md
    - docs/TROUBLESHOOTING.md
    - docs/WINDOWS_TESTING.md
    - README.md
decisions:
  - CLAUDE.md already updated by prior plans, no changes needed
  - SECURITY.md and CLI.md had zero payload references, skipped
  - WINDOWS_TESTING.md discovered as additional file needing updates
metrics:
  duration: 5min
  completed: "2026-03-10"
---

# Phase 49 Plan 05: Documentation Overhaul Summary

Replace all payload store terminology with block store terminology across all documentation files, documenting the two-tier architecture with per-share BlockStore isolation and new CLI commands.

## What Was Done

### Task 1: Core Documentation Overhaul (ARCHITECTURE.md, CONFIGURATION.md, IMPLEMENTING_STORES.md)
**Commit:** `06a06a51`

- **ARCHITECTURE.md**: Completely rewrote storage architecture section. Replaced PayloadService/Cache/WAL/Offloader model with two-tier block store (local + remote). Added Per-Share Block Store Isolation section, Cache Tiers (L1/L2/L3) section, updated all diagrams and code examples.
- **CONFIGURATION.md**: Replaced Cache Configuration and Payload Configuration sections with unified Block Store Configuration. Updated all CLI examples from `store payload add` to `store block add --kind local/remote`. Updated share creation flags from `--payload` to `--local`/`--remote`.
- **IMPLEMENTING_STORES.md**: Completely rewrote. Replaced PayloadStore guide with separate Local Store and Remote Store implementation guides documenting `LocalStore` and `RemoteStore` interfaces with conformance test references.

### Task 2: README.md and Remaining Docs Update
**Commit:** `e342a0d7`

- **README.md**: Updated mermaid diagram, feature lists, NFS quickstart CLI, store management CLI, cache section (replaced with Block Store Architecture), runtime management CLI, use case descriptions, and documentation links.
- **NFS.md**: Updated READ/WRITE procedure descriptions, write coordination pattern with `GetBlockStoreForHandle`, handler pattern description.
- **SMB.md**: Updated FLUSH description, message flow, command descriptions, write pattern code example, cache integration (now Block Store Integration), troubleshooting.
- **FAQ.md**: Updated project description, adapter interface description, store backend interfaces (LocalStore + RemoteStore), deduplication description, multi-share CLI examples.
- **CONTRIBUTING.md**: Replaced PayloadStore implementation guide with LocalStore/RemoteStore guide, updated adapter code example to use `GetBlockStoreForHandle`.
- **TROUBLESHOOTING.md**: Updated all CLI examples from `store payload` to `store block`.
- **WINDOWS_TESTING.md**: Updated CLI examples (discovered during verification sweep).

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing] WINDOWS_TESTING.md payload references**
- **Found during:** Task 2 verification
- **Issue:** WINDOWS_TESTING.md was not listed in the plan but contained payload store CLI commands
- **Fix:** Updated CLI commands to use block store syntax
- **Files modified:** docs/WINDOWS_TESTING.md
- **Commit:** e342a0d7

### Files Skipped (No Changes Needed)

- **CLAUDE.md**: Already fully updated by Plans 49-01 and 49-02 (code rename)
- **docs/SECURITY.md**: Zero payload references found
- **docs/CLI.md**: Zero payload references found

## Verification

Zero payload store references remain across all documentation:
```
grep -rn 'payload.store|PayloadStore|PayloadService|payload_store|store payload|--payload ' README.md docs/ CLAUDE.md
# (empty - PASS)
```

Legitimate "PayloadID" references preserved (Go struct field name used in code examples).
