# Phase 16: NFSv4.1 Types and Constants - Research

**Researched:** 2026-02-20
**Domain:** NFSv4.1 protocol wire types, XDR structures, operation/error constants (RFC 8881)
**Confidence:** HIGH

## Summary

Phase 16 defines all NFSv4.1 wire types, operation numbers, error codes, and XDR structures needed by subsequent phases (17-25). This is a foundational "types-only" phase with no handler implementations -- only data definitions, XDR encode/decode methods, a stub dispatch table, and COMPOUND minor version routing.

The existing codebase has a well-established pattern in `internal/protocol/nfs/v4/types/` with `constants.go`, `errors.go`, and `types.go` for NFSv4.0. The user has decided to extend these files (not create a separate v41/ package), with per-operation files for v4.1-specific types. The XDR codec style is hand-written struct methods matching the existing v4.0 pattern, not code-generated.

The total scope covers 19 new operations (40-58), 10 new callback operations (5-14), ~40 new error codes (10049-10087), and approximately 25-30 new XDR struct/union types. The COMPOUND dispatcher needs a single branch point for minorversion=1 routing to a new stub dispatch table.

**Primary recommendation:** Follow the existing v4.0 patterns exactly. Add constants/errors to existing files with separator comments, create per-operation type files with self-contained struct + Encode/Decode methods, and add a thin dispatch table branch in compound.go.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Add v4.1 types to the **existing `internal/protocol/nfs/v4/types/` package** -- no separate v41/ package
- **Per-operation files** for types: `exchange_id.go`, `create_session.go`, `sequence.go`, `destroy_session.go`, etc. Each file contains request + response XDR structs and their encode/decode methods
- **Shared sub-types** (ChannelAttrs, StateProtect, ServerOwner, NfsImplId, etc.) go in `session_common.go`
- **Constants** (op numbers, error codes) added to the **existing `constants.go` and `errors.go`** files, separated by a `// --- NFSv4.1 ... ---` comment block
- v4.1 handlers (in later phases) go in the **existing `v4/handlers/` tree** -- no separate v41/handlers/ directory
- Update `internal/protocol/CLAUDE.md` with a section on v4.0/v4.1 coexistence conventions
- **Hand-written** encode/decode (same as v4.0), not code-generated
- **Struct methods**: `func (a *ExchangeIdArgs) Encode(w) error` / `func (a *ExchangeIdArgs) Decode(r) error`
- Types and encode/decode **in the same file** (e.g., `exchange_id.go` has both struct definitions and codec methods)
- **Add union helpers** to the `internal/protocol/xdr/` package for discriminated unions
- Each type gets **per-type unit tests** with encode/decode round-trip verification
- **No prefix/suffix** for v4.1-specific types: `SessionId4`, `ChannelAttrs`, `ExchangeIdArgs`, `CreateSessionRes`
- Operation constants follow **same OP_ pattern**: `OP_EXCHANGE_ID`, `OP_CREATE_SESSION`, `OP_SEQUENCE`
- Callback constants follow **same CB_ pattern**: `CB_LAYOUTRECALL`, `CB_NOTIFY`, `CB_SEQUENCE`
- Error codes follow **same NFS4ERR_ pattern**: `NFS4ERR_BACK_CHAN_BUSY`, `NFS4ERR_CONN_NOT_BOUND_TO_SESSION`
- Flag/bitmask constants use **RFC names directly**: `EXCHGID4_FLAG_USE_NON_PNFS`, `SP4_NONE`, `CREATE_SESSION4_FLAG_PERSIST`
- **All 19 v4.1 ops + 10 CB ops** get full XDR types upfront (not phased/deferred)
- **Full pNFS types** included (LAYOUTCOMMIT, LAYOUTGET, LAYOUTRETURN, GETDEVICEINFO, GETDEVICELIST)
- **Define v4.1 dispatch table** with placeholder handlers (each returns NFS4ERR_NOTSUPP)
- **Add v4.1 detection** to COMPOUND dispatcher: minorversion=1 routes to v4.1 dispatch table
- **Add NFS4_MINOR_VERSION_1** constant and **update version negotiation** to accept minorversion 0 and 1
- **Per-operation test files**: `exchange_id_test.go`, `create_session_test.go`, etc.
- **Test fixtures file** (`testutil_test.go` or `fixtures_test.go`) with helpers for common test data
- Types implement **`fmt.Stringer`** for debug/log readability
- Types implement **`XdrEncoder`/`XdrDecoder` interfaces** -- enables generic codec helpers
- **New v4.1 handler signature** (distinct from v4.0): includes session context
- Session context passed via **`V41RequestContext` struct** (bundled session, slot, sequence info)

