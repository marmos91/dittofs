# DittoFS Benchmark Infrastructure Plan

## Context

DittoFS needs real-world performance benchmarks to guide optimization work and compare against competitors. The existing `bench/` directory has Docker Compose infrastructure for comparative benchmarking (9 profiles), but Docker adds container/VM overhead that distorts results, the fio workload files are empty, and there's no built-in benchmark command or automation.

**New approach**: Replace the Docker Compose suite with bare-metal Scaleway VMs provisioned via Pulumi (Go SDK), running one competitor at a time on a clean VM restored from a base snapshot. A new `dfsctl bench` command provides consistent Go-native measurement across any mounted filesystem — DittoFS or competitor. This gives us:
- No container overhead — real network, real disk I/O
- Clean environment per competitor — no cross-contamination between tests
- Same measurement tool for all systems — fair comparison
- Built-in comparison via `dfsctl bench compare`
- Reproducible infrastructure via Pulumi IaC

---

## Phase A: `pkg/bench/` — Benchmark Engine Library

Core, reusable package with no Cobra dependency. Pure I/O benchmarking logic.

### New files:

**`pkg/bench/types.go`** — Data structures
- `WorkloadType` enum: `seq-write`, `seq-read`, `rand-write`, `rand-read`, `metadata`
- `Config` struct: Path, Threads (default 4), FileSize (default 1GiB), BlockSize (default 4KiB), Duration (default 60s), MetaFiles (default 1000), Workloads
- `Result` struct: Timestamp, System label, Path, Config summary, []WorkloadResult, total duration
- `WorkloadResult` struct: ThroughputMBps, IOPS, OpsPerSec, LatencyP50/P95/P99/Avg (microseconds), TotalOps, TotalBytes, Errors

**`pkg/bench/stats.go`** — Percentile computation
- `computePercentiles([]time.Duration) → (p50, p95, p99, avg float64)` — sort-based percentile calculation
- `FormatSize(int64) → string` — bytes to human-readable (e.g., "1.0 GiB")
- `ParseSize(string) → (int64, error)` — human-readable to bytes

**`pkg/bench/workload_seq.go`** — Sequential I/O
- `runSeqWrite(ctx)`: Each thread writes a file (`_dfsctl_bench/seq_write_{N}.dat`) in 1MiB chunks, records per-write latency
- `runSeqRead(ctx)`: Reads back files in 1MiB chunks, uses `F_NOCACHE` on macOS (no `O_DIRECT` on darwin)

**`pkg/bench/workload_rand.go`** — Random I/O
- `runRandWrite(ctx)`: Pre-creates files, random seek + write BlockSize bytes for Duration, reports IOPS
- `runRandRead(ctx)`: Random seek + read BlockSize bytes for Duration

**`pkg/bench/workload_meta.go`** — Metadata operations
- `runMetadata(ctx)`: Three phases — create MetaFiles small files (128B), stat all, delete all. Reports ops/sec per phase and combined

**`pkg/bench/runner.go`** — Orchestrator
- `NewRunner(cfg Config, progress ProgressFunc) → *Runner`
- `Run(ctx) → (*Result, error)`: Creates `_dfsctl_bench/` subdir, runs workloads sequentially, collects results, cleans up
- `Validate() → error`: Checks path exists and is writable
- Each workload uses `cfg.Threads` goroutines internally
- Latency stored as `[]time.Duration` (~10MB for 60s at 20K IOPS — acceptable)

**`pkg/bench/runner_test.go`** — Unit tests against `os.TempDir()`

---

## Phase B: `cmd/dfsctl/commands/bench/` — CLI Commands

Follows existing Cobra patterns from `cmd/dfsctl/commands/root.go`.

### New files:

**`cmd/dfsctl/commands/bench/bench.go`** — Parent command
- Exports `var Cmd` with `Use: "bench"`, `Short: "Run filesystem benchmarks"`
- Registers `runCmd` and `compareCmd` subcommands

