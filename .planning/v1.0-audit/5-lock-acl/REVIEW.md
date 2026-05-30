# Lock Manager + ACL — v1.0 Area Audit (Area 5)

**Status:** NEEDS-FIX (HIGH durability/correctness holes; ACL/SD subsystem PATCH-grade)
**Date:** 2026-05-29
**Scope:** `pkg/metadata/lock/` (core durability, byte-range/share/oplock conflict detection, grace, leases, delegations), `pkg/metadata/acl/` (DACL evaluation + security-descriptor model), cross-protocol codecs (`internal/adapter/nfs/v4/attrs/acl.go`, `internal/adapter/smb/v2/handlers/security.go`, `pkg/auth/sid/sid.go`), and the consumer `pkg/metadata/auth_permissions.go`. Production call sites confirmed via `pkg/metadata/service.go` + `pkg/metadata/lock_exports.go`.
**Cross-check refs:** MS-FSA §2.1.5 (sharing/byte-range locks), MS-DTYP §2.4 (security descriptors/ACLs), RFC 7530 §9 (NFSv4 locking) + §6.4.1, X/Open NLMv4, Samba `source3/locking` + `libcli/security`, Linux `fs/locks.c`. Corroborates the NFS-area audit's grace/reclaim finding (gracePeriod==nil always, reclaim no-op, `NLM4_DENIED_GRACE_PERIOD` unreachable) and the prior H7/H8/H15 gap.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW | RESOLVED |
|---|---:|---:|---:|---:|
| Lock-core durability (`pkg/metadata/lock/` core) | 2 | 2 | 2 | 0 |
| Byte-range / share / oplock conflict detection | 1 | 1 | 2 | 0 |
| ACL + security descriptor (`pkg/metadata/acl/` + codecs) | 0 | 1 | 4 | 0 |
| **Total** | **3** | **4** | **8** | **0** |

**Verdict: NEEDS-FIX.** The ACL/security-descriptor subsystem is in strong shape and unusually faithful to Samba / MS-DTYP / RFC 7530 — on its own it is a clean PATCH. The lock layer is what drags the area to NEEDS-FIX: three HIGH integrity holes, all confirmed at production call sites (not inferred):

1. **Grace period is dead in production** — every reclaim is rejected, conflicting new locks are admitted immediately on restart, `NLM4_DENIED_GRACE_PERIOD` is unreachable.
2. **The lock persistence seam is entirely dead** — `SetLockStore` is never called, so *no* lock state (byte-range, share reservation, lease grant, lease-break) is persisted on any backend including badger/postgres, despite a fully-specced, fully-implemented persistence interface implying otherwise.
3. **Byte-range locks live in two non-cross-checked stores** — SMB2 byte-range locks and NLM/NFSv4 byte-range locks never conflict-check against each other, so concurrent conflicting writes can both be granted.

**Architecture invariants:** The conflict math, lease break state machine, oplock/delegation break dispatch, and ACL/SD evaluation all hold and are spec-faithful. What does *not* hold is the **durability contract**: a fully-specced `LockStore` + `GracePeriodManager` + badger/postgres persistence are shipped but wired to nothing, and the `FileLock` godoc itself admits "Server restarts: all locks lost." This is false-durability debt — the contract implies recovery that production never performs. The two-store byte-range split also breaks the cross-protocol byte-range exclusion invariant of MS-FSA §2.1.5.

---

## 2. HIGH findings

Ranked by blast radius. All three are confirmed at production call sites.

### H-1. Grace period is dead in production — reclaims always rejected, conflicting new locks admitted on restart, `NLM4_DENIED_GRACE_PERIOD` unreachable
**`pkg/metadata/service.go:95` (real call site) — `pkg/metadata/lock/manager.go:604-630, 2284-2321` — `reclaim.go:30-35`**

