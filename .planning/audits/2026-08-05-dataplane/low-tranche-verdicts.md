# LOW tranche — verdict working notes

Basis for the STATUS markers written into `report.md`. Kept so the marking can be
re-checked rather than trusted.

Written 2026-08-18; the in-flight table was re-checked against `origin/develop` on
2026-08-20, after #2014-#2025 merged.

## Method

330 findings total. 171 LOW carried no STATUS marker. For each, the finding's
`Where:` file was cross-checked against the diffs of the four merged LOW sweeps
and against current `origin/develop`:

| sweep | tranche |
|---|---|
| #1996 | LOW `comments` + `slop` |
| #1997 | Dockerfile `EXPOSE` drift (split out of #1996) |
| #1999 | LOW `structure`, TRIVIAL grade |
| #2000 | LOW `bloat` |

- **116** — the finding's dimension maps to a sweep AND that sweep touched the
  file. Five sampled at random were each named explicitly in the sweep's own PR
  body; marked FIXED in the mapped PR.
- **18** — file touched, but by a *different* sweep than the dimension implies.
  Each checked individually against `origin/develop`; results below. Roughly a
  third were fixed, the rest are still open, so "file was touched" alone is not
  evidence and was not used as such.
- **37** — file untouched by any merged sweep. Ten were claimed by the then-in-flight
  PRs #2014-#2025, all since merged and re-checked below; the remainder stay open.

## The 18 dimension/file disagreements

| Finding | Verdict on develop |
|---|---|
| `common/read_payload.go` dead EOF branch | OPEN — branch still at `read_payload.go:54` |
| `engine/syncer.go` Syncer god object | OPEN — #1996 changed comments only |
| `journal/segment.go` appendTombstone/appendTruncateMarker near-duplicates | OPEN — still two ~45-line methods (`:399`, `:454`) |
| `Dockerfile.prebuilt` EXPOSE drift | FIXED in #1997 |
| `sqlite/transaction.go` redundant "Debug logging" comment (x2, duplicate finding) | FIXED in #1996 — now carry the gating rationale |
| `badger/durable_handles.go` per-index add/remove duplication | OPEN — no shared index helper |
| `badger/objects.go` retry-with-backoff duplicated | OPEN — `updateWithConflictRetry` documents the duplication rather than removing it |
| `badger/objects.go` raw ErrConflict sentinel escapes the store boundary | FIXED — mapped via `mapBadgerError` at `objects.go:179` |
| `memory/transaction.go` two divergent handle-minting paths | FIXED in #1999 — both delegate to `metadata.GenerateNewHandle` |
| `sqlite/clients.go` duplicated 9-field Scan | **OPEN — see discrepancy below** |
| `sqlite/block_record_store.go` duplicated SQL bodies | PARTIAL — `scanBlockRecord` now shared (`:242`); pool and tx bodies still separate |
| `sqlite/files.go` SetParent ctx divergence | FIXED in #1999 — tx form returns `ctx.Err()` |
| `runtime/netgroups.go` package-level DNS cache | OPEN — comments only |
| `runtime/netgroups.go` one-impl `dnsResolver` | OPEN — comments only |
| `runtime/settings_watcher.go` pollNFS/pollSMB duplication | OPEN — comments only |
| `runtime/stores/service.go` SRP violation | OPEN — comments only |
| `smb/handlers/read.go` ReadRequest.Flags decoded, never consulted | OPEN — still decoded at `read.go:98` |

## Discrepancy: #1999's body overstates its sqlite scanner work

#1999's description says it collapsed "one scanner per row shape client
registrations (9 columns) durable handles (32 columns)". Its diff contains
`sqlite/durable_handles.go` and `memory/clients.go` — not `sqlite/clients.go`.
On `origin/develop` the 9-column Scan block, including the `privBytes` copy, is
still written out verbatim in both `GetClientRegistration` (`clients.go:81`) and
`ListClientRegistrations` (`clients.go:135`).

No behaviour is wrong today — the two copies are identical. The finding is simply
not closed, and marking it FIXED on the strength of the PR body would have hidden
that. This is the reason the 18 were checked against the tree rather than against
the PR descriptions.

## The 37 in files no merged sweep touched

Ten were claimed by a PR in flight. All ten have since merged; re-checked against
`origin/develop` on 2026-08-20 rather than trusted from the PR bodies, for the reason
the sqlite discrepancy below exists.

