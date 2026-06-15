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

## Final verdict: BUILD the cache (opt-in, postgres-focused)

Both gates met. Recommended follow-up (separate issue/PR):

- **Opt-in read-through LRU+TTL cache in `MetadataService`**, in front of the
  backend store, keyed by handle (`GetFile`) and (dirHandle,name) (`GetChild`).
- **Write-driven invalidation** on every mutation that touches an entry —
  write/setattr/rename/remove/link/ACL/EA/link-count. Single-node single-writer:
  all mutations funnel through the runtime, so invalidation is tractable (no
  distributed coherence).
- **Default off; enable for postgres** (and any future remote metadata backend).
  memory needs none (it *is* RAM); badger is borderline — leave it opt-in.
- Bound the cache (entry count + TTL) so a large crawl can't pin unbounded RAM;
  TTL also caps staleness if an invalidation path is ever missed.
- Guard with a conformance test that asserts no stale read survives a mutation
  (the invalidation is the only correctness risk).

Caveat: this helps the *cold-cache / large-working-set / crawl* regime measured
here. Small hot working sets are already absorbed by the NFS/SMB client attr
cache and won't exercise the server cache — which is fine; default-off means
deployments opt in when their workload is metadata-crawl-heavy on postgres.

## Status

- [x] Part 1 micro-bench built + run (memory/badger/postgres). Gate 1 MET.
- [x] Prepared-statements confound measured + ruled out.
- [x] Part 2 e2e scw run + server pprof. Gate 2 MET (postgres GetFile ~44% server CPU).
- [x] Final verdict: build opt-in read-through cache, postgres-focused, default off.
