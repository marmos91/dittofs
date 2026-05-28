# Phase 42: Legacy Cleanup - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Remove DirectWriteStore interface and filesystem payload store dead code. This is a pure deletion phase — no new features, no architecture changes. Cleans up the legacy filesystem backend code path that is replaced by the two-tier Local/Remote block store model.

</domain>

<decisions>
## Implementation Decisions

### Core Removals (CLEAN-01 through CLEAN-06)
- Delete `DirectWriteStore` interface from `pkg/payload/store/store.go`
- Delete `pkg/payload/store/fs/` directory entirely (store.go + store_test.go)
- Remove `directWritePath` field, `SetDirectWritePath()`, and `IsDirectWrite()` from cache struct and methods
- Remove `IsDirectWrite()` checks from offloader (offloader.go, upload.go)
- Remove `blockfs` import and `DirectWriteStore` detection logic from `init.go`
- Remove all `if bc.directWritePath != nil` branches from cache (read.go, write.go, flush.go, cache.go)
- Keep non-direct-write code path only — it becomes the sole path

### Extended Scope (beyond CLEAN-01-06)
- Remove `"filesystem"` case from E2E test matrices (store_matrix_test.go, nfsv4_store_matrix_test.go) — 3 matrix entries removed
- Remove filesystem test helpers (temp dir creation for fs payload stores)
- Delete filesystem CRUD integration tests from payload_stores_test.go entirely
- Rewrite nfsv4_recovery_test.go to use memory stores instead of filesystem stores
- Remove `"filesystem"` case from CLI commands (dfsctl store payload add/edit)
- Update CLI help text to no longer list "filesystem" as a valid store type
- Remove "filesystem" from all comments, doc strings, and error type docs (errors.go Backend field)
- Clean all interface docs on BlockStore to only mention memory and S3

### Config Error Handling
- In init.go: explicit `case "filesystem"` returns helpful error: "payload store type 'filesystem' removed in v4.0 — use 'memory' or 's3'"
- Also keep generic `default` case for truly unknown types
- Error surfaces at startup during store creation (not config validate)
- CLI: remove `"filesystem"` case entirely — default/unknown handler rejects it

### Comment and Documentation Sweep
- Full sweep for "direct write", "directwrite", "filesystem backend", "fs store" references in comments
- Update flush.go header comments to describe current behavior (cache-only writes, offloader handles remote sync)
- Remove directWritePath field comment from cache struct
- No trace of filesystem/direct-write left in codebase

### Test Cleanup
- Fix cache_test.go `WriteDownloaded` → `WriteFromRemote` breakage from Phase 41 rename
- No new tests — pure deletion phase
- Verification: `go build ./...` && `go test ./...`
- No coverage concern — deleting code and its tests simultaneously

### Deletion Strategy
- Single plan, single atomic commit
- Commit message: "refactor(42): remove DirectWriteStore and filesystem payload store"
- No pre-check of fs/ tests before deletion
- Claude verifies all import references during execution

### Claude's Discretion
- Exact ordering of file edits within the single plan
- Whether any additional dead code surfaces during removal (follow the dependency chain)
- Minor wording adjustments on updated comments

</decisions>

<specifics>
## Specific Ideas

- User wants zero trace of filesystem/direct-write left — full comment sweep, not just code removal
- Recovery tests should be preserved by switching to memory stores, not deleted
- Explicit "filesystem removed" error in init.go but NOT in CLI (CLI just removes the case)
- Both explicit filesystem case AND generic default case in init.go switch

</specifics>

<code_context>
## Existing Code Insights

### Files to Delete
- `pkg/payload/store/fs/store.go` (377 lines) — full filesystem store implementation
- `pkg/payload/store/fs/store_test.go` (548 lines) — filesystem store tests

### Files to Edit (~15 files)
- `pkg/payload/store/store.go` — remove DirectWriteStore interface (6 lines)
- `pkg/cache/cache.go` — remove directWritePath field, SetDirectWritePath(), IsDirectWrite(), WriteAt branch
- `pkg/cache/read.go` — remove directWritePath nil check branch
- `pkg/cache/write.go` — remove directWritePath nil check branch
- `pkg/cache/flush.go` — remove directWritePath nil check branch + update header comment
- `pkg/cache/cache_test.go` — fix WriteDownloaded → WriteFromRemote
- `pkg/payload/offloader/offloader.go` — remove IsDirectWrite() check
- `pkg/payload/offloader/upload.go` — remove IsDirectWrite() check
- `pkg/controlplane/runtime/init.go` — remove blockfs import, DirectWriteStore detection, replace filesystem case with error
- `pkg/payload/errors.go` — remove "filesystem" from Backend docs
- `test/e2e/store_matrix_test.go` — remove 3 filesystem matrix entries + helper
- `test/e2e/nfsv4_store_matrix_test.go` — remove filesystem case
- `test/e2e/payload_stores_test.go` — delete filesystem CRUD tests
- `test/e2e/nfsv4_recovery_test.go` — change filesystem → memory stores
- `cmd/dfsctl/commands/store/payload/add.go` — remove filesystem case + update help
- `cmd/dfsctl/commands/store/payload/edit.go` — remove filesystem case + update help

### Established Patterns
- init.go switch statement for store type creation — add explicit error case
- E2E test matrix pattern — remove entries from slice literal
- CLI Cobra command pattern — remove case from validation switch

### Integration Points
- After this phase, only "memory" and "s3" are valid payload store types
- Phase 43 (Local-Only Mode) builds on this clean foundation
- Phase 45 (Package Architecture) moves remaining payload packages to new locations

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 42-legacy-cleanup*
*Context gathered: 2026-03-09*
