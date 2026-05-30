# Area 4 — NFS handlers — PR-A Audit (REVIEW.md)

**Status**: AUDIT COMPLETE — awaiting PR-B triage/kickoff.
**Branch**: NFS audit run on `v1.0/blockstore-perf-b1` tree (= `develop` + B1/B2/B3 perf). PR-A to be opened off `develop`.
**Date**: 2026-05-29.
**Scope**: `internal/adapter/nfs/`, `pkg/adapter/nfs/` — ~56K src LOC (measured; PLAN headline 52.9K was the Wave-0 estimate).
**Cross-check refs**: RFC 1813 (v3), RFC 7530 (v4.0), RFC 8881 (v4.1), RFC 5531 (ONC RPC), RFC 2203 (RPCSEC_GSS), RFC 2623 (AUTH_SYS over NFS); Linux `fs/nfsd/`, `net/sunrpc/`.

**Method**: 5 parallel read-only sub-audits across 2 batches. Batch 1: v3 handlers, v4 state machine, RPC/GSS. Batch 2: aux protocols (NLM/NSM/portmap) + v4 attrs/types/XDR. **Area #4 PR-A coverage now complete.**

**Agent outputs** (raw findings, kept for provenance):
- `_partial-a-v3.md` — NFSv3 handlers (12.5K) + mount handlers
- `_partial-b-v4state.md` — NFSv4 state machine (7.7K) + v4/v4.1 OPEN/LOCK handlers
- `_partial-c-rpcgss.md` — RPC framing + RPCSEC_GSS + AUTH_SYS + dispatch (3.9K)
- `_partial-d-aux.md` — NLM (lockd 3.4K) + NSM (statd 1.7K) + portmap (1K)
- `_partial-e-v4types.md` — v4 attrs/types/XDR wire encoding (~8K)

This REVIEW.md consolidates and triages all five.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW |
|---|---|---|---|
| A — v3 handlers | 3 | 6 | 4 |
| B — v4 state machine | 7 | 7 | 4 |
| C — RPC / GSS / auth | 3¹ | 6 | 5 |
| D — NLM / NSM / portmap | 4 | 9 | 7 |
| E — v4 attrs/types/XDR | 0 | 2 | 10 |
| H — handlers 2nd pass | 2 | 2 | 3 |
| **Total (post-triage)** | **19** | **32** | **33** |

Batch-3 design/perf/bloat/versions passes (DESIGN-AUDIT.md + `_partial-f/g/i/j`) are structural — summarized in §9 below, not in this correctness tally.

¹ C originally reported 4 HIGH; the AUTH_SYS-squash-bypass HIGH was **downgraded to RESOLVED** during triage (see §3).

**Architecture invariants: clean.** All three sub-audits confirm handlers respect CLAUDE.md rules — business logic stays in `pkg/metadata`, `*metadata.AuthContext` is threaded everywhere, file handles are opaque (decode only for routing), WRITE follows Prepare→BlockStore→Commit. **Every HIGH is correctness / interop / security drift, not an invariant break.** The code is well-commented and cites RFCs heavily.

**Verdict**: the NFSv4 **state machine** carries real v1.0-blocking integrity holes (share reservations are a silent no-op; a special stateid bypasses WRITE checks; both v4.0/v4.1 replay caches are broken). The **RPCSEC_GSS** path has a complete krb5 auth bypass. The **NLM/NSM lock-recovery** half is unfinished (crashed-client locks held forever; grace plumbed but never enforced; statd SM_NOTIFY is an unauthenticated stub). The **v4 wire/XDR layer (E) is the healthiest sub-area** — zero HIGH, every alloc bounded, no short-read; only consolidation + idmap-symmetry work. Grade: **NEEDS-FIX** before tag — heavier than blockstore's PATCH.

**Theme**: correctness *backbone* is sound (logic delegated to `pkg/metadata`, NLM uses the unified lock manager, wire decoding is DoS-safe). The holes cluster in (1) **security gates** (GSS header MIC, statd auth) and (2) **recovery/exactly-once** (v4 replay caches, v4/NLM grace + reclaim, crash cleanup, durable state). Both are exactly what "tests pass" misses.

---

