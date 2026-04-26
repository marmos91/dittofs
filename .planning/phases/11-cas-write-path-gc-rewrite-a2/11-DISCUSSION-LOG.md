# Phase 11: CAS write path + GC rewrite (A2) — Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in `11-CONTEXT.md` — this log preserves the alternatives considered.

**Date:** 2026-04-25
**Phase:** 11-cas-write-path-gc-rewrite-a2
**Areas discussed:** GC mark-sweep design, State machine + atomicity, Read-path verification + dual-read shim, Sync hot path + TD-09 + LocalStore narrowing

---

## GC mark-sweep design

### Live-set construction

| Option | Description | Selected |
|--------|-------------|----------|
| In-memory exact set | `map[ContentHash]struct{}` accumulated by streaming all metadata-store FileAttr.Blocks. ~64 B/entry. 10M entries ≈ 640 MB. | |
| Two-pass sorted external file | Stream hashes to sorted on-disk file; sweep streams candidates with binary-merge. Bounded memory. | ✓ (Claude pick) |
| Bloom filter + exact recheck | Memory-bounded; Bloom rejection definitive (no false negatives); FP retains some garbage forever. | |
| You decide | Defer to planner. | |

**User's choice:** "You choose. Balance between performance and reliability (should resist restarts/crashes)" — Claude picked disk-backed exact set under `<localStore>/gc-state/<runID>/`. Rejected Bloom (silently retains garbage), rejected pure in-memory (loses hours of mark work on large deployments). Backing impl planner's discretion (recommend Badger temp store).

### Sweep parallelism

| Option | Description | Selected |
|--------|-------------|----------|
| Bounded worker pool over 256 top prefixes | gc.sweep_concurrency=16 default. Caps S3 connection pressure. | ✓ |
| Full grid (256×256 = 65536) | Maximum parallelism; rate-limit risk. | |
| Sequential single walk | Slowest but cheapest. | |
| Adaptive ramp | Ramp until 503 SlowDown; back off. | |

### GC trigger model

| Option | Description | Selected |
|--------|-------------|----------|
| Both: periodic + CLI/REST on-demand | gc.interval (default OFF) + dfsctl + REST endpoint. | ✓ |
| On-demand only | Smallest blast radius. | |
| Periodic only | Less operational tooling. | |
| Manual signal | Operationally awkward. | |

### Sweep vs syncer concurrency

| Option | Description | Selected |
|--------|-------------|----------|
| Snapshot mark + grace TTL on object age | Mark records T; sweep deletes only objects with LastModified < T - grace. | ✓ |
| Hard fence: drain syncer before sweep | Pauses write throughput. | |
| Soft delete + second-pass purge | Two-cycle latency. | |
| Optimistic re-upload on conflict | Risks read-amplification storms. | |

### Cross-share remote-store coordination

| Option | Description | Selected |
|--------|-------------|----------|
| Mark across all shares with same remote identity | Global union live set per remote. | ✓ |
| Per-share mark + per-share sweep | Breaks cross-share dedup. | |
| Mark global, sweep per-share-prefix | Architecturally inconsistent. | |

### Snapshot grace TTL default

| Option | Description | Selected |
|--------|-------------|----------|
| Default 1h, configurable via gc.grace_period | Warn if set below 5min. | ✓ |
| Default 24h | Wastes storage extra day. | |
| Tied to syncer interval | Self-tuning; less obvious. | |
| You decide | | |

### Metadata-store mark-phase API

| Option | Description | Selected |
|--------|-------------|----------|
| New cursor: MetadataStore.EnumerateFileBlocks(ctx, fn) | Native impl per backend; conformance-suite extension. | ✓ |
| Reuse ListFiles + per-file GetFile | N+1 query pattern. | |
| Backend-specific fast path | Breaks conformance contract. | |

### Sweep error handling

| Option | Description | Selected |
|--------|-------------|----------|
| Continue + capture; report at end | Single transient doesn't kill sweep run. | ✓ |
| Fail-fast on first error | Wastes whole sweep. | |
| Bounded retry per prefix, then fail-fast | Middle ground. | |
| You decide | | |

### Observability

