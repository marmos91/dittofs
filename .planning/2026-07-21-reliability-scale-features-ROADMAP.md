# DittoFS program roadmap — reliability → metadata → benchmarks → scale & features

Created 2026-07-21. Supersedes `.planning/perf/2026-07-19-open-issue-roadmap.md` (archived).
Priority order is **speed and reliability first**, then the big metadata unification, then
fresh benchmarks, then HA + feature themes.

Per-PR quality bar (every PR, no exceptions):
1. `code-simplifier` pass on the diff.
2. `code-reviewer` pass; fix everything it and Copilot flag.
3. Lint + format (`go fmt ./... && go vet ./...`), build, unit/integration.
4. Push → iterate until **CI green**. A flake is *investigated and fixed*, never retried blind.
5. Green → **squash-merge to develop** → close the issue manually (develop ≠ default branch) → `graphify update .` → next.

Parallelize independent issues (separate worktrees off `origin/develop`, up to ~6/wave);
serialize only on real dependencies.

---

## Phase 0 — Ship v0.28.1 (IN FLIGHT)

Follow-up fixes on top of v0.28.0. Safe to release: all are pure bug fixes.

- **#1832** replay6 durable-handle (smbtorture `smb2.replay.replay6`) — *agent in flight*.
  Root cause: a fresh/non-replay create colliding on the volatile FileId (`handle.data[0]`)
  vs. `resolveCreateReplay` returning the same FileId on `FLAGS_REPLAY_OPERATION`.
  This is the fix that turns #1830's `badger-fs` shard green.
- **#1833** `#1810`/`#1829` adapter Port=0 rebind — `resolvePort` before the unchanged-listener
  check (NFS 12049 / SMB 12445). Closes **#1810**.
- **#1830** `#1781` smbtorture badger-fs timeout budget (replay/durable suite 300s on badger-fs).
  Goes green behind the #1832 fix.
- **#1834** `#1831` mutation-safety: materialize legacy payload before write/delete/truncate
  during local-only migration (data-loss window Copilot caught after v0.28.0). Verified locally.

**Gate:** all four merged + develop green → cut **v0.28.1** (bump `flake.nix` version only →
signed `chore(release)` on develop → FF main → signed annotated tag → push).

---

## Phase 1 — Reliability/perf issue sweep (in order)

1. **#1832** — see Phase 0 (in flight).
2. **#1810** — closed by #1833 (Phase 0). Verify the listener no longer drops on hot-reload.
3. **#1781** — closed by #1830 (Phase 0). Confirm badger-fs shard is durably green (not just timeout-padded).
4. **#1715** — metadata store cleanup: simplify, de-dup, cut hot-path write cost across the 3 backends.
   Feeds directly into #1828. Plans: `perf/2026-07-15-metadata-cleanup-PLAN.md`,
   `perf/2026-07-15-metadata-{entity-model,schema-inventory,sql-schema}.md`.
5. **#1692** — three-wall NFS/SMB read/write perf (segstore redesign + adapter wins).
   Plans: `perf/2026-07-15-nfs-smb-three-walls-PLAN.md`, `perf/2026-07-15-wall-a-segstore-BLUEPRINT.md`.
6. **#1658** — intermittent SMB rename/dirlease SHARING_VIOLATION flake. Reproduce deterministically
   BEFORE fixing (see #1701 lesson); #1655 diagnostic is armed.
7. **#1616** — `feat(smb)`: serve share-level SD via srvsvc `NetrShareGetInfo` level 502 (Explorer Share tab).
8. **#1432** — S3 uploader parallelization. Perf has improved a lot since the report; **ask the reporter to
   re-test on current develop and temporarily close** pending their confirmation.
   Plan: `2026-06-29-1432-upload-perf-plan.md`.

Parallelizable within this phase (no shared files): #1616, #1658, #1432-retest are independent of
the metadata/perf tracks (#1715, #1692). #1715 must land before / feed #1692's segstore + #1828.

---

## Phase 2 — #1828 metadata unification (PLAN ONLY, do not execute yet)

Collapse the 4 metadata backends into **2 families + a shared base**: flagship `sql` impl over a
Dialect (sqlite+postgres, ~95% identical), plus the KV family (badger). JuiceFS as prior art.
Keep retry semantics per-family (never fold SSI conflict handling into the SQL time-budget path).

Deliver a written plan only; land it after Phase 1 so the cleanup (#1715) is already in.
Existing design material: `perf/metadata-cache-decision.md`, the `perf/2026-07-15-metadata-*` set.

---

## Phase 3 — Complete the benchmarks (after the metadata refactor)

Fresh, apples-to-apples data on the refactored write path.

- **#1767** — fix the cold-read benchmark barrier (drain-uploads stall + sub-1GiB evict no-op).
- **#1743** — complete the fair matrix: all systems + large size + cold-read pass.

Plans: `perf/2026-07-18-full-qos-matrix-PLAN.md`, `perf/BENCHMARK-PLAN.md`. Bench discipline:
prod binary, heartbeat-monitor long runs, tear down fresh VMs only (never coder VMs), ≥6 runs.

---

## Phase 4 — Two themes

### 4a. HA scalability (brainstorm + design, no execution yet)
Make DittoFS scale across multiple instances. Open questions to work through:
- Shared vs. partitioned metadata (postgres already multi-writer; badger/sqlite are single-node).
- Block store coordination: per-share ownership vs. shared remote tier; who owns the syncer.
- File-handle stability + routing across instances (handles already encode share identity).
- Lease/lock coordination (SMB leases, NLM) across nodes.
- Session/mount affinity vs. failover.
Output: a design doc, then a phased plan.

### 4b. Features
- **Share snapshots v2** — restore a snapshot onto a **new** share (so a single file can be
  recovered without disturbing the live share being recovered). Builds on existing snapshot work.
- **Metadata backup** — automate full DB backup to S3 for all three backends (sqlite/badger/postgres),
  scheduled + on-demand, restorable.
- **Import/export** — export a single share (metadata + block manifest + optionally data) and import
  it into a separate DittoFS instance.

---

## Status log
- 2026-07-21: roadmap created; Phase 0 in flight (#1832 agent running, #1833/#1834/#1830 open, CI running).