## 2. HIGH findings (triaged, ranked by blast radius)

### Security / auth

- **H1 — RPCSEC_GSS DATA call-header MIC never verified → krb5 auth bypass.** `rpc/gss/framework.go:574` (`handleData` plumbs `verifBody` but never verifies it). For `krb5` (svc_none, `framework.go:632`) DATA requests get **zero** crypto checks: a context handle (16 bytes, cleartext on every call) lets an attacker forge requests under the victim principal. krb5i/krb5p verify the body MIC/Wrap so practical exposure is smaller, but the header (gss_proc/service/handle) stays unauthenticated. Fix: `gss_verify_mic` over the marshalled call header through the credential using `gssCtx.SessionKey`; thread the raw header preimage into `Process`. **Compounds with C-MED service-downgrade + C-LOW handle-not-connection-scoped.**

### v4 data-integrity

- **H2 — Share reservations (`share_deny`) never enforced across open-owners.** `v4/handlers/open.go:331`, `v4/state/manager.go:944`. `NFS4ERR_SHARE_DENIED` is defined but has **zero** call sites; OPEN only OR-merges bits for the same owner, never scans for conflicting deny from other owners. DENY_* is a silent no-op → concurrent writers corrupt a file the owner believed reserved; breaks cross-protocol SMB/NFS deny semantics. Fix: add `openStatesByFile` index, conflict-check under `sm.mu`, return `NFS4ERR_SHARE_DENIED`; also validate `share_access ∈ {READ,WRITE,BOTH}` at decode.
- **H3 — All-ones (READ-bypass) special stateid accepted on WRITE/SETATTR/LOCK.** `v4/state/stateid.go:127`, `v4/handlers/write.go:144`. `ValidateStateid` short-circuits *any* special stateid to `(nil,nil)`; a nil openState skips the `SHARE_ACCESS_WRITE` check. Per RFC 7530 §9.1.4.3 all-ones is READ-only — WRITE/SETATTR/LOCK MUST return `NFS4ERR_BAD_STATEID`. Lets a client write bypassing share-mode + byte-range locks. Fix: distinguish the two special stateids; allow all-ones on READ only.

### v4 exactly-once (replay)

- **H4 — v4.0 open-owner replay cache cross-op contaminated.** `v4/state/manager.go:1074`, `open.go:486`. `OpenOwner.LastResult` is one slot populated only by OPEN, but CLOSE/DOWNGRADE/CONFIRM all advance `LastSeqID` → a legit TCP retransmit replays stale/wrong bytes → `NFS4ERR_BAD_SEQID` storms. Fix: cache encoded result+status for *every* owner-seqid op (mirror Linux `so_replay`).
- **H5 — v4.0 LOCK/LOCKU replay not honored → silent lock loss.** `v4/state/manager.go:1577/1655/1889`. `LockOwner.LastResult` declared but never populated → replayed LOCK returns `NFS4ERR_BAD_SEQID`, which Linux treats as fatal → drops the lock-owner. Fix: populate + return cached lock result.
- **H6 — v4.1 `SEQ_FALSE_RETRY` never detected.** `v4/state/slot_table.go:177`, `v41/handlers/sequence.go:54`. Slot retry returns cached reply with no request-fingerprint compare (RFC 8881 §2.10.6.1.3). A client reusing slot+seqid with different ops silently gets the stale reply — confused-deputy / data-exposure risk. Fix: cache a request digest, compare, return `NFS4ERR_SEQ_FALSE_RETRY` on mismatch.

### v4 restart / durability

- **H7 — Grace skipped on ungraceful restart.** `v4/state/grace.go:104`. `StartGrace` no-ops when `expectedClientIDs` is empty, which is exactly the kill-9/power-loss case → reclaiming clients hit `ErrNoGrace` instead of `NFS4ERR_GRACE`. Fix: enter grace on any cold start that may have had prior state; gate "skip" purely on a verified clean shutdown.
- **H8 — All v4 open/lock/stateid state is in-memory only.** `v4/state/manager.go:31`. Only a thin `ClientSnapshot` (id/verifier/addr, not opens/locks) persists at clean shutdown. Locks + share reservations don't survive restart even on badger/postgres → reclaim races / silent lock loss. Partly known/documented (single-node). Fix (scoped): either persist a per-share stateid/lock journal, or block all new state-creating opens during a full cold-start grace window. **Note: H7+H8 interlock — even with grace fixed, reclaim has nothing to reclaim against until H8.**