| Option | Description | Selected |
|--------|-------------|----------|
| Structured logs + dfsctl status surfaces last-run summary | Slog INFO + last-run.json + dfsctl gc-status. No Prometheus. | ✓ |
| Logs + Prometheus metrics | Adds metrics dependency surface earlier. | |
| Logs only | Weakest UX. | |
| Logs + REST event stream | Heavyweight. | |

### Dry-run mode

| Option | Description | Selected |
|--------|-------------|----------|
| Yes — dfsctl gc --dry-run + REST flag | Critical for first deploy + debugging. | ✓ |
| No, add later | Risks operator anxiety. | |
| Yes log-only no REST | Easier to delete. | |

---

## State machine + atomicity

### Upload vs metadata-txn ordering

| Option | Description | Selected |
|--------|-------------|----------|
| Upload first, then metadata-txn flips to Remote | CAS idempotent re-upload; orphans GC-recoverable. | ✓ (Claude pick — user said "you decide the best") |
| Metadata-txn first (record intent), then upload, then mark | Doubles metadata write pressure; Intent-state recovery. | |
| Co-commit via outbox pattern | Heavyweight for v0.15.0. | |

### State home

| Option | Description | Selected |
|--------|-------------|----------|
| FileBlock.State persisted, indexed by ContentHash | Three states {Pending, Syncing, Remote}; literal STATE-01/03; best introspection. | ✓ (Claude pick — user said "you choose the best/most clean") |
| Inferred from on-disk presence + metadata; no explicit field | Weaker observability for stuck-Syncing diagnosis. | |
| Explicit field + ephemeral in-memory Syncing tracker | Less metadata write pressure; weaker introspection. | |

### Transition trigger owner

| Option | Description | Selected |
|--------|-------------|----------|
| engine.Syncer in syncer.go after PUT+meta-txn success | Single owner; no callback up LocalStore stack. | ✓ |
| Background reconciler observes S3 + flips state | Eventual-consistency window. | |
| Engine API caller passes write-completion callback | Stranded blocks risk. | |

### INV-03 verification approach

| Option | Description | Selected |
|--------|-------------|----------|
| Crash-injection unit test in pkg/blockstore/engine | fakeRemoteStore + fakeMetadataStore + kill-points. | ✓ |
| E2E with kill-mid-upload via SIGKILL | Higher fidelity; slower; race-y. | |
| Both unit + E2E | Best coverage; bigger surface. | |

---

## Read-path verification + dual-read shim

### BLAKE3 verification model

| Option | Description | Selected |
|--------|-------------|----------|
| Streaming verifier wrapping resp.Body | Zero extra alloc; single pass. | ✓ |
| Buffer-then-verify | Doubles peak memory. | |
| Trust x-amz-meta-content-hash header (skip recompute) | Defeats INV-06. | |
| Header pre-check + streaming recompute | Fail-closed twice; same hot-path cost. | (folded into D-19 alongside ✓) |

### Read-path perf gate

| Option | Description | Selected |
|--------|-------------|----------|
| Hard gate ≤5% rand-read regression vs Phase 10 baseline | Within ≤6% global budget. | ✓ |
| Soft gate: warn 5%, block 10% | Lets minor regressions ship. | |
| No gate | Risky. | |
| Skip verification on cache-hit reads | Defers IOPS hit to cold-cache. | |

### Dual-read resolution mechanism

| Option | Description | Selected |
|--------|-------------|----------|
| Lookup by metadata key shape, not S3 trial | FileBlock row presence selects CAS vs legacy. | ✓ |
| Try CAS first, fall back to legacy on 404 | Doubles S3 GETs for legacy. | |
| Legacy reader as separate code path keyed by file-creation-version | Schema migration cost. | |
| Lazy migration on first read | Belongs to Phase 14. | |

### Cache key bifurcation

| Option | Description | Selected |
|--------|-------------|----------|
| Both — CAS by ContentHash, legacy by storeKey | Two key spaces during dual-read window. | ✓ |
| storeKey only | No cross-file dedup. | |
| ContentHash only | Forces hashing on legacy reads. | |
| You decide | Defer. | |

---

## Sync hot path + TD-09 + LocalStore narrowing

### TD-09 implementation

