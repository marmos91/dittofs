# Frequently Asked Questions

Common questions about DittoFS and their answers.

## Table of Contents

- [General Questions](#general-questions)
- [Technical Questions](#technical-questions)
- [Usage Questions](#usage-questions)
- [Comparison Questions](#comparison-questions)
- [Known Limitations](#known-limitations)
  - [NFS Protocol Limitations](#nfs-protocol-limitations)
  - [SMB Client Limitations](#smb-client-limitations)
  - [Storage Backend Limitations](#storage-backend-limitations)
  - [General Limitations](#general-limitations)
  - [POSIX Compliance Summary](#posix-compliance-summary)

## General Questions

### What is DittoFS?

DittoFS is a modular virtual filesystem written entirely in Go that decouples file access protocols
from storage backends. It supports NFSv3, NFSv4/v4.1, and SMB2 with pluggable metadata and payload
repositories, making it easy to serve files over multiple protocols from various backends (memory,
filesystem, S3, BadgerDB, PostgreSQL, etc.).

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

Use it for development, testing, and experimentation, but wait for a stable 1.0 release before production use.

### What license is DittoFS under?

DittoFS is released under the MIT License, which is permissive and allows commercial use.

## Technical Questions

### Which NFS versions are supported?

DittoFS supports **NFSv3 over TCP** (28 procedures fully implemented), **NFSv4.0**, and **NFSv4.1** with features including:
- Compound operations and sessions
- File and directory delegations with CB_NOTIFY
- ACLs (Access Control Lists)
- Kerberos authentication via RPCSEC_GSS

### Does it support file locking?

NFSv3 does not include locking (NLM not implemented). However, NFSv4 provides built-in file locking support. SMB2 supports byte-range locking (shared and exclusive).

### Does it support Kerberos authentication?

Yes. NFSv4 supports Kerberos via RPCSEC_GSS, and SMB supports Kerberos via SPNEGO alongside NTLM.

### Can I implement my own protocol adapter?

Yes! That's one of the main goals of DittoFS. Implement the `Adapter` interface and wire it to the metadata/payload stores:

```go
type Adapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    SetRuntime(*runtime.Runtime)
    Protocol() string
    Port() int
}
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for details.

### Can I implement my own storage backend?

Absolutely! Implement either or both of these interfaces:

- **Metadata Store**: `pkg/metadata/Store` interface
- **Payload Store**: `pkg/payload/store/BlockStore` interface

See [IMPLEMENTING_STORES.md](IMPLEMENTING_STORES.md) for implementation guidelines.

### How does performance compare to kernel NFS?

The lack of FUSE overhead and optimized Go implementation provides competitive performance for most workloads. Results show:

- Good sequential read/write performance
- Efficient handling of small files
- Low latency for metadata operations
- Scales well with concurrent connections

### Does metadata persist across server restarts?

It depends on the metadata store:

- **Memory backend** (`type: memory`): No, all data is lost on restart
- **BadgerDB backend** (`type: badger`): Yes, all metadata persists
- **PostgreSQL backend** (`type: postgres`): Yes, all metadata persists across restarts and supports distributed deployments

Configure your metadata store accordingly:

```bash
./dfsctl store metadata add --name persistent --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'
```

### Can I import an existing filesystem into DittoFS?

Not yet, but the path-based file handle strategy in BadgerDB enables this as a future feature. The
handles are deterministic based on file paths (`shareName:/path/to/file`), making filesystem scanning
and import possible.

### Is content deduplication supported?

Not currently, but the payload store abstraction allows for implementing content-addressable storage
with deduplication. This could be added as a custom payload store or a wrapper around existing stores.

## Usage Questions

### Can I use this with Windows clients?

Yes. DittoFS supports SMB2, which is the native Windows file sharing protocol. Windows clients can connect directly without additional software. NFS is also available (Windows 10 Pro and Enterprise include an NFS client).

### How do I mount DittoFS shares?

**Linux (NFS):**
```bash
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
```

**macOS (NFS):**
```bash
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport localhost:/export /mnt/test
```

**Windows (SMB):**
```powershell
net use Z: \\localhost\export /user:username password
```

See [NFS.md](NFS.md) and [SMB.md](SMB.md) for more details.

### Can I have multiple shares with different backends?

Yes! This is a core feature. Create stores and shares via CLI:

```bash
# Create metadata stores
./dfsctl store metadata add --name fast-memory --type memory
./dfsctl store metadata add --name persistent-db --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'

# Create payload stores
./dfsctl store payload add --name local-disk --type filesystem \
  --config '{"path":"/var/lib/dfs/content"}'
./dfsctl store payload add --name cloud-s3 --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'

# Create shares referencing different stores
./dfsctl share create --name /temp --metadata fast-memory --payload local-disk
./dfsctl share create --name /archive --metadata persistent-db --payload cloud-s3
```

See [CONFIGURATION.md](CONFIGURATION.md) for more examples.

### Can multiple shares share the same metadata store?

Yes! Multiple shares can reference the same store instance for resource efficiency:

```bash
# Create one shared metadata store
./dfsctl store metadata add --name shared-meta --type badger \
  --config '{"path":"/var/lib/dfs/shared-metadata"}'

# Create separate payload stores
./dfsctl store payload add --name s3-prod --type s3 \
  --config '{"region":"us-east-1","bucket":"prod-bucket"}'
./dfsctl store payload add --name s3-archive --type s3 \
  --config '{"region":"us-east-1","bucket":"archive-bucket"}'

# Both shares use the same metadata store
./dfsctl share create --name /prod --metadata shared-meta --payload s3-prod
./dfsctl share create --name /archive --metadata shared-meta --payload s3-archive
```

### How do I enable debug logging?

**Via environment variable:**
```bash
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
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
| Metadata Backend | Filesystem only | Pluggable (Memory/BadgerDB/PostgreSQL) |
| Language | C/C++ | Pure Go |
| Deployment | Complex (kernel modules) | Single binary |
| Multi-protocol | Separate servers | Unified (NFS + SMB) |
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
- Multi-share and multi-protocol support (NFS + SMB)
- Extensive configuration system
- Multiple backend options
- Production features (metrics, rate limiting, graceful shutdown)
- NFSv4/v4.1 support with delegations and Kerberos

### What's unique about DittoFS?

1. **Store Registry Pattern**: Named, reusable stores that can be shared across exports
2. **Multi-Protocol**: NFS (v3, v4, v4.1) and SMB2 from a single server
3. **Production-Oriented**: Built-in metrics, rate limiting, graceful shutdown
4. **Flexible Storage**: Mix and match backends per share
5. **Pure Go**: Easy deployment, no C dependencies
6. **Modern Architecture**: Designed for cloud-native deployments

## Known Limitations

### NFS Protocol Limitations

These limitations are fundamental constraints of the NFSv3 protocol. Many are resolved by NFSv4.

#### ETXTBSY (Text File Busy)

| Status | Reason |
|--------|--------|
| Not supported | NFS protocol limitation |

NFS servers have no way to know if any client is executing a file, so ETXTBSY cannot be enforced. This affects all NFS implementations. In practice, most package managers remove-then-replace rather than overwrite executables.

#### Timestamps (Y2106 Limitation)

| Status | Reason |
|--------|--------|
| NFSv3: Max 2106-02-07 | NFSv3 uses 32-bit unsigned seconds |
| NFSv4: No practical limit | NFSv4 uses 64-bit timestamps |

NFSv3's `nfstime3` structure uses a 32-bit unsigned integer for seconds since Unix epoch. NFSv4 resolves this with 64-bit timestamps.

#### File Locking (NFSv3)

| Status | Reason |
|--------|--------|
| NFSv3: Not implemented | NLM protocol not implemented |
| NFSv4: Supported | Built-in locking |

NFSv3 relies on the NLM (Network Lock Manager) protocol for locking, which is not implemented. NFSv4 has built-in locking support.

#### Extended Attributes

| Status | Reason |
|--------|--------|
| Not supported | Not in NFSv3 base specification |

Extended attributes (xattrs) are not part of NFSv3. They require NFS extensions (RFC 8276 for NFSv4.2).

#### fallocate/posix_fallocate

| Status | Reason |
|--------|--------|
| Not supported | No ALLOCATE procedure in NFSv3 |

NFSv3 has no procedure for pre-allocating disk space. Space is allocated on actual write.

### SMB Client Limitations

#### macOS Mount Owner-Only Access

| Status | Reason |
|--------|--------|
| Handled by dfsctl | Apple security restriction - only mount owner can access |

macOS restricts SMB mount access to the mount owner regardless of Unix permissions. When using `sudo dfsctl share mount`, it automatically uses `sudo -u $SUDO_USER` to mount as your user. See [SMB.md](SMB.md) for workarounds.

### Storage Backend Limitations

#### Hard Links

All backends (Memory, BadgerDB, PostgreSQL) fully support hard links via the NFS LINK procedure.

#### Special Files

| Type | Status | Notes |
|------|--------|-------|
| Character devices | Metadata only | MKNOD creates entry, no device functionality |
| Block devices | Metadata only | MKNOD creates entry, no device functionality |
| FIFOs | Metadata only | MKNOD creates entry, no pipe functionality |
| Sockets | Metadata only | MKNOD creates entry, no socket functionality |

DittoFS can create special file entries via MKNOD, but they don't function as actual devices, pipes, or sockets.

### General Limitations

#### Single Node Only

DittoFS currently runs as a single server instance:
- No clustering or high availability
- No replication (except via S3 bucket replication)
- Single point of failure

#### Security

DittoFS is experimental and has not been security audited. See [SECURITY.md](SECURITY.md) for detailed recommendations.

### POSIX Compliance Summary

DittoFS achieves **99.99% pass rate** on [pjdfstest](https://github.com/saidsay-so/pjdfstest) POSIX compliance tests (8789 tests, 1 expected failure).

This pass rate applies to **all metadata backends** (Memory, BadgerDB, PostgreSQL).

| Metric | Value |
|--------|-------|
| Total tests | 8789 |
| Passed | 8788 |
| Failed (expected) | 1 |
| Pass rate | 99.99% |

#### Expected Failures

| Test Pattern | Reason |
|--------------|--------|
| `utimensat/09.t:test5` | NFSv3 32-bit timestamp limit (max year 2106) |
| `open::etxtbsy` | NFS protocol limitation (not testable) |
| `flock/*` | NLM not implemented (NFSv3 only) |
| `fcntl/lock*` | NLM not implemented (NFSv3 only) |
| `lockf/*` | NLM not implemented (NFSv3 only) |
| `xattr/*`, `*xattr/*` | Not in NFSv3 |
| `fallocate/*` | No ALLOCATE in NFSv3 |
| `chflags/*` | BSD-specific |

**Note**: Only `utimensat/09.t:test5` actually fails in current pjdfstest runs. Other patterns either don't have tests in the suite or the tests are skipped.

See `test/posix/known_failures.txt` for the complete list with detailed explanations.

## Still Have Questions?

- Check the other documentation in [docs/](.)
- Search [existing GitHub issues](https://github.com/marmos91/dittofs/issues)
- Open a [new issue](https://github.com/marmos91/dittofs/issues/new) for bugs or feature requests
- Review [CLAUDE.md](../CLAUDE.md) for detailed development guidance
