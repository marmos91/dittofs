# Technology Stack: NFSv4.1 Session Infrastructure (v3.0)

**Project:** DittoFS NFSv4.1 Sessions
**Researched:** 2026-02-20
**Scope:** Stack additions/changes for sessions, EOS, backchannel multiplexing, directory delegations, and connection trunking

## Recommended Stack

### Core Framework (unchanged)
| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| Go | 1.22+ | Implementation language | Existing codebase, goroutine-per-connection model |
| `net` stdlib | - | TCP connections | Already used for NFS adapter |
| `sync` stdlib | - | Mutexes, WaitGroups | Slot table and session concurrency |
| `encoding/binary` | - | XDR encoding | Already used throughout |

### New External Dependencies Required
| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| None | - | - | All v4.1 session features use stdlib only |

No new external dependencies are needed. NFSv4.1 sessions are a pure protocol-level feature that builds on existing Go stdlib primitives (mutexes, maps, byte slices, net.Conn). The existing XDR encoder/decoder, RPC framing, and connection management infrastructure handle all wire format requirements.

### Supporting Libraries (unchanged from v2.0)
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `crypto/rand` | stdlib | Session ID generation | Creating unique 16-byte session IDs |
| `sync/atomic` | stdlib | Sequence counters | XID generation for backchannel |
| `time` | stdlib | Lease timers | Session/client lease management |

## Key Technical Decisions

### Slot Table Implementation: Fixed Array

**Decision:** Fixed-size slice allocated at CREATE_SESSION time, resizable via `sr_target_highest_slotid`.

**Rationale:** The slot count is negotiated once at session creation. Dynamic resizing happens via the server telling the client to use fewer/more slots through the `target_highest_slotid` field in SEQUENCE responses. A fixed slice with a "highest active slot" marker is simpler and matches how Linux nfsd implements it.

### Replay Cache Storage: In-Memory Byte Slices

**Decision:** Cache the entire COMPOUND4res as `[]byte` in each slot.

**Rationale:** The EOS replay cache must store the complete response. Average NFS response is 2-8KB. With 64 slots per session and hundreds of sessions, total memory is manageable (50-100MB). Matches nfs4j and Linux nfsd approaches.

### Backchannel: Writer Goroutine on Existing Connection

**Decision:** Use a dedicated writer goroutine per backchannel-bound connection to avoid blocking the fore channel.

**Rationale:** The same TCP connection carries both fore and back channel messages. A writer goroutine with a channel-based queue prevents the fore channel read loop from being blocked by backchannel writes.

### Session ID Generation: crypto/rand 16 bytes

**Decision:** Generate 16-byte session IDs using `crypto/rand`.

**Rationale:** Must be unique and unpredictable. Same approach as existing confirm verifier generation. Matches RFC 8881 session ID format.

## Alternatives Considered

| Category | Recommended | Alternative | Why Not |
|----------|-------------|-------------|---------|
| Slot table lock | Per-SlotTable Mutex | StateManager RWMutex | Too much contention on every SEQUENCE |
| Replay cache | In-memory byte slices | Disk-backed cache | Single-instance; RAM is sufficient |
| Session ID | crypto/rand 16 bytes | UUID v4 | No need for UUID library; raw bytes match wire format |
| Backchannel | Writer goroutine | Synchronous write | Would block fore channel processing |

## Installation

No new dependencies. Existing build commands work unchanged.

```bash
go build -o dfs cmd/dfs/main.go
go build -o dfsctl cmd/dfsctl/main.go
go test ./...
```

## Sources

- Existing DittoFS codebase — verified by direct code reading
- [nfs4j session implementation](https://github.com/dCache/nfs4j) — MEDIUM confidence
- [Linux nfsd session implementation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html) — HIGH confidence