### Claude's Discretion
- XDR union helper abstraction level (minimal functions vs interface pattern)
- Whether to validate field constraints in codecs or at handler level
- Golden tests vs round-trip-only (based on RFC byte examples availability)
- v4.0 type compatibility audit (whether any existing types need modification)
- Regression test strategy (existing tests sufficient vs explicit regression test)
- XdrEncoder/XdrDecoder interface method signatures (based on existing xdr package)

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SESS-05 | NFSv4.1 constants, types, and XDR structures defined for all new operations (ops 40-58, CB ops 5-14) | Complete list of 19 operations, 10 CB operations, ~40 error codes, and all XDR struct definitions extracted from RFC 8881 and Linux kernel reference. Per-operation file organization, shared types in session_common.go, dispatch table with NOTSUPP stubs. |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go standard library | 1.21+ | `bytes.Buffer`, `io.Reader`, `encoding/binary`, `fmt.Stringer` | Existing XDR codec pattern uses these exclusively |
| `internal/protocol/xdr` | N/A | XDR encode/decode primitives | Project's existing XDR package -- `DecodeUint32`, `WriteUint32`, `DecodeOpaque`, `WriteXDROpaque`, etc. |
| `internal/protocol/nfs/v4/types` | N/A | NFSv4 type definitions package | Target package for all v4.1 additions per locked decision |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `testing` | stdlib | Unit test framework | Per-operation round-trip tests |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Hand-written XDR | buildbarn/go-xdr code generator | User locked decision: hand-written. Code gen adds build dependency and reduces readability. Existing v4.0 is hand-written. |

## Architecture Patterns

### Recommended File Structure
```
internal/protocol/nfs/v4/types/
    constants.go              # EXISTING -- add v4.1 op numbers, flags, session constants
    errors.go                 # EXISTING -- add v4.1 error code mapping
    types.go                  # EXISTING -- add NFS4_MINOR_VERSION_1, V41RequestContext
    session_common.go         # NEW -- ChannelAttrs, StateProtect, ServerOwner, NfsImplId, SessionId4
    exchange_id.go            # NEW -- ExchangeIdArgs, ExchangeIdRes + Encode/Decode
    exchange_id_test.go       # NEW -- round-trip tests
    create_session.go         # NEW -- CreateSessionArgs, CreateSessionRes + Encode/Decode
    create_session_test.go    # NEW
    destroy_session.go        # NEW
    destroy_session_test.go   # NEW
    sequence.go               # NEW
    sequence_test.go          # NEW
    bind_conn_to_session.go   # NEW
    bind_conn_to_session_test.go # NEW
    backchannel_ctl.go        # NEW
    backchannel_ctl_test.go   # NEW
    free_stateid.go           # NEW
    free_stateid_test.go      # NEW
    test_stateid.go           # NEW
    test_stateid_test.go      # NEW
    destroy_clientid.go       # NEW
    destroy_clientid_test.go  # NEW
    reclaim_complete.go       # NEW
    reclaim_complete_test.go  # NEW
    secinfo_no_name.go        # NEW
    secinfo_no_name_test.go   # NEW
    set_ssv.go                # NEW
    set_ssv_test.go           # NEW
    want_delegation.go        # NEW
    want_delegation_test.go   # NEW
    get_dir_delegation.go     # NEW
    get_dir_delegation_test.go # NEW
    layoutget.go              # NEW
    layoutget_test.go         # NEW
    layoutcommit.go           # NEW
    layoutcommit_test.go      # NEW
    layoutreturn.go           # NEW
    layoutreturn_test.go      # NEW
    getdeviceinfo.go          # NEW
    getdeviceinfo_test.go     # NEW
    getdevicelist.go          # NEW
    getdevicelist_test.go     # NEW
    cb_sequence.go            # NEW -- CB_SEQUENCE args/res
    cb_sequence_test.go       # NEW
    cb_layoutrecall.go        # NEW
    cb_layoutrecall_test.go   # NEW
    cb_notify.go              # NEW
    cb_notify_test.go         # NEW
    cb_push_deleg.go          # NEW
    cb_push_deleg_test.go     # NEW
    cb_recall_any.go          # NEW
    cb_recall_any_test.go     # NEW
    cb_recall_slot.go         # NEW
    cb_recall_slot_test.go    # NEW
    cb_wants_cancelled.go     # NEW
    cb_wants_cancelled_test.go # NEW
    cb_notify_lock.go         # NEW
    cb_notify_lock_test.go    # NEW
    cb_notify_deviceid.go     # NEW
    cb_notify_deviceid_test.go # NEW
    session_common_test.go    # NEW -- shared type tests
    fixtures_test.go          # NEW -- test helpers (valid SessionId, ChannelAttrs defaults)

internal/protocol/xdr/
    union.go                  # NEW -- discriminated union encode/decode helpers

internal/protocol/nfs/v4/handlers/
    compound.go               # MODIFY -- add minorversion=1 branch
    handler.go                # MODIFY -- add v4.1 dispatch table
```

