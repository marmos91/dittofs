# Area 3 — SMB2/3 adapter — PR-A Audit (REVIEW.md)

**Status**: AUDIT COMPLETE — awaiting PR-B triage/kickoff.
**Branch**: `v1.0/smb-audit` @ `origin/develop@a07c497d`.
**Date**: 2026-06-02.
**Scope**: `internal/adapter/smb/` + `pkg/adapter/smb/` — ~47.4K src LOC / ~43K test LOC (measured; 75 src files across 12 sub-packages: handlers/, auth/, session/, signing/, encryption/, smbenc/, kdf/, header/, lease/, rpc/, types/, + 11 root dispatch/compound/framing files).
**Cross-check refs**: MS-SMB2 (§2.2.2 error response, §3.3.5 request processing, §3.3.4.7 break fan-out, §3.3.5.9 oplock grant, §3.3.5.5 session bind, §2.10.6 replay), MS-FSCC, MS-DTYP, MS-NLMP, RFC 4178 SPNEGO, RFC 4121 Kerberos GSS; Samba `source3/smbd` + `source4/torture`.

**Method**: 4 read-only sub-audits over the four functional slices — (A) dispatch/compound/credits/framing, (B) auth/session/signing/encryption, (C) CREATE/lease/oplock/durable, (D) info-class/ACL/IOCTL/notify/read-write. This was the one Wave-1 area deferred for conformance; smbtorture is now COMPLETE on develop, so the audit was unblocked. **`go build ./...` clean on develop.**

This is the first REVIEW.md for area #3 (no prior REVIEW2.md / DESIGN-AUDIT.md).

---

## 1. Summary

| Sub-area | HIGH | MED | LOW |
|---|---|---|---|
| A — dispatch / compound / credits / framing | 0 | 2 | 3 |
| B — auth / session / signing / encryption | 0 | 1 | 3 |
| C — CREATE / lease / oplock / durable | 0 | 1 | 2 |
| D — info-class / ACL / IOCTL / notify / R-W | 0 | 2 | 3 |
| **Total** | **0** | **6** | **11** |

**Architecture invariants: clean.** Handlers handle only protocol/wire concerns; business logic stays in `pkg/metadata` + stores; `*metadata.AuthContext` is threaded everywhere (`BuildAuthContext`/`primeAuthContextFromOpenFile`); file handles are opaque (decoded only for LockManager share routing via `metadataServiceResolver`); block stores resolved per-share via `common.ResolveForWrite`/`ResolveForRead`; the WRITE path follows the mandated order **`PrepareWrite` → `ResolveForWrite` → `WriteToBlockStore` → `CommitWrite`** (`handlers/write.go:391-419`). Single dispatch table (`dispatch.go`), no dual-dispatch, no NFS-style parallel routing seam.

