# Area 7 — Runtime / Control-plane — PR-A Audit (REVIEW.md)

**Status**: AUDIT COMPLETE + main-thread re-validated against `develop`. Awaiting PR-B triage.
**Date**: 2026-05-31.
**Scope**: `pkg/controlplane/runtime/` (core facade + six sub-services `adapters/ stores/ shares/ mounts/ lifecycle/ identity/` + `clients/`; snapshot/restore/GC orchestration; init/boot; netgroups/checkers/settings-watcher) + control-plane surface `pkg/controlplane/{api,store,models}` + `internal/controlplane/api/` + per-share registration into `pkg/metadata.MetadataService`.
**Method**: `area-audit` workflow — 6 parallel read-only sub-audits → adversarial-verify every HIGH → synthesize. 8 agents, ~1.39M tokens. Full raw output: `REVIEW.raw.md`.

---

## ⚠️ Provenance correction (READ FIRST)

The workflow ran against the **cwd checkout, which was on the stale branch `replay-rebase`** (predates merged #897/#907/#904/#909/#911), so its synthesized REVIEW had two errors I corrected by re-reading `develop`:

1. **The reported "H1" (RemoveShare leaks all 5 MetadataService maps; "RemoveStoreForShare exists nowhere") is a FALSE POSITIVE on develop.** `RemoveStoreForShare` exists — `runtime.go:389` calls `r.metadataService.RemoveStoreForShare(name)`; `service.go:266` defines it (deregisters all 5 maps + aborts grace timer). Merged #897, hardened #907. → **RESOLVED.** (Likewise its metadata-side duplicate.)
2. **The real area-7 HIGH was buried under the stale H1.** Re-validated against develop and CONFIRMED below as **H-A (BlockStore use-after-Close TOCTOU)**.

All §4/§5/§6 findings below were dumped from the raw output and the load-bearing ones re-checked against develop.

---

## 1. Summary (corrected vs develop)

| Sub-area | HIGH | MED | LOW |
|---|---|---|---|
| runtime core / routing | 1 (H-A) | 1 | 3 |
| share-lifecycle | 0 | 3 | 2 |
| sub-stores / adapters / mounts / identity | 0 | 0 | 4 |
| snapshot / restore / GC | 0 | 0 | 1 |
| api / dto / models / store | 0 | 0 | 4 |
| structural / design / resource | 0 | 0 | 3 |
| **Total (on develop)** | **1** | **~4** | **17** |

**Verdict: NEEDS-FIX (narrow) — 1 real HIGH (concurrency lifecycle), otherwise PATCH-grade. Architecture invariants CLEAN** (branch-independent, re-confirmed): single-entrypoint composition; opaque-handle routing; per-share block stores (remote ref-counted by configID via `nonClosingRemote`, local dirs isolated via injective `sanitizeShareName`); WRITE order; NO import cycles / no upward deps; REST authz present + fails-closed; DTO mapping lossless, no cross-boundary aliasing.

**Theme:** correctness backbone is genuinely strong (snapshot fail-closed hold enumeration, restore commit-marker ordering, remote-refcount unwind, layering, REST auth). Real residual clusters in **teardown/lifecycle concurrency symmetry** — removal paths racing in-flight ops — which in-process add-then-access tests never exercise.

## 2. HIGH findings

### H-A — BlockStore use-after-Close TOCTOU on RemoveShare vs in-flight WRITE/READ
`shares/service.go:1129-1146` (`GetBlockStoreForHandle`) vs `:784-811` (`RemoveShare`). **What:** `GetBlockStoreForHandle` returns `share.BlockStore` and its `defer s.mu.RUnlock()` releases the lock *before the caller uses the store* — the WRITE/READ data path then operates on `bs` with **no lock held**. `RemoveShare` captures `bs` under `s.mu.Lock()`, `Unlock()`s (:795), then calls `bs.Close()` (:810) **outside the lock with no in-flight drain**. A concurrent op holding the just-returned `*engine.Store` can therefore WriteAt/ReadAt a store being Closed → lost/torn writes, torn reads, or panic. **Verified on develop** (not the stale-tree artifact): both code sites read directly; #897/#907 fixed metadata-map deregistration, NOT the engine.Store lifecycle, so this is untouched. **Why:** data-path correctness + crash under share-remove-during-IO (admin removes/hot-reloads a share while a client is mid-transfer). **Fix:** pin the store with an in-flight refcount/RWMutex acquired under RLock and held across the op, and have `RemoveShare` drain before `Close()`; OR make `engine.Store.Close` drain in-flight + return `ErrClosed` mapped to `NFS3ERR_STALE`/SMB equivalent. Add a `-race` test: concurrent `WriteAt` vs `RemoveShare`.

## 3. RESOLVED / refuted
- **stale-H1** RemoveShare leaks 5 metadata maps — RESOLVED on develop (`RemoveStoreForShare` runtime.go:389/service.go:266; #897/#907). Stale-tree artifact.
- **"Unauthenticated REST API"** — REFUTED: authz present + fails-closed on every mutating endpoint; JWT alg-confusion blocked; secret ≥32 enforced.
- **"DTO clone-field-drop/aliasing"** — REFUTED: responses build fresh slices; security fields `json:"-"`; mapping lossless.

## 4. MED findings (re-validated vs develop)
- **M1 — register/remove TOCTOU resurrects `lockManagers[share]` via lazy getter** (`metadata/service.go:130-197` + `:346-401`). Even WITH `RemoveStoreForShare` (#897/#907), if `RemoveShare` deregisters metadata while the share is still in the shares registry, an in-flight op calling `GetLockManagerForShare`/`ForHandle` can re-insert `lockManagers[share]` AFTER the delete → re-introduces the leak/contamination under concurrent load. The registration side documents the analogous race; the removal side has no guard. **Fix:** remove from shares registry FIRST so lazy getters fail to resolve, or a per-share "removing" tombstone, or re-run the 5-map delete after `sharesSvc.RemoveShare` returns; add a concurrent RemoveShare-vs-GetLockManager test. *(This is the residual #897/#907 did NOT close — and it's the same root as slice-3's grace-coordinator concern.)*
- **M2 — concurrent `AddShare(sameName)` registers metadata before the registry uniqueness recheck** (`shares/service.go:368-383`). Phase-3 `RegisterStoreForShare` unconditionally replaces `s.stores[name]` (`metadata/service.go:139`) before the Phase-4 write-locked duplicate recheck (:377). Two racers both register; the loser's `cleanupShare` releases its remote ref but does NOT deregister its metadata store → `s.stores[name]` may point at the loser's store while the registry exposes the winner. Masked today only because `GetMetadataStore` returns a shared pointer. **Fix:** reserve the registry name under the write lock before the side-effecting register; loser's cleanup also deregisters; concurrent-AddShare test.
- **M3 — `CheckNetgroupAccess` returns `(false,nil)` on share-lookup failure** (`netgroups.go:120-124`) — config drift (unknown/renamed share) is indistinguishable from a legit netgroup deny; zero diagnostic signal. **Fix:** log Debug + return wrapped `shares.ErrShareNotFound` on the share-miss branch.
- **M4 — `RemoveShare` partial-failure asymmetry** (the broader case behind H-A): if `bs.Close()` errors mid-teardown, registry/snapshot-dir state can be left half-removed. **Fix:** ordered best-effort teardown continuing past individual errors. *(Co-fix with H-A.)*

## 5. LOW findings (17, terse — from raw, re-validate at fix time)
**runtime/routing:** netgroup global DNS cache + latent `matchHostname` nil-deref (`netgroups.go:156,188-196`); `LoadSharesFromStore` warn-skips failed shares → silent partial server (`init.go:219-235`); implicit two-step boot ordering unenforced (`init.go`).
**share-lifecycle:** settings_watcher callbacks no recover/timeout — a panic kills all hot-reload (`settings_watcher.go:252-258`); `GetShare` returns live `*Share` by pointer → torn read during `UpdateShare` (`service.go:816-853,968-977`).
**sub-stores/adapters/mounts/identity:** SMB never records mounts (tracker NFS-only despite "unified" doc); two parallel squash impls can drift (`identity/service.go:29-64` vs `metadata/auth_identity.go:254`); `CloseMetadataStores` swallows close errors (`stores/service.go:81-95`); `EnableAdapter` errors when already-running (breaks idempotent retry).
**snapshot/GC:** `DeleteSnapshot` deletes DB row before dir wipe → orphaned manifest on wipe-failure, no retry handle (`snapshot.go:1551-1580`); fix = RemoveAll before row delete.
**api/store:** pprof + `/api/v1/grace` unauthenticated when enabled (`api/router.go:71-88,99-102`); admin password-reset leaves user `MustChangePassword=false` (policy, not defect); snapshot show URL share-name → `os.Stat`/`Open` without `..`/sep rejection (`internal/controlplane/api/handlers/snapshot.go:70,244-256` — admin-gated, low exploit, lacks defense-in-depth); netgroup in-use check uses substring `LIKE` on config JSON (`store/netgroups.go:42-50`).
**structural:** deprecated `NFSClientProvider`/`SetNFSClientProvider` shims vs delete-eagerly policy (`runtime.go:724-728`); `openPostgresAtSchema` permanent stub **leaking "Plan 04"/"Plan 06" GSD IDs in a runtime error** (`stores/service.go:334-343` — violates no-plan-IDs convention, + postgres schema-scoped restore non-functional); runtime.go facade 777 lines (justified, run staticcheck U1000 for dead exports).

## 6. Verified-correct (checked, no finding — branch-independent)
Single-entrypoint composition; opaque-handle decode-for-routing-only; `AddShare` 4-phase (registry insert deferred until metadata+BlockStore init succeed; `cleanupShare` unwinds; dup recheck under write lock); remote-store refcount correct under concurrent same-configID (double-checked locking, no double-free/under-count); `nonClosingRemote` ownership; local-dir isolation (`sanitizeShareName` injective via `url.PathEscape`); `DisableShare`/`EnableShare` idempotent+crash-consistent; `RegisterStoreForShare` builds+recovers on local var before publish, aborts unpublished grace timer on race (registration-side sound); no import cycles / no upward deps / runtime doesn't import api; every goroutine has owner+stop (settings_watcher, clients sweeper, adapter Serve, snapshot rooted at runtimeCtx drained-first); identity LIVE + squash correct (fresh Identity copy, not aliased); mount tracker locking correct (deep copies); adapters lifecycle (buffered errCh, bounded stop); lifecycle shutdown ordering deliberate+bounded; **snapshot hold ground-truth = on-disk manifest, any stat/stream error ABORTS GC mark (fail-closed) — the data-loss-prevention property**; restore `ResetLocalState` before Reset+Restore, pre-restore safety snap holds all blocks across Reset window, durable restore marker = commit point, idempotent rollback; REST authz fails-closed + JWT alg-confusion blocked + no secret leakage (`json:"-"`); store atomicity (gorm tx, conditional UPDATE + RowsAffected vs TOCTOU, GORM bool-default defeated); `go build`/`go vet ./pkg/controlplane/...` exit 0.

## 7. Recommended PR-B shape
- **PR-B1 blockstore-lifecycle (HIGH)**: H-A — refcount/drain the `*engine.Store` so RemoveShare can't Close it under an in-flight op; + M4 ordered teardown; `-race` test. *(Touches shares/service.go + maybe engine.Store.Close.)*
- **PR-B2 lifecycle-races (MED)**: M1 (removal-side lazy-getter resurrection guard — coordinate with slice-3 grace coordinator), M2 (AddShare-sameName registry reservation), M3 (netgroup deny diagnostics).
- **Backlog issues**: LOW set — notably the `openPostgresAtSchema` plan-ID leak (convention violation) + pprof/grace auth doc + snapshot-show name validation (defense-in-depth) are worth quick separate fixes.

## 8. Coverage
Audited all six sub-areas + full control-plane surface. Provenance caveat resolved (ran on stale `replay-rebase`; H-A + MED re-validated against develop on the main thread; stale-H1/M5 invalidated). NOT separately load-tested (concurrency findings are static-analysis + code-read; H-A/M1/M2 each call for a `-race` reproducer in PR-B). Raw audit: `REVIEW.raw.md`.
