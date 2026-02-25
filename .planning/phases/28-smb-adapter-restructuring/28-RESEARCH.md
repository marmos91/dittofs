# Phase 28: SMB Adapter Restructuring - Research

**Researched:** 2026-02-25
**Domain:** Go package restructuring, SMB2 adapter organization, BaseAdapter extraction, Authenticator interface design
**Confidence:** HIGH

## Summary

Phase 28 restructures the SMB adapter to mirror the NFS adapter pattern established in Phase 27, extracts a shared BaseAdapter from both adapters, defines an Authenticator interface for multi-protocol auth, and moves `internal/auth/` to `internal/adapter/smb/auth/`. This is a pure code organization phase with zero behavioral changes.

The SMB adapter currently has two oversized files: `smb_adapter.go` (614 lines) and `smb_connection.go` (1071 lines). The connection file mixes four separate concerns: NetBIOS framing, dispatch/response logic, compound request handling, and the thin serve loop. The goal is to extract these concerns into dedicated files in `internal/adapter/smb/`, reducing `connection.go` to ~150 lines (thin serve loop only), mirroring NFS's `connection.go` (318 lines).

The BaseAdapter extraction is the most architecturally significant task. Both NFS and SMB adapters share nearly identical lifecycle code: TCP listener management, graceful shutdown (initiateShutdown, gracefulShutdown, forceCloseConnections, interruptBlockingReads, Stop), connection tracking (sync.Map + WaitGroup + atomic counter), connection semaphore, and metrics logging. This accounts for ~300-400 lines of duplicated code per adapter.

**Primary recommendation:** Execute as 5 ordered plans: (1) file rename + auth move, (2) BaseAdapter extraction, (3) connection slimming (move framing/dispatch/compound to internal), (4) Authenticator interface, (5) handler documentation. Verify `go build ./...` and `go test ./...` after each plan.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- **BaseAdapter is embedded struct** in both NFS and SMB adapters
- **Full lifecycle scope**: Listener management, shutdown orchestration (graceful + force-close), connection tracking (sync.Map + WaitGroup + atomic counter), connection semaphore, metrics logging
- **Runtime reference**: SetRuntime() lives in BaseAdapter. Protocol adapters can override for additional setup (e.g., SMB applies live settings, NFS sets up portmap)
- **Both NFS + SMB**: Extract BaseAdapter and immediately refactor BOTH adapters to use it in this phase
- **Port() in base, Protocol() per-adapter**: Port comes from config (identical logic). Protocol() returns a constant string, stays per-adapter
- **ConnectionFactory interface**: Protocol adapters implement `ConnectionFactory` with `NewConnection(net.Conn) ConnectionHandler`. BaseAdapter calls it in the accept loop. More explicit and testable than callbacks
- **TCP_NODELAY in base**: Universally wanted. Protocol-specific pre-accept checks (e.g., SMB live max_connections) stay per-adapter
- **Unified Authenticator interface in `pkg/adapter/auth.go`**: Single interface used by NFS AUTH_UNIX, NFSv4 RPCSEC_GSS, and SMB SPNEGO
- **Full bridge pattern**: Authenticator validates token, looks up DittoFS user in control plane, returns `models.User` + session key
- **SPNEGO handled inside Authenticator**: SMB Authenticator receives raw SPNEGO tokens, internally detects mechanism (NTLM vs Kerberos), delegates
- **SMB implementations in `internal/adapter/smb/auth/`**: Single package with authenticator.go, ntlm.go, spnego.go
- **NFS implementations in `internal/adapter/nfs/auth/`**: For symmetry, create AUTH_UNIX authenticator extracted from dispatch.go/middleware
- **Mirror NFS structure exactly**: SMB should mirror NFS as closely as possible
- **4-way connection split**: connection.go (~150 lines), dispatch.go, framing.go, compound.go
- **NFS-style dispatch chain**: Single dispatch.go for now (only SMB2)
- **Signing verification** moves to existing `internal/adapter/smb/signing/` package
- **Rename utils.go to helpers.go**: Align naming with NFS's helpers.go
- **Drop smb_ prefix**: smb_adapter.go -> adapter.go, smb_connection.go -> connection.go
- **Rename structs**: SMBAdapter -> Adapter, SMBConnection -> Connection
- **git mv for auth move**: `internal/auth/` -> `internal/adapter/smb/auth/` preserving history
- **Delete old internal/auth/ entirely**: Clean break, no stale directories
- **Config stays as-is**: pkg/adapter/smb/config.go is already well-structured
- **Separate final pass for handler documentation**: Dedicated documentation plan after all restructuring

