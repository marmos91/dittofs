# bench/gc (stub)

Status: **not yet implemented**.

## Scope

Benchmark the reference-counted blockstore garbage collector and
its interaction with live workloads. The CAS write path leaves
unreferenced blocks behind whenever an in-place overwrite drops the
last reference to a chunk; GC reclaims them.

## Intended workloads

- `mark-sweep` — wall clock vs working-set size and dead-ratio
- `mixed-with-gc` — concurrent write workload with GC running every N ms
- `gc-pauseless` — measure max p99 latency stall introduced by GC sweeps
- `gc-storm` — sustained delete bursts, ensure throughput recovers

## Library layout (when wired)

```
bench/gc/
  doc.go
  fixture.go     // shared engine fixture (likely embeds bench/blockstore)
  workloads.go   // RunWorkload(ctx, bs, opts) over the workloads above
  workloads_test.go
```

## Running (once implemented)

```sh
./dfsbench gc --workload mark-sweep --garbage-ratio 0.4 --ops 1000
```

## Tracking

Not yet filed.