**`cmd/dfsctl/commands/bench/run.go`** — `dfsctl bench run PATH`
- Flags: `--threads` (4), `--file-size` (1GiB), `--block-size` (4KiB), `--duration` (60s), `--workload` (comma-separated, default all), `--system` (label), `--save` (JSON output file), `--meta-files` (1000)
- Does NOT require API auth — operates purely on filesystem
- Respects global `-o` flag for output format (table/json/yaml)
- Progress reporting to stderr during execution
- Table output example:
  ```
  WORKLOAD      THROUGHPUT    IOPS      P50       P95       P99
  seq-write     312.4 MB/s   -         187 us    342 us    891 us
  seq-read      489.1 MB/s   -         121 us    234 us    567 us
  rand-write    -            12,341    312 us    892 us    2,341 us
  rand-read     -            18,923    201 us    567 us    1,234 us
  metadata      -            -         45 us     123 us    456 us
  ```

**`cmd/dfsctl/commands/bench/compare.go`** — `dfsctl bench compare FILE [FILE...]`
- Loads 2+ JSON result files, renders side-by-side comparison table
- Highlights best/worst values in color (table mode)

**`cmd/dfsctl/commands/bench/table.go`** — `TableRenderer` implementations for results and comparison output

### Modified file:

**`cmd/dfsctl/commands/root.go`** — Add `benchcmd "...commands/bench"` import and `rootCmd.AddCommand(benchcmd.Cmd)`

---

## Phase C: fio Workload Profiles

Create `.fio` job files in `bench/workloads/`. Uses fio's native `${VAR:-default}` env var substitution.

### New files:

| File | Workload | Block Size | Notes |
|------|----------|------------|-------|
| `bench/workloads/seq-write.fio` | Sequential write | 1M | `direct=1`, `fsync_on_close=1` |
| `bench/workloads/seq-read.fio` | Sequential read | 1M | `direct=1` |
| `bench/workloads/rand-read-4k.fio` | Random read | 4K | `iodepth=32` |
| `bench/workloads/rand-write-4k.fio` | Random write | 4K | `iodepth=32`, `fsync_on_close=1` |
| `bench/workloads/mixed-rw.fio` | Mixed 70/30 R/W | 4K | `iodepth=16` |
| `bench/workloads/metadata.fio` | Small file create | 4K | `nrfiles=1000`, `create_on_open=1` |

All job files use configurable env vars: `$FIO_ENGINE` (libaio/posixaio), `$BENCH_THREADS` (4), `$BENCH_RUNTIME` (60), directory `/mnt/bench`, file size 1G.

### Delete:
- `bench/workloads/.gitkeep` (replaced by actual files)

---

## Phase D: Infrastructure via Pulumi (`bench/infra/`)

Pulumi Go program managing Scaleway VMs with snapshot-based isolation. Each competitor runs on a pristine VM restored from a base snapshot — zero cross-contamination.

### Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│  Pulumi "base" stack (run once)                                     │
│                                                                     │
│  1. Create VPC + private network                                    │
│  2. Create client VM (persistent across all tests)                  │
│     - Install: fio, iperf3, nfs-common, cifs-utils, jq, dfsctl     │
│  3. Create base server VM                                           │
│     - Install: Go 1.24, iperf3, common deps                        │
│  4. Snapshot base server VM → "dittofs-bench-base"                  │
│  5. Destroy base server VM (snapshot is kept)                       │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│  Pulumi "bench" stack (per competitor, cycled by orchestrator)       │
│                                                                     │
│  For each competitor:                                               │
│  1. Create server VM from "dittofs-bench-base" snapshot             │
│  2. Run competitor install script (cloud-init or SSH provisioner)   │
│  3. Client mounts, runs dfsctl bench + fio, saves results          │
│  4. Destroy server VM                                               │
│  → Next competitor: repeat from step 1                              │
└─────────────────────────────────────────────────────────────────────┘

