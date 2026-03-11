---
phase: "03"
plan: "01"
subsystem: nsm-protocol
tags: [nsm, xdr, types, persistence, crash-recovery]

dependency_graph:
  requires:
    - 01: "Lock infrastructure (EnhancedLock, ConnectionTracker)"
    - 02: "NLM protocol (shared XDR utilities)"
  provides:
    - "NSM RPC constants and procedure numbers"
    - "NSM XDR types and encoding/decoding"
    - "Extended ClientRegistration with NSM fields"
    - "ClientRegistrationStore interface for persistence"
  affects:
    - "03-02: NSM handlers will use these types"
    - "03-03: NSM service will implement ClientRegistrationStore"

tech_stack:
  added: []
  patterns:
    - "NSM types mirror NLM structure (types/constants.go, types/types.go, xdr/)"
    - "Fixed-size opaque[16] for priv field (no length prefix in XDR)"
    - "Persistence interface pattern for crash recovery"

key_files:
  created:
    - internal/protocol/nsm/types/constants.go
    - internal/protocol/nsm/types/types.go
    - internal/protocol/nsm/xdr/decode.go
    - internal/protocol/nsm/xdr/encode.go
    - pkg/metadata/lock/client_store.go
  modified:
    - pkg/metadata/lock/connection.go

decisions:
  - id: "03-01-01"
    choice: "NSM types package mirrors NLM structure"
    rationale: "Consistency with existing protocol implementation patterns"
  - id: "03-01-02"
    choice: "priv field as [16]byte fixed array"
    rationale: "XDR opaque[16] encoding requires fixed size, no length prefix"
  - id: "03-01-03"
    choice: "ClientRegistrationStore interface for persistence"
    rationale: "Enables crash recovery by persisting NSM registrations across restarts"
  - id: "03-01-04"
    choice: "Extend existing ClientRegistration vs new type"
    rationale: "CONTEXT.md specified extending existing type for consistency"

metrics:
  duration: "~3 min"
  completed: "2026-02-05"
---

# Phase 3 Plan 01: NSM Types and Foundation Summary

**One-liner:** NSM protocol types, XDR encoding, and client registration persistence interface for crash recovery

## What Was Built

### 1. NSM Types Package (`internal/protocol/nsm/types/`)

**constants.go:**
- RPC program number: `ProgramNSM = 100024`
- Protocol version: `SMVersion1 = 1`
- Procedure numbers: `SMProcNull`, `SMProcStat`, `SMProcMon`, `SMProcUnmon`, `SMProcUnmonAll`, `SMProcSimuCrash`, `SMProcNotify`
- Result codes: `StatSucc`, `StatFail`
- String limit: `SMMaxStrLen = 1024`
- Helper functions: `ProcedureName()`, `ResultString()`

**types.go:**
- `SMName` - Host identifier for SM_STAT/SM_UNMON
- `MyID` - RPC callback information (hostname, program, version, procedure)
- `MonID` - Combines monitored host and callback info
- `Mon` - SM_MON arguments with 16-byte private data
- `SMStatRes` - SM_STAT/SM_MON response with result and state
- `SMStat` - State-only response variant
- `StatChge` - SM_NOTIFY arguments (host + new state)
- `Status` - SM_NOTIFY callback payload with priv data

### 2. NSM XDR Package (`internal/protocol/nsm/xdr/`)

**decode.go:**
- `DecodeSmName()` - Decode host name for SM_STAT/SM_UNMON
- `DecodeMyID()` - Decode callback RPC info
- `DecodeMonID()` - Decode monitor ID (host + callback)
- `DecodeMon()` - Decode SM_MON arguments
- `DecodeStatChge()` - Decode SM_NOTIFY arguments

**encode.go:**
- `EncodeSMStatRes()` - Encode SM_STAT/SM_MON response
- `EncodeSMStat()` - Encode state-only response
- `EncodeStatus()` - Encode SM_NOTIFY callback payload
- `EncodeSmName()` - Encode host name response

### 3. Extended ConnectionTracker (`pkg/metadata/lock/connection.go`)

**New fields in ClientRegistration:**
- `MonName` - Monitored hostname from SM_MON
- `Priv` - 16-byte private data for callbacks
- `SMState` - NSM state counter at registration
- `CallbackInfo` - Pointer to NSMCallback struct

**New NSMCallback struct:**
- `Hostname` - Callback target host
- `Program` - RPC program number (typically NLM 100021)
- `Version` - Program version
- `Proc` - Procedure number (NLM_FREE_ALL = 23)

**New methods:**
- `UpdateNSMInfo()` - Set NSM fields after SM_MON
- `UpdateSMState()` - Update state counter
- `GetNSMClients()` - Get all clients with callbacks (for SM_NOTIFY)
- `ClearNSMInfo()` - Clear NSM fields after SM_UNMON

### 4. Client Registration Persistence (`pkg/metadata/lock/client_store.go`)

**PersistedClientRegistration type:**
- Storage representation of client registration
- Includes all callback fields flattened for storage
- Server epoch for stale detection

**ClientRegistrationStore interface:**
- `PutClientRegistration()` - Store/update registration
- `GetClientRegistration()` - Retrieve by client ID
- `DeleteClientRegistration()` - Remove single registration
- `ListClientRegistrations()` - Get all (for server restart)
- `DeleteAllClientRegistrations()` - Clear all (SM_UNMON_ALL)
- `DeleteClientRegistrationsByMonName()` - Clear by monitored host

**Conversion functions:**
- `ToPersistedClientRegistration()` - ClientRegistration to storage form
- `FromPersistedClientRegistration()` - Storage form to ClientRegistration

## Technical Notes

### XDR Encoding for priv Field
The `priv` field is XDR `opaque[16]` - a fixed-size opaque array. Per RFC 4506, fixed-size opaque has NO length prefix, unlike variable-length opaque. The encode/decode functions handle this correctly by directly reading/writing 16 bytes.

### Crash Recovery Flow
1. Server starts, loads persisted registrations from ClientRegistrationStore
2. Server increments its NSM state counter (odd = up)
3. Server sends SM_NOTIFY to all registered callbacks with new state
4. Clients receive notification, know server restarted
5. Clients reclaim locks during grace period using NLM_LOCK with reclaim=true

## Deviations from Plan

None - plan executed exactly as written.

## Commits

| Task | Commit | Description |
|------|--------|-------------|
| 1 | a6b8373 | NSM types package (constants, XDR structures) |
| 2 | ba18078 | NSM XDR encode/decode functions |
| 3 | e12a38b | Extended ClientRegistration, persistence interface |

## Verification Results

```bash
$ go build ./internal/protocol/nsm/...
# Success

$ go build ./pkg/metadata/lock/...
# Success

$ go test ./pkg/metadata/lock/...
ok  github.com/marmos91/dittofs/pkg/metadata/lock  1.236s
```

## Next Phase Readiness

Ready for Plan 03-02: NSM Dispatcher and Handlers
- Types and XDR encoding/decoding complete
- ClientRegistration extended with NSM fields
- Persistence interface defined (implementations in 03-03)

**No blockers identified.**
