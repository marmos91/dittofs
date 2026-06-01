> **⚠️ STALE-TREE CORRECTION (2026-06-01).** This pass ran against a stale local-develop checkout (`e16f0b01`) MISSING merged PRs #918/#919/#920/#921. Re-verified against real `origin/develop` (`db4328b4`):
> - **H-2 (= round-1 H-A) BlockStore use-after-Close — REFUTED:** #918 (`8188a08b`) IS in develop's ancestry; the close-gate exists. REVIEW2's `git merge-base --is-ancestor 8188a08b HEAD → exit 1` ran against the STALE tree and is WRONG.
> - **H-1 AddShare arms the whole NFSv4 server into reboot-grace → ~90s OPEN/LOCK stall for live clients — CONFIRMED.** #919 slice-3 fixed only the symmetric *RemoveShare-ends-grace-early* direction; the *AddShare-arms-grace* direction is untouched and genuinely open.
> **Net valid HIGH on real develop: 1 (H-1).**

# Area 7 — Runtime / Control-plane — ROUND 2 Audit (REVIEW.md)

**Status**: AUDIT COMPLETE. ROUND 2 (missed-findings + integration lens). Awaiting PR-B triage.
**Date**: 2026-06-01.
**Scope**: `pkg/controlplane/runtime/` (core facade + six sub-services `adapters/ stores/ shares/ mounts/ lifecycle/ identity/`) + control-plane surface `pkg/controlplane/{api,store,models}` + `internal/controlplane/api/` + the integration seams into `pkg/metadata.MetadataService`, `pkg/adapter/nfs` grace coordinator, `pkg/blockstore/engine`, and the REST authz layer.
**Method**: `area-audit` workflow, round-2 lens — 4 parallel read-only sub-audits focused on (1) cross-area/cross-component CONTRACT seams, (2) error & failure paths, (3) concurrency under load, (4) re-verification of round-1 HIGHs on current `develop`. Every HIGH adversarially verified; refute-by-default.
**Cross-check refs**: Round-1 `REVIEW.md` (all round-1 findings treated as KNOWN, not re-reported). Canonical impls consulted where protocol-relevant (Linux `fs/nfs` grace semantics, Samba grace/reclaim). Re-verified against `develop` HEAD `e16f0b01`.

---

## ⚠️ Provenance correction (READ FIRST)

The audit context handed to round-2 asserted **"Round-1 H-A fixed #918 (engine close-gate)."** This is **incorrect for `develop`.** Two independent sub-audits verified via `git merge-base --is-ancestor 8188a08b HEAD` (exit 1) that **#918 (commit `8188a08b`) lives only on branch `v1.0/runtime-lifecycle-races` and is NOT in `develop`'s ancestry.** Round-1 H-A (BlockStore use-after-Close TOCTOU) **STILL REPRODUCES unchanged on `develop`** — the close-gate fix has not landed. This is re-raised below as a verified HIGH so any release cut from `develop` is not falsely believed clean.

One round-1 sub-audit's `verifiedCorrect` list claimed H-A was "real-fixed on develop" (engine.go closeMu). That claim was made against a checkout that included `v1.0/runtime-lifecycle-races`, not `develop`. The `develop`-anchored verification (HEAD `e16f0b01`) supersedes it.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---|---|---|---|
| share-lifecycle-races | 0 | 1 | 0 | 0 |
| grace / REST integration | 1 | 1 | 0 | 0 |
| REST authz (deep) | 0 | 1 | 0 | 0 |
| DTO + sub-service seams | 1* | 2 | 3 | 0 |
| **Total (round-2 net, on develop)** | **2** | **5** | **3** | **0** |

\* The DTO-seam HIGH is the **re-verified round-1 H-A** (BlockStore use-after-Close), confirmed still-open on `develop`. It is counted once as a HIGH here because the round-2 context wrongly assumed it shipped; the genuinely *new* round-2 HIGH is the grace-coordinator liveness block (H-1 below).

**Verdict: NEEDS-FIX (narrow but real).** Two HIGH integrity holes: (H-1) a routine admin action (runtime `AddShare`) freezes NFSv4 new-state creation server-wide for ~90s — a self-inflicted liveness block; and (H-2, ex round-1 H-A) an unmitigated BlockStore use-after-Close data-corruption/crash path that the fix has not yet reached `develop`. Everything else is PATCH-grade (operability, error-shape drift, dead surface).

