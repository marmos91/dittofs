# Stack Research: NFSv4 Protocol Implementation for DittoFS

**Domain:** NFSv4 protocol server implementation in Go
**Researched:** 2026-02-04
**Confidence:** MEDIUM (Go NFSv4 ecosystem is sparse; recommendations based on available options + protocol requirements)

## Executive Summary

The Go ecosystem for NFSv4 server implementation is nascent compared to C-based solutions like NFS-Ganesha. No single library provides production-ready NFSv4.x with RPCSEC_GSS. The recommended approach is to **extend DittoFS's existing hand-written XDR/RPC** rather than adopt external libraries, while leveraging `jcmturner/gokrb5` (already in go.mod) for Kerberos primitives. RPCSEC_GSS must be implemented from RFC 2203/5403 specifications.

---

## Recommended Stack

### Core Technologies

| Technology | Version | Purpose | Why Recommended | Confidence |
|------------|---------|---------|-----------------|------------|
| **Go** | 1.25+ | Language runtime | Already in use; concurrent, performant | HIGH |
| **Custom XDR** | N/A | XDR encoding/decoding | Extend existing `internal/protocol/nfs/xdr/`; avoids dependency churn, full control over NFSv4 compound ops | HIGH |
| **Custom RPC** | N/A | ONC RPC layer | Extend existing `internal/protocol/nfs/rpc/`; RPCSEC_GSS integration requires tight coupling | HIGH |
| **jcmturner/gokrb5/v8** | v8.4.4 | Kerberos 5 | Pure Go, already in go.mod, active maintenance, AD integration | HIGH |

### Supporting Libraries

| Library | Version | Purpose | When to Use | Confidence |
|---------|---------|---------|-------------|------------|
| **github.com/jcmturner/gokrb5/v8/gssapi** | v8.4.4 | GSS-API primitives | Wrap/MIC tokens for RPCSEC_GSS integrity/privacy | HIGH |
| **github.com/jcmturner/gokrb5/v8/spnego** | v8.4.4 | SPNEGO negotiation | If SPNEGO mechlist needed (rare for pure NFS) | MEDIUM |
| **github.com/davecgh/go-xdr/xdr2** | latest | Reference/fallback XDR | Testing, validation of custom implementation | MEDIUM |
| **github.com/xdrpp/goxdr** | latest | XDR code generator | Generate NFSv4 types from .x specs if starting fresh | LOW |

### Development Tools

| Tool | Purpose | Notes |
|------|---------|-------|
| **wireshark** | Protocol debugging | NFS/RPC dissectors essential for debugging |
| **rpcdebug** | Linux RPC tracing | Kernel-side debugging |
| **kinit/klist** | Kerberos ticket management | Testing RPCSEC_GSS |
| **MIT krb5 / Heimdal** | KDC for testing | Local KDC for development |

---

## Decision Rationale

### XDR: Extend Custom vs. Use Library

**Decision:** Extend existing custom XDR in `internal/protocol/nfs/xdr/`

**Why:**
1. DittoFS already has ~400 lines of hand-tuned XDR (encode.go, decode.go)
2. NFSv4 compound operations require streaming encode/decode with variable-length arrays
3. External libraries (davecgh/go-xdr, xdrpp/goxdr) are reflection-based or code-generator-based
4. Custom code allows zero-copy optimizations crucial for NFS performance
5. RFC 7530/7531/8881/7862 XDR specs can be translated directly

**Tradeoffs:**
- More code to maintain
- Must verify correctness against specs
- No code generation from .x files

**Alternative considered:** `xdrpp/goxdr` for code generation
- Pros: RFC 5531 compliant, generates from .x files
- Cons: Less control, another dependency, may conflict with existing patterns
- When to use: Greenfield NFSv4 project without existing XDR

### RPC: Extend Custom vs. Use Library

**Decision:** Extend existing custom RPC in `internal/protocol/nfs/rpc/`

**Why:**
1. RPCSEC_GSS authentication must integrate with RPC call/reply handling
2. Existing auth.go handles AUTH_UNIX; extend pattern for RPCSEC_GSS
3. NFSv4 uses single RPC program (100003) with compound operations
4. Session management (NFSv4.1+) requires RPC-level state tracking
5. No Go library provides RPCSEC_GSS out-of-box

