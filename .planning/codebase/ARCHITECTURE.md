# Architecture

**Analysis Date:** 2026-05-28

## Pattern Overview

**Overall:** Modular layered architecture. A single `Runtime` composes six sub-services and acts as the canonical entrypoint for every file/share/adapter operation. Protocol adapters (NFSv3/v4/v4.1 and SMB2/3) and REST handlers both go through `Runtime`.

**Key characteristics:**
- Single composition root (`pkg/controlplane/runtime/`) over six sub-services: `adapters/`, `stores/`, `shares/`, `mounts/`, `lifecycle/`, `identity/`.
- Per-share resources — every share owns its own `*engine.BlockStore` (local + remote + syncer); remote stores ref-counted when configs are shared.
- Content-addressed storage (BLAKE3-256) end to end; FastCDC chunker + dedup; reference-CAS GC.
- Opaque file handles created by metadata stores; encode share identity so the runtime can route, but never parsed by protocol handlers.
- `*metadata.AuthContext` threads every operation; permission checks live in metadata layer, not in adapters.

## Layers

**Protocol adapters (shell):** `pkg/adapter/{nfs,smb}/`
- Lifecycle, connection/session bookkeeping, settings, graceful shutdown.
- Provide hooks `SetRuntime(r *Runtime)` and `Serve(ctx)`.

**Protocol implementations (wire):** `internal/adapter/{nfs,smb}/`
- XDR/SMB framing, RPC/command dispatch, codecs per procedure.
- Auth extraction (AUTH_UNIX, RPCSEC_GSS, NTLM/SPNEGO, Kerberos).
- Handlers call into `Runtime` → metadata + blockstore.

**Runtime composition:** `pkg/controlplane/runtime/`
- `Runtime` owns and wires six sub-services + the metadata façade + client registry.
- Health-checker cache (`checkers.go`) lazily produces per-entity health probes.
- `SettingsWatcher` reacts to live config changes and rewires sub-services.
- `blockstore_init.go` brings up per-share blockstore (local + remote + syncer + GC).

**Sub-services:**
- `adapters/` — protocol adapter registration + lifecycle
- `stores/` — metadata store factory (memory/badger/postgres)
- `shares/` — share coordinator: create/delete, ACL canonicalization, healthcheck
- `mounts/` — NFS mount tracking
- `lifecycle/` — auxiliary servers (REST API, metrics), shutdown orchestration
- `identity/` — squash mapping (RootSquash/AllSquash), idmap (Kerberos ↔ POSIX)

**Metadata service:** `pkg/metadata/`
- `MetadataService` façade; one `MetadataStore` per share, routed by share name encoded in handles.
- Owns ACLs, BR locks, leases, oplocks, delegations, NFSv4 grace + reclaim, idempotency cookies.
- Conformance via `pkg/metadata/storetest/`.

**Block storage:** `pkg/blockstore/`
- Unified CAS contract: `BlockStore` + optional `BlockStoreAppend`.
- `engine/` composes local + remote + syncer + dedup + GC + audit.
- FastCDC chunker (`chunker/`), optional lz4 compression (`compression/`), optional AEAD encryption (`encryption/`).
- Filesystem backend has append log + idempotent rollup (D-23 deleted ClaimBatchSize; D-21 sentinel-zero observation).
- Reference-CAS GC with HoldProvider (engine `Options`).

**Control-plane store:** `pkg/controlplane/store/`
- GORM-based persistence (SQLite default, PostgreSQL HA).
- Users, groups, group memberships, shares + share-adapter configs, adapters + adapter-settings, settings, identity mappings, netgroups, durable-handle hints.

**REST API:** `pkg/controlplane/api/` + `internal/controlplane/api/`
- chi router; JWT middleware; resource handlers under `internal/controlplane/api/handlers/`.

## Data Flow

