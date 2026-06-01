# Area 4 — NFS — Round-2 (missed-findings + integration lens) REVIEW.md

**Status**: ROUND-2 AUDIT COMPLETE — awaiting PR-B triage/kickoff.
**Branch**: re-verified on `v1.0/blockstore-perf-b1` tree (= `develop` + B1/B2/B3 perf), post PR-A #844 + PR #885 + durable-recovery #911/#919. PR-B to be opened off `develop`.
**Date**: 2026-06-01.
**Scope**: `internal/adapter/nfs/`, `pkg/adapter/nfs/`, plus the cross-component seams into `pkg/metadata/` (lock, service, file_modify) and `pkg/controlplane/runtime/snapshot.go`. Round-2 mandate: find what round-1 MISSED — (1) cross-area/cross-component contract seams, (2) error/failure/rollback/crash paths, (3) concurrency under load, (4) re-verify every unfixed round-1 HIGH on current develop.
**Cross-check refs**: RFC 1813 (v3), RFC 7530 (v4.0), RFC 8881 (v4.1), RFC 5531 (ONC RPC), RFC 2203 (RPCSEC_GSS); Linux `fs/nfsd/`, `net/sunrpc/auth_gss/`, `fs/lockd/`; rpc.statd / NSM semantics; Samba lock recovery. Round-1 baseline: `.planning/v1.0-audit/nfs/REVIEW.md` (19 HIGH) — every round-1 finding treated as KNOWN and not re-reported except where re-verification status materially changed.

**Method**: 5 parallel round-2 sub-audits, each handed the round-1 REVIEW as a known-findings baseline. Every HIGH independently adversarially verified against current-develop source; refuted/over-stated HIGHs downgraded with rationale (§3).

---

## 1. Summary

| Sub-area | HIGH | MED | LOW |
|---|---|---|---|
| v4-state / stateid integration | 1 | 2 | 1 |
| v3↔v4 interop / cross-component seams | 1 | 4 | 0 |
| DoS / resource limits | 1 | 2 | 1 |
| auth / RPCSEC_GSS boundary | 1 | 2 | 1 |
| NLM / NSM / statd lifecycle | 1 | 1 | 2 |
| **Total (post-triage, de-duplicated)** | **4** | **9** | **5** |

**De-duplication note**: SETATTR-never-validates-stateid (round-1 H18) was independently re-confirmed-LIVE by THREE sub-audits (v4-state, v3-v4-interop, auth-gss). It is counted **once** as a single HIGH (§2 H-A). The raw per-sub-audit tally (5 HIGH) collapses to **4 distinct HIGH** after merging the duplicate.

**Round-1 re-verification scorecard (the headline of this pass):**