**Architecture invariants HOLD.** Single-entrypoint composition, opaque-handle routing, per-share block stores, WRITE ordering, REST fails-closed authz, lossless DTO mapping — all re-confirmed clean on the integration seams. The defects are at lifecycle/grace boundaries and error-mapping seams, exactly the cross-component class round-1 (auditing each area in isolation) could not see.

---

## 2. HIGH findings (ranked by blast radius)

### H-1 — Runtime `AddShare` drives the entire NFSv4 server into grace, blocking OPEN/LOCK for every connected client (~90s)
`pkg/adapter/nfs/grace_coordinator.go:53-64` + `pkg/metadata/service.go:181-186` + `initLockManagerFromStore` `service.go:341-391`.

**What:** `POST /api/v1/shares` at runtime → `runtime.AddShare` → `shares/service.go:377` `RegisterStoreForShare`. For any NEW share no lock manager exists yet, so `service.go:168-186` runs `initLockManagerFromStore`. A freshly-created store's clean-shutdown marker is the Go zero value `false` (`memory/locks.go:39 cleanShutdown bool`; `lock/store.go:276-286` documents the fail-safe `false` default for a "fresh store … never written"; only graceful `Close` writes `true` — `postgres/store.go:239`, `badger/store.go:579`). So `service.go:347` `unclean=true` and `service.go:391` `enterGrace = unclean || len(persisted)>0` is TRUE even with zero persisted locks. That fires `graceCoordinator.OnLockGraceStart` (`service.go:183-185`). The coordinator (`grace_coordinator.go:53`) guards only on `sm.IsInGrace()`; when v4 grace is not already active it calls `sm.StartGracePeriod(sm.GetConfirmedClientIDs())` (`:60-63`). At runtime `GetConfirmedClientIDs()` (`manager.go:967`) returns the **LIVE** confirmed v4 client set (`clientsByName`), non-empty whenever any NFSv4 client is mounted — **not** an empty boot roster. `StartGrace` with a non-empty set sets `active=true` (`grace.go:134-167`), so `CheckGraceForNewState` returns `NFS4ERR_GRACE` for every `OPEN(CLAIM_NULL)` (`open.go:177`) and `LOCK` (`manager.go:1710,1861`) from every connected client until the hard backstop timer lifts at `DefaultLockGracePeriod` (~90s).

**Why:** A routine admin action freezes new-state creation for the whole NFSv4 server and every existing client — a self-inflicted DoS/liveness block that recovers no data (the new share has no prior clients to reclaim). **Corollary (same missing-refcount root):** removing any in-grace share at runtime fires `OnLockGraceEnd → sm.ForceEndGrace()` (`service.go:286-288`) unconditionally with no refcount (`grace_coordinator.go:68-73`), *prematurely* ending v4 grace while another share's window is still open. Round-1 explicitly reasoned this was "latent, becomes relevant only alongside v4 client persistence" (`grace_coordinator.go:38-42`) because it only considered the **boot** path where `clientsByName` is empty; the **runtime-AddShare-with-live-clients** path defeats that no-op assumption with zero v4 persistence.

**Fix:** Gate the coordinator coupling so it arms v4 grace only at boot/recovery, not on every `RegisterStoreForShare` — skip `OnLockGraceStart` for shares added after the server is already serving (a runtime-added share has no pre-restart clients to reclaim), or pass `GetConfirmedClientIDs()` only when those clients are a genuine prior-boot reclaim roster. At minimum, refcount active lock-manager grace windows in the coordinator (the doc at `grace_coordinator.go:42` already prescribes this) so one runtime `AddShare` cannot pull a server past recovery into grace, and one `RemoveShare` cannot end it early. Add a `-race` test: server live with a confirmed v4 client → `AddShare` → assert `CheckGraceForNewState` stays nil.

*Verifier rationale (HIGH retained):* the full 6-step chain was confirmed at every cited location; the coordinator is wired at runtime (`adapter.go:424-425`), not just boot; no guard elsewhere neutralizes it. The boot path is correctly a no-op (`clientsByName` empty), which is exactly why round-1 missed it.

