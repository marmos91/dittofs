# Phase 33: SMB3 Dialect Negotiation and Preauth Integrity - Research

**Researched:** 2026-02-28
**Domain:** SMB3 protocol negotiation, preauth integrity hash chain, negotiate contexts, binary codec
**Confidence:** HIGH

## Summary

Phase 33 upgrades DittoFS's SMB implementation from supporting only SMB 2.0.2/2.1 to also supporting SMB 3.0, 3.0.2, and 3.1.1 dialects. The core work involves: (1) creating a dedicated binary codec package (`smbenc`) to replace ad-hoc `encoding/binary` calls, (2) implementing negotiate context parsing/encoding for SMB 3.1.1's PREAUTH_INTEGRITY_CAPABILITIES and ENCRYPTION_CAPABILITIES, (3) computing SHA-512 preauth integrity hash chains on the Connection, (4) implementing dialect-aware capability advertisement, (5) upgrading FSCTL_VALIDATE_NEGOTIATE_INFO for SMB 3.x, and (6) enforcing the SMB internal package boundary (encoding/decoding only, no business logic).

The existing codebase already has dialect constants (Dialect0300, Dialect0302, Dialect0311), SMB adapter settings with min/max dialect configuration persisted in the control plane store, and a well-established dispatch table pattern. The negotiate handler currently selects only 2.0.2/2.1 and returns STATUS_NOT_SUPPORTED for 3.x dialects. The VALIDATE_NEGOTIATE_INFO handler duplicates the negotiate selection logic -- Phase 33 must centralize this.

**Primary recommendation:** Build the smbenc codec first (it unblocks all subsequent work), then refactor negotiate to support 3.x dialects with negotiate contexts, then add preauth integrity hash hooks, then refactor IOCTL dispatch and VALIDATE_NEGOTIATE_INFO, then enforce the ARCH-02 boundary.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Create new `internal/adapter/smb/smbenc/` package -- dedicated SMB binary codec
- Buffer-based pattern (Reader wraps []byte with position cursor), not streaming io.Reader
- Little-endian only (SMB is always LE) -- methods named ReadUint16(), WriteUint32() etc. with implicit LE
- Error accumulation pattern: Reader tracks first error, all subsequent reads become no-ops, caller checks reader.Err() once at end (like bufio.Scanner)
- Include validation helpers: ExpectUint16(), EnsureRemaining(n)
- Refactor ALL existing SMB handlers (negotiate, session_setup, etc.) to use the codec -- not just new SMB3 code
- Update existing handler tests to use codec for building test payloads
- Parse/encode three essential contexts: PREAUTH_INTEGRITY_CAPABILITIES, ENCRYPTION_CAPABILITIES, NETNAME_NEGOTIATE_CONTEXT_ID
- Defer COMPRESSION_CAPABILITIES, RDMA_TRANSFORM_CAPABILITIES, TRANSPORT_CAPABILITIES, SIGNING_CAPABILITIES to later phases
- Unrecognized contexts from client: log at DEBUG level with context type ID, then skip (no error, no response context)
- Context types and structs live in existing `internal/adapter/smb/types/` package (new negotiate_context.go file)
- Store negotiated cipher ID (CipherId) on connection during negotiate -- Phase 35 reads it
- Store negotiated signing algorithm ID (SigningAlgorithmId) on connection -- Phase 34 reads it
- Error context framework (SMB 3.1.1 error data in responses) built into smbenc codec
- SHA-512 hash chain stored in ConnectionCryptoState struct on Connection
- Extensible hash algorithm selection with SHA-512 as default (interface-based, future-proof)
- ConnectionCryptoState created eagerly for ALL connections, dialect-aware (2.x get zeroed hash)
- Hash computation via generic dispatch pre/post hooks -- not in negotiate handler
- Generic hook mechanism: BeforeHandler/AfterHandler hook slices per command, operating on raw wire bytes
- Phase 33: register preauth hash hooks for NEGOTIATE command only
- Phase 34 extends to SESSION_SETUP
- Same hooks reusable for signing and encryption middleware
- Connection-level hash only in Phase 33 -- per-session fork deferred to Phase 34
- Test with both MS-SMB2 spec test vectors AND synthetic tests (real Windows captured replays deferred to Phase 40 conformance)
- ConnectionCryptoState lives in `pkg/adapter/smb/` (same package as Connection)
- Mostly immutable after negotiate: dialect, cipher, signing algo, server GUID are set-once
- Preauth hash field mutable (updated during SESSION_SETUP in Phase 34) -- sync.RWMutex for hash field only
- Passed to handlers via ConnInfo struct (added CryptoState field)
- Dialect -> Capabilities map: defines max capabilities per dialect level
- Min/max dialect range configurable in SMB adapter config (like NFS min_version/max_version)
- String format: "2.0.2", "2.1", "3.0", "3.0.2", "3.1.1"
- Default: min_dialect: "2.0.2", max_dialect: "3.1.1" (accept everything)
- Client dialects outside configured range: return STATUS_NOT_SUPPORTED
- Capabilities admin-configurable via adapter config in control plane store (persisted)
- Individual flags: encryption_enabled, directory_leasing_enabled, etc.
- Exposed via `dfsctl adapter update smb --min-dialect 3.0 --max-dialect 3.1.1 --encryption-enabled` etc.
- REST API uses individual fields, not JSON blob
- Allow SMB 3.0/3.0.2 negotiation even without encryption (Phase 35) or SMB3 signing (Phase 34) -- progressive enhancement
- Existing IOCTL handler refactored to separate file (ioctl_validate_negotiate.go)
- Full validation of all 4 fields per spec: Capabilities, ServerGUID, SecurityMode, Dialect
- Negotiate handler stores original response parameters on ConnectionCryptoState -- VNEG reads from there (single source of truth)
- Accept unsigned requests for now (signing not upgraded until Phase 34)
- On mismatch (possible downgrade): log WARN with client IP + mismatch details, then drop TCP connection
- IOCTL dispatch refactored from switch statement to dispatch table (map[uint32]IOCTLHandler) -- same pattern as main command dispatch, package-level var populated in init()
- Keep existing 2-step flow: multi-protocol negotiate -> echo 0x02FF -> client sends SMB2 negotiate with 3.x dialects
- Negotiate contexts only included in SMB2 negotiate response, NOT in multi-protocol negotiate response
- ConnectionCryptoState created eagerly during connection accept (before negotiate)
- Cleaned up automatically on connection close (existing connection cleanup path)
- Core crypto state immutable after negotiate; preauth hash mutable with RWMutex
- Build error context framework into smbenc codec (ErrorContext type with Encode/Decode)
- Phase 33 builds the framework; specific error contexts added in later phases as needed