### v3 interop / correctness

- **H9 — WCC pre-op attrs captured non-atomically (TOCTOU).** Every mutating v3 handler (`remove.go:115`, `rename.go:146`, `mkdir/rmdir/create/setattr/symlink/mknod/link/commit`) does a separate `GetFile` then mutates — a concurrent mutation in the window yields a `wccBefore` that never preceded the op → silent client-cache corruption (the exact failure WCC prevents). WRITE is the correct model (`PrepareWrite` returns `PreWriteAttr`). Fix: have store mutation methods return pre-op attrs.
- **H10 — WRITE hardcodes 1 MB cap → `NFS3ERR_FBIG`, ignoring advertised `wtmax`.** `write.go:152`, `write_validation.go:64`. Decoupled from FSINFO `MaxWriteSize`; FBIG is the wrong code for an over-large request. Fix: derive cap from `GetFilesystemCapabilities().MaxWriteSize`; short-write or `NFS3ERR_INVAL`.
- **H11 — READDIRPLUS ignores client `maxcount` byte budget.** `readdirplus.go:279`, `readdirplus_codec.go:155`. Encodes all entries+fattr3+filehandle with no running tally vs `MaxCount` (RFC 1813 §3.3.17) → oversized reply the client truncates/rejects → large-dir listing breaks. Fix: track encoded bytes, stop at budget, set `eof=false`.
- **H12 — No duplicate-request cache (DRC) for non-idempotent v3 ops.** No XID/reply cache anywhere in dispatch. Retried CREATE-excl/REMOVE/RENAME/LINK/MKDIR re-executes → spurious `EEXIST`/`ENOENT` to client (Linux `fs/nfsd/nfscache.c` exists for this). Fix: XID+checksum+srcaddr reply cache, or document the limitation.
- **H13 — Default `MaxConnections = 0` (unlimited).** `pkg/adapter/nfs/adapter.go:236`. Each conn = a `Serve` goroutine + up to 1.25 MB fragment buffer + 100 in-flight → memory-exhaustion DoS. Fix: ship a sane default cap (e.g. 1024) + global in-flight-bytes budget.

### v4 stateid validation gaps (batch 3, 2nd-pass handlers — sub-area H, both NEW)

- **H18 — v4 SETATTR stateid decoded but `ValidateStateid` NEVER called.** `v4/handlers/setattr.go:46-49`. Stateid decoded, then `_ = stateid` + logged only — no validation anywhere in the file (verified). A bogus/expired/revoked-delegation stateid silently succeeds on SETATTR; no implicit lease renewal; no open-mode guard on size-reduce. Any client that observes a stateid can SETATTR files after the real holder closed. Compare WRITE (`write.go:129`). Fix: `ValidateStateid` after decode → `NFS4ERR_BAD_STATEID` on error + write-access check on size change. **Confidence 100.**
- **H19 — v4 READ discards `openState` from `ValidateStateid` → no `SHARE_ACCESS_READ` check.** `v4/handlers/read.go:71` (`if _, stateErr := ...ValidateStateid(...)`). A write-only open can READ via its stateid (RFC 7530 §16.25.4 violation; symmetric to H3 on the READ side for ANY open stateid). Fix: capture openState; `if openState != nil && openState.ShareAccess&OPEN4_SHARE_ACCESS_READ == 0 { return NFS4ERR_OPENMODE }`. **Confidence 100.**

### NLM/NSM lock recovery + statd security (batch 2, sub-area D)

