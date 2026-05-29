# bench/metadata (stub)

Status: **not yet implemented**.

## Scope

Benchmark the metadata store: directory listings, rename, hard / symbolic
links, ACL evaluation, and contention under concurrent operations. Covers
both the in-memory store and persistent backends (badger, sqlite, postgres)
behind the same `pkg/metadata.Store` interface.

## Intended workloads

- `readdir-large` — listing throughput vs directory entry count
- `rename-churn` — rename storm across a deep tree
- `hardlink-create` — link create rate vs existing-link count
- `acl-eval` — ACL check fast path vs cold path
- `meta-contention` — N concurrent writers against the same parent

## Library layout (when wired)

```
bench/metadata/
  doc.go
  fixture.go     // store-agnostic engine fixture per backend
  workloads.go   // RunWorkload(ctx, store, opts)
  workloads_test.go
```

## Running (once implemented)

```sh
./dfsbench metadata --workload readdir-large --entries 1000000 --backend badger
./dfsbench metadata --workload meta-contention --writers 32
```

## Tracking

Not yet filed.
