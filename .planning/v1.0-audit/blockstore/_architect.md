# Blockstore — Structural Findings (`code-architect` agent output)

**Scope**: 171 `.go` files under `pkg/blockstore/`. Branch `v1.0/area1-blockstore-audit` @ develop@22f0afd0. READ-ONLY.

---

## 1. Module Structure Assessment

### Package inventory

| Package | Files (src) | Purpose | Verdict |
|---|---|---|---|
| `pkg/blockstore` (root) | 11 | Types, interfaces, errors, constants | BLOATED — see §2 |
| `engine/` | 22 | Orchestrator + syncer + cache + GC + dedup + audit | OVER-STUFFED — see §7 |
| `local/fs/` | 22 | FSStore CAS chunk store + append log + rollup | OK — size concern |
| `local/memory/` | 3 | In-memory LocalStore for tests | OK |
| `local/` | 2 | LocalStore interface | OK |
| `remote/` | 2 | RemoteStore interface + doc | Interface duplication — §2 |
| `remote/s3/` | 3 | S3 CAS store | OK |
| `remote/memory/` | 2 | In-memory RemoteStore | OK |
| `compression/` | 5 | Zstd/LZ4 decorator over RemoteStore | OK |
| `encryption/` | 4 | AES-GCM/ChaCha decorator over RemoteStore | OK |
| `encryption/keyprovider/` | 3 | KeyProvider interface + impls | OK |
| `chunker/` | 3 | FastCDC | OK |
| `migrate/` | 5 | Legacy-to-CAS migration | OK |
| `blockstoretest/` | 3 | Conformance suite | Audit subject |

Problems concentrate in root package + `engine/`. No util grab-bags (no `util.go`, `helpers.go`, `misc.go`).

---

## 2. Collapse Candidates

### C-01 (HIGH) — Dead pair: `RemoteObjectInfo` + `RemoteStoreSweepSurface`

**File**: `pkg/blockstore/store.go:248-272`

Self-described as "documentation-only interface ... never used as a parameter — production code uses `remote.RemoteStore` directly." `RemoteObjectInfo` only referenced by `RemoteStoreSweepSurface.ListByPrefixWithMeta`. Zero callsites outside declaration. `ListByPrefixWithMeta` was pre-Walk GC API; `sweepPhase` now uses `remoteStore.Walk`. Dead residue.

**Action**: DELETE both.

---

### C-02 (HIGH) — Dead decomposition: `Store` = `Reader + Writer + Flusher`

**File**: `pkg/blockstore/store.go:161-231`

Only assertion: compile-time `var _ blockstore.Store = (*BlockStore)(nil)` in `engine/engine.go:19`. NO function in entire codebase takes `Store`/`Reader`/`Writer`/`Flusher` as parameter. Callers hold `*engine.BlockStore` concretely.

**Action**: DELETE `Reader`, `Writer`, `Flusher`, `Store`. Keep `FlushResult` + `Stats`.

---

### C-03 (HIGH) — Duplicate parse function

**Files**: `pkg/blockstore/engine/engine.go:1219-1237` + `pkg/blockstore/local/fs/readpayload.go:277-296`

Identical bodies. Comment explicitly says "duplicated to avoid circular import." Neither location depends on the other; `blockstore` root has no deps on `engine` or `local/fs`.

**Action**: Move to `pkg/blockstore/ids.go` as `ParseChunkOffset(id string) (uint64, bool)`. Delete both copies.

---

### C-04 (HIGH) — Dual not-found sentinels: `ErrBlockNotFound` + `ErrChunkNotFound`

**File**: `pkg/blockstore/errors.go:122-129`

`ErrBlockNotFound` from remote backends; `ErrChunkNotFound` from local fs. Godoc says callers match via `errors.Is` on either. Conformance suite `testGetNotFound` only tests `ErrChunkNotFound` → remote backends returning `ErrBlockNotFound` uncovered.

**Action**: Merge — flip remote backends to `ErrChunkNotFound` (matches conformance), delete `ErrBlockNotFound`.

