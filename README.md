<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-light.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/logo-dark.svg">
  <img alt="DittoFS" src="assets/logo-dark.svg" width="320">
</picture>

<br>

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Nix Flake](https://img.shields.io/badge/Nix-flake-5277C3?style=flat&logo=nixos)](https://nixos.org/)
[![Go Report Card](https://goreportcard.com/badge/github.com/marmos91/dittofs)](https://goreportcard.com/report/github.com/marmos91/dittofs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange?style=flat)](https://github.com/marmos91/dittofs)

**A modular virtual filesystem written entirely in Go**

NFSv3/v4.0/v4.1 and SMB2/3 servers in userspace — no FUSE, no kernel modules — with
pluggable metadata and block stores you can mix and match per share.

[Website](https://dittofs.io) • [Quick Start](#quick-start) • [Documentation](#documentation) • [Features](#features) • [Contributing](docs/CONTRIBUTING.md)

</div>

---

> ⚠️ **Experimental software, pre-1.0.** Not production ready. No security audit has
> been performed. APIs and on-disk formats may change without notice. Do not use for
> data you cannot afford to lose. See [Security](#security) and [FAQ](docs/FAQ.md).

## What is DittoFS?

Traditional file servers are welded to one storage layer and one access protocol.
DittoFS separates the two. A single server process can:

- Speak **NFSv3, NFSv4.0, NFSv4.1, and SMB2/3** at the same time, over the same data.
- Store metadata in **memory, [BadgerDB](https://github.com/dgraph-io/badger), or [PostgreSQL](https://www.postgresql.org/docs/)** — chosen per share.
- Store file content in a two-tier **block store**: a fast local tier (filesystem or
  memory) backed by a durable remote tier ([S3](https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html) or memory), with an async syncer between them.
- Run **entirely in userspace** — no FUSE, no kernel modules, no special privileges.

Everything is built from **named, reusable stores** wired together into **shares**.
Two binaries drive it:

| Binary | Role |
|--------|------|
| **`dfs`** | The server daemon — runs the protocol adapters and a control-plane REST API. |
| **`dfsctl`** | The REST client — manages users, groups, shares, stores, and adapters on a running server. |

## Features

| Area | Status |
|------|--------|
| **NFSv3** | All 28 core procedures; embedded portmapper + mount protocol |
| **NFSv4.0** | Compound ops, ACLs, delegations, built-in byte-range locking |
| **NFSv4.1** | Sessions, sequence slots, backchannel |
| **SMB 2.0.2 / 3.0 / 3.0.2 / 3.1.1** | Multi-dialect negotiation, preauth integrity, compound requests |
| **SMB3 encryption** | AES-128/256-GCM and AES-128/256-CCM |
| **SMB3 signing** | AES-128-CMAC / AES-128-GMAC (HMAC-SHA256 for 2.x) |
| **SMB leases & oplocks** | Leases V2 with directory leasing; oplocks; byte-range locks |
| **SMB durable handles** | V1 and V2 for session resilience |
| **SMB security descriptors** | Windows ACL mapping via a shared cross-protocol ACL model |
| **Authentication** | AUTH_UNIX + Kerberos (RPCSEC_GSS) for NFS; NTLM + Kerberos (SPNEGO) for SMB |
| **Cross-protocol coordination** | Bidirectional lease/delegation breaks between SMB and NFS |
| **Metadata stores** | Memory, BadgerDB, PostgreSQL — pluggable per share |
| **Block stores** | Local: filesystem, memory. Remote: S3, memory. Per-share isolation, async sync |
| **Client-side encryption** | Per-remote envelope encryption (AES-256-GCM / ChaCha20-Poly1305 / XChaCha20-Poly1305) |
| **Share snapshots** | Point-in-time reference holds (no data copy) with restore |
| **Control plane** | Unified users/groups, share permissions, REST API with JWT auth |
| **Observability** | Prometheus metrics, OpenTelemetry tracing, structured JSON logging |
| **Deployment** | Single static binary; Docker images; Kubernetes operator |

DittoFS passes the pjdfstest POSIX suite at 99.99% (8,788/8,789) across all three
metadata backends; the single expected failure is the NFSv3 32-bit timestamp limit
(year 2106). See the [FAQ](docs/FAQ.md) for known limitations.

On the SMB side, DittoFS passes the Samba **smbtorture** and Microsoft **Windows
Protocol Test Suite (WPTS)** conformance batteries on the implementable surface —
every test that a single-node userspace VFS can satisfy. The remaining known-failures
are genuinely out-of-scope features (RSVD shared-VHD, Service Witness clustering,
Storage QoS, DFS namespaces, kernel oplocks, NTFS-internal pseudo-files) plus a
handful of upstream-Samba known-fails; none are fixable protocol gaps. See
[docs/SMB.md](docs/SMB.md) and [docs/WINDOWS_TESTING.md](docs/WINDOWS_TESTING.md).

## Quick Start

### Install

```bash
# Nix (runs without installing)
nix run github:marmos91/dittofs -- init
nix run github:marmos91/dittofs -- start

# Homebrew
brew tap marmos91/tap
brew install marmos91/tap/dfs marmos91/tap/dfsctl

# Quick install script (macOS / Linux)
curl -fsSL https://github.com/marmos91/dittofs/releases/latest/download/install.sh | sh
```

Docker, the Kubernetes operator, APT/YUM/Arch packages, and Scoop (Windows) are
covered in **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)**.

### Build from source

```bash
git clone https://github.com/marmos91/dittofs.git
cd dittofs
go build -o dfs    cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go
./dfs init     # writes ~/.config/dittofs/config.yaml
./dfs start
```

### First run & admin password

On first start, DittoFS creates an `admin` user. The password is either auto-generated
and printed **once** to the log, or pre-set via the `DITTOFS_ADMIN_INITIAL_PASSWORD`
environment variable (recommended for Docker/Kubernetes/CI, where you can't read
interactive output):

```bash
# Auto-generated — save the password from the log; it is not shown again
./dfs start

# Or choose your own (skips the forced password change on first login)
DITTOFS_ADMIN_INITIAL_PASSWORD=my-secure-password ./dfs start
```

### Serve an NFS share in under a minute

```bash
# 1. Start the server (see above), then log in
./dfsctl login --server http://localhost:8080 --username admin
./dfsctl user change-password

# 2. Create a user mapped to your host UID (needed for NFS write access)
./dfsctl user create --username $(whoami) --host-uid

# 3. Create the stores (interactive prompts collect paths / S3 credentials)
./dfsctl store metadata add --name default --type badger
./dfsctl store block local add  --name local-cache --type fs
./dfsctl store block remote add --name s3-remote   --type s3

# 4. Create a share and grant access
./dfsctl share create --name /export --metadata default \
  --local local-cache --remote s3-remote
./dfsctl share permission grant /export --user $(whoami) --level read-write

# 5. Enable the NFS adapter
./dfsctl adapter enable nfs

# 6. Mount it
# Linux:
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs
# macOS:
sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/nfs

echo "Hello DittoFS!" > /mnt/nfs/hello.txt
```

> This uses persistent storage (BadgerDB metadata, local filesystem cache, S3 durable
> backend). Writes land locally first and sync to S3 in the background. For dependency-free
> local testing, use `--type memory` for both the metadata and block stores instead.

### Mount an SMB share

SMB always requires user authentication:

```bash
./dfsctl user create --username alice            # password prompted
./dfsctl share permission grant /export --user alice --level read-write

# Linux (use a credentials file — never put passwords on the command line)
sudo mount -t cifs //localhost/export /mnt/smb \
  -o port=12445,credentials=$HOME/.smbcredentials,vers=3.1.1

# macOS (prompts for password)
mount -t smbfs //alice@localhost:12445/export /tmp/smb
```

See [docs/SMB.md](docs/SMB.md) for dialect, encryption, and client details.

### Default ports

| Port | Service |
|------|---------|
| `12049` | NFS |
| `12445` | SMB |
| `8080`  | Control-plane REST API (health checks, management) |
| `9090`  | Prometheus metrics |

## Configuration model

DittoFS configuration has **two layers**:

1. **Server config file** (`~/.config/dittofs/config.yaml`) — process-level infrastructure:
   logging, telemetry, control-plane API, and the control-plane database. Environment
   variables with the `DITTOFS_` prefix override any field (e.g. `DITTOFS_LOGGING_LEVEL=DEBUG`).
2. **Runtime resources** (`dfsctl` / REST API) — stores, shares, adapters, users, groups,
   and permissions. These live in the control-plane database (SQLite by default, PostgreSQL
   for HA), not in the config file.

```yaml
# ~/.config/dittofs/config.yaml
database:
  type: sqlite            # or "postgres" for HA
  sqlite:
    path: ~/.config/dittofs/controlplane.db   # default when omitted: $XDG_CONFIG_HOME/dittofs/controlplane.db

controlplane:
  port: 8080
  jwt:
    secret: "your-secret-key-at-least-32-characters"
```

Block storage and caching are configured per share through the store CLI — each share owns
an isolated local storage directory and its own caching tiers. See
[docs/CONFIGURATION.md](docs/CONFIGURATION.md) for the full reference.

## Documentation

**Getting started & operations**

- [Configuration](docs/CONFIGURATION.md) — config file, environment variables, stores, shares, adapters
- [CLI Reference](docs/CLI.md) — every `dfs` and `dfsctl` command (generated)
- [Deployment](docs/DEPLOYMENT.md) — Docker, Kubernetes operator, package managers
- [Troubleshooting](docs/TROUBLESHOOTING.md) — common issues and fixes
- [Security](docs/SECURITY.md) — threat model, authentication, recommendations
- [FAQ](docs/FAQ.md) — known limitations and design questions

**Protocols & features**

- [NFS](docs/NFS.md) — NFSv3/v4.0/v4.1 status, mounting, internals
- [SMB](docs/SMB.md) — SMB2/3 status, encryption, signing, leases, durable handles
- [ACLs](docs/ACLS.md) — the cross-protocol ACL model shared by NFS and SMB
- [Encryption](docs/ENCRYPTION.md) — client-side per-remote envelope encryption
- [Snapshots](docs/SNAPSHOTS.md) — reference-based share snapshots and restore
- [Windows Testing](docs/WINDOWS_TESTING.md) — SMB conformance against Windows
- [Glossary](docs/GLOSSARY.md) — plain-language definitions of protocol, ACL, and storage terms

**Architecture & development**

- [Architecture](docs/ARCHITECTURE.md) — design, components, directory map
- [Contributing](docs/CONTRIBUTING.md) — dev setup, workflow, testing
- [Debugging](docs/DEBUGGING.md) — protocol interop and pcap-diff playbook
- [Implementing Stores](docs/IMPLEMENTING_STORES.md) — metadata/block store contracts
- [Block Store Migration](docs/BLOCKSTORE_MIGRATION.md) — migrating legacy block layouts to CAS
- [Benchmarks](docs/BENCHMARKS.md) — performance harness and results
- [Releasing](docs/RELEASING.md) — release process and versioning

## Testing

```bash
go test ./...                    # unit + integration
go test -race ./...              # with the race detector

# E2E (needs sudo + a kernel NFS client)
cd test/e2e && sudo ./run-e2e.sh
```

See [test/e2e/](test/e2e/) (and [test/e2e/BENCHMARKS.md](test/e2e/BENCHMARKS.md)) and [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md).

## Security

DittoFS is experimental and has not been professionally audited.

- **Authentication:** AUTH_UNIX and Kerberos (RPCSEC_GSS) for NFS; NTLM and Kerberos
  (SPNEGO) for SMB.
- **Transport encryption:** SMB3 encrypts its transport (AES-GCM/CCM). NFS has no transport
  encryption — run it over a VPN or a trusted network.
- **At-rest:** optional client-side per-remote envelope encryption protects block content in
  the remote store. See [docs/ENCRYPTION.md](docs/ENCRYPTION.md).

See [docs/SECURITY.md](docs/SECURITY.md) for the full picture and recommendations.

## Contributing

Contributions are welcome — see [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md) for development
setup, code structure, and testing guidelines.

## References

New to these protocols? Start with the [Glossary](docs/GLOSSARY.md) for plain-language
definitions, then dive into the authoritative specs below.

- [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813) — NFS Version 3
- [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530) — NFS Version 4.0
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881) — NFS Version 4.1
- [RFC 5531](https://www.rfc-editor.org/rfc/rfc5531) — ONC RPC Protocol
- [RFC 4506](https://www.rfc-editor.org/rfc/rfc4506) — XDR Standard
- [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) — SMB2/3 protocol
- [MS-DTYP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) — SID, ACL, ACE, and security descriptor formats
- [MS-NLMP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) — NTLM authentication
- [RFC 4120](https://www.rfc-editor.org/rfc/rfc4120) — Kerberos V5 · [RFC 4178](https://www.rfc-editor.org/rfc/rfc4178) — SPNEGO · [RFC 2743](https://www.rfc-editor.org/rfc/rfc2743) — GSS-API

## License

MIT — see [LICENSE](LICENSE).
