# Coding Conventions

**Analysis Date:** 2026-05-28

## Naming Patterns

**Files:**
- All lowercase, underscores allowed (`file_create.go`, `auth_permissions.go`).
- One file per RPC/SMB procedure under `internal/adapter/{nfs/v3,smb/v2}/handlers/`.
- Codec / wire helpers next to handler: `read.go` + `read_codec.go`.
- Tests co-located: `foo.go` + `foo_test.go`.
- Per-OS suffixes: `*_unix.go`, `*_windows.go`, `*_darwin.go`, `*_linux.go`.

**Packages:**
- Lowercase, no underscores. Domain-focused (`metadata`, `blockstore`, `controlplane`, `adapter`).
- Implementation subpackages named for backend (`memory`, `badger`, `postgres`, `fs`, `s3`).
- Test-only helpers in sibling packages with the `test` suffix (`storetest`, `blockstoretest`).

**Functions / methods:**
- Exported: PascalCase. Unexported: camelCase.
- Constructors prefixed `New`: `New`, `NewWithOptions`, `NewMemoryStore`.
- Accessors prefixed `Get` only when the operation is non-trivial. Plain field access otherwise.
- Verb-first for mutations: `WriteAt`, `RegisterAdapter`, `MarkUploaded`.

**Receivers:**
- Short, 1–3 letters (`r *Runtime`, `s *Service`, `e *Engine`).

**Variables:**
- Short in narrow scopes (`ctx`, `t`, `err`, `i`).
- Meaningful in broader scopes (`metaStore`, `shareName`, `authCtx`).

**Types:**
- Structs PascalCase (`Runtime`, `FileAttr`, `MetadataService`).
- Interfaces named for role: `BlockStore`, `BlockStoreAppend`, `MetadataStore`, `Adapter`.
- Error sentinels: `Err*` (`ErrNotFound`, `ErrUnknownHash`, `ErrLegacyLayoutDetected`).

**Constants:**
- Protocol enums: SCREAMING_SNAKE_CASE matching the spec (`NFS3_OK`, `STATUS_PENDING`, `FILE_ATTRIBUTE_ARCHIVE`).
- Internal constants: PascalCase (`DefaultShutdownTimeout`, `BlockSize`).

**Tests:**
- `Test{Subject}_{Scenario}` pattern. Spec references in test names where useful: `TestWrite_RFC1813_Stable`, `TestSession_MS_SMB2_3_3_5_5_StepOne`.

## Code Style

**Formatting:** `gofmt -s -w .` and `go vet ./...` before every push (memory: "Always run gofmt + lint before push").

**Linting:** `golangci-lint` via `.golangci.yml` v2 config.
- Enabled: `govet`, `unused`, `errcheck`, `staticcheck`, `ineffassign`.
- Explicitly disabled: `intrange` (avoids noisy modernization suggestions).

**Indentation:** Tabs (Go standard).

**Line length:** No hard limit. Prefer wrapping near 100–120 for readability.

## Imports

**Groups (separated by blank lines):**
1. Standard library
2. Third-party (`github.com/...`, `golang.org/x/...`)
3. Internal module imports (`github.com/marmos91/dittofs/...`)

**Aliases:**
- Avoid unless package name collides. Common alias: `metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"` to distinguish from `pkg/blockstore/local/memory`.

## Error Handling

**Strategy:** Semantic errors at the metadata layer, mapped to protocol codes at the adapter boundary.

**Patterns:**
- `metadata.ExportError` (`pkg/metadata/errors/`): canonical semantic errors — `ErrNotDirectory`, `ErrNoEntity`, `ErrAccess`, `ErrExist`, `ErrNotEmpty`, `ErrIO`, …
- Adapter mappers convert these to wire-level codes (`internal/adapter/common/errmap.go`, plus per-protocol mapper packages).
- Blockstore sentinels for control flow: `ErrUnknownHash`, `ErrLegacyLayoutDetected`, `ErrAlreadyExists` (CAS dedup conflict is a no-op success).
- Wrap unexpected infrastructure errors with `fmt.Errorf("... %w", err)`; never lose the root cause.

**Logging discipline (CLAUDE.md invariant 6):**
- Expected / well-formed errors: `logger.Debug(...)`.
- Unexpected errors (I/O failure, invariant breach): `logger.Error(...)`.

## Comments & Docs

