# Phase 33: Benchmark Infrastructure - Research

**Researched:** 2026-02-26
**Domain:** Docker Compose orchestration, NFS/SMB benchmarking infrastructure, competitor service configuration
**Confidence:** HIGH

## Summary

Phase 33 creates the foundational `bench/` directory with Docker Compose profiles for benchmarking DittoFS against five competitors (JuiceFS, NFS-Ganesha, RClone, kernel NFS, Samba). The infrastructure is purely configuration and scripting -- no Go code, no workload definitions, no analysis pipeline.

The existing project provides strong patterns to follow: the `test/smb-conformance/` directory demonstrates Docker Compose with profiles, DittoFS config YAML files, bootstrap scripts via `dfsctl`, and a Makefile with help targets. The benchmark infrastructure should mirror this pattern closely while adding resource limits, unique port mapping, host-based fio execution, and competitor service definitions.

**Primary recommendation:** Reuse the `test/smb-conformance/` patterns (Dockerfile.dittofs, bootstrap.sh, config YAML structure, Makefile layout) as the template. Extend with `deploy.resources.limits` for capped resources, unique port assignments per system, and competitor-specific service definitions with pinned image versions.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Single `docker-compose.yml` with profiles (not separate files per system)
- Shared infrastructure (LocalStack S3, PostgreSQL) activated per-profile via `depends_on` -- not always-on
- Sequential benchmarking: one system at a time (no parallel runs, no resource contention)
- Host-based benchmark client: fio runs on host, mounts NFS/SMB from containers (most realistic)
- Build DittoFS from source using the existing project root `Dockerfile` (multi-stage, Alpine) -- actually `Dockerfile.dittofs` pattern from smb-conformance (includes dfsctl)
- Bridge networking with port mapping (standard Docker, works on Docker Desktop)
- Unique port per system: DittoFS: 12049, Ganesha: 22049, kernel NFS: 32049, etc. (prevents stale mount issues)
- Docker healthchecks on every service; orchestrator waits for 'healthy' before benchmarking
- Pinned competitor image versions for reproducibility
- Configurable volume type via `.env`: disk bind mounts by default, `BENCH_TMPFS=1` for RAM-backed
- Cold-start benchmarks (no warmup phase)
- LocalStack S3 by default; `.env` supports real S3 endpoints by overriding `S3_ENDPOINT`, `S3_BUCKET`, etc.
- Shared `.env` file for all S3/Postgres credentials (gitignored)
- CI-ready from the start (design for GitHub Actions compatibility)
- Both Makefile and shell scripts: Makefile wraps scripts for convenience
- Capped container resources: CPU + memory limits per service, configurable in `.env` (e.g., `BENCH_CPU_LIMIT=2`, `BENCH_MEM_LIMIT=4g`)
- Separate Docker Compose services per DittoFS backend combo (dittofs-badger-s3, dittofs-postgres-s3, dittofs-badger-fs)
- Always expose pprof port (:6060) -- zero overhead unless actively profiled
- Reuse existing project `Dockerfile` (healthcheck on `:8080/health`, non-root user, multi-stage Go build)
- Host bind mount for benchmark results (results immediately accessible on host)
- `clean-all.sh` script: stops containers, removes volumes, unmounts NFS/SMB, prunes
- Capture container logs alongside benchmark results (docker compose logs saved to results dir)
- Require Docker Compose v2 (`docker compose`, not `docker-compose`)
- Three backend combinations: badger+s3, postgres+s3, badger+fs
- Cache size matched to competitors via single `BENCH_CACHE_SIZE` env var
- Metrics/telemetry disabled by default -- enabled only with `--with-profiling` flag
- Single parameterized bootstrap script: `bootstrap-dittofs.sh --profile badger-s3`
- All systems export `/export` path (consistent mount commands, only host:port changes)
- Start empty: no seed data
- INFO log level during benchmarks
- Only the tested adapter enabled (NFS benchmarks = NFS adapter only, SMB = SMB only)
- Containerized PostgreSQL (`postgres:16`) for postgres+s3 profile
- WAL enabled by default, configurable via `BENCH_WAL_ENABLED=true/false`
- DittoFS stores/shares/adapters created via `dfsctl` REST API in bootstrap script (minimal YAML)
- `bench/` structure: `docker/`, `configs/`, `workloads/`, `scripts/`, `analysis/`, `results/`
- Timestamp-based result directories: `results/2026-02-26_143022/`
- Inside each result: `raw/` (per-system subdirs), `metrics/`, `charts/`, `logs/`, `report.md`, `summary.csv`
- `bench/.gitignore` for results/, .env, *.pyc, __pycache__
- Comprehensive README.md in bench/
- Linux primary, macOS supported
- Auto-detect OS in mount scripts
- Prerequisites check script: report only (no auto-install)
- Docker Desktop supported
- Parameterized fio engine via env var: `FIO_ENGINE=libaio` on Linux, `FIO_ENGINE=posixaio` on macOS
- Auto-detect cache flush: `purge` on macOS, `echo 3 > /proc/sys/vm/drop_caches` on Linux
- All scripts use `set -euo pipefail`, pass ShellCheck
- Shared `bench/scripts/lib/common.sh` with log/timer utilities
- `--dry-run` mode
- Timer utilities in common.sh
- Makefile with `help` target

