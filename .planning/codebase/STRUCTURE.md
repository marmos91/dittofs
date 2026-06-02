# Codebase Structure

**Analysis Date:** 2026-05-28

## Directory Layout

```
dittofs-v10-plan/
├── cmd/                                  # Binary entry points
│   ├── dfs/                              # Server daemon
│   │   ├── main.go                       # Entry point
│   │   └── commands/
│   │       ├── root.go                   # Root Cobra command
│   │       ├── start.go                  # Launch server (loads config, runtime, adapters)
│   │       ├── stop.go / stop_unix.go / stop_windows.go
│   │       ├── status.go, logs.go
│   │       ├── init.go                   # Generate initial config
│   │       ├── migrate.go                # DB schema migrations
│   │       ├── migrate_to_cas.go         # Legacy → CAS blockstore migration
│   │       ├── daemon_unix.go / daemon_windows.go
│   │       ├── version.go, completion.go
│   │       └── config/                   # config subcommands (show, edit, validate, schema)
│   │
│   └── dfsctl/                           # Remote management CLI
│       ├── main.go
│       ├── cmdutil/                      # Shared CLI helpers (output, auth, flags)
│       └── commands/
│           ├── root.go
│           ├── login.go / logout.go / switch_user.go
│           ├── status.go, version.go, completion.go
│           ├── context/                  # Multi-server context management
│           ├── user/, group/, share/     # Resource CRUD (each w/ subcommands)
│           ├── store/                    # metadata + blockstore management
│           ├── adapter/                  # Protocol adapter management
│           ├── settings/                 # Server settings
│           ├── netgroup/, idmap/         # NIS netgroups + identity mapping
│           ├── grace/                    # NFSv4 grace period control
│           ├── client/                   # Client info / kill
│           ├── system/                   # System info, health
│           └── bench/                    # Bench harness commands
│
├── pkg/                                  # Public-ish packages (stable surfaces)
│   ├── adapter/                          # Protocol adapter shell (NFS + SMB lifecycle)
│   │   ├── adapter.go                    # Adapter interface, base lifecycle
│   │   ├── base.go, auth.go, errors.go
│   │   ├── identity.go, healthcheck.go
│   │   ├── nfs/                          # NFSv3+v4 adapter shell
│   │   │   ├── adapter.go, connection.go, dispatch.go
│   │   │   ├── handlers.go, reply.go, shutdown.go
│   │   │   ├── settings.go, nlm.go, portmap.go
│   │   │   └── identity/
│   │   └── smb/                          # SMB2/3 adapter shell
│   │       ├── adapter.go, connection.go, config.go
│   │       ├── lease_notifier.go, healthcheck.go
│   │
│   ├── apiclient/                        # HTTP client for control-plane REST API
│   │
│   ├── auth/                             # Auth primitives shared by protocols
│   │   ├── auth.go, identity.go
│   │   ├── kerberos/                     # Kerberos provider + keytab handling
│   │   └── sid/                          # Windows SID, well-known mapping
│   │
│   ├── bench/                            # In-tree benchmark workloads
│   │   ├── runner.go, stats.go
│   │   └── workload_*.go                 # seq, rand, small, meta, storage_tiers
│   │
│   ├── blockstore/                       # Unified CAS block storage
│   │   ├── blockstore.go                 # BlockStore + BlockStoreAppend interfaces
│   │   ├── store.go, types.go            # Shared types
│   │   ├── defaults.go, errors.go
│   │   ├── objectid.go, retention.go     # ObjectID + retention metadata
│   │   ├── hashset.go                    # Hash-keyed set helper
│   │   ├── doc.go                        # Package overview + sentinel files
│   │   ├── chunker/                      # FastCDC chunker (BLAKE3-keyed CAS)
│   │   ├── compression/                  # lz4 codec + decorator
│   │   ├── encryption/                   # AEAD decorator + KMIP keyprovider
│   │   ├── engine/                       # Coordinator (local + remote + syncer + GC + audit)
│   │   │   ├── engine.go, coordinator.go, cache.go
│   │   │   ├── syncer.go, sync_queue.go, sync_entry.go, sync_health.go
│   │   │   ├── gc.go, gcstate.go         # Reference-CAS GC
│   │   │   ├── dedup.go, fetch.go, upload.go
│   │   │   ├── audit_state.go, healthcheck.go
│   │   │   └── range.go                  # Range read assembly
│   │   ├── local/                        # Local CAS implementations
│   │   │   ├── memory/                   # In-memory CAS
│   │   │   └── fs/                       # Filesystem CAS (append log + rollup)
│   │   │       ├── fs.go                 # FSStore
│   │   │       ├── appendlog.go, rollup.go, logindex.go
│   │   │       ├── chunkstore.go, eviction.go, dedup_lru.go
│   │   │       ├── compaction.go, recovery.go, groupcommit.go
│   │   │       ├── access_tracker.go, fdpool.go
│   │   │       └── interval_tree.go
│   │   ├── remote/                       # Remote CAS implementations
│   │   │   ├── remote.go                 # Common helpers
│   │   │   ├── memory/                   # Ephemeral remote
│   │   │   └── s3/                       # S3 + verifier
│   │   ├── migrate/                      # Legacy blockstore → CAS migration
│   │   └── blockstoretest/               # Cross-backend conformance suite
│   │
│   ├── config/                           # Config types + loading + validation
│   │
│   ├── controlplane/                     # Control plane (config + runtime)
│   │   ├── controlplane.go               # Top-level wiring
│   │   ├── models/                       # Domain models (User, Group, Share, Adapter, …)
│   │   ├── store/                        # GORM persistent store (SQLite/Postgres)
│   │   │   ├── gorm.go, store.go, interface.go
│   │   │   ├── users.go, groups.go, shares.go, permissions.go
│   │   │   ├── adapters.go, adapter_settings.go, settings.go
│   │   │   ├── identity.go, netgroups.go, metadata.go, block.go
│   │   │   └── health.go, helpers.go
│   │   ├── api/                          # REST API server (chi + JWT)
│   │   │   ├── server.go, router.go, config.go
│   │   └── runtime/                      # Ephemeral runtime composing sub-services
│   │       ├── runtime.go                # Composition root + Runtime type
│   │       ├── init.go, share.go, mounts.go
│   │       ├── checkers.go               # Lazy per-entity health-checkers
│   │       ├── settings_watcher.go       # Live settings → sub-service rewiring
│   │       ├── blockstore_init.go        # Per-share blockstore bring-up
│   │       ├── blockgc.go, blockaudit.go # GC + audit hooks
│   │       ├── clients.go, netgroups.go
│   │       ├── adapters/                 # adapters.Service (registration + lifecycle)
│   │       ├── stores/                   # stores.Service (metadata-store factory)
│   │       ├── shares/                   # shares.Service (share coordinator, ACL, healthcheck)
│   │       ├── mounts/                   # mounts.Service (NFS mount tracker)
│   │       ├── lifecycle/                # lifecycle.Service (aux servers, shutdown)
│   │       ├── identity/                 # identity.Service (squash + idmap)
│   │       ├── blockstoreprobe/          # Bootstrap probe before mounting backends
│   │       └── clients/                  # Client registry (per-IP tracking)
│   │
│   ├── health/                           # Health-check primitives + wrappers
│   ├── identity/                         # ID mapping + resolver + Kerberos provider
│   │
│   └── metadata/                         # Metadata service + store contract
│       ├── service.go, interface.go      # MetadataService (façade)
│       ├── store.go                      # MetadataStore interface
│       ├── types.go, errors.go           # File, FileAttr, FileHandle, ExportError
│       ├── auth_identity.go, auth_permissions.go, authentication_test.go
│       ├── file_create.go, file_modify.go, file_remove.go, file_helpers.go, file_types.go
│       ├── directory.go, io.go, validation.go
│       ├── object.go, pending_writes.go  # Content-addressed coordination
│       ├── rollup_store.go, synced_hash_store.go
│       ├── unified_view.go, lock_exports.go
│       ├── tx_context.go, cookies.go     # Transaction context, READDIR cookies
│       ├── notify_dir_change_parent_key_test.go
│       ├── acl/                          # ACL evaluation, inheritance, GENERIC expansion
│       ├── backup/                       # Metadata-store envelope for CAS snapshots
│       ├── errors/                       # ExportError catalog
│       ├── lock/                         # Locks: BR, leases, oplocks, delegations, grace
│       ├── store/                        # MetadataStore implementations
│       │   ├── memory/                   # Ephemeral in-memory store
│       │   ├── badger/                   # BadgerDB persistent store
│       │   └── postgres/                 # PostgreSQL distributed store
│       └── storetest/                    # Cross-store conformance suite
│
├── internal/                             # Private implementations
│   ├── adapter/                          # Wire-protocol implementations
│   │   ├── common/                       # Shared adapter helpers
│   │   │   ├── resolve.go                # Handle resolution
│   │   │   ├── read_payload.go, write_payload.go, copy_payload.go
│   │   │   ├── cache_invalidator.go
│   │   │   ├── errmap.go, content_errmap.go, lock_errmap.go
│   │   ├── pool/                         # Adapter buffer pool (4K/64K/1M tiers)
│   │   ├── nfs/                          # NFSv3 + NFSv4 + Mount + NLM + NSM + portmap
│   │   │   ├── dispatch.go, dispatch_nfs.go, dispatch_mount.go
│   │   │   ├── connection.go, helpers.go
│   │   │   ├── rpc/, xdr/, types/, middleware/
│   │   │   ├── auth/                     # AUTH_UNIX + RPCSEC_GSS
│   │   │   ├── mount/                    # Mount protocol handlers
│   │   │   ├── nlm/                      # Network Lock Manager
│   │   │   ├── nsm/                      # Network Status Monitor
│   │   │   ├── portmap/                  # Portmapper
│   │   │   ├── v3/                       # NFSv3 procedure handlers
│   │   │   │   └── handlers/             # One *.go + codec + tests per procedure
│   │   │   └── v4/                       # NFSv4.0 + 4.1 handlers
│   │   │       ├── handlers/             # Compound op handlers
│   │   │       ├── attrs/, types/, state/, pseudofs/
│   │   │       └── v41/                  # SESSION-related extensions
│   │   └── smb/                          # SMB2/3 protocol
│   │       ├── dispatch.go, framing.go, response.go, helpers.go, compound.go
│   │       ├── crypto_state.go, conn_types.go, hooks.go
│   │       ├── header/, rpc/, types/
│   │       ├── auth/                     # SMB auth state
│   │       ├── kdf/                      # SMB3 KDF
│   │       ├── session/                  # Session + channel + credit + sequence window
│   │       ├── signing/                  # HMAC + CMAC + GMAC signers
│   │       ├── encryption/               # AES-CCM/GCM encryptors + middleware
│   │       ├── smbenc/                   # Encoding helpers
│   │       ├── lease/                    # Lease manager + notifier
│   │       └── v2/                       # Procedure handlers
│   │           └── handlers/             # 88 files — one per SMB2 command + tests
│   │
│   ├── auth/                             # Auth services (NTLM, SPNEGO, GSS wrap, replay)
│   │   └── kerberos/                     # Kerberos service wiring
│   ├── bench/                            # Internal bench helpers
│   ├── bytesize/                         # "1MB", "512KB" parser
│   ├── cli/                              # Shared CLI utilities
│   │   ├── output/                       # table / json / yaml
│   │   ├── prompt/                       # confirm, input, password, select
│   │   ├── credentials/                  # Multi-context credential store
│   │   ├── health/                       # Health display
│   │   └── timeutil/
│   ├── controlplane/                     # REST API handler implementations
│   │   └── api/                          # handlers, middleware, JWT
│   ├── logger/                           # Structured logger (slog-based)
│   ├── mfsymlink/                        # MFSYMLINK support
│   ├── pathutil/                         # Path expansion helpers
│   └── sysinfo/                          # Per-OS sysinfo (darwin/linux/windows)
│
├── test/                                 # Test suites outside packages
│   ├── e2e/                              # End-to-end tests (real NFS/SMB mounts)
│   │   ├── framework/                    # Server startup, containers, fixtures
│   │   ├── helpers/                      # CLI runner, login, unique naming
│   │   ├── fixtures/
│   │   ├── run-e2e.sh
│   │   ├── BENCHMARKS.md
│   │   └── *_test.go                     # Per-feature suites (matrix, ACLs, lease, etc.)
│   ├── edge/                             # Edge / fuzz-adjacent suites
│   ├── integration/
│   │   ├── kerberos/
│   │   └── portmap/
│   ├── nfs-conformance/                  # pjdfstest / RFC suite runner
│   ├── smb-conformance/                  # smbtorture / WPTS runner
│   └── posix/                            # POSIX compliance tests
│
├── docs/                                 # User + developer documentation
│   ├── ARCHITECTURE.md, CONFIGURATION.md
│   ├── NFS.md, SMB.md, ACLS.md, ENCRYPTION.md
│   ├── CLI.md, CONTRIBUTING.md, IMPLEMENTING_STORES.md
│   ├── FAQ.md, SECURITY.md, TROUBLESHOOTING.md
│   ├── RELEASING.md, BENCHMARKS.md
│   ├── BLOCKSTORE_MIGRATION.md, WINDOWS_TESTING.md
│   └── assets/
│
├── bench/                                # External benchmark harness (Pulumi)
│   ├── infra/                            # Pulumi stacks (Scaleway) + scripts
│   └── tools/, runner/, …
│
├── k8s/                                  # Kubernetes operator
│   └── dittofs-operator/
│       ├── api/, internal/, cmd/, config/, chart/, utils/
│
├── monitoring/                           # Prometheus + Grafana configs
├── .planning/                            # GSD planning artifacts (NOT shipped)
├── .github/workflows/                    # CI: unit, lint, e2e, smb/nfs/posix conformance, …
├── CLAUDE.md                             # Project rules for Claude Code
├── README.md, LICENSE
├── go.mod, go.sum
├── Dockerfile, Dockerfile.goreleaser
├── docker-compose.yml
├── flake.nix
├── .golangci.yml, .markdownlint.yaml, .goreleaser.yml
└── install.sh, uninstall.sh
```

