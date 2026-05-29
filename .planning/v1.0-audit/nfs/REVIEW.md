# Area 4 — NFS handlers — PR-A Audit (REVIEW.md)

**Status**: AUDIT COMPLETE — awaiting PR-B triage/kickoff.
**Branch**: NFS audit run on `v1.0/blockstore-perf-b1` tree (= `develop` + B1/B2/B3 perf). PR-A to be opened off `develop`.
**Date**: 2026-05-29.
**Scope**: `internal/adapter/nfs/`, `pkg/adapter/nfs/` — ~56K src LOC (measured; PLAN headline 52.9K was the Wave-0 estimate).
**Cross-check refs**: RFC 1813 (v3), RFC 7530 (v4.0), RFC 8881 (v4.1), RFC 5531 (ONC RPC), RFC 2203 (RPCSEC_GSS), RFC 2623 (AUTH_SYS over NFS); Linux `fs/nfsd/`, `net/sunrpc/`.

**Method**: 3 parallel read-only sub-audits on the highest-value, independent surfaces. Aux protocols (NLM/NSM/portmap/mount handlers ~6K) and v4 attrs/types (~8K) deferred to a 2nd batch.

**Agent outputs** (raw findings, kept for provenance):
- `_partial-a-v3.md` — NFSv3 handlers (12.5K) + mount handlers
- `_partial-b-v4state.md` — NFSv4 state machine (7.7K) + v4/v4.1 OPEN/LOCK handlers
- `_partial-c-rpcgss.md` — RPC framing + RPCSEC_GSS + AUTH_SYS + dispatch (3.9K)

This REVIEW.md consolidates and triages the three.

---

## 1. Summary

| Sub-area | HIGH | MED | LOW |
|---|---|---|---|
| A — v3 handlers | 3 | 6 | 4 |
| B — v4 state machine | 7 | 7 | 4 |
| C — RPC / GSS / auth | 3¹ | 6 | 5 |
| **Total (post-triage)** | **13** | **19** | **13** |

¹ C originally reported 4 HIGH; the AUTH_SYS-squash-bypass HIGH was **downgraded to RESOLVED** during triage (see §3).

**Architecture invariants: clean.** All three sub-audits confirm handlers respect CLAUDE.md rules — business logic stays in `pkg/metadata`, `*metadata.AuthContext` is threaded everywhere, file handles are opaque (decode only for routing), WRITE follows Prepare→BlockStore→Commit. **Every HIGH is correctness / interop / security drift, not an invariant break.** The code is well-commented and cites RFCs heavily.

**Verdict**: the NFSv4 **state machine** carries real v1.0-blocking integrity holes (share reservations are a silent no-op; a special stateid bypasses WRITE checks; both v4.0/v4.1 replay caches are broken). The **RPCSEC_GSS** path has a complete krb5 auth bypass. NFSv3 is the healthiest layer (interop/WCC drift only). Grade: **NEEDS-FIX** before tag — heavier than blockstore's PATCH.

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

---

## 3. Triage downgrades / resolved during this pass

- **C-HIGH "AUTH_SYS uid/gid passthrough → squash bypass" → RESOLVED.** The RPC layer (`middleware/auth.go:108`) does hand raw uid downstream, but squash **is** re-applied per-call: every v3 handler calls `BuildAuthContextWithMapping` (`v3/handlers/auth_helper.go:52`) which runs `reg.ApplyIdentityMapping` (all_squash/root_squash) on the live per-op credential; v4 mirrors via `v4/handlers/helpers.go:59`. So root/all-squash is **not** bypassable on stateless NFSv3 as the sub-audit feared. Residual MED for PR-B: confirm *every* mutating handler routes through `BuildAuthContextWithMapping` (godoc claims "all v3 handlers" — verify by grep, no silent bypass).

---

## 4. MED findings (defer as issues unless trivially co-fixable)

**v3 (A):** READDIRPLUS per-entry `Lookup`/`GetFile` fan-out on large dirs (hot-path amplification, `readdirplus.go:191/326/341`); CREATE/MKNOD/MKDIR/LINK non-atomic Lookup-then-create probe + CREATE drops client atime/mtime (`create.go:174/401`); EXCLUSIVE-create idempotency token not stored when verifier==0 (`create.go:223`); FSSTAT free-bytes unchecked subtraction underflow (`fsstat.go:190`); 64-bit→uint32 time truncation (Y2038, wire-correct today, `utils.go:70`); `Fsid` hardcoded 0 for all shares → cross-share fileid aliasing (`xdr/attributes.go:64`).