- **H14 — NLM `handleClientCrash` is a no-op stub → crashed-client locks held forever.** `pkg/adapter/nfs/nlm.go:479`. Wired into both the NSM crash detector and FREE_ALL, but the body does nothing (`totalReleased` hard-coded 0; comment admits it). A crashed client's byte-range locks are never released → permanent deadlock of those ranges. Fix: prefix-scoped release (`nlm:{clientID}:`) across each share's lock manager + drain the blocking queue for affected files. **Boot-side own-reboot notify IS wired** — only the release side is missing.
- **H15 — NLM grace built but never entered; `LockFileNLM` never consults it → `NLM4_DENIED_GRACE_PERIOD` unreachable, reclaim a no-op.** `pkg/adapter/nfs/nlm.go:74`, `pkg/metadata/lock/manager.go:2282`, `lock/grace.go`. Every lock Manager is constructed with `gracePeriod==nil` (no `NewGracePeriodManager`/`EnterGracePeriod` caller), AND `LockFileNLM` goes straight to `AddUnifiedLock` without `IsOperationAllowed`. So restart drops/strands every lock (compounds H14). Fix: construct managers with grace, `EnterGracePeriod` on boot seeded from persisted locks, gate `LockFileNLM` on `IsOperationAllowed`. **Interlocks with H8 (no durable v4 state) — same durable-lock-state gap spans v4 + NLM.**
- **H16 — NSM SM_NOTIFY is an inert TODO stub with no sender authentication.** `nsm/handlers/notify.go:14`. Relay unimplemented (peer-reboot lock recovery broken today) AND no source-addr / monitored-host / privileged-port check (NSM procs are `NeedsAuth:false`). When the relay ships as-written this becomes the **classic statd spoofing primitive**: any reachable host forges `SM_NOTIFY mon_name=<victim>` → server drops victim's NLM locks → another client grabs the range → silent corruption. Fix: gate on monitored-list membership + source-addr match (or trusted-network bind) **before** implementing the relay; enforce monotonicity (H17).
- **H17 — No SM_NOTIFY state-number monotonicity → replays re-trigger lock release.** `nsm/handlers/notify.go`, `nsm/handlers/mon.go:71`. No per-monitored-host last-seen state stored or compared; `Mon` records the *server's own* state, not the peer's. Replayed/duplicate NOTIFY re-fires release every time. Fix: store last-seen state per `mon_name`; act only when `incoming > stored`.

---

## 3. Triage downgrades / resolved during this pass

- **C-HIGH "AUTH_SYS uid/gid passthrough → squash bypass" → RESOLVED.** The RPC layer (`middleware/auth.go:108`) does hand raw uid downstream, but squash **is** re-applied per-call: every v3 handler calls `BuildAuthContextWithMapping` (`v3/handlers/auth_helper.go:52`) which runs `reg.ApplyIdentityMapping` (all_squash/root_squash) on the live per-op credential; v4 mirrors via `v4/handlers/helpers.go:59`. So root/all-squash is **not** bypassable on stateless NFSv3 as the sub-audit feared. Residual MED for PR-B: confirm *every* mutating handler routes through `BuildAuthContextWithMapping` (godoc claims "all v3 handlers" — verify by grep, no silent bypass).

---

## 4. MED findings (defer as issues unless trivially co-fixable)

**v3 (A):** READDIRPLUS per-entry `Lookup`/`GetFile` fan-out on large dirs (hot-path amplification, `readdirplus.go:191/326/341`); CREATE/MKNOD/MKDIR/LINK non-atomic Lookup-then-create probe + CREATE drops client atime/mtime (`create.go:174/401`); EXCLUSIVE-create idempotency token not stored when verifier==0 (`create.go:223`); FSSTAT free-bytes unchecked subtraction underflow (`fsstat.go:190`); 64-bit→uint32 time truncation (Y2038, wire-correct today, `utils.go:70`); `Fsid` hardcoded 0 for all shares → cross-share fileid aliasing (`xdr/attributes.go:64`).

**v4 (B):** no `CLID_INUSE` principal check on SETCLIENTID (`manager.go:296`); EXCHANGE_ID unconditional reboot-purge w/o principal binding (`v41_client.go:200`); SeqRetry skips lease renewal + replays stale status flags (`sequence.go:54`); unbounded v4.0 state-map growth, reaper is v4.1-only (`manager.go:46`); LOCK-denied owner reported as clientid=0 + raw bytes (`manager.go:1735`); lease-timer expiry vs Renew race window (`lease.go:48`).

