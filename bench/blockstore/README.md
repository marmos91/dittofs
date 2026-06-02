# bench/blockstore

Workload drivers for `pkg/blockstore/engine`. Same exported functions
power both the `dfsbench blockstore` CLI subcommand and the Go
`Benchmark*` tests in this package.

## Workloads

| Name               | Block size | Working set            | Notes                                                                  |
|--------------------|------------|------------------------|------------------------------------------------------------------------|
| `sequential-write` | 8 MiB      | monotonic offset       | per-file offsets when `--working-set > 1`                              |
| `random-write`     | 4 KiB      | seeded 64 MiB file/N   | uniform-random offsets, payload re-fill per op                         |
| `dedup-heavy`      | 8 MiB      | N distinct payloadIDs  | same bytes across files; flush per op so rollup → CAS runs             |
| `mixed-rw`         | 4 KiB      | seeded 32 MiB file/N   | 50/50 read/write at random offsets                                     |
| `flush-churn`      | 4 KiB      | monotonic offset       | write→flush→write tight loop                                           |
| `mixed-ops-storm`  | 4 KiB      | 1 MiB files, ≥4/worker | concurrent WRITE/READ/LIST/DELETE (50/30/15/5); `--workers N`           |

Defaults match the legacy `cmd/blockstore-perf` shape so historical
results stay comparable. Override per-op size with `--block-size`.

`mixed-ops-storm` is the only concurrent workload. It partitions its
keyspace — a fixed stable set (pre-seeded, never deleted) backs READs and
WRITE-overwrites, while WRITE-create/DELETE churn a separate pool — so no
op ever races a concurrent delete of its own file. Each worker uses a PRNG
seeded from `(--seed, worker)`, so a given `(seed, workers)` reproduces the
same *multiset* of ops; goroutine interleaving (and thus the exact per-type
tally) is not deterministic at `--workers > 1`.

## Profiles & replay

Each run writes `cpu/heap/goroutine.pprof` + `seed.txt` to
`<profile-dir>/blockstore/[<phase>/]<workload>-<UTC-ts>/`. Add
`--full-profiles` to also enable the runtime mutex/block profilers and emit
`mutex.pprof` + `block.pprof` (full-fidelity sampling; off by default — it
adds per-event accounting overhead). `--phase baseline|post-fix` inserts a
parent dir so before/after captures sit side by side. `--replay <dir>`
reloads a recorded run's `seed.txt` (workload, ops, sizes, workers, seed,
remote, full-profiles) so a regression can be re-captured without retyping
flags; `--phase`/`--profile-dir` still pick the fresh output location.

```sh
./dfsbench blockstore --workload mixed-ops-storm --ops 50000 --workers 8 \
    --full-profiles --phase baseline --profile-dir .planning/v1.0-audit/blockstore/_profiles
./dfsbench blockstore --replay <baseline-run-dir> --phase post-fix \
    --profile-dir .planning/v1.0-audit/blockstore/_profiles
```

## Running

CLI (macro + pprof + real backend):

```sh
go build -o dfsbench ./cmd/bench
./dfsbench blockstore --workload sequential-write --ops 10000
./dfsbench blockstore --workload random-write   --ops 5000   --working-set 4
./dfsbench blockstore --workload mixed-rw       --ops 20000  --remote s3 --env-file .env
```

Go benchmarks (micro + benchstat-friendly):

```sh
go test -bench=. -benchmem -run=^$ ./bench/blockstore/
go test -bench=BenchmarkRandomWrite4KB -count=10 -run=^$ ./bench/blockstore/
```

## S3 remote

`--remote=s3` reads from the environment (or `--env-file`). See
`bench/README.md` for the full table. At minimum:

```sh
export AWS_S3_BUCKET=dittofs-bench
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_S3_REGION=us-east-1
```

For Localstack / MinIO add `AWS_ENDPOINT_URL` and
`AWS_S3_PATH_STYLE=true`.

## Library API

```go
import bs "github.com/marmos91/dittofs/bench/blockstore"

remoteStore, remoteClose, _ := bs.SetupRemote(ctx, bs.Opts{Remote: bs.RemoteMemory})
defer remoteClose()

engine, engineClose, _ := bs.NewEngine(tmpDir, remoteStore)
defer engineClose()

res, err := bs.RunWorkload(ctx, engine, bs.Opts{
    Workload:   bs.WorkloadRandomWrite,
    Ops:        5000,
    BlockSize:  4096,
    WorkingSet: 4,
    Seed:       1,
})
```

`RunWorkload` does no profiling and owns no engine lifecycle — the
caller wraps it for pprof or `b.N` timing as needed.

## Profile output

Every CLI run writes a seeded-workload profile set under a timestamped
directory. `cpu`, `heap`, and `goroutine` are always captured. Add
`--full-profiles` to also enable the runtime mutex + block profilers and
emit `mutex.pprof` + `block.pprof` — without that flag those two
profiles would be empty, since `runtime.SetMutexProfileFraction` /
`SetBlockProfileRate` default to off (see #671). The flag is opt-in
because the extra per-event accounting skews throughput.

`seed.txt` records the exact parameters (workload, ops, block size,
working set, workers, seed, remote, full-profiles) for deterministic
replay via `--replay <dir>`.

```
_profiles/blockstore/<workload>-<UTC-timestamp>/
  cpu.pprof
  heap.pprof
  goroutine.pprof
  mutex.pprof        # only with --full-profiles
  block.pprof        # only with --full-profiles
  seed.txt
```

```sh
# Full set under the seeded sequential-write workload:
./dfsbench blockstore --workload sequential-write --ops 2000 \
    --block-size 65536 --working-set 4 --full-profiles

go tool pprof -http :8080 _profiles/blockstore/sequential-write-*/cpu.pprof
go tool pprof -top         _profiles/blockstore/sequential-write-*/mutex.pprof
```

These captures are run on demand only — the `go test` suite never
constructs a profile session, so normal CI is unaffected.
