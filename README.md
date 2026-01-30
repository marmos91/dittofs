<div align="center">

# DittoFS

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Nix Flake](https://img.shields.io/badge/Nix-flake-5277C3?style=flat&logo=nixos)](https://nixos.org/)
[![Tests](https://img.shields.io/badge/tests-passing-brightgreen?style=flat)](https://github.com/marmos91/dittofs)
[![Go Report Card](https://goreportcard.com/badge/github.com/marmos91/dittofs)](https://goreportcard.com/report/github.com/marmos91/dittofs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange?style=flat)](https://github.com/marmos91/dittofs)

**A modular virtual filesystem written entirely in Go**

Decouple file interfaces from storage backends. NFSv3 and SMB2 server with pluggable metadata and content stores. Kubernetes-ready with official operator.

[Quick Start](#quick-start) • [Documentation](#documentation) • [Features](#features) • [Use Cases](#use-cases) • [Contributing](docs/CONTRIBUTING.md)

</div>

---

## Overview

DittoFS provides a modular architecture with **named, reusable stores** that can be mixed and matched per share:

```
┌──────────────────────────────────────┐
│       Protocol Adapters              │
│         NFS ✅  SMB ✅               │
└──────────────┬───────────────────────┘
               │
               ▼
┌──────────────────────────────────────┐
│         Control Plane                │
│  ┌─────────────────────────────────┐ │
│  │ REST API (users, groups, shares)│ │
│  │ Database (SQLite/PostgreSQL)    │ │
│  └─────────────────────────────────┘ │
│                                      │
│  Metadata Stores │  Block Storage    │
│  • Memory        │  ┌─────────────┐  │
│  • BadgerDB      │  │ Cache + WAL │  │
│  • PostgreSQL    │  └──────┬──────┘  │
│                  │         │         │
│                  │  ┌──────▼──────┐  │
│                  │  │ Transfer Mgr│  │
│                  │  └──────┬──────┘  │
│                  │         │         │
│                  │  ┌──────▼──────┐  │
│                  │  │ Block Store │  │
│                  │  │ • Memory    │  │
│                  │  │ • S3        │  │
│                  │  └─────────────┘  │
└──────────────────────────────────────┘
```

### Key Concepts

- **Protocol Adapters**: Multiple protocols (NFS, SMB, etc.) can run simultaneously
- **Control Plane**: Centralized management of users, groups, shares, and configuration via REST API
- **Shares**: Export points that clients mount, each referencing specific stores
- **Named Store Registry**: Reusable store instances that can be shared across exports
- **Pluggable Storage**: Mix and match metadata and content backends per share

## Features

- ✅ **Production-Ready NFSv3**: 28 procedures fully implemented
- ✅ **SMB2 Support**: Windows/macOS file sharing with NTLM authentication
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
nix run github:marmos91/dittofs -- init
nix run github:marmos91/dittofs -- start

# Or install to your profile
nix profile install github:marmos91/dittofs
dittofs init && dittofs start

# Development environment with all tools
nix develop github:marmos91/dittofs
```

#### Build from Source

```bash
# Clone and build
git clone https://github.com/marmos91/dittofs.git
cd dittofs
go build -o dittofs cmd/dittofs/main.go

# Initialize configuration (creates ~/.config/dittofs/config.yaml)
./dittofs init

# Start server
./dittofs start
```

### CLI Tools

Users and groups are stored in the control plane database (SQLite by default, PostgreSQL for HA). Manage them via CLI or REST API.

DittoFS provides two CLI binaries for complete management:

| Binary | Purpose | Examples |
|--------|---------|----------|
| **`dittofs`** | Server daemon management | start, stop, status, config, logs, backup |
| **`dittofsctl`** | Remote API client | users, groups, shares, stores, adapters |

#### Server Management (`dittofs`)

```bash
# Configuration
./dittofs config init              # Create default config file
./dittofs config show              # Display current configuration
./dittofs config validate          # Validate config file

# Server lifecycle
./dittofs start                    # Start in foreground
./dittofs start --pid-file /var/run/dittofs.pid  # Start with PID file
./dittofs stop                     # Graceful shutdown
./dittofs stop --force             # Force kill
./dittofs status                   # Check server status

# Logging
./dittofs logs                     # Show last 100 lines
./dittofs logs -f                  # Follow logs in real-time
./dittofs logs -n 50               # Show last 50 lines
./dittofs logs --since "2024-01-15T10:00:00Z"

# Backup
./dittofs backup controlplane --output /tmp/backup.json

# Shell completion (bash, zsh, fish, powershell)
./dittofs completion bash > /etc/bash_completion.d/dittofs
```

#### Remote Management (`dittofsctl`)

```bash
# Authentication & Context Management
./dittofsctl login --server http://localhost:8080 --username admin
./dittofsctl logout
./dittofsctl context list          # List all server contexts
./dittofsctl context use prod      # Switch to production server
./dittofsctl context current       # Show current context

# User Management (password will be prompted interactively)
./dittofsctl user create --username alice
./dittofsctl user create --username bob --email bob@example.com --groups editors,viewers
./dittofsctl user list
./dittofsctl user list -o json     # Output as JSON
./dittofsctl user get alice
./dittofsctl user update alice --email alice@example.com
./dittofsctl user delete alice

# Group Management
./dittofsctl group create --name editors
./dittofsctl group list
./dittofsctl group add-user editors alice
./dittofsctl group remove-user editors alice
./dittofsctl group delete editors

# Share Management
./dittofsctl share list
./dittofsctl share create --name /archive --metadata badger-main --payload s3-content
./dittofsctl share delete /archive

# Share Permissions
./dittofsctl share permission list /export
./dittofsctl share permission grant /export --user alice --level read-write
./dittofsctl share permission grant /export --group editors --level read
./dittofsctl share permission revoke /export --user alice

# Store Management (Metadata)
./dittofsctl store metadata list
./dittofsctl store metadata add --name fast-meta --type memory
./dittofsctl store metadata add --name persistent --type badger --config '{"db_path":"/data/meta"}'
./dittofsctl store metadata remove fast-meta

# Store Management (Payload/Blocks)
./dittofsctl store payload list
./dittofsctl store payload add --name s3-content --type s3 --config '{"bucket":"my-bucket"}'
./dittofsctl store payload remove s3-content

# Adapter Management
./dittofsctl adapter list
./dittofsctl adapter add --type nfs --port 12049
./dittofsctl adapter update nfs --config '{"port":2049}'
./dittofsctl adapter remove smb

# Settings
./dittofsctl settings list
./dittofsctl settings get logging.level
./dittofsctl settings set logging.level DEBUG

# Shell completion
./dittofsctl completion bash > /etc/bash_completion.d/dittofsctl
./dittofsctl completion zsh > ~/.zsh/completions/_dittofsctl
```

#### Output Formats

All list commands support multiple output formats:

```bash
./dittofsctl user list              # Default table format
./dittofsctl user list -o json      # JSON format
./dittofsctl user list -o yaml      # YAML format
```

#### REST API

```bash
# Login to get JWT token
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin"}' | jq -r '.access_token')

# List users
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/users

# Create a user
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

# Run DittoFS
docker run -d \
  --name dittofs \
  -p 12049:12049 \
  -p 12445:12445 \
  -p 8080:8080 \
  -p 9090:9090 \
  -v ~/.config/dittofs/config.yaml:/config/config.yaml:ro \
  -v dittofs-metadata:/data/metadata \
  -v dittofs-content:/data/content \
  -v dittofs-cache:/data/cache \
  marmos91c/dittofs:latest

# Check health
curl http://localhost:8080/health

# View logs
docker logs -f dittofs
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
- Persistent volume claims for metadata and content stores
- Service exposure for NFS/SMB protocols

See the [`operator/`](operator/) directory for detailed documentation and configuration options.

### Mount from Client

**NFS:**
```bash
# Linux
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS
mkdir -p /tmp/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/nfs
```

**SMB** (requires user authentication):
```bash
# First, create a user with dittofsctl (server must be running)
./dittofsctl login --server http://localhost:8080 --username admin
./dittofsctl user create --username alice  # Password prompted interactively
./dittofsctl share permission grant /export --user alice --level read-write

# Linux (using credentials file for security)
sudo mkdir -p /mnt/smb
echo -e "username=alice\npassword=secret" > ~/.smbcredentials && chmod 600 ~/.smbcredentials
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

Different tenants get isolated metadata and content stores for security and billing separation.

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
- **[NFS Implementation](docs/NFS.md)** - NFSv3 protocol status and client usage
- **[SMB Implementation](docs/SMB_IMPLEMENTATION_PLAN.md)** - SMB2 protocol status, capabilities, and roadmap
- **[Contributing](docs/CONTRIBUTING.md)** - Development guide and contribution guidelines
- **[Implementing Stores](docs/IMPLEMENTING_STORES.md)** - Guide for implementing custom metadata and content stores

### Operational Guides

- **[Known Limitations](docs/KNOWN_LIMITATIONS.md)** - NFS protocol and implementation limitations
- **[Troubleshooting](docs/TROUBLESHOOTING.md)** - Common issues and solutions
- **[Security](docs/SECURITY.md)** - Security considerations and best practices
- **[FAQ](docs/FAQ.md)** - Frequently asked questions

### Development

- **[CLAUDE.md](CLAUDE.md)** - Detailed guidance for Claude Code and developers
- **[Releasing](docs/RELEASING.md)** - Release process and versioning

## Current Status

### ✅ Implemented

**NFS Adapter (NFSv3)**
- All core read/write operations (28 procedures)
- Mount protocol support
- TCP transport with graceful shutdown
- Buffer pooling and performance optimizations
- Read/write caching with background flush

**SMB2 Protocol Adapter**
- SMB2 dialect 0x0202 negotiation
- NTLM authentication with SPNEGO
- Session management with adaptive credit flow control
- Tree connect with share-level permission checking
- File operations: CREATE, READ, WRITE, CLOSE, FLUSH
- Directory operations: QUERY_DIRECTORY
- Metadata operations: QUERY_INFO, SET_INFO
- Compound request handling (CREATE+QUERY_INFO+CLOSE)
- Read/write caching (shared with NFS)
- Parallel request processing
- macOS Finder and smbclient compatible

**Storage Backends**
- In-memory metadata (ephemeral, fast)
- BadgerDB metadata (persistent, path-based handles)
- PostgreSQL metadata (persistent, distributed)
- In-memory block store (ephemeral, testing)
- S3 block store (production-ready with range reads, streaming uploads, stats caching)

**Caching & Persistence**
- Slice-aware cache with sequential write optimization
- WAL (Write-Ahead Log) persistence for crash recovery
- Transfer manager for async cache-to-block-store flushing

**POSIX Compliance**
- 99.99% pass rate on pjdfstest (8,788/8,789 tests)
- All metadata stores (Memory, BadgerDB, PostgreSQL) achieve parity
- Single expected failure due to NFSv3 32-bit timestamp limitation (year 2106)
- See [Known Limitations](docs/KNOWN_LIMITATIONS.md) for details

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
- [ ] SMBv3 support (encryption, multichannel)
- [ ] File locking (oplocks, byte-range locks)
- [ ] Security descriptors and Windows ACLs
- [ ] Extended attributes (xattrs) support
- [ ] Kerberos/LDAP/Active Directory integration

**Kubernetes Integration**
- [x] Kubernetes Operator for deployment
- [x] Health check endpoints
- [ ] CSI driver implementation

**Advanced Features**
- [ ] Sync between DittoFS replicas
- [ ] Scan content stores to populate metadata stores
- [x] Admin REST API for users/permissions/shares/configs
- [ ] NFSv4 support
- [ ] Advanced caching strategies

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for complete roadmap.

## Configuration Example

```yaml
# Control plane database (stores users, groups, shares)
database:
  type: sqlite  # or "postgres" for HA
  sqlite:
    path: /var/lib/dittofs/controlplane.db

# REST API server (enabled by default)
server:
  api:
    enabled: true
    port: 8080
    jwt:
      secret: "your-secret-key-at-least-32-characters"

# Define named stores (reusable across shares)
metadata:
  stores:
    badger-main:
      type: badger
      badger:
        db_path: /var/lib/dittofs/metadata

payload:
  stores:
    s3-cloud:
      type: s3
      s3:
        region: us-east-1
        bucket: my-dittofs-bucket

# Define shares with permissions
shares:
  - name: /archive
    metadata: badger-main
    payload: s3-cloud
    default_permission: read  # Allows guest access with read-only permissions

adapters:
  nfs:
    enabled: true
    port: 12049
  smb:
    enabled: true
    port: 12445
```

> **Note**: Users and groups are managed via CLI (`dittofs user/group`) or REST API, not in the config file.

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
- Basic AUTH_UNIX only (no Kerberos)
- No built-in encryption
- Use behind VPN or with network encryption

See [docs/SECURITY.md](docs/SECURITY.md) for details and recommendations.

## References

### Specifications
- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3
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
