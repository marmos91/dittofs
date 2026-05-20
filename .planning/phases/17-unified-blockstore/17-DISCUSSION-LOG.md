# Phase 17: Unified BlockStore interface + legacy delete + migration tool - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-05-20
**Phase:** 17-unified-blockstore
**Areas discussed:** PR splitting strategy, Migration tool placement & UX, Walk + Meta + conformance contract, Boot-time hard-error detection

---

## PR splitting strategy

| Option | Description | Selected |
|--------|-------------|----------|
| Single mega-PR per spec | All Decision 2 work in one PR. Reviewable as a coherent unit; develop stays buildable only at PR merge. Aligns with spec's 'no half-states'. Risk: 6k LoC diff hostile to review; merge conflicts during work. | ✓ |
| Multi-PR, develop buildable each step | Sequence: ifaces+adapters → conformance+remote rename → delete legacy local+shim+narrow → migration tool+boot guard. Each PR keeps develop green; each is review-sized. | |
| Two PRs | PR1 = ifaces + remote rename + conformance (additive). PR2 = delete legacy + migration tool + boot guard (destructive, atomic). | |

**User's choice:** Single mega-PR per spec
**Notes:** User explicitly aligned with the locked spec phrasing — "no flag-gated half-states, no dual-read shims". The mega-PR is wide but atomic; the alternative (split PRs) would either leave develop unbuildable mid-sequence or require transient compat shims that contradict the design contract. Internal commit ordering inside the PR remains a planner concern for review hygiene.

---

## Migration tool placement & UX

### Placement

| Option | Description | Selected |
|--------|-------------|----------|
| Offline only — `dfs migrate-to-cas` subcmd | Server-side cobra subcommand on dfs binary. Daemon must be stopped (PID/lock check). Touches local fs + metadata directly; no REST round-trips. Mirrors pkg/blockstore/migrate/migrate_offline.go pattern. Cleanest for one-shot cutover. | ✓ |
| Online via dfsctl REST | `dfsctl blockstore migrate-to-cas` POSTs to a server endpoint that runs migration in-process. Server keeps responding to /status; clients get EBUSY on file ops. | |
| Both modes | Mirror Phase 14: offline `dfs migrate-to-cas` + online dfsctl variant calling shared library. | |

**User's choice:** Offline only — `dfs migrate-to-cas` subcommand
**Notes:** Diverges from the spec's wording ("dfsctl blockstore migrate-to-cas") but is the more conservative shape for a one-shot cutover. Downstream agents: follow this choice, not the spec verbatim.

### UX

| Option | Description | Selected |
|--------|-------------|----------|
| Idempotent re-run | Per-share progress journal (.dittofs-migrate-to-cas.state) so crash recovery resumes mid-stream. | ✓ |
| --dry-run flag | Walk legacy tree, report counts/bytes/dedup estimate, no writes. Required before destructive run. | ✓ |
| Per-share scope | `--share <name>` flag; default = all shares. Lets ops migrate biggest share off-hours. | ✓ |
| Progress reporting | stdout files/sec + MiB/sec + ETA + dedup hits; optional --json for machine parsing. | ✓ |

**User's choice:** ALL FOUR — idempotent re-run, --dry-run, per-share scope, progress reporting
**Notes:** Operator-grade tooling baseline. Multi-TB migrations may run for hours; observability and resumability are non-negotiable.

---

## Walk + Meta + conformance contract

### Walk callback error semantics

| Option | Description | Selected |
|--------|-------------|----------|
| ErrStopWalk sentinel + any-error propagates | Callback returns `blockstore.ErrStopWalk` for clean early-exit; other non-nil errors halt walk and Walk returns them wrapped. Context cancellation aborts immediately. Mirrors filepath.SkipDir / fs.SkipAll. | ✓ |
| Any non-nil error halts | No special sentinel. Callers reinvent stop semantics. | |
| Continue-on-error, accumulate | Callback errors logged but don't halt. Walk returns first-error after exhaustion. | |

**User's choice:** ErrStopWalk sentinel + any-error propagates

### `Meta` struct fields + location