| Round-1 HIGH | Status on current develop | Fixed by |
|---|---|---|
| H1 GSS DATA header-MIC | **FIXED** (wired through live dispatch, fail-closed CREDPROBLEM) | PR-A #844 |
| H2 share_deny conflict scan | **FIXED** | PR #885 |
| H3 all-ones stateid on WRITE | **FIXED** | PR #885 |
| H4/H5/H6 v4.0/v4.1 replay caches | **FIXED** (all three) | PR-A #844 |
| H7 grace on ungraceful restart | **FIXED** (durable client-recovery roster) | #911/#919 |
| H8 durable open/lock state | **CORRECT-BY-DESIGN** (Linux-nfsd identity-only recovery model) | #911/#919 |
| H9 WCC pre-op TOCTOU | **STILL LIVE** (store methods still don't return pre-op attrs) | — |
| H10 WRITE wtmax/FBIG | **STILL LIVE** | — |
| H11 READDIRPLUS maxcount | **STILL LIVE (v3)**; v4 READDIR now correct → version asymmetry | — |
| H12 v3 DRC | **STILL LIVE** (downgraded HIGH→MED, see §3) | — |
| H13 MaxConnections=0 | **STILL LIVE** (re-confirmed HIGH) | — |
| H14 NLM crash-cleanup | **FIXED** (releaseCrashedClientLocks real) | PR-A #844 + lock-durability |
| H15 grace never entered | **FIXED** … but **newly defeated** for the worst-case client (§2 H-D) | PR-A #844 |
| H16 SM_NOTIFY auth gate | **FIXED** (double-gated) | PR-A #844 |
| H17 state monotonicity | **FIXED** (in-memory; latent restart gap, §5) | PR-A #844 |
| H18 SETATTR stateid | **STILL LIVE** (#885 added only the all-ones-on-size guard) | — |
| H19 READ share-access | **FIXED** | PR #885 |

**Architecture invariants: still clean.** Round-2 found no new invariant breaks — business logic stays in `pkg/metadata`, `*metadata.AuthContext` threads everywhere, handles stay opaque, WRITE ordering holds. The four new HIGHs are all **integration-seam / failure-path** defects, exactly the class round-1's per-area isolation could not see.

**Verdict: NEEDS-FIX (still heavier than blockstore's PATCH), but the surface has shrunk sharply.** PR-A/#885/#911/#919 closed 12 of the 19 round-1 HIGHs, including the entire auth-bypass and replay-cache families. What remains is: (a) one true round-1 leftover — **H18 SETATTR stateid** — now re-confirmed by three independent audits; (b) three **NEW** integration-seam HIGHs that only surface across boundaries: a dead restore-verifier contract (silent client-write loss on snapshot restore), a grace-defeating crash-on-notify-failure (nullifies the H15 fix for the very clients it protects), and the long-standing default-unbounded-connections DoS. Two security-relevant NEW boundary findings (GSS_DESTROY teardown, GSS-with-Kerberos-disabled fail-open) verified real but downgraded to MED on blast radius.

**Theme**: round-1 fixed the *gates*; round-2 finds the defects live where the gates *meet other components* — the verifier promise that the restore orchestrator never honors, the crash-cleanup that fires inside the grace window it should respect, the auth fallthrough between the dispatch gate and the identity extractor. "Tests pass" misses all four because each is invisible from inside a single package.

---

## 2. HIGH findings (ranked by blast radius)

### Data-correctness across components

- **H-B — `BumpBootVerifier()` has zero production callers → in-place snapshot restore is invisible to NFS clients; in-flight UNSTABLE writes silently lost.** `internal/adapter/nfs/v4/handlers/write.go:24-50` (godoc + symbol) vs `pkg/controlplane/runtime/snapshot.go:1098/1371/1389/1452`. The v4 WRITE godoc documents a contract: *"The restore path calls `BumpBootVerifier()` on successful in-place restore … clients enter reclaim grace and fail reclaim with NFS4ERR_RECLAIM_BAD."* A repo-wide grep finds `BumpBootVerifier` **only** in `verifier_test.go:15/59` — zero production call sites. The named caller in the godoc (`storebackups.Service.RunRestore`) **does not exist anywhere in the repo** (phantom reference). The actual restore orchestrator `restoreSnapshot` performs the destructive in-place swap (`resetable.Reset` 1371, `backupable.Restore` 1389) and returns success **without bumping either verifier**. The v3 write verifier `serverBootTime` (`v3/handlers/verf.go:13`) is set once at init and never reassigned. **Why:** after a hot in-place restore (process stays up — the documented case), a client holding UNSTABLE writes sees the SAME WRITE/COMMIT verifier on its next COMMIT, concludes no restart occurred, treats its pre-restore writes as durably committed, and discards them — or never re-drives reclaim. The documented safety property never fires. This is cross-component contract drift on a *shipped* feature (snapshot/restore, Phase 24 / #797 / #868): the NFS-verifier promise lives in a v4 handler godoc, the obligation lives in the controlplane restore orchestrator, and the two never connect. The required disable-before-restore precheck (`snapshot.go:1149-1156`) narrows but does not close the window — disable/enable neither bumps the verifier nor resets NFSv4 client state, so a client mounted before disable retains its cached verifier across disable→restore→enable. **Fix:** have `restoreSnapshot` (and `recoverInterruptedRestores`) call `handlers.BumpBootVerifier()` after a successful in-place swap, AND bump an equivalent for the v3 `serverBootTime` (or unify both versions onto one atomically-bumpable boot verifier wired into the runtime); add a test asserting the restore path changes both verifiers. Until wired, delete the false godoc claim or block restore behind a documented must-remount caveat. *Verifier: confidence 85; the recovery mechanism is dead code and its godoc contract is false.*

### v4 integrity / authorization (round-1 leftover, re-confirmed by 3 audits)

- **H-A — SETATTR never calls `ValidateStateid` — bogus/expired/revoked/wrong-file stateid silently accepted; no lease renewal; a READ-only open's stateid can truncate (round-1 H18, STILL LIVE).** `internal/adapter/nfs/v4/handlers/setattr.go:43-99`. SETATTR decodes the stateid (line 44) and logs it (57), and the **only** enforcement is rejecting the all-ones READ-bypass *special* stateid when `setAttrs.Size != nil` (line 90 — the partial mitigation PR #885 added). It **never** calls `h.StateManager.ValidateStateid` (grep confirms the only production callers are `write.go:132` and `read.go:74`). Consequences: (a) a bogus / expired / revoked-after-CLOSE / wrong-filehandle / stale-epoch real open stateid is silently accepted — `ValidateStateid` (`state/stateid.go:146`) would have returned `BAD_STATEID`/`STALE_STATEID`/`OLD_STATEID`/FH-mismatch; (b) no implicit lease renewal occurs (RFC 7530 §9.6 step), so a client using SETATTR to keep state alive sees its lease wrongly expire; (c) a **real READ-only open** stateid passes line 90 and can perform a size-reducing truncate, bypassing the `SHARE_ACCESS_WRITE` open-mode gate that WRITE (`write.go:146-159` → `NFS4ERR_OPENMODE`) and the share-deny / byte-range-lock machinery enforce. The truncate-reclaim path (`setattr.go:142-184`) makes size-down destructive (drops blocks / reaps RefCount). The inline comment (54-56) explicitly defers full validation to a "dedicated stateid slice" that never shipped for SETATTR. **Narrowing (all three verifiers agree):** the metadata-layer `SetFileAttributes` (`pkg/metadata/file_modify.go:170-224`) still enforces POSIX owner/write-permission, so this is **not** an arbitrary cross-user write — but every NFSv4-specific guarantee (stateid validity, lease renewal, share-reservation / lock / read-only-open enforcement) is bypassed, leaving SETATTR strictly weaker than its WRITE/READ siblings two files over. Test coverage is zero: `setattr_test.go` uses `specialStateid()` (all-zeros) in every one of its 15 cases. **Fix:** after decode, `openState, err := ValidateStateid(stateid, ctx.CurrentFH, op)` with `op = StateidOpWrite` when `setAttrs.Size != nil` else `StateidOpRead`; map errors to `NFS4ERR_*`; on a size change require `openState == nil || openState.ShareAccess&OPEN4_SHARE_ACCESS_WRITE != 0` else `NFS4ERR_OPENMODE`; mirror `write.go:132-159`; add bogus-stateid and read-only-open test cases. *Verifier: confidence 88-97 across three independent confirmations; HIGH upheld.*

### Recovery / lock durability (NEW regression introduced by the H15 fix)

- **H-D — Server-restart SM_NOTIFY failure wipes an absent client's locks DURING the grace window — defeats the H15 grace fix for exactly the clients it was built to protect.** `internal/adapter/nfs/nsm/notifier.go:164-171` → `186-200` → `pkg/adapter/nfs/nlm.go:496-565` (`releaseCrashedClientLocks`) → `pkg/metadata/lock/manager.go:2879-2902/1146-1151`. On server restart, `performNSMStartup` launches `NotifyAllClients` in a background goroutine *while* each per-share lock manager is in its grace window (entered from persisted locks at `pkg/metadata/service.go:172-186`; the adapter catches up shares already in grace at `adapter.go:426-430`). `NotifyAllClients` sends SM_NOTIFY to every registered client with a **single 5s-timeout attempt, no retry** (`callback.NewClient(0)`). For any client whose notify **errors** — temporarily down, network blip, firewall, changed ephemeral port — the result loop logs "treating client as crashed" and calls `handleClientCrash → releaseCrashedClientLocks`, which **unconditionally** `ReleaseByOwnerPrefix("nlm:{client}:")` deletes **both the in-memory AND the persisted** lock records (the crash-cleanup comments explicitly say *"Immediate cleanup … no delay/grace window"*). There is **no grace-state check anywhere on this path**. **Why:** this is inverted NSM semantics. SM_NOTIFY on *server* restart tells clients the server rebooted so they reclaim; a *notify failure* means the notification didn't reach the client, NOT that the client crashed. Canonical rpc.statd / Linux `fs/lockd` treat restart-notify as best-effort and release a host's locks **only** on an INBOUND SM_NOTIFY / FREE_ALL from the rebooted peer. The grace window exists precisely so a momentarily-unreachable client can reclaim. Destroying that client's locks AND their persisted copy the instant the outbound notify fails means: the client reconnects within grace, issues reclaim, gets nothing back; a third party then acquires the freed range → **silent byte-range lock loss + cross-protocol corruption**, nullifying the H15/area-5 grace fix for the worst-case client. **Fix:** a failed outbound SM_NOTIFY must NOT trigger release. Retry/best-effort the notify and leave locks intact for the grace timer + `onGraceEnd` sweep (`service.go:417-437`) to age out genuinely-unreclaimed locks. Reserve `releaseCrashedClientLocks` for INBOUND crash evidence (peer SM_NOTIFY after the H16/H17 gates, FREE_ALL). At minimum, gate `releaseCrashedClientLocks` behind "not in grace" so restart cleanup cannot run inside the reclaim window. *Verifier: confidence 88; HIGH upheld — round-1 could not see this because crash-cleanup was a no-op stub then (H14).*

### DoS (round-1 leftover, re-confirmed)

- **H-C — Default `MaxConnections = 0` → unbounded connections; unauthenticated memory/goroutine-exhaustion DoS (round-1 H13, STILL LIVE).** `pkg/adapter/nfs/adapter.go:228-258` (`applyDefaults`) / `pkg/adapter/base.go:152-155` / `adapter.go:526-549` (`preAcceptCheck`). `applyDefaults` sets Port, `MaxRequestsPerConnection`, and all timeouts but never assigns `MaxConnections`, so it stays `0` (the code even documents "0 (unlimited)"). `connSemaphore` is built only when `MaxConnections > 0`, so the accept loop (`base.go:246-256`) acquires nothing and accepts unbounded connections. The only other gate, `preAcceptCheck`, is itself conditioned on `liveSettings.MaxConnections > 0` (live-settings default also 0, gorm `default:0`) and is a no-op at default. The similarly-named `lock.ConnectionTracker` (default 10000) is wired only to NLM client registration, NOT the TCP accept path. Each accepted connection = one `Serve` goroutine + up to `MaxRequestsPerConnection`(=100) in-flight requests each holding a pooled buffer up to `MaxFragmentSize`(=1.25MB), with **no global in-flight-byte budget**. Re-verified line-by-line; PR-A #844 did not touch it. **Fix:** ship a sane static default in `applyDefaults` (e.g. 1024) so `connSemaphore` is always built, plus a global in-flight-bytes budget across connections. *Verifier: confidence 95; HIGH upheld.*

---

## 3. Triage downgrades / RESOLVED

Every HIGH that an audit raised but verification downgraded or refuted:

- **`v3 duplicate-request cache` (round-1 H12) — re-confirmed PRESENT but downgraded HIGH→MED.** `pkg/adapter/nfs/connection.go:140-160/226-241`, `dispatch.go:167-211`. No XID/reply cache exists for the v3 path (reply caching lives only in the v4.1 slot machinery). Retried REMOVE/RENAME/LINK/MKDIR re-execute on TCP retransmit → spurious `ENOENT`/`EEXIST`. **Downgraded** because: (1) it is the standard, *documented* NFSv3 design tradeoff (`docs/NFS.md:115-117` calls out statelessness + idempotency reliance) — a DRC is an optimization Linux nfsd layers on, not a spec violation; (2) the failure is narrow (requires a full TCP retransmit of an already-replied request — the client kernel matches in-flight replies by XID), intermittent, non-crashing; (3) the finding's claim that **CREATE-EXCLUSIVE** is broken is **wrong** — `v3/handlers/create.go:217-253/417-452` implements RFC-1813 verifier idempotency via `FileAttr.IdempotencyToken` and correctly returns success on a matching-verifier retransmit. Real correctness gap worth tracking, not HIGH-grade. *Verifier confidence 90.*

- **`RPCSEC_GSS_DESTROY processed with no MIC verification` (NEW) — verified REAL but downgraded HIGH→MED.** `rpc/gss/framework.go:412-413, 851-881`. `Process()` routes DESTROY to `handleDestroy(cred)` dropping both `verifBody` and `headerPreimage` (which ARE threaded into `handleData`); `handleDestroy` looks up the context by the cleartext 16-byte handle and deletes it (line 859) with zero crypto verification — no `verifyHeaderMIC`, no seq check. `framework_test.go:305/331` codify DESTROY succeeding with nil verifier. This violates RFC 2203 (DESTROY carries a header verifier MIC that MUST be validated) and diverges from Linux nfsd (`gss_verify_header` runs on all GSS procs). An attacker who observes/injects one packet carrying the handle can forge a DESTROY and evict the victim's context, forcing a re-INIT/KDC round-trip, repeatable per observed handle. **Downgraded** because the impact is a liveness / auth-context-integrity DoS (forced reconnect churn), **not** data compromise, impersonation, or read/write auth bypass — lower blast radius than the round-1 HIGH auth class; the H1 fix closed forged-DATA but was never extended to DESTROY. **Fix:** thread `verifBody`+`headerPreimage` into `handleDestroy`, verify the call-header MIC against the looked-up context's session key before Delete, mirror `handleData`. *Verifier confidence 92.*

No round-2 HIGH was fully refuted to RESOLVED. The most consequential RESOLVED-class outcomes are the **round-1 HIGHs re-verified as FIXED** (H1/H2/H3/H4/H5/H6/H7/H8/H14/H15/H16/H17/H19) — see the §1 scorecard; these were checked line-by-line on current develop and are *not* re-reported as findings.

---

## 4. MED findings (defer as issues unless trivially co-fixable)

**v4-state / stateid:**
- CLOSE / OPEN_DOWNGRADE / LOCKU never validate the stateid against `ctx.CurrentFH` — the handlers don't pass the current FH down (`close.go:67`, `stubs.go:110`, `lock.go:449`), whereas READ/WRITE enforce FH↔stateid binding via `ValidateStateid` step 5 (`stateid.go:208-216`). A COMPOUND that PUTFHs file A then CLOSE/LOCKUs a valid stateid for file B mutates B's state with no `NFS4ERR_BAD_STATEID`. Blast radius limited (a client cannot forge another's stateid) but it diverges from the now-enforced READ/WRITE binding. Fix: thread `ctx.CurrentFH` into `CloseFile`/`DowngradeOpen`/`UnlockFile` and `bytes.Equal`-check.
- CLOSE / OPEN_DOWNGRADE / LOCKU reply cached in a *separate* `sm.mu` acquisition after the mutate returns (`close.go:90-95` after `CloseFile` released the lock at :67) → replay TOCTOU: a retransmit landing between the two locked sections gets `LastResult == nil` or the previous op's bytes, returning wrong bytes / `NFS4ERR_BAD_SEQID` (which Linux treats as fatal for LOCKU). Fix: cache the encoded reply inside the same critical section that advances the owner seqid.

**v3↔v4 interop / cross-component seams:**
- Share named exactly `pseudofs` collides with the v4 pseudo-FS handle prefix (`pseudofs/pseudofs.go:30/139`, vs `metadata.EncodeFileHandle = shareName+":"+uuid`, `types.go:42-48`). All its v4 handles `IsPseudoFSHandle`-misroute to the read-only pseudo-FS (every LOOKUP/GETATTR/READ/WRITE/SETATTR → NOENT/ISDIR/ROFS) while v3 routes to the real store. `shares/service.go:332-333` only rejects empty names. Fix: reserve `pseudofs` (and names containing `:`) or make the pseudo-FS prefix unspoofable by a legal share name.
- v4 WRITE enforces no size / `OffsetMax` / `MaxFileSize` cap (`write.go:208-217` checks only uint64 overflow), diverging from v3's `NFS3ERR_FBIG` (v3 `write.go:187-191`); advertised `FATTR4_MAXFILESIZE` (`encode.go:559`) is never enforced on either version. Same too-large/high-offset write gets three different outcomes across v3/v4/store. This is the v4 face of round-1 H10 plus a new MaxFileSize-unenforced seam. Fix: add `NFS4ERR_FBIG` bound to v4 WRITE; enforce `MaxFileSize` in metadata `PrepareWrite`.
- `FATTR4_MAXREAD/MAXWRITE/MAXFILESIZE` served from package-global `atomic.Uint64` (`attrs/encode.go:98-118`) that GETATTR mutates per-request via `SetFilesystemCapabilities` (`getattr.go:48-53`); with two shares advertising different caps, the value any client observes is whichever share last ran GETATTR — a last-writer race. v3 FSINFO reads per-call caps correctly, so it's also a v3↔v4 inconsistency. Fix: encode from the per-request caps already fetched, not package globals.
- (SETATTR-stateid raised here too — consolidated into §2 H-A.)

**DoS / resource limits:**
- NFSv4.1 backchannel `ConnWriter` does a bare `c.conn.Write(data)` under `writeMu` with **no** `SetWriteDeadline` (`pkg/adapter/nfs/handlers.go:378-383`), whereas every fore-channel reply sets `Timeouts.Write` (`reply.go:119-124`). A v4.1 client that binds a backchannel and stalls its socket wedges a CB_RECALL/CB_SEQUENCE write indefinitely; because `writeMu` serializes all writes, every concurrent fore-channel reply goroutine then blocks, pinning pooled buffers + requestSem slots until idle-timeout tears the connection down. Per-connection liveness (one conn per offending client). Fix: set a write deadline before the backchannel Write, mirroring `reply.go`.

**auth / RPCSEC_GSS boundary:**
- A GSS-flavored request reaching a server with Kerberos **disabled** (`gssProcessor == nil`, `dispatch.go:52` skips GSS), or any DATA request where GSS identity is absent, silently falls through to anonymous/"other"-permission access (`middleware/auth.go:71-75`, `v4/handlers/context.go:50-53`) instead of `AUTH_TOOWEAK`/`REJECTEDCRED`. Cross-component seam between the dispatch gate and the auth-extraction fallback. Fail-open posture (a client believing it is Kerberos-protected runs unauthenticated); access equals existing AUTH_NULL anonymous surface, so MED not HIGH. Linux nfsd rejects unconfigured/unsupported flavors. Fix: in `ExtractHandlerContext`/`ExtractV4HandlerContext`, when `AuthFlavor==AuthRPCSECGSS` and no GSS identity is present, fail with `NFS4ERR_WRONGSEC` / RPC `AUTH_TOOWEAK`.

**NLM / NSM / statd lifecycle:**
- NLM FREE_ALL releases no locks — the release wiring claimed in its comment does not exist (`nlm/handlers/free_all.go:77-93`, `dispatch.go:100`; `OnClientCrash`/`releaseCrashedClientLocks` are only ever called from the NSM notifier path). A client that reboots while the *server* stays up leaves its old NLM byte-range locks held forever (no server restart → no grace sweep ever reaps them) — a narrower recurrence of the H14 class on the client-reboot-only path. Round-1's "FREE_ALL wired" positive no longer holds. Fix: wire FreeAll to `ReleaseByOwnerPrefix("nlm:{req.Name}:")` + `RemoveClientWaiters`, and gate it on source-addr/monitored-host like the H16 SM_NOTIFY gate (FREE_ALL is unauthenticated — `NeedsAuth` is the dead field from round-1 SF-6).

---

## 5. LOW findings

**v4-state / stateid:**
- CLOSE and OPEN_DOWNGRADE don't compare the supplied stateid's own seqid against the current open-state seqid (`manager.go:1396-1440/1510-1542`), the way `LockExisting` (1903-1908) and `ValidateStateid` (`stateid.go:193-205`) do — a stale-seqid stateid is accepted instead of `NFS4ERR_OLD_STATEID`. Owner-seqid lock-step normally keeps them in sync, so it has not surfaced. Minor RFC 7530 §9.1.4 gap.

**DoS / auth (GSS ContextStore — flagged by two audits, same root):**
- `ContextStore.Store` does an unlocked `Count()` (O(n) Range) then `evictOldest()` then `contexts.Store()` with no spanning lock (`rpc/gss/context.go:207-216`) — concurrent RPCSEC_GSS INIT can each observe `count < max` and all insert, transiently overshooting `maxContexts`, and eviction can race an insert. `sync.Map` gives per-key safety but not the read-modify-write atomicity the cap needs; each Store is also O(n)+O(n). Bounded soft-cap overshoot only (default 10000 + TTL + LRU eventually reclaim), Kerberos-only. Fix: guard count-check+evict+store with a dedicated mutex, or maintain an atomic counter for an O(1) consistent cap.

**NLM / NSM lifecycle (both latent — gated behind the unimplemented NSM relay):**
- H16 SM_NOTIFY source-address gate likely matches the wrong host in real NSM topology (`nsm/handlers/notify.go:111-126`): it compares the NOTIFY source IP against `SM_MON.RemoteAddr`, but `RemoteAddr` is the *monitoring client*'s address while a NOTIFY for "mon_name rebooted" originates from *mon_name*'s host — generally different machines, so legitimate NOTIFYs would be rejected. Harmless today (the NOTIFY relay/release body is a no-op, `notify.go:83-92`); becomes a real correctness bug the moment the relay ships — which is exactly when the gate is load-bearing. Fix: validate against the address of the host NAMED in `mon_name`, not the registrant; add a topology test.
- SM_NOTIFY state-monotonicity (H17) is in-memory only — the `peerState` map (`nsm/handlers/handler.go:59/110/173-182`) is recreated empty on every `NewHandler`, and the server's own NSM state is hardcoded `1` each boot (`nlm.go:459`). After a restart the first NOTIFY for any mon_name compares against the zero default and is admitted, bypassing replay protection across the restart boundary. Latent for the same relay-unimplemented reason. Fix: persist last-seen peer state (and server state) via `ClientRegistrationStore`, reload in `LoadRegistrationsFromStore`.

---

## 6. Verified-correct (checked this pass, found OK)

**Round-1 HIGHs re-verified FIXED on current develop** (line-by-line; see §1 scorecard for the PR attribution):
- **H1** GSS DATA call-header MIC: `dispatch.go:58` computes `HeaderMICPreimage` over xid..end-of-credential (bounds-checked, `parser.go:182-206`), `framework.go:608` `verifyHeaderMIC` (constant-time HMAC, `KeyUsageInitiatorSign`) before trusting any credential field; empty preimage/verifier/key all fail closed CREDPROBLEM. Wired through the live path, not just tests.
- **H2/H3/H19** share_deny + all-ones-on-WRITE + READ share-access — `manager.go:1129-1144` (`shareConflictLocked`), `stateid.go:151-156` (`StateidOp` rejects all-ones on non-READ), `read.go:74/92-101` (captures openState, enforces `SHARE_ACCESS_READ`).
- **H4/H5/H6** all three replay caches — `CacheOpenOwnerResult` (`close.go:90-95`, `stubs.go:133-136`, reaching just-removed owners `manager.go:1255-1274`); `CacheLockOwnerResult` (`lock.go:222-243/473-476`) + replay-on-`SeqIDReplay` (`manager.go:1891-1896`); `SlotTable.ValidateSequence` per-slot `RequestDigest` → `NFS4ERR_SEQ_FALSE_RETRY` (`slot_table.go:154/206-208`).
- **H7/H8** grace + durability — cold-start grace seeded from durable client-recovery records surviving kill-9 (`client_recovery.go:173-227`, keyed by stable ClientIDString); `V4ClientRecoveryRecord` intentionally identity-only (id+boot-verifier+principal) mirroring the Linux nfsd client-record model, CLAIM_PREVIOUS verifier-gated against the boot snapshot (`manager.go:1040-1058`).
- **H14/H15/H16/H17** NLM/NSM recovery — `releaseCrashedClientLocks → ReleaseByOwnerPrefix + RemoveClientWaiters` is real; grace entered on unclean boot via `initLockManagerFromStore` + clean-shutdown marker; `LockFileNLM` gates on `IsOperationAllowed`; SM_NOTIFY double-gated (monitored-list/source-addr + state monotonicity). The grace/recovery code in `pkg/metadata/service.go` is unusually careful (publish-after-recovery, coordinator balancing on lost-register/removed-mid-flight). *(Caveat: the H15 fix is newly defeated for the restart-notify-failure case — §2 H-D; and FREE_ALL/H14-client-reboot path is unwired — §4.)*

**v4 stateid consistency (the ops that DO validate correctly):**
- LOCK/LOCKU (`LockExisting` `manager.go:1855-1939`) validate lock-owner seqid → stateid seqid (OLD/BAD) → open-mode-for-lock-type → grace-for-new-state; replay handled before the strict seqid check.
- DELEGRETURN / `ReturnDelegation` (`delegation.go:279-346`) validate via `delegByOther`, handle stale-epoch, idempotent for already-returned.
- FREE_STATEID / TEST_STATEID (`stateid.go:334-619`) reject special stateids, route by type tag, check seqid/revocation; TEST_STATEID correctly side-effect-free (no lease renewal).
- OPEN_DOWNGRADE enforces subset-only `share_access`/`share_deny` + non-zero access (RFC 7530 §16.19).

**v3↔v4 version parity:**
- v4 READDIR honors the maxcount byte budget (`readdir.go:171-219`, `NFS4ERR_TOOSMALL` when even the first entry won't fit) — so the v4 side of the H11 family is correct; the gap is v3 READDIRPLUS-only.
- v3/v4 READDIR cookieverf use identical mtime-based scheme, both treat mismatch as advisory; pagination cookies version-independent (both delegate to `metaSvc.ReadDirectory`).
- v4 READ clamps to `file.Size-offset`, EOF on `offset>=Size` (`read.go:162-166`).
- File handles version-agnostic in normal operation (both v3 LOOKUP and v4 `lookupInRealFS` encode via `metadata.EncodeFileHandle`) — same file, same handle across versions (modulo the `pseudofs` edge case, §4).

**DoS positives (round-1 positives that hold + the new ones checked):**
- RPC fragment cap 1.25MB enforced before alloc; v4 COMPOUND op-count bounded `MaxCompoundOps=128` before result-slice alloc; v4.1 slot count clamped to `[MinSlots, DefaultMaxSlots]`; GSS opaque decode capped 1MB; GSS context store bounded (default 10000 + 8h TTL + 5-min cleanup sweep + LRU); v3 READ/WRITE/READDIR all bounded before alloc; `parser.go:130` ReadData now has explicit overflow/overrun checks (round-1's panic concern fixed by the H1 HeaderMICPreimage refactor); per-connection in-flight bounded to `MaxRequestsPerConnection`=100; fore-channel `writeReply` sets `Timeouts.Write`.

**auth / GSS boundary positives:**
- AUTH_SYS squash re-applied per-call via `BuildAuthContextWithMapping`/`ApplyIdentityMapping` (`metadata/auth_identity.go:254`) on the live credential — not bypassable on stateless v3 (confirms round-1 §3).
- GSS SeqWindow replay/sliding-window fully mutex-protected (`sequence.go:65-106`); MAXSEQ destroy + silent-discard match RFC 2203 §5.3.3.1; per-call service-downgrade enforced (`framework.go:628`); `sendGSSReply` fails closed on wrap errors; context Store-before-reply ordering preserved (Ganesha race avoided); cleanup goroutine leak-safe.

---

## 7. Recommended PR-B shape

Round-2 collapses the open NFS work to a small set. Split into focused fix PRs (do **not** one-shot them):

1. **`v1.0/nfs-restore-verifier`** — **H-B**. Wire `RestoreSnapshot`/`recoverInterruptedRestores` → `BumpBootVerifier()` for both v4 and v3 (or unify onto one runtime-owned boot verifier); add a restore-path test asserting both verifiers change. **Highest priority — silent data loss on a shipped feature.** Touches the v4/v3 handler ↔ controlplane seam; co-ordinate with the snapshot/restore owner.
2. **`v1.0/nfs-v4-setattr-stateid`** — **H-A** (round-1 H18). `ValidateStateid` in SETATTR + `SHARE_ACCESS_WRITE` open-mode guard on truncate + bogus/read-only-open test cases. Self-contained; mirror `write.go`. Optionally fold in the §4 MED (CLOSE/DOWNGRADE/LOCKU FH-binding) as a same-family stateid-consistency pass.
3. **`v1.0/nlm-grace-notify`** — **H-D**. Stop treating outbound SM_NOTIFY failure as a client crash; gate `releaseCrashedClientLocks` behind "not in grace"; reserve it for INBOUND crash evidence. Pairs with the §4 MED **FREE_ALL release-unwired** fix (same crash-release wiring) — land them together.
4. **`v1.0/nfs-dos`** — **H-C** (round-1 H13): sane `MaxConnections` default + global in-flight-byte budget. Optionally fold in the §4 backchannel-write-deadline MED and the §5 ContextStore-TOCTOU LOW (small robustness items).
5. **`v1.0/nfs-gss-destroy`** (MED, security-adjacent) — thread the verifier into `handleDestroy` + the GSS-disabled fail-open fix (`AUTH_TOOWEAK` instead of anonymous fallthrough). Both are GSS-boundary one-liners.

**Defer as backlog issues** (MED/LOW, not v1.0-blocking): v3 DRC (documented tradeoff), `pseudofs` name collision, v4 WRITE FBIG/MaxFileSize bound + per-share FATTR4 cap race (one "v4 write-cap parity" issue), CLOSE/DOWNGRADE stateid-seqid LOW, the two NSM-relay-latent LOWs (file against whoever ships the NSM relay — the gate + monotonicity are *pre*-requisites for that relay, not independent fixes).

Each PR-B: apply fix → `code-simplifier:code-simplifier` → `feature-dev:code-reviewer` → `go test -race` → verify. Sign commits, assign marmos91.

---

## 8. Coverage

**Audited (round-2 lens):**
- v4 state/stateid integration across **all** stateid-consuming ops (OPEN/CLOSE/READ/WRITE/SETATTR/LOCK/LOCKU/OPEN_DOWNGRADE/DELEGRETURN/FREE_STATEID/TEST_STATEID) — cross-op consistency, not per-op isolation.
- v3↔v4 interop seams: write verifier across restore, READDIR/READDIRPLUS budget asymmetry, handle routing (incl. pseudo-FS prefix), WRITE size-cap divergence, per-share FATTR4 capability advertisement.
- **Cross-component seams**: the NFS-verifier ↔ controlplane `restoreSnapshot` contract; the dispatch-gate ↔ auth-extractor fallthrough; the NSM-notifier ↔ per-share-lock-manager ↔ grace-window interaction; FREE_ALL ↔ crash-release wiring.
- **Failure/recovery paths**: snapshot restore, server-restart SM_NOTIFY, client-reboot FREE_ALL, GSS context teardown, Kerberos-disabled fallthrough, replay/retransmit TOCTOU windows.
- **Concurrency under load**: backchannel write-deadline / `writeMu` serialization, GSS ContextStore eviction TOCTOU, CLOSE/DOWNGRADE/LOCKU two-phase reply-cache race, per-connection vs global resource budgets.
- DoS/resource limits re-verified end-to-end (fragment cap, compound ops, slot count, GSS caps, connection cap).
- Re-verification of all 19 round-1 HIGHs against current develop (§1 scorecard).

**Cross-checked against canonical impls**: Linux `fs/nfsd/nfscache.c` (DRC), `net/sunrpc/auth_gss/svcauth_gss.c` (`gss_verify_header` on DESTROY), `fs/lockd`/rpc.statd (SM_NOTIFY release semantics), Linux nfsd client-record recovery model (H8 design), RFC 1813/7530/8881/2203.

**NOT re-audited (deliberately out of round-2 scope):** the v4 wire/XDR layer (round-1 sub-area E — zero HIGH, confirmed DoS-clean; not re-run); portmap/rpcbind internals (round-1 LOW/MED only); the structural/design/perf/bloat passes (round-1 §9 — Option-A/B refactor + perf quick-wins are a separate track, unchanged). No new unaudited NFS surface was discovered.
