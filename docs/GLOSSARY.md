# Glossary

Plain-language definitions of the protocol, security, and storage terms used
throughout the DittoFS documentation. Each entry links to the authoritative
specification for readers who want the full detail. Protocol-specific glossaries
also live in [NFS.md](NFS.md#glossary) and [SMB.md](SMB.md#glossary).

## Filesystem protocols

| Term | Definition |
|------|------------|
| **NFS** (Network File System) | A protocol that lets a client read and write files on a remote server as if they were local. DittoFS speaks NFSv3, v4.0, and v4.1. [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813) (v3), [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530) (v4.0), [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881) (v4.1) |
| **SMB** (Server Message Block) | Microsoft's network file-sharing protocol, used natively by Windows and macOS. DittoFS speaks SMB 2.0.2 through 3.1.1. [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) |
| **CIFS** (Common Internet File System) | An older name for SMB; the two terms are used interchangeably. [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) |
| **Export** | An NFS term for a directory the server makes available to clients. The SMB equivalent is a "share". |
| **Share** | A named, network-accessible storage area. In DittoFS a share binds a metadata store and a block store together; both NFS and SMB serve the same shares. |
| **RPC** (Remote Procedure Call) | The request/response framing NFS is built on: a client invokes a numbered procedure on the server and gets a reply. DittoFS uses ONC RPC. [RFC 5531](https://www.rfc-editor.org/rfc/rfc5531) |
| **XDR** (External Data Representation) | The canonical binary encoding used to serialize RPC/NFS data structures on the wire. [RFC 4506](https://www.rfc-editor.org/rfc/rfc4506) |
| **NLM** (Network Lock Manager) | The sideband protocol NFSv3 uses for file locking (NFSv3 itself is stateless). [Open Group NLM](https://pubs.opengroup.org/onlinepubs/9629799/chap10.htm) |
| **NSM / statd** (Network Status Monitor) | The companion to NLM that tracks client/server reboots so locks can be reclaimed or released after a crash. [Open Group NSM](https://pubs.opengroup.org/onlinepubs/9629799/chap11.htm) |
| **Mount protocol** | The NFSv3 helper protocol that turns an export path into the initial file handle a client uses to start accessing files. |
| **File handle** | An opaque, server-issued identifier for a file or directory. Clients pass it back on every operation; they never interpret its bytes. |
| **stateid** | An NFSv4 identifier that names a piece of open/lock state on the server, so the server can tie an operation to a specific open or lock. |
| **WCC** (Weak Cache Consistency) | NFSv3 data returning a file's attributes both before and after an operation, so a client can cheaply tell whether its cached copy is still valid. |
| **Delegation** | An NFSv4 grant that lets a client cache a file (or directory) and operate on it locally until the server recalls the grant. The SMB analog is a lease/oplock. |

## SMB caching and resilience

| Term | Definition |
|------|------------|
| **Oplock** (opportunistic lock) | An SMB caching hint that lets a client buffer reads/writes locally until another client needs the file. |
| **Lease** | The SMB 2.1+ successor to oplocks: finer-grained (read/write/handle) caching rights. DittoFS implements Lease V2 with directory leasing. [MS-SMB2 §2.2.13.2.8](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) |
| **Durable / persistent handle** | An open file handle a client can reclaim after a brief network drop (durable) or a server restart (persistent), avoiding data loss on transient failures. |

## Identity, permissions, and ACLs

| Term | Definition |
|------|------------|
| **ACL** (Access Control List) | The ordered list of rules that decides who may do what to a file. DittoFS uses one cross-protocol ACL model for both NFS and SMB. See [ACLS.md](ACLS.md). |
| **ACE** (Access Control Entry) | A single rule inside an ACL — e.g. "allow user X to read and write". [MS-DTYP §2.4.4](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **DACL** (Discretionary ACL) | The part of a security descriptor that grants or denies access. "Discretionary" because the object's owner controls it. [MS-DTYP §2.4.5](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **SACL** (System ACL) | The part of a security descriptor that defines auditing (which accesses get logged), set by an administrator rather than the owner. [MS-DTYP §2.4.5](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **Security descriptor** | The Windows/SMB structure that bundles a file's owner, group, DACL, and SACL together. [MS-DTYP §2.4.6](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **SID** (Security Identifier) | The Windows-style unique identifier for a user or group (e.g. `S-1-5-21-...`). DittoFS maps SIDs to and from Unix UIDs/GIDs and NFSv4 `user@domain` names. [MS-DTYP §2.4.2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) |
| **Root-squash** | An export option that maps a client's `root` (UID 0) to an unprivileged user, so a remote root user can't gain root power on the server's files. |
| **All-squash** | An export option that maps *every* client user to a single unprivileged user, regardless of who they claim to be. |

## Authentication

| Term | Definition |
|------|------------|
| **Kerberos** | A ticket-based network authentication protocol. DittoFS supports it for both NFS (via RPCSEC_GSS) and SMB (via SPNEGO). [RFC 4120](https://www.rfc-editor.org/rfc/rfc4120) |
| **GSS-API** | A standard programming interface that lets applications use security mechanisms like Kerberos without hard-coding them. [RFC 2743](https://www.rfc-editor.org/rfc/rfc2743) |
| **SPNEGO** (Simple and Protected GSS-API Negotiation) | The wrapper that lets an SMB client and server agree on whether to use Kerberos or NTLM. [RFC 4178](https://www.rfc-editor.org/rfc/rfc4178) |
| **NTLM / NTLMSSP** | Microsoft's challenge/response authentication scheme, used by SMB when Kerberos isn't available. NTLMSSP is the GSS-API wrapping of NTLM. [MS-NLMP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) |
| **RPCSEC_GSS** | The NFS/RPC security flavor that carries GSS-API (Kerberos) authentication and optional integrity/privacy. [RFC 2203](https://www.rfc-editor.org/rfc/rfc2203) |
| **AUTH_UNIX** | The basic NFS authentication flavor that simply asserts a UID/GID — trusted only on a trusted network. |

## Storage internals

| Term | Definition |
|------|------------|
| **CAS** (Content-Addressed Storage) | Storage that names each block by the hash of its contents rather than by location, so identical data is stored once and deduplicated automatically. |
| **FastCDC** | A content-defined chunking algorithm that splits file data into variable-size chunks at content-based boundaries, so small edits only re-chunk the affected region. [FastCDC paper (USENIX ATC '16)](https://www.usenix.org/conference/atc16/technical-sessions/presentation/xia) |
| **BLAKE3** | The fast cryptographic hash DittoFS uses to address CAS blocks and verify them end-to-end. [BLAKE3 specification](https://github.com/BLAKE3-team/BLAKE3-specs) |
| **Block store** | The per-share content layer: a fast local tier (filesystem or memory) backed by a durable remote tier ([S3](https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html) or memory), with an async syncer between them. |
| **Metadata store** | The pluggable backend that holds the directory tree, file attributes, and ACLs — memory, [BadgerDB](https://github.com/dgraph-io/badger), or [PostgreSQL](https://www.postgresql.org/docs/), chosen per share. |

## Implementation and tooling

External components and standards DittoFS builds on. Links go to each project's
own documentation for readers who want to go deeper.

| Term | What it is |
|------|------------|
| **S3 API** | The HTTP object-storage API used for the durable remote block tier; works against Amazon S3 and S3-compatible stores (MinIO, Ceph, …). [S3 API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html) |
| **BadgerDB** | An embedded key/value store (pure Go) — one of the metadata-store backends. [Badger](https://github.com/dgraph-io/badger) |
| **PostgreSQL** | A relational database — the production-grade metadata-store backend. [PostgreSQL docs](https://www.postgresql.org/docs/) |
| **SQLite** | The embedded SQL database used for the control-plane store by default. [SQLite](https://www.sqlite.org/docs.html) |
| **Cobra** | The Go library that builds the `dfs`/`dfsctl` command trees (subcommands, flags, help). [Cobra](https://github.com/spf13/cobra) |
| **Viper** | The configuration library that layers the config file, environment variables, and flags. [Viper](https://github.com/spf13/viper) |
| **XDG Base Directory Specification** | The Unix convention that places config under `~/.config` and state under `~/.local/state`; DittoFS follows it for its config and state directories. [XDG spec](https://specifications.freedesktop.org/basedir-spec/latest/) |
| **JWT** (JSON Web Token) | The signed token format the REST API uses for authenticated sessions. [RFC 7519](https://www.rfc-editor.org/rfc/rfc7519) |
| **pprof** | Go's built-in CPU/memory/blocking profiler; DittoFS can expose it for performance debugging. [pprof](https://pkg.go.dev/net/http/pprof) |
| **Prometheus** | The metrics format/scraper DittoFS exposes operational metrics in. [Prometheus](https://prometheus.io/docs/) |

## See also

- [NFS.md](NFS.md#glossary) — NFS-specific terms (XID, verifier, COMPOUND, FSID, …)
- [SMB.md](SMB.md#glossary) — SMB-specific terms (dialect, credit, transform header, KDF, …)
- [ACLS.md](ACLS.md) — the cross-protocol ACL model in depth
- [ENCRYPTION.md](ENCRYPTION.md) — at-rest encryption (master key / block key)
