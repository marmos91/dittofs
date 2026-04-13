<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-light.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/logo-dark.svg">
  <img alt="DittoFS" src="assets/logo-dark.svg" width="320">
</picture>

<br>

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Nix Flake](https://img.shields.io/badge/Nix-flake-5277C3?style=flat&logo=nixos)](https://nixos.org/)
[![Tests](https://img.shields.io/badge/tests-passing-brightgreen?style=flat)](https://github.com/marmos91/dittofs)
[![Go Report Card](https://goreportcard.com/badge/github.com/marmos91/dittofs)](https://goreportcard.com/report/github.com/marmos91/dittofs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange?style=flat)](https://github.com/marmos91/dittofs)

**A modular virtual filesystem written entirely in Go**

Decouple file interfaces from storage backends. NFSv3/v4/v4.1 and SMB2/3 server with pluggable metadata and block stores. Kubernetes-ready with official operator.

[Quick Start](#quick-start) • [Documentation](#documentation) • [Features](#features) • [Use Cases](#use-cases) • [Contributing](docs/CONTRIBUTING.md)

</div>

---

## Overview

DittoFS provides a modular architecture with **named, reusable stores** that can be mixed and matched per share.

### Key Concepts

- **Protocol Adapters**: Multiple protocols (NFS, SMB, etc.) can run simultaneously
- **Control Plane**: Centralized management of users, groups, shares, and configuration via REST API
- **Shares**: Export points that clients mount, each referencing specific stores
- **Named Store Registry**: Reusable store instances that can be shared across exports
- **Pluggable Storage**: Mix and match metadata and block store backends per share

## Features

- ✅ **NFS Support**: NFSv3 (28 procedures), NFSv4.0, and NFSv4.1 with sessions, delegations, and ACLs
- ✅ **SMB2/3 Support**: Windows/macOS file sharing with encryption (AES-GCM/CCM), signing (AES-CMAC/GMAC), leases V2, durable handles, and Kerberos authentication (SMB 3.0-3.1.1)
- ✅ **Kerberos Authentication**: RPCSEC_GSS for NFS and SPNEGO for SMB
- ✅ **No Special Permissions**: Runs entirely in userspace - no FUSE, no kernel modules
- ✅ **Pluggable Storage**: Mix protocols with any backend (S3, filesystem, custom)
- ✅ **Cloud-Native**: S3 backend with production optimizations
- ✅ **Pure Go**: Single binary, easy deployment, cross-platform
- ✅ **Extensible**: Clean adapter pattern for new protocols
- ✅ **User Management**: Unified users/groups with share-level permissions (CLI + REST API)
- ✅ **REST API**: Full management API with JWT authentication for users, groups, and shares

## Quick Start

### Installation

#### Using Nix (Recommended)

```bash
# Run directly without installation
nix run github:marmos91/dittofs -- init        # runs dfs (server)
nix run github:marmos91/dittofs -- start
nix run github:marmos91/dittofs#dfsctl -- login # runs dfsctl (client CLI)

# Or install to your profile (installs both dfs and dfsctl)
nix profile install github:marmos91/dittofs
dfs init && dfs start

# Development environment with all tools
nix develop github:marmos91/dittofs
```

#### Quick Install (macOS / Linux)

```bash
curl -fsSL https://github.com/marmos91/dittofs/releases/latest/download/install.sh | sh
```

#### Using Homebrew

```bash
brew tap marmos91/tap
brew install marmos91/tap/dfs      # Server daemon
brew install marmos91/tap/dfsctl   # Client CLI
```

#### Debian / Ubuntu (APT)

```bash
curl -fsSL https://s3.cubbit.eu/dittofs-binaries/apt/dittofs.gpg.key | gpg --dearmor | sudo tee /usr/share/keyrings/dittofs.gpg > /dev/null
echo "deb [signed-by=/usr/share/keyrings/dittofs.gpg] https://s3.cubbit.eu/dittofs-binaries/apt stable main" | sudo tee /etc/apt/sources.list.d/dfs.list
sudo apt update && sudo apt install dfs
sudo systemctl enable --now dfs
```

#### RHEL / Fedora (YUM)

```bash
sudo curl -fsSLo /etc/yum.repos.d/dfs.repo https://s3.cubbit.eu/dittofs-binaries/rpm/dfs.repo
sudo yum install dfs
sudo systemctl enable --now dfs
```

#### Arch Linux

```bash
# Download the latest .pkg.tar.zst from GitHub Releases
sudo pacman -U dfs_<version>_amd64.pkg.tar.zst
sudo systemctl enable --now dfs
```

#### Docker

```bash
docker run -d \
  -p 12049:12049 \
  -p 12445:12445 \
  -p 8080:8080 \
  -v /path/to/config.yaml:/config/config.yaml:ro \
  -v dittofs-data:/data \
  marmos91c/dittofs:latest
```

#### Scoop (Windows)

```powershell
scoop bucket add marmos91 https://github.com/marmos91/scoop-bucket
scoop install dfs       # Server daemon
scoop install dfsctl    # Client CLI
```

#### Build from Source

```bash
# Clone and build
git clone https://github.com/marmos91/dittofs.git
cd dittofs
go build -o dfs cmd/dfs/main.go

# Initialize configuration (creates ~/.config/dfs/config.yaml)
./dfs init

# Start server (see "First Run & Admin Password" section below)
./dfs start
```

### First Run & Admin Password

On first start, DittoFS creates an `admin` user automatically. The password is handled in one of two ways:

**Option 1: Auto-generated password (default)**

```bash
./dfs start
# Output:
# *** IMPORTANT: Admin user created with password: aBcDeFgHiJkLmNoPqRsTuVwX ***
# Please save this password. It will not be shown again.
```

The generated password is printed **only once**. If you lose it, you'll need to delete the database and restart.
The admin account is created with `MustChangePassword` enabled, so you'll be prompted to set a new password on first login via `dfsctl`.

**Option 2: Pre-set password via environment variable**

Set `DITTOFS_ADMIN_INITIAL_PASSWORD` before the first start to choose your own admin password:

```bash
DITTOFS_ADMIN_INITIAL_PASSWORD=my-secure-password ./dfs start
```

When using the environment variable, `MustChangePassword` is **not** enabled, so the password is ready to use as-is. This is especially useful for Docker, Kubernetes, and CI/CD environments where you can't read interactive output.

### NFS Quickstart

Get an NFS share running in under a minute:

```bash
# 1. Start the server (see "First Run & Admin Password" above)
./dfs start

# 2. Login (use the password from first start output or DITTOFS_ADMIN_INITIAL_PASSWORD)
./dfsctl login --server http://localhost:8080 --username admin
./dfsctl user change-password

# 3. Create a user with your host UID (for NFS write access)
./dfsctl user create --username $(whoami) --host-uid

# 4. Create stores (interactive prompts will ask for paths and S3 credentials)
./dfsctl store metadata add --name default --type badger
./dfsctl store block local add --name local-cache --type fs
./dfsctl store block remote add --name s3-remote --type s3

# 5. Create a share and grant access
./dfsctl share create --name /export --metadata default \
  --local local-cache --remote s3-remote
./dfsctl share permission grant /export --user $(whoami) --level read-write

# 6. Enable NFS adapter
./dfsctl adapter enable nfs

# 7. Mount and use!
# Linux:
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs
echo "Hello DittoFS!" > /mnt/nfs/hello.txt

# macOS:
mkdir -p /tmp/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/nfs
echo "Hello DittoFS!" > /tmp/nfs/hello.txt
```

> **Note:** This quickstart uses persistent storage: BadgerDB for metadata, local filesystem for block cache, and S3 for durable remote storage. Writes land on the local filesystem first and are asynchronously synced to S3. For quick local testing without external dependencies, you can use `--type memory` for both metadata and block stores instead.

### CLI Tools

Users and groups are stored in the control plane database (SQLite by default, PostgreSQL for HA). Manage them via CLI or REST API.

DittoFS provides two CLI binaries for complete management:

| Binary | Purpose | Examples |
|--------|---------|----------|
| **`dfs`** | Server daemon management | start, stop, status, config, logs, backup |
| **`dfsctl`** | Remote API client | users, groups, shares, stores, adapters |

#### Server Management (`dfs`)

```bash
# Configuration
./dfs config init              # Create default config file
./dfs config show              # Display current configuration
./dfs config validate          # Validate config file

# Server lifecycle
./dfs start                    # Start in foreground
./dfs start --pid-file /var/run/dfs.pid  # Start with PID file
./dfs stop                     # Graceful shutdown
./dfs stop --force             # Force kill
./dfs status                   # Check server status

# Logging
./dfs logs                     # Show last 100 lines
./dfs logs -f                  # Follow logs in real-time
./dfs logs -n 50               # Show last 50 lines
./dfs logs --since "2024-01-15T10:00:00Z"

# Backup
./dfs backup controlplane --output /tmp/backup.json

# Shell completion (bash, zsh, fish, powershell)
./dfs completion bash > /etc/bash_completion.d/dfs
```

#### Remote Management (`dfsctl`)

```bash
# Authentication & Context Management
./dfsctl login --server http://localhost:8080 --username admin
./dfsctl logout
./dfsctl context list          # List all server contexts
./dfsctl context use prod      # Switch to production server
./dfsctl context current       # Show current context

# User Management (password will be prompted interactively)
./dfsctl user create --username alice
./dfsctl user create --username alice --host-uid  # Use your current UID (for NFS)
./dfsctl user create --username bob --email bob@example.com --groups editors,viewers
./dfsctl user list
./dfsctl user list -o json     # Output as JSON
./dfsctl user get alice
./dfsctl user update alice --email alice@example.com
./dfsctl user delete alice

# Group Management
./dfsctl group create --name editors
./dfsctl group list
./dfsctl group add-user editors alice
./dfsctl group remove-user editors alice
./dfsctl group delete editors

# Share Management
./dfsctl share list
./dfsctl share create --name /archive --metadata badger-main --local local-cache --remote s3-content
./dfsctl share delete /archive

# Share Permissions
./dfsctl share permission list /export
./dfsctl share permission grant /export --user alice --level read-write
./dfsctl share permission grant /export --group editors --level read
./dfsctl share permission revoke /export --user alice

# Store Management (Metadata)
./dfsctl store metadata list
./dfsctl store metadata add --name fast-meta --type memory
./dfsctl store metadata add --name persistent --type badger --config '{"path":"/data/meta"}'
./dfsctl store metadata remove fast-meta

# Store Management (Block Stores)
./dfsctl store block list
./dfsctl store block add --kind local --name local-cache --type fs --config '{"path":"/data/cache"}'
./dfsctl store block add --kind remote --name s3-content --type s3 --config '{"bucket":"my-bucket"}'
./dfsctl store block remove s3-content

# Adapter Management
./dfsctl adapter list
./dfsctl adapter add --type nfs --port 12049
./dfsctl adapter update nfs --config '{"port":2049}'
./dfsctl adapter remove smb

# Settings
./dfsctl settings list
./dfsctl settings get logging.level
./dfsctl settings set logging.level DEBUG

# Shell completion
./dfsctl completion bash > /etc/bash_completion.d/dfsctl
./dfsctl completion zsh > ~/.zsh/completions/_dfsctl
```

#### Output Formats

All list commands support multiple output formats:

```bash
./dfsctl user list              # Default table format
./dfsctl user list -o json      # JSON format
./dfsctl user list -o yaml      # YAML format
```

#### REST API

```bash
# Login to get JWT token
# NOTE: For production, avoid passing passwords in command line - they appear in shell history
# and process listings. Use environment variables or prompt-based input instead.
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin"}' | jq -r '.access_token')

# List users
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/users

# Create a user (for demos only - use dfsctl for secure password entry)
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"secret123","uid":1001,"gid":1001}' \
  http://localhost:8080/api/v1/users
```

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md#cli-management-commands) for complete CLI documentation.

### Run with Docker

#### Using Pre-built Images (Recommended)

Pre-built multi-architecture images (`linux/amd64`, `linux/arm64`) are available on Docker Hub:

```bash
# Pull the latest image
docker pull marmos91c/dittofs:latest

# Initialize a config file first
mkdir -p ~/.config/dittofs
docker run --rm -v ~/.config/dittofs:/config marmos91c/dittofs:latest init --config /config/config.yaml

# Run DittoFS (set admin password via env var for Docker)
docker run -d \
  --name dittofs \
  -p 12049:12049 \
  -p 12445:12445 \
  -p 8080:8080 \
  -p 9090:9090 \
  -e DITTOFS_ADMIN_INITIAL_PASSWORD=my-secure-password \
  -v ~/.config/dittofs/config.yaml:/config/config.yaml:ro \
  -v dittofs-metadata:/data/metadata \
  -v dittofs-content:/data/content \
  -v dittofs-cache:/data/cache \
  marmos91c/dittofs:latest

# Check health
curl http://localhost:8080/health

# View logs (if no DITTOFS_ADMIN_INITIAL_PASSWORD was set, the auto-generated password appears here)
docker logs dittofs | head -20
```

**Available Tags:**
- `marmos91c/dittofs:latest` - Latest stable release
- `marmos91c/dittofs:vX.Y.Z` - Specific version
- `marmos91c/dittofs:vX.Y` - Latest patch for a minor version
- `marmos91c/dittofs:vX` - Latest minor for a major version

**Ports:**
- `12049`: NFS server
- `12445`: SMB server
- `8080`: REST API (health checks, management)
- `9090`: Prometheus metrics

#### Using Docker Compose

For more complex setups with different backends:

```bash
# Start with local filesystem backend (default)
docker compose up -d

# Start with S3 backend (includes localstack)
docker compose --profile s3-backend up -d

# Start with PostgreSQL backend (includes postgres)
docker compose --profile postgres-backend up -d

# View logs
docker compose logs -f dittofs
```

**Storage Backends:**
- **Local Filesystem (default)**: Uses Docker volumes for both metadata (BadgerDB) and content
- **S3 Backend**: Uses Docker volume for metadata (BadgerDB), S3 (localstack) for content
- **PostgreSQL Backend**: Uses PostgreSQL for metadata, Docker volume for content

**Monitoring:**
For Prometheus and Grafana monitoring stack, see [`monitoring/README.md`](monitoring/README.md).

> **Tip**: Make sure your `config.yaml` matches the backend you're using:
> - Default profile expects BadgerDB metadata + filesystem content
> - `--profile s3-backend` expects BadgerDB metadata + S3 content
> - `--profile postgres-backend` expects PostgreSQL metadata + filesystem content

### Deploy with Kubernetes Operator

DittoFS can be deployed on Kubernetes using our official operator:

```bash
# Install the operator (from the operator directory)
cd operator
make deploy

# Create a DittoFS instance
kubectl apply -f config/samples/dittofs_v1alpha1_dittofs.yaml

# Check status
kubectl get dittofs
```

The operator manages:
- DittoFS deployment lifecycle
- Configuration via Custom Resources
- Persistent volume claims for metadata and block stores
- Service exposure for NFS/SMB protocols

See the [`operator/`](operator/) directory for detailed documentation and configuration options.

### Mount from Client

**NFS:** See [NFS Quickstart](#nfs-quickstart) for complete setup. Mount commands:

```bash
# Linux
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS
sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/nfs
```

**SMB** (requires user authentication):
```bash
# First, create a user with dfsctl (server must be running)
./dfsctl login --server http://localhost:8080 --username admin
./dfsctl user create --username alice  # Password prompted interactively
./dfsctl share permission grant /export --user alice --level read-write

# Linux (using credentials file for security)
# Create credentials file securely - never echo passwords in scripts or command line
sudo mkdir -p /mnt/smb
cat > ~/.smbcredentials << 'EOF'
username=alice
password=YOUR_PASSWORD_HERE
EOF
chmod 600 ~/.smbcredentials
sudo mount -t cifs //localhost/export /mnt/smb -o port=12445,credentials=$HOME/.smbcredentials,vers=2.0

# macOS (will prompt for password)
mkdir -p /tmp/smb
mount -t smbfs //alice@localhost:12445/export /tmp/smb
```

See [docs/SMB.md](docs/SMB.md) for detailed SMB client usage.

### Testing

```bash
# Run unit tests
go test ./...

# Run E2E tests (requires NFS client installed)
go test -v -timeout 30m ./test/e2e/...
```

## Use Cases

### Multi-Tenant Cloud Storage Gateway

Different tenants get isolated metadata and block stores for security and billing separation.

### Performance-Tiered Storage

Hot data in memory, warm data on local disk, cold data in S3 - all with shared metadata for consistent namespace.

### Development & Testing

Fast iteration with in-memory stores, no external dependencies.

### Hybrid Cloud Deployment

Unified namespace across on-premises and cloud storage with shared metadata.

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for detailed examples.

## Documentation

### Core Documentation

- **[Architecture](docs/ARCHITECTURE.md)** - Deep dive into design patterns and internal implementation
- **[Configuration](docs/CONFIGURATION.md)** - Complete configuration guide with examples
- **[NFS Implementation](docs/NFS.md)** - NFSv3/v4/v4.1 protocol status and client usage
- **[SMB Implementation](docs/SMB.md)** - SMB2/3 protocol status, encryption, signing, leases, durable handles, and client usage
- **[Contributing](docs/CONTRIBUTING.md)** - Development guide and contribution guidelines
- **[Implementing Stores](docs/IMPLEMENTING_STORES.md)** - Guide for implementing custom metadata and block stores

### Operational Guides

- **[Troubleshooting](docs/TROUBLESHOOTING.md)** - Common issues and solutions
- **[Security](docs/SECURITY.md)** - Security considerations and best practices
- **[FAQ](docs/FAQ.md)** - Frequently asked questions

### Development

- **[CLAUDE.md](CLAUDE.md)** - Detailed guidance for Claude Code and developers
- **[Releasing](docs/RELEASING.md)** - Release process and versioning

## Current Status

### ✅ Implemented

**NFS Adapter (NFSv3, NFSv4.0, NFSv4.1)**
- NFSv3: All core read/write operations (28 procedures)
- NFSv4.0: Compound operations, ACLs, delegations, built-in file locking
- NFSv4.1: Sessions, sequence slots, backchannel support
- Kerberos authentication via RPCSEC_GSS
- Mount protocol support (v3)
- TCP transport with graceful shutdown
- Buffer pooling and performance optimizations
- Read/write caching with background flush

**SMB2/3 Protocol Adapter**
- Multi-dialect negotiation (SMB 2.0.2, 3.0, 3.0.2, 3.1.1)
- SMB3 encryption: AES-128-GCM, AES-128-CCM, AES-256-GCM, AES-256-CCM
- SMB3 signing: AES-128-CMAC, AES-128-GMAC (plus HMAC-SHA256 for 2.x)
- Preauth integrity (SHA-512 hash chain) for downgrade protection
- SP800-108 key derivation for per-session cryptographic keys
- NTLM and Kerberos authentication via SPNEGO
- Leases V2 with directory leasing and epoch-based break prevention
- Durable handles V1/V2 for session resilience
- Cross-protocol coordination: bidirectional lease/delegation breaks with NFS
- Session management with adaptive credit flow control
- Tree connect with share-level permission checking and per-share encryption
- File operations: CREATE, READ, WRITE, CLOSE, FLUSH
- Directory operations: QUERY_DIRECTORY, CHANGE_NOTIFY
- Metadata operations: QUERY_INFO, SET_INFO
- Byte-range locking and oplocks
- Compound request handling (CREATE+QUERY_INFO+CLOSE)
- Read/write caching (shared with NFS)
- Parallel request processing
- macOS Finder and smbclient compatible

See [docs/SMB.md](docs/SMB.md) for complete SMB3 protocol documentation, wire format details, and configuration.

**Storage Backends**
- In-memory metadata (ephemeral, fast)
- BadgerDB metadata (persistent, path-based handles)
- PostgreSQL metadata (persistent, distributed)
- Local block stores: filesystem and in-memory (per-share isolated)
- Remote block stores: S3 (production-ready, ref-counted across shares) and in-memory (testing)

**Block Store Architecture**
- Two-tier model: local stores for fast access, remote stores for durability
- Per-share BlockStore isolation with independent local storage directories
- Async syncer for local-to-remote data transfer
- In-memory read cache (L1) for frequently accessed blocks

**POSIX Compliance**
- 99.99% pass rate on pjdfstest (8,788/8,789 tests)
- All metadata stores (Memory, BadgerDB, PostgreSQL) achieve parity
- Single expected failure due to NFSv3 32-bit timestamp limitation (year 2106)
- See [FAQ](docs/FAQ.md) for known limitations

**User Management & Control Plane**
- Unified identity system for NFS and SMB
- Users with bcrypt password hashing
- Groups with share-level permissions
- Permission resolution: user → group → share default
- CLI tools for user/group management
- REST API with JWT authentication
- Control plane database (SQLite/PostgreSQL)

**Production Features**
- Prometheus metrics integration
- OpenTelemetry distributed tracing
- Structured JSON logging
- Request rate limiting
- Enhanced graceful shutdown
- Comprehensive E2E test suite
- Performance benchmark framework

### 🚧 In Development

**SMB Protocol Enhancements**
- [ ] Windows client compatibility testing
- [x] E2E test suite for SMB

### 🚀 Roadmap

**SMB Advanced Features**
- [x] SMB3 support (encryption, signing, leases V2, durable handles, dialect 3.0-3.1.1)
- [x] File locking (oplocks, byte-range locks)
- [ ] Security descriptors and Windows ACLs
- [ ] Extended attributes (xattrs) support
- [x] Kerberos authentication via SPNEGO
- [ ] Multichannel (multiple TCP connections per session)

**Kubernetes Integration**
- [x] Kubernetes Operator for deployment
- [x] Health check endpoints
- [ ] CSI driver implementation

**Advanced Features**
- [ ] Sync between DittoFS replicas
- [ ] Scan content stores to populate metadata stores
- [x] Admin REST API for users/permissions/shares/configs
- [x] NFSv4.0 and NFSv4.1 support (sessions, delegations, ACLs, built-in locking)
- [x] Kerberos authentication (RPCSEC_GSS for NFS, SPNEGO for SMB)
- [ ] Advanced caching strategies

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for complete roadmap.

## Configuration

DittoFS uses a **two-layer configuration** approach:

1. **Config file** (`~/.config/dfs/config.yaml`): Server infrastructure settings (logging, telemetry, cache, database, API)
2. **CLI/API** (`dfsctl`): Runtime resources (stores, shares, adapters) persisted in the control plane database

### Server Config File

```yaml
database:
  type: sqlite  # or "postgres" for HA
  sqlite:
    path: /var/lib/dfs/controlplane.db

controlplane:
  port: 8080
  jwt:
    secret: "your-secret-key-at-least-32-characters"

cache:
  path: /var/lib/dfs/cache
  size: "1Gi"               # supports: "512MB", "2Gi", "4GB", etc.
```

### Cache

DittoFS uses a **mandatory write-through cache** backed by a WAL (Write-Ahead Log) for crash recovery. All file writes pass through the cache before being flushed asynchronously to the configured payload store (S3, filesystem, etc.).

**How it works:**
1. **Write**: Data is written to in-memory 4MB block buffers and journaled to the WAL on disk
2. **Flush**: Blocks are uploaded asynchronously to the payload store in the background
3. **Evict**: After a successful upload, cache blocks become evictable under memory pressure (LRU)
4. **Recovery**: On crash/restart, the WAL replays uncommitted writes automatically

**Key points:**
- The `path` is **required** - the cache creates a `cache.dat` WAL file in this directory
- Default `path` is `$TMPDIR/dittofs-cache` (e.g., `/tmp/dittofs-cache`) - fine for development, but use a persistent path for production
- Default `size` is `1Gi` - increase for workloads with large files or many concurrent writers
- All shares use the same global cache
- Dirty (unflushed) blocks cannot be evicted, providing backpressure when the payload store is slower than the write rate

**Sizing guidance:**
- **Development/testing**: `512Mi` to `1Gi` (default)
- **General use**: `2Gi` to `4Gi`
- **Heavy write workloads** (large files, S3 backend): `4Gi` to `16Gi`

```yaml
# Production example
cache:
  path: /var/lib/dfs/cache   # Use a persistent directory (not /tmp)
  size: "4Gi"
```

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md#6-cache-configuration) for full details.

### Runtime Management (CLI)

Stores, shares, and adapters are managed via `dfsctl` and persisted in the database:

```bash
# Create named stores (reusable across shares)
./dfsctl store metadata add --name badger-main --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'
./dfsctl store block add --kind local --name local-cache --type fs \
  --config '{"path":"/var/lib/dfs/blocks"}'
./dfsctl store block add --kind remote --name s3-cloud --type s3 \
  --config '{"region":"us-east-1","bucket":"my-dfs-bucket"}'

# Create shares referencing stores (each gets its own BlockStore)
./dfsctl share create --name /archive --metadata badger-main \
  --local local-cache --remote s3-cloud

# Grant permissions
./dfsctl share permission grant /archive --user alice --level read-write

# Enable protocol adapters
./dfsctl adapter enable nfs --port 12049
./dfsctl adapter enable smb --port 12445
```

> **Note**: Users and groups are also managed via CLI (`dfsctl user/group`) or REST API, not in the config file.

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for complete documentation.

## Why DittoFS?

**The Problem**: Traditional filesystem servers are tightly coupled to their storage layers, making it difficult to:
- Support multiple access protocols
- Mix and match storage backends
- Deploy without kernel-level permissions
- Customize for specific use cases

**The Solution**: DittoFS provides:
- Protocol independence through adapters
- Storage flexibility through pluggable repositories
- Userspace operation with no special permissions
- Pure Go for easy deployment and integration

## Comparison

| Feature | Traditional NFS | Cloud Gateways | DittoFS |
|---------|----------------|----------------|---------|
| Permissions | Kernel-level | Varies | Userspace only |
| Multi-protocol | Separate servers | Limited | Unified |
| Storage Backend | Filesystem only | Vendor-specific | Pluggable |
| Metadata Backend | Filesystem only | Vendor-specific | Pluggable |
| Language | C/C++ | Varies | Pure Go |
| Deployment | Complex | Complex | Single binary |

See [docs/FAQ.md](docs/FAQ.md) for detailed comparisons.

## Contributing

DittoFS welcomes contributions! See [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md) for:

- Development setup
- Testing guidelines
- Code structure
- Common development tasks

## Security

⚠️ **DittoFS is experimental software** - not yet production ready.

- No security audit performed
- AUTH_UNIX and Kerberos (RPCSEC_GSS) for NFS; NTLM and Kerberos (SPNEGO) for SMB
- SMB3 encryption (AES-GCM/CCM) for SMB transport; NFS requires VPN or network-level encryption

See [docs/SECURITY.md](docs/SECURITY.md) for details and recommendations.

## References

### Specifications
- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3
- [RFC 7530](https://tools.ietf.org/html/rfc7530) - NFS Version 4.0
- [RFC 8881](https://tools.ietf.org/html/rfc8881) - NFS Version 4.1
- [RFC 5531](https://tools.ietf.org/html/rfc5531) - RPC Protocol
- [RFC 4506](https://tools.ietf.org/html/rfc4506) - XDR Standard

### Related Projects
- [go-nfs](https://github.com/willscott/go-nfs) - Another NFS implementation in Go
- [FUSE](https://github.com/libfuse/libfuse) - Filesystem in Userspace

## License

MIT License - See [LICENSE](LICENSE) file for details

## Disclaimer

⚠️ **Experimental Software**

- Do not use in production without thorough testing
- API may change without notice
- No backwards compatibility guarantees
- Security has not been professionally audited

---

**Getting Started?** → [Quick Start](#quick-start)

**Questions?** → [FAQ](docs/FAQ.md) or [open an issue](https://github.com/marmos91/dittofs/issues)

**Want to Contribute?** → [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md)