### Claude's Discretion
- FSCTL_VALIDATE_NEGOTIATE_INFO behavior for 3.1.1 clients (process anyway vs reject -- Claude decides based on spec + real client behavior)
- SMB internal package audit: pre-step vs interleaved with negotiate work (Claude decides based on refactor risk)
- Exact ConnectionCryptoState field layout and naming
- Hook registration mechanism details (slice-based vs map-based)
- Specific port numbers or error codes for edge cases

### Deferred Ideas (OUT OF SCOPE)
- COMPRESSION_CAPABILITIES negotiate context -- future phase (no compression support planned)
- RDMA_TRANSFORM_CAPABILITIES -- requires RDMA support, out of scope
- SIGNING_CAPABILITIES context parsing -- Phase 34 (signing upgrade)
- Per-session preauth hash fork -- Phase 34 (key derivation)
- Actual encryption implementation -- Phase 35
- Kerberos SMB3 key derivation -- Phase 36
- Lock sequence numbers (LockSequence field) -- Phase 38 (durable handles)
- Warmup mode for benchmarks -- future enhancement
- Artificial S3 latency simulation -- future enhancement
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| NEG-01 | Server negotiates SMB 3.0/3.0.2/3.1.1 dialects selecting highest mutually supported | Dialect selection logic with configurable min/max range; existing constants already defined in types/constants.go; MS-SMB2 section 3.3.5.4 processing rules documented below |
| NEG-02 | Server parses and responds with negotiate contexts (preauth integrity, encryption, signing capabilities) | Wire format for SMB2_NEGOTIATE_CONTEXT, PREAUTH_INTEGRITY_CAPABILITIES, ENCRYPTION_CAPABILITIES fully documented; signing capabilities deferred to Phase 34 per CONTEXT.md |
| NEG-03 | Server advertises CapDirectoryLeasing and CapEncryption capabilities for 3.0+ | Capability gating rules from MS-SMB2 section 3.3.5.4: CapDirectoryLeasing for 3.x family, CapEncryption for 3.0/3.0.2 (conditional on CipherId for 3.1.1); existing Capabilities type in types/constants.go |
| NEG-04 | Server computes SHA-512 preauth integrity hash chain over raw wire bytes on Connection and Session | Hash chain algorithm from MS-SMB2: H(i) = SHA-512(H(i-1) || message(i)), H(0) = zeros; Connection-level only in Phase 33; hook-based approach on raw bytes per CONTEXT.md |
| SDIAL-01 | Server handles FSCTL_VALIDATE_NEGOTIATE_INFO IOCTL for SMB 3.0/3.0.2 clients | Per MS-SMB2 section 3.3.5.15.12: for 3.1.1 connections, terminate connection; for 3.0/3.0.2 validate all 4 fields; existing handler already implements basic validation |
| ARCH-02 | SMB internal package contains only protocol encoding/decoding/framing -- no business logic | smbenc codec creation + handler refactoring enforces this boundary; audit existing handlers to move any business logic to metadata/runtime layer |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `crypto/sha512` | Go stdlib | SHA-512 hash for preauth integrity | Standard Go crypto, no external dependency needed |
| `crypto/rand` | Go stdlib | CSPRNG for preauth salt generation | Spec requires cryptographically random salt |
| `encoding/binary` | Go stdlib | Foundation for smbenc codec | Little-endian encoding; smbenc wraps this with error accumulation |
| `sync` | Go stdlib | RWMutex for ConnectionCryptoState hash | Protects mutable preauth hash field |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `bytes` | Go stdlib | Buffer operations in smbenc Reader/Writer | Core of the codec buffer pattern |
| `testing` | Go stdlib | Unit tests with spec test vectors | All codec and negotiate context tests |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Custom smbenc codec | `encoding/binary` directly | Current approach; error accumulation and validation helpers justify the codec |
| Interface-based hash algorithm | Hardcoded SHA-512 | Interface costs ~5 lines but enables future hash algorithms per spec extensibility |
| `sync.RWMutex` for hash | `sync.Mutex` | RWMutex allows concurrent reads of immutable fields while only blocking hash mutations |