- **What:** Production constructs the lock manager via `NewLockManager()` (the deprecated alias in `pkg/metadata/lock_exports.go:68 → lock.NewManager()`), which routes to `newBaseManager` and **never sets `gracePeriod`** (field at `manager.go:588`, commented "may be nil"). Only `NewManagerWithGracePeriod` (`manager.go:626-630`) wires it, and a repo-wide grep confirms `NewManagerWithGracePeriod` / `NewGracePeriodManager` / `EnterGracePeriod` have **zero production callers** — only definitions in `lock/manager.go` + `lock/grace.go` and deprecated re-export wrappers in `lock_exports.go:66-68, 119-121`. With `gracePeriod` nil, every grace-gated method nil-guards to a no-op: `IsInGracePeriod` → false (`manager.go:2317`), `IsOperationAllowed` → allowed (`2301-2302`), `Enter/Exit/MarkReclaimed` → silent (`2285/2293/2310`). `reclaimLeaseImpl`'s gate `if !lm.IsInGracePeriod()` (`reclaim.go:30-35`) therefore **always fires**, rejecting every reclaim with a plain `fmt.Errorf` — never the grace status the NLM layer maps to `NLM4_DENIED_GRACE_PERIOD`.
- **Why:** X/Open NLMv4 and RFC 7530 §9.6.2 require the server to admit reclaims and reject *conflicting new* locks during the post-restart grace window so a prior owner recovers before a third party steals the range. With grace nil, new locks are admitted immediately on restart and reclaims rejected outright — silent lock-recovery failure. Conformance tests miss it because the in-memory map is also empty after restart, so there is nothing to reclaim and nothing to conflict with.
- **Fix:** Either (a) wire a `GracePeriodManager` into the `service.go` construction (`NewManagerWithGracePeriod` with a default ~90s NLM grace, `EnterGracePeriod(expectedClients)` at store open) and load persisted state; or (b) if grace recovery is out of v1.0 scope, **delete** the grace methods from the `LockManager` interface, the `GracePeriodManager` type, and the NLM `DENIED_GRACE_PERIOD` path rather than ship a no-op contract plus a never-constructed manager.
- **Verifier note:** Substance confirmed end-to-end; the only error in the original finding was the cited line (`service.go:124` is a file-handle decoder — the real construction is `service.go:95` via the `NewLockManager` alias). HIGH retained.

### H-2. Lock persistence seam is entirely dead in production — `SetLockStore` is never called; no byte-range, share-reservation, lease-grant, or lease-break state is ever persisted
**`pkg/metadata/lock/manager.go:888-893` (`SetLockStore`, only definition, zero production callers) — dead `!= nil` branches at `1799-1801, 1918-1920, 1939-1941, 2009-2011`; `leases.go:452-454,552-554,584-586,848-850,938-940,981-983,1030-1032,1055-1056,1095-1096`; `reclaim.go:38`; `cross_protocol.go:157`; `FileLock` godoc `manager.go:319-323`**

- **What:** Repo-wide grep confirms `SetLockStore` is referenced only by its own definition (`manager.go:888-889`) and test callers (`leases_test.go:620,664,718,1097,1135`) — **zero production callers**. `SetLockStore` is the only mutator of `lm.lockStore` (field `manager.go:590`, "persistent lock store (optional)"), so `lm.lockStore` is nil for the entire process lifetime. That makes **every** persistence branch dead: byte-range `Lock`/`AddUnifiedLock` were never persisted to begin with, and all the lease-break `PutLock` calls in `breakOpLocks` / `applyBreakStageLocked` / `forceCompleteBreaksExceptKey` (guarded by `if lm.lockStore != nil`) never execute. `reclaimLeaseImpl`'s `lm.lockStore != nil` branch (`reclaim.go:38`) is never taken — control falls to the `reclaim.go:108` path whose own comment says "No lockStore: try to find in memory (for testing without persistence)." The cross-protocol NLM conflict check (`cross_protocol.go:157`) is likewise gated on the nil store and never fires. The `LockStore` interface (`store.go:211`), `PersistedLock` schema, and badger/postgres impls (`durable_store.go` / `client_store.go`) are fully built but connected to nothing in this layer.
- **Why:** Root of the H7/H8/H15 gap, confirmed end-to-end. RFC 7530 §9 / X/Open NLMv4 assume lock state is reclaimable across restart; MS-FSA §2.1.5 share reservations gate concurrent opens. A fully-specced, backend-implemented persistence interface that is never connected implies durability that does not exist. Combined with H-1, restart silently drops all advisory byte-range locks, SMB share modes, and leases — and reclaim cannot work because nothing was persisted. The code already documents this as known debt: `cross_protocol.go:150` ("not yet pushed through lockStore.PutLock (architectural ...)"), `leases.go:320` ("SMB2 LOCK callers; not yet pushed through lockStore"), and the `FileLock` godoc explicitly lists "Server restarts (all locks lost)."
- **Fix:** If durability is in scope: call `SetLockStore` from `service.go` with the backend store, then `PutLock` on grant (`Lock`/`AddUnifiedLock`) + `DeleteLock` on release, stamp `ServerEpoch` at startup, and load persisted locks on store open. If out of scope for v1.0: delete `SetLockStore`, the `lockStore` field and all its dead `!= nil` branches, the `LockStore` interface, and fix the docs that imply restart-survival. The current half-wiring is pure false-durability debt.
- **Verifier note:** Confirmed at every cited location. Only mitigation (not enough to lower severity): pre-1.0 experimental FS, and the gap is already flagged in code comments.

