# DittoFS benchmark suite

This directory holds the **cross-system benchmark harness assets** — currently
the `fio` job files under [`workloads/`](workloads/). The harness that drives
them, `dfsbench` (`cmd/bench`), is being built incrementally; see
**issue #1602** and `.planning/2026-07-08-bench-harness-consolidation.md` for the
plan and the per-PR sequence.

The goal is ONE fio-based comparison harness that measures DittoFS against real
competitors (juicefs, s3ql, rclone-mount, s3fs) over NFSv3 / NFSv4.1 / SMB3 on a
single disposable Scaleway VM, with uniform fio metrics and a real server↔S3
bandwidth number for every system.

## Component microbenchmarks live with their code

Per-package `Benchmark*` functions (chunker, hash, block engine, metadata,
snapshot) run via `go test -bench` in their home package — not here:

```sh
go test -bench=. -benchmem -run=^$ ./pkg/block/engine/    # write/read-path engine benches
go test -bench=. -benchmem -run=^$ ./pkg/snapshot/        # snapshot create/manifest/verify scale
go test -bench=. -benchmem -run=^$ ./pkg/block/chunker/   # FastCDC throughput
```

Use `benchstat` to A/B two commits:

```sh
go test -bench=. -count=10 -run=^$ ./pkg/block/engine/ > before.txt
# ... checkout other commit ...
go test -bench=. -count=10 -run=^$ ./pkg/block/engine/ > after.txt
benchstat before.txt after.txt
```

## Layout

```
bench/
  README.md          # this file
  workloads/         # fio job files, consumed by the dfsbench harness (#1602)
cmd/bench/           # the dfsbench binary (under construction — #1602)
```
