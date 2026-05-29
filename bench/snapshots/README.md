# bench/snapshots (stub)

Status: **not yet implemented**.

## Scope

Benchmark reference-based CAS snapshots: create, restore, GC interaction,
and overhead on the live write path. Replaces the deprecated v0.13.0
backup harness.

## Intended workloads

- `snapshot-create` — metadata-dump + hash-manifest time vs file count
- `snapshot-restore` — restore overhead vs snapshot age and live churn
- `snapshot-gc-hold` — measure GC throttle under N concurrent snapshots
- `snapshot-live-overhead` — write throughput with a snapshot pinned

## Library layout (when wired)

```
bench/snapshots/
  doc.go
  fixture.go     // engine + snapshot store + hold provider
  workloads.go   // RunWorkload(ctx, bs, snap, opts)
  workloads_test.go
```

## Running (once implemented)

```sh
./dfsbench snapshots --workload snapshot-create --files 100000
./dfsbench snapshots --workload snapshot-restore --age 7d
```

## Tracking

Not yet filed.