### Claude's Discretion
- Exact BaseAdapter field names and helper method signatures
- Internal organization of dispatch.go (function ordering, helper grouping)
- How to handle import cycle avoidance during the auth move
- Test file organization (which tests move with which code)

### Deferred Ideas (OUT OF SCOPE)
- SMB3 dialect dispatch split (dispatch_smb2.go / dispatch_smb3.go) -- Phase 39+
- NFS AUTH_UNIX Authenticator is extracted in this phase, but full NFS auth refactoring may need Phase 29
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| REF-04.1 | `pkg/adapter/smb/` files renamed (remove `smb_` prefix) | 3 files to rename: smb_adapter.go -> adapter.go, smb_connection.go -> connection.go, smb_connection_test.go -> connection_test.go. Struct renames: SMBAdapter -> Adapter, SMBConnection -> Connection, NewSMBConnection -> NewConnection. 13 files reference these types (found via grep). |
| REF-04.2 | BaseAdapter extracted to `pkg/adapter/base.go` with shared lifecycle | ~350 lines of identical lifecycle code in NFS (adapter.go 789 lines + shutdown.go 262 lines) and SMB (smb_adapter.go 614 lines). Shared fields: listener, activeConns, shutdownOnce, shutdown chan, connCount, connSemaphore, shutdownCtx, cancelRequests, activeConnections, listenerReady, listenerMu. Shared methods: initiateShutdown, interruptBlockingReads, gracefulShutdown, forceCloseConnections, Stop, logMetrics, GetActiveConnections, GetListenerAddr. |
| REF-04.3 | Both NFS and SMB adapters embed BaseAdapter | NFS has protocol-specific shutdown additions (portmapper, GSS, Kerberos cleanup). SMB has live settings max_connections check. These stay per-adapter. ConnectionFactory interface enables per-adapter connection creation. |
| REF-04.4 | NetBIOS framing moved to `internal/adapter/smb/framing.go` | Functions to move from connection.go: readRequest (~170 lines, reads NetBIOS header + SMB2 message), writeNetBIOSFrame (~30 lines), sendRawMessage (~10 lines). Total ~210 lines. |
| REF-04.5 | Signing verification moved to `internal/adapter/smb/signing.go` | Signing verification is inline in readRequest (connection.go lines 255-303) and verifyCompoundCommandSignature (~35 lines). The `internal/adapter/smb/signing/` package already exists with signing.go (170 lines) and signing_test.go (196 lines). Move verification logic there. |
| REF-04.6 | Dispatch + response logic consolidated in `internal/adapter/smb/dispatch.go` | Functions to move from connection.go: processRequest (~75 lines), processRequestWithFileID (~55 lines), processRequestWithInheritedFileID (~10 lines), sendResponse (~35 lines), sendErrorResponse (~12 lines), sendMessage (~40 lines), handleSMB1Negotiate (~80 lines), SendAsyncChangeNotifyResponse (~25 lines), trackSessionLifecycle (~30 lines), makeErrorBody (~8 lines). Total ~370 lines. Note: internal/adapter/smb/dispatch.go already exists (326 lines) with DispatchTable and handler wrappers. Consolidate. |
| REF-04.7 | Compound handling moved to `internal/adapter/smb/compound.go` | Functions to move: processCompoundRequest (~80 lines), parseCompoundCommand (~40 lines), verifyCompoundCommandSignature (~35 lines), injectFileID (~35 lines). Total ~190 lines. |
| REF-04.8 | `auth.Authenticator` interface defined, NTLM + Kerberos implementations | Interface in `pkg/adapter/auth.go`. SMB implementations in `internal/adapter/smb/auth/` (move from `internal/auth/ntlm/` 838 lines + `internal/auth/spnego/` 193 lines, plus tests). Only 2 consuming files: session_setup.go and session_setup_test.go. NFS auth/ package created for symmetry. |
| REF-04.9 | Shared handler helpers extracted to `internal/adapter/smb/helpers.go` | Current `internal/adapter/smb/utils.go` (155 lines) already has the generic handleRequest helper. Rename to helpers.go for consistency with NFS. |
| REF-04.10 | `pkg/adapter/smb/connection.go` reduced to thin read/dispatch/write loop | Current: 1071 lines. Target: ~150 lines. After extracting framing (~210), dispatch+response (~370), compound (~190), signing verification (~50) to internal/, what remains: Serve loop (~75 lines), handleConnectionClose (~20 lines), cleanupSessions (~35 lines), handleRequestPanic (~15 lines), NewConnection (~8 lines), struct definition (~15 lines), TrackSession/UntrackSession (~20 lines). Total ~190 lines (close to target). |
| REF-04.11 | Handler documentation added (3-5 lines each, all SMB2 commands) | 40 handler files in `internal/adapter/smb/v2/handlers/`. 19 commands in dispatch table. Each handler function needs 3-5 line Godoc comment. Separate final pass. |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go | 1.25.0 | Language runtime | Project's go.mod specifies 1.25.0 |
| `go build ./...` | - | Build verification after each step | Catches all import errors immediately |
| `go test ./...` | - | Test verification after each step | Ensures behavior unchanged |
| `go vet ./...` | - | Static analysis | Catches common mistakes |