**NFSv3 READ:**
1. NFS client RPC → connection handler in `pkg/adapter/nfs/connection.go`.
2. `internal/adapter/nfs/dispatch_nfs.go` decodes the RPC, builds `*metadata.AuthContext` (UID/GID, client addr).
3. Procedure handler (`internal/adapter/nfs/v3/handlers/read.go`) calls `Runtime` for the metadata store + blockstore for the handle's share.
4. `MetadataStore.ReadFile` (permission check, attr fetch) → returns block references.
5. Engine `Get(ctx, hash)` resolves bytes (local cache → local CAS → remote on miss).
6. Bytes XDR-encoded and returned.

**NFSv3 WRITE + COMMIT:**
1. `WRITE` handler:
   - `MetadataStore.WriteFile` runs permission check + size/mtime update + returns pre-op attrs for WCC.
   - `Runtime.GetBlockStoreForHandle(ctx, handle)` resolves the per-share engine.
   - Engine `WriteAt` chunks via FastCDC (random-write path stages through the per-file append log on `fs` backend); dedup is consulted before upload.
   - Handler returns updated attrs.
2. `COMMIT`:
   - Engine syncer drains pending dirty regions to the remote tier; rollup converts append-log records into normalized blocks (idempotent under interrupted runs).

**Server startup (`dfs start`):**
1. Cobra entry → load config (file + env + flags).
2. Init logger, telemetry, optional Pyroscope.
3. Open control-plane store (SQLite/PostgreSQL); ensure schema (golang-migrate).
4. Ensure default admin user, default groups, default adapters.
5. `runtime.New(...)` composes sub-services + metadata service.
6. `Runtime.Initialize`:
   - Loads metadata-store configs → instantiates them through `stores.Service`.
   - Loads shares → for each share, brings up blockstore engine (local + remote + syncer + GC) via `blockstore_init.go`, attaches metadata store, creates root handle.
   - Registers adapter factories + creates adapter instances.
7. `lifecycle.Service` starts auxiliary servers (REST API + metrics).
8. Each adapter goroutine runs `Serve(ctx)`.
9. Process blocks on signal; on shutdown the lifecycle service cascades context cancellation.

**Graceful shutdown:**
1. SIGINT/SIGTERM → root context cancelled.
2. `lifecycle.Service` issues shutdown to adapters in registered order.
3. Each adapter closes listeners, waits up to `ShutdownTimeout` for in-flight requests, force-closes the rest.
4. Engine syncers drain (configurable timeout); GC stops cleanly.
5. Metadata stores close; control-plane store closes.
6. Process exits.

## Key Abstractions

**`adapter.Adapter` (`pkg/adapter/adapter.go`):**
- Unified lifecycle: `SetRuntime` → `Serve(ctx)` → `Stop`. NFS and SMB adapters both implement this.

**`Runtime` (`pkg/controlplane/runtime/runtime.go`):**
- Single composition point. Type-aliases sub-services for backward compatibility.
- Exposes accessors like `GetBlockStoreForHandle`, `MetadataServiceForShare`, plus health-checker factories.

**`MetadataStore` (`pkg/metadata/store.go`):**
- Files, directories, ACLs, locks, durable handles, content-addressed object references.
- Backends: `memory`, `badger`, `postgres`. All must pass `pkg/metadata/storetest`.

**`BlockStore` + `BlockStoreAppend` (`pkg/blockstore/blockstore.go`):**
- BLAKE3-keyed unified content surface. Append variant adds the absorber-tier methods used only by `local/fs`.
- Backends: `local/memory`, `local/fs`, `remote/memory`, `remote/s3`.

**`engine.BlockStore` (`pkg/blockstore/engine/engine.go`):**
- Per-share orchestrator wrapping local + remote + syncer + GC + audit + dedup LRU. Created by `blockstore_init.go`.

**`MetadataService`:** Façade routing handle → store, applying export squashing, owning lock managers, mediating durable-handle reconnect.

## Entry Points

