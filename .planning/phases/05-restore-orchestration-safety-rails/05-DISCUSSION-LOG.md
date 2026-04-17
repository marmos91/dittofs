# Phase 5: Restore Orchestration + Safety Rails - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in 05-CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-04-16
**Phase:** 05-restore-orchestration-safety-rails
**Mode:** `--auto` (no interactive questioning; every pick is Claude's recommended default)
**Areas discussed:** Share enable/disable model, restore orchestration pipeline, manifest pre-flight, block-GC retention hold, interrupted-restore recovery, post-restore client invalidation, observability, code layout

---

## Area: Share Enable/Disable State Model (REST-02)

### Schema shape

| Option | Description | Selected |
|--------|-------------|----------|
| Enabled bool column | Simple boolean, default true, mirrors ReadOnly pattern | ✓ |
| DisabledAt *time.Time | Richer semantics (when was it disabled), nullable | |
| Separate share_states table | Overkill for a single boolean |  |

**Auto-pick:** Enabled bool (D-01). Rationale: simplest expression of REST-02 binary, mirrors existing `Share.ReadOnly`, avoids nullable-column complexity for a requirement that only needs yes/no.

### Enforcement semantics

| Option | Description | Selected |
|--------|-------------|----------|
| Disconnect + refuse | Closes active connections, refuses new (REQUIREMENTS language) | ✓ |
| Refuse new only | Legacy connections linger | |

**Auto-pick:** Disconnect + refuse new (D-02). Rationale: matches REST-02 literal.

### Transition timing

| Option | Description | Selected |
|--------|-------------|----------|
| Synchronous wait | Blocks until adapters drop connections (bounded by ShutdownTimeout) | ✓ |
| Fire-and-forget | Returns immediately, eventual consistency | |

**Auto-pick:** Synchronous (D-03). Rationale: simpler mental model; restore can proceed without guessing adapter state.

### Auto-re-enable on restore

| Option | Description | Selected |
|--------|-------------|----------|
| No — operator explicitly re-enables | Forces inspection before resuming traffic | ✓ |
| Yes — restore completion re-enables | Quicker but skips verification | |

**Auto-pick:** No (D-04). Rationale: safety-first; operator verifies before clients hit possibly-mismatched state.

---

## Area: Restore Orchestration Pipeline (REST-01, REST-03, REST-05)

### Swap mechanism

| Option | Description | Selected |
|--------|-------------|----------|
| Side-engine + atomic registry swap | Fresh engine on temp path, swap pointer under stores.Service lock | ✓ |
| In-place (engine.Restore on live) | Violates Phase 2 D-06 "empty destination" | |
| Quiesce only | Still exposes partial state to runtime callers | |

**Auto-pick:** Side-engine + swap (D-05). Rationale: reuses Phase 2 D-06 "empty destination" invariant; orchestrator owns the commit moment; failure before swap is cleanly rolled back.

### Store identity gate

| Option | Description | Selected |
|--------|-------------|----------|
| Hard reject on mismatch | manifest.store_id != target → abort | ✓ |
| Warn + --force-cross-store | Escape hatch for cross-store migration | |
| Warn only | Too permissive for a destructive operation | |

**Auto-pick:** Hard reject (D-06). Rationale: Pitfall #4 mitigation; cross-store restore is deferred (REST2NEW-01).

### Concurrency

| Option | Description | Selected |
|--------|-------------|----------|
| Shared per-repo mutex with backup | Same OverlapGuard (Phase 4 D-07) | ✓ |
| Separate mutex | Allows in-repo concurrent backup+restore | |

**Auto-pick:** Shared mutex (D-07). Rationale: backup + restore on same repo is physically incoherent; machine-enforced mutual exclusion.

### Fresh engine construction

| Option | Description | Selected |
|--------|-------------|----------|
| stores.Service.OpenMetadataStoreAtPath + SwapMetadataStore | Decoupled open/register | ✓ |
| Overload existing RegisterMetadataStore | Mingles registration semantics | |

**Auto-pick:** New methods (D-08, D-23). Rationale: keep open and register as separate primitives for testability.

---

## Area: Manifest Pre-flight Verification (REST-03)

### Verification surface

| Option | Description | Selected |
|--------|-------------|----------|
| Manifest-only + streaming SHA-256 during restore | Cheap pre-flight + Phase 3 D-11 streaming verify | ✓ |
| Full payload download + verify + then restore | Two-pass, wastes bandwidth | |
| No pre-flight (let engine.Restore fail) | Risks touching live state with bad input | |

**Auto-pick:** Manifest-only + streaming (D-05 steps 3-4, 7-9). Rationale: cheap; leverages Phase 3 D-11 already-implemented streaming verify; aborts before any live-state touch on mismatch.

---

## Area: Block-Store GC Retention Hold (SAFETY-01)

### Hold computation strategy

| Option | Description | Selected |
|--------|-------------|----------|
| At-GC-time manifest union | Iterate records, fetch manifests, union PayloadIDSet | ✓ |
| Persisted backup_holds table | Write on backup-completion, read on GC | |
| No hold, documented warning | Exposes data-loss on restore of old backups | |

**Auto-pick:** At-GC-time (D-11). Rationale: self-heals on retention deletes; no new write path or recovery surface; PayloadID sets small enough to fit in memory.

### New Destination method

| Option | Description | Selected |
|--------|-------------|----------|
| Add GetManifestOnly(ctx, id) | Cheap per-id manifest fetch for GC hold | ✓ |
| Reuse GetBackup(ctx, id) and discard payload | Downloads full payload wastefully | |

**Auto-pick:** Add GetManifestOnly (D-12). Rationale: GC runs may iterate dozens of backups; cheap path matters.

### Scope of the hold

| Option | Description | Selected |
|--------|-------------|----------|
| Orphan-block path only | Hold protects GC-initiated deletes, not file-unlink deletes | ✓ |
| Global: all block deletes | Over-restrictive, violates user-initiated deletes | |

**Auto-pick:** Orphan-block only (D-13). Rationale: narrowest correct fix; honors user intent.

---

## Area: Interrupted Restore Job Recovery (SAFETY-02 extension)

### Recovery model

| Option | Description | Selected |
|--------|-------------|----------|
| Mark interrupted + operator re-runs | New BackupJob on retry; no auto-retry | ✓ |
| Auto-retry from checkpoint on startup | Partial-restore footgun | |

**Auto-pick:** Mark + operator re-run (D-17, D-18). Rationale: partial restore unsafe (Phase 2 D-07); idempotence simpler than checkpointing.

### Orphan restore temp paths

| Option | Description | Selected |
|--------|-------------|----------|
| Sweep at Serve-time | Delete stale `.restore-<ulid>` dirs / schemas | ✓ |
| Operator-cleanup only | Accumulates on crash | |

**Auto-pick:** Sweep (D-14). Rationale: parallels Phase 3 D-06; keeps disk/DB tidy.

---

## Area: Post-Restore Client Invalidation (Pitfall #2)

### NFSv4 boot verifier

| Option | Description | Selected |
|--------|-------------|----------|
| Bump on restore completion | Defense-in-depth for reclaim-grace path | ✓ |
| No bump (rely on share-disable) | Relies on operator never re-enabling before clients observe disconnect | |

**Auto-pick:** Bump (D-09). Rationale: cheap (1-2 lines); covers edge case where operator races client reconnect; NFSv4 clients see new verifier → reclaim path → clean failure.

### SMB durable handles / leases

| Option | Description | Selected |
|--------|-------------|----------|
| No explicit clear (metadata travels in store) | State persisted inside metadata store; replaced by snapshot | ✓ |
| Explicit clear step in orchestrator | Speculative complexity; not required | |

**Auto-pick:** No explicit clear (D-10). Rationale: SMB durable-handle / lease state lives in Badger `dh:*` / `lock:*` keys → naturally replaced by restored snapshot; in-memory adapter session tables already cleared by share-disable.

---

## Area: Observability

### Phase 5 observability scope

| Option | Description | Selected |
|--------|-------------|----------|
| Minimal counters + top-level OTel span | backup_operations_total + last_success_timestamp + one span | ✓ |
| Full Prometheus suite | Histograms, gauges, byte-throughput, retention counters | |
| Skip (defer to Phase 7) | Ships without silent-failure alerting | |

**Auto-pick:** Minimal (D-19). Rationale: Pitfall #10 (silent failure) needs last_success_timestamp + counter minimum; full suite beyond v0.13.0 need.

### Gating

| Option | Description | Selected |
|--------|-------------|----------|
| Reuse server.metrics.enabled | No new config knob | ✓ |
| New backup.metrics.enabled config | Fine-grained but redundant | |

**Auto-pick:** Reuse existing flag (D-20).

---

## Area: Code Layout

### Restore package location

| Option | Description | Selected |
|--------|-------------|----------|
| pkg/backup/restore/ parallel to executor | Mirrors Phase 4 D-24 | ✓ |
| Embed in storebackups sub-service | Couples orchestration to runtime layer | |
| New runtime/restore/ sub-service | Violates Phase 4 D-25 "single service both kinds" | |

**Auto-pick:** pkg/backup/restore/ (D-21).

### Entrypoint

| Option | Description | Selected |
|--------|-------------|----------|
| storebackups.Service.RunRestore(ctx, repoID, recordID *string) | Sibling of RunBackup (D-23) | ✓ |
| Separate service | Duplicates overlap-guard and lifecycle plumbing | |

**Auto-pick:** Extend storebackups.Service (D-24).

---

## Claude's Discretion

Items left to planner / researcher during Phase 5 planning:

- Exact `Destination.GetManifestOnly` signature (return `*Manifest` vs raw bytes)
- `shares.Service.DisableShare` grace-period parameter vs reusing `lifecycle.ShutdownTimeout`
- Postgres temp schema naming convention + nested txn semantics
- Badger temp dir location (adjacent vs sibling-parent)
- Post-restore boot-verifier bump call site (handlers pkg vs runtime primitive)
- Prometheus metric prefix naming (`dittofs_` vs bare)
- Whether to surface `ListRestoreCandidates` now vs. defer to Phase 6

All documented in 05-CONTEXT.md §Claude's Discretion.

---

## Deferred Ideas (summary — full list in 05-CONTEXT.md)

- Restore to different store (REST2NEW-01)
- Cross-engine restore (XENG-01)
- Automatic verify command (AUTO-01)
- Incremental restore (INCR-01)
- Checkpoint-based resumable restore
- Full Prometheus suite
- Persisted block-GC hold table
- Explicit SMB durable-handle clear on restore
- K8s operator integration

No scope creep surfaced in auto mode; all areas stayed within REST-01..05 / SAFETY-01/02 requirements.