### Supporting
| Tool | Purpose | When to Use |
|------|---------|-------------|
| `git mv` | Rename files preserving history | Every file rename and auth package move |
| `sed -i '' 's/old/new/g'` | Bulk import path and identifier rewriting | After file renames and struct renames |
| `grep -rn` | Find all references to renamed identifiers | Before each rename to know scope |

## Architecture Patterns

### Recommended Project Structure (Post-Phase 28)

```
pkg/adapter/
├── adapter.go            # Adapter interface (existing)
├── auth.go               # Authenticator interface (NEW)
├── base.go               # BaseAdapter embedded struct (NEW)
├── nfs/
│   ├── adapter.go        # NFSAdapter embeds BaseAdapter (refactored)
│   ├── connection.go     # NFSConnection (existing, ~318 lines)
│   ├── dispatch.go       # NFS handleRPCCall dispatch (existing)
│   ├── handlers.go       # handleRequest generic helper (existing)
│   ├── identity/         # NFS identity resolution (existing)
│   ├── nlm.go            # NLM service (existing)
│   ├── portmap.go        # Portmapper service (existing)
│   ├── reply.go          # RPC reply building (existing)
│   ├── settings.go       # NFS live settings (existing)
│   └── shutdown.go       # NFS-specific shutdown additions (refactored)
└── smb/
    ├── adapter.go         # Adapter embeds BaseAdapter (renamed from smb_adapter.go)
    ├── config.go          # SMBConfig (unchanged)
    ├── connection.go      # Connection thin serve loop (renamed+slimmed from smb_connection.go)
    └── connection_test.go # Connection tests (renamed from smb_connection_test.go)

internal/adapter/
├── pool/                  # Shared buffer pool (existing)
├── nfs/
│   ├── auth/              # NFS AUTH_UNIX Authenticator (NEW, extracted from middleware)
│   ├── connection.go      # NFS internal connection helpers (existing)
│   ├── dispatch.go        # NFS dispatch entry point (existing)
│   ├── dispatch_mount.go  # Mount dispatch (existing)
│   ├── dispatch_nfs.go    # NFS v3/v4 dispatch (existing)
│   ├── helpers.go         # Shared handler helpers (existing)
│   ├── middleware/        # Auth extraction middleware (existing)
│   └── ...
└── smb/
    ├── auth/              # SMB Authenticator implementations (MOVED from internal/auth/)
    │   ├── authenticator.go  # SMBAuthenticator struct (NEW wrapper)
    │   ├── ntlm.go           # NTLM auth (moved from internal/auth/ntlm/)
    │   ├── ntlm_test.go      # NTLM tests
    │   ├── spnego.go          # SPNEGO parsing (moved from internal/auth/spnego/)
    │   └── spnego_test.go     # SPNEGO tests
    ├── compound.go        # Compound request handling (NEW, extracted from connection.go)
    ├── dispatch.go        # Dispatch + response logic (EXPANDED with connection.go code)
    ├── doc.go             # Package documentation (existing)
    ├── framing.go         # NetBIOS framing (NEW, extracted from connection.go)
    ├── header/            # SMB2 header parsing (existing)
    ├── helpers.go         # Generic handleRequest helper (RENAMED from utils.go)
    ├── rpc/               # DCE/RPC pipe handling (existing)
    ├── session/           # Session management (existing)
    ├── signing/           # Signing + verification (EXPANDED)
    ├── types/             # SMB2 constants and types (existing)
    └── v2/handlers/       # SMB2 command handlers (existing)
```

