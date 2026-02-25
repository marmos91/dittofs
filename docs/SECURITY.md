# DittoFS Security Considerations

**Current Security Status**: DittoFS is experimental software and has not undergone formal security auditing. Exercise caution before deploying in production environments without thorough testing and security review.

## Table of Contents

- [Security Overview](#security-overview)
- [Authentication](#authentication)
  - [NFS: RPCSEC_GSS (Kerberos)](#nfs-rpcsec_gss-kerberos)
  - [NFS: AUTH_UNIX](#nfs-auth_unix)
  - [NFS: AUTH_NULL](#nfs-auth_null)
  - [SMB: Kerberos via SPNEGO](#smb-kerberos-via-spnego)
- [Message Integrity](#message-integrity)
  - [SMB Message Signing](#smb-message-signing)
- [Access Control](#access-control)
  - [NFSv4 ACLs](#nfsv4-acls)
  - [POSIX File Permissions](#posix-file-permissions)
  - [Export-Level Access Control](#export-level-access-control)
  - [IP-Based Restrictions](#ip-based-restrictions)
  - [Identity Mapping](#identity-mapping)
  - [Read-Only Shares](#read-only-shares)
- [Network Security](#network-security)
- [Kerberos Configuration](#kerberos-configuration)
  - [Server Configuration](#server-configuration)
  - [Keytab Management](#keytab-management)
  - [Environment Variable Overrides](#environment-variable-overrides)
  - [NFS Client Configuration](#nfs-client-configuration)
  - [SMB Client Configuration](#smb-client-configuration)
- [Remaining Limitations](#remaining-limitations)
- [Planned Security Features](#planned-security-features)
- [Production Recommendations](#production-recommendations)
- [Security Best Practices](#security-best-practices)
- [Reporting Security Issues](#reporting-security-issues)
- [References](#references)

## Security Overview

### Implemented Security Features

- Kerberos authentication for NFS via RPCSEC_GSS (RFC 2203)
- Kerberos authentication for SMB via SPNEGO
- SMB message signing with HMAC-SHA256
- NFSv4 ACL-based access control
- POSIX file permission enforcement (owner/group/other)
- Export-level IP-based access restrictions
- Identity mapping (root squash, all squash)
- AUTH_UNIX support for trusted-network deployments

### Remaining Limitations

- No formal security audit performed
- No built-in encryption in transit for NFS (use VPN or network-level encryption)
- No built-in encryption at rest
- No audit logging for file operations

## Authentication

### NFS: RPCSEC_GSS (Kerberos)

DittoFS implements RPCSEC_GSS authentication per RFC 2203, enabling Kerberos-based strong authentication for NFSv4 clients. This is the recommended authentication method for any deployment outside of a fully trusted network.

When enabled, clients authenticate using Kerberos tickets. The server validates tickets against its keytab and maps Kerberos principals to Unix UID/GID for authorization decisions.

Key properties:
- Mutual authentication (server identity is also verified by the client)
- Cryptographic credential verification (no trust-based UID spoofing)
- Configurable context lifetime and clock skew tolerance
- Hot-reload support for keytab rotation without server restart

See [Kerberos Configuration](#kerberos-configuration) for setup instructions.

### NFS: AUTH_UNIX

AUTH_UNIX is the traditional NFS authentication mechanism. The client provides UID, GID, and supplementary GIDs with each request. The server trusts these values without independent verification.

- Suitable for trusted networks only
- Clients can impersonate any user by sending arbitrary UID/GID values
- Use identity mapping (root squash, all squash) to limit exposure

### NFS: AUTH_NULL

AUTH_NULL provides anonymous access with no authentication. All requests are treated as coming from an unauthenticated user. Use with extreme caution and only for public read-only shares.

### SMB: Kerberos via SPNEGO

The SMB adapter supports Kerberos authentication through the SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism) protocol during SESSION_SETUP. When a Kerberos provider is configured, SMB clients can authenticate using Kerberos tickets.

When Kerberos is not configured, the SMB adapter falls back to NTLM or guest authentication.

See [Kerberos Configuration](#kerberos-configuration) for shared Kerberos setup that applies to both NFS and SMB.

## Message Integrity

### SMB Message Signing

DittoFS supports SMB2 message signing using HMAC-SHA256, providing integrity protection against man-in-the-middle attacks and message tampering.

Signing behavior is configurable per the MS-SMB2 specification:

- **Enabled** (default: `true`): The server advertises signing capability during NEGOTIATE by setting `SMB2_NEGOTIATE_SIGNING_ENABLED`.
- **Required** (default: `false`): When set to `true`, the server sets `SMB2_NEGOTIATE_SIGNING_REQUIRED` and rejects unsigned messages from established sessions.

For production deployments, set `required: true` to enforce signing on all sessions.

Signing is configured via the control plane when creating or updating the SMB adapter:

```bash
./dfsctl adapter create --type smb --config '{
  "signing": {
    "enabled": true,
    "required": true
  }
}'
```

## Access Control

### NFSv4 ACLs

DittoFS supports NFSv4 ACL-based access control, providing fine-grained permission management beyond traditional POSIX owner/group/other modes. NFSv4 ACLs allow:

- Per-user and per-group access control entries (ACEs)
- Explicit ALLOW and DENY entries with defined ordering
- Granular permission bits (read data, write data, append, execute, delete, read attributes, write attributes, read ACL, write ACL, etc.)
- Inheritance flags for directories (propagation to new files and subdirectories)

ACLs are enforced at the metadata layer and evaluated in order: DENY entries take precedence when encountered before a matching ALLOW entry, following the NFSv4 specification.

### POSIX File Permissions

Traditional Unix file permissions are enforced at the metadata layer:

```go
// Enforced at metadata layer
func (m *MetadataStore) CheckAccess(handle FileHandle, authCtx *AuthContext) error {
    attr := m.GetFile(handle)

    // Check owner
    if attr.UID == authCtx.UID {
        // Check owner permissions
    }

    // Check group
    if attr.GID == authCtx.GID {
        // Check group permissions
    }

    // Check other permissions
}
```

When both NFSv4 ACLs and POSIX permissions are present, the ACL takes precedence for NFSv4 operations.

### Export-Level Access Control

```yaml
shares:
  - name: /export
    # IP-based access control
    allowed_clients:
      - 192.168.1.0/24
    denied_clients:
      - 192.168.1.50

    # Authentication requirements
    require_auth: true
    allowed_auth_methods: [unix, krb5]
```

### IP-Based Restrictions

**Allow specific networks:**
```yaml
shares:
  - name: /export
    allowed_clients:
      - 192.168.1.0/24  # Local network
      - 10.0.0.0/8       # Private network
```

**Deny specific hosts:**
```yaml
shares:
  - name: /export
    denied_clients:
      - 192.168.1.100   # Block specific IP
```

### Identity Mapping

**All Squash (map all users to anonymous):**
```yaml
shares:
  - name: /export
    identity_mapping:
      map_all_to_anonymous: true
      anonymous_uid: 65534  # nobody
      anonymous_gid: 65534  # nogroup
```

**Root Squash (map root to anonymous):**
```yaml
shares:
  - name: /export
    identity_mapping:
      map_privileged_to_anonymous: true  # root becomes nobody
      anonymous_uid: 65534
      anonymous_gid: 65534
```

**No Squashing (trust client UIDs):**
```yaml
shares:
  - name: /export
    identity_mapping:
      map_all_to_anonymous: false
      map_privileged_to_anonymous: false
```

**Warning**: No squashing trusts client-provided UIDs completely. Only use on trusted networks or when Kerberos authentication is enforced.

### Read-Only Shares

**Prevent all writes:**
```yaml
shares:
  - name: /export
    read_only: true  # All write operations will fail
```

## Network Security

### Encryption in Transit

DittoFS does not currently provide built-in TLS encryption for NFS or SMB wire traffic. While Kerberos provides authentication and message integrity, file data is transmitted in cleartext over TCP unless network-level encryption is used.

**Implications:**
- File data can be intercepted without network-level encryption
- Use VPN, IPsec, or WireGuard to protect data in transit
- SMB message signing protects integrity but not confidentiality

### Network-Level Protection

**Use VPN or encrypted tunnels:**

1. **WireGuard (Recommended):**
   ```bash
   # Set up WireGuard VPN between client and server
   # Then mount over the VPN interface
   sudo mount -t nfs -o nfsvers=4,tcp,port=2049 10.0.0.1:/export /mnt/test
   ```

2. **IPsec:**
   ```bash
   # Configure IPsec tunnel between client and server
   # NFS traffic flows through encrypted tunnel
   ```

3. **SSH Tunnel:**
   ```bash
   # Forward NFS port through SSH
   ssh -L 12049:localhost:12049 user@server

   # Mount through tunnel
   sudo mount -t nfs -o nfsvers=4,tcp,port=12049 localhost:/export /mnt/test
   ```

### Firewall Configuration

**Restrict access to DittoFS ports:**

```bash
# Linux (iptables)
sudo iptables -A INPUT -p tcp --dport 12049 -s 192.168.1.0/24 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 12049 -j DROP

# Linux (firewalld)
sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port protocol="tcp" port="12049" accept'
sudo firewall-cmd --reload

# macOS (pf)
# Add to /etc/pf.conf:
# pass in proto tcp from 192.168.1.0/24 to any port 12049
# block in proto tcp from any to any port 12049
sudo pfctl -f /etc/pf.conf
```

## Kerberos Configuration

DittoFS uses a shared Kerberos layer (`pkg/auth/kerberos`) that serves both the NFS (RPCSEC_GSS) and SMB (SPNEGO) adapters. Configure Kerberos once and both protocols benefit.

### Server Configuration

Add the `kerberos` section to your DittoFS configuration file:

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
| `keytab_path` | Path to the Kerberos keytab file | (required when enabled) |
| `service_principal` | Service principal name (SPN) in `service/hostname@REALM` format | (required when enabled) |
| `krb5_conf` | Path to `krb5.conf` | `/etc/krb5.conf` |
| `max_clock_skew` | Maximum allowed clock difference between client and server | `5m` |
| `context_ttl` | Maximum lifetime of an RPCSEC_GSS security context | `8h` |

### Keytab Management

The keytab file contains the service principal's cryptographic key. It must be:

- Readable only by the DittoFS process user
- Stored securely with restricted file permissions (`chmod 600`)
- Rotated periodically according to your organization's security policy

DittoFS supports hot-reload of the keytab file. When the keytab is replaced on disk, the server picks up the new key without requiring a restart.

**Create a keytab (example with MIT Kerberos):**
```bash
# On the KDC or using kadmin
kadmin -q "addprinc -randkey nfs/server.example.com@EXAMPLE.COM"
kadmin -q "ktadd -k /etc/dittofs/dittofs.keytab nfs/server.example.com@EXAMPLE.COM"

# Set appropriate permissions
chmod 600 /etc/dittofs/dittofs.keytab
chown dittofs:dittofs /etc/dittofs/dittofs.keytab
```

### Environment Variable Overrides

Kerberos configuration can be overridden with environment variables, which is useful for container deployments and CI/CD pipelines:

| Environment Variable | Config Field | Notes |
|---------------------|--------------|-------|
| `DITTOFS_KERBEROS_KEYTAB` | `keytab_path` | Primary override |
| `DITTOFS_KERBEROS_KEYTAB_PATH` | `keytab_path` | Compatibility alias |
| `DITTOFS_KERBEROS_PRINCIPAL` | `service_principal` | Primary override |
| `DITTOFS_KERBEROS_SERVICE_PRINCIPAL` | `service_principal` | Compatibility alias |

### NFS Client Configuration

Mount with Kerberos authentication:

```bash
# Mount with Kerberos (krb5 security flavor)
sudo mount -t nfs -o sec=krb5,nfsvers=4,tcp,port=2049 server.example.com:/export /mnt/secure

# Verify the mount is using Kerberos
mount | grep /mnt/secure
```

Ensure the client has a valid Kerberos ticket:
```bash
kinit user@EXAMPLE.COM
klist
```

### SMB Client Configuration

SMB clients that support Kerberos (Windows, smbclient, CIFS kernel module) will automatically negotiate Kerberos via SPNEGO during session setup when the client has a valid ticket-granting ticket (TGT).

```bash
# Linux: mount with Kerberos
sudo mount -t cifs -o sec=krb5,vers=3.0 //server.example.com/export /mnt/secure

# smbclient with Kerberos
smbclient -k //server.example.com/export
```

## Remaining Limitations

- **No formal security audit**: The codebase has not been reviewed by a third-party security firm
- **No built-in TLS**: Wire-level encryption requires network-level solutions (VPN, IPsec)
- **No encryption at rest**: Content stores do not encrypt data (S3 server-side encryption can be used independently)
- **No audit logging**: File operation audit trail is not yet implemented
- **AUTH_UNIX trust model**: When Kerberos is not enabled, NFS AUTH_UNIX trusts client-provided UIDs

## Planned Security Features

### Encryption
- [ ] Built-in TLS support for RPC transport
- [ ] Encryption at rest for content stores
- [ ] Encrypted metadata storage

### Auditing
- [ ] Audit logging for all file operations
- [ ] Failed authentication tracking
- [ ] Suspicious activity detection
- [ ] Integration with SIEM systems

### Advanced Access Control
- [ ] Role-based access control (RBAC) for administrative operations
- [ ] Attribute-based access control (ABAC)
- [ ] Per-file encryption keys

## Production Recommendations

### Deployment Checklist

- [ ] Enable Kerberos authentication for NFS and SMB
- [ ] Enable SMB message signing with `required: true`
- [ ] Deploy behind VPN or use network-level encryption for data confidentiality
- [ ] Use read-only exports where appropriate
- [ ] Enable monitoring and alerting
- [ ] Restrict export access by IP address
- [ ] Use root squashing for all exports
- [ ] Configure NFSv4 ACLs for fine-grained access control
- [ ] Regular security updates
- [ ] Periodic security audits

### Secure Configuration Example

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

metadata:
  global:
    dump_restricted: true
    dump_allowed_clients:
      - 127.0.0.1  # Only localhost can see mounts

shares:
  - name: /export
    # Network restrictions
    allowed_clients:
      - 10.0.0.0/8  # Only private network

    # Authentication
    require_auth: true
    allowed_auth_methods: [krb5]

    # Identity mapping
    identity_mapping:
      map_privileged_to_anonymous: true  # Root squash
      anonymous_uid: 65534
      anonymous_gid: 65534

    # Read-only for maximum safety
    read_only: true

adapters:
  nfs:
    port: 2049
    max_connections: 100
    timeouts:
      idle: 5m
  smb:
    port: 445
    signing:
      enabled: true
      required: true
```

## Security Best Practices

### 1. Use Strong Authentication

- Enable Kerberos for all NFS and SMB clients
- Avoid AUTH_UNIX and AUTH_NULL in untrusted environments
- Enforce SMB message signing to prevent tampering
- Rotate keytabs on a regular schedule

### 2. Network Isolation

- Deploy DittoFS in isolated network segments
- Use VLANs to separate storage traffic
- Implement network segmentation
- Use VPN or IPsec for encryption in transit

### 3. Minimal Permissions

- Use least-privilege principle
- Configure NFSv4 ACLs with explicit DENY entries where needed
- Enable root squash on all exports
- Use read-only exports when possible

### 4. Monitoring

- Enable Prometheus metrics collection
- Monitor failed authentication attempts
- Alert on unusual access patterns
- Track file access patterns

### 5. Regular Updates

- Keep DittoFS updated
- Monitor security advisories
- Apply patches promptly

### 6. Defense in Depth

Do not rely on a single security measure:
- Kerberos authentication
- SMB message signing
- Network encryption (VPN/IPsec)
- IP-based access control
- NFSv4 ACLs and POSIX permissions
- Identity mapping (root squash)
- Monitoring and alerting
- Regular audits

## Reporting Security Issues

If you discover a security vulnerability in DittoFS:

1. **DO NOT** open a public GitHub issue
2. Email security concerns to the maintainers (see repository)
3. Include:
   - Description of the vulnerability
   - Steps to reproduce
   - Potential impact
   - Suggested fixes (if any)

We will acknowledge receipt within 48 hours and provide a timeline for a fix.

## References

- [RFC 1813 - NFS Version 3 Protocol Specification](https://tools.ietf.org/html/rfc1813)
- [RFC 2203 - RPCSEC_GSS Protocol Specification](https://tools.ietf.org/html/rfc2203)
- [RFC 2623 - NFS Version 2 and Version 3 Security Issues](https://tools.ietf.org/html/rfc2623)
- [RFC 4121 - The Kerberos Version 5 GSS-API Mechanism](https://tools.ietf.org/html/rfc4121)
- [RFC 7530 - NFS Version 4 Protocol](https://tools.ietf.org/html/rfc7530)
- [MS-SMB2 - Server Message Block Protocol Versions 2 and 3](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/)
