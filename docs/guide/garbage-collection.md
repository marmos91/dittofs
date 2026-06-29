# Garbage Collection

DittoFS stores file data as content-addressed **blocks** (chunks). When you delete
a file, its directory entry disappears immediately, but the underlying blocks are
reclaimed **asynchronously** by garbage collection (GC). GC frees space on **both**
tiers of a share's block store:

- the **local** tier (the on-disk cache, e.g. `/var/lib/dittofs/blocks`), and
- the **remote** tier (S3 / object storage), if the share has one.

A block is reclaimed only when **no live file references it** and **no snapshot
holds it**. Blocks shared by several files (deduplication) survive until the last
referrer is gone.

## Automatic GC (default)

Background GC is **on by default** — you do not need to run anything. The server
reclaims orphaned blocks on a fixed interval:

```yaml
gc:
  auto_enabled: true     # background GC on/off (default true)
  auto_interval: 15m     # period between runs (default 15m; (0,1m) rejected)
  grace_period: 1h       # blocks younger than this are never swept (default 1h)
```

(Env vars: `DITTOFS_GC_AUTO_ENABLED`, `DITTOFS_GC_AUTO_INTERVAL`,
`DITTOFS_GC_GRACE_PERIOD`.)

The **grace period** protects freshly written blocks: a block whose last
modification is within `grace_period` is never deleted, even if it currently looks
unreferenced. Keep it comfortably longer than your worst-case metadata-commit
latency after a write (the 1h default is ample).

Disable automatic GC (`auto_enabled: false`) only if you want to drive
reclamation entirely on demand or via external scheduling.

## Manual GC

You can trigger GC at any time:

```bash
# Reclaim now (both tiers) for a share:
dfsctl store block gc <share>

# Preview only — enumerate candidates, delete nothing:
dfsctl store block gc <share> --dry-run

# See the last run's summary (objects swept, bytes freed, errors):
dfsctl store block gc-status <share>
```

> The command lives under `dfsctl store block`, not `dfsctl system`.

## Reclaiming space leaked by older versions

Versions before the unlink-refcount fix could leave **stranded** block rows behind
when files were deleted, so their blocks were never reclaimed and disk usage only
grew. On upgrade, the server runs a **one-time reconcile** automatically (guarded by
a per-store marker) to reap those stranded rows and reclaim the space.

You can also run it on demand — recommended if an older deployment accumulated a
large backlog:

```bash
# Server-wide: reap stranded rows, then sweep both tiers.
dfsctl store block gc <share> --reconcile

# Preview the reconcile first:
dfsctl store block gc <share> --reconcile --dry-run
```

A reconcile run reports how many rows it reaped as `stranded_rows_reaped` — visible in
`dfsctl store block gc <share> --reconcile -o json` (under `stats.stranded_rows_reaped`),
in the Prometheus counter `dittofs_gc_stranded_rows_reaped_total`, and in the server log
(`INFO RunBlockGCReconcile: complete`).

## Retention and pinning

A share's `retention` policy controls the **local cache**, not GC of orphans:

- `retention: pin` keeps *referenced* blocks on local disk indefinitely (no cache
  eviction). It does **not** protect *orphaned* blocks — GC still reclaims blocks
  that no live file or snapshot references, on both tiers, regardless of `pin`.
- Other retention policies additionally let the local cache evict cold *referenced*
  blocks under disk pressure (they can be refetched from the remote tier).

So under `retention: pin`, deleting files **does** free space once GC runs — pin
only affects whether *live* data stays cached locally.

## Snapshots

GC never deletes a block held by a snapshot. Snapshot holds are derived from each
snapshot's file manifests, independently of the live file set, so taking a snapshot
pins its blocks until the snapshot is deleted. See [Snapshots](snapshots.md).

## Trash (recycle bin)

If a share has the [trash](configuration.md#8-shares-exports) feature enabled
(`--enable-trash`, **off by default**), deleting a file **moves it to `#recycle`**
rather than removing it — its blocks stay referenced and are **not** GC-eligible.
Blocks are reclaimed only after the file leaves the trash:

```
delete file → moved to #recycle (blocks retained) → empty trash → real delete → GC reclaims
```

```bash
dfsctl trash status <share>    # items + bytes held in the recycle bin
dfsctl trash empty <share>     # purge — makes the blocks GC-eligible
dfsctl trash restore <share> <id>
```

## Monitoring

GC health is exported on the Prometheus endpoint (see
[Configuration § Metrics](configuration.md)):

| Metric | Meaning |
| --- | --- |
| `dittofs_gc_runs_total{result}` | GC passes, labelled `ok`/`error` |
| `dittofs_gc_running` | passes currently in flight (0 when idle) |
| `dittofs_gc_swept_objects_total` | objects reclaimed across both tiers |
| `dittofs_gc_freed_bytes_total` | bytes reclaimed across both tiers |
| `dittofs_gc_duration_seconds` | per-pass duration histogram |
| `dittofs_gc_last_run_timestamp_seconds` | wall-clock of the last completed pass |
| `dittofs_gc_stranded_rows_reaped_total` | rows reaped by reconcile/migration runs |

A `gc_last_run_timestamp_seconds` that stops advancing means the scheduler is wedged
or disabled; a climbing `gc_runs_total{result="error"}` means passes are failing —
check the server logs.

## Related

- [Configuration § GC](configuration.md) — every knob and env var.
- [FAQ](faq.md) — common questions about reclamation timing.
- [Architecture: mark-sweep](../internals/architecture.md#garbage-collection-mark-sweep)
  — the internal design.
