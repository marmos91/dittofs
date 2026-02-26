# DittoFS Benchmark Suite

Compare DittoFS performance against five competitor NFS/SMB implementations using Docker Compose profiles, fio workloads, and automated result collection. Each system runs in isolation with identical resource constraints for fair comparison.

## Quick Start

```bash
# 1. Copy environment template
cp .env.example .env

# 2. Check prerequisites
make check

# 3. Build DittoFS image
make build PROFILE=dittofs-badger-s3

# 4. Start the system under test
make up PROFILE=dittofs-badger-s3

# 5. Bootstrap DittoFS stores/shares/adapter
docker compose exec dittofs-badger-s3 /app/bootstrap.sh badger-s3

# 6. Mount and run benchmarks (see workloads/ for fio jobs)
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/bench
fio workloads/random-read-4k.fio --output-format=json+ --output=results/random-read.json

# 7. Unmount and stop
sudo umount /mnt/bench
make down PROFILE=dittofs-badger-s3
```

## Pipeline

```
Prerequisites   Build Image   Start System   Bootstrap   Mount NFS/SMB
     |               |              |             |            |
     v               v              v             v            v
 make check --> make build --> make up -----> bootstrap --> mount
                                                              |
                                                              v
                                                     Run fio workloads
                                                              |
                                                              v
                                              Collect results --> umount --> make down
```

## Available Systems

| Profile | Protocol | Backend | Port | Description |
|---------|----------|---------|------|-------------|
| `dittofs-badger-s3` | NFS | BadgerDB + S3 | 12049 | DittoFS with persistent metadata and S3 payload |
| `dittofs-postgres-s3` | NFS | PostgreSQL + S3 | 12049 | DittoFS with PostgreSQL metadata and S3 payload |
| `dittofs-badger-fs` | NFS | BadgerDB + filesystem | 12049 | DittoFS with local-only storage |
| `dittofs-smb` | SMB | BadgerDB + S3 | 12445 | DittoFS SMB adapter |
| `ganesha` | NFS | Local filesystem (FSAL_VFS) | 22049 | NFS-Ganesha userspace NFS server |
| `kernel-nfs` | NFS | Local filesystem | 32049 | Linux kernel NFS server (gold standard) |
| `rclone` | NFS | S3 via VFS cache | 42049 | RClone NFS serve over S3 |
| `juicefs` | FUSE | PostgreSQL + S3 | - | JuiceFS FUSE mount (host path: `/mnt/juicefs`) |
| `samba` | SMB | Local filesystem | 22445 | Samba SMB server |

Only one profile should run at a time to avoid resource contention.

### ARM64 Compatibility

Not all competitor images provide native ARM64 builds. On ARM64 hosts (e.g., Apple Silicon Macs, Graviton instances), the following profiles will fail:

| Profile | ARM64 Status | Notes |
|---------|-------------|-------|
| `ganesha` | Not available | `apnar/nfs-ganesha` is amd64-only |
| `kernel-nfs` | Not available | `erichough/nfs-server` is amd64-only |

All DittoFS profiles, `rclone`, `juicefs`, and `samba` work natively on ARM64. While Docker can emulate amd64 via QEMU (`--platform linux/amd64`), this adds significant overhead (5-20x slower) making benchmark results unreliable. Avoid QEMU emulation for performance testing; use it only for functional validation and always flag results accordingly.

## Directory Structure

