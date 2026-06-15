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

## Part 2 — e2e on scw VM + server pprof (PENDING)

Gate 2 — *is the cost actually hit on the realistic path* — is unanswered by the
micro-bench, because NFS/SMB clients absorb most repeat GETATTR/LOOKUP in their
own attr cache. Needs: dfs server (postgres metadata, `controlplane.pprof: true`)
on a scw VM, client mount, metadata-heavy browse, scrape server `/debug/pprof`.

Caveat for executing Part 2: the existing `bench/infra` competitor matrix uses
**badger**, not postgres, and does not enable pprof — a `dittofs-postgres`
install script + pprof config are prerequisites.

## Recommendation (interim)

Gate 1 is clearly met for postgres; gate 2 decides it. Two honest readings:

- **If** Part 2 shows the server frequently serves cold metadata reads (cache
  cold/insufficient client TTLs, large working sets, READDIR-heavy crawls) →
  build an **opt-in read-through LRU+TTL in `MetadataService`**, keyed by handle
  and (dirHandle,name), invalidated on every write/rename/remove/ACL/EA/
  link-count change, **default off, postgres-focused**. Single-node single-writer
  makes invalidation tractable (all mutations funnel through the runtime).
- **If** client attr-caching already absorbs the reads (small hot sets, repeated
  stat) → the server rarely pays this cost; **do not add the cache** (surface =
  debt). Record the negative result.

Do not commit to building the cache until Part 2 quantifies hot-path frequency.

## Status

- [x] Part 1 micro-bench built + run (memory/badger/postgres). Gate 1 MET.
- [x] Prepared-statements confound measured + ruled out.
- [ ] Part 2 e2e scw run + server pprof (needs dittofs-postgres provisioning + pprof).
- [ ] Final verdict.