### Pattern 1: BaseAdapter Embedded Struct

**What:** Extract shared TCP lifecycle into an embeddable struct with a ConnectionFactory callback.
**When to use:** When multiple protocol adapters share identical listener/shutdown/tracking logic.

```go
// pkg/adapter/base.go
package adapter

type ConnectionHandler interface {
    Serve(ctx context.Context)
}

type ConnectionFactory interface {
    NewConnection(conn net.Conn) ConnectionHandler
}

type BaseConfig struct {
    BindAddress        string
    Port               int
    MaxConnections     int
    ShutdownTimeout    time.Duration
    MetricsLogInterval time.Duration
}

type BaseAdapter struct {
    Config             BaseConfig
    listener           net.Listener
    activeConns        sync.WaitGroup
    shutdownOnce       sync.Once
    shutdown           chan struct{}
    connCount          atomic.Int32
    connSemaphore      chan struct{}
    shutdownCtx        context.Context
    cancelRequests     context.CancelFunc
    activeConnections  sync.Map
    listenerReady      chan struct{}
    listenerMu         sync.RWMutex
    registry           *runtime.Runtime
}

// SetRuntime sets the runtime. Protocol adapters embed BaseAdapter
// and override SetRuntime via method on the outer struct.
func (b *BaseAdapter) SetRuntime(rt *runtime.Runtime) {
    b.registry = rt
}

// ServeWithFactory runs the accept loop, delegating to factory.NewConnection
// for protocol-specific connection handling. PreAccept hook allows per-adapter
// checks (e.g., SMB live settings max_connections).
func (b *BaseAdapter) ServeWithFactory(ctx context.Context, factory ConnectionFactory, preAccept func(net.Conn) bool) error {
    // ... listener setup, accept loop, TCP_NODELAY, connection tracking ...
}

// Shutdown methods: initiateShutdown, gracefulShutdown, forceCloseConnections,
// interruptBlockingReads, Stop, logMetrics, GetActiveConnections, GetListenerAddr
```