---

### C-05 (MED) — `engine.BlockStoreStats` duplicated in `apiclient.BlockStoreStats`

**Files**: `pkg/blockstore/engine/engine.go:893-919` + `pkg/apiclient/blockstore.go:12-35`

Explicit comment: "Mirrors `pkg/blockstore/engine.BlockStoreStats`." `apiclient` already imports `engine`. 100% field-identical (24 fields, same json tags).

**Action**: `apiclient.BlockStoreStats` → type alias `= engine.BlockStoreStats`. Verify REST serialization unchanged.

---

### C-06 (MED) — FSStore constructor proliferation

**File**: `pkg/blockstore/local/fs/fs.go:313, 334, 629, 645, 653`

5 functions exist to thread one boolean (`skipSentinelCheck`):
1. `New` → `newFSStoreInternal(..., false)`
2. `NewWithOptions` → `newFSStoreWithOptionsInternal(..., false)`
3. `NewFSStoreForMigration` → `newFSStoreWithOptionsInternal(..., true)`
4. `newFSStoreInternal` (private)
5. `newFSStoreWithOptionsInternal` (private)

**Action**: Delete `New` (callers use `NewWithOptions(..., fs.FSStoreOptions{})`). Merge two internals into one. Result: 3 funcs → 1 private body.

---

### C-07 (MED) — `DeleteLog` is a one-line shim

**File**: `pkg/blockstore/local/fs/blockstore_methods.go:261-263`

```go
func (bc *FSStore) DeleteLog(ctx context.Context, payloadID string) error {
    return bc.DeleteAppendLog(ctx, payloadID)
}
```

Interface renamed; concrete didn't follow.

**Action**: Rename `DeleteAppendLog` → `DeleteLog`. Delete shim. One test callsite update.

---

### C-08 (MED) — Exported test accessors on production struct

**File**: `pkg/blockstore/engine/engine.go:859-872`

Three:
- `(bs *BlockStore) LocalForTest()` (method)
- `LocalForTest(bs *BlockStore)` (package func — duplicate)
- `(bs *BlockStore) RemoteForTesting()`

**Action**: Delete package-level `LocalForTest`. Move method forms to `engine_export_test.go` build-tag file if possible.

---

### C-09 (MED) — `RemoteStore` interface duplicates `BlockStore`

**File**: `pkg/blockstore/remote/remote.go:40-146`

Godoc: signatures "match the unified BlockStore method set verbatim ... future work retargets engine consumers onto that unified type." Future = v1.0. Adds 3 methods beyond `BlockStore`: `ReadBlockVerified`, `Close`, `HealthCheck`/`Healthcheck`.

**Action**: Embed `blockstore.BlockStore` + 3 extras:

```go
type RemoteStore interface {
    blockstore.BlockStore
    ReadBlockVerified(ctx, hash, expected) ([]byte, error)
    Close() error
    HealthCheck(ctx) error
    Healthcheck(ctx) health.Report
}
```

Removes ~50 lines of dup godoc. Conformance for `BlockStore` covers remote.

---

### C-10 (LOW) — `TransferRequest` constructors add no value

**File**: `pkg/blockstore/engine/sync_entry.go:14-38`

`NewDownloadRequest`, `NewPrefetchRequest`, `NewBlockUploadRequest` — struct literal wrappers, zero logic, 3 callsites total.

**Action**: DELETE, inline struct literals.

---

### C-11 (LOW) — Dead `Options.SweepConcurrency`

**File**: `pkg/blockstore/engine/gc.go:425`

Comment: "sweepConcurrency is accepted for backward-compatibility ... and ignored."

**Action**: DELETE field + clamping + parameter.

---

### C-12 (LOW) — `GCStats` legacy alias fields

**File**: `pkg/blockstore/engine/gc.go:116-122`

`SharesScanned`, `BlocksScanned`, `OrphanFiles`, `OrphanBlocks`, `BytesReclaimed`, `Errors` — "compat aliases." `SharesScanned`/`BlocksScanned` always 0. On REST surface (`dfsctl gc-status`).