**C (RPC/GSS):** `ReadData` unchecked length→panic (`parser.go:129`); `reader.Read` instead of `io.ReadFull` in GSS decoders → silent short-read/zero-fill (`types.go:249`, `integrity.go:155`); multi-fragment RPC records not reassembled, `IsLast` never consulted (`connection.go:187`); krb5 mech OID skipped without validation (`framework.go:201`); AUTH_NULL verifier fallback on INIT MIC-compute failure (`dispatch.go:110`); per-call service downgrade logged not enforced (`framework.go:589`).

**D (NLM/NSM/portmap):** blocking-grant drain races queue mutex + waiter starvation (`nlm.go:300`); NLM_GRANTED callback no retransmit/ack → single loss strands lock (`callback/client.go:53`); TEST/GRANTED holder drops svid/oh (`test.go:146`); CANCEL/GRANT cross-mutex race (`cancel.go:73`); callback port hard-coded 12049 ignores client-advertised port (`lock.go:234`); NSM server state in-memory only, resets to 1 each boot → cross-restart monotonicity broken (`nlm.go:438`); SM_MON/UNMON/UNMON_ALL accept any remote caller, not loopback-gated (`mon.go:18`); PMAP_DUMP unrestricted info-disclosure (`dump.go:9`); UDP DUMP unauthenticated amplification surface (`server.go:249`).

**E (v4 types/XDR):** single global 1 MB opaque cap is right defense / wrong granularity — should be per-field (handle ≤128, owner ≤1024, token, payload) (`xdr/core/decode.go:33`); `v4/types` 32 files (~20 sub-100-LoC single-DTO) → collapse to ~6 op-family files, halves count, zero behavior change (`v4/types/`).

## 5. LOW findings

**v3:** `truncateExistingFile` swallows truncate error → size/content divergence (`create.go:543`); READ doesn't clamp Count to rtmax (benign, file-size clamp covers it); WRITE always returns UNSTABLE (documented); cookieverf mismatch intentionally ignored (macOS Finder, documented).
**v4:** downgrade/confirm replay synthesizes stateid; predictable time-based verifier fallback on crypto/rand fail (`manager.go:263`); 24-bit boot-epoch aliasing (`stateid.go:74`); missing v4.1 current-stateid sentinel (`types.go:193`).
**C:** duplicate AUTH_UNIX parsers (`auth.go:117` vs `unix.go:123`); `ReadData` always-nil error contract; AUTH_SHORT/AUTH_DES advertised but unhandled → silent anonymous; GSS handle not connection-scoped; PROG vs PROC_UNAVAIL nit (`pkg/adapter/nfs/dispatch.go:194`).
**D:** NLM `fh`/`oh`/`cookie` opaque not capped (`MaxOpaqueLen` defined unused, `nlm/xdr/decode.go:44`); NM_LOCK + async _MSG/_RES procs unimplemented (PROC_UNAVAIL); SHARE/UNSHARE always grant untracked (`share.go:89`); NSM callback `encodeStatus` hand-rolls padding (`nsm/callback/client.go:148`); portmap decoder accepts trailing garbage + no `prot` validation (`portmap/xdr/decode.go:14`).
**E:** two bitmap impls / two caps (8 vs 256, `attrs/bitmap.go` vs `v4/types/session_common.go`); `resolveGroupString` skips idmap (asymmetric vs owner, `encode.go:718`); CHANGE attr from ctime nanos (`encode.go:511`); XDR padding-zero not validated (`xdr/core/decode.go:53`); trivial 1-line helper files (`xdr/utils.go`, `pointers.go`); missing supported_attrs↔encoder drift test + adversarial-length tests; pseudofs READDIR-encoding seam unverified.

## 6. Verified-correct (checked, no finding)