### H-3. Byte-range locks split across two non-cross-checked stores — SMB2 vs NLM/NFSv4 byte-range conflicts never detected
**`pkg/metadata/lock/manager.go:583-596` (two independent maps) — admission paths `Lock` `639-650`, `AddUnifiedLock` `1276-1290` — debt comment `cross_protocol.go:148-152`**

- **What:** `Manager` holds two independent maps: `locks map[string][]FileLock` (commented "legacy", the SMB2 byte-range path via `Manager.Lock`) and `unifiedLocks map[string][]*UnifiedLock` (the NLM/NFSv4 path via `AddUnifiedLock`). The admission paths do not cross-reference: `Manager.Lock` (`639`) iterates conflicts over `lm.locks[handleKey]` **only** (`643-650`); `AddUnifiedLock` (`1276`) iterates over `lm.unifiedLocks[handleKey]` **only** (`1280-1290`). Neither consults the other store when admitting a byte-range lock, so a conflicting SMB2 byte-range lock and an NLM/NFSv4 byte-range lock on the same range can **both be granted**. The partial mitigation `hasByteRangeLockConflictForLease` (`cross_protocol.go:156`) reads *both* stores — but only on the **lease-grant** path, not byte-range-vs-byte-range admission. SMB2 BRL is never persisted (ties to H-2).
- **Why:** MS-FSA §2.1.5 requires byte-range exclusion across protocols. This gap allows concurrent conflicting writes on a multi-protocol share — a data-correctness hole on the data-integrity path. The codebase documents it as known debt (`cross_protocol.go:148-152`; MEMORY.md "persist SMB2 byte-range locks via lockStore.PutLock").
- **Fix:** Route `Manager.Lock` through the same conflict surface as `AddUnifiedLock` (or unify both behind a single `ConflictsWith` check against both stores), and/or push SMB2 BRL through `lockStore.PutLock` so the cross-protocol check applies. Fixing H-2 (persistence) is a prerequisite for the persist-based variant.
- **Verifier caveat:** The structural two-store non-cross-check is fully confirmed from `manager.go` + `cross_protocol.go`. The handler-to-entrypoint wiring (SMB `lock.go` → `Manager.Lock`, NLM/NFSv4 → `AddUnifiedLock`) rests on in-tree comments + the `AddUnifiedLock` caller set, as those handler files could not be opened directly this session. HIGH retained.

---

## 3. Triage downgrades / RESOLVED

No HIGH findings were refuted. All three HIGHs from the sub-audits survived adversarial verification at HIGH. The only correction was a mis-cited line number in H-1 (`service.go:124` → real call site `service.go:95` via the `NewLockManager` alias); substance unchanged. Zero RESOLVED across all three sub-areas.

---

## 4. MED findings

