# Phase 14: Migration tool (A5) - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-05
**Phase:** 14-migration-tool-a5
**Areas discussed:** Resumability + state file; Online vs offline + shim cutover; Bandwidth + parallelism; Integrity check + CLI/status; Documentation

---

## Resumability + state file

### Q1 — Checkpoint granularity for migration state?

| Option | Description | Selected |
|--------|-------------|----------|
| Per-file | Atomic unit = one file; redo whole file on crash; GetByHash makes re-uploads idempotent. | ✓ |
| Per-chunk | Journal each chunk as it lands in S3; smaller redo but state grows ~hash-per-chunk. | |
| Per-batch | Group N files per checkpoint; tunable but more code. | |

**Notes:** Recommended option matched user's pick.

### Q2 — Where lives `.migration-state.json`?

| Option | Description | Selected |
|--------|-------------|----------|
| Per-share state dir | `{share-data-dir}/.migration-state.json`; atomic rename within same fs. | ✓ |
| Metadata store (table/bucket) | Replicated with metadata; couples tool to schema. | |
| CWD / operator-supplied path | `--state-file=PATH`; risk of loss. | |

### Q3 — State file write protocol?

| Option | Description | Selected |
|--------|-------------|----------|
| Append-only journal + periodic snapshot | Per-file commit appends JSONL line; periodic atomic-rename snapshot. | ✓ |
| Single JSON, atomic rewrite per commit | O(N) bytes per commit; slow on large shares. | |
| SQLite | ACID but adds dep + binary state. | |

### Q4 — On resume, what's re-verified?

| Option | Description | Selected |
|--------|-------------|----------|
| Trust journal head, GetByHash dedups redo | Resume from last committed file; orphan CAS chunks left for GC. | ✓ |
| Re-verify last N committed files via HEAD | `--resume-verify=N` sanity check. | |
| Full integrity scan on resume | Strong but expensive on large shares. | |

---

## Online vs offline + shim cutover

### Q1 — Migration concurrency model with the share?

| Option | Description | Selected |
|--------|-------------|----------|
| Offline only — share unmounted | Tool refuses if share active; matches ROADMAP "offline migration tool". | ✓ |
| Online, read-only | Live reads through shim; writes blocked at protocol layer. | |
| Online with per-file lock | Most flexible, most race-prone. | |

### Q2 — Where lives the per-share dual-read flag?

| Option | Description | Selected |
|--------|-------------|----------|
| Metadata store — share record | New `block_layout` field; metadata txn flips it. | ✓ |
| Server config file | Requires daemon restart; drift risk. | |
| Marker file in share data dir | Split-brain risk across replicas. | |

### Q3 — When does shim flip from `legacy` to `cas-only`?

| Option | Description | Selected |
|--------|-------------|----------|
| Auto on success after integrity check | Same metadata txn as last legacy delete; one-step. | ✓ |
| Two-step: explicit `migrate cutover` | Manual safety gate; more ceremony. | |
| Auto, no integrity check | Loses ROADMAP success-4 guarantee. | |

### Q4 — Rollback path on integrity failure / abort?

| Option | Description | Selected |
|--------|-------------|----------|
| No rollback — fail loud, manual fix | Exit non-zero; leave shim/journal/CAS intact; operator re-runs. | ✓ |
| Best-effort rollback: flip shim back | Only safe if no legacy keys deleted yet. | |
| Full reverse migration | Heavy code, used once. Reject. | |

---

## Bandwidth + parallelism

### Q1 — Bandwidth limit scope?

| Option | Description | Selected |
|--------|-------------|----------|
| Shared global token bucket, uploads only | Matches ROADMAP "aggregate of parallel workers". | ✓ |
| Shared bucket on uploads + downloads | Useful if legacy reads also remote. | |
| Two separate buckets (--bw-up / --bw-down) | More tunable, more flags. | |

### Q2 — Default `--parallel` value + interaction with running server?

| Option | Description | Selected |
|--------|-------------|----------|
| Default 4, refuse if daemon active | ROADMAP-mandated default; offline-only invariant. | ✓ |
| Default 4, advisory warning | Risk of double-billing remote API. | |
| Default = autoscaled NumCPU | Less predictable for maintenance windows. | |

### Q3 — `--bandwidth-limit` units?

| Option | Description | Selected |
|--------|-------------|----------|
| MB/s as 1,000,000 B/s + suffixes | Matches aws-cli + restic precedent. | ✓ |
| MiB/s as 2^20 only | Binary throughout. | |
| Bytes/s only | No suffixes; painful but unambiguous. | |

### Q4 — How is the global token bucket implemented?