**Action** (defer): mark `// Deprecated:`. File issue. Don't delete in PR-B without REST version bump.

---

### C-13 (LOW) — `SystemDetector` workaround interface

**File**: `pkg/blockstore/defaults.go:11-15`

Comment: "mirrors `sysinfo.Detector` but lives in `pkg/blockstore` to avoid importing `internal/` from `pkg/`." Duck-type alias workaround that's already violated 11x elsewhere.

**Action** (pending a/b/c): if (b) or (c), delete, use `internal/sysinfo.Detector` directly.

---

### C-14 (LOW) — `EvictReadBuffer` misleading name

**File**: `pkg/blockstore/engine/engine.go:994-1006`

Replaces real cache with `nullCache{}`. Name says "evict entries"; implementation permanently destroys cache subsystem. Comment: "legacy method name retained."

**Action**: Rename → `DestroyCache`. Or inline at sole caller (`pkg/controlplane/runtime/shares/service.go:1365`) + delete method.

---

## 3. Interface Leak Findings

### L-01 (MED) — `EngineFileBlockStore` impl-located, not consumer-located

**File**: `pkg/blockstore/store.go:141-153`

Declared in `pkg/blockstore` root but consumed only by `engine`-internal callers. Godoc: "Future work will eliminate the remaining call sites ... and this interface will go away."

**Action**: Evaluate `FileAttr.Blocks` path replaces it. If yes, delete. If not, move to `engine/` as unexported.

---

### L-02 (MED) — `engine.CacheInterface` exported but engine-internal

**File**: `pkg/blockstore/engine/cache.go:33-41`

Only impls in `engine` (`*Cache`, `nullCache{}`). No external refs.

**Action**: Unexport → `cacheInterface`.

---

### L-03 — `engine.MetadataCoordinator` correctly defined-where-consumed (no leak)

**File**: `pkg/blockstore/engine/coordinator.go:31-81`

Defined in `engine`, impl in `pkg/controlplane/runtime/shares/coordinator.go`. Standard Go "define where consumed" pattern. COMPLIANT.

---

### L-04 — `blockstoretest.Factory`/`AppendFactory` correctly placed (no leak)

Defined in test package, used by test backends.

---

## 4. Public-API Minimization — Zero-External-Caller Exports

| Symbol | File:line | Action |
|---|---|---|
| `RemoteObjectInfo` | `store.go:253` | DELETE |
| `RemoteStoreSweepSurface` | `store.go:269` | DELETE |
| `blockstore.Reader` | `store.go:162` | DELETE |
| `blockstore.Writer` | `store.go:181` | DELETE |
| `blockstore.Flusher` | `store.go:204` | DELETE |
| `blockstore.Store` | `store.go:215` | DELETE (keep `FlushResult`, `Stats`) |
| `engine.CacheInterface` | `cache.go:33` | UNEXPORT |
| `engine.LocalForTest` (func) | `engine.go:868` | DELETE (duplicate of method) |
| `engine.NewDownloadRequest` | `sync_entry.go:14` | DELETE inline |
| `engine.NewPrefetchRequest` | `sync_entry.go:24` | DELETE inline |
| `engine.NewBlockUploadRequest` | `sync_entry.go:33` | DELETE inline |
| `engine.GCStats.SharesScanned` | `gc.go:117` | DEPRECATE (REST compat) |
| `engine.GCStats.BlocksScanned` | `gc.go:118` | DEPRECATE (REST compat) |
| `engine.Options.SweepConcurrency` | `gc.go:126` | DELETE |
| `engine.HealthTransitionCallback` | `sync_health.go:14` | UNEXPORT |
| `engine.LoadByHashFn` | `cache.go:104` | UNEXPORT |
| `fs.ErrBlockNotFound` | `fs/errors.go` | DELETE (after C-04) |

---

## 5. `pkg/` ↔ `internal/` Imports