| PR | finding | verdict on develop |
|---|---|---|
| #2016, #2017 | `lock/manager.go` LockManager bundles 30+ methods | PARTIAL — the file split landed (`byterange.go`, `break.go`, `connection.go`, `grace.go`, `delegation.go`); the `Manager` *type* still bundles them behind one mutex. #2017 declined the type split deliberately. Residue tracked as #2029. |
| #2017 | `lock/connection.go` RegisterClient hand-builds a StoreError | FIXED — now `errors.NewConnectionLimitError(adapterType, limit)`. |
| #2017 | `lock/grace.go` Operation is boolean-soup instead of an enum | FIXED — `type Operation int` with `OpNew Operation = iota` at `grace.go:37`. |
| #2018 | `memory/files.go` store-level read CRUD duplicates tx-level | FIXED — no store-level `GetFile`/`GetChild`/`GetParent` remain in `files.go`; the transaction forms are the only copy. |
| #2018 | `memory/locks.go` three lazy sub-stores double-lock with a dead inner mutex | FIXED — sub-store and shadow mutexes gone. `memoryDurableStore` deliberately keeps its lock (it is the sole guard there), and the asymmetry that exposes is filed as #2023. |
| #2020 | `file_access_checker.go` single-impl capability interface in the producer package | FIXED — `FileAccessChecker` and its `var _` assertion are gone; consumers probe `*Service` directly. |
| #2021 | `lifecycle/service.go` positional dependency injection across three methods | FIXED — collaborators arrive as a named `Deps` struct. |
| #2025 | `errors.go` dead re-export shim (x2 findings) | REFRAMED — not deleted. #2025 answered the finding by documenting the file as the package's public error surface rather than a back-compat shim, which is what the ~436 in-package call sites make it. The 407-line count stands; the "dead" premise does not. |
| #2025 | `lock_exports.go` deprecated aliases are mutable function vars | FIXED/REFUTED — the aliases are `type X = lock.X` plus `const` blocks; the only functions are thin constructors. Nothing mutable is exported. |

The remaining 27 were checked one at a time against `origin/develop`.

### Closed by work outside the four sweeps

| Finding | Evidence |
|---|---|
| `synced_hash_store_suite.go` pulls `testing` into the production build | File is gone; #1999 moved it to `storetest/`. The remaining `testing` imports under `pkg/metadata` are all in `storetest/`, which is test support by design. |
| `blockstoretest/conformance.go` godoc names a nonexistent `BlockStoreAppendConformance` | No reference to that symbol survives anywhere in `blockstoretest/`. |
| `blockstoretest/doc.go` describes `BlockStoreAppendConformance` and appendlog files | Same. |
| `lock/oplock.go` exported mutable package-level slice globals | No exported package-level slice vars remain. |
| `lock/oplock_break.go` package doc restated verbatim in every file | Only `lock/types.go` carries the package doc now. |
| `postgres/shares.go` redundant step-narration comments in CreateRootDirectory | No step-narration comments remain. |
| `adapters/service.go` `adapterEntry.ctx` write-only dead field | No `ctx` field on the struct. |

### Refuted — the finding was wrong when written

| Finding | Why |
|---|---|
| `lock/errors.go` `NewGracePeriodError` silently drops `remainingSeconds` | It is used: `"grace period active, new locks blocked (%ds remaining)"`. |
| `lock/errors.go` constructors accept diagnostic params then discard them | `blockedBy`, `current` and `max` all appear in their `Message` format strings. |

### Still open

`engine/stats.go` (duplicate pending fields; `Stats`/`GetStats` drop ctx) ·
`engine/audit_state.go` (`Delta` still assigned `int64(result.DanglingRefs)`) ·
`adapters/service.go` + `lifecycle/service.go` (`DefaultShutdownTimeout` declared twice, plus the operator's copies) ·
`discovery.go` (`DiscoveryName()` still takes no ctx) ·
`identity/service.go` (wrapped cause dropped) ·
`badger/cache.go` (`globalCacheDefaults` package global) ·
`badger/block_record_store.go` and `postgres/block_record_store.go` (tx/store bodies duplicated) ·
`postgres/transaction.go` (three bare Debug comments; `GetFile`'s 20+ column SQL duplicated) ·
`nfs/v3/handlers/write.go` (offset overflow checked twice) ·
`dfsbench/exec/ssh.go` (one-impl `Executor`) ·
`encryption/decorator.go` (legacy CAS surface in the primary file) ·
`local/fs/fs.go` (`FSStore` embeds `*journal.Store`) ·
`local/local.go` (`LocalStore` 20-method god interface).

## Two cross-backend drifts the marking surfaced

**Debug comments.** #1996 reworded sqlite's bare "Debug logging" comments into
real rationale ("gated so the id formatting is..."). The identical finding
against `postgres/transaction.go` was not swept — three bare Debug comments
remain there. Same defect, two backends, one fixed.

**Stats fields.** The finding called `PendingSyncs` and `PendingUploads`
"duplicate always-zero fields". They are no longer always zero: `stats.go:160-161`
now assigns `pendingUploads` to *both*, so the REST payload reports the upload
count under `pending_syncs` as well. The finding is still open and its premise
has moved; whether the two should carry distinct values is a question for
whoever closes it, not something the sweep decided.
