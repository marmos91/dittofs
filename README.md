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
│         Store Registry               │
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
- ✅ **User Management**: Unified users/groups with share-level permissions (CLI included)

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

### User Management

```bash
# Add a user (prompts for password)
./dittofs user add alice

# Grant share permission
./dittofs user grant alice /export read-write

# Create a group and add user
./dittofs group add editors
./dittofs user join alice editors

# List users and groups
./dittofs user list
./dittofs group list
```

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md#cli-management-commands) for all user/group commands.

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
# First, create a user with the CLI
./dittofs user add alice
./dittofs user grant alice /export read-write

# Linux
sudo mkdir -p /mnt/smb
sudo mount -t cifs //localhost/export /mnt/smb -o port=12445,username=alice,password=yourpassword,vers=2.0

# macOS
mkdir -p /tmp/smb
mount -t smbfs //alice:yourpassword@localhost:12445/export /tmp/smb
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

**User Management**
- Unified identity system for NFS and SMB
- Users with bcrypt password hashing
- Groups with share-level permissions
- Permission resolution: user → group → share default
- CLI tools for user/group management

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
- [ ] Health check endpoints
- [ ] CSI driver implementation

**Advanced Features**
- [ ] Sync between DittoFS replicas
- [ ] Scan content stores to populate metadata stores
- [ ] Admin REST API for users/permissions/shares/configs
- [ ] Web UI for administration
- [ ] NFSv4 support
- [ ] Advanced caching strategies

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for complete roadmap.

## Configuration Example

```yaml
# Define named stores (reusable across shares)
metadata:
  stores:
    badger-main:
      type: badger
      badger:
        db_path: /var/lib/dittofs/metadata

blocks:
  stores:
    s3-cloud:
      type: s3
      s3:
        region: us-east-1
        bucket: my-dittofs-bucket

# User management
groups:
  - name: editors
    gid: 101
    share_permissions:
      /archive: read-write

users:
  - username: alice
    password_hash: "$2a$10$..."  # bcrypt hash
    uid: 1001
    gid: 101
    groups: [editors]

guest:
  enabled: true
  uid: 65534
  gid: 65534

# Define shares with permissions
shares:
  - name: /archive
    metadata_store: badger-main
    blocks_store: s3-cloud
    allow_guest: true
    default_permission: read

adapters:
  nfs:
    enabled: true
    port: 12049
  smb:
    enabled: true
    port: 12445
```

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
