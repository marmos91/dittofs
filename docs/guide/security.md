# Security Guide

**Current status:** DittoFS is experimental and has not undergone formal security auditing.
Exercise caution before deploying in production environments without thorough testing and review.

For the internal mechanism design and threat model, see
[`../internals/security-model.md`](../internals/security-model.md).

## Table of Contents

- [What is implemented](#what-is-implemented)
- [Remaining limitations](#remaining-limitations)
- [Authentication](#authentication)
  - [NFS: RPCSEC\_GSS (Kerberos)](#nfs-rpcsec_gss-kerberos)
  - [NFS: AUTH\_UNIX](#nfs-auth_unix)
  - [NFS: AUTH\_NULL](#nfs-auth_null)
  - [SMB: Kerberos via SPNEGO](#smb-kerberos-via-spnego)
  - [SMB: NTLM and guest fallback](#smb-ntlm-and-guest-fallback)
  - [Active Directory and LDAP](#active-directory-and-ldap)
- [Message integrity and encryption](#message-integrity-and-encryption)
  - [SMB signing](#smb-signing)
  - [SMB encryption](#smb-encryption)
- [Access control](#access-control)
  - [NFSv4 ACLs](#nfsv4-acls)
  - [POSIX file permissions](#posix-file-permissions)
  - [Export-level access control](#export-level-access-control)
  - [IP-based restrictions](#ip-based-restrictions)
  - [Identity mapping](#identity-mapping)
  - [Read-only shares](#read-only-shares)
- [Network security](#network-security)
  - [NFS-over-TLS (RFC 9289)](#nfs-over-tls-rfc-9289)
  - [Control plane API TLS](#control-plane-api-tls)
  - [Network-level alternatives](#network-level-alternatives)
  - [Firewall configuration](#firewall-configuration)
- [Kerberos configuration](#kerberos-configuration)
  - [Server configuration](#server-configuration)
  - [Keytab management](#keytab-management)
  - [Environment variable overrides](#environment-variable-overrides)
  - [NFS client setup](#nfs-client-setup)
  - [SMB client setup](#smb-client-setup)
- [Production hardening checklist](#production-hardening-checklist)
- [Secure configuration example](#secure-configuration-example)
- [Planned security features](#planned-security-features)
- [Reporting security issues](#reporting-security-issues)

---

## What is implemented

- Kerberos authentication for NFS via RPCSEC_GSS (RFC 2203)
- Kerberos authentication for SMB via SPNEGO
- Active Directory / LDAP idmap (`idmap_ad` RFC2307 attributes or `idmap_rid` RID derivation), with Kerberos keytab and NTLM machine-account pass-through
- SMB3 encryption with AES-128-GCM, AES-128-CCM, AES-256-GCM, AES-256-CCM
- SMB3 signing with AES-128-CMAC and AES-128-GMAC; SMB 2.x signing with HMAC-SHA256
- SMB 3.1.1 preauth integrity (SHA-512 hash chain) for downgrade protection
- SP800-108 key derivation for per-session cryptographic keys
- VALIDATE_NEGOTIATE_INFO for SMB 3.0/3.0.2 downgrade detection
- NFSv4 ACL-based access control
- POSIX file permission enforcement (owner/group/other)
- Export-level IP-based access restrictions
- Identity mapping (root squash, all squash)
- AUTH_UNIX for trusted-network deployments
- Native TLS (and optional mutual TLS) for the control plane API with hot-reload
- NFS-over-TLS (RFC 9289): opportunistic `AUTH_TLS` STARTTLS upgrade to TLS 1.3

## Remaining limitations

- No formal security audit performed
- NFS-over-TLS requires a client that supports RFC 9289 (`xprtsec=tls`); macOS has no such client (use Kerberos or a network tunnel instead)
- No encryption at rest
- No audit logging for file operations

---

## Authentication

### NFS: RPCSEC_GSS (Kerberos)

DittoFS implements RPCSEC_GSS per RFC 2203, enabling Kerberos-based strong authentication for
NFSv4 clients. This is the recommended authentication method for any deployment outside of a
fully trusted network.

When enabled, clients authenticate using Kerberos tickets. The server validates tickets against
its keytab and maps Kerberos principals to Unix UID/GID for authorization decisions.

Key properties:

- Mutual authentication (server identity is also verified by the client)
- Cryptographic credential verification (no trust-based UID spoofing)
- Configurable context lifetime and clock skew tolerance
- Hot-reload support for keytab rotation without server restart

See [Kerberos configuration](#kerberos-configuration) for setup instructions.

### NFS: AUTH_UNIX

AUTH_UNIX is the traditional NFS authentication mechanism. The client provides UID, GID, and
supplementary GIDs with each request. The server trusts these values without independent
verification.

- Suitable for trusted networks only
- Clients can impersonate any user by sending arbitrary UID/GID values
- Use identity mapping (root squash, all squash) to limit exposure

### NFS: AUTH_NULL

AUTH_NULL provides anonymous access with no authentication. All requests are treated as coming
from an unauthenticated user. Use with extreme caution and only for public read-only shares.

### SMB: Kerberos via SPNEGO

The SMB adapter supports Kerberos authentication through SPNEGO during `SESSION_SETUP`. When a
Kerberos provider is configured, SMB clients authenticate using Kerberos tickets. When Kerberos
is not configured, the SMB adapter falls back to NTLM or guest authentication.

See [Kerberos configuration](#kerberos-configuration) for shared setup that applies to both NFS
and SMB.

### SMB: NTLM and guest fallback

**Guest sessions** have significant security limitations: no session key is available, so no
signing and no encryption are possible. Guest sessions should be restricted to read-only access
on public shares. DittoFS never encrypts guest sessions, even in `required` mode (the connection
is rejected instead).

**NTLM fallback** occurs when Kerberos keytab is not configured, the client has no valid TGT, or
DNS resolution prevents Kerberos service ticket acquisition.

NTLM authenticates two classes of user:

- **Local DittoFS users:** the NTLMv2 response is validated directly against the NT hash stored
  in the control-plane user store.
- **AD domain users:** when a machine account is configured (`kerberos.machine_account`), the
  challenge/response is forwarded to the Domain Controller over a sealed NETLOGON secure channel
  (MS-NRPC sign+seal AES, post-ZeroLogon). The DC performs the validation; no local NT hash is
  stored or used. **Without a machine account, domain-user NTLM fails with
  `STATUS_LOGON_FAILURE`.**

> **Machine secret sensitivity:** the machine account shared secret is stored at rest in
> plaintext (like the LDAP bind password and Kerberos keytab). It is write-only and redacted from
> API responses. Protect it with filesystem permissions equivalent to a keytab file (`chmod 600`).

NTLM security tradeoffs:

- No mutual authentication (client cannot verify server identity)
- Vulnerable to relay attacks without channel binding
- Session key derived from password hash (weaker than Kerberos session key)
- Signing uses HMAC-SHA256 (weaker than AES-CMAC/GMAC)
- NETLOGON passthrough requires a pre-provisioned machine account

**Recommendation:** configure Kerberos for all production deployments. Use NTLM only as a
transition mechanism or for clients that cannot use Kerberos.

### Active Directory and LDAP

DittoFS can authenticate Active Directory domain users directly, with no local DittoFS
account for them. Three pieces are independent and can be enabled in any combination:

| Piece | What it does | Credential at rest |
|---|---|---|
| **Kerberos service keytab** | Verifies AD-issued service tickets over SMB (SPNEGO) and NFS (RPCSEC\_GSS) | Keytab file |
| **Machine account** (`kerberos.machine_account`) | Forwards the NTLM challenge/response to the Domain Controller over a sealed NETLOGON channel, for clients that cannot use Kerberos | Computer account secret |
| **LDAP idmap** | Maps the authenticated principal or SID to a Unix UID/GID and resolves nested group membership | Read-only bind DN + password |

Security notes:

- With a machine account configured, AD-user NTLM is validated by the Domain Controller —
  **no local NT hash is stored or used**. Without one, domain-user NTLM fails with
  `STATUS_LOGON_FAILURE`.
- The keytab, the LDAP bind password, and the machine account secret are stored at rest in
  plaintext (write-only, and redacted from API responses). Protect them with filesystem
  permissions equivalent to a keytab file (`chmod 600`).
- The LDAP bind account is read-only; reach the directory over LDAPS or a private network.
- A principal is resolved through the same idmap regardless of the protocol it arrived on,
  so an AD user gets one Unix identity across SMB and NFS.

See [identity.md](identity.md) for the provider reference and
[windows-ad-setup.md](windows-ad-setup.md) for the end-to-end operator runbook.

---

## Message integrity and encryption

### SMB signing

DittoFS supports SMB message signing, providing integrity protection against man-in-the-middle
attacks and message tampering.

| Algorithm | Dialect | Notes |
|-----------|---------|-------|
| HMAC-SHA256 | SMB 2.x | Legacy |
| AES-128-CMAC | SMB 3.0+ | SP800-108 derived key |
| AES-128-GMAC | SMB 3.1.1 | Preferred; leverages GCM hardware acceleration |

Signing behavior:

- **Enabled** (default `true`): server advertises signing capability during `NEGOTIATE`.
- **Required** (default `false`): when `true`, the server rejects unsigned messages from
  established sessions.

Configure via `dfsctl`, where the two states above are selected by one tri-state flag
(`disabled` | `enabled` | `required`):

```bash
# Advertise signing and reject unsigned messages
./dfsctl adapter settings smb update --signing required

# Advertise signing but still accept unsigned messages (the default)
./dfsctl adapter settings smb update --signing enabled
```

**Recommendation:** set `required: true` for all production deployments to prevent tampering even
when encryption is not used.

### SMB encryption

SMB3 provides AEAD encryption delivering both confidentiality and integrity for all messages on
an encrypted session.

**Supported cipher suites:**

| Cipher | Dialect | Performance |
|--------|---------|-------------|
| AES-128-GCM | 3.1.1 (default) | Fastest (AES-NI + CLMUL) |
| AES-128-CCM | 3.0/3.0.2 (default) | Good (AES-NI) |
| AES-256-GCM | 3.1.1 | Fast, higher security |
| AES-256-CCM | 3.0+ | Highest security AES-CCM variant |

**Encryption modes:**

- **`disabled`**: no encryption. Suitable for testing only. `dfs` logs a startup warning when
  SMB is bound to a non-loopback address in this mode.
- **`preferred`** *(shipped default)*: SMB 3.x sessions are encrypted; SMB 2.x sessions are
  still accepted. Secure by default while remaining wire-compatible with SMB 2.x.
- **`required`**: only encrypted SMB 3.x clients can connect. SMB 2.x clients are rejected at
  `NEGOTIATE`. Recommended for production with sensitive data.

**Per-share encryption:** individual shares can require encryption via `--encrypt-data`
(`dfsctl share create --encrypt-data`). This forces encryption on that tree regardless of the
global mode.

**Recommended production YAML:**

```yaml
adapters:
  smb:
    encryption:
      encryption_mode: required   # reject SMB 2.x; encrypt all SMB 3.x sessions
    signing:
      required: true              # reject unsigned messages
```

See [`../internals/security-model.md`](../internals/security-model.md) for cipher/key derivation
internals and the full transport security comparison table.

---

## Access control

### NFSv4 ACLs

DittoFS supports NFSv4 ACL-based access control with:

- Per-user and per-group access control entries (ACEs)
- Explicit ALLOW and DENY entries evaluated in order
- Granular permission bits (read data, write data, append, execute, delete, read/write
  attributes, read/write ACL, etc.)
- Inheritance flags for directories

ACLs are enforced at the metadata layer. DENY entries take precedence when encountered before a
matching ALLOW entry, following the NFSv4 specification. When both NFSv4 ACLs and POSIX
permissions are present, ACLs take precedence for NFSv4 operations.

### POSIX file permissions

Traditional Unix file permissions are enforced at the metadata layer: owner, group, other
permission bits are checked in that order. When Kerberos is enabled, the Kerberos principal is
mapped to a UID/GID before the permission check.

### Export-level access control

Client access is restricted per share by attaching a **netgroup** — a named list of
IP addresses, CIDR ranges and hostnames. A share with a netgroup accepts only
clients whose address matches a member; a share with no netgroup accepts any
client.

```bash
dfsctl netgroup create trusted-net
dfsctl netgroup add-member trusted-net --type cidr --value 192.168.1.0/24
dfsctl netgroup add-member trusted-net --type ip   --value 192.168.1.50
dfsctl share nfs-config set /export --netgroup trusted-net
```

The netgroup is enforced on both NFS versions, on paths that do not share code:

- **NFSv3** — checked at MOUNT and refused with `MNT3ERR_ACCES`. The check runs
  **only** at mount time; the per-operation path re-checks the auth-flavor
  policy but not the client allowlist.
- **NFSv4** — there is no MOUNT, so the check runs per request instead: when a
  share handle enters a compound (PUTFH, or a LOOKUP crossing an export
  junction out of the pseudo-fs) and again whenever an operation builds an auth
  context. A client outside the allowlist gets `NFS4ERR_ACCESS`.

**Consequence for tightening an export.** Because v4 re-checks per request,
adding or narrowing a netgroup takes effect on a v4 client's next operation,
including one already holding a file handle. On v3 it takes effect for new
mounts only — an already-mounted v3 client keeps working until it remounts.
Remount is what re-reads the allowlist on v3.

Which authentication flavors an export accepts is a separate policy, and it
*is* re-checked per operation on both versions — see `--allow-auth-sys` /
`--require-kerberos` below.

Which authentication flavors an export accepts is separate from the client
allowlist: set `--allow-auth-sys` / `--require-kerberos` with the same
`dfsctl share nfs-config set` command, and see
[configuration](configuration.md) for the Kerberos floor.

### Identity mapping

Squash mode is a per-share setting (`squash`). The anonymous UID/GID come from
the share's anonymous identity.

It is applied *after* the share grant is resolved and *before* file-level POSIX
and ACL checks. That ordering matters under `all_to_guest`: the share-level
permission is still chosen from the client's original wire identity, and only
the identity used for per-file checks is squashed. If a share grant depends on
the caller being a known user, squashing does not turn that grant into a guest
grant.

All squash — every client's *file-level* identity is anonymous:

```bash
dfsctl share nfs-config set /export --squash all_to_guest
```

Root squash — root (UID 0) is mapped to anonymous for file checks, everyone else passes through:

```bash
dfsctl share nfs-config set /export --squash root_to_guest
```

No squashing — client UIDs pass through unchanged; use only on trusted networks
or with Kerberos:

```bash
dfsctl share nfs-config set /export --squash none
```

The full set of modes is `none`, `root_to_admin`, `root_to_guest`,
`all_to_admin` and `all_to_guest`.

### Read-only shares

```bash
dfsctl share edit /export --read-only true
```

A read-only share fails write and create operations with `EROFS`
(`ErrReadOnly`), distinct from the `EACCES` a permission denial returns.

---

## Network security

### NFS-over-TLS (RFC 9289)

DittoFS implements the opportunistic `AUTH_TLS` STARTTLS upgrade from RFC 9289: a client sends
a `NULL` RPC with `auth_flavor = AUTH_TLS`, the server replies with the `"STARTTLS"` verifier,
and both perform a TLS 1.3 handshake on the same connection — all subsequent traffic is
encrypted. The handshake runs in userspace (no kernel TLS / `tlshd` on the server). Setting
`client_ca` requires and verifies a client certificate (mutual TLS).

```yaml
adapters:
  nfs:
    tls:
      cert_file: /etc/dittofs/tls/tls.crt
      key_file:  /etc/dittofs/tls/tls.key
      client_ca: /etc/dittofs/tls/ca.crt   # optional — mutual TLS
      mode: opportunistic                   # "opportunistic" (default) | "require"
```

- **`opportunistic`** still serves non-TLS clients in plaintext (smooth rollout).
- **`require`** rejects plaintext connections until they upgrade.
- DittoFS only loads and hot-reloads cert files; issuance and renewal are the platform's
  responsibility.
- **Client support:** Linux `mount -o vers=4.1,xprtsec=tls` needs `tlshd` + `CONFIG_NET_HANDSHAKE`
  (RHEL 9.x / kernel 6.7+). macOS has no NFS-over-TLS client — use Kerberos or a VPN tunnel.

### Control plane API TLS

The control plane REST API (default port `8080`) carries the highest-value credentials in the
system. It binds to `127.0.0.1` (loopback only) by default — a fresh `dfs start` does not
expose credentials off-host.

Any deployment that accepts connections from another machine (multi-host, Kubernetes) should set
`controlplane.host: 0.0.0.0` and pair it with TLS:

```yaml
controlplane:
  host: 0.0.0.0
  tls:
    cert_file: /etc/dittofs/tls/tls.crt
    key_file: /etc/dittofs/tls/tls.key
    client_ca: /etc/dittofs/tls/ca.crt   # optional — mutual TLS
    min_version: "1.2"                    # "1.2" (default) or "1.3"
```

DittoFS loads the files and hot-reloads them on rotation. It does not generate self-signed
certificates or perform ACME. Setting `cert_file` without `key_file` (or vice versa), or
`client_ca` without a server cert, is a fatal startup error.

**Recommended deployment model:**

- **Kubernetes / internet-facing edge:** terminate TLS at an ingress, service mesh, or reverse
  proxy (NGINX) in front of DittoFS.
- **Direct `dfsctl` access / non-Kubernetes hosts:** use DittoFS native TLS or mTLS.

`dfsctl` TLS is captured once at login and reused by every later command:

```bash
# Private CA
dfsctl login --server https://host:port --cacert /etc/dittofs/tls/ca.crt -u admin

# Mutual TLS
dfsctl login --server https://host:port --cacert ca.crt \
    --client-cert client.crt --client-key client.key -u admin

dfsctl share list   # reuses stored CA / client cert — no flags needed
```

The login stores certificate file paths (not contents); each command re-reads them, so
cert-manager rotation is picked up automatically. `--tls-skip-verify` is insecure and intended
only for development against self-signed certs.

See [`./configuration.md`](./configuration.md) for the full option reference.

### Network-level alternatives

Without NFS-over-TLS, NFS data travels in cleartext. Use one of:

1. **WireGuard (recommended):** set up a WireGuard VPN between client and server, then mount
   over the VPN interface.

2. **IPsec:** configure an IPsec tunnel so NFS traffic flows through an encrypted tunnel.

3. **SSH tunnel:**
   ```bash
   ssh -L 12049:localhost:12049 user@server
   sudo mount -t nfs -o nfsvers=4,tcp,port=12049 localhost:/export /mnt/test
   ```

### Firewall configuration

Restrict access to DittoFS ports:

```bash
# Linux (iptables)
sudo iptables -A INPUT -p tcp --dport 12049 -s 192.168.1.0/24 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 12049 -j DROP

# Linux (firewalld)
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port protocol="tcp" port="12049" accept'
sudo firewall-cmd --reload

# macOS (pf) — add to /etc/pf.conf:
# pass in proto tcp from 192.168.1.0/24 to any port 12049
# block in proto tcp from any to any port 12049
sudo pfctl -f /etc/pf.conf
```

---

## Kerberos configuration

DittoFS uses a shared Kerberos layer (`pkg/auth/kerberos`) that serves both the NFS (RPCSEC_GSS)
and SMB (SPNEGO) adapters. Configure Kerberos once and both protocols benefit.

### Server configuration

Add the `kerberos` section to your DittoFS config file:

```yaml
kerberos:
  enabled: true
  keytab_path: /etc/dittofs/dittofs.keytab
  service_principal: nfs/server.example.com@EXAMPLE.COM
  krb5_conf: /etc/krb5.conf
  max_clock_skew: 5m
  context_ttl: 8h
```

Configuration fields:

| Field | Description | Default |
|-------|-------------|---------|
| `enabled` | Enable Kerberos authentication | `false` |
| `keytab_path` | Path to the Kerberos keytab file | required when enabled |
| `service_principal` | Service principal name in `service/hostname@REALM` format | required when enabled |
| `krb5_conf` | Path to `krb5.conf` | `/etc/krb5.conf` |
| `max_clock_skew` | Maximum allowed clock difference between client and server | `5m` |
| `context_ttl` | Maximum lifetime of an RPCSEC_GSS security context | `8h` |

### Keytab management

The keytab file contains the service principal's cryptographic key. It must be:

- Readable only by the DittoFS process user
- Stored with restricted permissions (`chmod 600`)
- Rotated periodically per your organization's security policy

DittoFS supports hot-reload of the keytab: when the file is replaced on disk, the server picks
up the new key without a restart.

Create a keytab with MIT Kerberos:

```bash
# On the KDC or using kadmin
kadmin -q "addprinc -randkey nfs/server.example.com@EXAMPLE.COM"
kadmin -q "ktadd -k /etc/dittofs/dittofs.keytab nfs/server.example.com@EXAMPLE.COM"

chmod 600 /etc/dittofs/dittofs.keytab
chown dittofs:dittofs /etc/dittofs/dittofs.keytab
```

### Environment variable overrides

Useful for container deployments and CI/CD pipelines:

| Environment variable | Config field | Notes |
|---------------------|--------------|-------|
| `DITTOFS_KERBEROS_KEYTAB` | `keytab_path` | Primary override |
| `DITTOFS_KERBEROS_KEYTAB_PATH` | `keytab_path` | Compatibility alias |
| `DITTOFS_KERBEROS_PRINCIPAL` | `service_principal` | Primary override |
| `DITTOFS_KERBEROS_SERVICE_PRINCIPAL` | `service_principal` | Compatibility alias |

### NFS client setup

```bash
# Mount with Kerberos (krb5 security flavor)
sudo mount -t nfs -o sec=krb5,nfsvers=4,tcp,port=12049 server.example.com:/export /mnt/secure

# Verify mount
mount | grep /mnt/secure

# Ensure client has a valid ticket
kinit user@EXAMPLE.COM
klist
```

### SMB client setup

SMB clients that support Kerberos (Windows, smbclient, CIFS kernel module) automatically
negotiate Kerberos via SPNEGO during session setup when the client has a valid TGT.

```bash
# Linux: mount with Kerberos
sudo mount -t cifs -o sec=krb5,vers=3.0 //server.example.com/export /mnt/secure

# smbclient with Kerberos
smbclient -k //server.example.com/export
```

---

## Production hardening checklist

- [ ] Enable Kerberos authentication for NFS and SMB
- [ ] Enable SMB3 encryption with `encryption_mode: required` for sensitive data
- [ ] Enable SMB message signing with `required: true`
- [ ] Deploy behind VPN or use NFS-over-TLS for NFS data confidentiality
- [ ] Restrict export access by IP address (attach a netgroup to each share)
- [ ] Use root squash on all exports (`squash: root_to_guest`)
- [ ] Configure NFSv4 ACLs for fine-grained access control
- [ ] Use read-only exports where writes are not needed
- [ ] Bind control plane to loopback or behind a reverse proxy with TLS
- [ ] Set keytab permissions to `600`, owned by the DittoFS process user
- [ ] Enable Prometheus metrics and monitor failed authentication attempts
- [ ] Keep DittoFS updated; apply patches promptly

## Secure configuration example

```yaml
logging:
  level: WARN
  format: json
  output: /var/log/dittofs/security.log

kerberos:
  enabled: true
  keytab_path: /etc/dittofs/dittofs.keytab
  service_principal: nfs/server.example.com@EXAMPLE.COM
  krb5_conf: /etc/krb5.conf
  max_clock_skew: 5m
  context_ttl: 8h

# Shares, their netgroup allowlist and their squash mode live in the control
# plane, not this file. Create them once:
#
#   dfsctl netgroup create trusted-net
#   dfsctl netgroup add-member trusted-net --type cidr --value 10.0.0.0/8
#   dfsctl share create --name /export --metadata default --block-store default \
    --read-only --squash root_to_guest
#   dfsctl share nfs-config set /export --netgroup trusted-net

adapters:
  nfs:
    port: 12049
    max_connections: 100
    timeouts:
      idle: 5m
  smb:
    port: 12445
    signing:
      enabled: true
      required: true
    encryption:
      encryption_mode: required
      allowed_ciphers: []
```

See [`./configuration.md`](./configuration.md) for the full adapter configuration reference.

---

## Planned security features

- [ ] Encryption at rest for content stores
- [ ] Encrypted metadata storage
- [ ] Audit logging for all file operations and failed authentication attempts
- [ ] Integration with SIEM systems
- [ ] Role-based access control (RBAC) for administrative operations

---

## Reporting security issues

If you discover a security vulnerability in DittoFS:

1. **Do not** open a public GitHub issue.
2. Email security concerns to the maintainers (see repository for contact).
3. Include: description of the vulnerability, steps to reproduce, potential impact, and suggested
   fixes if any.

We will acknowledge receipt within 48 hours and provide a timeline for a fix.