**Installation:**
No new dependencies required. All work uses Go standard library.

## Architecture Patterns

### Recommended Project Structure

```
internal/adapter/smb/smbenc/           # NEW: Binary codec package
├── reader.go                          # Reader with error accumulation
├── reader_test.go
├── writer.go                          # Writer with error accumulation
├── writer_test.go
└── doc.go                             # Package documentation

internal/adapter/smb/types/
├── constants.go                       # EXISTING: Add negotiate context type IDs
└── negotiate_context.go               # NEW: Context types and encode/decode

pkg/adapter/smb/
├── connection.go                      # MODIFY: Add CryptoState field
├── crypto_state.go                    # NEW: ConnectionCryptoState struct
└── config.go                          # MODIFY: Add capability config fields

internal/adapter/smb/
├── dispatch.go                        # MODIFY: Add BeforeHandler/AfterHandler hooks
├── conn_types.go                      # MODIFY: Add CryptoState to ConnInfo
├── hooks.go                           # NEW: Hook registration + preauth hash hook
└── v2/handlers/
    ├── negotiate.go                   # MAJOR REFACTOR: 3.x dialect + contexts
    ├── negotiate_test.go              # MAJOR REFACTOR: new test vectors
    ├── ioctl_dispatch.go              # NEW: IOCTL dispatch table
    ├── ioctl_validate_negotiate.go    # NEW: Extracted VNEG handler
    └── stub_handlers.go              # MODIFY: Extract IOCTL + VNEG
```