**Server CLI:**
- `cmd/dfs/main.go` → `cmd/dfs/commands/start.go`
- Starts daemon (foreground or backgrounded via `daemon_*.go`).

**Remote CLI:**
- `cmd/dfsctl/main.go` → user/group/share/store/adapter/settings/bench/etc.

**Wire entry points:**
- NFS: `internal/adapter/nfs/dispatch.go` (NFS), `dispatch_mount.go` (Mount), plus NLM/NSM/portmap handlers.
- SMB: `internal/adapter/smb/dispatch.go` then `internal/adapter/smb/v2/handlers/`.

**REST API:**
- `pkg/controlplane/api/router.go` mounts handlers in `internal/controlplane/api/handlers/`.
- Auth via `internal/controlplane/api/middleware/` + JWT in `internal/controlplane/api/auth/`.

## Error Handling

**Strategy:** Semantic errors at the metadata layer get mapped to protocol-specific status codes at the adapter boundary.

**Patterns:**
- `metadata.ExportError` enum (`pkg/metadata/errors/`): `ErrNotDirectory`, `ErrNoEntity`, `ErrAccess`, `ErrExist`, `ErrNotEmpty`, `ErrIO`, …
- NFS adapter maps `ExportError` → `NFS3ERR_*` / NFSv4 `NFS4ERR_*` via `internal/adapter/nfs/types/`.
- SMB adapter maps to NTSTATUS via `internal/adapter/common/errmap.go` + `internal/adapter/smb/types/`.
- Blockstore sentinels: `ErrUnknownHash`, `ErrLegacyLayoutDetected`, `ErrAlreadyExists` (CAS conflict is a no-op success).
- Logging discipline: expected outcomes at `Debug`, unexpected at `Error` (CLAUDE.md invariant 6).

## Cross-Cutting Concerns

**Logging:** `internal/logger/` (slog-based) with structured fields, text/json format, configurable level + sink.

**Validation:** `go-playground/validator/v10` struct tags on config + REST DTOs; protocol-level checks in handlers before metadata calls.

**Authentication:**
- NFSv3: AUTH_UNIX + export-level RootSquash/AllSquash.
- NFSv4 / v4.1: AUTH_UNIX + RPCSEC_GSS (Kerberos via `gokrb5/v8`).
- SMB: NTLM + SPNEGO + Kerberos; signing (HMAC-SHA256, AES-CMAC, AES-GMAC); encryption (AES-CCM, AES-GCM).
- REST API: JWT (HMAC); access + refresh tokens.

**Metrics:** Optional Prometheus collector (zero overhead when off); registered through `pkg/health/` wrappers for per-entity probes.

**Tracing:** Optional OpenTelemetry OTLP (sample rate configurable; insecure transport gated by config).

**Profiling:** Optional Pyroscope; gated by config.

**Health:** `pkg/health/` types + `Runtime.statusCheckers` cache; per-entity checkers for blockstores, metadata stores, adapters, shares.

**Rate limiting:** Per-adapter token bucket (global; per-share/user limits are a known gap).

## Invariants (must preserve when editing)

These are codified in `CLAUDE.md`:

1. Protocol handlers handle only protocol concerns; business logic lives in `pkg/metadata/` and store implementations.
2. Every operation carries an `*metadata.AuthContext`; squashing happens at mount in `CheckExportAccess`.
3. File handles are opaque; handlers never parse them.
4. Block stores are per-share; resolve via `Runtime.GetBlockStoreForHandle`. Remote stores ref-counted when configs match; local storage dirs are isolated.
5. WRITE order: `metadataStore.WriteFile` → `GetBlockStoreForHandle` → `blockStore.WriteAt` → return updated attrs.
6. Return `ExportError` values; log expected at `Debug`, unexpected at `Error`.
7. New metadata backend must pass `pkg/metadata/storetest`; new blockstore backend must pass `pkg/blockstore/blockstoretest`.

---

*Architecture analysis: 2026-05-28*