VM A (server, ephemeral)                   VM B (client, persistent)
├── One competitor at a time:              ├── dfsctl bench
│   - DittoFS (badger+fs)   :12049        ├── fio workloads
│   - DittoFS (badger+s3)   :12049        ├── iperf3
│   - kernel NFS            :2049         └── results/
│   - NFS-Ganesha           :2049
│   - RClone NFS            :2049
│   - Samba                 :445
│   - JuiceFS              :FUSE
├── iperf3 server
└── Private VPC ←→ VM B
```

### Directory structure:

```
bench/infra/
├── go.mod                      # Separate Go module (pulumi + scaleway SDK)
├── go.sum
├── Pulumi.yaml                 # Project: dittofs-bench
├── Pulumi.base.yaml            # Stack config for base infra (no secrets)
├── Pulumi.bench.yaml           # Stack config for benchmark runs (no secrets)
├── main.go                     # Entry point — reads stack name, dispatches
├── base.go                     # "base" stack: VPC, client VM, base snapshot
├── bench.go                    # "bench" stack: server VM from snapshot
├── network.go                  # VPC + private network resources
├── systems.go                  # Competitor definitions (name, install script, port, mount opts)
├── scripts/                    # Cloud-init / provisioning scripts
│   ├── base-server.sh          # Common deps (Go, iperf3, build tools)
│   ├── client.sh               # Client deps (fio, iperf3, nfs-common, etc.)
│   ├── dittofs-badger-fs.sh    # DittoFS with BadgerDB + filesystem
│   ├── dittofs-badger-s3.sh    # DittoFS with BadgerDB + S3
│   ├── kernel-nfs.sh           # apt install nfs-kernel-server + configure
│   ├── ganesha.sh              # apt install nfs-ganesha + configure
│   ├── rclone.sh               # Install rclone, configure S3 NFS serve
│   ├── samba.sh                # apt install samba + configure
│   └── juicefs.sh              # Install JuiceFS, format + FUSE mount
└── .gitignore                  # Ignore Pulumi state files, .hosts, etc.
```

### Security — credentials kept out of repo:

| Credential | Source | Never committed |
|------------|--------|-----------------|
| Scaleway access key + secret | `SCW_ACCESS_KEY` / `SCW_SECRET_KEY` env vars | Pulumi reads from env |
| Scaleway project ID | `SCW_DEFAULT_PROJECT_ID` env var | Pulumi reads from env |
| SSH private key | `~/.ssh/id_ed25519` (local) | Referenced by path, never embedded |
| S3 credentials (for S3 backends) | Pulumi config secrets (`pulumi config set --secret`) | Encrypted in Pulumi state |
| Pulumi state | Local backend (`file://~/.pulumi`) or Pulumi Cloud | Not in repo |

**`.gitignore` additions:**
```
# Pulumi
bench/infra/.pulumi/
bench/infra/Pulumi.*.yaml      # Stack configs may contain encrypted secrets
!bench/infra/Pulumi.yaml       # Project file is safe to commit

# Hosts file generated by provisioning
bench/infra/.hosts
```

**Pulumi stack config pattern** (secrets encrypted, non-secrets plain):
```yaml
# Pulumi.base.yaml — safe to commit (no secrets)
config:
  dittofs-bench:zone: fr-par-1
  dittofs-bench:vmType: POP2-HC-4C-8G
  dittofs-bench:image: ubuntu_noble
  dittofs-bench:sshKeyId: <your-scw-ssh-key-id>
```

```bash
# Secrets set via CLI (encrypted in state, never plaintext in files)
pulumi config set --secret s3AccessKey AKIAIOSFODNN7EXAMPLE
pulumi config set --secret s3SecretKey wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

### Key Pulumi resources:

| Resource | Stack | Purpose |
|----------|-------|---------|
| `scaleway:VPC` | base | Private network for server↔client |
| `scaleway:Instance` (client) | base | Persistent client VM |
| `scaleway:Instance` (base-server) | base | Temporary — snapshotted then destroyed |
| `scaleway:Snapshot` | base | "dittofs-bench-base" — restored per competitor |
| `scaleway:Instance` (server) | bench | Ephemeral — created from snapshot, destroyed after test |

---

## Phase E: Benchmark Orchestrator (`bench/scripts/run-all.sh`)

Shell script that drives the full benchmark cycle. Calls Pulumi for VM lifecycle and `dfsctl bench` for measurement.

### Flow:

```
1. pulumi up --stack base                    # One-time: VPC + client + base snapshot
2. Run iperf3 baseline (client → server)
3. Run local disk fio baseline on client
4. For each competitor:
   a. pulumi up --stack bench -c system=<name>   # Server VM from snapshot + install competitor
   b. SSH: wait for competitor service ready
   c. Client: mount /mnt/bench (NFS/SMB/FUSE)
   d. Client: sync && echo 3 > /proc/sys/vm/drop_caches
   e. Client: dfsctl bench run /mnt/bench --system <name> --save results/<name>.json
   f. Client: run fio workloads, save results
   g. Client: umount /mnt/bench
   h. pulumi destroy --stack bench --yes          # Destroy server VM