**Key extensions needed:**
- `RPCSEC_GSS` auth flavor (6) handling
- GSS context establishment (INIT/CONTINUE_INIT/DESTROY)
- Sequence window and replay protection
- Integrity (MIC) and Privacy (wrap) message verification

### Kerberos/GSSAPI: gokrb5 vs. C Bindings

**Decision:** Use `jcmturner/gokrb5/v8` (already in go.mod)

**Why:**
1. Pure Go - no CGO, cross-compiles cleanly to Linux containers
2. Already a dependency (go.mod shows v8.4.4)
3. Implements RFC 4121 (Kerberos V5 GSS-API)
4. Supports keytab authentication for service principals
5. Active maintenance, well-tested with AD environments

**What gokrb5 provides:**
- Kerberos ticket acquisition and validation
- `gssapi.WrapToken` for RPCSEC_GSS_INTEGRITY/PRIVACY
- `gssapi.MICToken` for message authentication codes
- AD PAC decoding for authorization data

**What gokrb5 does NOT provide:**
- RPCSEC_GSS protocol layer (must implement RFC 2203)
- RPC message framing with GSS tokens
- Sequence number window management

**Alternative considered:** `golang-auth/go-gssapi`
- Pros: Full GSS-API RFC 2743 interface
- Cons: v3 requires C bindings (MIT/Heimdal), v2 pure-Go "not production ready"
- When to use: If strict GSS-API compliance needed; accept CGO dependency

### NLM: Build vs. Buy

**Decision:** Implement custom NLM for NFSv3 compatibility (if needed)

**Why:**
1. No Go NLM library exists
2. NLM is obsolete with NFSv4 (locking is built into protocol)
3. Only needed for NFSv3 backwards compatibility
4. Small protocol (~10 procedures)

**NFSv4 alternative:** Implement NFSv4 lock operations directly
- LOCK, LOCKT, LOCKU, OPEN (with share locks)
- No separate NLM daemon needed
- Simpler architecture

---

## Alternatives Considered

| Recommended | Alternative | When to Use Alternative |
|-------------|-------------|-------------------------|
| Custom XDR | `davecgh/go-xdr/xdr2` | Simple XDR needs, no performance concerns |
| Custom XDR | `xdrpp/goxdr` | Greenfield project, want code generation from .x specs |
| Custom RPC | None available | N/A - no Go RPCSEC_GSS library exists |
| `gokrb5/v8` | `golang-auth/go-gssapi` v3 | Need strict GSS-API compliance, accept CGO |
| `gokrb5/v8` | `openshift/gssapi` | Need MIT/Heimdal C library bindings |
| Custom NLM | None available | N/A - no Go NLM library exists |

---

## What NOT to Use

| Avoid | Why | Use Instead |
|-------|-----|-------------|
| **github.com/rasky/go-xdr** | Unmaintained since 2017, training data reference | `davecgh/go-xdr/xdr2` or custom |
| **github.com/smallfz/libnfs-go** | NFSv4.0 only, experimental, no Kerberos | Custom implementation |
| **github.com/kuleuven/nfs4go** | 5 commits total, minimal community, AUTH_SYS only | Reference architecture only |
| **github.com/willscott/go-nfs** | NFSv3 only, no NFSv4 support | Useful patterns but not applicable |
| **C bindings for GSSAPI** | CGO complicates deployment, cross-compilation | `gokrb5/v8` pure Go |
| **FUSE-based approach** | Adds latency, complexity; DittoFS is already userspace NFS | Direct protocol implementation |

---

## Stack Patterns by NFSv4 Minor Version

### NFSv4.0 (RFC 7530)

**Pattern:** Stateful protocol, client ID + state ID model
```
Required:
- Compound operations (single RPC, multiple ops)
- File delegation (read/write)
- ACL support
- Mandatory locking
- RPCSEC_GSS (optional but recommended)

Use:
- gokrb5/v8 for Kerberos (if RPCSEC_GSS)
- Custom state management (client IDs, state IDs, lock owners)
- 128-bit file handles
```

### NFSv4.1 (RFC 8881)

**Pattern:** Session-based, mandatory RPCSEC_GSS support
```
Additional requirements:
- Sessions (EXCHANGE_ID, CREATE_SESSION)
- Backchannel (server-to-client callbacks)
- pNFS (optional, high complexity)
- Session binding (security association)

Use:
- Session state machine implementation
- Callback RPC server (for delegations)
- gokrb5/v8 for mandatory RPCSEC_GSS
```