### H-2 (= round-1 H-A) — BlockStore engine.Store use-after-Close TOCTOU, STILL OPEN on `develop` (#918 not merged)
`pkg/controlplane/runtime/shares/service.go:809-813` (`RemoveShare` `bs.Close()` outside lock) vs `:1129-1146` (`GetBlockStoreForHandle`); `pkg/blockstore/engine/engine.go:406-424` (`Store.Close`, no drain).

**What:** The round-2 context claimed this was fixed by #918. It is not: `git merge-base --is-ancestor 8188a08b HEAD` → exit 1; #918 exists only on `v1.0/runtime-lifecycle-races`. On `develop` HEAD `e16f0b01` there is **no `closeMu` / `closed` flag / `enter()` gate anywhere in `pkg/blockstore/engine`** (grep for `enter()`/`closeMu` → zero non-test hits; the `engine.Store` struct `engine.go:69-102` has no lifecycle-gate field). `GetBlockStoreForHandle` (`service.go:1135-1136`) returns `share.BlockStore` and its `defer s.mu.RUnlock()` releases the lock **before** the WRITE/READ data path touches the store. `RemoveShare` (`service.go:791-795`) captures `bs` under `Lock`, `Unlock`s at `:795`, then calls `bs.Close()` at `:810` with **no in-flight drain**; `Runtime.RemoveShare` (`runtime.go:380`) only drains snapshot goroutines (`cancelAndWaitInFlightSnaps`), not client WRITE/READ. `engine.Store.Close` tears down syncer/local/remote unconditionally. A concurrent `WriteAt`/`ReadAt` on the just-returned `*engine.Store` can race a `Close` → torn/partial write, torn read, or panic.