### Pattern 1: Per-Operation Type File (Locked Decision)
**What:** Each v4.1 operation gets its own file with args/res structs and their Encode/Decode methods.
**When to use:** Every new v4.1 operation.
**Example:**
```go
// exchange_id.go
package types

import (
    "bytes"
    "fmt"
    "io"

    "github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ExchangeIdArgs represents EXCHANGE_ID4args per RFC 8881 Section 18.35.
type ExchangeIdArgs struct {
    ClientOwner  ClientOwner4
    Flags        uint32
    StateProtect StateProtect4A
    ClientImplId []NfsImplId4 // optional, max 1
}

func (a *ExchangeIdArgs) Encode(buf *bytes.Buffer) error {
    if err := a.ClientOwner.Encode(buf); err != nil {
        return fmt.Errorf("encode client_owner: %w", err)
    }
    if err := xdr.WriteUint32(buf, a.Flags); err != nil {
        return fmt.Errorf("encode flags: %w", err)
    }
    if err := a.StateProtect.Encode(buf); err != nil {
        return fmt.Errorf("encode state_protect: %w", err)
    }
    // Optional array (max 1)
    if err := xdr.WriteUint32(buf, uint32(len(a.ClientImplId))); err != nil {
        return fmt.Errorf("encode impl_id count: %w", err)
    }
    for i := range a.ClientImplId {
        if err := a.ClientImplId[i].Encode(buf); err != nil {
            return fmt.Errorf("encode impl_id[%d]: %w", i, err)
        }
    }
    return nil
}

func (a *ExchangeIdArgs) Decode(r io.Reader) error {
    if err := a.ClientOwner.Decode(r); err != nil {
        return fmt.Errorf("decode client_owner: %w", err)
    }
    flags, err := xdr.DecodeUint32(r)
    if err != nil {
        return fmt.Errorf("decode flags: %w", err)
    }
    a.Flags = flags
    if err := a.StateProtect.Decode(r); err != nil {
        return fmt.Errorf("decode state_protect: %w", err)
    }
    count, err := xdr.DecodeUint32(r)
    if err != nil {
        return fmt.Errorf("decode impl_id count: %w", err)
    }
    if count > 1 {
        return fmt.Errorf("impl_id count %d exceeds max 1", count)
    }
    a.ClientImplId = make([]NfsImplId4, count)
    for i := uint32(0); i < count; i++ {
        if err := a.ClientImplId[i].Decode(r); err != nil {
            return fmt.Errorf("decode impl_id[%d]: %w", i, err)
        }
    }
    return nil
}

func (a *ExchangeIdArgs) String() string {
    return fmt.Sprintf("EXCHANGE_ID{flags=0x%08x, protect=%s}", a.Flags, a.StateProtect.How)
}
```

### Pattern 2: Shared Sub-Types in session_common.go
**What:** Types used by 2+ operations live in `session_common.go`.
**When to use:** SessionId4, ChannelAttrs, StateProtect4A/4R, ServerOwner4, NfsImplId4, ClientOwner4, CallbackSecParms4.
**Example:**
```go
// session_common.go
package types

// NFS4_SESSIONID_SIZE is the size of a session identifier (16 bytes).
// Per RFC 8881 Section 2.10.3.
const NFS4_SESSIONID_SIZE = 16

// SessionId4 is an NFSv4.1 session identifier (opaque, 16 bytes).
type SessionId4 [NFS4_SESSIONID_SIZE]byte

// ChannelAttrs represents channel_attrs4 per RFC 8881 Section 18.36.
type ChannelAttrs struct {
    HeaderPadSize        uint32
    MaxRequestSize       uint32
    MaxResponseSize      uint32
    MaxResponseSizeCached uint32
    MaxOperations        uint32
    MaxRequests          uint32
    RdmaIrd              []uint32 // optional, max 1
}
```