```go
// pkg/adapter/smb/adapter.go
type Adapter struct {
    adapter.BaseAdapter  // embedded
    handler        *handlers.Handler
    sessionManager *session.Manager
    // ... SMB-specific fields only
}

func (a *Adapter) SetRuntime(rt *runtime.Runtime) {
    a.BaseAdapter.SetRuntime(rt) // call base
    a.handler.Registry = rt       // SMB-specific setup
    a.applySMBSettings(rt)
}

func (a *Adapter) Serve(ctx context.Context) error {
    return a.ServeWithFactory(ctx, a, a.preAcceptCheck)
}

func (a *Adapter) NewConnection(conn net.Conn) adapter.ConnectionHandler {
    return NewConnection(a, conn)
}
```

### Pattern 2: Authenticator Interface (Full Bridge)

**What:** Unified interface where each auth method validates a token and returns a DittoFS `models.User`.
**When to use:** When multiple protocols (NFS, SMB) need to authenticate users via different mechanisms but produce the same identity model.

```go
// pkg/adapter/auth.go
package adapter

type AuthResult struct {
    User       *models.User
    SessionKey []byte  // For signing (NTLM/Kerberos session key)
    IsGuest    bool
}

type Authenticator interface {
    // Authenticate validates a security token and returns the authenticated user.
    // For multi-round protocols (NTLM, SPNEGO), returns ErrMoreProcessingRequired
    // with a challenge token that must be sent back to the client.
    Authenticate(ctx context.Context, token []byte) (*AuthResult, []byte, error)
}
```

### Pattern 3: Connection Slimming via Package Boundary

**What:** Move protocol-specific wire format and dispatch logic to `internal/` packages, keeping only the thin serve loop in `pkg/`.
**When to use:** When a connection file mixes multiple concerns that can be separated by package.

The NFS adapter demonstrates this pattern:
- `pkg/adapter/nfs/connection.go` (318 lines): Serve loop, readRequest (using internal framing), processRequest (delegates to internal dispatch)
- `internal/adapter/nfs/dispatch.go`: Program/version/procedure routing
- `internal/adapter/nfs/connection.go`: RPC framing utilities (ReadFragmentHeader, ReadRPCMessage, ValidateFragmentSize)

SMB will mirror:
- `pkg/adapter/smb/connection.go` (~150 lines): Serve loop, delegates to internal for read/dispatch/write
- `internal/adapter/smb/framing.go`: NetBIOS framing (readRequest, writeNetBIOSFrame)
- `internal/adapter/smb/dispatch.go`: Command routing, response building, SMB1 negotiate
- `internal/adapter/smb/compound.go`: Compound request handling

### Anti-Patterns to Avoid
- **Moving methods that access private struct fields to separate packages**: The connection struct has private fields (server, conn, writeMu, etc.). Methods that need these must either stay on the struct or accept parameters. Use receiver methods on Connection in pkg/ that delegate to package functions in internal/ that receive parameters.
- **Breaking the dispatch table initialization**: The DispatchTable in `internal/adapter/smb/dispatch.go` uses `init()`. When moving dispatch logic from connection.go, ensure the DispatchTable import chain isn't broken.
- **Circular imports between pkg/adapter/smb and internal/adapter/smb**: The connection.go in `pkg/` already imports `internal/adapter/smb`. Moving dispatch code to `internal/` must not create a reverse dependency. The pattern is: `pkg/` imports `internal/`, never the reverse.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Import path rewriting | Manual find/replace | `sed -i '' 's/old/new/g'` + `go build ./...` verification | Too many files (39 reference `adapter/smb`) |
| Struct rename verification | Manual grep | `go build ./...` immediately catches missed renames | Compiler is authoritative |
| Git history preservation | `cp` + `rm` | `git mv` | Preserves blame and history |

**Key insight:** This is purely mechanical refactoring. The compiler (`go build ./...`) is the authoritative verification tool. Every step should be immediately verifiable with `go build` + `go test`.

## Common Pitfalls