All `pkg/blockstore/` → `internal/` imports are to `internal/logger`:

| File | Import |
|---|---|
| `engine/engine.go` | `internal/logger` |
| `engine/syncer.go` | `internal/logger` |
| `engine/sync_queue.go` | `internal/logger` |
| `engine/upload.go` | `internal/logger` |
| `engine/fetch.go` | `internal/logger` |
| `engine/dedup.go` | `internal/logger` |
| `engine/sync_health.go` | `internal/logger` |
| `local/fs/appendwrite.go` | `internal/logger` |
| `local/fs/recovery.go` | `internal/logger` |
| `local/fs/recovery_test.go` | `internal/logger` |
| `engine/perf_bench_helpers_test.go` | `internal/logger` |

**Count**: 10 production + 1 test. No other `internal/` packages imported.

**(a)/(b)/(c) decision input**:
- (a) "no pkg/ → internal/" — all 10 production imports require remediation (move `internal/logger` → `pkg/logger` or facade).
- (b) "organizational only" — no action.
- (c) "flatten" — no action.

**Recommendation**: (b) for DittoFS as app — these imports are non-issue.

---

## 6. Layering & Cycles

```
engine/ → local/, remote/, root, chunker/, pkg/metadata
local/fs/ → root, pkg/metadata
local/memory/ → root, chunker/
remote/s3/ → root
remote/memory/ → root
compression/ → remote/
encryption/ → remote/
migrate/ → root, pkg/metadata
```

**No cycles detected.** `engine/` → `pkg/metadata` justified in-file: "GC is cross-share metadata-mark entrypoint — MUST bind `metadata.MetadataStore`/`MetadataReconciler` to enumerate live FileBlocks."

**CLAUDE.md invariant 1 cross-check**: `pkg/blockstore/engine/` imports no adapter/handler packages. No violations.

---

## 7. File-Layout Findings

### F-01 (HIGH) — `engine/engine.go` is a 1239-line god file

Contains: `Config`, `BlockStore` struct, `New` (160-line constructor with embedded closures for ObjectIDPersister/OnChunkComplete/ChunkEmitter), `Start`, `loadByHash`, `Close`, `ReadAt`, `blockRefHashes`, `computeFileSize`, `GetSize`, `Exists`, `WriteAt`, `Truncate`, `Delete`, `CopyPayload`, `Flush`, `DrainAllUploads`, `Stats`, `HealthCheck`, `Healthcheck`, `LocalForTest` ×2, `RemoteForTesting`, `ListFiles`, `EvictLocal`, `LocalStats`, `BlockStoreStats`, `GetStats`, `populateBlockCounts`, `EvictReadBuffer`, `HasRemoteStore`, `SetRetentionPolicy`, `SetEvictionEnabled`, `readAtInternal`, `ensureAndReadFromLocal`, `readLocalByHash`, `rowWithOffset`, `findRowCoveringOffset`, `parseChunkOffsetFromID`.

**Action — split**:
- `engine.go` — `BlockStore`, `Config`, `New`, `Start`, `Close`
- `readwrite.go` — `ReadAt`, `WriteAt`, `Truncate`, `Delete`, `CopyPayload`
- `flush.go` — `Flush`, `DrainAllUploads`
- `stats.go` — `BlockStoreStats`, `GetStats`, `populateBlockCounts`, `Stats`, `LocalStats`
- `health.go` — `HealthCheck`, `Healthcheck`
- `read_internal.go` — `readAtInternal`, `ensureAndReadFromLocal`, `readLocalByHash`, `rowWithOffset`, `findRowCoveringOffset`

---

### F-02 (MED) — `pkg/blockstore/store.go` mixes 4 concerns

After C-01 + C-02 shrinks to `FileBlockStore`, `EngineFileBlockStore`, `FlushResult`, `Stats`. Rename → `fileblock.go`.

---

### F-03 (LOW) — `engine/sync_entry.go` (46 lines, value type + 3 constructors)

After C-10, only `TransferRequest` + `TransferType`. Merge into `engine/types.go`.