RPC fragment-size bound (1.25 MB pre-alloc); AUTH_UNIX name/gid caps (≤255/≤16); all GSS opaque length caps (≤64K/≤1M); RPCSEC_GSS sequence-window replay bitmap (RFC 2203 §5.3.3.1); MAXSEQ → CTXPROBLEM; context Store-before-reply (Ganesha race avoided); krb5i/krb5p body MIC/Wrap + embedded seq_num dual-validate; program/version dispatch codes; v4 stale-stateid on boot-epoch change. v3 handlers respect all architecture invariants. v4.1 unaffected by the v4.0 replay bugs (uses slot table).
**D positives**: portmap SET/UNSET localhost-gated (`IsLocalhost` threaded through dispatch); PMAP_CALLIT omitted (no reflection/amplification, test-pinned); NLM byte-range locking delegates to the unified `pkg/metadata/lock` manager with cross-protocol conflict detection; NLM owner identity uses full `caller_name+svid+oh` triple; blocking-grant queue + drain + GRANTED callback wired end-to-end; NSM `priv` fixed-16 + str caps; boot-side own-reboot notify wired.
**E positives**: the **entire v4 wire/XDR layer is DoS-clean** — every `make([]T, count)` bounded before alloc (verified site-by-site); `io.ReadFull` throughout (no silent short-read); fattr4 SETATTR walks bits ascending + rejects non-writable; pseudofs fsid `(0,1)` + RWMutex concurrency correct; `nfs/types` is a proper protocol-DTO package (not a domain mirror).

## 7. Recommended PR-B shape

Split into focused fix PRs (do **not** one-shot 17 HIGHs):
1. **`v1.0/nfs-gss-fix`** — H1 (header MIC verify) + C-MED service-downgrade enforce + C-LOW handle scoping. **Security; highest priority.**
2. **`v1.0/nfs-v4-shareres`** — H2 (share_deny) + H3 (all-ones stateid). Self-contained integrity fixes.
3. **`v1.0/nfs-v4-replay`** — H4 + H5 + H6 (the three replay caches; shared mechanism).
4. **`v1.0/nfs-v3-interop`** — H9 (WCC, store-contract change) + H10 (wtmax) + H11 (READDIRPLUS maxcount).
5. **`v1.0/nfs-dos`** — H12 (DRC) + H13 (MaxConnections default).
6. **`v1.0/nlm-crash-cleanup`** — H14 (crashed-client lock release). Self-contained; pairs with the durability issue below.
7. **`v1.0/nsm-statd-auth`** — H16 (SM_NOTIFY sender auth + monitored-list gate) + H17 (state-number monotonicity). **Security; must land before the SM_NOTIFY relay is implemented.**
8. **H7 + H8 + H15 (grace / durable lock state)** — file as ONE design issue (`v1.0-nfs-lock-durability`): v4 open/lock state + NLM grace/reclaim share the same root gap (no durable lock journal). Needs the persistence decision, not a quick fix.

Each PR-B: apply HIGH → `code-simplifier` → `code-reviewer` → `go test -race` → verify. MED/LOW → backlog issues per area.

## 8. Audit coverage — COMPLETE

All of area #4 covered across 3 batches: v3 handlers + mount, v4 state machine + OPEN/LOCK handlers, v4.1 session handlers, RPC/RPCSEC_GSS/auth/dispatch, NLM (lockd), NSM (statd), portmap (rpcbind), v4 attrs/types/XDR, pseudofs, + 2nd-pass handlers + design/perf/bloat/versions/interface structural passes. No remaining unaudited NFS surface. Cross-cutting follow-up flagged for the **metadata/permissions sub-audit (area #6)**: confirm every mutating v3 handler routes through `BuildAuthContextWithMapping` (§3).

## 9. Structural / design / perf / bloat (batch 3 — see DESIGN-AUDIT.md + `_partial-f/g/i/j`)

Separate from the correctness tally; drives PR-B0 + Option-B restructure.

**Design (DESIGN-AUDIT.md)** — top structural findings:
- **SF-1 dual dispatch entry point** (HIGH-design): live `pkg/adapter/nfs/handlers.go` vs test-only `internal/adapter/nfs/dispatch.go:Dispatch()` — parallel routing, silent divergence, NLM/NSM auth only in live path.
- **SF-4 four auth-context builders** (HIGH-design): `buildV4AuthContext` silently falls back to UNMAPPED identity on mapping error (`v4/handlers/helpers.go:62`) while v3 propagates — security divergence. All funnel to `ApplyIdentityMapping` (squash NOT bypassable — confirms §3).
- **SF-6 `NeedsAuth` dead field**: 42 declarations, **0 consumers** (verified) — abandoned dispatch-auth gate.
- **SF-2/SF-3** 5 dispatch tables / 6 signatures / 3 result types; 5 context types with identical credential fields.
- **Recommendation**: Option A (auth unify, PR-B0) now + Option B (collapse dual dispatch + `RPCHandlerBase`) next sprint. Option C (rewrite) NOT warranted — handlers disciplined, only dispatch layer is accidentally complex.