| Option | Description | Selected |
|--------|-------------|----------|
| `golang.org/x/time/rate.Limiter` | stdlib-adjacent; battle-tested. | ✓ |
| Custom token bucket (no extra dep) | ~50 LoC; subtle-bug risk. | |
| Per-worker semaphore + sleep | Aggregate = N×per-worker; contradicts ROADMAP risk. | |

---

## Integrity check + CLI/status

### Q1 — Post-migration integrity check level?

| Option | Description | Selected |
|--------|-------------|----------|
| HEAD per BlockRef across all migrated files | Linear in unique blocks; dedup helps; matches ROADMAP success-4. | ✓ |
| HEAD + sample N% body readback + hash verify | Catches bit-rot; slow at TB scale. | |
| HEAD with x-amz-meta-content-hash check | Drift detection at zero extra request cost. | |

**Notes:** Selected option already covers the metadata-header parity check at zero extra cost — folded that into D-A12 anyway.

### Q2 — Legacy key deletion ordering?

| Option | Description | Selected |
|--------|-------------|----------|
| End-of-share, after integrity check passes | Enables fail-loud rollback. | ✓ |
| Per-file, immediately after FileAttr.Blocks commit | Lower peak storage but defeats rollback. | |
| Deferred to Phase 15 / explicit `migrate gc` | Doubles storage during rollout. | |

### Q3 — Progress reporting style?

| Option | Description | Selected |
|--------|-------------|----------|
| Structured slog + TTY progress bar when stdout is a tty | Pipe-safe by default; friendly for interactive runs. | ✓ |
| Structured slog only | Operator tails the log. | |
| Progress bar primary, slog suppressed unless --verbose | Bad for unattended runs. | |

### Q4 — `migrate status` output shape + REST endpoint scope?

| Option | Description | Selected |
|--------|-------------|----------|
| CLI + `-o json`, no REST in Phase 14 | ROADMAP says optional. | |
| CLI + REST endpoint shipped in Phase 14 | `GET /api/v1/blockstore/migrate/status`; for dittofs-pro UI. | ✓ |
| CLI human only | Breaks scripting. | |

**Notes:** User overrode the recommended "defer REST". REST endpoint is now in scope for Phase 14.

---

## Documentation

### Q1 — Primary operator-facing doc shape?

| Option | Description | Selected |
|--------|-------------|----------|
| `docs/BLOCKSTORE_MIGRATION.md` runbook + cobra-generated CLI ref | Hand-written runbook; auto-gen flag table to avoid drift. | ✓ |
| Single deep dive, no auto-gen | Drift risk on flag rename. | |
| Inline cobra `--help` only | Insufficient for production rollout. | |

### Q2 — Architecture / internals doc updates?

| Option | Description | Selected |
|--------|-------------|----------|
| Update ARCHITECTURE.md + IMPLEMENTING_STORES.md + FAQ | New shim section; share-record schema note for backend authors; v0.13→v0.15 FAQ. | ✓ |
| ARCHITECTURE.md only | Future backend authors miss schema change. | |
| No internals updates | Operators rolling out v0.15 see stale docs. | |

### Q3 — Walked-through examples in BLOCKSTORE_MIGRATION.md?

| Option | Description | Selected |
|--------|-------------|----------|
| Four worked examples: small share + large share + crash-resume + integrity-failure | Sets operator expectations concretely under pressure. | ✓ |
| One worked example + reference | Lighter to maintain. | |
| No examples, prose only | Hardest to follow under pressure. | |

### Q4 — Where do migration design docs live?

| Option | Description | Selected |
|--------|-------------|----------|
| .planning/phases/14-*/ artifacts only | Matches Phase 11/12/13 precedent. | ✓ |
| New `docs/adr/0XXX-cas-migration.md` ADR | First ADR for milestone; doc surface upkeep. | |
| Both: phase artifacts + condensed ADR | Redundant with phase CONTEXT.md. | |

---

## Claude's Discretion

- Concrete journal snapshot interval (default ~1000 entries — planner picks; tunable).
- Cobra command/subcommand naming detail — match existing dfsctl conventions.
- REST handler placement within `cmd/dfs/` API tree.
- Worker pool implementation style (`errgroup` vs custom semaphore) — match `pkg/blockstore/engine/syncer.go`.

## Deferred Ideas

- `--resume-verify=N` flag for HEAD-verify of last N committed files on resume.
- `migrate revert` (CAS → legacy reverse) — rejected.
- Two-step manual cutover (`migrate cutover`) — rejected in favor of auto cutover.
- Per-chunk checkpoint granularity — rejected.
- Bandwidth limit on legacy reads — rejected.
- Cross-bucket dedup — non-goal.
- New Prometheus metrics for migration progress — milestone-wide deferral.
- ADR file — phase artifacts are the record.