### Pitfall 1: Import Cycles During Auth Move
**What goes wrong:** Moving `internal/auth/` to `internal/adapter/smb/auth/` could create import cycles if the new location imports packages that import the old location.
**Why it happens:** `internal/auth/ntlm/` imports `pkg/controlplane/models` (for `models.User`). `internal/adapter/smb/v2/handlers/session_setup.go` imports `internal/auth/ntlm`. Moving auth under `internal/adapter/smb/auth/` keeps the same import direction (handlers import auth), so no cycle.
**How to avoid:** The import direction is: handlers -> auth -> models. Moving auth to `internal/adapter/smb/auth/` preserves this direction. Only 2 files import from `internal/auth/`: `session_setup.go` and `session_setup_test.go`.
**Warning signs:** `go build` fails with "import cycle" error.

### Pitfall 2: Connection Methods Needing Private Fields After Move
**What goes wrong:** Functions moved from `smb_connection.go` (pkg/) to `internal/adapter/smb/framing.go` need access to `SMBConnection.conn`, `SMBConnection.server.config`, and `SMBConnection.writeMu`.
**Why it happens:** Go's access control is package-level. Moving functions to a different package means they can't access unexported fields.
**How to avoid:** Two approaches: (1) Keep methods as Connection receivers in pkg/ that delegate to internal functions with explicit parameters, or (2) Pass the needed values (net.Conn, config values) as parameters to internal functions. For readRequest/writeNetBIOSFrame, pass `net.Conn` + timeout values + max message size as parameters.
**Warning signs:** Compilation errors about unexported fields.

### Pitfall 3: BaseAdapter Abstraction Leakage
**What goes wrong:** The BaseAdapter tries to handle too much protocol-specific logic.
**Why it happens:** NFS has protocol-specific shutdown steps (portmapper, GSS, Kerberos). SMB has live settings max_connections check in the accept loop.
**How to avoid:** Keep BaseAdapter focused on TCP-level concerns only. Use the PreAccept hook for protocol-specific connection-level checks. Protocol-specific shutdown stays in the outer adapter's Stop() method (calls base Stop after its own cleanup).
**Warning signs:** BaseAdapter imports NFS or SMB internal packages.

### Pitfall 4: Test Files Reference Old Struct Names
**What goes wrong:** Tests reference `SMBAdapter`, `SMBConnection`, `NewSMBConnection` after renaming to `Adapter`, `Connection`, `NewConnection`.
**Why it happens:** Test files in `pkg/adapter/smb/` use the old names. External files (e.g., `cmd/dfs/commands/start.go`) use `smb.New(SMBConfig{})` which becomes `smb.New(Config{})`.
**How to avoid:** Use `sed` to rename all occurrences. The struct rename must include: SMBAdapter -> Adapter, SMBConnection -> Connection, NewSMBConnection -> NewConnection, SMBConfig -> Config, SMBTimeoutsConfig -> TimeoutsConfig. Run `go build ./...` immediately after.
**Warning signs:** Tests fail to compile after rename.

### Pitfall 5: Signing Verification Depends on Session State
**What goes wrong:** Moving signing verification from connection.go to `internal/adapter/smb/signing/` fails because verification needs access to `handler.GetSession()` to retrieve the session's signing key.
**Why it happens:** Signing verification is interleaved with request reading and needs session state.
**How to avoid:** The verification function should accept the session signing state as a parameter (or a function to look up a session), not import the handler package. Keep the "get session" logic in the connection/dispatch layer and pass signing-relevant data to the signing package.
**Warning signs:** Import cycle between signing/ and v2/handlers/.

## Code Examples

### BaseAdapter Serve Loop Pattern (from NFS adapter analysis)

The shared accept loop pattern found in both adapters:

