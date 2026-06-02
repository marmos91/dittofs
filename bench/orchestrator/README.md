# bench/orchestrator

Runs a manifest of DittoFS benchmark workloads and emits a **versioned,
machine-readable result document** so performance can be tracked over time and
gated in CI.

This is an orchestration layer on top of the existing `bench/blockstore`
primitives — it does **not** implement workloads. It runs the blockstore
workloads (`sequential-write`, `random-write`, `dedup-heavy`, `mixed-rw`,
`mixed-ops-storm`, the concurrent (a)–(d) workloads, `walk`/`delete`/`gc`),
captures the shared pprof envelope per workload, and collects the numbers into
one JSON document.

The package is deliberately free of engine/pprof/runtime dependencies: the
caller injects a `WorkloadRunner`. `cmd/bench orchestrate` wires the real
engine-backed runner; tests inject a fast fake. Run metadata (timestamp, run
ID, git SHA) is injected too — the run loop never calls `time.Now()` or reads
the environment, so runs are reproducible and tests are deterministic.

## Run

```bash
go build -o dfsbench ./cmd/bench

# Default manifest (fast, in-process, memory remote — no S3/network):
dfsbench orchestrate --out result.json --summary

# Pin the timestamp/run-id and git SHA for a reproducible capture:
dfsbench orchestrate --out result.json \
  --timestamp 2026-06-02T12:00:00Z --run-id baseline-1 --git-sha "$(git rev-parse HEAD)"

# Custom manifest + baseline/post-fix profile nesting:
dfsbench orchestrate --manifest my.json --phase baseline --profile-dir _profiles
dfsbench orchestrate --manifest my.json --phase post-fix --profile-dir _profiles

# Compare two result documents (exits non-zero on a regression — CI-gateable):
dfsbench orchestrate \
  --compare-baseline baseline.json --compare-candidate post-fix.json \
  --compare-threshold 10
```

Flags: `--out` (write JSON to a path; default stdout), `--summary` (human table
to stderr), `--full-profiles` (also capture mutex+block per workload),
`--profile-dir`/`--phase` (where pprof lands — same layout as the `blockstore`
subcommand, so before/after captures sit side by side).

### Manifest format

A manifest is a JSON list of workload entries. Names must be unique (they key
the result document). Omitted numeric fields fall back to the workload's
defaults (e.g. block size resolves to 8 MiB for sequential/dedup, 4 KiB
otherwise).

```json
{
  "workloads": [
    { "name": "seq-write",    "workload": "sequential-write", "ops": 2000, "block_size": 65536, "working_set": 4, "seed": 1, "remote": "memory" },
    { "name": "storm-4w",     "workload": "mixed-ops-storm",  "ops": 4000, "workers": 4, "mix": "50,30,15,5", "seed": 1, "remote": "memory" }
  ]
}
```

Omit `--manifest` to use the built-in default set (a quick five-workload
memory-remote snapshot suitable for a CI gate).

## Result schema

The document is versioned via the top-level `schema_version` field.

```json
{
  "schema_version": 1,
  "run_id": "baseline-1",
  "timestamp": "2026-06-02T12:00:00Z",
  "git_sha": "459ebbae...",
  "system": { "os": "darwin", "arch": "arm64", "num_cpu": 10, "go_version": "go1.26.3", "hostname": "amaterasu" },
  "outcome": "completed",
  "workloads": {
    "seq-write": {
      "outcome": "completed",
      "params": { "name": "seq-write", "workload": "sequential-write", "ops": 2000, "block_size": 65536, "working_set": 4, "seed": 1, "remote": "memory" },
      "metrics": {
        "duration_ns": 28311948292,
        "ops": 2000,
        "ns_per_op": 14155974.146,
        "ops_per_sec": 70.64,
        "bytes": 131072000,
        "bytes_per_sec": 4629564.83
      },
      "profile_paths": [
        ".../seq-write-.../cpu.pprof",
        ".../seq-write-.../heap.pprof",
        ".../seq-write-.../goroutine.pprof"
      ]
    }
  }
}
```

Fields:

- `schema_version` (int) — read this first; see the version contract below.
- `run_id`, `timestamp` (RFC3339 UTC), `git_sha` — injected run metadata.
- `system` — host/runtime the run executed on (so results from different
  machines are not silently compared).
- `outcome` — one of `completed` (every workload succeeded), `partial` (some
  workload failed; surviving results are still present), `aborted` (the run was
  halted before finishing — e.g. an invalid manifest; `abort_reason` carries
  the cause).
- `workloads` — map keyed by manifest entry name. Each entry carries its own
  `outcome`, the echoed `params`, `metrics` (omitted when the workload did not
  complete), and `profile_paths` (the captured pprof files; empty when
  profiling is disabled, e.g. under test).

A CI gate asserts `outcome == "completed"` and that every workload completed.

## Version contract

- `schema_version` is the **first and only** field a consumer must read before
  interpreting anything else. It is an integer and is always present.
- **Additive (minor) changes** — new optional fields — do **not** bump
  `schema_version`. Consumers must ignore unknown fields (Go's `encoding/json`
  does this by default) so they keep working across additive changes.
- A **bump** signals a **breaking** change (a field removed, renamed, or its
  meaning/units changed). A consumer that does not recognize a document's
  `schema_version` must **refuse** to interpret the numbers rather than
  silently mis-read them.
- Use `orchestrator.DecodeDocument` (or `CheckVersion`) to read a document — it
  verifies `schema_version` before handing the document back. Compare mode and
  any CI gate go through it.

## Follow-ups (not built here)

- **Per-area wrappers** — NFS / SMB / lock / runtime / metadata workload
  runners that plug into the same manifest + schema. The `WorkloadRunner`
  injection point and the per-workload `params`/`metrics`/`profile_paths` shape
  are designed to accept them without a schema bump.
- **Latency percentiles** — `metrics` currently reports duration/ops/throughput;
  p50/p95/p99 are an additive field (no version bump) once a runner records
  per-op timings.