**Lock-core durability**
- **48-method single-implementation `LockManager` interface (interface-segregation violation; god-file driver).** `manager.go:27-280, 307`. ~48 methods, one impl (`var _ LockManager = (*Manager)(nil)`, line 307) in the same package — not a cycle-break. Consumers use disjoint slices (NFS/NLM vs SMB); SMB-CREATE-park internals (`SignalParkedCreates`, `WaitForBreakCompletionExceptKey`, `AnyHolderHasLeaseBits`) leak into the shared contract; callers already reach *past* the interface via concrete-only helpers (`HasActiveLeaseRecord`, `RequestLeaseAsOplock`, `TestLockByParams`, `UnlockAllForSession`, …) — the abstraction isn't buying isolation and is a top driver of the 2400-line god-file. Fix: export `*Manager` directly, or split into role interfaces (`ByteRangeLocker`, `LeaseManager`, `GraceController`, `BreakNotifier`).
- **`SetLeaseEpoch` raises each matching record independently — same-key records can diverge in epoch.** `manager.go:954-981`. Per-record `if epoch >= lock.Lease.Epoch { ... }` means two live records for one leaseKey can end up at different epochs; `GetLeaseState` / break dispatch then read whichever map-order surfaces first → non-deterministic `NewEpoch` in break notifications (the `break_twice` / `v2_breaking3` regression class). Fix: compute the max over all matching records first, then assign that single max to every matching record.

**Byte-range / share / oplock**
- **`SplitLock` uint64 overflow on partial unlock.** `manager.go:1022`. `unlockOffset + unlockLength` can wrap, corrupting the partial-unlock split (drop a lock or fabricate a fragment); values are wire-controlled. Fix: clamp `unlockEnd` to `maxUint64` on overflow.

**ACL + security descriptor**
- **ALARM ACEs silently dropped on SMB translation while `FATTR4_ACLSUPPORT` advertises ALARM to NFSv4 clients (cross-protocol lossiness).** `internal/adapter/smb/v2/handlers/security.go:60-71, 75-86`; advertised at `internal/adapter/nfs/v4/attrs/acl.go:117-119` + `pkg/metadata/acl/types.go:69-70`. `EncodeACLSupportAttr` reports `acl.FullACLSupport` (ALLOW|DENY|AUDIT|ALARM); NFSv4 stores ALARM ACEs unfiltered, but `nfsToWindowsACEType` has no ALARM case → `buildDACL` (`security.go:331-335`) silently `continue`s past it. An ALARM ACE set over NFSv4 is stored, advertised as supported, then silently stripped on any SMB read/rewrite. AUDIT survives; ALARM does not. Fix: stop advertising ALARM in `EncodeACLSupportAttr` (report ALLOW|DENY|AUDIT only — the honest fix since ALARM is a store-only no-op everywhere), or add an ALARM mapping path.

---

## 5. LOW findings

**Lock-core durability**
- **`ToPersistedLock` double-logs the Lease+Delegation invariant and silently truncates the record.** `store.go:293-353`. Logs the `Lease != nil && Delegation != nil` violation at Error (`297-300`) *and* Warn (`320-323`), then persists only lease fields and drops the delegation; no error return, so the persist path can't detect the lossy record. (Currently unreachable in production per H-2, but latent if persistence is ever wired.) Fix: collapse to one Error log; enforce at-most-one-of(Lease,Delegation) at construction, or give `ToPersistedLock` an error return.
- **`GracePeriodManager` triplicates its stop-timer + transition + capture-callback exit sequence; `AfterFunc` callback not cancellable.** `grace.go:142-145, 153-180, 184-208, 253-294`. `ExitGracePeriod`, `exitGracePeriodInternal`, and the `MarkReclaimed` early-exit each independently stop the timer / set state=Normal / capture+invoke `onGraceEnd`. Verified safe (idempotent `state==Normal` re-check, no double-fire, no timer leak — `Close` at 375 also stops it), so LOW/maintainability only; the type is dead in production per H-1. Fix: extract the sequence into one lock-held helper.

**Byte-range / share / oplock**
- **`End()` returns 0 for unbounded locks.** `types.go:225-230`. Inconsistent with `rangeEnd`/`rangeLast` which return `maxUint64`; latent byte-range-math footgun. Fix: return `maxUint64` for `Length==0`.
- **Break path discards `PutLock` errors and uses `context.Background()`.** `manager.go:1920, 1941, 2011`. Load-bearing break-in-progress persistence errors are silently dropped and the calls use `context.Background()` — can strand parked CREATEs after restart (once H-2 is wired). Fix: log errors; plumb the request `ctx`.

