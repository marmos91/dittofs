# bench/e2e (stub)

Status: **not yet implemented as a Go library** — existing shell-based
E2E orchestration lives in sibling directories:

- `bench/infra/`     — Pulumi stack for cloud bench VMs
- `bench/workloads/` — `fio` job files (`seq-write.fio`, `rand-read-4k.fio`, …)
- `bench/scripts/`   — shell glue (`run-all.sh`, `s3-baseline.sh`)
- `bench/analysis/`  — `benchstat` + CSV post-processing

The `bench e2e` Cobra subcommand will eventually orchestrate those
runs from Go: spin up infra, push the build, drive workloads on the
client VM, pull results back, and emit JSON.

## Scope

Real NFS / SMB clients driving the DittoFS server. Goal: catch
regressions that the engine-level micro-bench misses — kernel page
cache, real RPC framing, scheduler interference, TCP retransmits,
real-disk and real-network latency.

## Intended workloads

- `fio` — sequential / random / mixed at multiple block sizes and IO depths
- `iozone` — record-size sweep, mmap, fsync gates
- `smbtorture-perf` — SMB2/3 CREATE / lease / lock storms
- `nfs-stress` — NFSv3 + NFSv4.0 + NFSv4.1 conformance-grade load

## Output

JSON-per-run with a stable schema so dashboards can ingest:

```json
{
  "workload":  "fio.rand-read-4k",
  "client":    {"os": "linux-6.6", "nfs": "kernel"},
  "duration_s": 60.0,
  "iops":       1234,
  "bw_mibps":   4.82,
  "lat_p50_us": 412,
  "lat_p99_us": 5120
}
```

## Tracking

Not yet filed.