**Perf (`_partial-g-perf.md`)** — measured M1 Max, Go 1.26:
- v4.1 COMPOUND **1556 ns/op, 1208 B/op, 62 allocs/op**; v4.0 **483 ns/op, 25 allocs** (v4.1 slot machinery ~3.2× slower).
- Allocation-bound. Top hotspot: unsized `bytes.Buffer` in `encodeCompoundResponse` (`compound.go:600`) = **27% / 2.92 GB** of allocation — pure `buf.Grow(estimate)` fix.
- Auth-context rebuilt UNCACHED on 7/10 v3 ops (lookup/readdir/readdirplus etc.) + all v4 — caching saves 1 GetShare + 1 ApplyIdentityMapping + 1 identity lookup per hot RPC.
- READDIRPLUS per-entry fan-out = O(n) round-trips (~4095→1 for a 4096-entry dir).
- **Gap**: only 3 benchmarks exist (all v4.1 SEQUENCE); v3 data path (READ/WRITE/LOOKUP/READDIRPLUS) has zero benchmarks. 5 micro-benchmarks to add. Mutex/block profiling never wired (baseline B3).

**Bloat (`_partial-f-bloat.md`)** — ~1,300–2,400 removable LOC, ~−53 files (≈205→152):
- Fold 22 `v3/handlers/*_codec.go` into handlers (−22 files); collapse `v4/types` 32→~6 op-families + drop unreferenced leaf `String()`s (−26 files); merge 4 auth builders; `compoundErr`/`compoundOK` helpers kill ~150 repeated `CompoundResult{}` literals; split 2,871-LOC `v4/state/manager.go`.
- Genuinely dead: `GroupResolver` (no impl), `ApplySetAttrs` (0 callers), `handleClientCrash` (no-op = H14). NFS comments are planning-ref CLEAN. The 4 `*Dispatcher` interfaces are load-bearing import-cycle breaks — KEEP.

**Versions/coexistence (`_partial-i-versions.md`)** — 3 HIGH (all already in REVIEW: grace/durability/share_deny) / 2 MED / 4 LOW:
- v3/v4.0/v4.1 only, no v2. v2-client → clean `PROG_MISMATCH[3,4]`; v4.2 → `NFS4ERR_MINOR_VERS_MISMATCH`. **Zero negotiation-correctness defects.**
- Cross-version lock visibility CORRECT (NLM + v4 LOCK both → unified `pkg/metadata/lock`, cross-protocol conflict detection) — NOT siloed. Recovery half broken (H7/H8/H15).
- MED: dual dispatcher (= SF-1) + `v4/types` over-fragmentation (= bloat).

**Interface audit (`_partial-j-interfaces.md`)** — 24 interfaces; **21 load-bearing, 1 dead, 1 over-fat, 4 cycle-break artifacts**:
- `GroupResolver` (`identity/mapper.go:75`) — **0 impls, 0 real callers → DELETE** (only dead interface found).
- `LockManager` (`lock/manager.go:27`) — 48 methods, 1 impl, 107 refs → fat (ISP smell); SPLIT into role interfaces, low priority, not v1.0-blocking.
- **Key positive**: NLM + v4-state + SMB all share ONE `LockManager` — NO siloed/parallel lock interfaces. This is what makes cross-protocol conflict detection work. The 4 `*Dispatcher` interfaces are `pkg`↔`internal` import-cycle breaks (resolve via Option B / SF-1, not deletion). `IdentityMapper`/`Verifier`/lock-store seams all genuine. Interface surface is healthy — the real fragmentation is at the *function* level (4 auth builders), not interfaces.

**Net area-4 PR-B picture**: REVIEW §7 correctness PRs (now incl. H18/H19 in a `nfs-v4-stateid` slice) + PR-B0 auth-unify (DESIGN Option A) + perf quick-wins (`buf.Grow` + auth cache) + a separate bloat/restructure track (Option B).