### Pattern 1: smbenc Error-Accumulating Reader
**What:** A buffer-based reader that tracks the first error and makes subsequent reads no-ops.
**When to use:** All SMB binary decoding throughout the codebase.
**Example:**
```go
// Source: Design decision from CONTEXT.md, modeled after bufio.Scanner
type Reader struct {
    buf []byte
    pos int
    err error
}

func NewReader(data []byte) *Reader {
    return &Reader{buf: data}
}

func (r *Reader) ReadUint16() uint16 {
    if r.err != nil { return 0 }
    if r.pos+2 > len(r.buf) {
        r.err = ErrShortRead
        return 0
    }
    v := binary.LittleEndian.Uint16(r.buf[r.pos:])
    r.pos += 2
    return v
}

func (r *Reader) ReadUint32() uint32 { /* similar */ }
func (r *Reader) ReadBytes(n int) []byte { /* similar */ }
func (r *Reader) Skip(n int) { /* advance position */ }
func (r *Reader) ExpectUint16(expected uint16) { /* read + validate */ }
func (r *Reader) EnsureRemaining(n int) { /* check without consuming */ }
func (r *Reader) Err() error { return r.err }
func (r *Reader) Remaining() int { return len(r.buf) - r.pos }
```

### Pattern 2: smbenc Error-Accumulating Writer
**What:** A buffer-based writer that tracks errors and builds wire-format bytes.
**When to use:** All SMB binary encoding throughout the codebase.
**Example:**
```go
type Writer struct {
    buf []byte
    err error
}

func NewWriter(capacity int) *Writer {
    return &Writer{buf: make([]byte, 0, capacity)}
}

func (w *Writer) WriteUint16(v uint16) { /* append LE bytes */ }
func (w *Writer) WriteUint32(v uint32) { /* append LE bytes */ }
func (w *Writer) WriteUint64(v uint64) { /* append LE bytes */ }
func (w *Writer) WriteBytes(data []byte) { /* append raw */ }
func (w *Writer) WriteZeros(n int) { /* append n zero bytes */ }
func (w *Writer) Pad(alignment int) { /* pad to alignment boundary */ }
func (w *Writer) Bytes() []byte { return w.buf }
func (w *Writer) Err() error { return w.err }
```

### Pattern 3: Negotiate Context Parsing
**What:** Parse the NegotiateContextList from SMB 3.1.1 NEGOTIATE requests.
**When to use:** When the selected dialect is 3.1.1 and the request includes negotiate contexts.
**Example:**
```go
// Source: MS-SMB2 section 2.2.3.1
// Contexts are 8-byte aligned in the list
func ParseNegotiateContextList(data []byte, count uint16) ([]NegotiateContext, error) {
    r := smbenc.NewReader(data)
    var contexts []NegotiateContext
    for i := uint16(0); i < count; i++ {
        contextType := r.ReadUint16()
        dataLength := r.ReadUint16()
        r.Skip(4) // Reserved
        contextData := r.ReadBytes(int(dataLength))
        if r.Err() != nil { return nil, r.Err() }
        contexts = append(contexts, NegotiateContext{
            ContextType: contextType,
            Data:        contextData,
        })
        // Pad to 8-byte alignment for next context (except last)
        if i < count-1 {
            padding := (8 - (r.Position() % 8)) % 8
            r.Skip(int(padding))
        }
    }
    return contexts, r.Err()
}
```

### Pattern 4: Preauth Integrity Hash Chain
**What:** Compute SHA-512 hash chain over raw wire bytes of NEGOTIATE request/response.
**When to use:** Before and after dispatch of NEGOTIATE (and later SESSION_SETUP in Phase 34).
**Example:**
```go
// Source: MS-SMB2 section 3.3.5.4
// H(i) = SHA-512(H(i-1) || message(i))
// H(0) = 64 bytes of zeros
func (cs *ConnectionCryptoState) UpdatePreauthHash(message []byte) {
    cs.hashMu.Lock()
    defer cs.hashMu.Unlock()
    h := sha512.New()
    h.Write(cs.preauthHash[:])
    h.Write(message)
    copy(cs.preauthHash[:], h.Sum(nil))
}
```

### Pattern 5: Dispatch Hook Mechanism
**What:** BeforeHandler/AfterHandler hooks on the dispatch path, operating on raw wire bytes.
**When to use:** For preauth hash computation, and later signing/encryption middleware.
**Example:**
```go
// Hook type operating on raw message bytes
type DispatchHook func(connInfo *ConnInfo, command types.Command, rawMessage []byte)

// Hooks registered per command (slice-based for ordered execution)
var BeforeHooks map[types.Command][]DispatchHook
var AfterHooks  map[types.Command][]DispatchHook

func init() {
    BeforeHooks = make(map[types.Command][]DispatchHook)
    AfterHooks = make(map[types.Command][]DispatchHook)
}

// RegisterBeforeHook adds a hook executed before handler dispatch
func RegisterBeforeHook(cmd types.Command, hook DispatchHook) {
    BeforeHooks[cmd] = append(BeforeHooks[cmd], hook)
}
```