**ACL + security descriptor**
- **`MaxDACLSize` (64KB) is declared as the DoS bound but never enforced; only ACE count (128) is checked.** `pkg/metadata/acl/types.go:124-125`; `validate.go:38-54`; SMB `parseDACL` `security.go:518-520`; NFS `DecodeACLAttr` `acl.go:74-76`. `ACE.Who` is an unbounded Go string, so 128 ACEs with large `who` strings can exceed 64KB with no rejection. Practical alloc is bounded by the wire frame + 128-ACE cap (so not a critical unbounded alloc), but the declared invariant is silently unenforced. Fix: enforce a serialized-size estimate against `MaxDACLSize` in `ValidateACL` + at ingress, or delete `MaxDACLSize` and document that count + wire-frame are the only bounds.
- **`aceMatchesWho` wrapper is dead code in production (0 non-test callers).** `pkg/metadata/acl/evaluate.go:285-287`. The false-`ownerRights` wrapper is referenced only by its own definition and `evaluate_test.go`; the sole production path (`Evaluate`, line 226) calls `aceMatchesWhoWithOwnerRights` directly. A second "matches who" entrypoint on security-critical matching logic invites drift. Fix: delete the wrapper; update the test to call `aceMatchesWhoWithOwnerRights(ace, ctx, false)` directly.
- **Non-canonical DACL: a DENY ordered after an ALLOW for the same bit is silently ineffective.** `pkg/metadata/acl/evaluate.go:230-244` (contract at `validate.go:25-37`). "Newly-decided bits only" accumulation means a trailing DENY can't flip an already-granted bit. Correct for the smbtorture DENY1 case (and matches Windows/Samba first-match), but an admin authoring `[ALLOW X:WRITE]` then `[DENY X:WRITE]` expecting deny-wins gets a silent GRANT with no canonicalization or warning on the SET path. Fix: document that ACE order is load-bearing (DENY must precede ALLOW per principal/bit), or canonicalize to deny-before-allow on SET, or emit a Debug when a DENY follows a deciding ALLOW.
- **`SynthesizeFromMode` grants SYSTEM@ and ADMINISTRATORS@ FullControl on every POSIX-derived DACL, inconsistent with `SynthesizeWindowsDefault`.** `pkg/metadata/acl/synthesize.go:78-92` vs `113-131`. A mode-0600 file synthesized for SMB shows `BUILTIN\Administrators` FullControl though POSIX intent is owner-only; the two synthesizers disagree on whether Administrators get implicit full control. Server enforcement keeps POSIX mode authoritative (per the `buildDACL` comment), so this is displayed-SD vs enforced-access — but undocumented divergence is a smell. Fix: make the well-known-SID set consistent between the two functions and document that synthesized DACLs intentionally grant SYSTEM/Administrators FullControl (Samba convention).

---

## 6. Verified-correct

Checked and found OK.

**Lock conflict math & in-memory hygiene**
- `IsLockConflicting` / `locksOverlap` / `CheckIOConflict` (`manager.go:412-559`) faithfully mirror Samba `brl_conflict` + MS-FSA §2.1.4.10: zero-byte semantics, read-on-write same-open stacking, shared-blocks-all-writes-including-holder, per-open (OpenID, session fallback) ownership.
- `RangesOverlap`/`rangeLast` (`types.go:371-407`) use inclusive-end arithmetic, avoiding the 2^64-1 wrap bug; `SplitLock` (`manager.go:1007-1057`) implements POSIX 0/1/2-lock splitting incl. unbounded handling. `ConflictsWith` 4-case dispatch correct; `mergeRanges` uses `cmp.Compare`; `RemoveUnifiedLock` calls `SplitLock` (partial unlock supported).
- Map hygiene: `Unlock` / `UnlockAllForOpen` / `UnlockAllForSession` / `RemoveFileUnifiedLocks` / `RemoveAllLocks` / `RemoveClientLocks` all delete empty entries (`707/744/777/1384/2345-2347/2362`); `UnlockAllForOpen` guards empty openID (`721`).
- Defensive copying: `ListLocks`/`ListUnifiedLocks`/`ListDelegations`/getters return clones (`853/1373/2157/1400/2143`); `UnifiedLock.Clone` deep-copies Lease + Delegation (`types.go:255-275`).