**Why:** Real, unmitigated data-loss/corruption + crash + liveness path on the shipping branch, triggered whenever an admin removes/hot-reloads a share (or restore's `ResetLocalState`/teardown runs) mid-transfer. The fix exists but has not landed, so any release cut from `develop` ships the bug.

**Note — the `ErrStoreClosed` red herring:** the `ErrStoreClosed` gate that exists in `pkg/blockstore` is on the LOCAL stores (`memory`/`fs`) and the `Syncer.closed` flag (`syncer.go:79`) — it does **not** protect the `engine.Store` lifecycle that H-A is about. The local `FSStore.closedFlag` (`fs.go:836`) is only partial: `FSStore.Close` (`fs.go:855`) acquires only `logsMu`, never the per-file `mu` an in-flight `AppendWrite` holds across `writeRecord`/`lf.f.Close` (`appendwrite.go:329,365`), so a concurrent write can still hit a closing fd; syncer/remote `Close` have no in-flight protection at all.

**Fix:** Merge #918 into `develop`, or re-apply the engine-internal close-gate: `closeMu sync.RWMutex` + `closed` flag, `enter()`/`RUnlock` around every public data op (`WriteAt`/`ReadAt`/`Flush`/`Truncate`/`Delete`/`CopyPayload`/`GetSize`/`Exists`/`Drain*`/`ResetLocalState`/`EvictLocal`); `Close` takes the write lock to drain; return `ErrStoreClosed` mapped to NFS `*_STALE` / SMB `STATUS_FILE_CLOSED`. Add the `-race` reproducer (`WriteAt` vs `RemoveShare`/`Close`). **Verify with `git merge-base --is-ancestor` before declaring fixed.**

---

## 3. Triage downgrades / RESOLVED

No round-2 HIGH was refuted — both were adversarially verified and retained. The single triage correction is **the audit-context premise itself**: "Round-1 H-A fixed #918" is RESOLVED-as-FALSE (see Provenance correction + H-2). #918's close-gate is correct and complete *on its branch*; it is simply not in `develop`.

Round-1 refutations (carried forward, not re-litigated): "Unauthenticated REST API" (refuted — fails-closed), "DTO clone-field-drop/aliasing" (refuted — lossless), stale-H1 RemoveShare-leaks-5-maps (resolved by #897/#907). Round-2 re-verified the REST authz refutation in depth and confirms it still holds (see §6).

---

## 4. MED findings (grouped by sub-area)

**grace / REST integration**
- **M-1 — `/api/v1/grace/end` force-ends only the NFSv4 machine, leaving per-share NLM/SMB lock-manager grace active** (`internal/controlplane/api/handlers/grace.go:74-83`, `:81`). `ForceEnd` calls `h.sm.ForceEndGrace()` (`manager.go:910`), ending ONLY the global v4 `StateManager` grace. The per-share `lock.Manager` grace machines (`manager.go:2724` `EnterGracePeriod` / `:2732` `ExitGracePeriod`) that gate NLM byte-range and SMB lease reclaim are independent with no path from this endpoint, and the `GraceStatus` it reports (`grace.go:51`) is likewise v4-only — so the status an admin reads can disagree with the lock-manager grace actually blocking NLM/SMB locks. *Fix:* have `ForceEnd` also iterate the `MetadataService` lock managers and `ExitGracePeriod` each in-grace share (the lock-manager `onGraceEnd` already balances the coordinator), OR scope the endpoint+docs to "NFSv4 grace only" and add a separate lock-manager control; reconcile `GraceStatus` to surface both machines.

**REST authz (deep)**
- **M-2 — `operator` role reads full adapter `Config` via List, but per-adapter Get of the identical data is admin-only** (`pkg/controlplane/api/router.go:291-294` operator List vs `:297-304` admin Get; handler `adapters.go:114-135` List, `:280-291` `adapterToResponse`). `GET /api/v1/adapters` is gated `RequireRole("admin","operator")` and `List` calls `adapterToResponse(a)` per adapter with NO redaction (`resp.Config = a.GetConfig()` verbatim; `AdapterResponse.Config` is `map[string]any`). The per-adapter `GET /api/v1/adapters/{type}` returns the SAME payload but is `RequireAdmin`. So the read-only `operator` account is denied the single-item read yet obtains the identical Config (SMB/NFS: keytab path, SPN override, signing/security settings — `pkg/adapter/smb/adapter.go:76,356,451`) via List. Least-privilege violation + info disclosure; MED not HIGH because `operator` is authenticated/semi-trusted and Config holds paths/settings rather than raw secret material. *Fix:* drop `operator` from the List gate, OR give List a redacted DTO (`adapterToListResponse()` niling `Config` for non-admin), branching on `claims.IsAdmin()`.

**share-lifecycle-races**
- **M-3 — Share-removed-mid-op resolve MISS returns EIO not STALE; #918's STALE unification missed the registry-miss branch** (`shares/service.go:1142` `GetBlockStoreForHandle`, `:1241` no-block-store; NFSv3 `write.go:167`/`read.go:148`; SMB v2 `write.go:303`). `RemoveShare` deletes the share from the shares registry (`service.go:794`) *before* `runtime.RemoveShare` deregisters metadata (`runtime.go:387`). In the window where an in-flight WRITE/READ already resolved metadata and then calls `GetBlockStoreForHandle`, that lookup misses the registry and returns an untyped share-not-found error. #918 routed the in-flight-`Close` case (`ErrStoreClosed`) to `NFS3ERR_STALE`/`NFS4ERR_STALE`/`STATUS_FILE_CLOSED`, but this **sibling registry-miss path** hardcodes `NFS3ErrIO` (`write.go:167`, `read.go:148`) and `StatusInternalError` (`smb write.go:303`). So the same share-removed-mid-op event yields STALE on one timing and EIO/SERVERFAULT/INTERNAL on another — EIO makes a client retry against the dead handle instead of re-resolving via the mount. No data loss (CommitWrite never runs), hence MED. *Fix:* return a typed stale-handle error from `GetBlockStoreForHandle`/`GetBlockStoreForShare` via `metadata.NewStaleHandleError` (matching `GetStoreForShare` at `metadata/service.go:457`) and route the three resolve-miss sites through the existing error mapper instead of hardcoding. Add a race test asserting STALE not EIO.

**DTO + sub-service seams**
- **M-4 — Cross-boundary error drift: `ErrRestoreInProgress` falls through to HTTP 500 instead of 409 Conflict** (`internal/controlplane/api/handlers/snapshot.go:279-329` `mapSnapshotError` vs `pkg/controlplane/runtime/snapshot.go:1142-1145`). `restoreSnapshot` returns `models.ErrRestoreInProgress` when a second concurrent restore for the same share fails `TryLock` (`snapshot.go:1143-1144`); `RestoreSnapshot` (`:1098-1104`) passes it through unmodified to `handleErr → mapSnapshotError`, which has NO case for it (nor for `models.ErrRestoreMarkerNotFound`) → returns false → `handleErr` logs at Error (treats as unexpected) and writes a sanitized 500 "snapshot restore failed". The sentinel's own doc-comment (`errors.go:62`, `snapshot_hold.go:257`) says the caller should retry once it completes — semantically a retryable 409. This is exactly the operator-to-server error-shape drift class round-1 was tasked to find at the boundary; a polling client gets a non-retryable 500 and operator logs fill with false Error-severity alarms. *Fix:* add `case errors.Is(err, models.ErrRestoreInProgress): Conflict(w, "...retry once it completes"); return true` (mirroring the existing `ErrSnapshotInFlight` 409 branch); audit the full `models` error set against `mapSnapshotError`/`MapStoreError` for other unmapped sentinels.
- **M-5 — API→runtime transactionality gap: `CreateShare` returns 201 even when `runtime.AddShare` fails (lying success)** (`internal/controlplane/api/handlers/shares.go:347-412`). `Create()` writes the share DB row via `store.CreateShare` (committed, `:347`), best-effort writes default NFS/SMB adapter configs ignoring errors (`_ =` at `:360,:366`), then calls `runtime.AddShare` (`:401`). If `AddShare` fails (block-store init failure, unreachable remote, invalid config, or a `sameName` race returning a plain non-`ErrDuplicateShare` error from `shares/service.go:388`), the handler only logs Warn (`:404`) and still returns **201 Created** with the full share body (`:412`). The client believes the share is live and serving; it exists only as a DB row "can be loaded on restart" (`:403`). No rollback, no error surfaced. *Fix:* either fail the request (rollback `store.CreateShare` + adapter configs, return 502/422 with detail) or, if best-effort load is intentional, return **202 Accepted** with a not-yet-loaded body field + the runtime error — never 201; also stop swallowing `SetShareAdapterConfig` errors at `:360/:366`.

---

## 5. LOW findings (terse, grouped)

**DTO + sub-service seams**
- **L-1 — Snapshot `toWire` drops persisted `models.Snapshot.ManifestCount`; LIST always returns `manifest_count=0`** (`internal/controlplane/api/handlers/snapshot.go:230-269`). `toWire` never reads `s.ManifestCount`; for GET (`includeDisk=true`) it re-derives by parsing the on-disk manifest (`:256-261`), which fails silently (Debug-logged, leaves 0) once local artifacts are gone (post-`ResetLocalState`/eviction); for LIST (`includeDisk=false`, `:161`) it returns early at `:241` and never sets the field → every snapshot in a list reports `manifest_count:0` despite a non-zero authoritative DB value. *Fix:* seed `out.ManifestCount = int(s.ManifestCount)` unconditionally; override from disk only when `includeDisk` and the read succeeds.
- **L-2 — Vestigial DTO field: `dto.Snapshot.RetryOf` is never populated and has no models source** (`pkg/controlplane/api/dto/snapshot.go:17`). `models.Snapshot` has no `RetryOf` column, `toWire` never sets it, no apiclient sets it; `RetryOf` exists only as a `CreateSnapshotOpts` request input (`snapshot.go:39`). Dead wire surface / misleading contract. *Fix:* drop it, or persist on `models.Snapshot` and populate in `toWire` if the linkage is wanted.
- **L-3 — Dead-on-live-path squash impl: `metadata.ApplyIdentityMapping` never invoked in production** (`pkg/metadata/auth_permissions.go:124-125` call site; `auth_identity.go:246-310` impl; `types.go:159` field). The runtime/identity `Service.ApplyIdentityMapping` (`runtime.go:518` → NFS v3/v4 handlers) is the ONLY squash on the live data path and its 5-mode semantics are correct. The `metadata.IdentityMapping` field (`types.go:159`) that drives `metadata.ApplyIdentityMapping` is NEVER set in non-test code (zero production assignments), so `opts.IdentityMapping` is always nil and the metadata-side squash + `IsAdministratorSID` Windows-SID handling never executes. Round-1 flagged the *duplication* but not that one side is dead. Maintenance trap: a future caller wiring `opts.IdentityMapping` would silently double-apply a DIFFERENT squash model (booleans vs 5 modes). *Fix (prefer deletion per delete-eagerly policy):* delete `metadata.IdentityMapping`/`ApplyIdentityMapping`/`IsAdministratorSID` + the `PermissionOptions.IdentityMapping` field, or document runtime/identity as the sole authority.

---

## 6. Verified-correct (checked round-2, no finding)

**share-lifecycle (failure/concurrency paths):**
- GC-vs-`RemoveShare` is fail-closed, not data-loss: remote close is a benign flag-flip (`s3 store.go:527`) with checkClosed-guarded methods (`:218,:470`); GC mark aborts on removed share (`gc.go:400`) and an empty share list hard-errors (`gc.go:380`); blocks marked before removal stay live.
- Snapshot/restore use gated ops (`snapshot.go:473,640,1355`); remote refcount release deletes from the map under `s.mu` before `Close` (`service.go:760`); ungated `ListFiles` is benign (`FSStore filesMu.RLock`, `fs.go:1068`); `AddShare` 4-phase registration is race-safe with colon-name rejection (`service.go:339`).

**grace / REST integration:**
- `RegisterStoreForShare` lost-publish / removed-mid-flight grace-coordinator balancing (`service.go:209-240`) is asymmetric-but-correct (winner owns the coupling; `removedMidFlight` fires exactly one `OnLockGraceEnd`) — matches the documented exactly-once contract.
- `RemoveStoreForShare` (`service.go:266-300`) captures `IsInGracePeriod` before `AbortGracePeriod`, fires the balancing `OnLockGraceEnd` under `s.mu`; `AbortGracePeriod` is non-blocking (deadlock-free); idempotent.
- `newGraceAwareLockManager onGraceEnd` (`service.go:417-437`) reads the coordinator LIVE under RLock, so a coordinator installed after shares register at boot is still notified.
- `GracePeriodState.startGrace` (`grace.go:125-167`) correctly skips grace when both expected sets are empty (fresh-boot fast path) and always arms a hard backstop timer (grace cannot wedge new-state indefinitely); force-end/early-exit invoke `onGraceEnd` outside `g.mu` (no deadlock).
- SMB-never-records-mounts gap is **benign** (round-1 LOW stands): tracker is observability-only (`mounts/service.go Record` only via `runtime.RecordMount` hardcoded `"nfs"`, `runtime.go:534-535`); `RemoveShare` does not consult it. Mount tracker is concurrency-correct (all mutators under `mt.mu.Lock`; `collectMounts` returns deep copies under RLock).
- Adapter boot catch-up loop (`adapter.go:426-430`) calls `OnLockGraceStart` for shares already in grace at startup; at boot `clientsByName` is empty so it is a no-op (the bug is the runtime path, not boot). v4 client-recovery roster wiring (`adapter.go:390-399` + `StartGraceWithRoster`) is sound and no-op on a fresh store.

**REST authz (deep — round-1 refutation re-confirmed):**
- `JWTAuth` fails-closed (missing/invalid Bearer → 401, `middleware/auth.go:57-76`); HMAC alg-confusion blocked (`jwt_service.go:135-141`), access-token-type enforced (`:158-169`); secret ≥32 enforced (`jwt_service.go:60-63`).
- `RequireAdmin`/`RequireRole` fail-closed on nil claims (401) and wrong role (403); `RequireRole` denies when no roles supplied (`auth.go:80-127`).
- Refresh re-fetches fresh user, rejects missing (401)/disabled (403), regenerates claims from current role — stale-role/disabled refresh tokens unusable (`auth.go:123-180`). Login rejects disabled (403, `auth.go:85-88`).
- `ChangeOwnPassword` strictly scoped to `claims.Username` (`users.go:320-399`); `UserHandler.Get` enforces admin-or-self (`users.go:154-165`).
- Every mutating endpoint (users/groups/shares/snapshots/store/blockstore/adapters CRUD/grace ForceEnd/netgroups/identity-mappings/system/settings/clients/mounts/durable-handles) is inside a `RequireAdmin` group; capability-gated subroutes under `/adapters/{type}` correctly inherit `RequireAdmin` (`router.go:298`). `AdapterConfig.Config` raw string is `json:"-"`. `operator` appears once in the route tree (`router.go:292`) — no other non-admin elevation path (see M-2 for the one inconsistency).

**DTO + sub-service seams:**
- Architecture invariants remain clean; live-path squash semantics verified correct (fresh Identity copy, 5-mode mapping matches `models.SquashMode` docs: `SquashRootToAdmin`=no-op, `SquashAllToAdmin`→root, `*ToGuest`→anon).

---

## 7. Recommended PR-B shape

- **PR-B1 — grace-coordinator liveness (HIGH H-1).** Gate `OnLockGraceStart` so runtime-added shares don't arm v4 grace; refcount active lock-manager grace windows in the coordinator (per its own `grace_coordinator.go:42` doc) so `RemoveShare` can't end grace early either. `-race` test: live confirmed-client + `AddShare` ⇒ `CheckGraceForNewState` stays nil. Co-fix **M-1** (`/grace/end` only ends v4) in the same PR since both touch the v4↔lock-manager grace seam, and reconcile `GraceStatus` to surface both machines.
- **PR-B2 — blockstore-lifecycle close-gate (HIGH H-2 = round-1 H-A).** Merge #918 into `develop` (preferred — the fix is already reviewed on `v1.0/runtime-lifecycle-races`) or re-apply the engine-internal `closeMu`/`enter()` gate + drain-on-`Close`. Fold in **M-3** (route the registry-miss resolve branch through the typed stale-handle mapper so STALE is uniform across both the `Close` and the registry-miss timings). `-race` reproducer; verify with `git merge-base --is-ancestor`.
- **PR-B3 — API↔runtime contract hygiene (MED).** **M-4** (`ErrRestoreInProgress` → 409 + audit unmapped sentinels) and **M-5** (`CreateShare` lying-201 → 202/rollback + stop swallowing adapter-config errors). Small, boundary-local, no concurrency surface.
- **PR-B4 — authz consistency (MED M-2).** Drop `operator` from the `/adapters` List gate or ship a redacted list DTO.
- **Backlog issues (LOW):** L-1 (`ManifestCount` drop), L-2 (`RetryOf` dead field), L-3 (dead `metadata.ApplyIdentityMapping` — delete-eagerly). Quick, independent.

---

## 8. Coverage

**Audited (round-2, integration lens):** the grace-coordinator ↔ mount-tracker ↔ `/api/v1/grace` boundary; the share-churn (runtime `AddShare`/`RemoveShare`) path through `MetadataService.RegisterStoreForShare`/`initLockManagerFromStore`/`RemoveStoreForShare`; REST authz depth (JWT validation, role gates, refresh/login/password/self-or-admin, every mutating route's gate, capability subroutes); the DTO + 6-sub-service integration seams (snapshot wire object, error-shape mapping, `CreateShare` transactionality, identity-mapping live vs dead path); and **re-verification of round-1 H-A on `develop` HEAD `e16f0b01`** (via `git merge-base --is-ancestor`).

**Confirmed still-reproducing on develop:** round-1 H-A (now H-2 here) — #918 is NOT merged. **Confirmed benign:** SMB-never-records-mounts (round-1 LOW), `RegisterStoreForShare` grace-balancing asymmetry, mount-tracker locking.

**NOT separately audited / NOT load-tested:** the concurrency findings (H-1, H-2, M-3, M-5) are static-analysis + code-read on `develop`; each calls for a `-race` reproducer in PR-B. Protocol wire correctness (NFS/SMB encoding) is out of area-7 scope and covered by areas #4/#6. `metadata.ApplyIdentityMapping`'s Windows-SID path (L-3) was confirmed unreachable on the live path but not exhaustively traced through all test callers. M-3's third call site (`smb write.go:303`) was confirmed by read, not by an SMB-mid-remove integration run.
