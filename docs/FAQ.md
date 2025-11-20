# Frequently Asked Questions

Common questions about DittoFS and their answers.

## Table of Contents

- [General Questions](#general-questions)
- [Technical Questions](#technical-questions)
- [Usage Questions](#usage-questions)
- [Comparison Questions](#comparison-questions)

## General Questions

### What is DittoFS?

DittoFS is a modular virtual filesystem written entirely in Go that decouples file access protocols
from storage backends. It currently supports NFSv3 with pluggable metadata and content repositories,
making it easy to serve files over NFS from various backends (memory, filesystem, S3, BadgerDB, etc.).

### Why not use FUSE?

FUSE adds an additional abstraction layer and requires kernel modules. DittoFS runs entirely in
userspace and implements protocols directly, giving better control over protocol behavior, easier
debugging, and no kernel dependencies. This also makes deployment simpler - just a single binary with
no special permissions.

### Can I use this in production?

**Not yet**. DittoFS is experimental and needs:
- More testing and hardening
- Security auditing
- Performance optimization
- Production deployment experience
- Better documentation and tooling

Use it for development, testing, and experimentation, but wait for a stable 1.0 release before production use.

### What license is DittoFS under?

DittoFS is released under the MIT License, which is permissive and allows commercial use.

## Technical Questions

### Which NFS version is supported?

Currently **NFSv3 over TCP** is fully supported with 28 procedures implemented. NFSv4 support is planned for a future phase.

### Does it support file locking?

Not yet. The NLM (Network Lock Manager) protocol is not currently implemented. This is planned for future development.

### Can I implement my own protocol adapter?

Yes! That's one of the main goals of DittoFS. Implement the `Adapter` interface and wire it to the metadata/content stores:

```go
type Adapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    SetRegistry(*registry.Registry)
    Protocol() string
    Port() int
}
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for details.

### Can I implement my own storage backend?

Absolutely! Implement either or both of these interfaces:

- **Metadata Store**: `pkg/store/metadata/Store` interface
- **Content Store**: `pkg/store/content/Store` interface

See [CONTRIBUTING.md](CONTRIBUTING.md) for implementation guidelines.

### How does performance compare to kernel NFS?

We're still benchmarking comprehensively. The lack of FUSE overhead and optimized Go implementation should provide competitive performance for most workloads. Preliminary results show:

- Good sequential read/write performance
- Efficient handling of small files
- Low latency for metadata operations
- Scales well with concurrent connections

### Does metadata persist across server restarts?

It depends on the metadata store:

- **Memory backend** (`type: memory`): No, all data is lost on restart
- **BadgerDB backend** (`type: badger`): Yes, all metadata persists

Configure your metadata store accordingly:

```yaml
metadata:
  stores:
    persistent:
      type: badger
      badger:
        db_path: /var/lib/dittofs/metadata
```

### Can I import an existing filesystem into DittoFS?

Not yet, but the path-based file handle strategy in BadgerDB enables this as a future feature. The
handles are deterministic based on file paths (`shareName:/path/to/file`), making filesystem scanning
and import possible.

### Is content deduplication supported?

Not currently, but the content store abstraction allows for implementing content-addressable storage
with deduplication. This could be added as a custom content store or a wrapper around existing stores.

## Usage Questions

### Can I use this with Windows clients?

Yes, Windows can mount NFS shares (Windows 10 Pro and Enterprise include an NFS client). However, the SMB/CIFS adapter will provide better Windows integration when implemented.

To enable NFS client on Windows:
```powershell
# Run as Administrator
Enable-WindowsOptionalFeature -FeatureName ServicesForNFS-ClientOnly, ClientForNFS-Infrastructure -Online -NoRestart
```

### How do I mount DittoFS shares?

**Linux:**
```bash
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
```

**macOS:**
```bash
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport localhost:/export /mnt/test
```

**Windows:**
```powershell
mount -o anon \\localhost\export Z:
```

See [NFS.md](NFS.md) for more details.

### Can I have multiple shares with different backends?

Yes! This is a core feature. Each share references stores by name:

```yaml
metadata:
  stores:
    fast-memory:
      type: memory
    persistent-db:
      type: badger
      badger:
        db_path: /var/lib/dittofs/metadata

content:
  stores:
    local-disk:
      type: filesystem
      filesystem:
        path: /var/lib/dittofs/content
    cloud-s3:
      type: s3
      s3:
        region: us-east-1
        bucket: my-bucket

shares:
  - name: /temp
    metadata_store: fast-memory
    content_store: local-disk

  - name: /archive
    metadata_store: persistent-db
    content_store: cloud-s3
```

See [CONFIGURATION.md](CONFIGURATION.md) for more examples.

### Can multiple shares share the same metadata store?

Yes! Multiple shares can reference the same store instance for resource efficiency:

```yaml
metadata:
  stores:
    shared-meta:
      type: badger
      badger:
        db_path: /var/lib/dittofs/shared-metadata

shares:
  - name: /prod
    metadata_store: shared-meta
    content_store: s3-prod

  - name: /archive
    metadata_store: shared-meta  # Same metadata
    content_store: s3-archive    # Different content
```

### How do I enable debug logging?

**Via environment variable:**
```bash
DITTOFS_LOGGING_LEVEL=DEBUG ./dittofs start
```

**Via configuration:**
```yaml
logging:
  level: DEBUG
  format: text
```

### Why do I get "permission denied" errors?

Common causes:

1. **Identity mapping**: Try enabling `map_all_to_anonymous: true` for development
2. **Root directory permissions**: Set `mode: 0777` temporarily to isolate the issue
3. **Client UID mismatch**: Check your UID with `id` command
4. **Export restrictions**: Check `allowed_clients` in configuration

See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for solutions.

## Comparison Questions

### How does DittoFS compare to traditional NFS servers?

| Feature | Traditional NFS | DittoFS |
|---------|----------------|---------|
| Permission Requirements | Kernel-level | Userspace only |
| Storage Backend | Filesystem only | Pluggable |
| Metadata Backend | Filesystem only | Pluggable (Memory/BadgerDB/custom) |
| Language | C/C++ | Pure Go |
| Deployment | Complex (kernel modules) | Single binary |
| Multi-protocol | Separate servers | Unified (planned) |
| Customization | Limited | Full control |

### How does DittoFS compare to cloud storage gateways?

| Feature | Cloud Gateways | DittoFS |
|---------|---------------|---------|
| Vendor Lock-in | Often present | None |
| Protocol Support | Limited | Extensible |
| Storage Backend | Vendor-specific | Pluggable |
| Cost | Often high | Free and open-source |
| Customization | Limited | Full control |
| Deployment | Complex | Single binary |

### How does DittoFS compare to go-nfs?

Both are NFS implementations in Go, but with different goals:

**go-nfs:**
- Library-focused
- Embeddable in other projects
- Minimal configuration

**DittoFS:**
- Complete server application
- Store registry pattern for sharing resources
- Multi-share support
- Extensive configuration system
- Multiple backend options
- Production features (metrics, rate limiting, graceful shutdown)
- Designed for protocol extensibility

### What's unique about DittoFS?

1. **Store Registry Pattern**: Named, reusable stores that can be shared across exports
2. **Multi-Protocol Ready**: Clean adapter interface for adding new protocols
3. **Production-Oriented**: Built-in metrics, rate limiting, graceful shutdown
4. **Flexible Storage**: Mix and match backends per share
5. **Pure Go**: Easy deployment, no C dependencies
6. **Modern Architecture**: Designed for cloud-native deployments

## Still Have Questions?

- Check the other documentation in [docs/](.)
- Search [existing GitHub issues](https://github.com/marmos91/dittofs/issues)
- Open a [new issue](https://github.com/marmos91/dittofs/issues/new) for bugs or feature requests
- Review [CLAUDE.md](../CLAUDE.md) for detailed development guidance