---

### F-04 (LOW) — `local/fs/manage.go` (31 lines, 2 admin methods)

`SetEvictionEnabled` + `GetStoredFileSize`. Merge into `fs.go` or rename `admin.go`.

---

## 8. Naming Pass

### N-01 (HIGH) — `engine.Config` is too generic vs `engine.SyncerConfig`

**File**: `pkg/blockstore/engine/engine.go:22`

Asymmetry: `Config` for the BlockStore (highly specific); `SyncerConfig` for syncer. Callers write `engine.Config{...}` which looks like top-level.

**Rename**: `engine.Config` → `engine.BlockStoreConfig`. Callsites: `pkg/controlplane/runtime/shares/service.go:573` + 3 tests.

---

### N-02 (MED) — `engine.BlockStore` stutters vs `blockstore.BlockStore`

Orchestrator struct = `engine.BlockStore`; root interface = `blockstore.BlockStore`. Confusing in godoc.

**Rename proposal** (LOW impact): `engine.BlockStore` → `engine.Store` (satisfies `blockstore.Store`). Defer or accept.

---

### N-03 (MED) — `FSStore` receiver `bc` non-obvious

Pervasive in `pkg/blockstore/local/fs/`. Likely "block cache" from earlier era. Type is `FSStore`. Consistent internal convention.

**Recommendation**: ACCEPT — churn not worth clarity gain.

---

### N-04 (MED) — `HealthCheck` + `Healthcheck` split personality

Both exist on same types. `HealthCheck → error` (legacy); `Healthcheck → health.Report` (newer). Engine has both (`engine.go:810`, `:830`).

**Action**: For v1.0, `// Deprecated: use Healthcheck.` on all `HealthCheck`. Coordinate with runtime area before deletion (`syncer.HealthCheck` consumed there).

---

### N-05 (LOW) — `engine.gcRootLocks` + `gcRootLocksMu` package-level mutables

Forever-growing map; mutexes never deleted. See reviewer C-3 — bigger issue logged there.

**Action**: TODO comment.

---

### N-06 (LOW) — `local/fs/blockstore_methods.go` filename

After C-07, file = "CAS chunk interface adapter for FSStore." Rename → `cas_adapter.go` or `interface_impl.go`. Minor.

---

## 9. Module-Boundary Check

### Engine ↔ local — clean, with smell

Engine holds `local.LocalStore` interface; never type-asserts to `*fs.FSStore` in production. BUT: `engine.New` does 3 anonymous interface probes (`engine.go:157-259`):

```go
if setter, ok := cfg.Local.(interface { SetObjectIDPersister(...) }); ok { ... }
if setter, ok := cfg.Local.(interface { SetOnChunkComplete(...) }); ok { ... }
if emitter, ok := cfg.Local.(interface { SetChunkEmitter(...) }); ok { ... }
```

Engine special-cases `*fs.FSStore` behavior (setters exist only on FSStore) without naming concrete type.

**Finding**: Formalize as named optional interface `ChunkLifecycleHooks` in `local/` with 3 setters. `FSStore` satisfies; `MemoryStore` partially. `engine.New` → one type assertion.

---

### Engine ↔ remote — clean but redundant

Syncer wraps `remote.RemoteStore`. Decorator chain: `EncryptedRemote → compression.Decorator → s3.Store`. All satisfy `remote.RemoteStore`. C-09 removes redundancy without changing chain.

---

### Engine ↔ chunker — clean

`engine/dedup.go` imports `chunker.MinChunkSize` constant only.

---

### Engine ↔ metadata — justified

`engine/gc.go` + `engine/audit_state.go` import `pkg/metadata`. Both justify in-file. Acceptable.

---

### `local/fs` ↔ metadata — FSStore holds metadata types

`FSStore` fields `metadata.RollupStore`, `metadata.SyncedHashStore` (`fs.go:218-228`). Injected stores for rollup + sync paths. Justified.

---

### Compression ↔ encryption ordering — undefined contract