```go
// Both NFS and SMB use this exact pattern. Extract to BaseAdapter.
for {
    if s.connSemaphore != nil {
        select {
        case s.connSemaphore <- struct{}{}:
        case <-s.shutdown:
            return s.gracefulShutdown()
        }
    }

    tcpConn, err := s.listener.Accept()
    if err != nil {
        if s.connSemaphore != nil {
            <-s.connSemaphore
        }
        select {
        case <-s.shutdown:
            return s.gracefulShutdown()
        default:
            continue
        }
    }

    // TCP_NODELAY
    if tcp, ok := tcpConn.(*net.TCPConn); ok {
        _ = tcp.SetNoDelay(true)
    }

    // Protocol-specific pre-accept check (e.g., live settings)
    // ... this part differs per protocol, use PreAccept hook

    s.activeConns.Add(1)
    s.connCount.Add(1)
    connAddr := tcpConn.RemoteAddr().String()
    s.activeConnections.Store(connAddr, tcpConn)

    conn := factory.NewConnection(tcpConn)
    go func(addr string, tcp net.Conn) {
        defer func() {
            s.activeConnections.Delete(addr)
            s.activeConns.Done()
            s.connCount.Add(-1)
            if s.connSemaphore != nil {
                <-s.connSemaphore
            }
        }()
        conn.Serve(s.shutdownCtx)
    }(connAddr, tcpConn)
}
```

### Connection Slimming - Delegation Pattern

```go
// pkg/adapter/smb/connection.go (thin, ~150 lines)
func (c *Connection) Serve(ctx context.Context) {
    defer c.handleConnectionClose()
    // Set initial idle timeout
    // Main loop:
    for {
        // Check context/shutdown
        // Read request via internal framing
        hdr, body, compound, err := smb_internal.ReadRequest(ctx, c.conn, c.server.config)
        // If compound: delegate to smb_internal.ProcessCompound(...)
        // Else: dispatch via smb_internal.ProcessRequest(...)
        // Reset idle timeout
    }
}
```

### Authenticator Interface Usage in Session Setup