## Directory Purposes

**cmd/dfs/**, **cmd/dfsctl/:**
- Two Cobra-based binaries: server (`dfs`) and remote CLI (`dfsctl`).
- All logic in `commands/`; entry points are thin.

**pkg/:**
- Stable surfaces. Consumed by adapters, runtime, and (sparingly) external integrations.
- Key contracts: `pkg/blockstore/blockstore.go`, `pkg/metadata/store.go`, `pkg/adapter/adapter.go`.

**internal/:**
- Wire-protocol implementations, internal CLI helpers, OS-specific code.
- `internal/adapter/{nfs,smb}/` contain the bulk of protocol code.

**test/:**
- E2E, conformance, integration, edge suites. Often gated by build tags or env.

**docs/:**
- User and contributor docs. Maintained manually.

**bench/, monitoring/, k8s/:**
- Auxiliary tooling for benchmarking, observability, K8s operator.

## Key File Locations

**Entry points:**
- `cmd/dfs/main.go` → `cmd/dfs/commands/root.go` → `start.go`
- `cmd/dfsctl/main.go` → `cmd/dfsctl/commands/root.go`

**Core composition:**
- `pkg/controlplane/runtime/runtime.go` — Runtime composing 6 sub-services
- `pkg/controlplane/runtime/init.go` — Bring-up from persistent store
- `pkg/controlplane/runtime/share.go` — Per-share resources + root handle
- `pkg/controlplane/runtime/blockstore_init.go` — Per-share blockstore wiring

**Metadata:**
- `pkg/metadata/service.go` — Façade over per-share `MetadataStore`s
- `pkg/metadata/store.go` — Store contract; conformance suite at `storetest/`

**Block storage:**
- `pkg/blockstore/blockstore.go` — Unified `BlockStore` interface
- `pkg/blockstore/engine/engine.go` — Local + remote + syncer + GC coordinator
- `pkg/blockstore/local/fs/fs.go` — Append log + rollup CAS

**Protocol dispatch:**
- `internal/adapter/nfs/dispatch.go` — NFS RPC routing + AUTH_UNIX extraction
- `internal/adapter/smb/dispatch.go` — SMB2 command routing
- `internal/adapter/smb/handlers/` — 88 handler files

**Locks / leases / delegations:**
- `pkg/metadata/lock/` — byte-range, leases, oplocks, delegations, grace, reclaim

**Adapter shells:**
- `pkg/adapter/nfs/adapter.go` — NFS server lifecycle
- `pkg/adapter/smb/adapter.go` — SMB server lifecycle

## Naming Conventions

**Files:**
- Co-located tests: `foo.go` + `foo_test.go`
- Codec / serialization helpers next to handler: e.g. `read.go` + `read_codec.go`
- One file per RPC/SMB procedure in `internal/adapter/{nfs/v3,smb}/handlers/`
- Per-OS suffixes: `_unix.go`, `_windows.go`, `_darwin.go`, `_linux.go`

**Packages:**
- Lowercase, no underscores
- Implementation subdirs named for backend: `memory/`, `badger/`, `postgres/`, `fs/`, `s3/`
- Test-only doubles in sibling packages (e.g. `pkg/metadata/storetest`)

**Types:**
- PascalCase exported (`Runtime`, `BlockStore`, `MetadataService`, `FileHandle`)
- Interfaces named for role (`BlockStore`, `BlockStoreAppend`, `MetadataStore`)

**Functions:**
- `New*` constructors, `Get*` accessors, verb-first for actions
- Receiver names short (1–3 letters)

**Errors / constants:**
- `Err*` sentinel values for expected outcomes (`ErrNotFound`, `ErrUnknownHash`, `ErrLegacyLayoutDetected`)
- Status codes per protocol live in protocol type packages (`internal/adapter/nfs/types/`, `internal/adapter/smb/types/`)

## Where to Add New Code

**New NFSv3 procedure:**
- `internal/adapter/nfs/v3/handlers/{proc}.go` + `{proc}_codec.go` + `{proc}_test.go`
- Register in `internal/adapter/nfs/dispatch_nfs.go`

**New NFSv4 op:**
- `internal/adapter/nfs/v4/handlers/{op}.go`
- Wire into compound dispatcher in `internal/adapter/nfs/v4/`

**New SMB2/3 command:**
- `internal/adapter/smb/handlers/{cmd}.go` + `{cmd}_test.go`
- Register in `internal/adapter/smb/dispatch.go`

**New metadata backend:**
- `pkg/metadata/store/{backend}/` implementing `pkg/metadata/store.go::MetadataStore`
- Pass `pkg/metadata/storetest` conformance suite
- Factory in `pkg/controlplane/runtime/stores/service.go`

**New blockstore backend:**
- `pkg/blockstore/{local,remote}/{backend}/` implementing `BlockStore` (+ optionally `BlockStoreAppend`)
- Pass `pkg/blockstore/blockstoretest` conformance suite
- Wire in `pkg/controlplane/runtime/blockstore_init.go`

**New control-plane resource:**
- Model in `pkg/controlplane/models/`
- GORM persistence in `pkg/controlplane/store/`
- HTTP handler in `internal/controlplane/api/handlers/`
- Client method in `pkg/apiclient/`
- `dfsctl` command in `cmd/dfsctl/commands/{resource}/`

**New CLI command:**
- `cmd/{dfs,dfsctl}/commands/...`
- Register in the resource's parent command file

**Shared adapter utilities:**
- `internal/adapter/common/` for cross-protocol helpers (cache invalidation, error mapping, payload copy)

## Special Directories

**vendor/:**
- Not committed (Go modules with `go.sum` for verification)

**.planning/:**
- GSD planning artifacts (CONTEXT, PLAN, REVIEW per phase)
- `.planning/codebase/` — These intel docs (referenced from planning, NOT from source)
- Decision IDs / phase IDs live only here, never in source comments

**bench/infra/:**
- Pulumi-managed Scaleway VMs for benchmarking (`base` + `bench` stacks)

**k8s/dittofs-operator/:**
- Standalone Go module (separate `go.mod`) for the operator + Helm chart

---

*Structure analysis: 2026-05-28*
