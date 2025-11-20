<div align="center">

# DittoFS

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Tests](https://img.shields.io/badge/tests-passing-brightgreen?style=flat)](https://github.com/marmos91/dittofs)
[![Go Report Card](https://goreportcard.com/badge/github.com/marmos91/dittofs)](https://goreportcard.com/report/github.com/marmos91/dittofs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange?style=flat)](https://github.com/marmos91/dittofs)

**A modular virtual filesystem written entirely in Go**

Decouple file interfaces from storage backends. NFSv3 server with pluggable metadata and content stores. Designed for easy extension to additional protocols.

[Quick Start](#quick-start) • [Documentation](#documentation) • [Features](#features) • [Use Cases](#use-cases) • [Contributing](docs/CONTRIBUTING.md)

</div>

---

## Overview

DittoFS provides a modular architecture with **named, reusable stores** that can be mixed and matched per share:

```
┌──────────────────────────────────────┐
│       Protocol Adapters              │
│   NFS ✅  SMB(soon)  WebDAV(TBD)     │
└──────────────┬───────────────────────┘
               │
               ▼
┌──────────────────────────────────────┐
│         Store Registry               │
│  Metadata Stores  │  Content Stores  │
│  • Memory         │  • Filesystem    │
│  • BadgerDB       │  • S3            │
│  • Custom         │  • Memory        │
└──────────────────────────────────────┘
```

### Key Concepts

- **Protocol Adapters**: Multiple protocols (NFS, SMB, etc.) can run simultaneously
- **Shares**: Export points that clients mount, each referencing specific stores
- **Named Store Registry**: Reusable store instances that can be shared across exports
- **Pluggable Storage**: Mix and match metadata and content backends per share

## Features

✅ **Production-Ready NFSv3**: 28 procedures fully implemented
✅ **No Special Permissions**: Runs entirely in userspace - no FUSE, no kernel modules
✅ **Pluggable Storage**: Mix protocols with any backend (S3, filesystem, custom)
✅ **Cloud-Native**: S3 backend with production optimizations
✅ **Pure Go**: Single binary, easy deployment, cross-platform
✅ **Extensible**: Clean adapter pattern for new protocols

## Quick Start

### Installation

```bash
# Build from source
go build -o dittofs cmd/dittofs/main.go

# Initialize configuration (creates ~/.config/dittofs/config.yaml)
./dittofs init

# Start server
./dittofs start
```

### Mount from Client

```bash
# Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs
```

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
- **[Contributing](docs/CONTRIBUTING.md)** - Development guide and contribution guidelines

### Operational Guides

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

**Storage Backends**
- In-memory metadata (ephemeral, fast)
- BadgerDB metadata (persistent, path-based handles)
- Filesystem content (local/network storage)
- S3 content (production-ready with range reads, streaming uploads, stats caching)

**Production Features**
- Prometheus metrics integration
- Request rate limiting
- Enhanced graceful shutdown
- Comprehensive E2E test suite
- Performance benchmark framework

### 🚀 Roadmap

**Phase 2: Kubernetes Integration**
- [ ] Health check endpoints
- [ ] CSI driver implementation
- [ ] Helm charts
- [ ] Load testing

**Phase 3: SMB Protocol Adapter** (Optional)
- [ ] SMB2/3 protocol
- [ ] NTLM authentication
- [ ] Windows compatibility

**Phase 4: Advanced Features**
- [ ] NFSv4 support
- [ ] Kerberos authentication
- [ ] Advanced caching strategies
- [ ] Multi-region replication

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for complete roadmap.

## Configuration Example

```yaml
# Define named stores (reusable across shares)
metadata:
  stores:
    memory-fast:
      type: memory
    badger-main:
      type: badger
      badger:
        db_path: /var/lib/dittofs/metadata

content:
  stores:
    local-disk:
      type: filesystem
      filesystem:
        path: /var/lib/dittofs/content
    s3-cloud:
      type: s3
      s3:
        region: us-east-1
        bucket: my-dittofs-bucket

# Define shares that reference stores
shares:
  - name: /temp
    metadata_store: memory-fast
    content_store: local-disk

  - name: /archive
    metadata_store: badger-main
    content_store: s3-cloud

adapters:
  nfs:
    enabled: true
    port: 12049
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