```go
// After Authenticator interface is defined, session_setup.go becomes:
func (h *Handler) SessionSetup(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
    req, err := parseSessionSetupRequest(body)
    // ...
    result, challengeToken, err := h.authenticator.Authenticate(ctx.Context, req.SecurityBuffer)
    if errors.Is(err, adapter.ErrMoreProcessingRequired) {
        // NTLM Type 2: return challenge with STATUS_MORE_PROCESSING_REQUIRED
        return buildChallengeResponse(challengeToken), nil
    }
    if err != nil {
        return buildErrorResponse(types.StatusLogonFailure), nil
    }
    // Create session with result.User and result.SessionKey
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Monolithic connection.go (1071 lines) | Split into 4 files per concern | Phase 28 | Maintainability, matches NFS pattern |
| Duplicated lifecycle in NFS+SMB | Shared BaseAdapter | Phase 28 | ~600 lines of dedup, single place for shutdown bugs |
| Auth packages at `internal/auth/` | Auth under adapter that uses them (`internal/adapter/smb/auth/`) | Phase 28 | Clear ownership, aligned with protocol adapter structure |
| `SMBAdapter`/`SMBConnection` names | `Adapter`/`Connection` (package provides context) | Phase 28 | Consistent with Go naming conventions and NFS pattern |

## Open Questions

1. **BaseAdapter NFS metrics integration**
   - What we know: NFS adapter records metrics (RecordConnectionAccepted, etc.) in the accept loop. SMB does not.
   - What's unclear: Should metrics recording be part of BaseAdapter or stay protocol-specific?
   - Recommendation: Add an optional `MetricsRecorder` interface to BaseAdapter. NFS provides one, SMB provides nil. This keeps BaseAdapter generic while allowing metrics.

2. **Exact PreAccept hook signature**
   - What we know: SMB checks live settings max_connections before accepting. NFS checks live settings and re-applies NFS settings.
   - What's unclear: Should PreAccept return bool (reject/accept) or allow modifying connection state?
   - Recommendation: `PreAccept(conn net.Conn) bool` - simple reject/accept. NFS's settings re-apply can happen before the factory call instead.

3. **Auth package flattening vs. subpackages**
   - What we know: Currently `internal/auth/ntlm/` and `internal/auth/spnego/` are separate packages. Moving to `internal/adapter/smb/auth/` could be one flat package or retain sub-packages.
   - What's unclear: Whether flattening causes naming conflicts.
   - Recommendation: Flatten into single `auth` package. The ntlm and spnego types don't conflict. This simplifies imports. If there are naming conflicts, keep sub-packages.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (built-in) |
| Config file | None needed |
| Quick run command | `go test ./pkg/adapter/... ./internal/adapter/smb/...` |
| Full suite command | `go test ./...` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| REF-04.1 | File rename compiles | unit | `go build ./pkg/adapter/smb/...` | N/A (build test) |
| REF-04.2 | BaseAdapter lifecycle works | unit | `go test ./pkg/adapter/...` | Wave 0 |
| REF-04.3 | Both adapters embed BaseAdapter | unit | `go build ./pkg/adapter/nfs/... ./pkg/adapter/smb/...` | N/A (build test) |
| REF-04.4 | NetBIOS framing works | unit | `go test ./internal/adapter/smb/...` | Inline in connection_test |
| REF-04.5 | Signing verification works | unit | `go test ./internal/adapter/smb/signing/...` | Existing signing_test.go |
| REF-04.6 | Dispatch routing works | unit | `go test ./internal/adapter/smb/...` | Existing handler_test.go |
| REF-04.7 | Compound handling works | unit | `go test ./pkg/adapter/smb/...` | Existing connection_test.go |
| REF-04.8 | Auth implementations work | unit | `go test ./internal/adapter/smb/auth/...` | Existing ntlm_test.go, spnego_test.go |
| REF-04.9 | Helpers work | unit | `go test ./internal/adapter/smb/...` | Inline in utils.go |
| REF-04.10 | Thin connection compiles | unit | `go build ./pkg/adapter/smb/...` | N/A (build test) |
| REF-04.11 | Handler docs present | manual | Review Godoc output | N/A |

### Sampling Rate
- **Per task commit:** `go build ./... && go test ./pkg/adapter/... ./internal/adapter/smb/...`
- **Per wave merge:** `go test ./...`
- **Phase gate:** Full suite green + `go vet ./...`

### Wave 0 Gaps
- None -- existing test infrastructure covers all phase requirements. Tests for ntlm, spnego, signing, connection, and handlers already exist and will move with their code.

## Sources

### Primary (HIGH confidence)
- Codebase analysis: Direct reading of all relevant source files
  - `pkg/adapter/smb/smb_adapter.go` (614 lines) -- current SMB adapter
  - `pkg/adapter/smb/smb_connection.go` (1071 lines) -- current SMB connection
  - `pkg/adapter/nfs/adapter.go` (789 lines) -- NFS adapter (Phase 27 pattern)
  - `pkg/adapter/nfs/connection.go` (318 lines) -- NFS connection (target model)
  - `pkg/adapter/nfs/shutdown.go` (262 lines) -- NFS shutdown (shared pattern)
  - `internal/auth/ntlm/ntlm.go` (838 lines) -- NTLM auth to move
  - `internal/auth/spnego/spnego.go` (193 lines) -- SPNEGO to move
  - `internal/adapter/smb/dispatch.go` (326 lines) -- existing dispatch table
  - `internal/adapter/smb/utils.go` (155 lines) -- existing helpers
  - `internal/adapter/smb/signing/signing.go` (170 lines) -- existing signing

### Secondary (MEDIUM confidence)
- Phase 27 plans and research -- established patterns for adapter restructuring
- CONTEXT.md decisions -- user-confirmed design choices

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - pure Go refactoring with `go build/test` verification
- Architecture: HIGH - mirrors Phase 27 NFS pattern (already proven), all source files analyzed
- Pitfalls: HIGH - identified from direct code analysis, not hypothetical

**Research date:** 2026-02-25
**Valid until:** 2026-03-25 (stable -- pure refactoring, no external dependencies)