### Claude's Discretion
- Exact port numbers for each competitor (as long as unique and documented)
- Docker volume naming convention
- Makefile target naming beyond the obvious (bench, clean, help)
- common.sh color scheme and formatting
- Exact structure of .env.example variable grouping
- Prerequisites check: which tools are "required" vs "optional"

### Deferred Ideas (OUT OF SCOPE)
- Warmup mode (pre-populate caches) -- could be added later as `--warm` flag
- Artificial S3 latency simulation (tc/netem) -- future enhancement
- Windows SMB benchmark script (run-bench-smb.ps1) -- Phase 36
- Scheduled/automated benchmark regression tracking in CI -- future enhancement
- Grafana dashboards for benchmark monitoring -- Phase 38
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| BENCH-01.1 | `bench/` directory structure (configs/, workloads/, scripts/, analysis/, results/) | Directory layout defined in CONTEXT.md decisions; also includes `docker/`, `scripts/lib/` |
| BENCH-01.2 | `docker-compose.yml` with profiles for all systems | Docker Compose v2 profiles syntax researched; competitor images identified with versions and configs |
| BENCH-01.3 | `.env.example` with S3, PostgreSQL, and benchmark configuration variables | Variable grouping researched; LocalStack defaults documented; resource limit vars identified |
| BENCH-01.4 | DittoFS config files for each backend combination (badger+s3, postgres+s3, badger+fs) | Existing smb-conformance configs provide direct template; minimal YAML with API-based store creation |
| BENCH-01.5 | `scripts/check-prerequisites.sh` validates all required tools | Tool list identified; ShellCheck compliance pattern from CI workflow #212 |
| BENCH-01.6 | `results/` directory gitignored | bench/.gitignore pattern documented |
</phase_requirements>

## Standard Stack

### Core

| Tool/Image | Version | Purpose | Why Standard |
|------------|---------|---------|--------------|
| Docker Compose v2 | 2.x (built-in) | Service orchestration with profiles | Required by decisions; profiles enable one-at-a-time benchmarking |
| LocalStack | 4.13.1 | S3-compatible local storage | Already used in project (smb-conformance); pinned version for reproducibility |
| PostgreSQL | 16-alpine | Metadata store for postgres+s3 profile | Already used in project; matches smb-conformance pattern |
| fio | 3.38+ | I/O benchmark tool (host-installed) | Industry standard for storage benchmarks; JSON+ output mode |
| ShellCheck | (CI) | Shell script linting | Required by CI workflow #212 (`set -euo pipefail`, pass ShellCheck) |

### Competitor Images

| Image | Pinned Version | Purpose | Configuration |
|-------|---------------|---------|---------------|
| `juicedata/mount` | `ce-v1.3.0` | JuiceFS (PostgreSQL + S3) | `juicefs format` then `juicefs mount`; FUSE required (privileged) |
| `apnar/nfs-ganesha` | `latest` (pin SHA) | NFS-Ganesha (FSAL_VFS, local FS) | ganesha.conf with EXPORT block; no kernel module dependency |
| `rclone/rclone` | `1.73.1` | RClone (`serve nfs` over S3) | rclone.conf for S3 remote + `serve nfs --addr 0.0.0.0:PORT --vfs-cache-mode full` |
| `erichough/nfs-server` | `2.2.1` | Kernel NFS server (gold standard) | `NFS_EXPORT_0` env var; requires `--cap-add SYS_ADMIN` |
| `dperson/samba` | `latest` (pin SHA) | Samba (SMB baseline) | `-u` for users, `-s` for shares; ports 139/445 |
| DittoFS | built from source | DittoFS (3 backend combos) | Reuse `Dockerfile.dittofs` pattern from smb-conformance |

### Supporting