**GoDoc:**
- All exported types, methods, and package-level vars/consts get doc comments.
- Package overview lives in `doc.go` for non-trivial packages (`pkg/blockstore/doc.go`, `pkg/controlplane/runtime/adapters/doc.go`, etc.).

**Non-obvious algorithms:**
- Lock ordering, transaction isolation, idempotency guarantees, and protocol invariants get explicit comments.
- Spec references inline where load-bearing (e.g., `MS-SMB2 §3.3.5.5`, `RFC 1813 §3.3.6`).

**Provenance:**
- GSD planning IDs (Phase, Decision, SNAP-*, P*-*, D-*) DO NOT appear in source code or shipped docs. They live in `.planning/` only.
- Git history (commit messages + PR descriptions) carries the audit trail.

## Function Design

**Size:** Single-responsibility. Larger handler bodies acceptable when they map 1:1 to a wire-level command and stay linear.

**Parameters:**
- `ctx context.Context` first.
- `*metadata.AuthContext` threaded through every metadata/blockstore call (CLAUDE.md invariant 2).
- File handles are `metadata.FileHandle` (opaque `[]byte`); never parsed by callers (invariant 3).

**Return values:**
- `error` last.
- Multiple returns common (`(file *File, preAttr FileAttr, err error)`).
- Named returns only when they materially aid readability of WCC / pre-op-attr patterns.

## Module Design

**Visibility:**
- `pkg/` — relatively stable surfaces consumed by `cmd/` and `internal/adapter/`.
- `internal/` — wire protocol implementations + private helpers.
- `cmd/` — binary-specific glue only.

**Cross-module boundaries:**
- New backend = new sibling package implementing the contract, plus pass the conformance suite.
- Sub-services under `pkg/controlplane/runtime/` each own their `doc.go` + `service.go` + tests.

## Concurrency Patterns

**Mutexes:** `sync.RWMutex` for read-heavy structures (`Runtime`, `Service` registries). Always `defer mu.Unlock()`.

**Atomic counters:** `sync/atomic` (`atomic.Int64`, `atomic.Bool`) for hot counters; avoid lock contention.

**Channels:** Used for signals (shutdown, sync drain), backpressure on the engine syncer, lease/oplock break dispatch. Not used as a substitute for mutex-protected state.

**Lock ordering:** Documented in `pkg/blockstore/local/fs/lockorder_test.go` and in handler files where deadlock risk exists. Lease/oplock manager has strict ordering between `Store`, `Manager`, and per-record locks.

**Race tests:** Critical packages have `*_race_test.go` / `*_norace_test.go` split via build tags (e.g. `pkg/blockstore/`). Memory: "Always run -race on lock-heavy packages."

## Protocol Layer Conventions

**Separation of concerns (CLAUDE.md invariant 1):**
- Wire protocol (`internal/adapter/{nfs,smb}/`): XDR/SMB framing, dispatch, codecs only.
- Business logic (`pkg/metadata/`, blockstore engine): permission checks, file ops, ACL evaluation, locks.

**Auth context:**
- `dispatch.go::ExtractAuthContext` (NFS) and SMB session layer build `*metadata.AuthContext`.
- Threaded RPC → handler → service → store. Squashing applied at mount in `CheckExportAccess` (invariant 2).

**File handles (invariant 3):**
- Created by metadata stores; encode share identity for runtime routing.
- Stable across restarts for persistent backends.
- Handlers never decode them.

**WRITE order (invariant 5):**
`MetadataStore.WriteFile` (perm check + WCC pre-op attrs) → `Runtime.GetBlockStoreForHandle` → `engine.WriteAt` → return updated attrs.

**Per-share blockstores (invariant 4):**
- One `*engine.BlockStore` per share. Resolve via `Runtime.GetBlockStoreForHandle(ctx, handle)`.
- Remote backends ref-counted when configs match; local storage dirs are always isolated.

## Configuration Conventions

- YAML files (`pkg/config/`) parsed via Viper + mapstructure.
- Env vars override via `DITTOFS_*` prefix (Viper auto-binding).
- `validator/v10` struct tags on `Config` and sub-configs.
- Defaults in `pkg/config/defaults.go`. JSON-schema export via `dfs config schema`.

## Documentation Conventions

- User-facing docs in `docs/` (manual, not generated).
- Architecture / planning artifacts in `.planning/` (NOT shipped, NOT referenced from source).
- README is the entry point; CLAUDE.md captures non-obvious invariants for Claude Code.

---

*Convention analysis: 2026-05-28*
