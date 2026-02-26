# Phase 33: Benchmark Infrastructure - Context

**Gathered:** 2026-02-26
**Status:** Ready for planning

<domain>
## Phase Boundary

Create the `bench/` directory structure with Docker Compose profiles, DittoFS configuration, competitor service definitions, prerequisites check, and supporting infrastructure. This is the foundation all other benchmark phases (34-38) build on. Does NOT include workload definitions, orchestration scripts, analysis pipeline, or profiling — those are separate phases.

</domain>

<decisions>
## Implementation Decisions

### Docker Compose Strategy
- Single `docker-compose.yml` with profiles (not separate files per system)
- Shared infrastructure (LocalStack S3, PostgreSQL) activated per-profile via `depends_on` — not always-on
- Sequential benchmarking: one system at a time (no parallel runs, no resource contention)
- Host-based benchmark client: fio runs on host, mounts NFS/SMB from containers (most realistic)
- Build DittoFS from source using the existing project root `Dockerfile` (multi-stage, Alpine)
- Bridge networking with port mapping (standard Docker, works on Docker Desktop)
- Unique port per system: DittoFS: 12049, Ganesha: 22049, kernel NFS: 32049, etc. (prevents stale mount issues)
- Docker healthchecks on every service; orchestrator waits for 'healthy' before benchmarking
- Pinned competitor image versions (e.g., `juicedata/juicefs:1.2.0`, `nfsganesha/nfs-ganesha:5.9`) for reproducibility
- Configurable volume type via `.env`: disk bind mounts by default, `BENCH_TMPFS=1` for RAM-backed (user measures what they want)
- Cold-start benchmarks (no warmup phase)
- LocalStack S3 by default (zero latency, measures protocol overhead), but `.env` supports real S3 endpoints (Cubbit, Wasabi, AWS) by overriding `S3_ENDPOINT`, `S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`
- Shared `.env` file for all S3/Postgres credentials (gitignored)
- CI-ready from the start (design for GitHub Actions compatibility)
- Both Makefile and shell scripts: Makefile wraps scripts for convenience
- Capped container resources: CPU + memory limits per service, configurable in `.env` (e.g., `BENCH_CPU_LIMIT=2`, `BENCH_MEM_LIMIT=4g`)
- Separate Docker Compose services per DittoFS backend combo (dittofs-badger-s3, dittofs-postgres-s3, dittofs-badger-fs) — explicit `depends_on` per profile
- Always expose pprof port (:6060) — zero overhead unless actively profiled
- Reuse existing project `Dockerfile` (healthcheck on `:8080/health`, non-root user, multi-stage Go build)
- Host bind mount for benchmark results (results immediately accessible on host)
- `clean-all.sh` script: stops containers, removes volumes, unmounts NFS/SMB, prunes
- Capture container logs alongside benchmark results (docker compose logs saved to results dir)
- Require Docker Compose v2 (`docker compose`, not `docker-compose`)

### DittoFS Backend Configs
- Three backend combinations: badger+s3, postgres+s3, badger+fs
- Cache size matched to competitors via single `BENCH_CACHE_SIZE` env var (applied to DittoFS, JuiceFS, RClone equally)
- Metrics/telemetry disabled by default — enabled only with `--with-profiling` flag
- Single parameterized bootstrap script: `bootstrap-dittofs.sh --profile badger-s3` (creates stores, shares, adapters via dfsctl)
- Rely on Docker healthcheck for API readiness before bootstrap runs
- All systems export `/export` path (consistent mount commands, only host:port changes)
- Start empty: no seed data, benchmarks create their own data
- INFO log level during benchmarks
- Only the tested adapter enabled (NFS benchmarks = NFS adapter only, SMB = SMB only)
- Containerized PostgreSQL (`postgres:16`) for postgres+s3 profile
- WAL enabled by default for fairness (matches competitor disk-cache behavior), configurable via `BENCH_WAL_ENABLED=true/false` to measure WAL overhead separately
- DittoFS does NOT configure stores via YAML — minimal YAML for server basics (port, logging), stores/shares/adapters created via `dfsctl` REST API in bootstrap script

### Directory Layout & Results
- `bench/` structure: `docker/`, `configs/`, `workloads/`, `scripts/`, `analysis/`, `results/`
- Timestamp-based result directories: `results/2026-02-26_143022/`
- Inside each result: `raw/` (per-system subdirs), `metrics/`, `charts/`, `logs/` (container logs), `report.md`, `summary.csv`
- Per-system raw subdirs: `raw/dittofs-badger-s3/`, `raw/juicefs/`, `raw/kernel-nfs/`, etc.
- `results/` entirely gitignored
- `.env.example` with sensible LocalStack defaults — copy to `.env` and run immediately, no configuration needed
- `bench/.gitignore` for results/, .env, *.pyc, __pycache__ (self-contained, doesn't pollute project root)
- Comprehensive README.md in bench/ (full workflow guide)

### Cross-Platform Scope
- Linux primary, macOS supported
- Auto-detect OS in mount scripts: `resvport` on macOS, `vers=3` on Linux, etc.
- Prerequisites check script: report only (lists missing tools with install instructions, no auto-install)
- Docker Desktop supported (works on macOS/Windows, but Linux native recommended for accuracy)
- Parameterized fio engine via env var: `FIO_ENGINE=libaio` on Linux, `FIO_ENGINE=posixaio` on macOS
- Auto-detect cache flush: `purge` on macOS, `echo 3 > /proc/sys/vm/drop_caches` on Linux
- Windows SMB PowerShell script deferred to Phase 36

### Code Structure & Design
- All scripts use `set -euo pipefail`, pass ShellCheck (matches CI ShellCheck workflow #212)
- Shared `bench/scripts/lib/common.sh` with `log_info()`, `log_error()`, `die()`, color helpers, timer utilities
- `--dry-run` mode: prints all commands without executing (verify setup before long runs)
- Timer utilities in common.sh: `timer_start`/`timer_stop` for measuring script step durations
- Makefile with `help` target listing all commands with descriptions
- README includes ASCII flow diagram of benchmark pipeline
- Contributing section in README (how to add new workloads or competitors)

### Claude's Discretion
- Exact port numbers for each competitor (as long as they're unique and documented)
- Docker volume naming convention
- Makefile target naming beyond the obvious (bench, clean, help)
- common.sh color scheme and formatting
- Exact structure of .env.example variable grouping
- Prerequisites check: which tools are "required" vs "optional"

</decisions>

<specifics>
## Specific Ideas

- After running benchmarks, add results section to main project README — but only if DittoFS performs well against the competition
- The existing project `Dockerfile` (multi-stage Go build, Alpine runtime, healthcheck on `:8080/health`) should be reused directly — no custom benchmark Dockerfile
- Bootstrap pattern should follow what was done for WPTS in Phase 29.8 (bootstrap.sh creates stores/shares via dfsctl API)
- Support real S3 providers (Cubbit, Wasabi) via `.env` override, not just LocalStack
- WAL toggle (`BENCH_WAL_ENABLED`) to measure the isolated impact of WAL on performance

</specifics>

<deferred>
## Deferred Ideas

- Warmup mode (pre-populate caches before benchmarking) — could be added later as a `--warm` flag
- Artificial S3 latency simulation (tc/netem) — future enhancement for realistic cloud testing
- Windows SMB benchmark script (run-bench-smb.ps1) — Phase 36
- Scheduled/automated benchmark regression tracking in CI — future enhancement
- Grafana dashboards for benchmark monitoring — Phase 38

</deferred>

---

*Phase: 33-benchmark-infrastructure*
*Context gathered: 2026-02-26*