```
bench/
|-- docker-compose.yml          # All services with profiles
|-- .env.example                # Environment template (copy to .env)
|-- .gitignore                  # Excludes results/, .env, etc.
|-- Makefile                    # Convenience targets
|-- README.md                   # This file
|-- docker/
|   +-- Dockerfile.dittofs      # Multi-stage DittoFS build (dfs + dfsctl)
|-- configs/
|   |-- dittofs/
|   |   |-- badger-s3.yaml      # BadgerDB + S3 config (also used by dittofs-smb)
|   |   |-- postgres-s3.yaml    # PostgreSQL + S3 config
|   |   +-- badger-fs.yaml      # BadgerDB + filesystem config
|   |-- ganesha/
|   |   +-- ganesha.conf        # NFS-Ganesha FSAL_VFS export
|   |-- kernel-nfs/
|   |   +-- exports             # Reference export line (service uses env var)
|   |-- rclone/
|   |   +-- rclone.conf         # S3 remote for LocalStack
|   +-- samba/
|       +-- smb.conf            # Samba share configuration
|-- scripts/
|   |-- lib/
|   |   +-- common.sh           # Shared logging/timer utilities
|   |-- check-prerequisites.sh  # Validate required tools
|   |-- bootstrap-dittofs.sh    # Create DittoFS stores/shares per profile
|   +-- clean-all.sh            # Full cleanup (unmount, stop, prune)
|-- workloads/                  # fio job files (Phase 34)
|-- analysis/                   # Result analysis scripts (Phase 37)
+-- results/                    # Benchmark output (gitignored)
```

## Configuration

### Environment Variables

Copy `.env.example` to `.env` and adjust as needed.

#### S3 (LocalStack)

| Variable | Default | Description |
|----------|---------|-------------|
| `AWS_ENDPOINT_URL` | `http://localhost:4566` | S3 endpoint |
| `AWS_ACCESS_KEY_ID` | `test` | S3 access key |
| `AWS_SECRET_ACCESS_KEY` | `test` | S3 secret key |
| `AWS_DEFAULT_REGION` | `us-east-1` | S3 region |
| `S3_BUCKET` | `bench` | S3 bucket name |

#### PostgreSQL

| Variable | Default | Description |
|----------|---------|-------------|
| `POSTGRES_USER` | `bench` | PostgreSQL username |
| `POSTGRES_PASSWORD` | `bench` | PostgreSQL password |
| `POSTGRES_DB` | `bench` | PostgreSQL database name |
| `POSTGRES_HOST` | `postgres` | PostgreSQL hostname (Docker service name) |
| `POSTGRES_PORT` | `5432` | PostgreSQL port |

#### DittoFS

| Variable | Default | Description |
|----------|---------|-------------|
| `DITTOFS_CONTROLPLANE_SECRET` | (see .env.example) | DittoFS admin password |
| `BENCH_WAL_ENABLED` | `true` | Enable write-ahead log |

#### Resource Limits

| Variable | Default | Description |
|----------|---------|-------------|
| `BENCH_CPU_LIMIT` | `2` | CPU cores per container |
| `BENCH_MEM_LIMIT` | `4g` | Memory limit per container |
| `BENCH_CACHE_SIZE` | `256M` | Cache size for RClone (Go size format) |
| `BENCH_CACHE_SIZE_MB` | `256` | Cache size in MB for JuiceFS |

#### Benchmark Parameters

| Variable | Default | Description |
|----------|---------|-------------|
| `BENCH_THREADS` | `4` | fio thread count |
| `BENCH_ITERATIONS` | `3` | Number of benchmark iterations |
| `BENCH_RUNTIME` | `60` | fio runtime in seconds |

#### Platform

| Variable | Default | Description |
|----------|---------|-------------|
| `FIO_ENGINE` | (auto-detected) | fio I/O engine (`libaio` on Linux, `posixaio` on macOS) |

### Resource Limits

All benchmarked services share the same CPU and memory limits (via `deploy.resources.limits`). Adjust `BENCH_CPU_LIMIT` and `BENCH_MEM_LIMIT` in `.env` to match your test hardware.

## Usage Examples

```bash
# Run any system by setting PROFILE
make up PROFILE=ganesha
make down PROFILE=ganesha

# Build and run DittoFS with PostgreSQL + S3
make build PROFILE=dittofs-postgres-s3
make up PROFILE=dittofs-postgres-s3
docker compose exec dittofs-postgres-s3 /app/bootstrap.sh postgres-s3

# Check container status
make ps PROFILE=dittofs-badger-s3

# Follow logs
make logs PROFILE=dittofs-badger-s3

# Full cleanup (unmount, stop, remove volumes, prune)
make clean

# Direct docker compose usage
docker compose --profile kernel-nfs up -d
docker compose --profile kernel-nfs down -v
```

### Mount Commands

