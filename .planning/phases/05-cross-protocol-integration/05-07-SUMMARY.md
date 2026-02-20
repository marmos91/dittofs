# Plan 05-07 Summary: Portmapper Auto-Registration

## Status: COMPLETE (Superseded by PR #154)

**Duration:** N/A (delivered via separate PR)
**Gap Closure:** Yes
**Scope Change:** Embedded portmapper server instead of rpcbind client

## Objective

Auto-register NFS-related RPC services (NLM, MOUNT, NFS, NSM) with portmapper on startup so NFS clients can discover services via standard port 111 queries.

**Original goal:** Portmapper client that registers with system rpcbind.
**Actual delivery:** Full embedded RFC 1057 portmapper server — no system rpcbind dependency.

## Superseded By

**PR #154**: `feat(nfs): embed RFC 1057 portmapper for standard NFS mount experience`
**Commit:** `faef38d8`

PR #154 went beyond the plan by implementing a complete embedded portmapper server rather than just a client to system rpcbind. This is strictly better because:
- No dependency on system rpcbind being installed or running
- Works out of the box in containers and minimal environments
- Full control over service discovery lifecycle
- Dual TCP+UDP support per RFC 1057

## What Was Delivered

### Embedded Portmapper Server (`internal/protocol/portmap/`)
- Full RFC 1057 portmapper with types, XDR codec, registry, handlers, dispatch
- TCP (with RPC record marking) and UDP server support
- Thread-safe registry with `sync.RWMutex`
- CALLIT (procedure 5) intentionally omitted to prevent DDoS amplification

### NFS Adapter Integration
- Portmapper starts/stops with the NFS adapter lifecycle
- Auto-registers NFS, MOUNT, NLM, NSM on both TCP and UDP
- Non-fatal: NFS continues if port 111 is unavailable
- Configurable via `adapters.nfs.portmapper.enabled` (default true) and `.port` (default 111)

### Tests
- 27 registry unit tests (Set, Unset, Getport, Dump, Clear, concurrent access)
- 20 server tests (TCP/UDP for NULL, GETPORT, DUMP, SET/UNSET, lifecycle, errors)
- Integration tests verifying full SET/UNSET/GETPORT/DUMP over both transports
- Concurrent query stress tests (10 goroutines x 100 queries)
- All tests pass with `-race` flag

## Files Created/Modified

| File | Changes |
|------|---------|
| `internal/protocol/portmap/dispatch.go` | RPC procedure dispatch table |
| `internal/protocol/portmap/handlers/` | NULL, GETPORT, DUMP, SET, UNSET handlers |
| `internal/protocol/portmap/registry.go` | Thread-safe service registry |
| `internal/protocol/portmap/server.go` | TCP+UDP server implementation |
| `internal/protocol/portmap/types/constants.go` | RFC 1057 constants |
| `internal/protocol/portmap/xdr/` | XDR encode/decode |
| `pkg/adapter/nfs/nfs_adapter.go` | Portmapper lifecycle integration |
| `pkg/adapter/nfs/nfs_adapter_portmap.go` | Portmapper setup and registration |
| `pkg/adapter/nfs/nfs_adapter_shutdown.go` | Deregistration on shutdown |
| `pkg/controlplane/models/adapter_settings.go` | Portmapper config model |
| `test/e2e/portmapper_test.go` | E2E portmapper tests |
| `test/integration/portmap/` | System integration tests |

## Verification

1. `go build ./...` succeeds
2. `go test -race ./...` passes
3. Standard `mount -t nfs server:/export /mnt` works without `-o port=` options
4. `rpcinfo -p localhost` shows all registered services
5. Server starts normally even if port 111 is unavailable

## Key Decisions

- **Embedded server over client**: Eliminates system rpcbind dependency entirely
- **Dual TCP+UDP**: Required by RFC 1057 for compatibility
- **No CALLIT**: Security measure against DDoS amplification attacks
- **Tri-state config**: `*bool` pointer distinguishes "not set" (default true) from explicit false
