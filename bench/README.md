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

## Using `dfsbench` (local mode)

The harness runs `fio` against a mounted filesystem and prints a comparison
table; each cell's metrics are written as a JSON file under `--results` so
`--resume` re-runs skip completed cells.

`fio` must be installed and on `PATH`. The dev shell provides it — `nix develop`
(fio is in `commonBuildInputs`) — otherwise install it yourself (`brew install
fio`, `apt-get install fio`, …).

```sh
go build -o dfsbench ./cmd/bench

./dfsbench list                                   # workloads + size classes
./dfsbench run --smoke                            # self-contained tiny run (CI, no secrets)
./dfsbench run --local --target /mnt/dittofs      # fio a filesystem you mounted
./dfsbench run --local --target /mnt/juicefs \
    --workloads rand-read-4k --sizes large        # one cell
./dfsbench run --local --target /mnt/x --resume   # skip cells already recorded
./dfsbench report --results ./bench-results       # re-render the table from saved JSON
```

Size classes: `small` = 64 KiB, `medium` = 1 MiB, `large` = 1 GiB. Sequential
workloads use a 1 MiB block, so they need `medium` or larger (fio requires file
size ≥ block size). Cloud provisioning, competitor backends, protocol
re-export, and S3-byte/ctxsw metering land in follow-up PRs (see #1602).

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