**v4 (B):** no `CLID_INUSE` principal check on SETCLIENTID (`manager.go:296`); EXCHANGE_ID unconditional reboot-purge w/o principal binding (`v41_client.go:200`); SeqRetry skips lease renewal + replays stale status flags (`sequence.go:54`); unbounded v4.0 state-map growth, reaper is v4.1-only (`manager.go:46`); LOCK-denied owner reported as clientid=0 + raw bytes (`manager.go:1735`); lease-timer expiry vs Renew race window (`lease.go:48`).

**C (RPC/GSS):** `ReadData` unchecked length→panic (`parser.go:129`); `reader.Read` instead of `io.ReadFull` in GSS decoders → silent short-read/zero-fill (`types.go:249`, `integrity.go:155`); multi-fragment RPC records not reassembled, `IsLast` never consulted (`connection.go:187`); krb5 mech OID skipped without validation (`framework.go:201`); AUTH_NULL verifier fallback on INIT MIC-compute failure (`dispatch.go:110`); per-call service downgrade logged not enforced (`framework.go:589`).

## 5. LOW findings

**v3:** `truncateExistingFile` swallows truncate error → size/content divergence (`create.go:543`); READ doesn't clamp Count to rtmax (benign, file-size clamp covers it); WRITE always returns UNSTABLE (documented); cookieverf mismatch intentionally ignored (macOS Finder, documented).
**v4:** downgrade/confirm replay synthesizes stateid; predictable time-based verifier fallback on crypto/rand fail (`manager.go:263`); 24-bit boot-epoch aliasing (`stateid.go:74`); missing v4.1 current-stateid sentinel (`types.go:193`).
**C:** duplicate AUTH_UNIX parsers (`auth.go:117` vs `unix.go:123`); `ReadData` always-nil error contract; AUTH_SHORT/AUTH_DES advertised but unhandled → silent anonymous; GSS handle not connection-scoped; PROG vs PROC_UNAVAIL nit (`pkg/adapter/nfs/dispatch.go:194`).

## 6. Verified-correct (checked, no finding)

RPC fragment-size bound (1.25 MB pre-alloc); AUTH_UNIX name/gid caps (≤255/≤16); all GSS opaque length caps (≤64K/≤1M); RPCSEC_GSS sequence-window replay bitmap (RFC 2203 §5.3.3.1); MAXSEQ → CTXPROBLEM; context Store-before-reply (Ganesha race avoided); krb5i/krb5p body MIC/Wrap + embedded seq_num dual-validate; program/version dispatch codes; v4 stale-stateid on boot-epoch change. v3 handlers respect all architecture invariants. v4.1 unaffected by the v4.0 replay bugs (uses slot table).

## 7. Recommended PR-B shape

Split into focused fix PRs (do **not** one-shot 13 HIGHs):
1. **`v1.0/nfs-gss-fix`** — H1 (header MIC verify) + C-MED service-downgrade enforce + C-LOW handle scoping. **Security; highest priority.**
2. **`v1.0/nfs-v4-shareres`** — H2 (share_deny) + H3 (all-ones stateid). Self-contained integrity fixes.
3. **`v1.0/nfs-v4-replay`** — H4 + H5 + H6 (the three replay caches; shared mechanism).
4. **`v1.0/nfs-v3-interop`** — H9 (WCC, store-contract change) + H10 (wtmax) + H11 (READDIRPLUS maxcount).
5. **`v1.0/nfs-dos`** — H12 (DRC) + H13 (MaxConnections default).
6. **H7 + H8 (grace/durability)** — file as a design issue (`v1.0-nfs-state-durability`); needs the persistence decision, not a quick fix.

Each PR-B: apply HIGH → `code-simplifier` → `code-reviewer` → `go test -race` → verify. MED/LOW → backlog issues per area.

## 8. Remaining audit gap (2nd batch)

Not yet audited: aux protocols (`nlm/`, `nsm/`, `portmap/`, `mount/handlers/` ~6K), v4 `attrs/` + `types/` (~8K), `pseudofs/`. Schedule a follow-up sub-audit before declaring area #4 PR-A fully complete.