**Lease / oplock / delegation break machine**
- `breakOpLocks` builds `toBreak` under `lm.mu`, releases the mutex, *then* dispatches callbacks (`1945-1949`) — no reentrancy deadlock; snapshots cloned before dispatch (`1992`). `dispatchOpLockBreak`/`dispatchDelegationRecall` copy the callback slice under RLock before invoking (`2242-2244/2261-2263`).
- Lease break state machine spec-cited and consistent: epoch advanced once on fresh dispatch (not per stage) (`1927-1931, 2002-2014`); concurrent breaks AND-merge `BreakingToRequired` without re-dispatch (`1912-1922`); traditional-oplock target masked to R/None (`1908-1910`); force-complete-on-timeout drains to None with `BrokenViaTimeout` tombstone, no epoch bump (`1770-1812`).
- `WaitForBreakCompletion`/`ExceptKey` acquire-or-create the wait channel under `lm.mu` before `select` (`1732-1737/1566-1571`) so no `signalBreakWait` close is missed; `signalBreakWaitLocked` close()+delete broadcasts (`1832-1837`). No goroutine leak — waiters exit on ctx.Done or channel close.
- Same-key + parent-dir-lease break suppression (`1875-1891`) correctly implement MS-SMB2 §3.3.5.9 nobreakself + §3.3.4.20 dirlease_should_break (parent-dir suppression scoped to `IsDirectory` leases). `OnDirChange` clones under `lm.mu` (race-safe); `OpLockBreakScanner` Start/Stop race-safe.
- `GrantDelegation` checks the `recentlyBroken` anti-storm cache atomically inside `lm.mu` with the lease-conflict scan (`2026-2029`); `breakDelegations` marks `recentlyBroken` only when something was actually broken (`2230-2232`).

**Persistence model (logic-correct, though unreachable per H-2)**
- `PersistedLock`↔`UnifiedLock` conversion (`store.go:293-439`) round-trips lease+delegation, omitempty-gates V1, reconstructs `BreakingToRequired` for legacy records (`406-419`). `reclaimLeaseImpl` validates isDirectory match, dir-handle existence via `HandleChecker`, and requestedState subset of persisted (`reclaim.go:60-76`) — logic correct even though its lockStore branch is currently unreachable.
- `DurableHandleStore` (`durable_store.go:90-112`) provides atomic `ConsumeDurableHandleByFileID`/`ByCreateGuid` fetch-and-delete closing the double-reconnect TOCTOU per MS-SMB2 §3.3.5.9.7/.12; `PersistedDurableHandle` persists `GrantedAccess` verbatim, `OriginalFileID`, `ClientGUID`, `PositionInfo`.
- `GracePeriodManager` (in isolation) invokes `onGraceEnd` *outside* the lock on all three exit paths (`174-179/204-207/284-289`); `Close()` stops the timer without firing `onGraceEnd` (`375-387`) — correct callback lifecycle, no timer leak.