5. dfsctl bench compare results/*.json          # Final comparison
6. Optional: pulumi destroy --stack base --yes  # Tear down everything
```

### New files:

**`bench/scripts/run-all.sh`** — Full orchestrator
- Reads system definitions from `bench/infra/systems.go` (or a shared config)
- Passes `system` config to Pulumi bench stack to select competitor install script
- Handles different mount types per system (NFS port options, SMB credentials, FUSE bind)
- Supports `--system <name>` to benchmark a single competitor
- Supports `--skip-baseline` to skip iperf3/disk tests
- Supports `--keep-base` to skip base stack teardown
- Logs progress to stderr
- Copies results from client VM to local `bench/results/`

**`bench/scripts/lib/common.sh`** — Shared utilities (logging, timer, SSH helpers)
- Reuses patterns from existing `bench/scripts/lib/common.sh` before it's deleted

---

## Phase F: Analysis Pipeline (`bench/analysis/`)

Result aggregation and reporting from collected benchmark data.

### New files:

**`bench/analysis/aggregate.sh`** — Merge results
- Reads all `results/*.json` (dfsctl bench output)
- Reads all `results/fio_*.json` (fio output)
- Produces `results/summary.json` with all systems side-by-side

**`bench/analysis/report.sh`** — Generate markdown report
- Reads `results/summary.json`
- Outputs markdown tables comparing all systems
- Sections: sequential throughput, random IOPS, metadata ops/sec, latency percentiles
- Highlights best values per metric

---

## Phase G: Remove Old Docker Compose Suite

Delete the old `bench/` Docker Compose infrastructure, replaced by Pulumi + native VM benchmarking.

### Delete:
- `bench/docker-compose.yml`
- `bench/docker/` (Dockerfile.dittofs)
- `bench/configs/` (dittofs/, ganesha/, kernel-nfs/, rclone/, samba/)
- `bench/scripts/` (old: bootstrap-dittofs.sh, check-prerequisites.sh, clean-all.sh, lib/common.sh)
- `bench/Makefile`
- `bench/README.md`
- `bench/.env.example`
- `bench/.gitignore` (replaced by new one)
- `bench/analysis/.gitkeep`

### Keep / replace:
- `bench/workloads/` (new fio files from Phase C)
- `bench/infra/` (new Pulumi program from Phase D)
- `bench/scripts/` (new orchestrator + lib from Phase E)
- `bench/analysis/` (new analysis scripts from Phase F)
- `bench/results/` (gitignored output)

---

## Implementation Order

1. **Phase A** (pkg/bench/) — engine library + tests
2. **Phase B** (CLI) — dfsctl bench run + compare commands
3. **Phase C** (fio) — workload profiles
4. **Phase D** (infra) — Pulumi program with Scaleway VMs + snapshot isolation
5. **Phase E** (orchestrator) — run-all.sh cycling through competitors via Pulumi
6. **Phase F** (analysis) — result aggregation + markdown report
7. **Phase G** (cleanup) — remove old Docker Compose suite

Phases A+B are core Go code (main module). Phase D is a separate Go module (Pulumi). C+E+F are config/scripts. G is deletion.

## Verification

1. `go test ./pkg/bench/...` — unit tests pass against temp dir
2. `go build ./cmd/dfsctl/` — binary builds
3. `dfsctl bench run /tmp --threads 2 --file-size 10MiB --duration 5s` — quick local smoke test
4. `dfsctl bench run /tmp -o json | jq .` — JSON output is valid
5. `fio bench/workloads/seq-write.fio --directory=/tmp --parse-only` — fio job files parse correctly
6. `cd bench/infra && pulumi preview --stack base` — Pulumi program compiles and plans
7. `cd bench/infra && pulumi up --stack base` — base infra provisions successfully
8. `bench/scripts/run-all.sh --system kernel-nfs` — single competitor benchmark completes
9. `dfsctl bench compare results/*.json` — comparison table renders correctly
10. No Scaleway credentials, SSH keys, or secrets committed to repo
11. Old Docker Compose files are removed
