# Server-side metadata read cache — decision (issue #1169)

Triggered by the JuiceFS architecture comparison: should DittoFS add a "Local
Metadata Cache" like JuiceFS shows in its client diagram?

## Framing (settled)

- **No-FUSE is a confirmed plus** — zero client install, no kernel module.
- **JuiceFS's "Local Metadata Cache" is client-side, RAM-only** (FUSE attr/entry
  kernel timeouts + open-file memory). DittoFS already gets that client cache
  **for free** from the NFS/SMB client kernel (attribute cache, dentry cache,
  leases/oplocks/delegations); the server already emits the invalidation signals
  (`change_info4`, ctime/mtime, SMB change-time + lease breaks). Replicating it
  inside `dfs` would be redundant.
- The only open question is a **server-side** read cache between the runtime and
  the backend store. Decision gated on two questions:
  1. Is the per-op backend read cost materially high? (micro-bench)
  2. Is it on the realistic hot path after client caching? (e2e + server pprof)

## Part 1 — metadata-store micro-benchmark (DONE)

`dfsbench metadata` calls the store directly (no protocol, no client cache).
Run: ops=20000, workers=4, tree=8 dirs × 64 files, warm. memory/badger on the
dev laptop; postgres against a local ephemeral postgres@16 cluster over loopback
(no network RTT — a lower bound for a real deployment).

| backend | getattr p50 | lookup p50 | readdir p50 | mixed p50 | getattr ops/s |
|---|---|---|---|---|---|
| memory | 0.21 µs | 0.13 µs | 20 µs | 0.21 µs | 5.7 M |
| badger | 7.7 µs | 1.4 µs | 525 µs | 6.5 µs | 366 k |
| postgres (prep on) | 160 µs | 73 µs | 315 µs | 131 µs | 24 k |

Postgres `GetFile` p50 ≈ **760× memory, ≈ 21× badger**. Loopback only — a remote
managed postgres adds ~0.5–2 ms RTT *per query* on top, and `GetFile` issues
1–2 queries (metadata + `loadFileBlockRefs`).

**Prepared statements ruled out as the cheap fix.** prep=on vs off was within
noise (getattr 160 µs vs 166 µs warm). The cost is the round-trip + the 2-query
`GetFile`, not query planning — so a cache, not a pg-tuning knob, is the lever if
one is needed.

### Gate 1 verdict: MET for postgres.
Per-op postgres read cost is materially high (100s of µs loopback, ms-class
remote). memory is already the zero-cost floor (it *is* a RAM cache — no cache to
add there); badger is cheap enough that a cache is marginal.

## Part 2 — e2e on scw VM + server pprof (DONE)

Setup: single Scaleway VM (PRO2-XXS, 2 vCPU, Ubuntu 24.04), postgres@16 local,
`dfs` with **postgres** metadata + fs block store + `controlplane.pprof: true`.
NFS share `/export` mounted loopback (vers=3). Seeded 50 dirs × 400 files = 20k.
Workload = repeated **cold** `find /mnt/bench/tree -ls` with `drop_caches` each
pass, so the NFS client re-issues every LOOKUP/GETATTR to the server (defeats
client attr-caching — measures the server's per-op handling cost). 30 s CPU
profile scraped from `/debug/pprof/profile` during the browse.

Standalone signal: a single cold crawl of 20k files took **~17 s** — only 2 full
passes fit in 34 s. Metadata browse is server-bound.

CPU profile (30 s, 18.77 s samples), cumulative share of dfs server CPU:

| frame | cum % |
|---|---|
| NFS dispatch (`handleRPCCall`) | 70.9% |
| `handleNFSLookup` (LOOKUP — the `find` hot op) | 64.3% |
| `metadata.Service.Lookup` | 46.8% |
| **`postgres.GetFile`** (the backend read) | **43.7%** |
| pgx `Query` / `queryRow` | 39.6% / 36.6% |
| `Syscall6` (flat — pg round-trip + NFS writes) | 31.3% (flat) |
| `pgconn.ExecPrepared` | 27.9% |

`ExecPrepared` confirms prepared statements were active (consistent with Part 1:
the cost is the round-trip + the GetFile query, not planning). Loopback postgres
here — a **remote** managed pg shifts even more time into the syscall/RTT bucket.

### Gate 2 verdict: MET.
When the server actually handles metadata ops (NFSv3 LOOKUP → `Service.Lookup`
→ `postgres.GetFile`), the backend read is the **dominant** server cost —
~44% of CPU in `GetFile`, ~64% in the LOOKUP handler (mostly the pg query). A
read-through cache on `GetFile`/`GetChild` directly attacks this.

## Verdict: query-reduction yes (#1176); cache deferred (#1173)

Both gates are technically met — but **160 µs/op is acceptable** in absolute
terms, and the 44%-CPU figure came from a `drop_caches`-forced cold crawl (worst
case). So the conclusion is **not** "build the cache now":

1. **Do the cheap, unconditional win first — query-reduction (#1176).** The cost
   is round-trip + the 2-query `GetFile`, not indexing (point lookups already) or
   planning (prepared statements made no difference). Collapse `GetFile`'s 1–2
   queries into one, batch READDIR hydration, review the pool. Helps *every*
   postgres deployment, no invalidation risk. "Why not."

2. **Defer the cache decision (#1173) pending a scaling + scale-out study.** Open
   questions before committing: how latency/throughput hold up under concurrency;
   whether per-op cost stays flat as the dataset grows to millions of entries;
   and — decisively — **k8s scale-out**. Multiple `dfs` replicas sharing one
   postgres **break the single-node single-writer assumption** that made cache
   invalidation tractable: a per-pod cache would need cross-pod invalidation
   (`LISTEN/NOTIFY` / pub-sub / short TTL), much harder. So the cache is *not*
   obviously worth it in the world we actually care about, and #1176 may be the
   better lever there.

If a cache is ever built (design constraints, carried to #1173): opt-in
read-through LRU+TTL in `MetadataService`, default off, postgres-focused,
write-invalidated, **cross-pod-safe invalidation designed in from the start**,
guarded by a conformance test asserting no stale read survives a mutation.

## Status

- [x] Part 1 micro-bench built + run (memory/badger/postgres). Gate 1 MET.
- [x] Prepared-statements confound measured + ruled out.
- [x] Part 2 e2e scw run + server pprof. Gate 2 MET (postgres GetFile ~44% server CPU).
- [x] Verdict: query-reduction is the action (#1176); cache decision deferred
      pending scaling + k8s scale-out study (#1173). 160 µs/op is acceptable;
      indexing won't help (already point lookups).