**Verdict: PATCH-grade — the cleanest protocol adapter in the audit set.** **Zero HIGH correctness, security, or invariant findings.** This is consistent with the conformance posture: smbtorture is fully green (0 deferred non-appendix entries) after the multi-session conformance campaign (#673), and the source carries dense MS-SMB2 / Samba citations on nearly every gate. Every enforcement gate this audit went looking for (signing-required disconnect, encryption-required, session-bind step-1, per-info-class access, share-mode conflict, copychunk source/dest access, SACL/`ACCESS_SYSTEM_SECURITY`, WRITE/READ `GrantedAccess` post-DACL) **is present and spec-cited.** Findings are (1) **structural bloat** — oversized god-files + a ~180-line duplicated compound loop + a mis-named "stub" file that is really 1.1K LOC of live handlers — and (2) a handful of **robustness MEDs** (READ length not clamped to advertised `MaxReadSize`; unlimited default `MaxConnections`; transitional no-NT-hash auth bypass that is already logged as SECURITY). No tag-gating gap.

**Theme**: where NFS area #4 had its holes clustered in security gates + recovery/exactly-once (because those paths had no test forcing them), SMB's equivalents were all *driven* by smbtorture and are correct. The residual SMB debt is the inverse — **accidental complexity** accreted while chasing conformance flips (the async-CREATE compound machinery, the four near-identical compound loops, handler.go growth), not missing enforcement.

---

## 2. HIGH findings

**None.** No auth-bypass surface, no unauthenticated mutating operation, no missing enforcement gate, no invariant break. See §6 for the verified-correct enforcement gates that were specifically probed.

The closest things to elevated risk — all classified MED/LOW with rationale in §3/§4 — are: the transitional "user with no NT hash authenticates without credential validation" path (already a logged `SECURITY` warning and an operator-config issue, not a code bug — M-B1); READ not clamping request length to the advertised `MaxReadSize` (bounded by the 64 MB frame cap — M-D1); and the unlimited default `MaxConnections` (same class as NFS H13 — L-A3, kept LOW because SMB also caps in-flight requests per connection at 100 and the live-settings path can cap connections).

---

## 3. MED findings (defer as issues unless trivially co-fixable)

### A — dispatch / compound / credits

- **M-A1 — `completeCompoundAfterAsyncCreate` duplicates the entire compound-processing loop (~180 LOC).** `compound.go:938-1126` re-implements the related-op error propagation, FileID inheritance, CHANGE_NOTIFY-non-last gate, per-subcommand signature verify, and PostSend collection that `ProcessCompoundRequest:251-457` already contains — line-for-line. Two copies of intricate MS-SMB2 §3.3.5.2.7.2 logic will drift (a fix to one is silently missed in the other; the async path already differs subtly — it always returns `StatusInvalidParameter` for a session-level predecessor at `:1008` instead of calling `relatedSessionFailureStatus`, so the SESSION_EXPIRED propagation fix is NOT mirrored on the async-CREATE path). **Fix**: extract the per-subcommand body into a shared `processCompoundTail(state, …)` consumed by both entry points. Highest-leverage structural cleanup in the area.

- **M-A2 — `VerifyCompoundCommandSignature` / `VerifyRequest` silently skip verification when the session lookup fails (`if !ok { return nil }`).** `compound.go:799-802`, `framing.go:400-403`. The comment-stated intent is "let `prepareDispatch` return `STATUS_USER_SESSION_DELETED`", and `prepareDispatch` does reject `NeedsSession` commands for an unknown SessionID — so the gap is closed in practice for every dispatched command. But the skip is unconditional (not gated on the command later being session-rejected), so it is one refactor away from a bypass if a future `NeedsSession=false` command is added that touches state. **Fix**: keep the skip but assert/document that every command reaching the handler with a non-zero SessionID has been re-validated by `prepareDispatch`; or fold the unknown-session decision into the verifier so it returns `ErrSignatureVerification` rather than nil for a non-NEGOTIATE/SS command. Low real-world risk today; flagged for defense-in-depth.

### B — auth / session / signing

- **M-B1 — User with no configured NT hash authenticates with NO credential validation.** `session_setup.go:1218-1252`. When a UserStore user exists+enabled but has no NT hash, any client presenting that username is authenticated as that user (no NTLMv2 verify), with signing disabled. This is already emitted as a `logger.Warn("SECURITY: User authenticated without credential validation …")` and is a transitional/operator-config state (fix = `dittofs user passwd`), not a wire-protocol bug — but it is a real auth-bypass-by-misconfiguration surface. **Fix (policy)**: add a server setting (default off) that refuses login for users lacking an NT hash, so a half-provisioned user fails closed instead of authenticating open. Track as a security-hardening issue, not a v1.0 blocker.

### C — CREATE / lease / oplock / durable

- **M-C1 — `Adapter.OnReconnect` durable-handle/lease reclaim is a documented stub.** `pkg/adapter/smb/adapter.go:557-587`: the body only logs; the docstring states a "full implementation would enumerate persisted leases … and call HandleLeaseReclaim" and relies on *implicit* reclaim when the client re-requests the same lease key during grace. smbtorture's durable/lease suite passes because the implicit path covers the tested flows, but explicit server-driven reclaim on restart is absent. Pairs with NFS H7/H8/H15 (the cross-protocol durable-lock-state durability gap already filed as `v1.0-nfs-lock-durability`). **Fix**: fold SMB durable/lease restart-reclaim into that same durability design issue rather than a standalone fix — it is the same persisted-lock-state question.

### D — info-class / ACL / IOCTL / notify / read-write

- **M-D1 — READ does not clamp `req.Length` to the advertised `MaxReadSize` (1 MB).** `handlers/read.go` validates offset overflow and gates access but never compares `req.Length` against `h.MaxReadSize` (NEGOTIATE advertises 1 MB, `handler.go:806`). A client can request up to the 64 MB `MaxMessageSize`, forcing a 64 MB serve/alloc. Windows/Samba return `STATUS_INVALID_PARAMETER` for a READ exceeding the negotiated max. Bounded by the frame cap so not a HIGH DoS, but it (a) violates the advertised contract and (b) lets one request allocate 64× the advertised ceiling. **Fix**: `if req.Length > h.MaxReadSize { return STATUS_INVALID_PARAMETER }` (and the symmetric check on WRITE `req.Length` vs `MaxWriteSize`).

- **M-D2 — `stub_handlers.go` is a 1148-LOC misnomer mixing live handlers with genuine stubs.** It contains `Cancel`, `ChangeNotify`, `OplockBreak`, lease/oplock-break-ACK dispatch, and IOCTL helpers (all live, load-bearing) alongside actual stubs (`handleReadFileUsnData` returns zeros, `handleGetNtfsVolumeData`). The name advertises dead/placeholder code and hides ~1K LOC of real dispatch from a reader grepping for handlers. **Fix**: split into `cancel.go`, `change_notify_dispatch.go` (or fold into `change_notify.go`), `oplock_break.go`, and a small honest `unimplemented_fsctl.go` for the true stubs.

## 4. LOW findings

**A (dispatch):**
- **L-A1** `buildCompoundParseErrorResponse` (`compound.go:878`) hand-rolls a synthetic header + grants `requested=1` credit on an unparseable frame — fine, but the magic `1` and the literal `[4]byte{0xFE,'S','M','B'}` should reuse `header` constants.
- **L-A2** `fileIDOffset`/`ExtractFileID`/`InjectFileID` (`compound.go:846-925`) encode per-command wire offsets by hand; if a new FileID-carrying command is added the table is silently incomplete (returns `-1` → no inheritance). Low risk; add a compile-time/test cross-check against the command set.
- **L-A3** Default `MaxConnections = 0` (unlimited) — `config.go:54`. Same class as NFS **H13**, kept LOW here because (i) per-connection in-flight requests are capped at 100 (`MaxRequestsPerConnection`), (ii) the live-settings `preAcceptCheck` path enforces a runtime cap when configured. Ship a sane static default (e.g. 1024) for parity with NFS.

**B (auth/session/signing):**
- **L-B1** `BuildChallenge` (`auth/ntlm.go:351`) ignores the `rand.Read` error (`_, _ = rand.Read(...)`); a (near-impossible) RNG failure yields a zero server challenge. Promote to a logged error / fail the SESSION_SETUP.
- **L-B2** `DeriveSigningKey` (`auth/ntlm.go:906-931`) silently falls back to `sessionBaseKey` on a bad KEY_EXCH length / RC4 error rather than failing — produces a wrong-but-non-erroring signing key (client then rejects). Low impact (client-visible failure) but a `Debug`/`Warn` would aid diagnosis.
- **L-B3** `extractNTLMToken` / SPNEGO branch does a raw `findNTLMSSP` byte-scan fallback (`session_setup.go:485-493, 517-522`) when ASN.1 parse fails — pragmatic for odd clients, but accepts an NTLMSSP blob embedded anywhere in the buffer. Documented; keep but note the looseness.

**C (CREATE/lease/durable):**
- **L-C1** `handler.go` is a 2385-LOC / 72-function god-file (server config + session/tree accessors + open-file table + state-snapshot + NEGOTIATE defaults). Cohesive but oversized; split along the same lines as the NFS `manager.go` recommendation (config / open-table / accessors).
- **L-C2** `findDurableHandleStore` (`adapter.go:526-555`) disables durable handles entirely when >1 metadata store is registered (`TODO: resolve per-share`). Correct fail-safe, but durable handles silently vanish in multi-store topologies — surface as a startup `Warn` (it does) and track the per-share resolution as a backlog item.

**D (info/ACL/IOCTL/notify):**
- **L-D1** `MapToSMB`/`MapContentToSMB` default unmapped errors to `StatusInternalError` (`common/errmap.go:304`, `content_errmap.go`). Reasonable catch-all, but a few storefront errors map to `INTERNAL_ERROR` where `STATUS_INVALID_PARAMETER`/`STATUS_DISK_FULL` would be more client-actionable — audit the table rows for SMB-specific overrides (the table already has an NFS column; SMB column has fewer overrides).
- **L-D2** `handleReadFileUsnData` returns stub zeros for USN/TimeStamp/Reason (`stub_handlers.go:999`); benign (USN journal is not implemented) but a client relying on the USN record gets silent zeros rather than `STATUS_NOT_SUPPORTED`. Confirm the FSCTL is not advertised as supported.
- **L-D3** `set_info.go` (2176 LOC) + `query_info.go` (1391) + `create.go` (2331) are large but per-info-class cohesive; lower-priority split candidates after handler.go.

## 5. Triage downgrades / non-findings

- The async-CREATE compound machinery (`compound.go:149-207`, the `ReplaceCallback`/`MarkStarted` parked-CREATE gate) reads as alarmingly intricate, but every twist is pinned to a named smbtorture test (`compound.compound-break`, `compound_async.getinfo_middle`, `compound_find.compound_find_close`) and the ordering rationale is documented inline. **Not a correctness finding** — it is correct and tested. It IS the strongest argument for the M-A1 dedup (one shared, well-tested implementation beats two divergent copies of this).
- Signing/encryption "skip on session lookup miss" (M-A2) was considered as a HIGH auth-bypass but **downgraded to MED**: `prepareDispatch` independently rejects every `NeedsSession` command on an unknown/logged-off session before the handler runs, so no dispatched mutating op escapes the gate today. The finding is the *fragility* of relying on that, not a live hole.
- `verifyChannelSequence` (response.go:128 / `channel_sequence.go`) correctly rejects stale modifying replays (MS-SMB2 §3.3.5.2.10) — verified present, not a gap.

## 6. Verified-correct (probed, no finding)

- **Signing enforcement**: `SigningRequired && !signed` → disconnect (`framing.go:420-432`); SMB 3.1.1 authenticated + unsigned + unencrypted → disconnect (`framing.go:440-448`, `compound.go:813-819`); signed-but-wrong → `ACCESS_DENIED` with the reply signed by the session key, connection kept (Samba parity, `response.go:830`).
- **Encryption enforcement**: global `mode=required` and per-share `EncryptData` both reject unencrypted post-SS commands (`response.go:checkEncryptionRequired`); encrypted-request → encrypted-reply (MS-SMB2 §3.3.4.1.4, `sendDispatchError`/`compoundShouldEncrypt`); inner/transform SessionID mismatch rejected (`framing.go:154`); 5-strike decrypt-failure connection drop (`connection.go:301`); anon/guest sessions correctly bypass (no keys).
- **Session-bind gating (MS-SMB2 §3.3.5.5.2)**: full 9-step Samba-ordered validation (`session_setup.go:545-699`); GMAC-symmetry; dialect/cipher match; guest/null rejected; channel cap 32 (`StatusInsufficientResources`); re-bind on already-bound connection rejected; per-channel signing key derived from the **bind's own** session key (not the origin's). Re-auth on a bound channel is signature-verified (`verifyReauthChannelSignature`).
- **Auth fail-closed**: wrong NTLMv2 → `LOGON_FAILURE` (not silent guest); malformed NTLMv2 blob → `INVALID_PARAMETER` (`ntlmssp_bug14932`); failed *re-auth* destroys the session (no identity strip, MS-SMB2 §3.3.5.5.3); guest only when no credentials presented AND gated by `GuestEnabled`/`checkGuestPolicy`; NTLM gated by `NtlmEnabled`; bind never downgrades to guest.
- **Access gates (post-DACL `Open.GrantedAccess`, not pre-DACL DesiredAccess)**: WRITE `hasWriteAccess` (`write.go:196`), READ `hasReadAccess` (`read.go:196`), per-info-class `fileInfoClassRequiredAccess` → `FILE_READ_ATTRIBUTES` (`query_info.go:288-295`), SET_INFO `FILE_WRITE_ATTRIBUTES`/delete/`WRITE_DAC`/`WRITE_OWNER` (`set_info.go:212-264`), security-query `READ_CONTROL` for owner/group/DACL + `ACCESS_SYSTEM_SECURITY` for SACL (`query_info.go:382-394`), copychunk source `FILE_READ_DATA|FILE_EXECUTE` + dest checks (`ioctl_copychunk.go:246-271`). Handle TreeID/SessionID re-bound to the owning open on every READ/WRITE/CHANGE_NOTIFY (`smb2.tcon`).
- **Share-mode / deny**: `checkShareModeConflict` → `STATUS_SHARING_VIOLATION` (`create.go:1373`) — the SMB-side enforcement NFS H2 is missing.
- **Framing / DoS**: NetBIOS length bounded by `MaxMessageSize` (64 MB, `framing.go:238`); `io.ReadFull` throughout (no short-read); min-size floor; keepalive handled; per-connection in-flight cap 100; compound `NextCommand` 8-byte-alignment + magic validated (`ParseCompoundCommand`); CHANGE_NOTIFY in non-last compound position → `INTERNAL_ERROR`.
- **Credit accounting**: compound-level charge on first command; middle responses grant 0 + window `Reclaim`; CANCEL exempt from Consume (reuses target MessageID); `grantConnectionCredits` extends the window synchronously pre-write to close the TOCTOU (`response.go:698`). The full `smb2.credits` suite passes (per KNOWN_FAILURES).
- **Multichannel session survival (§3.3.7.1)**: connection close removes only its channel; session torn down only at last-channel; break-routing entry deleted only when it points at the closing conn (`connection.go:535-586`).
- **CHANGE_NOTIFY**: bounded maps with tombstone GC (`gcCancelTombstonesLocked`), MessageID-collision eviction (#416), overflow → `STATUS_NOTIFY_ENUM_DIR` + sticky handle flag (§3.3.5.19). QUERY_DIRECTORY builds entries incrementally against `OutputBufferLength`.
- **NTLM mechListMIC**: correct legacy (CRC32/RC4) vs extended-security (HMAC-MD5) branch + sealing-key strength truncation (40/56/128-bit) matching Samba — the subtle KDF that gated `anon-signing1/2`.
- **Lease/oplock break races**: break paths run async (`BreakHandleLeasesOnOpenAsync`) to avoid single-threaded-client deadlock, with documented lock ordering on `lm.mu`; WRITE breaks Level-II leases + purges conflicting disconnected durable handles (§3.3.5.16).

## 7. #673 conformance status

**Verdict: #673 is CLOSEABLE.** `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` (updated 2026-06-02) records the v1.0 conformance gate as **met**: *"No UNJUSTIFIED entries remain — the #673 acceptance criterion is met."*

Final tally (non-Kerberos): **48 total known entries, every one justified**, in exactly these buckets:
- **Upstream Samba known-fail — 2**: `charset.Testing` (partial-surrogate failure in the smbtorture client's own `iconv`, fails against reference Samba too), `session.reauth5` (Samba `selftest/knownfail:213` — asserts stricter-than-Samba anonymous CREATE-on-nonexistent semantics).
- **Deferred past v1.0 — 0**: the last deferred rows (the 6 `dhv2-pending2*-sane` durable-replay cases) flipped under #749 (parked-CREATE now finalizes on holder-release, not just break-timeout). **No remaining deferrals.**
- **Permanently unimplementable / harness-only — 46**: Samba-internal test FSCTLs (`torture_block_tcp_transport`, `FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT`), kernel `F_SETLEASE` oplocks, NTFS 8.3 name-mangling, VSS/TWRP shadow copies, NTFS `$Extend\$Quota` fake files, Samba `smb.conf` glob knobs (`hide files`/`dosmode`), parameterized driver tests needing `--option=`, the `smb2.maxfid` 65520-CREATE wall-clock test, smbtorture-4.22.6 **client** SIGSEGVs (`scan.scan`, `dirlease.oplocks`), the 20 `replay.dhv2-pending*-windows` arms (Windows IRP-scheduling artifacts Samba's own source does not reproduce → no spec-conformant target). Each is individually documented with an architectural reason; the appendix is the only place entries may live without a GH sub-issue.

Kerberos: 1 row (`reauth5`, upstream knownfail) in `KNOWN_FAILURES_KERBEROS.md`, loaded only under `--use-kerberos`, **not part of the v1.0 CI gate**.

There are **no open SMB sub-issues blocking the gate** and no `*-sane` (genuinely-fixable) rows outstanding. The MED/LOW findings in this REVIEW are *internal-quality* items the conformance suite cannot see (bloat, the duplicated compound loop, READ length-clamp, the no-NT-hash auth policy) — none of them regress a conformance test. **Recommend closing #673** with a pointer to this REVIEW for the residual non-conformance hardening (PR-B below).

## 8. Recommended PR-B shape

All structural; none gate the tag. Ship off `develop` tip, not chained.

1. **`v1.0/smb-compound-dedup`** — M-A1: extract `processCompoundTail` shared by `ProcessCompoundRequest` + `completeCompoundAfterAsyncCreate`; fold the SESSION_EXPIRED-propagation fix into the shared path. Highest-leverage; closes a real drift.
2. **`v1.0/smb-read-write-clamp`** — M-D1: clamp READ `req.Length` ≤ `MaxReadSize` and WRITE `req.Length` ≤ `MaxWriteSize` → `STATUS_INVALID_PARAMETER`. Tiny, contract-correctness.
3. **`v1.0/smb-file-split`** — M-D2 + L-C1 + L-D3: rename/split `stub_handlers.go` (cancel/change_notify_dispatch/oplock_break/unimplemented_fsctl), split `handler.go` god-file, optionally `set_info.go`/`query_info.go`/`create.go`. Pure move, no logic change.
4. **`v1.0/smb-auth-hardening`** — M-B1 (refuse-no-NT-hash setting) + L-B1 (rand error) + L-B2 (KEY_EXCH fallback log). Security-hardening, default-off policy.
5. **Durable/lease restart-reclaim (M-C1)** — fold into the existing cross-protocol `v1.0-nfs-lock-durability` design issue (NFS H7/H8/H15); same persisted-lock-state question, do not solve standalone.
6. **`v1.0/smb-dos-default`** — L-A3: ship a static `MaxConnections` default for NFS parity.
7. Backlog: L-A1/L-A2 (compound header/offset-table hardening), L-B3 (SPNEGO scan looseness doc), L-C2 (per-share durable store), L-D1 (errmap SMB overrides), L-D2 (USN stub → NOT_SUPPORTED or confirm unadvertised).

Each PR-B: HIGH/MED fix → `code-simplifier` → `code-reviewer` → `go test -race` (+ targeted smbtorture rerun for #1/#2) → verify. LOW → backlog issues.

## 9. Audit coverage — COMPLETE

All of area #3 covered: dispatch/compound/credit/framing (root `internal/adapter/smb/*.go` + `pkg/adapter/smb/{adapter,connection}.go`), auth (NTLM/SPNEGO/Kerberos session_setup + mechListMIC), session/signing/encryption/KDF, CREATE/lease/oplock/durable state machine, info-class/ACL/security-descriptor/IOCTL/copychunk/sparse/change-notify/read-write, lsarpc/types. No remaining unaudited SMB surface. Cross-cutting: the SMB durable-lock-state gap (M-C1) is the same root as NFS area #4 H7/H8/H15 — consolidate in one durability design issue.
