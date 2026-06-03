# DittoFS benchmark suite

Quick-start for the `dfsbench` harness. The canonical performance
documentation — results, how to run, perf gates, snapshot scale limits — lives
in **[`docs/BENCHMARKS.md`](../docs/BENCHMARKS.md)**. This file is the developer
quick-start and a map of the `bench/` packages.

```sh
go build -o dfsbench ./cmd/bench

./dfsbench blockstore --workload sequential-write --ops 10000   # one workload + pprof
./dfsbench orchestrate --out result.json --summary              # manifest → versioned JSON
./dfsbench remote --stack bench --dry-run                       # plan a remote Scaleway run
go test -bench=. -benchmem -run=^$ ./bench/blockstore/          # micro via Go benchmarks
```

## Why one suite

Pre-1.0 DittoFS shipped per-workload one-off binaries (`cmd/blockstore-perf`,
ad-hoc shell loops) and per-package `Benchmark*` tests with overlapping
fixture code. The unified `cmd/bench` Cobra orchestrator plus the
`bench/<area>/` library packages give us:

- **One CLI** (`dfsbench <area> <flags>`) for macro runs, real-backend exercise,
  and pprof capture.
- **Library workloads** (`bench/<area>.RunWorkload(...)`) callable from both
  the CLI and per-package Go `Benchmark*` tests — no duplicate fixture or
  per-op shape.
- **Shell + make** glue (`bench/scripts/`, `bench/infra/`, `Makefile`) for the
  external-client E2E layer that drives `fio`, `iozone`, and
  `smbtorture-perf` against real NFS / SMB clients.

## Areas

| Area        | Scope                                                       | Status |
|-------------|-------------------------------------------------------------|--------|
| blockstore  | local FSStore + remote + Syncer + engine                    | done   |
| gc          | reference counting + sweep                                  | stub   |
| snapshots   | reference-CAS snapshot create / verify / manifest scale     | done   |
| metadata    | listings, rename, hard links, ACL eval                      | stub   |
| adapters    | NFS XDR + SMB2/3 framing perf (no real network)             | stub   |
| e2e         | real NFS / SMB clients driving fio / iozone / smbtorture    | stub   |

Stub areas print `<area> benchmarks: not yet implemented. See
bench/<area>/README.md` and exit 0 so CI / scripts can call them
unconditionally during the migration.

## Running

Build the orchestrator binary:

```sh
go build -o dfsbench ./cmd/bench
```

Macro run with pprof capture:

```sh
./dfsbench blockstore --workload sequential-write --ops 10000
./dfsbench blockstore --workload random-write   --ops 5000 --remote=s3 --env-file ./.env
```

Output shape (unchanged from the legacy `cmd/blockstore-perf`):

```
workload=sequential-write ops=10000 dur=1234.567ms ops_per_sec=8101.23 bytes_per_sec=67934567.89 profiles=_profiles/blockstore/sequential-write-20260529T120000Z
stats before/after: files=0/1 dirty=0/3 disk=0/268435456 pending=0 completed=14
```

Micro runs via `go test -bench` against the library directly:

```sh
go test -bench=. -benchmem -run=^$ ./bench/blockstore/
```

### Orchestrated runs (versioned result JSON)

`dfsbench orchestrate` runs a manifest of workloads and emits a versioned,
machine-readable result document (schema_version 1) — run metadata, host/CPU
info, and per-workload ns/op / throughput / pprof paths — plus a compare mode
that flags ns/op regressions between two runs. See
[`bench/orchestrator/README.md`](orchestrator/README.md) for the manifest
format, schema, and version contract.

```sh
./dfsbench orchestrate --out result.json --summary
./dfsbench orchestrate --compare-baseline base.json --compare-candidate new.json
```

### Remote runs (Scaleway over SSH)

`dfsbench remote` is the Go replacement for the old `scripts/run-bench.sh` /
`bench/scripts/run-all.sh` bash glue. It reads the `bench/infra` Pulumi stack
outputs (public IP for SSH, private-network IP for the mount), scp's a prebuilt
`dfsbench` binary to the host, runs `orchestrate` over SSH, and pulls the result
JSON back. See [`docs/BENCHMARKS.md`](../docs/BENCHMARKS.md#remote-runs-scaleway)
for required env and the full flow; `--dry-run` prints the plan without touching
the host.

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dfsbench.linux ./cmd/bench
./dfsbench remote --stack bench --binary dfsbench.linux --ssh-key ~/.ssh/id_rsa --out remote.json
```

Use `benchstat` to A/B compare two commits:

```sh
go test -bench=. -count=10 -run=^$ ./bench/blockstore/ > before.txt
# ... checkout other commit ...
go test -bench=. -count=10 -run=^$ ./bench/blockstore/ > after.txt
benchstat before.txt after.txt
```

## Profile inspection

CPU and heap profiles land under `_profiles/<area>/<workload>-<timestamp>/`:

```sh
go tool pprof -http :8080 _profiles/blockstore/sequential-write-*/cpu.pprof
go tool pprof -http :8080 _profiles/blockstore/sequential-write-*/heap.pprof
```

## S3 backend env vars

The `--remote=s3` flag drives `pkg/blockstore/remote/s3`. Either set the
following in the environment or pass them via `--env-file ./.env`:

| Variable                  | Required | Notes                                                |
|---------------------------|----------|------------------------------------------------------|
| `AWS_S3_BUCKET`           | yes      | bucket name                                          |
| `AWS_ACCESS_KEY_ID`       | yes      | access key ID                                        |
| `AWS_SECRET_ACCESS_KEY`   | yes      | secret access key                                    |
| `AWS_S3_REGION`           | no       | AWS SDK default if empty (us-east-1 fallback)        |
| `AWS_ENDPOINT_URL`        | no       | for Localstack / MinIO                               |
| `AWS_S3_KEY_PREFIX`       | no       | prepended to every block key                         |
| `AWS_S3_MAX_RETRIES`      | no       | integer; SDK default if unset                        |
| `AWS_S3_PATH_STYLE`       | no       | bool; defaults true when `AWS_ENDPOINT_URL` is set   |

Real-env values always win over `--env-file` so CI secret injection
behaves as expected.

## Make targets

```sh
make build-bench       # builds the cmd/bench binary
make bench-blockstore  # runs the blockstore Go benchmarks
make bench-all         # umbrella; stubs other areas for now
```

## Layout

```
cmd/bench/
  main.go            # cobra root, global flags, env-file pre-run
  blockstore.go      # implemented subcommand
  orchestrate.go     # manifest runner → versioned result JSON + compare
  remote.go          # drive a run on a Scaleway host over SSH
  snapshots.go       # snapshot scale workloads
  stubs.go           # gc / metadata / adapters / e2e stubs

bench/
  README.md          # this file (quick-start)
  blockstore/
    fixture.go       # NewEngine(baseDir, remoteStore)
    remote.go        # SetupRemote(ctx, opts) — memory | s3
    latency.go       # per-op LatencyRecorder threaded through the runners
    workloads.go     # Opts, Result, RunWorkload, exported workloads
  orchestrator/      # versioned schema, manifest, runner, compare, latency math
  remote/            # SSH/scp/Pulumi-output ports + the remote orchestrator
  snapshots/ README.md
  gc/        README.md   (stub)
  metadata/  README.md   (stub)
  adapters/  README.md   (stub)
  e2e/       README.md   (points at existing infra/ + workloads/ + scripts/)
  infra/     Pulumi stack for cloud benchmark VMs
  workloads/ fio job files driven by E2E runs
  scripts/   shell glue for E2E + analysis
  analysis/  benchstat / CSV post-processing
```