| Option | Description | Selected |
|--------|-------------|----------|
| Stage-bytes-and-release pattern | bytes.Clone under lock; I/O outside. | ✓ |
| Route through Phase 10 AppendWrite, retire flushBlock | Bigger scope; flag for follow-up. | |
| Per-block channel + worker | More moving parts. | |

### Sync trigger

| Option | Description | Selected |
|--------|-------------|----------|
| Periodic only (current behavior) | syncer.tick=30s default; pressure channel from Phase 10 still triggers. | ✓ |
| Periodic + COMMIT/FLUSH-driven | Better RPO; needs adapter plumbing. | |
| Periodic + pressure-driven (log fill) | Phase 10 already has it. | (already wired; confirmed routing through CAS path) |
| Both periodic + pressure | | |

### Upload concurrency

| Option | Description | Selected |
|--------|-------------|----------|
| Bounded share-wide pool (default 8) | Caps S3 connections; predictable. | ✓ |
| Per-file serialized | Single-large-file tanks throughput. | |
| Unbounded | Goroutine explosion. | |
| Adaptive on S3 latency | Costly to implement now. | |

### LocalStore narrowing scope

| Option | Description | Selected |
|--------|-------------|----------|
| MarkBlockRemote, MarkBlockSyncing, MarkBlockLocal, GetDirtyBlocks, SetSkipFsync (5 methods, 22→17) | Exact spec target. | ✓ |
| Above + WriteAt | Tighter than spec. | |
| Above + EvictMemory | Tighter than spec. | |
| You decide — audit during planning | Defer to planner. | |

### Eviction signal

| Option | Description | Selected |
|--------|-------------|----------|
| On-disk presence in blocks/ + LRU index in LocalStore | Self-contained; no FileBlockStore lookup on hot path. | ✓ |
| Disk-watcher background goroutine | Higher complexity. | |
| Pure on-disk-presence check; no in-process index | stat() per read. | |

### PR/commit shape

| Option | Description | Selected |
|--------|-------------|----------|
| Three PRs: write+state / read+verify+dual-read / GC+canonical-test | Each PR ships green; PR-A unblocks Phase 12. | ✓ |
| Two PRs: write-path / GC-path | Larger PR-A; reviewer fatigue. | |
| Single PR with atomic per-requirement commits | Maximum atomicity; longest CI. | |
| You decide | Defer to planner. | |

### Test scope

| Option | Description | Selected |
|--------|-------------|----------|
| Canonical E2E + syncer crash-unit + conformance-suite extensions + perf gate | Phase 11 floor. | ✓ |
| Above + reusable crash-injection harness | Doubles test surface. | |
| Canonical + crash-unit only; defer perf gates | Risks unnoticed regression. | |
| You decide | Defer. | |

---

## Claude's Discretion

- Backing impl for `<localStore>/gc-state/` (Badger vs flat sorted file vs boltdb)
- Exact `MetadataStore.EnumerateFileBlocks` Go signature (callback vs iterator vs channel)
- `FileBlock.State` + `last_sync_attempt_at` schema layout per backend
- Whether the `pkg/blockstore/sync/upload.go` → `engine/syncer.go` rename is already done
- `dfsctl` REST endpoint paths
- Whether `WriteAt` / `EvictMemory` get removed in this phase (planner audit)
- Per-PR commit subdivision above the per-requirement floor
- `docs/CONTRIBUTING.md` "Adding a new metadata-backend EnumerateFileBlocks" recipe inclusion

## Deferred Ideas

- Reusable crash-injection test harness (test-infra phase)
- Prometheus metrics for GC + syncer (observability phase)
- COMMIT/FLUSH-driven sync (RPO-focused phase)
- Adaptive sweep concurrency (data-driven follow-up)
- Resume-on-restart for partial mark phase
- WriteAt / EvictMemory removal from LocalStore (Phase 12+)
- Routing all writes through AppendWrite, retiring flushBlock entirely
- Three-phase upload ordering (rejected for double write pressure)
- Bloom-filter live set (rejected for FP retention)
- Skip verification on cache-hit reads
- Per-share atomic backup integration (v0.16.0)
- Block-payload compression/encryption (BlockStore Security milestone)