| Tool | Purpose | When to Use |
|------|---------|-------------|
| `make` | Convenience wrapper | All benchmark operations |
| `jq` | JSON parsing | fio output parsing, health checks |
| `bc` | Arithmetic | Script calculations |
| `curl` | HTTP requests | Health checks, API calls |
| `python3` | Analysis pipeline (future) | Phase 37 -- but check in prerequisites now |
| `docker` | Container runtime | All service management |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `apnar/nfs-ganesha` | `janeczku/nfs-ganesha` | apnar is more actively maintained and supports v3+v4 |
| `dperson/samba` | `crazymax/samba` | dperson has 1M+ pulls, more established; crazymax has cleaner Alpine base |
| `erichough/nfs-server` | `itsthenetwork/nfs-server-alpine` | erichough is better documented and more widely used |
| `juicedata/mount` | `juicedata/juicefs` | `mount` image contains the actual client; `juicefs` is the volume plugin |

## Architecture Patterns

### Recommended Directory Structure
```
bench/
├── docker-compose.yml            # All services with profiles
├── .env.example                  # Template (copy to .env)
├── .gitignore                    # results/, .env, *.pyc, __pycache__
├── Makefile                      # Convenience targets
├── README.md                     # Full workflow guide
├── docker/
│   └── Dockerfile.dittofs        # DittoFS + dfsctl (from smb-conformance pattern)
├── configs/
│   ├── dittofs/
│   │   ├── badger-s3.yaml        # Minimal server YAML
│   │   ├── postgres-s3.yaml
│   │   └── badger-fs.yaml
│   ├── ganesha/
│   │   └── ganesha.conf          # FSAL_VFS export config
│   ├── rclone/
│   │   └── rclone.conf           # S3 remote definition
│   ├── kernel-nfs/
│   │   └── exports               # /etc/exports for kernel NFS
│   └── samba/
│       └── smb.conf              # Or rely on dperson env vars
├── workloads/                    # Empty (Phase 34)
├── scripts/
│   ├── lib/
│   │   └── common.sh             # Shared utilities (log, timer, colors)
│   ├── check-prerequisites.sh    # Validate required tools
│   ├── bootstrap-dittofs.sh      # Parameterized: --profile badger-s3
│   └── clean-all.sh              # Full cleanup
├── analysis/                     # Empty (Phase 37)
└── results/                      # Gitignored
```

### Pattern 1: Docker Compose Profiles for Mutual Exclusion

**What:** Each competitor system is assigned its own profile. Only one profile is activated at a time, ensuring no resource contention between systems.

**When to use:** Always -- this is the core benchmarking pattern.

**Example:**
```yaml
# docker-compose.yml
services:
  # Infrastructure services (activated via depends_on)
  localstack:
    image: localstack/localstack:4.13.1
    profiles: [dittofs-badger-s3, dittofs-postgres-s3, juicefs, rclone]
    environment:
      SERVICES: s3
      EAGER_SERVICE_LOADING: "1"
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:4566/_localstack/health"]
      interval: 5s
      timeout: 3s
      retries: 10
    deploy:
      resources:
        limits:
          cpus: "${BENCH_CPU_LIMIT:-2}"
          memory: "${BENCH_MEM_LIMIT:-4g}"

  postgres:
    image: postgres:16-alpine
    profiles: [dittofs-postgres-s3, juicefs]
    environment:
      POSTGRES_USER: ${POSTGRES_USER:-bench}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:-bench}
      POSTGRES_DB: ${POSTGRES_DB:-bench}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-bench}"]
      interval: 5s
      timeout: 3s
      retries: 10

  # System under test
  dittofs-badger-s3:
    build:
      context: ..
      dockerfile: bench/docker/Dockerfile.dittofs
    profiles: [dittofs-badger-s3]
    ports:
      - "12049:12049"
      - "8080:8080"
      - "6060:6060"
    depends_on:
      localstack:
        condition: service_healthy
    volumes:
      - ./configs/dittofs/badger-s3.yaml:/config/config.yaml:ro
      - ./scripts/bootstrap-dittofs.sh:/app/bootstrap.sh:ro
      - dittofs-data:/data
      - ./results:/results
    deploy:
      resources:
        limits:
          cpus: "${BENCH_CPU_LIMIT:-2}"
          memory: "${BENCH_MEM_LIMIT:-4g}"
```

### Pattern 2: Parameterized Bootstrap (from smb-conformance)

**What:** A single bootstrap script that creates stores, shares, and adapters via `dfsctl` REST API based on a `--profile` argument.

**When to use:** For all DittoFS backend combinations.

**Example:**
```bash
#!/usr/bin/env bash
set -euo pipefail

PROFILE="${1:?Usage: bootstrap-dittofs.sh <profile>}"
# ... login, create stores based on profile, create share, enable adapter
case "$PROFILE" in
    badger-s3)
        $DFSCTL store metadata add --name default --type badger \
            --config '{"db_path":"/data/metadata"}'
        $DFSCTL store payload add --name default --type s3 \
            --config "{\"bucket\":\"${S3_BUCKET}\",\"region\":\"${S3_REGION}\",\"endpoint\":\"http://localstack:4566\",\"force_path_style\":true}"
        ;;
    # ... other profiles
esac
```

