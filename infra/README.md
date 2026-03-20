# DittoFS Infrastructure

Shared Pulumi Go program for provisioning Scaleway VMs. Used by both the
benchmark suite and the edge/offline test suite.

## Stacks

| Stack | Purpose | Lifecycle |
|-------|---------|-----------|
| `base` | VPC, persistent client VM (51.15.199.235), S3 bucket (`dittofs-bench`) | Created once, kept running |
| `bench` | Ephemeral server VM for benchmarks or edge tests | Created per test, destroyed after |

## Systems

Set `system` in Pulumi config to select which software to install on the server VM:

| System | Protocol | Port | Use Case |
|--------|----------|------|----------|
| `dittofs-badger-fs` | NFS | 12049 | Benchmark: BadgerDB + local FS |
| `dittofs-badger-s3` | NFS | 12049 | Benchmark: BadgerDB + S3 |
| `dittofs-edge` | NFS | 12049 | Edge test: BadgerDB + FS + S3 (pin retention) |
| `kernel-nfs` | NFS | 2049 | Benchmark: Linux kernel NFS |
| `ganesha` | NFS | 2049 | Benchmark: NFS-Ganesha |
| `rclone` | NFS | 2049 | Benchmark: RClone serve NFS |
| `samba` | SMB | 445 | Benchmark: Samba |
| `juicefs` | FUSE | - | Benchmark: JuiceFS |
| `rclone-s3` | NFS | 2049 | Benchmark: RClone + S3 |
| `juicefs-s3` | NFS | 2049 | Benchmark: JuiceFS + S3 |
| `s3ql` | NFS | 2049 | Benchmark: S3QL + S3 |

## Quick Start

```bash
# Prerequisites: Pulumi CLI, Go 1.25+, Scaleway credentials
export SCW_ACCESS_KEY=<key>
export SCW_SECRET_KEY=<key>
export SCW_DEFAULT_PROJECT_ID=<project-id>
export PULUMI_CONFIG_PASSPHRASE=""

# Deploy base infrastructure (once)
cd infra
pulumi up --stack base

# Deploy edge test server
pulumi config set --stack bench dittofs-bench:system dittofs-edge
pulumi config set --stack bench dittofs-bench:privateNetworkID <from base output>
pulumi config set --stack bench --secret s3AccessKey <key>
pulumi config set --stack bench --secret s3SecretKey <key>
pulumi up --stack bench

# Tear down server (keeps base)
pulumi destroy --stack bench -y
```

## Cost

- **PLAY2-MICRO VM**: ~0.01 EUR/hour
- **150 GB Block Storage**: ~0.01 EUR/hour
- **S3 Storage**: ~0.01 EUR/GB/month
- **Flexible IP**: ~0.004 EUR/hour

Estimated total: **~0.03 EUR/hour** while running. Always destroy the bench stack
when not in use.

## Directory Structure

```
infra/
  main.go            # Pulumi entry (base/bench stack switch)
  base.go            # Base stack (VPC, client VM, S3)
  bench.go           # Bench stack (ephemeral server VM)
  config.go          # Pulumi config loading
  network.go         # VPC/private network
  systems.go         # System registry
  scripts/           # Per-system install scripts
    dittofs-edge.sh  # Edge test deployment
    ...              # Benchmark scripts
```