**ACL / security descriptor / codecs**
- `Evaluate` (`evaluate.go:170-272`): nil ACL ⇒ deny, NullDACL ⇒ allow-all, empty DACL ⇒ owner-implicit pass (MS-DTYP §2.5.3).
- `AnonymousFileOwnerUID` (0xFFFFFFFF) sentinel (`evaluate.go:58-70` + `auth_permissions.go:372-375,876-879`) correctly prevents anon UID==0 from matching OWNER@ on root-owned files and gaining implicit RC|WRITE_DAC (#540 guard sound); wired in both `evaluateACLPermissions` and `buildFileAccessEvalContext`.
- Owner-implicit grant (`evaluate.go:116-136,252-268`) splits base RC|WRITE_DAC (always) from WRITE_OWNER (only `RequesterHasTakeOwnership`), masked by `deniedBits` so explicit DENY wins per-bit; matches Samba `se_access_check_implicit_owner`. `RequesterHasTakeOwnership` derived via `acl.HasTakeOwnershipPrivilege` (`auth_permissions.go:414,906`).
- OWNER_RIGHTS (S-1-3-4) pre-scan + sole-authority arbitration (`evaluate.go:188-212,298-328`); AUDIT/ALARM and INHERIT_ONLY correctly excluded from triggering the override (MS-DTYP §2.5.3/§2.4.10 + Samba).
- SID-form (`sid:`) matching (`evaluate.go:330-354`) checks requester SID then GroupSIDs via `slices.Contains`; localdomain numeric matching strict via `parseLocalDomainID` (CutSuffix+ParseUint(10,32)) — no panic/overflow.
- `ExpandGenericMask` (`generic.go:98-118`): GENERIC_READ/WRITE/EXECUTE/ALL expanded to file-object rights then 4 generic bits stripped (MS-DTYP §2.4.3); GENERIC_ALL ⇒ 0x001F01FF; applied at the SD-update boundary (`security.go:566`, `inherit.go:283`).
- `ComputeInheritedACL` (`inherit.go:114-296`): OI/CI/IO/NP + CREATOR dual-emission + INHERITED_ACE iff `parent.AutoInherited` + GENERIC_* expansion on non-INHERIT_ONLY emitted ACEs; mirrors Samba `calculate_inherited_from_parent` + `desc_expand_generic`; `MaxACECount` FIFO truncation enforced before append (`141-146,191-196`) — inheritance DoS-safe.
- NFSv4↔Windows ACE-flag translation (`flags.go:16-64`) remaps INHERITED_ACE 0x80↔0x10 (not naive truncation), OI/CI/NP/IO 1:1; both directions bit-exact and mutually inverse.
- `ParseSecurityDescriptorWithOptions` (`security.go:407-502`): bounds-checks all offsets against `len(data)` (`435,445,457`), null-DACL detection via SE_DACL_PRESENT+offset==0 (`463-465`), AUTO_INHERITED canonicalization gated on AUTO_INHERIT_REQ mirroring Samba `canonicalize_inheritance_bits` (`487-498`).
- `parseDACL` (`security.go:506-577`): ACE count capped at 128 (`518-520`), per-ACE header + aceSize bounds (`526-528,539-541`), SID slice bounded by aceSize (`544`), GENERIC_* expanded before store (`566`), unknown ACE types skipped by advancing offset (`551-554`) — no unbounded alloc, no slice-OOB.
- `DecodeSID` (`pkg/auth/sid/sid.go:60-83`): len≥8 guard, SubAuthorityCount is uint8 (max 1028 bytes), size re-checked against `len(data)` before reading sub-authorities — no OOB, alloc bounded.
- NFSv4 `DecodeACLAttr` (`acl.go:68-106`) caps aceCount at 128 before allocating; type/flag/mask/who round-trip lossless (same internal bit-space).
- `CheckFileAccessWithParent` (`auth_permissions.go:702-843`): per-bit `acl.Evaluate` probe (`753-761`), MAXIMUM_ALLOWED never denies but still requires explicit non-MAX bits to be DACL-granted (`826-831`, MS-SMB2 §3.3.5.9 ¶8), ACCESS_SYSTEM_SECURITY ⇒ ErrPrivilegeRequired not AccessDenied (`727-732`), FILE_READ_ATTRIBUTES always granted (`773-775`, MS-FSA §2.1.4.13), parent FILE_DELETE_CHILD delete override (`787-791,850-864`, Samba `parent_override_delete`).
- `DeriveMode`/`AdjustACLForMode` (`mode.go`): chmod touches only OWNER@/GROUP@/EVERYONE@ ACEs, preserves named-principal + non-rwx bits, excludes INHERIT_ONLY (RFC 7530 §6.4.1).
- `ProbeBitsAll` (`probe.go:20-35`) excludes NFSv4-only retention bits (0x200/0x400) with no SMB analog and is shared by the enforcement gate + MxAc reply to prevent drift (MS-SMB2 §2.2.13.2).

---

## 7. Recommended PR-B shape

Split the HIGHs into focused fix PRs; defer MED/LOW as tracked issues. The two durability HIGHs share a root cause and a strategic decision (wire vs. delete), so they should be decided together.

- **PR-B1 — Durability decision: wire or delete (H-1 + H-2).** These are one decision. **Option A (wire it):** call `SetLockStore` from `service.go:95`-area construction with the backend store; `PutLock` on grant / `DeleteLock` on release in `Lock`+`AddUnifiedLock`; `NewManagerWithGracePeriod` with a ~90s default + `EnterGracePeriod(expectedClients)` at store open; stamp `ServerEpoch`; load persisted locks on store open. **Option B (delete it):** remove `SetLockStore` + `lockStore` field + all dead `!= nil` branches, the `GracePeriodManager` type + grace interface methods + the NLM `DENIED_GRACE_PERIOD` path, and fix the `FileLock` godoc to stop implying restart-survival. Given pre-1.0 status and the broad blast radius of a half-correct durability implementation, **Option B is the lower-risk v1.0 shape** unless reclaim/grace is an explicit v1.0 requirement. Decide first; the choice dictates H-3.
- **PR-B2 — Cross-protocol byte-range exclusion (H-3).** Unify `Manager.Lock` and `AddUnifiedLock` behind a single conflict surface that checks both stores (the persist-based variant depends on PR-B1 Option A; the in-memory unification does not). Land after the PR-B1 decision so the conflict check targets the surviving store set. Include a regression test asserting an SMB2 BRL conflicts with an overlapping NLM/NFSv4 lock.

**Defer as issues (MED):** 48-method `LockManager` interface split / export `*Manager` (god-file driver); `SetLeaseEpoch` divergent-epoch fix; `SplitLock` uint64 overflow clamp; ALARM-ACE advertise-vs-drop honesty (stop advertising ALARM in `EncodeACLSupportAttr`).

**Defer as issues (LOW):** `ToPersistedLock` double-log + truncation; `GracePeriodManager` triplicated exit; `End()` unbounded-lock return; break-path `PutLock` error/ctx plumbing; `MaxDACLSize` enforce-or-delete; `aceMatchesWho` dead-code deletion; non-canonical DACL doc/canonicalize; `SynthesizeFromMode` well-known-SID consistency + doc. (Several LOWs in the lock layer become moot under PR-B1 Option B.)

---

## 8. Coverage

**Audited (read/verified line-by-line):**
- Lock core: `pkg/metadata/lock/manager.go` (all ~2400 lines), `grace.go` (387), `store.go`, `durable_store.go`, `client_store.go`, `types.go`, `reclaim.go`, `cross_protocol.go`, `leases.go`. Production wiring confirmed at `pkg/metadata/service.go` + `pkg/metadata/lock_exports.go`; `SetLockStore` / grace-constructor caller sets confirmed by repo-wide grep.
- Conflict detection: byte-range (`Lock`/`AddUnifiedLock`/`SplitLock`/`RangesOverlap`/`ConflictsWith`/`IsLockConflicting`/`CheckIOConflict`), share-mode, oplock + lease + delegation break machine, break-wait/park primitives.
- ACL/SD: `pkg/metadata/acl/` (`evaluate.go`, `generic.go`, `inherit.go`, `flags.go`, `validate.go`, `synthesize.go`, `mode.go`, `probe.go`, `types.go`), cross-protocol codecs (`internal/adapter/nfs/v4/attrs/acl.go`, `internal/adapter/smb/v2/handlers/security.go`, `pkg/auth/sid/sid.go`), and the consumer `pkg/metadata/auth_permissions.go`.

**Not fully audited / caveats:**
- **Protocol handler wiring for byte-range locks** (SMB `lock.go` → `Manager.Lock`; NLM/NFSv4 → `AddUnifiedLock`) could not be opened directly this session; the H-3 entrypoint mapping rests on in-tree debt comments + the `AddUnifiedLock` caller set. The structural two-store non-cross-check itself is fully confirmed from `manager.go` + `cross_protocol.go`.
- **Backend persistence implementations** (`durable_store.go` / `client_store.go` badger/postgres bodies) were audited for interface/contract shape and `PersistedLock` round-trip logic, not for backend-specific transaction/encoding correctness — since they are dead in production (H-2), runtime behavior was not exercisable.
- **SMB SACL parsing** is a known write-only empty stub (`offsetSACL` skipped) — client-supplied SACLs are silently dropped; noted in the headline but not raised as a separate finding since SACL/audit enforcement is out of scope for the current ACL model.
- **No runtime/integration testing** (in-memory conformance, restart-recovery, cross-protocol lock races) was performed — this was a static correctness + design audit. The grace/persistence HIGHs in particular would benefit from a restart-recovery integration test once the PR-B1 direction is chosen.