### Pattern 6: IOCTL Dispatch Table
**What:** Replace switch statement with map-based dispatch for IOCTL/FSCTL codes.
**When to use:** In the Ioctl handler method.
**Example:**
```go
// IOCTLHandler is the signature for individual IOCTL handlers
type IOCTLHandler func(h *Handler, ctx *SMBHandlerContext, body []byte) (*HandlerResult, error)

// ioctlDispatch maps FSCTL codes to handlers
var ioctlDispatch map[uint32]IOCTLHandler

func init() {
    ioctlDispatch = map[uint32]IOCTLHandler{
        FsctlValidateNegotiateInfo:  handleValidateNegotiateInfo,
        FsctlGetReparsePoint:        handleGetReparsePoint,
        FsctlSrvEnumerateSnapshots:  handleEnumerateSnapshots,
        FsctlPipeTransceive:         handlePipeTransceive,
        // ... etc
    }
}
```

### Anti-Patterns to Avoid
- **Duplicating negotiate logic in VALIDATE_NEGOTIATE_INFO:** The current code has parallel dialect selection in both negotiate and VNEG. Store original negotiate response values on ConnectionCryptoState; VNEG reads from there.
- **Raw binary.LittleEndian calls in handlers:** After smbenc exists, all handlers must use the codec. Manual binary encoding is a maintenance and correctness risk.
- **Business logic in handler files:** Per ARCH-02, handlers should only do wire encoding/decoding and delegate to runtime/metadata. Any permission checks, share resolution, or state management belongs elsewhere.
- **Computing hash inside the negotiate handler:** The hash must be computed over the raw wire bytes (including SMB2 header), which the handler does not have access to. Use the pre/post dispatch hooks.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SHA-512 hash | Custom implementation | `crypto/sha512` (stdlib) | Standard, audited, fast |
| Cryptographic salt | Math/rand or sequential | `crypto/rand` | MS-SMB2 requires CSPRNG |
| Binary encoding | Ad-hoc binary.LittleEndian | smbenc codec | Error accumulation, validation, consistency |
| Negotiate context alignment | Manual padding math | smbenc Writer.Pad(8) | 8-byte alignment is easy to get wrong |

**Key insight:** The smbenc codec is the single most important new component. Every subsequent task depends on it. The error-accumulation pattern eliminates an entire class of bugs (unchecked bounds, partial reads) that are common in binary protocol parsing.

## Common Pitfalls

### Pitfall 1: Negotiate Context 8-Byte Alignment
**What goes wrong:** Negotiate contexts in the NegotiateContextList must be 8-byte aligned. The DataLength field does NOT include padding. Forgetting to pad between contexts or padding the last context causes parse failures.
**Why it happens:** The MS-SMB2 spec mentions alignment in the list description but not in each context's format description. Easy to miss.
**How to avoid:** The smbenc Writer.Pad(8) method handles this. When encoding the NegotiateContextList, pad after each context except the last. When parsing, skip padding between contexts.
**Warning signs:** Windows clients fail to connect with 3.1.1 or fall back to 2.x.

