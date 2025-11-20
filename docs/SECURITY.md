# DittoFS Security Considerations

⚠️ **Current Security Status**: DittoFS is experimental software and has not undergone security auditing. Do not use in production environments without thorough testing and security review.

## Table of Contents

- [Current Security Status](#current-security-status)
- [Current Implementation](#current-implementation)
- [Access Control](#access-control)
- [Authentication](#authentication)
- [Network Security](#network-security)
- [Planned Security Features](#planned-security-features)
- [Production Recommendations](#production-recommendations)
- [Security Best Practices](#security-best-practices)

## Current Security Status

### Known Limitations

- ❌ No security audit performed
- ❌ No encryption in transit (raw NFS over TCP)
- ❌ Basic AUTH_UNIX only (no Kerberos)
- ❌ No built-in encryption at rest
- ❌ No audit logging for operations
- ⚠️ File permissions enforced but not extensively tested

### Production Use

**DO NOT use DittoFS in production without:**
- Thorough security review
- Network-level encryption (VPN, IPsec, WireGuard)
- Proper access controls
- Regular security audits
- Comprehensive monitoring

## Current Implementation

### Authentication Support

**AUTH_UNIX (Supported):**
- Client provides UID, GID, and supplementary GIDs
- No verification of credentials (trust-based)
- Suitable for trusted networks only

**AUTH_NULL (Supported):**
- Anonymous access
- No authentication required
- Use with extreme caution

**AUTH_GSS/Kerberos (Not Supported):**
- Strong authentication not yet implemented
- Planned for future release

### Authorization

**File Permissions:**
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

**Export-Level Access Control:**
```yaml
shares:
  - name: /export
    # IP-based access control
    allowed_clients:
      - 192.168.1.0/24
    denied_clients:
      - 192.168.1.50

    # Authentication requirements
    require_auth: false
    allowed_auth_methods: [anonymous, unix]
```

## Access Control

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

⚠️ **Warning**: No squashing trusts client-provided UIDs completely. Only use on trusted networks.

### Read-Only Shares

**Prevent all writes:**
```yaml
shares:
  - name: /export
    read_only: true  # All write operations will fail
```

## Authentication

### Current Authentication Flow

1. **Client connects** via TCP
2. **Client sends RPC call** with auth flavor
3. **Server extracts auth context:**
   ```go
   type AuthContext struct {
       Flavor    AuthFlavor
       UID       uint32
       GID       uint32
       GIDs      []uint32
       ClientIP  string
   }
   ```
4. **Server validates access** based on export rules
5. **Operation proceeds** if authorized

### Disabling Authentication

**For development only:**
```yaml
shares:
  - name: /export
    require_auth: false
    allowed_auth_methods: [anonymous]
    identity_mapping:
      map_all_to_anonymous: true
```

## Network Security

### No Built-In Encryption

DittoFS does not encrypt NFS traffic. All data is transmitted in plaintext over TCP.

**Implications:**
- File data can be intercepted
- Credentials can be captured
- Operations can be observed

### Network-Level Protection

**Use VPN or encrypted tunnels:**

1. **WireGuard (Recommended):**
   ```bash
   # Set up WireGuard VPN between client and server
   # Then mount over the VPN interface
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 10.0.0.1:/export /mnt/test
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
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
   ```

### Firewall Configuration

**Restrict access to DittoFS port:**

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

## Planned Security Features

### Phase 1: Authentication
- [ ] Kerberos support (AUTH_GSS)
- [ ] Token-based authentication
- [ ] Certificate-based authentication

### Phase 2: Encryption
- [ ] Built-in TLS support for RPC
- [ ] Encryption at rest for content stores
- [ ] Encrypted metadata storage

### Phase 3: Auditing
- [ ] Audit logging for all operations
- [ ] Failed authentication tracking
- [ ] Suspicious activity detection
- [ ] Integration with SIEM systems

### Phase 4: Advanced Access Control
- [ ] Role-based access control (RBAC)
- [ ] Attribute-based access control (ABAC)
- [ ] Fine-grained ACLs
- [ ] Per-file encryption keys

## Production Recommendations

### Deployment Checklist

- [ ] Deploy behind VPN or use network encryption
- [ ] Implement authentication at the network layer
- [ ] Use read-only exports where appropriate
- [ ] Enable monitoring and alerting
- [ ] Restrict export access by IP address
- [ ] Use root squashing for all exports
- [ ] Enable audit logging when available
- [ ] Regular security updates
- [ ] Periodic security audits

### Secure Configuration Example

```yaml
logging:
  level: WARN
  format: json
  output: /var/log/dittofs/security.log

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
    allowed_auth_methods: [unix]

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
    max_connections: 100  # Rate limiting
    timeouts:
      idle: 5m  # Close idle connections
```

## Security Best Practices

### 1. Network Isolation

- Deploy DittoFS in isolated network segments
- Use VLANs to separate storage traffic
- Implement network segmentation

### 2. Minimal Permissions

- Use least-privilege principle
- Enable root squash on all exports
- Use read-only exports when possible

### 3. Monitoring

- Enable metrics collection
- Monitor failed authentication attempts
- Alert on unusual access patterns
- Track file access patterns

### 4. Regular Updates

- Keep DittoFS updated
- Monitor security advisories
- Apply patches promptly

### 5. Defense in Depth

Don't rely on a single security measure:
- Network encryption (VPN/IPsec)
- IP-based access control
- Authentication requirements
- File permissions
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

- [NFS Security Best Practices](https://tools.ietf.org/html/rfc2623)
- [RPCSEC_GSS Protocol Specification](https://tools.ietf.org/html/rfc2203)
- [NFS Version 4 Security](https://tools.ietf.org/html/rfc7530#section-3)