```bash
# DittoFS NFS (any dittofs-* NFS profile)
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/bench

# NFS-Ganesha
sudo mount -t nfs -o tcp,port=22049,mountport=22049 localhost:/export /mnt/bench

# Kernel NFS
sudo mount -t nfs -o tcp,port=32049,mountport=32049 localhost:/export /mnt/bench

# RClone NFS
sudo mount -t nfs -o tcp,port=42049,mountport=42049 localhost:/ /mnt/bench

# DittoFS SMB
sudo mount -t cifs //localhost/export /mnt/bench -o port=12445,username=admin,password=<secret>

# Samba
sudo mount -t cifs //localhost/export /mnt/bench -o port=22445,username=bench,password=bench
```

## Port Map

| Port | Service | Protocol |
|------|---------|----------|
| 12049 | DittoFS (NFS) | NFS |
| 8080 | DittoFS (API) | HTTP |
| 6060 | DittoFS (pprof) | HTTP |
| 22049 | NFS-Ganesha | NFS |
| 32049 | Kernel NFS | NFS |
| 32111 | Kernel NFS (portmapper) | RPC |
| 42049 | RClone | NFS |
| 52049 | JuiceFS (S3 gateway) | HTTP |
| 12445 | DittoFS (SMB) | SMB |
| 22445 | Samba | SMB |

## Platform Notes

### Linux (recommended)

- fio engine: `libaio` (default)
- Cache flush: `echo 3 > /proc/sys/vm/drop_caches` (requires sudo)
- NFS mount: standard `mount -t nfs` with port options
- Docker: native (best performance, no VM overhead)

### macOS

- fio engine: `posixaio`
- Cache flush: `purge` (requires sudo)
- NFS mount: `mount -t nfs` with port options (Docker Desktop required)
- Docker Desktop adds VM overhead; Linux is recommended for accurate results
- Some containers may need extra setup (kernel NFS requires `SYS_ADMIN`)

Set the fio engine in `.env`:

```bash
FIO_ENGINE=posixaio
```

## Adding a New Competitor

1. Add a service definition in `docker-compose.yml` with a unique profile name
2. Assign a unique port (next available in the 10000+ range)
3. Create a config directory under `configs/<name>/` if needed
4. Add a healthcheck (required for orchestrator integration)
5. Add `deploy.resources.limits` for fair resource capping
6. Export `/export` path for consistent mount commands
7. Update the systems table and port map in this README

## Troubleshooting

### Stale NFS mounts

```bash
# Force unmount stale NFS handles
sudo umount -f /mnt/bench
# or on Linux
sudo umount -l /mnt/bench
```

### Port conflicts

Each system uses a unique port to avoid kernel NFS client caching issues. If you see "address already in use", check for leftover containers:

```bash
docker compose --profile <profile> down -v
# or full cleanup
make clean
```

### Docker Desktop limitations

Docker Desktop runs containers in a VM. NFS port mapping works but adds overhead. For accurate benchmarks, use Linux with native Docker.

### Container fails to start

Check logs for the specific service:

```bash
docker compose --profile <profile> logs <service-name>
```

Common issues:
- **kernel-nfs**: Requires `SYS_ADMIN` capability (configured in docker-compose.yml)
- **juicefs**: Requires `privileged: true` for FUSE (configured in docker-compose.yml)
- **LocalStack**: Must be healthy before S3-dependent services start

### Bootstrap fails

The DittoFS bootstrap script requires the API to be ready. If it fails:

```bash
# Check API health
curl -sf http://localhost:8080/health/ready

# Retry bootstrap
docker compose exec <service> /app/bootstrap.sh <backend>
```

The `<backend>` argument is the profile name without the `dittofs-` prefix (e.g., `badger-s3`, `postgres-s3`, `badger-fs`, `smb`).

## Related Phases

- **Phase 34**: fio workload definitions (random read/write, sequential, metadata-heavy)
- **Phase 35**: Competitor-specific setup and tuning
- **Phase 36**: Orchestrator script (automated full benchmark runs)
- **Phase 37**: Analysis pipeline (CSV aggregation, chart generation)
- **Phase 38**: Profiling integration (pprof, flamegraphs)
