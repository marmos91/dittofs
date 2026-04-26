---
phase: 11-cas-write-path-gc-rewrite-a2
plan: 09
subsystem: docs
tags: [docs, gc, cas, syncer, blockstore, dual-read]
requires: [11-CONTEXT.md D-01..D-35, ROADMAP §Phase 11, REQUIREMENTS §BSCAS/LSL/GC/STATE/INV/TD]
provides:
  - mark-sweep + three-state lifecycle + dual-read window sections in ARCHITECTURE.md
  - FAQ entries for v0.15.0 GC, dual-read window, residual legacy keys
  - MetadataStore.EnumerateFileBlocks + RemoteStore.WriteBlockWithHash + ReadBlockVerified contracts in IMPLEMENTING_STORES.md
  - syncer.* and gc.* knob docs in CONFIGURATION.md
  - dfsctl store block gc + gc-status reference in CLI.md
  - EnumerateFileBlocks bullet in CONTRIBUTING.md "Adding a New Store Backend" recipe
affects:
  - operator tuning workflow (new knobs documented)
  - new metadata backend implementers (cursor contract documented)
  - new remote backend implementers (CAS contract documented)
tech-stack:
  added: []
  patterns:
    - cross-doc cross-references between ARCHITECTURE/CONFIGURATION/CLI/FAQ
    - YAML knob blocks with default/range/meaning per knob
    - per-contract semantics list + conformance scenarios block
key-files:
  created: []
  modified:
    - docs/ARCHITECTURE.md
    - docs/FAQ.md
    - docs/IMPLEMENTING_STORES.md
    - docs/CONFIGURATION.md
    - docs/CLI.md
    - docs/CONTRIBUTING.md
decisions:
  - Updated CONTRIBUTING.md (yes) — existing "Adding a New Store Backend" recipe already had a Metadata Store section with a numbered list; one-line addition pointing at EnumerateFileBlocks + IMPLEMENTING_STORES.md anchor kept the diff small per D-34 planner discretion.
  - Cross-references added: ARCHITECTURE.md → CONFIGURATION.md (knobs) + CLI.md (dfsctl gc); CONFIGURATION.md → ARCHITECTURE.md (mark-sweep design) + CLI.md (on-demand commands); CLI.md → ARCHITECTURE.md (mark-sweep design) + CONFIGURATION.md (knobs); FAQ.md → ARCHITECTURE.md (mark-sweep section anchor) + CONFIGURATION.md (knobs); CONTRIBUTING.md → IMPLEMENTING_STORES.md (EnumerateFileBlocks anchor). The five docs form a closed graph so an operator entering at any one of them can discover the others.
metrics:
  duration: ~30m
  completed: 2026-04-25T15:14:43Z
  tasks: 2
  commits: 3
  files: 6
---

# Phase 11 Plan 09: Phase 11 Documentation Summary

Updated five docs files (and one bonus per D-34 planner discretion) to land
the v0.15.0 Phase 11 documentation surface — operator-visible (GC behavior,
new config knobs, new CLI surface) and developer-visible (new conformance
contract for MetadataStore + RemoteStore backends).

## Tasks Executed

### Task 1 — `docs/ARCHITECTURE.md`

Commit: `087af807` (`docs(11-09): document Phase 11 mark-sweep GC, three-state lifecycle, dual-read window`)

- Added **Block Lifecycle (three-state, v0.15.0 Phase 11)** section: Pending → Syncing → Remote diagram, claim-batch serialization rationale (STATE-03 taken literally, single owner = `engine.Syncer`, restart-recovery janitor at `syncer.claim_timeout`).
- Added **Garbage Collection (mark-sweep, v0.15.0 Phase 11)** section: algorithm (mark via `EnumerateFileBlocks` cursor + on-disk live set under `<localStore>/gc-state/<runID>/db/`, sweep over 256 `cas/{XX}/` prefixes with snapshot+grace TTL), fail-closed posture asymmetry (D-06 vs D-07), `gc-state/` directory layout with `incomplete.flag` and `last-run.json`, periodic + on-demand triggers, slog observability.
- Added **Dual-Read Window (Phase 11 → Phase 14)** section: metadata-key-shape resolution (D-21), CAS path with BLAKE3 verification vs. legacy path with no verification, intentional deletion clock (Phase 15).
- Updated the existing "Block Store -- Hybrid Local Tier" forward reference to point at the two new sections rather than just naming them.

### Task 2 — `docs/FAQ.md`, `docs/IMPLEMENTING_STORES.md`, `docs/CONFIGURATION.md`, `docs/CLI.md`

Commit: `8b16b02a` (`docs(11-09): document Phase 11 GC, EnumerateFileBlocks, CAS contracts, knobs, dfsctl gc`)

