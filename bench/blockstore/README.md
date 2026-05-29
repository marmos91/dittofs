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

Defaults match the legacy `cmd/blockstore-perf` shape so historical
results stay comparable. Override per-op size with `--block-size`.

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

```
_profiles/blockstore/<workload>-<UTC-timestamp>/
  cpu.pprof
  heap.pprof
```

```sh
go tool pprof -http :8080 _profiles/blockstore/random-write-*/cpu.pprof
```