| Option | Description | Selected |
|--------|-------------|----------|
| blockstore.Meta = {Size int64, LastModified time.Time} | Minimal per spec. Hash is the key, not echoed. S3's x-amz-meta-content-hash kept inside s3 impl as BSCAS-06 defense-in-depth, not on Meta. | ✓ |
| Add Hash field too | Meta = {Hash, Size, LastModified}. Lets callers verify key during Walk without re-hashing. | |
| Meta as opaque interface | `type Meta interface { Size() int64; LastModified() time.Time }`. Future-proof; heavier. | |

**User's choice:** blockstore.Meta = {Size int64, LastModified time.Time}

### blockstoretest/ suite shape

| Option | Description | Selected |
|--------|-------------|----------|
| Two top-level funcs | `BlockStoreConformance(t, factory)` + `BlockStoreAppendConformance(t, factory)`. Backends call whichever applies; fs calls both. Discoverable; matches existing storetest pattern. | ✓ |
| Single RunSuite with feature flags | RunSuite(t, factory, opts{IncludeAppendLog: true}). One entrypoint; backends pass feature flags. | |
| Subtest table | Long table of named subtests; explicit but bulky. | |

**User's choice:** Two top-level funcs

---

## Boot-time hard-error detection

### Detection mechanism

| Option | Description | Selected |
|--------|-------------|----------|
| Sentinel marker file `.cas-migrated-v1` | Written by migrate-to-cas on success (atomic rename from .tmp). FSStore constructor stats it; O(1). Provenance via timestamp+version. Survives mid-migration crash (only written at end). | ✓ |
| Walk for any .blk files | Scan storage dir; presence of .blk → hard error. O(N) cost every boot. | |
| Config flag `migration_completed: true` | User edits config.yaml. Footgun: can be flipped without actually migrating. | |
| Metadata store sentinel row | `_dittofs_meta/cas_migrated_at`. Per-backend; survives storage-dir relocation. More plumbing. | |

**User's choice:** Sentinel marker file `.cas-migrated-v1`

### Failure mode

| Option | Description | Selected |
|--------|-------------|----------|
| FSStore constructor returns sentinel error; dfs start prints + exits 78 | `NewFSStore` returns `ErrLegacyLayoutDetected` wrapping the path. cmd/dfs/start unwraps via errors.As, prints multi-line directive, exits 78 (EX_CONFIG). Per-share fail-fast. | ✓ |
| Health-check on /status, server runs but refuses ops | Server boots, handlers return NFS3ERR_IO / STATUS_INTERNAL_ERROR. Lets ops curl /status. Worse UX. | |
| Adapter init checks | NFS/SMB adapters call IsMigrated() before mount; refuse to bind ports. Cleaner than per-op refusal but later than constructor. | |

**User's choice:** FSStore constructor returns sentinel error; dfs start prints + exits 78

---

## Claude's Discretion

- Exact LocalStore method cull from 22 → ~12 — researcher + planner decide borderline methods (Truncate, EvictMemory, SetRetentionPolicy, Stats, ListFiles, etc.) stay on the narrowed LocalStore vs move to LocalStoreAdmin.
- Internal commit ordering inside the mega-PR — additive first, then deletions; planner-decided. Spec requires atomic merge; internal hygiene is review-quality only.
- Whether `Walk` exposes its concurrency knob — keep internal unless conformance asserts serial order.
- Whether `Has` is HEAD (S3) or `Get` with `Range: bytes=0-0` — backend-specific.

## Deferred Ideas

- Online migration via dfsctl REST — revisit v0.17+ if operators need remote-triggered migration.
- Continue-on-error walks — locked to ErrStopWalk + halt; GC handles retry/skip in callback.
- Hash echoed in `Meta` — Meta stays minimal; future need = backend-specific extension type, not shared struct.
- Single-RunSuite conformance API — two top-level funcs locked.
- Config flag for migration completion — sentinel file wins; flag is a footgun.
- `LocalStoreAdmin` separate interface — possibly, under Claude's discretion.
- macOS mmap unlink-race investigations — already moot post-Phase-16.
- Cold-cache benchmarks — deferred to v0.17+; Phase 17 keeps warm-cache D-06 gate ≤1.02 vs Phase 16 baseline.
