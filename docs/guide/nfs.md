# NFS Guide

> Implementing or debugging the protocol? See [NFS protocol internals](../internals/nfs-protocol.md).

This guide covers everything an operator or end user needs to mount and use DittoFS over NFS: supported versions, mount commands, portmapper configuration, Kerberos, NFS-over-TLS, and troubleshooting pointers.

## Table of Contents

- [Protocol Overview](#protocol-overview)
- [Supported Versions](#supported-versions)
  - [Which version should I use?](#which-version-should-i-use)
  - [How a version is negotiated](#how-a-version-is-negotiated)
- [Embedded Portmapper](#embedded-portmapper)
- [NFSv3 File Locking (NLM/NSM)](#nfsv3-file-locking-nlmnsm)
- [Mounting](#mounting)
  - [The easy way: `dfsctl share mount`](#the-easy-way-dfsctl-share-mount)
  - [By NFS version (raw `mount`)](#by-nfs-version-raw-mount)
  - [With Portmapper on Port 111](#with-portmapper-on-port-111)
  - [With Explicit Ports](#with-explicit-ports)
- [Identity Squashing (root_squash and friends)](#identity-squashing-root_squash-and-friends)
- [Permissions: share grants over NFS](#permissions-share-grants-over-nfs)
- [Kerberos Exports (sec=krb5)](#kerberos-exports-seckrb5)
- [NFS-over-TLS (RFC 9289)](#nfs-over-tls-rfc-9289)
- [Testing Your Mount](#testing-your-mount)
- [Troubleshooting](#troubleshooting)
- [Glossary](#glossary)
- [References](#references)

---

## Protocol Overview

**NFS (Network File System)** is a distributed file system protocol that lets a client access files over a network as if they were on local storage. NFS uses **ONC RPC** for message framing and **XDR** for binary encoding. Each operation carries authentication credentials (UID/GID or Kerberos ticket) and a file handle — an opaque identifier for the file or directory being operated on.

The request/response flow:

```
Client                                Server
  |  1. TCP Connection (port 12049)     |
  | ----------------------------------> |
  |  2. MOUNT /export                   |
  | ----------------------------------> |
  |  <---- Root file handle ----------- |
  |  3. LOOKUP "file.txt"               |
  | ----------------------------------> |
  |  <---- File handle + attributes --- |
  |  4. READ (offset=0, count=4096)     |
  | ----------------------------------> |
  |  <---- Data + EOF flag ------------ |
  |  5. UMOUNT /export                  |
  | ----------------------------------> |
```

For the wire format, procedure tables, XDR encoding, and error-code mapping, see [NFS protocol internals](../internals/nfs-protocol.md).

---

## Supported Versions

DittoFS implements **NFSv3**, **NFSv4.0**, **NFSv4.1**, and **NFSv4.2**.

| Version | Key Features |
|---------|--------------|
| NFSv3 | Stateless, 64-bit file sizes, TCP, async writes, WCC |
| NFSv4.0 | Stateful, ACLs, compound operations, RPCSEC_GSS (Kerberos) |
| NFSv4.1 | Sessions, backchannel, directory delegations with CB_NOTIFY |
| NFSv4.2 | Sparse files (ALLOCATE, DEALLOCATE, SEEK, READ_PLUS), server-side CLONE/reflink (RFC 7862) + extended attributes (RFC 8276) |

All versions listen on port **12049** by default (not the standard 2049). The embedded portmapper listens on **10111** by default.

CLONE (reflink), ALLOCATE, DEALLOCATE, SEEK, and READ_PLUS are implemented for NFSv4.2; inter-server COPY (OP_COPY) is not.

### Which version should I use?

The version is chosen entirely by the **client** at mount time (via the
`vers=` / `nfsvers=` option). The server speaks all four; nothing on the server
side restricts which one a client may pick. Pick based on what you need:

| You want… | Use | Why |
|-----------|-----|-----|
| The simplest setup, no extra config | **NFSv4.1** | One TCP port, in-protocol locking, no MOUNT/NLM/NSM/portmapper to wire up. The recommended default for new mounts. |
| ACLs, Kerberos (`sec=krb5`), or NFS-over-TLS | **NFSv4.0+** | These features are NFSv4-only. NFSv3 has none of them. |
| Sparse files (ALLOCATE/DEALLOCATE/SEEK/READ_PLUS) or reflink/CLONE | **NFSv4.2** | Those operations were added in 4.2. (Inter-server `OP_COPY` is *not* implemented.) |
| Maximum client compatibility / legacy clients | **NFSv3** | Works everywhere, but byte-range locking needs the NLM/NSM side-channel (UDP + portmapper on 111 — see [NFSv3 File Locking](#nfsv3-file-locking-nlmnsm)). |

> **Rule of thumb:** reach for **NFSv4.1** unless a specific client or workload
> forces NFSv3. v4.1 avoids every portmapper/NLM headache documented below, and
> locking just works.

**Client compatibility notes**

- **Linux** supports all versions. The kernel default is usually `vers=4.2`
  with automatic fall-back, but DittoFS runs on a non-standard port, so you
  always pass the version (and port) explicitly anyway.
- **macOS** has a mature NFSv3 client and a more limited NFSv4 client. NFSv3 is
  the best-trodden path on macOS; some macOS releases negotiate only up to
  `vers=4.0`. macOS has **no** NFS-over-TLS client — use Kerberos or a network
  tunnel for confidentiality.

### How a version is negotiated

There is no server-side "default version" — the client states the version it
wants:

- **Linux:** `-o vers=4.1` (or `nfsvers=4.1`). Omitting it lets the kernel
  negotiate the highest version it and the server share, but with DittoFS on a
  non-standard port you specify it explicitly.
- **macOS:** `-o vers=3` or `-o vers=4`. macOS accepts a major version; it does
  not take a `4.2` minor on older releases.
- **`dfsctl share mount --nfs-version`** wraps both: pass `3`, `4`, `4.0`,
  `4.1`, or `4.2` and it builds the right `mount` command for your platform
  (see [The easy way](#the-easy-way-dfsctl-share-mount)).

---

## Embedded Portmapper

DittoFS includes an embedded portmapper that enables standard NFS service discovery without requiring a system-level `rpcbind` daemon. It answers both legacy **PMAP v2** (RFC 1057, used by `rpcinfo -p` and Linux) and **RPCBIND v3/v4** (RFC 1833 universal addresses, used by macOS/BSD lock clients).

### Why an Embedded Portmapper?

NFS clients traditionally rely on a portmapper (port 111) to discover which port an NFS server is listening on. Without a portmapper, clients require explicit port options (`-o port=12049,mountport=12049`), and standard tools like `rpcinfo` and `showmount` do not work.

The embedded portmapper solves this by:

- Registering all DittoFS services (NFS, MOUNT, NLM, NSM) automatically on startup
- Responding to standard portmap queries via TCP and UDP
- Running on an unprivileged port (default 10111) to avoid requiring root
- Enabling `rpcinfo` and `showmount` to discover DittoFS services

### Service Discovery

With the portmapper running, standard NFS tools work:

```bash
# Query registered services
rpcinfo -p localhost -n 10111

# Show available exports
showmount -e localhost
```

### Configuration

The portmapper is disabled by default. Enable it via `dfsctl`:

```bash
# Check current settings
dfsctl adapter settings nfs

# Change the portmapper port
dfsctl adapter settings nfs update --portmapper-port 10111

# Disable the portmapper entirely
dfsctl adapter settings nfs update --portmapper-enabled=false
```

Or via environment variables:

```bash
DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT=10111
DITTOFS_ADAPTERS_NFS_PORTMAPPER_ENABLED=false
```

#### Registering with the system rpcbind (port 111)

A kernel NFSv3 client discovers the NLM (lock manager) port by querying
`rpcbind` on **port 111** — a location fixed by the RPC standard with no
client-side override. On a host that already runs a system `rpcbind`, DittoFS
cannot bind 111 with its embedded portmapper, so a client mounted **without**
`nolock` finds no NLM registration and lock calls hang.

Set `register_with_system` to make DittoFS register its services (NFS, MOUNT,
NLM, NSM) with the host's existing `rpcbind` at startup — the same mechanism
`rpc.nfsd` and `rpc.statd` use — so NFSv3 byte-range locking works without
`nolock`:

```bash
DITTOFS_ADAPTERS_NFS_PORTMAPPER_REGISTER_WITH_SYSTEM=true
# NLM/NSM use UDP for status notifications — enable the UDP transport too:
DITTOFS_ADAPTERS_NFS_UDP_ENABLED=true
```

Best effort: if no `rpcbind` answers on 111 the registration is skipped with a
warning (NFS still serves; only `nolock`-free v3 locking is unavailable). The
mappings are unregistered cleanly on shutdown.

### Security

The embedded portmapper follows standard security practices:

- **SET/UNSET restricted to localhost**: Only local clients can register or unregister services
- **CALLIT (procedure 5) omitted**: Prevents DDoS amplification attacks
- **Connection limits**: TCP connections are capped at 64 concurrent
- **Non-privileged port**: Default port 10111 avoids requiring root privileges

### Portmapper Failure is Non-Fatal

If the portmapper fails to start (e.g., port already in use), NFS continues to operate normally. Clients just need to specify ports explicitly in mount options.

---

## NFSv3 File Locking (NLM/NSM)

NFSv3 has no in-protocol locking; byte-range locks travel over the separate
**NLM** (Network Lock Manager) protocol, with crash recovery coordinated by
**NSM** (Network Status Monitor). NFSv4 does not use NLM — its locking is
in-protocol, so none of this section applies to `vers=4` mounts.

> NFSv3 **mount and read/write need none of this.** Only byte-range locking
> (`flock`/`fcntl`/`lockf`) uses NLM. If you don't need cross-client locks,
> mount `-o nolock`, or use `-o vers=4` for in-protocol locking.

### What DittoFS serves

- **NLM** (program 100021) versions **1 and 3** (32-bit offsets) and **4**
  (64-bit offsets), including the **asynchronous `*_MSG`/`*_RES` procedures**
  (TEST/LOCK/CANCEL/UNLOCK_MSG → `*_RES` callbacks) that macOS/BSD `lockd` use,
  alongside the synchronous procedures Linux uses.
- **NSM** (program 100024) version 1, for crash-recovery monitoring (SM_MON /
  SM_NOTIFY). The SM_NOTIFY callback target is the request's transport source,
  not the client-supplied `my_name`.
- **Portmapper** speaking both legacy **PMAP v2** (RFC 1057) and **RPCBIND
  v3/v4** (RFC 1833, universal addresses) — macOS/BSD `lockd` discover NLM via
  RPCBIND v3/v4 and do not fall back to v2.
- All of the above over **TCP and UDP**. NFS data (program 100003) is
  **TCP-only** — it is never served over UDP (READ/WRITE payloads exceed a UDP
  datagram).

> **Status:** the full protocol chain (RPCBIND v3/v4 discovery → NSM monitoring
> → async NLM locking with reserved-port `*_RES` callbacks) is implemented and
> unit-tested. End-to-end acceptance against a live macOS client is validated on
> a same-LAN topology (a same-host loopback test is unreliable: the client and
> server contend for port 111 and a shared `lockd`/`statd`).

### Why macOS NFSv3 locking needs extra setup

A macOS/BSD lock client (`rpc.lockd` / `rpc.statd`) reaches NLM/NSM over **UDP**
and discovers them by querying the **server's portmapper on port 111** — there is
no mount option to redirect that lookup. So two server-side pieces, both
**disabled by default**, are required:

1. **UDP transport** — serve NLM/NSM/MOUNT over UDP:
   ```bash
   dfsctl adapter settings nfs update --udp-enabled true
   ```
2. **Portmapper on port 111** — so the client's discovery query resolves:
   ```bash
   dfsctl adapter settings nfs update --portmapper-enabled true --portmapper-port 111
   ```
   Binding 111 needs root or `CAP_NET_BIND_SERVICE`, and may clash with a host
   `rpcbind`. On Kubernetes the operator exposes port 111 via the adapter Service
   (mapped to the unprivileged container port), so no privileged binding is needed
   in the pod.

Restart the adapter after changing these settings. Linux clients (NLM v4) work
once the portmapper is reachable; they do not strictly require UDP, but enabling
it is harmless.

Equivalent config-file / env settings:

```yaml
adapters:
  nfs:
    udp:
      enabled: true
    portmapper:
      enabled: true
      port: 111
```

```bash
DITTOFS_ADAPTERS_NFS_UDP_ENABLED=true
DITTOFS_ADAPTERS_NFS_PORTMAPPER_ENABLED=true
DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT=111
```

### Recommended: just use NFSv4

For locking without any of the above, mount with `vers=4` — locking is part of
the protocol, and there is no NLM, NSM, MOUNT, or portmapper to configure:

```bash
dfsctl share mount /my-share /mnt/point --protocol nfs --nfs-version 4.1
# or directly:
mount -t nfs -o vers=4.1,port=12049 server:/my-share /mnt/point
```

---

## Mounting

DittoFS listens on **port 12049**, not the standard 2049. Unless you run the
[embedded portmapper on 111](#with-portmapper-on-port-111), every mount command
must name the port (and, for NFSv3, the `mountport`). The sections below show
the convenience wrapper first, then the raw `mount` command for each version.

> Mounting over **SMB** instead? See [Mounting SMB Shares](smb.md#mounting-smb-shares)
> — the same `dfsctl share mount` wrapper handles it with `--protocol smb`.
>
> Want mounts with **no `port=` option**? Run DittoFS on the standard port 2049
> — see [Running on standard ports (production)](install.md#running-on-standard-ports-production).

### The easy way: `dfsctl share mount`

`dfsctl share mount` resolves the server's NFS port, picks the right options for
your platform, and runs `mount` for you. The `--nfs-version` flag selects the
protocol version (default **3**):

```bash
# NFSv4.1 (recommended) — locking just works, no portmapper needed
sudo dfsctl share mount /my-share /mnt/point --protocol nfs --nfs-version 4.1

# NFSv3 (the default if --nfs-version is omitted)
sudo dfsctl share mount /my-share /mnt/point --protocol nfs

# NFSv4.2 — for sparse files / reflink
sudo dfsctl share mount /my-share /mnt/point --protocol nfs --nfs-version 4.2
```

Accepted `--nfs-version` values are `3`, `4`, `4.0`, `4.1`, and `4.2`. The
mount point must exist and be empty. On macOS, mounting under your home
directory does not require `sudo`.

Under the hood it builds these option strings (so you can reproduce them with a
plain `mount` — see the next section):

| Version | Generated `-o` options (Linux) | macOS additions |
|---------|--------------------------------|-----------------|
| `4.x` | `nfsvers=<x>,tcp,port=12049,actimeo=0` | `,resvport` |
| `3` | `nfsvers=3,tcp,port=12049,mountport=12049,actimeo=0,nolock` | `,resvport` (and **no** `nolock`) |

What the options mean:

- **`nfsvers=` / `vers=`** — the protocol version the client requests.
- **`port=`** — the NFS (program 100003) port; always 12049 by default.
- **`mountport=`** *(NFSv3 only)* — NFSv3 uses a separate MOUNT protocol;
  DittoFS serves it on the same port, so set `mountport=port`. NFSv4 has no
  MOUNT protocol and omits this.
- **`nolock`** *(NFSv3 on Linux)* — skips the NLM lock side-channel so the mount
  doesn't need UDP + a portmapper on 111. Drop it (and configure NLM/NSM) only
  if you need cross-client byte-range locking — see
  [NFSv3 File Locking](#nfsv3-file-locking-nlmnsm).
- **`actimeo=0`** — disables attribute caching for immediate cross-client
  visibility. Raise it (e.g. `actimeo=3`) for better performance once you don't
  need instant consistency.
- **`resvport`** *(macOS)* — source from a reserved (<1024) port, which some
  NFS setups require.

### By NFS version (raw `mount`)

If you prefer to run `mount` yourself, these are the per-version equivalents.

**NFSv4.1 / 4.2 / 4.0** — one port, in-protocol locking, nothing else to wire up:

```bash
# Linux
sudo mount -t nfs -o vers=4.1,tcp,port=12049 server:/my-share /mnt/point

# macOS (vers=4; older releases don't accept a 4.x minor)
mount -t nfs -o vers=4,tcp,port=12049,resvport server:/my-share /mnt/point

# macOS — equivalent via the native mount_nfs(8) helper
mount_nfs -o vers=4,tcp,port=12049,resvport server:/my-share /mnt/point

# NFSv4.2 (Linux) — for sparse files / reflink
sudo mount -t nfs -o vers=4.2,tcp,port=12049 server:/my-share /mnt/point
```

**NFSv3** — needs `mountport` (separate MOUNT protocol); add `nolock` on Linux
unless you've set up NLM/NSM:

```bash
# Linux
sudo mount -t nfs -o vers=3,tcp,port=12049,mountport=12049,nolock server:/my-share /mnt/point

# macOS
mount -t nfs -o vers=3,tcp,port=12049,mountport=12049,resvport server:/my-share /mnt/point

# macOS — equivalent via mount_nfs(8)
mount_nfs -o vers=3,tcp,port=12049,mountport=12049,resvport server:/my-share /mnt/point
```

> On macOS, `mount -t nfs -o … server:/share /mnt` and
> `mount_nfs -o … server:/share /mnt` are interchangeable — `mount` simply
> dispatches to the `mount_nfs(8)` helper. Use whichever you prefer.

> NFSv3 data is **TCP-only** in DittoFS; only the NLM/NSM lock side-channel ever
> uses UDP. Don't add `udp` to a v3 data mount.

### With Portmapper on Port 111

When the portmapper runs on the standard port 111 (requires root or `CAP_NET_BIND_SERVICE`), NFS clients can auto-discover ports and mount commands are simplified:

```bash
# Configure portmapper on standard port (requires root)
dfsctl adapter settings nfs update --portmapper-port 111

# Linux - no port options needed, client queries portmapper automatically
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp localhost:/export /mnt/nfs

# macOS
mkdir -p /tmp/nfs
mount -t nfs -o tcp localhost:/export /tmp/nfs
```

### With Explicit Ports

When the portmapper is disabled or running on a non-standard port, specify the NFS port explicitly:

```bash
# Linux
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS (sudo not required)
mkdir -p /tmp/nfs
mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /tmp/nfs

# macOS may require resvport on some configurations
mount -t nfs -o tcp,port=12049,mountport=12049,resvport localhost:/export /tmp/nfs

# Unmount
sudo umount /mnt/nfs   # Linux
umount /tmp/nfs        # macOS
```

---

## Identity Squashing (root_squash and friends)

**Squashing** remaps the UID/GID a client *claims* to a different identity
before DittoFS checks permissions. Its classic use is to stop a remote `root`
from acting as `root` on your files — a remote machine's superuser should not
automatically be the superuser of your export.

This is a **per-share, NFS-only** policy. It does not apply to SMB (which has its
own [guest mapping](smb.md)). Crucially, squashing happens at the *identity*
layer — **after** the client authenticates but **before** the
[export gate and POSIX/ACL checks](access-control.md). It rewrites *who you are*;
it never grants access on its own.

### How NFS sends an identity

- **AUTH_SYS (AUTH_UNIX)** — the default for non-Kerberos mounts. The client
  simply *asserts* a UID/GID in every request. There is no verification: a
  client that says it is UID 0 **is** UID 0 to the server. This is exactly why
  squashing exists.
- **AUTH_NULL** — no credentials at all. DittoFS **always** maps AUTH_NULL to
  the anonymous identity, regardless of the squash mode.
- **Kerberos (RPCSEC_GSS)** — the principal is cryptographically verified and
  resolved to a UID/GID via the idmap. Squashing still applies on top of the
  resolved identity (e.g. `root_to_guest` squashes a resolved UID 0).

### The five modes

DittoFS models squashing as five modes (matching the familiar Synology NAS
options) rather than separate `root_squash` / `all_squash` toggles:

| Mode | Effect | Traditional NFS equivalent |
|------|--------|----------------------------|
| `none` | No remapping. UIDs pass through unchanged. | `no_root_squash` |
| `root_to_admin` | Root (UID 0) keeps root. Other UIDs unchanged. | `no_root_squash` |
| `root_to_guest` **(default)** | Root (UID 0) → anonymous. Other UIDs unchanged. | `root_squash` |
| `all_to_admin` | **Every** client UID → root (UID 0). | `all_squash` to root |
| `all_to_guest` | **Every** client UID → anonymous. | `all_squash` |

> **Default — root is squashed.** DittoFS defaults to `root_to_guest`, matching
> the conventional NFS `root_squash`: a remote root is mapped to the anonymous
> identity and does **not** keep root privileges on the export. If a trusted
> client's root must act as the server's root (e.g. single-tenant admin
> automation), opt into `root_to_admin` (`no_root_squash`) per share. `none` and
> `root_to_admin` behave identically for UID remapping.

The **anonymous** identity is UID/GID **65534** (`nobody`/`nogroup`) by default.

### Configuring it

```bash
# Squash remote root to the anonymous user (the usual hardening choice)
dfsctl share nfs-config set /export --squash root_to_guest

# Force every client to the anonymous user (e.g. a public read-only export)
dfsctl share nfs-config set /export --squash all_to_guest

# Inspect the current squash mode (and other NFS export options)
dfsctl share nfs-config show /export
```

Valid values: `none`, `root_to_admin`, `root_to_guest`, `all_to_admin`,
`all_to_guest`. A squash change applies to active clients immediately — no NFS
adapter restart is required. The anonymous UID/GID is
configurable via the REST API (`anonymous_uid` / `anonymous_gid` on the share's
NFS config); it is not exposed as a `dfsctl` flag and defaults to 65534.

### Worked examples

Assume a file owned by UID 1000, mode `0644`, and a share-gate
`default_permission` of `read-write`:

| Client mounts and acts as… | Squash mode | Effective identity | Result |
|----------------------------|-------------|--------------------|--------|
| `root` (UID 0) | `root_to_admin` | UID 0 (root) | Full access — root bypasses POSIX. |
| `root` (UID 0) | `root_to_guest` (default) | UID 65534 (nobody) | Treated as `EVERYONE@`; can read the `0644` file, **cannot** write it. |
| UID 1000 | `root_to_guest` | UID 1000 (unchanged) | Owner access — read/write its own file. |
| UID 1000 | `all_to_guest` | UID 65534 (nobody) | Squashed to nobody; read-only on the `0644` file even though it "owns" it. |
| any UID | `all_to_admin` | UID 0 (root) | Everyone gets root — only for fully-trusted, single-tenant exports. |

> **Squash is not access control.** Even `all_to_admin` (everyone → root) is
> still gated by the share's `default_permission` and the file's mode/ACL after
> remapping. Mapping a caller to root grants root's POSIX power, but the
> [export gate](access-control.md) must still admit them. Use squashing to
> *constrain* identity, and the [export gate + POSIX/ACL](access-control.md) to
> *grant* access.

---

## Permissions: share grants over NFS

Access is decided in two layers, and which layer a client can *see* depends on
the NFS version. Understanding this avoids the most common surprise:
**"I granted a user read-write but they get Permission denied over NFSv3."**

### The two layers

1. **Export gate** — the share's `default_permission` (access for principals
   without an explicit grant) plus per-user / per-group grants. Enforced
   server-side on every request.
2. **Filesystem layer** — the file/directory's POSIX mode bits and, where the
   protocol carries it, its ACL.

The share root directory's mode bits track `default_permission` so that
mode-only clients honour the share's access level:

| `default_permission` | Share root mode | A non-root client can… |
|----------------------|-----------------|------------------------|
| `none` / unset       | `0755`          | traverse/read only if the export gate admits it; never write |
| `read`               | `0755`          | read; not write |
| `read-write` / `admin` | `0777`        | read and write |

### NFSv3 vs NFSv4: where per-user grants apply

- **NFSv3 carries only mode bits — no ACL.** The Linux client enforces those
  bits *client-side*, before sending an RPC. So a **per-user grant is invisible
  over NFSv3**: mode bits cannot express "uid 2000 may write, uid 4000 may not."
  Over NFSv3 a non-root user can write the share root only when
  `default_permission` is `read-write` (root mode `0777`). This is normal Unix
  behaviour, not a DittoFS limitation — and it is also POSIX-ACL consistent
  (a named-user ACL entry simply has no NFSv3 transport).
- **NFSv4 (and SMB) carry the ACL.** Per-user and per-group grants *are*
  honoured: grant a user read-write and they can write over NFSv4 even on a
  share whose default is read-only.

**Rule of thumb:** for per-user least-privilege access, use **NFSv4**. Reserve
NFSv3 for share-wide access levels set via `default_permission`.

### Other behaviours worth knowing

- **Denials are `EACCES` ("Permission denied"), never `EIO`.** A permission
  failure — including a squashed-root or ungranted-user write — surfaces as
  `Permission denied`, not the misleading `Input/output error` older builds
  returned on NFSv3.
- **A fully-locked (`none`) share is not mountable over NFSv4 by a root client.**
  The mount runs as root, which `root_to_guest` squashes to the guest identity;
  with `default_permission=none` the guest cannot traverse the export to
  complete the NFSv4 mount. Such a share is reachable only over NFSv3 (whose
  separate mount protocol does not gate on the export root). For the common
  "world-readable, granted-writable" pattern, use `default_permission=read` and
  grant write to the specific users — they then write over NFSv4 while everyone
  else is read-only.

---

## Kerberos Exports (sec=krb5)

NFSv4.0 and NFSv4.1 can authenticate with Kerberos via RPCSEC_GSS. The server
verifies the client's Kerberos ticket against a keytab and maps the
authenticated principal to a DittoFS identity for permission checks.

**Mounting (Linux):**

```bash
# DittoFS listens on 12049, not the default 2049 — pass it explicitly.
sudo mount -t nfs4 -o vers=4.1,sec=krb5,port=12049 server:/export /mnt/dittofs
```

The non-default port also applies to the auth handshake; without `port=12049`
the client connects to 2049 and the mount fails with `Connection refused`
before Kerberos is ever attempted. (Run the embedded portmapper on 111 to drop
the explicit port — see [Embedded Portmapper](#embedded-portmapper).)

**Per-export policy.** Three NFS export options gate which auth flavors a share
accepts (set via `dfsctl adapter edit nfs` / the share's `NFSExportOptions`):

| Option | Default | Effect |
|--------|---------|--------|
| `allow_auth_sys` | `true` | When `false`, AUTH_SYS (AUTH_UNIX) mounts/operations are refused. (Set `require_kerberos` to also refuse AUTH_NULL and mandate RPCSEC_GSS — `allow_auth_sys=false` alone only gates AUTH_SYS.) |
| `require_kerberos` | `false` | When `true`, every mount/operation must use RPCSEC_GSS; AUTH_SYS and AUTH_NULL are refused. |
| `min_kerberos_level` | `krb5` | Minimum GSS protection level a Kerberos session must negotiate (`krb5` = authentication, `krb5i` = + integrity, `krb5p` = + privacy). A session below the floor is refused. |

`allow_auth_sys`, `require_kerberos`, and `min_kerberos_level` are enforced
identically on **NFSv3** (at MOUNT) and on **NFSv4.0/v4.1** (at the first
operation that resolves the export handle — v4 has no MOUNT call). A refusal
surfaces as `NFS4ERR_WRONGSEC` (v4) / `MNT3ERR_ACCES` (v3), prompting the client
to retry with the correct flavor.

`min_kerberos_level` only constrains RPCSEC_GSS sessions: it rejects a Kerberos
session whose negotiated service level is below the floor (e.g. a plain `krb5`
authentication-only session on a `krb5p` privacy share). Non-GSS flavors are
governed by `allow_auth_sys` / `require_kerberos`; pair `min_kerberos_level`
with `require_kerberos=true` to mandate a protection floor for *all* access.

**Principal → identity mapping (the "access denied after EXCHANGE_ID" case).**
A successful `sec=krb5` mount has two stages, and they fail differently:

1. **GSS context establishment** (the client's `EXCHANGE_ID` / context init).
   This succeeds as soon as the ticket verifies against the server keytab.
2. **Authorization** of the mapped principal on the export. The authenticated
   principal is resolved to a DittoFS user (and UID/GID) through the identity
   store / idmap. A principal with **no mapping** resolves to **nobody**
   (UID 65534), and nobody is then subject to the export's
   `default_permission`: if that is `none`, the export denies the operation and
   the mount fails with `access denied` **even though the Kerberos handshake
   succeeded**.

This is by design — a host that mounts with its **machine** credential (no user
`kinit`, e.g. `nfs/host.realm@REALM` from `/etc/krb5.keytab`) authenticates as
the machine principal, which has no user identity. To grant such a mount:

- add an idmap entry (or a DittoFS user) for the principal so it resolves to a
  real UID/GID, **or**
- set the export's `default_permission` to `read` / `read-write` so unmapped
  (nobody) principals are admitted with that ceiling, **and** ensure the export
  root's POSIX mode permits the resulting identity to traverse it.

User principals (`alice@REALM`, obtained via `kinit`) that are present in the
idmap mount and access files as that user with no extra configuration.

---

## NFS-over-TLS (RFC 9289)

DittoFS can encrypt NFS wire traffic with TLS 1.3, using the opportunistic `AUTH_TLS` STARTTLS mechanism from RFC 9289. A client opens TCP and sends a `NULL` RPC with `auth_flavor = AUTH_TLS (7)`; the server replies with the 8-octet `"STARTTLS"` verifier, then both perform a TLS 1.3 handshake on the **same** connection. All subsequent RPC traffic is encrypted. Because Go performs the handshake and crypto in userspace, no kernel TLS (`kTLS`) or `tlshd` daemon is needed on the server.

DittoFS only loads cert files — issuance/renewal/rotation is the platform's job; rotated files are hot-reloaded with no restart (shared with the control-plane TLS path via `internal/tlsconfig`).

```yaml
adapters:
  nfs:
    tls:
      cert_file: /etc/dittofs/tls/tls.crt
      key_file:  /etc/dittofs/tls/tls.key
      client_ca: /etc/dittofs/tls/ca.crt   # optional → mutual TLS (client-cert auth)
      min_version: "1.3"                    # RFC 9289 floor is TLS 1.3
      mode: opportunistic                   # "opportunistic" (default) | "require"
```

- **`opportunistic`** (default): clients that send the `AUTH_TLS` probe are upgraded; clients that do not are still served in plaintext. Lets a TLS rollout proceed without breaking existing mounts.
- **`require`**: a connection must upgrade via `AUTH_TLS` before any other RPC; plaintext requests are rejected (connection dropped).

**Client interop:**

- **Linux:** `mount -o vers=4.1,xprtsec=tls …` — requires `tlshd` (ktls-utils) and `CONFIG_NET_HANDSHAKE` (RHEL 9.x / upstream kernel 6.7+).
- **macOS:** no NFS-over-TLS client — use Kerberos or a network-level tunnel instead.

---

## Testing Your Mount

```bash
# Start server
./dfs start -log-level DEBUG

# Mount (Linux)
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/test
cd /mnt/test

# Exercise common operations
ls -la              # READDIR / READDIRPLUS
cat readme.txt      # READ
echo "test" > new   # CREATE + WRITE
mkdir foo           # MKDIR
rm new              # REMOVE
rmdir foo           # RMDIR
mv file1 file2      # RENAME
ln -s target link   # SYMLINK
ln file1 file2      # LINK (hard link)

# Run unit tests
go test ./...

# Run E2E tests (requires NFS client installed)
go test -v -timeout 30m ./test/e2e/...

# Run specific E2E suite
go test -v ./test/e2e -run TestE2E/memory/BasicOperations
```

---

## Troubleshooting

See [Troubleshooting](./troubleshooting.md) for common issues. Quick reference:

- **`Connection refused` on mount**: DittoFS listens on **12049**, not 2049. Always pass `-o port=12049,mountport=12049` unless the portmapper is running on 111.
- **`access denied` after Kerberos succeeds**: The principal has no idmap entry; it resolved to nobody and the export's `default_permission` is `none`. See [Kerberos Exports](#kerberos-exports-seckrb5) above.
- **`NFS4ERR_WRONGSEC`**: The client used AUTH_SYS on a Kerberos-only export (or vice versa). Check `allow_auth_sys` / `require_kerberos` on the share.
- **NFSv3 locks not working on macOS**: Requires UDP transport + portmapper on 111. See [NFSv3 File Locking](#nfsv3-file-locking-nlmnsm).
- **Configuration reference**: See [Configuration](./configuration.md) for the full `adapters.nfs` config block.
- **Security hardening**: See [Security](./security.md) for TLS, Kerberos, and export policy guidance.

---

## Glossary

| Term | Definition |
|------|------------|
| **AUTH_NULL** | No authentication flavor (flavor 0) |
| **AUTH_UNIX** | Unix-style authentication with UID/GID (flavor 1) |
| **Backchannel** | Server-to-client connection used for callbacks (NFSv4.1) |
| **CB_NOTIFY** | Callback operation for directory change notifications |
| **COMPOUND** | NFSv4 request containing multiple operations |
| **Cookie** | Opaque value used for directory iteration (READDIR) |
| **Delegation** | Server grants client exclusive or shared caching rights |
| **EOF** | End of file indicator in READ responses |
| **Export** | A directory shared via NFS (like an SMB share) |
| **File Handle** | Opaque identifier for a file/directory (max 64 bytes) |
| **ftype3** | File type enum (regular, directory, symlink, etc.) |
| **FSID** | File system identifier |
| **nfstime3** | NFS time format (seconds + nanoseconds) |
| **NLM** | Network Lock Manager -- sideband protocol NFSv3 uses for file locking |
| **NSM / statd** | Network Status Monitor -- tracks reboots so NLM locks can be reclaimed/released after a crash |
| **RPCSEC_GSS** | Kerberos-based RPC security flavor (NFSv4) |
| **RPC** | Remote Procedure Call -- foundation protocol |
| **sattr3** | Set attributes structure (for SETATTR, CREATE) |
| **Session** | NFSv4.1 construct tracking client connection state |
| **Stale Handle** | A handle that is no longer valid |
| **Stateid** | NFSv4 identifier naming a specific open/lock state on the server |
| **Verifier** | Server-unique value that changes on restart |
| **WCC** | Weak Cache Consistency -- pre/post attributes that let a client cheaply validate its cache |
| **XDR** | External Data Representation (encoding format) |
| **XID** | Transaction ID for matching requests/replies |

For protocol-independent terms (CAS, BLAKE3, SID, ACL, ...) see the project-wide [Glossary](./glossary.md).

---

## References

### Specifications

- [RFC 1057](https://www.rfc-editor.org/rfc/rfc1057) - RPC: Remote Procedure Call Protocol (Portmapper)
- [RFC 1094](https://www.rfc-editor.org/rfc/rfc1094) - NFS: Network File System Protocol (Version 2)
- [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813) - NFS Version 3 Protocol Specification
- [RFC 4506](https://www.rfc-editor.org/rfc/rfc4506) - XDR: External Data Representation Standard
- [RFC 5531](https://www.rfc-editor.org/rfc/rfc5531) - ONC RPC: Remote Procedure Call Protocol Specification Version 2
- [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530) - NFS Version 4.0 Protocol
- [RFC 7862](https://www.rfc-editor.org/rfc/rfc7862) - NFS Version 4.2 Protocol (sparse files)
- [RFC 8276](https://www.rfc-editor.org/rfc/rfc8276) - File System Extended Attributes in NFSv4
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881) - NFS Version 4.1 Protocol
- [RFC 9289](https://www.rfc-editor.org/rfc/rfc9289) - Towards Remote Procedure Call Encryption by Default (NFS-over-TLS)
- [Open Group XNFS](https://pubs.opengroup.org/onlinepubs/9629799/) - Network Lock Manager (NLM, chap. 10) and Network Status Monitor (NSM, chap. 11)
- [RFC 2203](https://www.rfc-editor.org/rfc/rfc2203) - RPCSEC_GSS Protocol · [RFC 4120](https://www.rfc-editor.org/rfc/rfc4120) - Kerberos V5 · [RFC 2743](https://www.rfc-editor.org/rfc/rfc2743) - GSS-API

### Related Projects

- [go-nfs](https://github.com/willscott/go-nfs) - Another NFS implementation in Go
- [FUSE](https://github.com/libfuse/libfuse) - Filesystem in Userspace

### DittoFS Documentation

- [Architecture](../internals/architecture.md) - Deep dive into design patterns and implementation
- [Configuration](./configuration.md) - Complete configuration guide
- [Glossary](./glossary.md) - Plain-language definitions of protocol, ACL, and storage terms
- [Troubleshooting](./troubleshooting.md) - Common issues and solutions
- [Security](./security.md) - Security hardening guide
- [NFS Protocol Internals](../internals/nfs-protocol.md) - Wire format, procedure tables, error mapping