### Pitfall 2: Preauth Hash Computed Over Raw Wire Bytes Including SMB2 Header
**What goes wrong:** Computing the hash over only the body (handler's view) or the decoded request (after parsing) gives the wrong hash value. The spec requires hashing over the complete message from the first byte of the SMB2 header to the last byte received from the network.
**Why it happens:** Handler functions receive only the body after the header. The raw bytes must be captured at the framing/dispatch layer, before any parsing.
**How to avoid:** Use the dispatch hook mechanism that operates on the full message bytes at the framing layer. The hook sees: `rawMessage = header(64 bytes) + body`.
**Warning signs:** Session key derivation fails in Phase 34 because hash doesn't match.

### Pitfall 3: FSCTL_VALIDATE_NEGOTIATE_INFO Must Drop Connection for 3.1.1
**What goes wrong:** Returning STATUS_NOT_SUPPORTED or an error response instead of terminating the TCP connection when a 3.1.1 client sends VALIDATE_NEGOTIATE_INFO.
**Why it happens:** The spec says "terminate the transport connection" which is unusual -- most errors return a status code. SMB 3.1.1 replaces VNEG with preauth integrity, so VNEG is unnecessary and suspicious when received on a 3.1.1 connection.
**How to avoid:** Check Connection.Dialect first. If "3.1.1", close the TCP connection immediately without sending a response. For 3.0/3.0.2, proceed with full validation.
**Warning signs:** Windows 10/11 clients disconnect after tree connect if VNEG isn't handled correctly.

### Pitfall 4: Capability Gating by Dialect Level
**What goes wrong:** Advertising capabilities in the NEGOTIATE response that are not valid for the negotiated dialect. For example, CapEncryption should only be advertised for 3.0/3.0.2 when AES-128-CCM is supported (for 3.1.1, the encryption capability comes from negotiate contexts, not the Capabilities field).
**Why it happens:** MS-SMB2 section 3.3.5.4 has different rules for each dialect level. CapEncryption in the Capabilities field is only for 3.0/3.0.2.
**How to avoid:** Build a dialect -> max capabilities map and mask the response capabilities against it. For 3.1.1, set CapEncryption only if Connection.CipherId is nonzero (after processing negotiate contexts).
**Warning signs:** Client logs show "encryption not supported" even though the server advertised it.

### Pitfall 5: Multi-Protocol Negotiate Must NOT Include Contexts
**What goes wrong:** Including negotiate contexts in the SMB1-to-SMB2 upgrade response. The multi-protocol negotiate (SMB1 NEGOTIATE -> SMB2 response with 0x02FF) must be a plain SMB2 NEGOTIATE response without any negotiate contexts.
**Why it happens:** The SMB2 NEGOTIATE response format has NegotiateContextCount/Offset fields but they must be zero for the upgrade response.
**How to avoid:** Keep the existing HandleSMB1Negotiate function as-is. Only add negotiate contexts in the SMB2 NEGOTIATE handler when dialect is 3.1.1.
**Warning signs:** macOS clients fail to connect because they start with multi-protocol negotiate.

### Pitfall 6: ConnectionCryptoState Must Exist Before First Negotiate
**What goes wrong:** Null pointer or missing state when the negotiate hook tries to update the preauth hash.
**Why it happens:** If CryptoState is created lazily (e.g., only when 3.1.1 is selected), the "before negotiate" hook has no state to update.
**How to avoid:** Per CONTEXT.md decision: create ConnectionCryptoState eagerly during connection accept, before any messages are processed. For 2.x connections, the hash stays zeroed and is never used.
**Warning signs:** Panic on first connection.

## Code Examples

### Negotiate Context Wire Format Constants
```go
// Source: MS-SMB2 section 2.2.3.1
const (
    NegCtxPreauthIntegrity   uint16 = 0x0001 // SMB2_PREAUTH_INTEGRITY_CAPABILITIES
    NegCtxEncryptionCaps     uint16 = 0x0002 // SMB2_ENCRYPTION_CAPABILITIES
    NegCtxCompressionCaps    uint16 = 0x0003 // SMB2_COMPRESSION_CAPABILITIES
    NegCtxNetnameContextID   uint16 = 0x0005 // SMB2_NETNAME_NEGOTIATE_CONTEXT_ID
    NegCtxTransportCaps      uint16 = 0x0006 // SMB2_TRANSPORT_CAPABILITIES
    NegCtxRDMATransformCaps  uint16 = 0x0007 // SMB2_RDMA_TRANSFORM_CAPABILITIES
    NegCtxSigningCaps        uint16 = 0x0008 // SMB2_SIGNING_CAPABILITIES
)

// Hash algorithm IDs (PREAUTH_INTEGRITY_CAPABILITIES)
const (
    HashAlgSHA512 uint16 = 0x0001
)

// Cipher IDs (ENCRYPTION_CAPABILITIES)
const (
    CipherAES128CCM uint16 = 0x0001
    CipherAES128GCM uint16 = 0x0002
    CipherAES256CCM uint16 = 0x0003
    CipherAES256GCM uint16 = 0x0004
)
```

### PREAUTH_INTEGRITY_CAPABILITIES Response Encoding
```go
// Source: MS-SMB2 section 2.2.4.1.1
// In response: HashAlgorithmCount MUST be 1
func EncodePreauthIntegrityResponse(hashAlg uint16, salt []byte) []byte {
    w := smbenc.NewWriter(64)
    // Context header
    w.WriteUint16(NegCtxPreauthIntegrity) // ContextType
    dataLen := 4 + 2 + len(salt)          // HashAlgCount(2) + SaltLen(2) + HashAlgs(2) + Salt
    w.WriteUint16(uint16(dataLen))         // DataLength
    w.WriteUint32(0)                       // Reserved
    // Context data
    w.WriteUint16(1)                       // HashAlgorithmCount (MUST be 1 in response)
    w.WriteUint16(uint16(len(salt)))       // SaltLength
    w.WriteUint16(hashAlg)                 // HashAlgorithms[0]
    w.WriteBytes(salt)                     // Salt
    return w.Bytes()
}
```

### ConnectionCryptoState Structure
```go
// Source: Design from CONTEXT.md, modeling MS-SMB2 Connection object
type ConnectionCryptoState struct {
    // Immutable after negotiate (no lock needed for reads)
    Dialect          types.Dialect // Negotiated dialect (0x0300, 0x0302, 0x0311)
    CipherId         uint16        // Negotiated cipher (0 = none, set by Phase 35)
    SigningAlgorithmId uint16      // Negotiated signing algo (set by Phase 34)
    ServerGUID       [16]byte      // Copy of server GUID for VNEG validation
    ServerCapabilities uint32      // Capabilities sent in NEGOTIATE response
    ServerSecurityMode uint16      // SecurityMode sent in NEGOTIATE response

    // Store original client values for VALIDATE_NEGOTIATE_INFO
    ClientCapabilities uint32
    ClientGUID         [16]byte
    ClientSecurityMode uint16
    ClientDialects     []uint16    // Required for 3.1.1 VNEG validation

    // Mutable: preauth integrity hash (protected by hashMu)
    hashMu      sync.RWMutex
    preauthHash [64]byte // SHA-512 produces 64 bytes

    // Hash algorithm (interface for extensibility)
    PreauthIntegrityHashId uint16
}
```

### VALIDATE_NEGOTIATE_INFO for 3.1.1 (Drop Connection)
```go
// Source: MS-SMB2 section 3.3.5.15.12
// "If Connection.Dialect is '3.1.1', the server MUST terminate
//  the transport connection and free the Connection object."
func handleValidateNegotiateInfo(h *Handler, ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
    // Check dialect from CryptoState
    if ctx.CryptoState != nil && ctx.CryptoState.Dialect == types.Dialect0311 {
        logger.Warn("VALIDATE_NEGOTIATE_INFO received on 3.1.1 connection, dropping",
            "client", ctx.ClientAddr)
        // Return special result that tells dispatch to close the connection
        return &HandlerResult{Status: types.StatusAccessDenied, DropConnection: true}, nil
    }
    // ... proceed with 3.0/3.0.2 validation
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| VALIDATE_NEGOTIATE_INFO for anti-downgrade | Preauth integrity hash chain (3.1.1) | Windows 10 / SMB 3.1.1 (2015) | VNEG only for 3.0/3.0.2; 3.1.1 uses hash chain instead |
| CapEncryption in Capabilities field | ENCRYPTION_CAPABILITIES negotiate context | SMB 3.1.1 (2015) | Encryption negotiation moved from global caps to negotiate contexts for 3.1.1 |
| HMAC-SHA256 signing only | AES-CMAC/GMAC via SIGNING_CAPABILITIES context | SMB 3.1.1 (2015) | Signing algo negotiation (Phase 34, not Phase 33) |
| Single cipher (AES-128-CCM) | Cipher negotiation via contexts | SMB 3.1.1 (2015) | Supports AES-128-GCM, AES-256-CCM, AES-256-GCM |

**Deprecated/outdated:**
- VALIDATE_NEGOTIATE_INFO for SMB 3.1.1: Replaced by preauth integrity. Server MUST drop connection if received on 3.1.1.

## Open Questions

1. **go-smb2 client library SMB 3.1.1 support status**
   - What we know: hirochachacha/go-smb2 supports SMB2/3 but it's unclear if it sends 3.1.1 negotiate contexts correctly
   - What's unclear: Whether the library negotiates 3.1.1 or falls back to 3.0.2
   - Recommendation: Test empirically during integration tests. If go-smb2 doesn't negotiate 3.1.1, use it for 3.0/3.0.2 tests and rely on WPTS for 3.1.1 conformance testing.

2. **DropConnection mechanism in HandlerResult**
   - What we know: The spec requires terminating the TCP connection for VNEG on 3.1.1. The current HandlerResult has Status and Data but no connection-close signal.
   - What's unclear: Best way to signal "drop connection" from handler to dispatch layer
   - Recommendation: Add a `DropConnection bool` field to HandlerResult. The dispatch layer checks this flag after handler returns and closes the TCP connection instead of sending a response.

3. **SMB adapter settings: dialect format consistency**
   - What we know: Existing SMBAdapterSettings uses "SMB2.0", "SMB2.1", "SMB3.0", "SMB3.1.1" format. CONTEXT.md says "2.0.2", "2.1", "3.0", "3.0.2", "3.1.1".
   - What's unclear: Whether to change the existing DB format or add a mapping layer
   - Recommendation: Keep existing DB format ("SMB2.0" etc.) and add a parser that maps to types.Dialect values. This avoids a DB migration.

## Sources

### Primary (HIGH confidence)
- [MS-SMB2 section 2.2.3.1 - SMB2_NEGOTIATE_CONTEXT](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/15332256-522e-4a53-8cd7-0bd17678a2f7) - Context wire format with ContextType IDs
- [MS-SMB2 section 2.2.3.1.1 - SMB2_PREAUTH_INTEGRITY_CAPABILITIES (Request)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a07bd66-4734-4af8-abcf-5a44ff7ee0e5) - Hash algorithm list format
- [MS-SMB2 section 2.2.4.1.1 - SMB2_PREAUTH_INTEGRITY_CAPABILITIES (Response)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/40e1607f-edae-4e0a-b3ec-e9b4713b2a0f) - Response format (HashAlgorithmCount=1)
- [MS-SMB2 section 2.2.3.1.2 - SMB2_ENCRYPTION_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/16693be7-2b27-4d3b-804b-f605bde5bcdd) - Cipher IDs (0x0001-0x0004)
- [MS-SMB2 section 3.3.5.4 - Receiving an SMB2 NEGOTIATE Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/b39f253e-4963-40df-8dff-2f9040ebbeb1) - Complete server processing rules
- [MS-SMB2 section 3.3.5.15.12 - Handling VALIDATE_NEGOTIATE_INFO](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/0b7803eb-d561-48a4-8654-327803f59ec6) - VNEG validation rules including 3.1.1 termination
- Existing DittoFS codebase - negotiate.go, stub_handlers.go, types/constants.go, connection.go, config.go, dispatch.go

### Secondary (MEDIUM confidence)
- [SMB 3.1.1 Pre-authentication integrity in Windows 10 (Microsoft Blog)](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) - Hash chain formula and motivation
- [SMB 3.1.1 SNIA Presentation](https://www.snia.org/sites/default/files/SDC15_presentations/smb/GregKramer_%20SMB_3-1-1_rev.pdf) - Overview of 3.1.1 features

### Tertiary (LOW confidence)
- [go-smb2 library](https://github.com/hirochachacha/go-smb2) - Unclear 3.1.1 negotiation support; needs validation

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All Go stdlib, no external dependencies, MS-SMB2 spec fully documents wire formats
- Architecture: HIGH - Codebase patterns well-established (dispatch table, conn_types, handler struct); CONTEXT.md provides detailed locked decisions
- Pitfalls: HIGH - Verified against MS-SMB2 official specification; alignment, VNEG behavior, and hash chain requirements are clearly specified
- Wire format: HIGH - All field sizes, offsets, and values extracted directly from MS-SMB2 official specification pages

**Research date:** 2026-02-28
**Valid until:** 2026-03-30 (MS-SMB2 spec is stable; no breaking changes expected)
