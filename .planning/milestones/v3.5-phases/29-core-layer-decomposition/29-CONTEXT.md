# Phase 29: Core Layer Decomposition - Context

**Gathered:** 2026-02-26
**Status:** Ready for planning

<domain>
## Phase Boundary

Decompose god objects into focused sub-services with interface composition, unify errors with structured types and middleware, reduce boilerplate with generics, and reorganize auth into a centralized package. All changes are pure refactoring — no behavior changes, no new features.

Scope includes: ControlPlane Store interface split, Runtime decomposition, MetadataService decomposition, PayloadService decomposition, Offloader rename/split, error/request/response middleware unification, auth centralization, file splitting, GORM/API client helpers, and comprehensive documentation updates.

</domain>

<decisions>
## Implementation Decisions

### Interface Composition Pattern (applies everywhere)
- **Narrowest possible interfaces**: every consumer accepts only the sub-interface it needs
- **Composite interfaces**: parent type embeds all sub-interfaces (e.g., `store.Store` embeds `UserStore`, `GroupStore`, etc.)
- **Composite stays**: the full composite interface remains for callers that need everything (Runtime, tests)
- **Interfaces in sub-packages**: to avoid circular imports, each sub-package defines its own interface alongside its implementation
- **Entity-grouped interfaces**: group related CRUD operations per entity (e.g., `UserStore` has all user ops), not single-method interfaces

### ControlPlane Store Decomposition
- **Flat package, renamed files**: keep `pkg/controlplane/store/` flat (no sub-packages), rename underscore files:
  - `adapter_configs.go` → methods absorbed into `shares.go` (ShareAdapterConfig is share-scoped)
  - `adapter_settings.go` → merged into `adapters.go`
  - `metadata_stores.go` → `metadata.go`
  - `payload_stores.go` → `payload.go`
  - `identity_mappings.go` → `identity.go`
- **interface.go restructured**: one giant `Store` interface becomes ~9 named sub-interfaces + composite `Store` embedding all
- **ShareAdapterConfig methods move to shares.go**: share-scoped config belongs with ShareStore, not AdapterStore
- **AdapterStore consolidates**: adapter CRUD + global adapter settings in one interface/file
- **API handlers narrowed**: each handler struct holds its specific sub-interface (e.g., `UserHandler { store store.UserStore }`)
- **Router passes full Store**: the router receives `store.Store` and passes narrow sub-interfaces to handler constructors
- **GORM helpers**: standalone generic functions (`getByField[T]`, `listAll[T]`, `createWithID[T]`) in `helpers.go` within the store package
- **API client helpers**: same pattern in `pkg/apiclient/helpers.go` (`getResource[T]`, `listResources[T]`)

### Runtime Decomposition
- **6 sub-services**: adapters, shares, stores, mounts, lifecycle, identity — each as a separate sub-package under `pkg/controlplane/runtime/`
- **Settings fold into adapters**: per-adapter settings are part of the adapters sub-service
- **Separate types, composed**: each sub-service is its own struct. Runtime composes them (holds references, delegates)
- **Sub-package interfaces**: `adapters.Service`, `shares.Service`, etc. defined alongside implementations in sub-packages
- **Runtime holds composite metadata/payload**: Runtime receives `metadata.Service` and `payload.Service` composites, passes narrow sub-interfaces to its own sub-services

### MetadataService Decomposition
- **Sub-package with delegation**: `pkg/metadata/file/` and `pkg/metadata/auth/` as sub-packages, each with its own `Service` type
- **FileService owns store reference**: constructed with metadata store, independently testable
- **AuthService same pattern**: owns its store reference, handles identity resolution and permission checks
- **Operation files within sub-packages**: `file/create.go`, `file/modify.go`, `file/remove.go`, `file/helpers.go`; `auth/identity.go`, `auth/permissions.go`
- **Composite in parent**: `pkg/metadata/` defines composite `Service` that embeds `file.Service` + `auth.Service`
- **Interface in sub-packages**: `file.Service`, `auth.Service` interfaces defined in their own packages (avoids circular imports)