Decorator chain `encryption → compression → remote` wired in `pkg/controlplane/runtime/shares/service.go`. NEITHER decorator documents required ordering. Compress-before-encrypt correct (encrypt-before-compress is wrong: ciphertext incompressible). No code-level guard.

**Finding**: Add doc comment in both decorators stating required composition order.

---

## 10. PR-B Groupings

### Group B1 — Dead type deletion (store.go cleanup)
C-01 + C-02 + rename `store.go` → `fileblock.go`. Zero logic change.

### Group B2 — Sentinel de-duplication
C-04: merge `ErrBlockNotFound` into `ErrChunkNotFound`. Update remote backends + conformance.

### Group B3 — Engine god-file split
F-01: split `engine/engine.go`. Plus C-03: move `parseChunkOffsetFromID` to root. Do FIRST (B3 → C-03 coupling).

### Group B4 — Constructor consolidation in local/fs
C-06 + C-07: drop `New`, merge internals, rename `DeleteAppendLog → DeleteLog`.

### Group B5 — Export reduction
L-02 unexport `CacheInterface`. Unexport `HealthTransitionCallback`, `LoadByHashFn`. Delete pkg-level `LocalForTest`. Delete 3 TransferRequest constructors. Merge `sync_entry.go` into `types.go`.

### Group B6 — Dead config field deletion
C-11 delete `SweepConcurrency`. C-12 deprecate GCStats alias fields.

### Group B7 — Naming fixes
N-01 rename `engine.Config → engine.BlockStoreConfig`. N-04 deprecate `HealthCheck(ctx) error`. C-14 rename `EvictReadBuffer → DestroyCache`.

### Group B8 — apiclient DTO collapse
C-05: type alias.

### Coupling notes
- B3 + C-03 coupled — do B3 first.
- B2 + C-09 coupled — B2 before C-09 (once `ErrBlockNotFound` gone, remote returns single sentinel matching embedded `BlockStore`).

---

## Summary Triage

| Finding | Severity | Impact | Effort |
|---|---|---|---|
| C-01 Dead RemoteObjectInfo/SweepSurface | HIGH | Public surface | Trivial |
| C-02 Dead Reader/Writer/Flusher/Store | HIGH | Public surface | Trivial |
| C-03 Duplicate parseChunkOffsetFromID | HIGH | Removes divergence risk | Low |
| C-04 Dual not-found sentinels | HIGH | Conformance + caller simpl | Low |
| F-01 engine.go 1239-line god file | HIGH | Reviewability | Med |
| C-05 BlockStoreStats DTO duplicate | MED | Dead code | Low |
| C-06 FSStore constructor proliferation | MED | API surface | Low |
| C-07 DeleteLog shim | MED | One-liner | Trivial |
| C-08 Exported test accessors | MED | Public surface | Low |
| C-09 RemoteStore duplicates BlockStore | MED | Clarity | Low |
| L-01 EngineFileBlockStore wrong pkg | MED | Package hygiene | Med |
| L-02 CacheInterface exported | MED | Public surface | Trivial |
| N-01 engine.Config naming | MED | API clarity | Low |
| N-04 HealthCheck/Healthcheck dual API | MED | API surface | Med |
| Anonymous interface probes in engine.New | MED | Clarity | Med |
| C-10 TransferRequest constructors | LOW | Noise | Trivial |
| C-11 SweepConcurrency dead field | LOW | Dead param | Trivial |
| C-12 GCStats alias fields | LOW | REST compat | Defer |
| C-13 SystemDetector workaround | LOW | Pending a/b/c | — |
| C-14 EvictReadBuffer misleading name | LOW | Clarity | Trivial |

**Public-surface reduction estimate**: deleting C-01+C-02 removes 6 exported types from `pkg/blockstore` root; C-04 removes 1 sentinel; C-05 removes 1:1 DTO; C-08+C-10+B5 unexport/delete ~8 additional symbols. Total **~15-17 exported symbols eliminated**, ~**200 lines of dead interface/type/constructor code**.