- **FAQ.md**: appended three Q&As — "How does garbage collection work in v0.15.0?", "What is the dual-read window?", "Why are residual `{payloadID}/block-{N}` keys present after upgrading to v0.15.0?".
- **IMPLEMENTING_STORES.md**: documented `MetadataStore.EnumerateFileBlocks` cursor (signature, must-haves: backend-native cursor, ctx.Done() honored, zero-hash blocks emitted, fn-error verbatim, safe under concurrent writes; 5 conformance scenarios). Documented `RemoteStore.WriteBlockWithHash` (CAS key derivation, `content-hash` header, atomicity, idempotency, external-verifier criterion) and `RemoteStore.ReadBlockVerified` (header pre-check + streaming BLAKE3 recompute, fail-closed twice, INV-06 hard-required).
- **CONFIGURATION.md**: added "Syncer + GC knobs (v0.15.0 Phase 11)" subsection with the YAML block for `blockstore.syncer.{tick,claim_batch_size,upload_concurrency,claim_timeout}` and `blockstore.gc.{interval,sweep_concurrency,grace_period,dry_run_sample_size}` (each knob has a default + range + meaning), plus a tuning-guidance bullet list referencing every knob by dot-notation, plus the env-var mapping list.
- **CLI.md**: added "Block Store Garbage Collection (v0.15.0 Phase 11)" section with `dfsctl store block gc <share> [--dry-run]` (flags, output, fail-closed posture) and `dfsctl store block gc-status <share>` (output schema).

### Bonus (D-34 planner discretion) — `docs/CONTRIBUTING.md`

Commit: `2d292fec` (`docs(11-09): add EnumerateFileBlocks bullet to MetadataStore backend recipe`)

- Added a new bullet to the existing "Metadata Store" recipe under "Adding a New Store Backend" pointing at the v0.15.0 cursor contract and the IMPLEMENTING_STORES.md anchor. Tiny diff (one numbered list item), so D-34's "include if the touched-docs diff stays small" condition is satisfied.

## Cross-References Added

The five-doc graph is now closed — an operator entering at any one of them can discover the others:

| From | To | Purpose |
| --- | --- | --- |
| ARCHITECTURE.md (existing hybrid tier section) | ARCHITECTURE.md §GC + §Lifecycle | Forward-reference from Phase 10 plumbing to Phase 11 design |
| ARCHITECTURE.md §GC | CONFIGURATION.md (knobs) + CLI.md (commands) | Operator can find the knobs and the CLI from the design |
| FAQ.md "How does GC work" | ARCHITECTURE.md §GC + CONFIGURATION.md | Quick-answer reader can dive into design or tune |
| IMPLEMENTING_STORES.md §EnumerateFileBlocks | (storetest reference) | Backend implementer can find the conformance suite |
| IMPLEMENTING_STORES.md §CAS contracts | (remotetest reference) | Remote-backend implementer can find the conformance suite |
| CONFIGURATION.md §Syncer+GC knobs | ARCHITECTURE.md §GC + CLI.md | Tuning operator can understand the design and run the CLI |
| CLI.md §gc commands | ARCHITECTURE.md §GC + CONFIGURATION.md | CLI user can understand the design and find the knobs |
| CONTRIBUTING.md (Metadata Store recipe) | IMPLEMENTING_STORES.md §EnumerateFileBlocks | New backend author finds the contract |

## Deviations from Plan

None — plan executed exactly as written. Task 2's `done` criterion required at least 8 dot-notation knob references in CONFIGURATION.md; the YAML knob block alone uses non-dot YAML syntax, so the tuning-guidance bullet list was deliberately written to reference every one of the 8 new knobs by dot-notation at least once. CONTRIBUTING.md was updated under D-34 planner discretion (recipe section already existed; one-line addition kept diff trivial).

## Self-Check: PASSED

Files modified:
- `docs/ARCHITECTURE.md` — FOUND, contains all required `mark-sweep`, `three-state`, `gc-state`, `dual-read window`, `FileBlock.Hash`, `incomplete.flag`, `last-run.json`, Pending/Syncing/Remote terms.
- `docs/FAQ.md` — FOUND, contains 5 dual-read/residual/GC v0.15 lines (criterion: ≥3).
- `docs/IMPLEMENTING_STORES.md` — FOUND, contains 11 references to EnumerateFileBlocks/WriteBlockWithHash/ReadBlockVerified (criterion: ≥3).
- `docs/CONFIGURATION.md` — FOUND, contains 8 dot-notation knob references (criterion: ≥8).
- `docs/CLI.md` — FOUND, contains 5 references to `dfsctl store block gc` (criterion: ≥2 lines covering gc + gc-status).
- `docs/CONTRIBUTING.md` — FOUND, single new bullet pointing at EnumerateFileBlocks contract.

Commits exist:
- `087af807` — ARCHITECTURE.md update.
- `8b16b02a` — FAQ + IMPLEMENTING_STORES + CONFIGURATION + CLI updates.
- `2d292fec` — CONTRIBUTING.md update.

No modifications to `.planning/STATE.md` or `.planning/ROADMAP.md` (per execution prompt).