### PayloadService Decomposition
- **3 sub-services**: io (read/write), offloader, gc — composed via interfaces
- **pkg/payload/io/**: read.go + write.go — handles read/write operations through cache layer. Import alias `payloadio` where stdlib `io` is also used
- **pkg/payload/offloader/**: the extracted Offloader (see below)
- **pkg/payload/gc/**: the extracted garbage collector (see below)
- **Composite in parent**: `pkg/payload/` defines composite `Service` embedding sub-interfaces

### Offloader Rename/Split
- **Rename**: TransferManager → Offloader
- **Move**: `pkg/payload/transfer/` → `pkg/payload/offloader/`
- **File split**:
  - `offloader.go` — struct, constructor, Flush(), Close(), orchestration
  - `upload.go` — upload orchestration methods
  - `download.go` — download coordination methods
  - `dedup.go` — dedup handler methods
  - `queue.go` — TransferQueue (keeps Transfer prefix — describes what it does)
  - `entry.go` — TransferQueueEntry (keeps Transfer prefix)
  - `types.go` — shared types
  - WAL replay: `recovery.go` renamed to something clearer (e.g., `wal_replay.go` or `startup_replay.go` — Claude's discretion)
- **Type naming**: keep `TransferQueue` and `TransferRequest` (Transfer prefix describes the action, not the package)
- **GC extracted**: `gc.go` moves to `pkg/payload/gc/` — standalone function, zero coupling to Offloader

### Error Unification
- **PayloadError**: structured type with performance debugging context:
  - `Op` (upload/download/dedup/gc), `Share`, `PayloadID`, `BlockIdx`, `Size`, `Duration`, `Retries`, `Backend`, `Err` (wrapped sentinel)
  - `errors.Is()` compatible via wrapped Err
  - Located in `pkg/payload/` (co-located with domain)
- **ProtocolError**: generic interface `{ Code() uint32; Message() string; Unwrap() error }` — works for both NFS (NFS3ERR_*) and SMB (NTSTATUS)
  - Located in `pkg/adapter/` (co-located with domain)
  - NFS implements as `nfsError`, SMB as `smbError`
  - Added to ProtocolAdapter interface: `MapError(err error) ProtocolError` — forces every new adapter to implement error translation
- **Unified Response**: single `Response` struct for API handlers — `{ Status int, Data any, Err error }`
- **Unified Request**: middleware parses request body into typed struct, validates before handler runs. Invalid requests get 400 before reaching handler.
- **Error middleware**: all API handlers return `*Response`. Middleware handles JSON encoding for both success and error responses. Handlers don't worry about encoding/decoding, only logic.
- **Middleware validates**: middleware parses + validates requests. Handlers trust their input.
- **API error mapping**: centralized via error middleware, not per-handler

### Auth Centralization
- **Single auth package**: `pkg/auth/` is the home for all authentication
  - `auth.go` — DittoFS Authenticator interface, AuthResult, errors (our abstractions)
  - `identity.go` — DittoFS identity model, IdentityMapper interface
  - `kerberos/` — Kerberos Provider, keytab management (provider.go, keytab.go)
- **Provider chain**: `AuthProvider` interface with `CanHandle(token) bool` + `Authenticate()`. Authenticator chains providers: DittoFS always present, Kerberos if configured. Config-driven, no type assertions.
- **Identity mapping**: auth owns resolution logic (principal → DittoFS user), controlplane owns the data (IdentityMappingStore). Auth receives `IdentityMappingStore` via dependency injection.
- **IdentityMapper interface** in `pkg/auth/identity.go`: shared interface for protocol-specific identity mapping
- **Adapter interface composition**: `ProtocolAdapter` embeds `auth.IdentityMapper` — each adapter must implement protocol-specific identity mapping
  - NFS: maps AUTH_UNIX UIDs, AUTH_NULL, RPCSEC_GSS/Kerberos (supports v3 + v4.0 + v4.1 auth mechanisms)
  - SMB: maps NTLM sessions, SPNEGO/Kerberos
- **TranslateAuth**: adapter interface method to convert central auth result to protocol-specific auth context
- **Identity is generic**: the `Identity` type must represent any auth outcome (Unix creds, Kerberos principal, anonymous, NTLM session) without protocol ties
- **NFS multi-version**: NFS IdentityMapper must handle all auth flavors across v3/v4.0/v4.1
- **Kerberos is shared**: both NFS and SMB can use the Kerberos provider

### Testing Strategy
- **In-memory implementations, not mocks**: tests use real in-memory store implementations, not mock objects
- **Conformance suite**: `pkg/metadata/storetest/` for metadata store conformance (memory, badger, postgres must all pass same tests). Not needed for controlplane (single GORM impl).
- **Tests follow code**: when code moves to sub-packages, tests move with it. Each sub-package is self-contained with its own test files.
- **Test file naming**: mirrors source files — `create.go` → `create_test.go`, `modify.go` → `modify_test.go`
- **Existing tests split**: existing unit tests decomposed alongside the code they test
- **Verification**: unit tests per layer, full E2E suite at the end

### Naming Conventions
- **Service for logic, Store for data**: sub-packages use `Service` for business logic types, `Store` for data access types
- **File pattern in sub-packages**: `service.go` (interface + struct + constructor) + operation files (create.go, modify.go, etc.)
- **No underscore files**: rename underscore-separated files (adapter_configs.go → absorbed, metadata_stores.go → metadata.go, etc.)
- **Composite names**: Claude's discretion per component (e.g., `runtime.Runtime` may stay as-is since it's a well-known name)

### Migration Approach
- **Layer by layer, bottom-up**: start with leaf packages (errors, helpers), then stores, then services, then runtime. Each layer compiles before moving up.
- **One PR per layer**: smaller, reviewable PRs. Each compiles and passes tests.
- **Clean break**: when code moves, all importers update in the same PR. No temporary aliases or re-exports.
- **Unit tests per layer, E2E at the end**: fast iteration with unit tests. Full E2E validation after all layers complete.

### Documentation
- **Full docs update**: ARCHITECTURE.md, CLAUDE.md, per-package doc.go, and all existing docs referencing moved code
- **Docs with each PR**: documentation updates included in each code change PR, not a separate pass
- **Godoc**: comprehensive pass on interfaces, service types, and constructors. Skip trivial getters.
- **Per-package doc.go**: each new sub-package gets a doc.go explaining package purpose, key types, and usage

### Claude's Discretion
- Package location for ControlPlane Store sub-interfaces (same flat package vs sub-packages — evaluate based on Go conventions and actual interface sizes)
- WAL replay file naming (wal_replay.go vs startup_replay.go)
- Composite interface naming (runtime.Runtime vs runtime.Service)
- Exact sub-interface groupings if methods don't cleanly separate
- Godoc wording and examples
- Generic helper function signatures

</decisions>

<specifics>
## Specific Ideas

- "I don't like underscore files — when adding an underscore there's an opportunity to create a module"
- "Single handlers should not worry about encoding/decoding, only about logic"
- "I don't like mocks, I prefer in-memory implementations for tests"
- Pattern reference: same interface composition pattern should apply consistently across ControlPlane Store, MetadataService, PayloadService, and Runtime
- Auth pattern: "central clean abstraction, each adapter translates auth for their own protocol-specific implementation"
- Identity must be generic enough to cover all auth scenarios (Unix creds, Kerberos, anonymous, NTLM)
- NFS supports multiple auth mechanisms across versions — IdentityMapper must handle all of them

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 29-core-layer-decomposition*
*Context gathered: 2026-02-26*