### NFSv4.2 (RFC 7862)

**Pattern:** Performance extensions
```
Additional requirements:
- Server-side copy (CLONE, COPY, OFFLOAD_*)
- Sparse files (ALLOCATE, DEALLOCATE)
- Labeled NFS (MAC security, sec_label attribute)
- Application I/O hints (IO_ADVISE)

Use:
- Block store support for server-side copy
- Extended attribute support for labels
```

---

## Version Compatibility Matrix

| Package | Compatible Go | NFSv4.0 | NFSv4.1 | NFSv4.2 | RPCSEC_GSS |
|---------|---------------|---------|---------|---------|------------|
| DittoFS custom XDR | 1.25+ | Extend | Extend | Extend | Extend |
| DittoFS custom RPC | 1.25+ | Extend | Extend | Extend | Extend |
| jcmturner/gokrb5/v8 | 1.18+ | Yes | Yes | Yes | Foundation |
| davecgh/go-xdr/xdr2 | 1.0+ | Reference | Reference | Reference | N/A |

---

## Installation

```bash
# Already in go.mod:
# github.com/jcmturner/gokrb5/v8 v8.4.4

# Optional - for XDR validation/testing:
go get github.com/davecgh/go-xdr/xdr2

# Optional - for code generation from .x specs:
go install github.com/xdrpp/goxdr/cmd/goxdr@latest

# For Kerberos testing:
# macOS: brew install krb5
# Linux: apt install krb5-user krb5-kdc (or equivalent)
```

---

## Implementation Complexity Estimates

| Component | Complexity | Rationale |
|-----------|------------|-----------|
| NFSv4.0 compound ops | HIGH | ~50 operations, complex state |
| RPCSEC_GSS layer | HIGH | Security-critical, RFC 2203 + 5403 |
| NFSv4.1 sessions | VERY HIGH | State machines, backchannels |
| NFSv4.2 copy | MEDIUM | Block store integration |
| NLM (if needed) | LOW | Small protocol, obsolete |

---

## Sources

### Official Documentation (HIGH confidence)
- [RFC 7530 - NFSv4.0](https://datatracker.ietf.org/doc/rfc7530/) - Core NFSv4 protocol
- [RFC 8881 - NFSv4.1](https://datatracker.ietf.org/doc/rfc8881/) - Sessions, pNFS
- [RFC 7862 - NFSv4.2](https://datatracker.ietf.org/doc/rfc7862/) - Server-side copy, sparse files
- [RFC 2203 - RPCSEC_GSS](https://datatracker.ietf.org/doc/rfc2203/) - GSS security for RPC
- [RFC 5403 - RPCSEC_GSS v2](https://datatracker.ietf.org/doc/rfc5403/) - Channel bindings
- [RFC 4121 - Kerberos V5 GSS-API](https://datatracker.ietf.org/doc/rfc4121/) - Kerberos mechanism

### Libraries (verified via GitHub/pkg.go.dev)
- [jcmturner/gokrb5](https://github.com/jcmturner/gokrb5) - Pure Go Kerberos (v8.4.4, Feb 2023)
- [davecgh/go-xdr](https://github.com/davecgh/go-xdr) - XDR reference implementation
- [xdrpp/goxdr](https://github.com/xdrpp/goxdr) - XDR code generator (updated Dec 2024)
- [golang-auth/go-gssapi](https://github.com/golang-auth/go-gssapi) - GSS-API (v3 beta, C bindings)

### Reference Implementations (MEDIUM confidence)
- [NFS-Ganesha Architecture](https://github.com/nfs-ganesha/nfs-ganesha/wiki/NFS-Ganesha-Architecture) - FSAL pattern
- [NFS-Ganesha RPCSEC_GSS](https://github.com/nfs-ganesha/nfs-ganesha/wiki/RPCSEC_GSS) - Kerberos setup
- [kuleuven/nfs4go](https://github.com/kuleuven/nfs4go) - Go NFSv4 reference (minimal)
- [smallfz/libnfs-go](https://github.com/smallfz/libnfs-go) - Go NFS server (experimental)

### WebSearch Findings (LOW confidence - verify before use)
- NFSv4.1 session state machine patterns from RFC only
- No production Go NFSv4.1+ server implementations found
- No Go RPCSEC_GSS implementations found

---

*Stack research for: NFSv4 Protocol Implementation*
*Researched: 2026-02-04*