### Pattern 3: Discriminated Union Encode/Decode
**What:** XDR unions are common in v4.1 (StateProtect, BindConnToSession res, layout types). A helper pattern reduces boilerplate.
**When to use:** Any XDR union type (switch on discriminant).
**Recommendation (Claude's discretion):** Use minimal helper functions rather than a full interface pattern. The number of unions (~8-10) doesn't justify interface overhead.
```go
// internal/protocol/xdr/union.go
package xdr

import (
    "fmt"
    "io"
)

// DecodeUnionDiscriminant reads the uint32 discriminant of an XDR union.
// This is just an alias for DecodeUint32 but makes union decode code self-documenting.
func DecodeUnionDiscriminant(r io.Reader) (uint32, error) {
    return DecodeUint32(r)
}

// EncodeUnionDiscriminant writes the uint32 discriminant of an XDR union.
func EncodeUnionDiscriminant(buf *bytes.Buffer, disc uint32) error {
    return WriteUint32(buf, disc)
}
```

The actual union switching is done in each type's Encode/Decode method (same as the existing v4.0 pattern for OPEN claim types, delegation types, etc.), keeping the union helpers lightweight.

### Pattern 4: V41RequestContext for Handler Signatures
**What:** A struct bundling session/slot/sequence info for v4.1 handler dispatch.
**When to use:** Defined in this phase, used by Phase 20+ handlers.
```go
// V41RequestContext holds session context for NFSv4.1 operations.
// Populated by SEQUENCE processing and passed to subsequent operations.
type V41RequestContext struct {
    SessionID   SessionId4
    SlotID      uint32
    SequenceID  uint32
    HighestSlot uint32
    CacheThis   bool
}
```

### Pattern 5: COMPOUND Minorversion Branch
**What:** The COMPOUND dispatcher checks minorversion and routes to v4.0 or v4.1 dispatch table.
**When to use:** In compound.go ProcessCompound method.
```go
// In ProcessCompound, after decoding minorVersion:
switch minorVersion {
case NFS4_MINOR_VERSION_0:
    // existing v4.0 dispatch
case NFS4_MINOR_VERSION_1:
    // v4.1 dispatch (stub table, all return NFS4ERR_NOTSUPP initially)
default:
    return encodeCompoundResponse(NFS4ERR_MINOR_VERS_MISMATCH, tag, nil)
}
```

### Anti-Patterns to Avoid
- **Separate v41/ package:** User explicitly decided against this. All types live in `v4/types/`.
- **Code-generated XDR:** User locked hand-written codecs. Don't use go-xdr or similar tools.
- **Version prefixes on types:** `ExchangeIdArgs` not `ExchangeId41Args` -- these ops only exist in v4.1.
- **Implementing handler logic:** This phase is types/constants ONLY. No business logic, no session management, no state tracking.
- **Monolithic types file:** Don't dump all v4.1 types into types.go. Per-operation files are the locked decision.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| XDR encode/decode primitives | Custom binary encoding | Existing `internal/protocol/xdr` package | Already battle-tested for v3 and v4.0 |
| Operation name lookup | Manual switch statement | Extend existing `OpName()` function and `opNameToNum` map | Maintains consistency with v4.0 pattern |
| Error code mapping | Ad-hoc error checks | Extend existing `MapMetadataErrorToNFS4()` | Unified error handling across versions |

**Key insight:** This phase adds NO new packages or dependencies. It extends existing patterns. The entire implementation uses only Go stdlib + project's existing xdr package.

## Common Pitfalls

### Pitfall 1: XDR Stream Desync in Dispatch Table
**What goes wrong:** If a v4.1 stub handler doesn't consume its arguments from the reader, the COMPOUND dispatcher will misinterpret the next operation's opcode.
**Why it happens:** Each v4.0 handler reads its args from an `io.Reader`. If v4.1 stubs just return NFS4ERR_NOTSUPP without consuming args, the reader position is wrong.
**How to avoid:** The current v4.0 COMPOUND parses each op into a `RawOp` with pre-extracted `Data []byte`. Check if this pattern also applies to v4.1, or if v4.1 ops use the reader model. Looking at the existing code: `compound.go` reads the opcode, then passes the reader to the handler. The handler MUST consume all its args. For v4.1 stubs, either: (a) use the RawOp pre-parse pattern (read remaining bytes for current op), or (b) parse args in stubs and discard them. Given that the existing stubs in `stubs.go` (e.g., `handleDelegPurge`) DO consume args, v4.1 stubs should follow this pattern.
**Warning signs:** COMPOUND requests with multiple ops return garbled results or fail on the 2nd operation.

### Pitfall 2: Forgetting Optional Arrays with Max 1
**What goes wrong:** XDR `<1>` arrays (like `eia_client_impl_id<1>`) are encoded as `count (uint32) + 0 or 1 elements`. Treating them as a simple optional bool leads to decode failures.
**Why it happens:** XDR optional is encoded differently from XDR array-max-1. In the NFSv4.1 spec, `nfs_impl_id4 eia_client_impl_id<1>` is a variable-length array with max 1 element, not an XDR optional.
**How to avoid:** Always encode as `uint32 count` + `count` elements. Validate `count <= 1` on decode.
**Warning signs:** Decode errors when parsing EXCHANGE_ID from real clients.

### Pitfall 3: Union Discriminant Mismatch
**What goes wrong:** XDR unions have a discriminant value that determines which arm to decode. If the discriminant constants don't match RFC values exactly, decode produces garbage.
**Why it happens:** Copy-paste errors, wrong constant values, or misreading the RFC.
**How to avoid:** Use RFC-named constants (`SP4_NONE = 0`, `SP4_MACH_CRED = 1`, `SP4_SSV = 2`). Cross-reference with Linux kernel `nfs4.h` for authoritative values.
**Warning signs:** Round-trip tests fail for union types.

### Pitfall 4: Breaking Existing v4.0 Tests
**What goes wrong:** Adding `NFS4_MINOR_VERSION_1` and changing the COMPOUND dispatcher to accept minorversion=1 could break existing v4.0 test assumptions.
**Why it happens:** Existing tests may rely on `minorversion != 0` returning `NFS4ERR_MINOR_VERS_MISMATCH`.
**How to avoid:** Check existing compound_test.go for tests that send minorversion > 0 and expect rejection. Update those tests to specifically test minorversion=2+ for rejection, while accepting 0 and 1.
**Warning signs:** `go test ./internal/protocol/nfs/v4/...` fails after COMPOUND changes.

### Pitfall 5: `ca_rdma_ird` Optional Array in ChannelAttrs
**What goes wrong:** The `ca_rdma_ird<1>` field in `channel_attrs4` is an optional uint32 array. Non-RDMA clients send count=0. If decode expects a fixed field, it reads the next struct's data.
**Why it happens:** This is the only optional field in channel_attrs4, easy to miss.
**How to avoid:** Decode as `count (uint32)` + conditionally read the uint32 value. Encode similarly.
**Warning signs:** ChannelAttrs decode reads wrong values for subsequent fields.

### Pitfall 6: SessionId4 Padding
**What goes wrong:** SessionId4 is `opaque sessionid4[16]` -- a fixed-size opaque. Fixed-size opaques do NOT have a length prefix. Variable-size opaques (`opaque<>`) do.
**Why it happens:** Confusing fixed-size XDR opaque encoding with variable-size.
**How to avoid:** Encode/decode SessionId4 as raw 16 bytes with NO length prefix. It's already 4-byte aligned (16 bytes).
**Warning signs:** Sessions fail to match because extra bytes were written/read.

## Code Examples

### Constants to Add to constants.go
```go
// --- NFSv4.1 Operation Numbers (RFC 8881 Section 18) ---

const (
    OP_BACKCHANNEL_CTL     = 40
    OP_BIND_CONN_TO_SESSION = 41
    OP_EXCHANGE_ID         = 42
    OP_CREATE_SESSION      = 43
    OP_DESTROY_SESSION     = 44
    OP_FREE_STATEID        = 45
    OP_GET_DIR_DELEGATION  = 46
    OP_GETDEVICEINFO       = 47
    OP_GETDEVICELIST       = 48
    OP_LAYOUTCOMMIT        = 49
    OP_LAYOUTGET           = 50
    OP_LAYOUTRETURN        = 51
    OP_SECINFO_NO_NAME     = 52
    OP_SEQUENCE            = 53
    OP_SET_SSV             = 54
    OP_TEST_STATEID        = 55
    OP_WANT_DELEGATION     = 56
    OP_DESTROY_CLIENTID    = 57
    OP_RECLAIM_COMPLETE    = 58
)

// --- NFSv4.1 Callback Operation Numbers (RFC 8881 Section 20) ---

const (
    CB_LAYOUTRECALL         uint32 = 5
    CB_NOTIFY               uint32 = 6
    CB_PUSH_DELEG           uint32 = 7
    CB_RECALL_ANY           uint32 = 8
    CB_RECALLABLE_OBJ_AVAIL uint32 = 9
    CB_RECALL_SLOT          uint32 = 10
    CB_SEQUENCE             uint32 = 11
    CB_WANTS_CANCELLED      uint32 = 12
    CB_NOTIFY_LOCK          uint32 = 13
    CB_NOTIFY_DEVICEID      uint32 = 14
)

// --- NFSv4.1 Minor Version ---

const NFS4_MINOR_VERSION_1 = 1

// --- NFSv4.1 Session Constants ---

const NFS4_SESSIONID_SIZE = 16

// --- NFSv4.1 EXCHANGE_ID Flags (RFC 8881 Section 18.35) ---

const (
    EXCHGID4_FLAG_SUPP_MOVED_REFER     = 0x00000001
    EXCHGID4_FLAG_SUPP_MOVED_MIGR      = 0x00000002
    EXCHGID4_FLAG_SUPP_FENCE_OPS       = 0x00000004
    EXCHGID4_FLAG_BIND_PRINC_STATEID   = 0x00000100
    EXCHGID4_FLAG_USE_NON_PNFS         = 0x00010000
    EXCHGID4_FLAG_USE_PNFS_MDS         = 0x00020000
    EXCHGID4_FLAG_USE_PNFS_DS          = 0x00040000
    EXCHGID4_FLAG_UPD_CONFIRMED_REC_A  = 0x40000000
    EXCHGID4_FLAG_CONFIRMED_R          = 0x80000000
    EXCHGID4_FLAG_MASK_A               = 0x40070103
    EXCHGID4_FLAG_MASK_R               = 0x80070103
)

// --- NFSv4.1 CREATE_SESSION Flags ---

const (
    CREATE_SESSION4_FLAG_PERSIST  = 0x00000001
    CREATE_SESSION4_FLAG_CONN_BACK_CHAN = 0x00000002
    CREATE_SESSION4_FLAG_CONN_RDMA = 0x00000004
)

// --- NFSv4.1 State Protection ---

const (
    SP4_NONE      = 0
    SP4_MACH_CRED = 1
    SP4_SSV       = 2
)

// --- NFSv4.1 Channel Direction ---

const (
    CDFC4_FORE         = 0x1
    CDFC4_BACK         = 0x2
    CDFC4_FORE_OR_BOTH = 0x3
    CDFC4_BACK_OR_BOTH = 0x7

    CDFS4_FORE = 0x1
    CDFS4_BACK = 0x2
    CDFS4_BOTH = 0x3
)

// --- NFSv4.1 SEQUENCE Status Flags ---

const (
    SEQ4_STATUS_CB_PATH_DOWN              = 0x00000001
    SEQ4_STATUS_CB_GSS_CONTEXTS_EXPIRING  = 0x00000002
    SEQ4_STATUS_CB_GSS_CONTEXTS_EXPIRED   = 0x00000004
    SEQ4_STATUS_EXPIRED_ALL_STATE_REVOKED = 0x00000008
    SEQ4_STATUS_EXPIRED_SOME_STATE_REVOKED = 0x00000010
    SEQ4_STATUS_ADMIN_STATE_REVOKED       = 0x00000020
    SEQ4_STATUS_RECALLABLE_STATE_REVOKED  = 0x00000040
    SEQ4_STATUS_LEASE_MOVED               = 0x00000080
    SEQ4_STATUS_RESTART_RECLAIM_NEEDED    = 0x00000100
    SEQ4_STATUS_CB_PATH_DOWN_SESSION      = 0x00000200
    SEQ4_STATUS_BACKCHANNEL_FAULT         = 0x00000400
    SEQ4_STATUS_DEVID_CHANGED             = 0x00000800
    SEQ4_STATUS_DEVID_DELETED             = 0x00001000
)
```

### Error Codes to Add to errors.go (or constants.go)
```go
// --- NFSv4.1 Error Codes (RFC 8881 Section 15) ---

const (
    NFS4ERR_BADIOMODE                   = 10049
    NFS4ERR_BADLAYOUT                   = 10050
    NFS4ERR_BAD_SESSION_DIGEST          = 10051
    NFS4ERR_BADSESSION                  = 10052
    NFS4ERR_BADSLOT                     = 10053
    NFS4ERR_COMPLETE_ALREADY            = 10054
    NFS4ERR_CONN_NOT_BOUND_TO_SESSION   = 10055
    NFS4ERR_DELEG_ALREADY_WANTED        = 10056
    NFS4ERR_BACK_CHAN_BUSY              = 10057
    NFS4ERR_LAYOUTTRYLATER              = 10058
    NFS4ERR_LAYOUTUNAVAILABLE           = 10059
    NFS4ERR_NOMATCHING_LAYOUT           = 10060
    NFS4ERR_RECALLCONFLICT              = 10061
    NFS4ERR_UNKNOWN_LAYOUTTYPE          = 10062
    NFS4ERR_SEQ_MISORDERED              = 10063
    NFS4ERR_SEQUENCE_POS                = 10064
    NFS4ERR_REQ_TOO_BIG                 = 10065
    NFS4ERR_REP_TOO_BIG                 = 10066
    NFS4ERR_REP_TOO_BIG_TO_CACHE        = 10067
    NFS4ERR_RETRY_UNCACHED_REP          = 10068
    NFS4ERR_UNSAFE_COMPOUND             = 10069
    NFS4ERR_TOO_MANY_OPS                = 10070
    NFS4ERR_OP_NOT_IN_SESSION           = 10071
    NFS4ERR_HASH_ALG_UNSUPP             = 10072
    // 10073 intentionally missing (no error code assigned)
    NFS4ERR_CLIENTID_BUSY               = 10074
    NFS4ERR_PNFS_IO_HOLE               = 10075
    NFS4ERR_SEQ_FALSE_RETRY             = 10076
    NFS4ERR_BAD_HIGH_SLOT               = 10077
    NFS4ERR_DEADSESSION                 = 10078
    NFS4ERR_ENCR_ALG_UNSUPP             = 10079
    NFS4ERR_PNFS_NO_LAYOUT             = 10080
    NFS4ERR_NOT_ONLY_OP                 = 10081
    NFS4ERR_WRONG_CRED                  = 10082
    NFS4ERR_WRONG_TYPE                  = 10083
    NFS4ERR_DIRDELEG_UNAVAIL            = 10084
    NFS4ERR_REJECT_DELEG                = 10085
    NFS4ERR_RETURNCONFLICT              = 10086
    NFS4ERR_DELEG_REVOKED               = 10087
)
```

### Round-Trip Test Pattern
```go
// exchange_id_test.go
package types

import (
    "bytes"
    "testing"
)

func TestExchangeIdArgs_RoundTrip(t *testing.T) {
    original := &ExchangeIdArgs{
        ClientOwner: ClientOwner4{
            Verifier: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
            OwnerID:  []byte("test-client-owner"),
        },
        Flags:        EXCHGID4_FLAG_USE_NON_PNFS,
        StateProtect: StateProtect4A{How: SP4_NONE},
        ClientImplId: []NfsImplId4{
            {
                Domain: "example.com",
                Name:   "DittoFS",
                Date:   NFS4Time{Seconds: 1700000000, Nseconds: 0},
            },
        },
    }

    // Encode
    var buf bytes.Buffer
    if err := original.Encode(&buf); err != nil {
        t.Fatalf("Encode failed: %v", err)
    }

    // Decode
    decoded := &ExchangeIdArgs{}
    reader := bytes.NewReader(buf.Bytes())
    if err := decoded.Decode(reader); err != nil {
        t.Fatalf("Decode failed: %v", err)
    }

    // Verify fields match
    if decoded.Flags != original.Flags {
        t.Errorf("Flags = 0x%x, want 0x%x", decoded.Flags, original.Flags)
    }
    // ... additional field comparisons
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| RFC 5661 (NFSv4.1 original) | RFC 8881 (NFSv4.1 bis, August 2020) | 2020 | RFC 8881 obsoletes RFC 5661. Use 8881 section numbers for all references. |
| NFSv4.0 SETCLIENTID model | NFSv4.1 EXCHANGE_ID + sessions | NFSv4.1 (2009) | v4.1 clients never use SETCLIENTID/RENEW. Session model replaces per-owner seqid tracking. |
| Per-owner seqid replay detection | Slot table exactly-once semantics | NFSv4.1 | Slot table provides superior replay detection without per-owner bookkeeping. |

**Deprecated/outdated:**
- RFC 5661: Obsoleted by RFC 8881. All section references should use RFC 8881.
- RFC 5662 (v4.1 XDR): Superseded by inline XDR in RFC 8881.

## Open Questions

1. **XdrEncoder/XdrDecoder Interface Signatures**
   - What we know: The user wants types to implement XdrEncoder/XdrDecoder interfaces. The existing xdr package has NO such interfaces -- it uses standalone functions.
   - What's unclear: Should the interface use `*bytes.Buffer` for encode (matching existing pattern) or `io.Writer` (more generic)? Should Decode take `io.Reader` (matching existing pattern)?
   - Recommendation: Use `Encode(*bytes.Buffer) error` and `Decode(io.Reader) error` to match the existing v4.0 codec pattern exactly. Define the interfaces in the xdr package. This is Claude's discretion per CONTEXT.md.

2. **v4.0 Type Compatibility Audit**
   - What we know: Some v4.0 types (Stateid4, NFS4Time, FSID4) are reused by v4.1. The existing encode/decode for Stateid4 uses standalone functions, not struct methods.
   - What's unclear: Should we retrofit Stateid4 with Encode/Decode struct methods to match the new interface?
   - Recommendation: Do NOT retrofit v4.0 types in this phase. The new interface types are for v4.1 types only. Retrofitting existing types risks breaking existing handlers. This can be done as optional tech debt later.

3. **COMPOUND Dispatcher: How to Handle v4.1 Arg Consumption**
   - What we know: The current COMPOUND reads opcode then passes the reader to handlers. Handlers MUST consume all their args. v4.1 stubs need to consume args to avoid desync.
   - What's unclear: For the initial "all NOTSUPP" dispatch table, should each stub have a dummy decode, or should the dispatcher pre-parse v4.1 ops into RawOp (pre-extracting data bytes)?
   - Recommendation: Since the existing v4.0 pattern has handlers consume their own args (see stubs.go `handleDelegPurge`, `handleOpenAttr`), v4.1 stubs should follow the same pattern. However, for an initial "return NOTSUPP for everything" approach where types ARE fully defined, the stubs can call the Decode method on the args struct and discard the result. This validates decode correctness even in stub mode.

4. **`callback_sec_parms4` Union Type**
   - What we know: `callback_sec_parms4` switches on `cb_secflavor` with cases AUTH_NONE (void), AUTH_SYS (authsys_parms), RPCSEC_GSS (gss_cb_handles4). It's used by CREATE_SESSION and BACKCHANNEL_CTL.
   - What's unclear: The `authsys_parms` and `gss_cb_handles4` sub-types may already exist in the RPC package or need to be defined.
   - Recommendation: Define `CallbackSecParms4` in session_common.go. For AUTH_NONE, store nothing. For AUTH_SYS, store raw opaque bytes (defer full authsys_parms parsing to handler phase). For RPCSEC_GSS, store raw opaque bytes similarly. This keeps the type phase clean and defers authentication logic to Phase 19/22.

## Sources

### Primary (HIGH confidence)
- [RFC 8881 - NFSv4.1 Protocol](https://datatracker.ietf.org/doc/rfc8881/) - Authoritative specification for all operations, types, constants, and error codes
- [Linux kernel nfs4.h](https://github.com/torvalds/linux/blob/master/include/linux/nfs4.h) - Reference implementation for all operation numbers, error codes, and flag values
- [Linux kernel uapi/nfs4.h](https://github.com/torvalds/linux/blob/master/include/uapi/linux/nfs4.h) - EXCHGID4_FLAG values, session constants, FATTR4 masks
- [nfstrace nfsv41.x XDR](https://github.com/epam/nfstrace/blob/master/src/protocols/nfs/nfsv41.x) - Complete XDR definitions for all v4.1 types (verified against RFC)
- Existing codebase: `internal/protocol/nfs/v4/types/` - Established patterns for constants, errors, types, and XDR codecs

### Secondary (MEDIUM confidence)
- [buildbarn/go-xdr](https://github.com/buildbarn/go-xdr) - Go XDR compiler reference (not used, but validates Go-to-XDR type mapping approach)

### Tertiary (LOW confidence)
- None. All findings verified against RFC 8881 and Linux kernel source.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Pure extension of existing patterns, no new dependencies
- Architecture: HIGH - File organization locked by user, patterns proven in v4.0
- Pitfalls: HIGH - Verified against existing codebase patterns and RFC requirements
- Constants/values: HIGH - Cross-verified between RFC 8881 and Linux kernel nfs4.h

**Research date:** 2026-02-20
**Valid until:** Indefinite (RFC 8881 is stable, published August 2020)

## NFSv4.1 Operations Complete Reference

### Forward Channel Operations (19 new, ops 40-58)

| Op | Name | Enum | Args Type | Res Type | Complexity |
|----|------|------|-----------|----------|------------|
| 40 | BACKCHANNEL_CTL | OP_BACKCHANNEL_CTL | cb_program + sec_parms | status-only | Medium (union in sec_parms) |
| 41 | BIND_CONN_TO_SESSION | OP_BIND_CONN_TO_SESSION | sessionid + dir + rdma | sessionid + dir + rdma | Simple |
| 42 | EXCHANGE_ID | OP_EXCHANGE_ID | client_owner + flags + protect + impl_id | clientid + seqid + flags + protect + owner + scope + impl_id | Complex (largest type) |
| 43 | CREATE_SESSION | OP_CREATE_SESSION | clientid + seqid + flags + channel_attrs + cb_program + sec_parms | sessionid + seqid + flags + channel_attrs | Complex (channel attrs) |
| 44 | DESTROY_SESSION | OP_DESTROY_SESSION | sessionid | status-only | Trivial |
| 45 | FREE_STATEID | OP_FREE_STATEID | stateid | status-only | Trivial |
| 46 | GET_DIR_DELEGATION | OP_GET_DIR_DELEGATION | signal + notification + delays + attrs | cookieverf + stateid + notification + attrs | Complex (nested union) |
| 47 | GETDEVICEINFO | OP_GETDEVICEINFO | deviceid + layout_type + maxcount + notify | device_addr + notification | Medium |
| 48 | GETDEVICELIST | OP_GETDEVICELIST | layout_type + maxdevices + cookie + cookieverf | cookie + cookieverf + deviceid_list + eof | Medium |
| 49 | LAYOUTCOMMIT | OP_LAYOUTCOMMIT | offset + length + reclaim + stateid + offset_union + time_union + update | newsize_union | Complex (3 unions) |
| 50 | LAYOUTGET | OP_LAYOUTGET | signal + layout_type + iomode + offset + length + minlength + stateid + maxcount | return_on_close + stateid + layouts | Complex |
| 51 | LAYOUTRETURN | OP_LAYOUTRETURN | reclaim + layout_type + iomode + return_union | stateid_union | Medium (nested union) |
| 52 | SECINFO_NO_NAME | OP_SECINFO_NO_NAME | style (enum) | reuses SECINFO4res | Trivial |
| 53 | SEQUENCE | OP_SEQUENCE | sessionid + seqid + slotid + highest_slotid + cachethis | sessionid + seqid + slotid + highest_slotid + target_highest + status_flags | Simple |
| 54 | SET_SSV | OP_SET_SSV | ssv + digest | digest | Simple |
| 55 | TEST_STATEID | OP_TEST_STATEID | stateids[] | status_codes[] | Simple (variable-length arrays) |
| 56 | WANT_DELEGATION | OP_WANT_DELEGATION | want + claim_union | delegation_union | Medium (reuses v4.0 union) |
| 57 | DESTROY_CLIENTID | OP_DESTROY_CLIENTID | clientid | status-only | Trivial |
| 58 | RECLAIM_COMPLETE | OP_RECLAIM_COMPLETE | one_fs (bool) | status-only | Trivial |

### Callback Operations (10 new, CB ops 5-14)

| Op | Name | Enum | Args Type | Res Type | Complexity |
|----|------|------|-----------|----------|------------|
| 5 | CB_LAYOUTRECALL | CB_LAYOUTRECALL | type + iomode + changed + recall_union | status-only | Medium (nested union) |
| 6 | CB_NOTIFY | CB_NOTIFY | stateid + fh + changes[] | status-only | Complex (notification types) |
| 7 | CB_PUSH_DELEG | CB_PUSH_DELEG | fh + delegation | status-only | Medium (reuses delegation union) |
| 8 | CB_RECALL_ANY | CB_RECALL_ANY | objects_to_keep + type_mask | status-only | Simple |
| 9 | CB_RECALLABLE_OBJ_AVAIL | CB_RECALLABLE_OBJ_AVAIL | void | status-only | Trivial |
| 10 | CB_RECALL_SLOT | CB_RECALL_SLOT | target_highest_slotid | status-only | Trivial |
| 11 | CB_SEQUENCE | CB_SEQUENCE | sessionid + seqid + slotid + highest + cache + referring_calls | sessionid + seqid + slotid + highest + target_highest | Medium |
| 12 | CB_WANTS_CANCELLED | CB_WANTS_CANCELLED | contended_wants + resourced_wants | status-only | Trivial |
| 13 | CB_NOTIFY_LOCK | CB_NOTIFY_LOCK | fh + lock_owner | status-only | Simple |
| 14 | CB_NOTIFY_DEVICEID | CB_NOTIFY_DEVICEID | changes[] | status-only | Medium |