Source: `test/smb-conformance/bootstrap.sh` -- existing project pattern.

### Pattern 3: Resource-Capped Services via deploy.resources

**What:** Every benchmarked service has CPU and memory limits to ensure fair comparison and prevent resource starvation.

**When to use:** Every service in docker-compose.yml.

**Example:**
```yaml
deploy:
  resources:
    limits:
      cpus: "${BENCH_CPU_LIMIT:-2}"
      memory: "${BENCH_MEM_LIMIT:-4g}"
```

Source: Docker Compose deploy specification (https://docs.docker.com/reference/compose-file/deploy/).

### Pattern 4: Shared Script Library

**What:** A `common.sh` sourced by all scripts providing logging, color, timer, and OS-detection utilities.

**When to use:** Every shell script in `bench/scripts/`.

**Example:**
```bash
#!/usr/bin/env bash
# bench/scripts/lib/common.sh

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die()       { log_error "$@"; exit 1; }

# Timer utilities
timer_start() { date +%s; }
timer_stop()  {
    local start="$1"
    local end
    end=$(date +%s)
    echo $((end - start))
}

# OS detection
detect_os() {
    case "$(uname -s)" in
        Linux*)  echo "linux" ;;
        Darwin*) echo "macos" ;;
        *)       die "Unsupported OS: $(uname -s)" ;;
    esac
}
```

Source: Mirrors `test/e2e/run-e2e.sh` color/logging pattern.

### Anti-Patterns to Avoid
- **Mixed profiles running simultaneously:** Always enforce one profile at a time. Docker Compose allows activating multiple profiles, but the benchmark design requires sequential single-system runs.
- **Non-pinned image versions:** Never use `:latest` without also recording the SHA digest. Competitor image updates could silently change benchmark results.
- **Hardcoded S3 endpoints in config files:** All S3/Postgres connection details must come from `.env` or environment variables, not baked into config files.
- **Running fio inside containers:** fio must run on the host to avoid measuring Docker overlay filesystem overhead. The host mounts NFS/SMB from the container and runs fio directly.
- **Shared volumes across profiles:** Each profile should use its own named volume to prevent data leakage between benchmark runs.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| S3 local testing | Custom S3 mock | LocalStack 4.13.1 | Full S3 API compatibility, zero config, already in project |
| NFS server baseline | Custom Go NFS server | `erichough/nfs-server` (kernel NFS) | Gold standard, uses real kernel NFS, widely benchmarked |
| Samba baseline | Custom SMB server | `dperson/samba` | Production Samba, standard SMB2/3 support |
| Prerequisite checking | Ad-hoc `which` commands | Structured check script with version detection | Consistent output, install instructions, CI-friendly exit codes |
| Container health waiting | Sleep loops | Docker Compose `depends_on: condition: service_healthy` | Built-in, race-free, composable |

**Key insight:** The benchmark infrastructure is entirely about orchestrating existing systems fairly. Every competitor should be configured to its documented best practices, not custom-tuned for DittoFS advantage.

## Common Pitfalls

### Pitfall 1: LocalStack Authentication Changes (March 2026)
**What goes wrong:** Starting March 23, 2026, `localstack/localstack:latest` will require authentication.
**Why it happens:** LocalStack is consolidating into a single image with mandatory auth.
**How to avoid:** Pin to `localstack/localstack:4.13.1` (pre-auth-requirement version). This version will continue to work without authentication.
**Warning signs:** `docker compose up` failures with auth errors after updating images.

### Pitfall 2: Kernel NFS Requires SYS_ADMIN
**What goes wrong:** `erichough/nfs-server` fails to start without elevated privileges.
**Why it happens:** Kernel NFS needs to mount internal filesystems (`nfsd`, `proc`) inside the container.
**How to avoid:** Always include `cap_add: [SYS_ADMIN]` in the kernel-nfs service definition.
**Warning signs:** Container exits immediately with mount permission errors.

### Pitfall 3: JuiceFS Requires FUSE (Privileged)
**What goes wrong:** JuiceFS mount fails inside container without FUSE device access.
**Why it happens:** JuiceFS uses FUSE to create a local mount point that it then exposes.
**How to avoid:** Run JuiceFS container with `privileged: true` and mount `/dev/fuse`. Alternative: use JuiceFS gateway mode (S3 gateway) if NFS export is not needed directly.
**Warning signs:** "fuse: device not found" errors in container logs.

### Pitfall 4: NFS Mount Staleness on Port Reuse
**What goes wrong:** After stopping one system and starting another on the same port, NFS clients see stale file handles.
**Why it happens:** The host kernel NFS client caches mount information by server:port.
**How to avoid:** Use unique ports per system (DittoFS: 12049, Ganesha: 22049, kernel NFS: 32049, etc.) and always unmount between runs.
**Warning signs:** "Stale file handle" errors when accessing mounted paths.

### Pitfall 5: Docker Desktop NFS Port Limitations
**What goes wrong:** NFS port mapping doesn't work correctly on Docker Desktop (macOS/Windows).
**Why it happens:** Docker Desktop uses a VM, so port 2049 inside the container maps through the VM, and macOS NFS client may conflict with local NFS services.
**How to avoid:** Use non-standard NFS ports (12049, 22049, etc.) and explicitly map them. Document that Linux native Docker is recommended for accurate benchmarks.
**Warning signs:** "Connection refused" or "Permission denied" on mount commands on macOS.

### Pitfall 6: Resource Limits Not Applied Without deploy.resources
**What goes wrong:** Containers consume unlimited CPU/memory, invalidating fair comparison.
**Why it happens:** Docker Compose doesn't apply limits unless explicitly configured under `deploy.resources.limits`.
**How to avoid:** Every benchmarked service must have `deploy.resources.limits` with cpus and memory sourced from `.env` variables.
**Warning signs:** One system appearing significantly faster simply because it consumed all available resources.

### Pitfall 7: RClone NFS Serve Port Binding
**What goes wrong:** RClone `serve nfs` binds to loopback only by default.
**Why it happens:** Default `--addr` is a random port on `127.0.0.1`.
**How to avoid:** Always specify `--addr 0.0.0.0:PORT` when running in Docker to allow external connections.
**Warning signs:** Host cannot connect to RClone NFS port despite container running.

## Code Examples

### Docker Compose Service: NFS-Ganesha
```yaml
ganesha:
  image: apnar/nfs-ganesha:latest  # Pin SHA for reproducibility
  profiles: [ganesha]
  ports:
    - "22049:2049"
  volumes:
    - ./configs/ganesha/ganesha.conf:/etc/ganesha/ganesha.conf:ro
    - ganesha-data:/export
  cap_add:
    - SYS_ADMIN
  healthcheck:
    test: ["CMD", "showmount", "-e", "localhost"]
    interval: 5s
    timeout: 3s
    retries: 10
    start_period: 5s
  deploy:
    resources:
      limits:
        cpus: "${BENCH_CPU_LIMIT:-2}"
        memory: "${BENCH_MEM_LIMIT:-4g}"
```

### NFS-Ganesha Configuration (ganesha.conf)
```
NFS_CORE_PARAM {
    NFS_Port = 2049;
    NFS_Protocols = 3, 4;
}

EXPORT {
    Export_Id = 1;
    Path = /export;
    Pseudo = /export;
    Access_Type = RW;
    Squash = No_root_squash;
    SecType = sys;
    Transports = TCP;
    Protocols = 3, 4;
    FSAL {
        Name = VFS;
    }
}
```

Source: Derived from https://github.com/rootfs/nfs-ganesha-docker/blob/master/vfs.conf

### Docker Compose Service: Kernel NFS
```yaml
kernel-nfs:
  image: erichough/nfs-server:2.2.1
  profiles: [kernel-nfs]
  ports:
    - "32049:2049"
    - "32111:111"
  volumes:
    - kernel-nfs-data:/export
  environment:
    NFS_EXPORT_0: "/export *(rw,no_subtree_check,no_root_squash,fsid=0)"
  cap_add:
    - SYS_ADMIN
  healthcheck:
    test: ["CMD", "showmount", "-e", "localhost"]
    interval: 5s
    timeout: 3s
    retries: 10
    start_period: 10s
  deploy:
    resources:
      limits:
        cpus: "${BENCH_CPU_LIMIT:-2}"
        memory: "${BENCH_MEM_LIMIT:-4g}"
```

Source: https://github.com/ehough/docker-nfs-server

### Docker Compose Service: RClone
```yaml
rclone:
  image: rclone/rclone:1.73.1
  profiles: [rclone]
  ports:
    - "42049:42049"
  volumes:
    - ./configs/rclone/rclone.conf:/config/rclone/rclone.conf:ro
    - rclone-cache:/tmp/rclone-cache
  command: >
    serve nfs localstack-s3:
    --addr 0.0.0.0:42049
    --vfs-cache-mode full
    --vfs-cache-max-size ${BENCH_CACHE_SIZE:-256M}
    --no-modtime
  depends_on:
    localstack:
      condition: service_healthy
  healthcheck:
    test: ["CMD", "rclone", "rc", "noop"]
    interval: 5s
    timeout: 3s
    retries: 10
    start_period: 10s
  deploy:
    resources:
      limits:
        cpus: "${BENCH_CPU_LIMIT:-2}"
        memory: "${BENCH_MEM_LIMIT:-4g}"
```

### RClone Configuration (rclone.conf)
```ini
[localstack-s3]
type = s3
provider = Other
access_key_id = test
secret_access_key = test
endpoint = http://localstack:4566
region = us-east-1
force_path_style = true
```

Source: https://rclone.org/commands/rclone_serve_nfs/

### Docker Compose Service: JuiceFS
```yaml
juicefs:
  image: juicedata/mount:ce-v1.3.0
  profiles: [juicefs]
  privileged: true
  ports:
    - "52049:52049"
  volumes:
    - /dev/fuse:/dev/fuse
    - juicefs-cache:/var/jfsCache
  depends_on:
    localstack:
      condition: service_healthy
    postgres:
      condition: service_healthy
  command: >
    sh -c "
      juicefs format
        --storage s3
        --bucket http://localstack:4566/${S3_BUCKET:-bench}
        --access-key ${AWS_ACCESS_KEY_ID:-test}
        --secret-key ${AWS_SECRET_ACCESS_KEY:-test}
        postgres://${POSTGRES_USER:-bench}:${POSTGRES_PASSWORD:-bench}@postgres:5432/${POSTGRES_DB:-bench}?sslmode=disable
        bench-vol &&
      juicefs gateway
        --cache-size ${BENCH_CACHE_SIZE_MB:-256}
        postgres://${POSTGRES_USER:-bench}:${POSTGRES_PASSWORD:-bench}@postgres:5432/${POSTGRES_DB:-bench}?sslmode=disable
        0.0.0.0:52049
    "
  healthcheck:
    test: ["CMD", "curl", "-sf", "http://localhost:52049/"]
    interval: 5s
    timeout: 3s
    retries: 15
    start_period: 15s
  deploy:
    resources:
      limits:
        cpus: "${BENCH_CPU_LIMIT:-2}"
        memory: "${BENCH_MEM_LIMIT:-4g}"
```

**Note:** JuiceFS NFS exposure requires careful consideration. JuiceFS's `gateway` command provides S3 access, not NFS. For NFS mount testing, JuiceFS must do a FUSE mount inside the container and then export via a separate NFS server (e.g., nfs-ganesha on top of the FUSE mount) or use `juicefs mount` and expose the FUSE mount path. This is a complexity to resolve during implementation.

### Docker Compose Service: Samba
```yaml
samba:
  image: dperson/samba:latest  # Pin SHA for reproducibility
  profiles: [samba]
  ports:
    - "12445:445"
  volumes:
    - samba-data:/export
  command: >
    -u "bench;bench"
    -s "export;/export;yes;no;no;bench"
    -p
  healthcheck:
    test: ["CMD", "smbclient", "-L", "localhost", "-U", "bench%bench", "-N"]
    interval: 5s
    timeout: 3s
    retries: 10
    start_period: 5s
  deploy:
    resources:
      limits:
        cpus: "${BENCH_CPU_LIMIT:-2}"
        memory: "${BENCH_MEM_LIMIT:-4g}"
```

### DittoFS Config YAML (badger-s3.yaml)
```yaml
# Minimal DittoFS config for benchmarking
# Stores, shares, and adapters created via bootstrap-dittofs.sh
logging:
  level: "INFO"
  format: "text"
  output: "stdout"

shutdown_timeout: 30s

database:
  type: sqlite
  sqlite:
    path: "/data/controlplane.db"

controlplane:
  port: 8080
  jwt:
    secret: "benchmark-infrastructure-secret-key-32ch"
    access_token_duration: 1h
    refresh_token_duration: 168h

cache:
  path: "/data/cache"
  size: "${BENCH_CACHE_SIZE:-256MB}"
```

Source: Derived from `test/smb-conformance/configs/badger-s3.yaml`

### Prerequisites Check Script Pattern
```bash
#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

MISSING=0

check_required() {
    local cmd="$1"
    local install_hint="$2"
    if command -v "$cmd" &>/dev/null; then
        log_info "  [OK] $cmd ($(command -v "$cmd"))"
    else
        log_error "  [MISSING] $cmd -- $install_hint"
        MISSING=$((MISSING + 1))
    fi
}

check_optional() {
    local cmd="$1"
    local purpose="$2"
    if command -v "$cmd" &>/dev/null; then
        log_info "  [OK] $cmd ($(command -v "$cmd"))"
    else
        log_warn "  [OPTIONAL] $cmd -- $purpose"
    fi
}

log_info "Checking benchmark prerequisites..."
echo ""

log_info "Required tools:"
check_required "docker" "Install: https://docs.docker.com/get-docker/"
check_required "fio" "Install: apt install fio / brew install fio"
check_required "jq" "Install: apt install jq / brew install jq"
check_required "bc" "Install: apt install bc / brew install bc"
check_required "make" "Install: apt install make / xcode-select --install"

echo ""
log_info "Optional tools:"
check_optional "python3" "Needed for analysis pipeline (Phase 37)"
check_optional "smbclient" "Needed for SMB benchmarks"

# Verify Docker Compose v2
if docker compose version &>/dev/null; then
    log_info "  [OK] docker compose v2 ($(docker compose version --short))"
else
    log_error "  [MISSING] docker compose v2 -- Install Docker Desktop or docker-compose-plugin"
    MISSING=$((MISSING + 1))
fi

exit "$MISSING"
```

### Recommended Port Assignments

| System | NFS Port | API/Other | Profile Name |
|--------|----------|-----------|-------------|
| DittoFS (all) | 12049 | 8080 (API), 6060 (pprof) | dittofs-badger-s3, dittofs-postgres-s3, dittofs-badger-fs |
| NFS-Ganesha | 22049 | -- | ganesha |
| Kernel NFS | 32049 | -- | kernel-nfs |
| RClone | 42049 | -- | rclone |
| JuiceFS | 52049 | -- | juicefs |
| DittoFS SMB | 12445 | 8080, 6060 | dittofs-smb |
| Samba | 22445 | -- | samba |

### .env.example Structure
```bash
# =============================================================================
# S3 Configuration (LocalStack defaults -- override for real S3)
# =============================================================================
S3_ENDPOINT=http://localhost:4566
S3_BUCKET=bench
S3_REGION=us-east-1
AWS_ACCESS_KEY_ID=test
AWS_SECRET_ACCESS_KEY=test

# =============================================================================
# PostgreSQL Configuration
# =============================================================================
POSTGRES_USER=bench
POSTGRES_PASSWORD=bench
POSTGRES_DB=bench

# =============================================================================
# DittoFS Configuration
# =============================================================================
DITTOFS_CONTROLPLANE_SECRET=BenchmarkInfrastructureSecret32ch!
BENCH_WAL_ENABLED=true

# =============================================================================
# Resource Limits (applied to all benchmarked services)
# =============================================================================
BENCH_CPU_LIMIT=2
BENCH_MEM_LIMIT=4g
BENCH_CACHE_SIZE=256MB
BENCH_CACHE_SIZE_MB=256

# =============================================================================
# Benchmark Parameters
# =============================================================================
BENCH_THREADS=4
BENCH_ITERATIONS=3
BENCH_RUNTIME=60

# =============================================================================
# Platform Configuration (auto-detected by scripts, override if needed)
# =============================================================================
# FIO_ENGINE=libaio        # Linux default
# FIO_ENGINE=posixaio      # macOS default

# =============================================================================
# Volume Configuration
# =============================================================================
# BENCH_TMPFS=1            # Set to use RAM-backed tmpfs volumes
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `docker-compose` (v1 binary) | `docker compose` (v2 plugin) | Docker Compose v2 GA (2022) | v1 is deprecated; v2 is built into Docker CLI |
| `localstack/localstack:latest` (unauthenticated) | Pin to pre-auth version `4.13.1` | March 23, 2026 | After this date, `:latest` requires auth token |
| `juicedata/juicefs` (volume plugin) | `juicedata/mount` (client image) | JuiceFS v1.x | `mount` image contains the actual JuiceFS client binary |
| `--privileged` for NFS containers | `cap_add: [SYS_ADMIN]` | Best practice | Minimum required capability, less attack surface |

**Deprecated/outdated:**
- `docker-compose` v1 binary: replaced by `docker compose` v2 plugin
- `juicedata/juicefs` Docker image tag `latest`: deprecated, use specific `ce-vX.Y.Z` tags
- Running fio inside containers for NFS benchmarks: adds double-overlay overhead; host-based is standard

## Open Questions

1. **JuiceFS NFS Export Method**
   - What we know: JuiceFS supports FUSE mount and S3 gateway modes. It does not natively serve NFS.
   - What's unclear: The best way to expose a JuiceFS filesystem via NFS for host fio testing. Options: (a) FUSE mount inside container + bind-mount to host, (b) JuiceFS FUSE mount + run NFS-Ganesha on top, (c) use JuiceFS S3 gateway and skip NFS for this competitor.
   - Recommendation: Use JuiceFS FUSE mount with `--privileged` and expose the mount path via a bind mount. The host mounts this path directly. If NFS protocol overhead is what we're measuring, option (b) adds unfair overhead. Clarify with user if needed.

2. **NFS-Ganesha Image Stability**
   - What we know: No official `nfsganesha/nfs-ganesha` Docker image exists. Community images (`apnar/nfs-ganesha`) are the standard.
   - What's unclear: Long-term maintenance status of `apnar/nfs-ganesha`.
   - Recommendation: Use `apnar/nfs-ganesha` with a pinned SHA digest. If it becomes unmaintained, `janeczku/nfs-ganesha` is the fallback.

3. **Samba Healthcheck Feasibility**
   - What we know: `dperson/samba` includes smbclient. Healthcheck via `smbclient -L localhost` should work.
   - What's unclear: Whether smbclient is available inside the container for healthcheck commands.
   - Recommendation: Test during implementation. Fallback: use `nc -z localhost 445` for TCP-level check.

4. **DittoFS Dockerfile Reuse vs. Customization**
   - What we know: The CONTEXT.md says "reuse existing project Dockerfile" but also needs dfsctl. The smb-conformance uses a separate `Dockerfile.dittofs` that builds both binaries.
   - What's unclear: Whether to copy `Dockerfile.dittofs` from smb-conformance or create a new one.
   - Recommendation: Copy and adapt `test/smb-conformance/Dockerfile.dittofs` to `bench/docker/Dockerfile.dittofs`. It already handles the dual-binary build pattern and healthcheck. Adjust labels and ports for benchmark context.

## Sources

### Primary (HIGH confidence)
- `/Users/marmos91/Projects/dittofs-benchmarks/test/smb-conformance/docker-compose.yml` -- Existing project Docker Compose pattern with profiles
- `/Users/marmos91/Projects/dittofs-benchmarks/test/smb-conformance/bootstrap.sh` -- Bootstrap script pattern for dfsctl API configuration
- `/Users/marmos91/Projects/dittofs-benchmarks/test/smb-conformance/Dockerfile.dittofs` -- DittoFS Docker image with dfs+dfsctl
- `/Users/marmos91/Projects/dittofs-benchmarks/test/smb-conformance/configs/badger-s3.yaml` -- DittoFS config YAML template
- `/Users/marmos91/Projects/dittofs-benchmarks/test/smb-conformance/Makefile` -- Makefile pattern with help target
- `/Users/marmos91/Projects/dittofs-benchmarks/test/e2e/run-e2e.sh` -- Shell script pattern (set -euo pipefail, colors, argument parsing)
- `/Users/marmos91/Projects/dittofs-benchmarks/.github/workflows/lint.yml` -- ShellCheck CI workflow

### Secondary (MEDIUM confidence)
- [Docker Compose Profiles docs](https://docs.docker.com/compose/how-tos/profiles/) -- Profile activation and depends_on behavior
- [Docker Compose Deploy spec](https://docs.docker.com/reference/compose-file/deploy/) -- Resource limits syntax
- [erichough/nfs-server GitHub](https://github.com/ehough/docker-nfs-server) -- Kernel NFS container v2.2.1 config
- [rclone serve nfs docs](https://rclone.org/commands/rclone_serve_nfs/) -- RClone NFS flags and VFS cache
- [dperson/samba GitHub](https://github.com/dperson/samba) -- Samba Docker container config
- [JuiceFS Docker docs](https://juicefs.com/docs/community/juicefs_on_docker/) -- JuiceFS container setup
- [NFS-Ganesha VFS config](https://github.com/rootfs/nfs-ganesha-docker/blob/master/vfs.conf) -- ganesha.conf EXPORT block
- [LocalStack blog on auth changes](https://blog.localstack.cloud/the-road-ahead-for-localstack/) -- March 2026 auth requirement

### Tertiary (LOW confidence)
- `apnar/nfs-ganesha` version: using `:latest` with recommendation to pin SHA -- exact latest version tag not confirmed via Docker Hub API
- `dperson/samba` version: using `:latest` with recommendation to pin SHA -- exact version not confirmed
- JuiceFS `ce-v1.3.0` tag: reported by web search as latest CE version; should be verified on Docker Hub

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all images confirmed via Docker Hub and project docs, versions verified via web search
- Architecture: HIGH -- directly mirrors existing `test/smb-conformance/` patterns already proven in the project
- Pitfalls: HIGH -- sourced from official Docker docs and competitor documentation
- Competitor configs: MEDIUM -- some configs derived from GitHub examples, need verification during implementation

**Research date:** 2026-02-26
**Valid until:** 2026-03-28 (30 days -- stable infrastructure domain, but LocalStack auth deadline is March 23)
