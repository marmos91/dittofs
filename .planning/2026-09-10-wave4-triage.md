# Wave 4 SMB triage — re-derivation results (2026-09-10)

Five read-only reviewer lanes re-derived the 157 findings in
`.planning/audits/2026-09-07-smb-adapter/report.md` (pinned at `40884ad4f`) against current
`origin/develop` (`15d3a50cf`). Verdicts: **FIXED** (closed on develop), **LIVE** (still
present verbatim), **WAVE5** (structure-only, belongs to the god-object wave), plus
DRIFTED/INVALID (none found). Workflow `ec014755`, mission `593837f3`.

## Tally (per-lane summary lines)

| Lane | TOTAL | FIXED | LIVE | WAVE5 |
| --- | --- | --- | --- | --- |
| T1 set-info/read-write/negotiate/durable/crypto/connection | 52 | 0 | 17 | 35 |
| T2 compound/tree-connect/auth/response-pipeline/session/lease-manager | 47 | 4 | 18 | 25 |
| T3 conn-dispatch/security-sd/ioctl-core/core-handler/lease-oplock/create-post-break | 40 | 1 | 16 | 23 |
| T4 create | 8 | 0 | 2 | 6 |
| T5 pkg-adapter/ntlm-spnego + HIGH cross-checks | 12 | 2 | 8 | 2 |
| **Total** | **159 rows (157 findings; 2 dup pairs counted twice)** | **7** | **61** | **91** |

Duplicate-root pairs to de-dup when scheduling fixes: AppInstanceId force-close (T1 durable
+ T4 create + T5 cross-check = one defect), AEAD nonce reuse (T1 crypto rows 2/3 = one
defect), the two Serve()-leak findings in T5 (one defect, both FIXED by the base.go
post-bind shutdown re-check).

## FIXED (7 rows, 6 defects — closed by landed fixes)

| Finding | Lane | Closed by |
| --- | --- | --- |
| TREE_DISCONNECT no tree-ownership check (MED/HIGH, two rows) | T2 | #2424/#2413 prepareDispatch gate |
| TREE_DISCONNECT + every NeedsTree op (HIGH gaps, two rows) | T2 | #2424/#2413 |
| CANCEL of another session's parked CREATE (AsyncId ownership) | T3 | #2426/#2414 |
| Serve() unbounded pre-bind work → Stop-before-bind leak (two rows) | T5 | #2502 base.go post-bind shutdown re-check |

## LIVE behavioral defects (61 rows → ~55 defects after de-dup)

### Priority HIGHs (all three cross-checks LIVE verbatim)

| Finding | Where | Evidence |
| --- | --- | --- |
| Fresh SESSION_SETUP keeps unclaimed nonzero client SessionId | `internal/adapter/smb/handlers/session_setup.go:976-984` | `sessionID := ctx.SessionID; if sessionID == 0 {GenerateSessionID()} else if GetSession ok {isReauth=true}` — a miss falls through keeping the client-supplied ID |
| Anonymous/guest bypass defeats per-share SMB2_SHAREFLAG_ENCRYPT_DATA | `internal/adapter/smb/response.go:709-713` | `if sess.IsNull | | sess.IsGuest { return 0 }` runs before the per-share `tree.EncryptData` check at :731-738 |
| AppInstanceId failover force-closes any user's handle, any share | `internal/adapter/smb/handlers/durable_context.go:907,970` + `create.go:1449-1451` | Filter `f.AppInstanceId == appId` only across sessions; no share/path/maximal-access scoping (MS-SMB2 3.3.5.9.13 absent) |

## Lane T1 — full re-derivation table

## Lane T1 triage — re-derivation table

| Finding (abbrev) | Verdict | Current location | Evidence |
| --- | --- | --- | --- |
| SET_INFO FileAllocationInformation short-buffer no-op | LIVE | `internal/adapter/smb/handlers/set_info.go:1692` | `if len(buffer) >= 8 {` guards only the alloc read; short/nil buffer falls to `setInfoStatus(types.StatusSuccess)` instead of INFO_LENGTH_MISMATCH |
| Truncate-and-reclaim seq dup EOF/Alloc | WAVE5 | `set_info.go:1533` vs `:1721` | Both branches still repeat SetFileAttributes→Reclaim→restore→notify tail (structure dup) |
| FileFullEaInformation never checks FILE_WRITE_EA | LIVE | `set_info.go:226-227, 1776` | FileFullEaInformation still in the Step-1b exemption list; grep for `FileWriteEa`/`WriteEa` in handlers = 0 hits |
| openFile.mu held across metadata round trips (BasicInfo) | WAVE5 | `set_info.go:406-653` | Lock at 406 still held through metaSvc.GetFile (~508) + both SetFileAttributes calls (structure/latency) |
| RequestedAllocSize / CreateOptions written with no lock | LIVE | `set_info.go:1695, 1766` | Both writes unlocked (no mu in either branch); `handler.go:430-433` still documents CreateOptions as immutable-safe-without-mutex — invariant broken |
| setFileInfoFromStore ~1,540-line god function | WAVE5 | `set_info.go:286` | Still one switch spanning ~286-1832; move-only refactor |
| SET_INFO FileEndOfFileInformation no FILE_WRITE_DATA gate | LIVE | `set_info.go:1545` | EOF sets `authCtx.WriteAuthorizedByHandle = hasWriteAccess(...)` only; `hasAccessRight(..., FileWriteData)` exists solely in the Allocation case (1666) |
| SET_INFO Rename non-zero RootDirectory silently renames | LIVE | `set_info.go:868-873` (link: `2564-2569`) | Same-dir fallback with "we don't resolve FileId handles" comment, returns Success — not STATUS_INVALID_PARAMETER |
| FileRenameInfo/FileLinkInfo decoder dup | WAVE5 | `set_info.go:76/148, 2472/2488` | Both type+decoder pairs still present (bloat) |
| Dest-dir resolution dup rename/hardlink | WAVE5 | `set_info.go:861-901, 2557-2594` | Both copies verbatim-modulo-identifiers |
| Truncate+reclaim+restore+notify tail dup | WAVE5 | `set_info.go:1595 / 1721` | Same 8-line tail twice (near-duplicate of row 2) |
| DecodeFileRenameInfo = DecodeFileLinkInfo | WAVE5 | `set_info.go:148, 2488` | Identical 20-byte decoders remain |
| ResolveForWrite clone of ResolveForRead | WAVE5 | `internal/adapter/common/resolve.go:35` | Verbatim twin of ResolveForRead:24, comment admits speculative divergence |
| WRITE never advances OpenFile.PositionInfo | LIVE | `handlers/write.go` (Write success path) | Zero `PositionInfo`/`CurrentByteOffset` hits in write.go; only read.go:141 `recordReadProgress` updates it |
| DecodeWriteRequest speculative offset fallback | LIVE | `handlers/write.go:128-131` | `} else if len(body) > 48 && int(req.Length) <= len(body)-48 {` substitutes guessed offset-48 data instead of failing per MS-SMB2 3.3.5.13 |
| RDMA Channel / WriteChannelInfo never validated | LIVE | `handlers/write.go:104-106`, `read.go:104` | `req.Channel` decoded then ignored; grep `Channel !=` = 0 hits in handlers |
| Naked fire-and-forget pipe-read goroutine | WAVE5 | `handlers/write.go:618` | `go func() { pending.Callback(...) }()` — established in-package idiom (pipe_read_registry.go same shape) |
| Duplicated tree/session/handle-ownership gate Read/Write | WAVE5 | `read.go:252-269` / `write.go:275-293` | Near-verbatim dup remains (drift risk only) |
| Write() 417-line mixed-concern handler | WAVE5 | `handlers/write.go:169-585` | Still one method; structure-only |
| PositionInfo mutated without openFile.mu (race) | LIVE | `handlers/read.go:145` | `open.PositionInfo = offset + bytesReturned` unlocked; also written unlocked at `set_info.go:1654`, read unlocked at `query_info.go:755, 945` — plain uint64, no atomic |
| Boolean-trap positional bools (CheckLockForIO etc.) | WAVE5 | `write.go:362, 396` / `read.go:344` | Bare bools + trailing comments remain (style) |
| Magic byte-offsets in WRITE decode | WAVE5 | `write.go:120-132` | Bare 64/48 literals remain |
| atime SetFileAttributes errors dropped unlogged | WAVE5 | `read.go:448`, `write.go:529/532` | `_, _ = metaSvc.SetFileAttributes(...)` still silent |
| Repeated error-response struct-literal boilerplate | WAVE5 | `read.go`/`write.go` (~40 sites) | Still present; optional cleanup |
| selectCipher / selectSigningAlgorithm dup | WAVE5 | `handlers/negotiate.go:482, 516` | Two copies of same intersection walk remain (now via slices.Contains) |
| Anonymous NTLM re-auth bypasses guest policy / mislabeled Guest | LIVE | `handlers/session_setup.go:1115-1119` | `tryReauthUpdate(pending, "anonymous", "", nil, true)` — isGuest hardcoded true, `checkGuestPolicy()` not called on this branch (only at 1700/1733/1757) |
| session_setup.go 2,133-LOC multi-concern file | WAVE5 | `handlers/session_setup.go` | File still larger than 2,133 LOC; split is Wave 5 |
| 8 issue-number refs in comments | WAVE5 | `session_setup.go:991,1309,1316,1318,1369,1371,1466,1562` | All 8 `(#NNNN)` refs confirmed verbatim |
| Kerberos bind checks identity by username only | LIVE | `handlers/kerberos_auth.go:396` | `if sess.User == nil | | user.Username != sess.User.Username {` — inline compare; `bindIdentityMatchesSession` used only by NTLM completeSessionBind |
| completeNTLMAuth 307-line god function | WAVE5 | `session_setup.go:1059-1388` | Still one function; SRP refactor |
| tryNetlogonFallback / tryNetlogonBind dup | WAVE5 | `session_setup.go:1388, 1490` | ~35 lines of DC-exchange/resolve/derive still duplicated |
| Fresh SESSION_SETUP keeps unclaimed nonzero SessionId | LIVE | `session_setup.go:976-984` | `sessionID := ctx.SessionID; if sessionID == 0 {...} else if GetSession ok { isReauth = true }` — nonzero miss falls through keeping client ID (orchestrator-flagged HIGH) |
| tryReauthUpdate unlocked identity writes | LIVE | `session_setup.go:2083-2087` | `existingSess.Username = ...` etc. direct writes, no session lock; session.go:60-62 still documents fields read-only after creation |
| configureSessionSigningWithKey 220-line fn | WAVE5 | `session_setup.go:1795` | Still one deeply-nested function |
| SPNEGO signature-byte sniff 3x | WAVE5 | `session_setup.go:317, 496, 679` | Identical 3-byte condition at all three sites |
| SMB2_SESSION_FLAG_BINDING naming | WAVE5 | `session_setup.go:53` | `const SMB2_SESSION_FLAG_BINDING = 0x01` unchanged |
| Durable family 2,334 LOC not split | WAVE5 | `handlers/durable_context.go` | Package-layout proposal only |
| ProcessAppInstanceId takes whole *Handler | WAVE5 | `durable_context.go:907-912` | Still `handler *Handler` param |
| 6-field reconnect-identity tuple loose params | WAVE5 | `durable_context.go:349, 464, 555` | All three 11-param lists remain |
| AppInstanceId force-close: no share/path scoping, no access gate | LIVE | `durable_context.go:970, 997` | Live filter is `f.AppInstanceId == appId` only; `GetDurableHandlesByAppInstanceId` unscoped; call site `create.go:1449` passes no share/path/access |
| sessionKeyHash threaded through 4 sigs, never read | WAVE5 | `durable_context.go:356, 472, 563, 708` | validateAndRestore still "intentionally NOT compare" — dead plumbing |
| Scavenger goroutine no join | WAVE5 | `pkg/adapter/smb/adapter.go:548` | `go scavenger.Run(ctx)`, zero WaitGroup in adapter.go |
| AppInstanceId failover force-close (gaps framing) | LIVE | `durable_context.go:970` | Same root cause re-verified at callee; §3.3.5.9.13 4-condition match + maximal-access gate all absent |
| Share-access bits dup second name | WAVE5 | `disconnected_state_machine.go:162` / `handler.go:2410` | smbShareRead/fileShareRead both still present |
| LockReplayCache.ForgetFile full scan + wrong bound comment | WAVE5 | `handlers/replay_cache.go:352-358` | Full-map scan per CLOSE; comment at 290-291 still says "16-slot cap" vs `LockSequenceIndexMax = 64` (272) |
| Scavenger timeoutMs never read | WAVE5 | `durable_scavenger.go:25, 43` | Only reads are `h.TimeoutMs` (89, 124); zero `s.timeoutMs` reads |
| purgeOneDisconnectedHandle re-derives LockOpenID | WAVE5 | `disconnected_state_machine.go:443-448` | Manual fallback+Sprintf instead of `d.LockOpenID()` (exists at `pkg/metadata/lock/durable_store.go:200`) |
| processV1Reconnect conflicting-V2 check unreachable | WAVE5 | `durable_context.go:503-507` | `ValidateDurableContexts` gate at `create.go:529` rejects dhnc+dh2c/dh2q first; DH2C also branches first at 363 |
| Sign() body triplicated across Signers | WAVE5 | `signing/hmac_signer.go:31`, `cmac_signer.go:139`, `gmac_signer.go:54` | Byte-identical 8-line bodies remain; verifySig precedent at signing.go:81 |
| AEAD nonce fully random (structure framing) | LIVE | `encryption/middleware.go:189-190` | `rand.Read(nonce)` per message, no per-session counter — violates MS-SMB2 3.1.4.3 MUST-NOT-repeat guarantee |
| AES-CCM/GCM nonce pure crypto/rand (gaps framing) | LIVE | `encryption/middleware.go:190` | Same root cause as prior row, duplicate finding — still no counter anywhere in encryption/ or crypto_state |
| asyncCallback closure rebuilt per request | WAVE5 | `pkg/adapter/smb/connection.go:436, 448` | Both goroutine branches still call `makeAsyncNotifyCallback(ci)` per request |

LANE=lane-T1 TOTAL=52 FIXED=0 LIVE=17 DRIFTED=0 INVALID=0 WAVE5=35

## Lane T2 — full re-derivation table

## Review — lane-T2 re-derivation (Wave 4 SMB triage)

Classification rule applied: `bugs`/`security`/`gaps` defects verified against current code → FIXED/LIVE/DRIFTED/INVALID; pure structure/bloat (god objects, move-only refactors, exported-but-unused, dead DRY flexibility) → WAVE5.

| Finding | Verdict | Current file:line | Evidence |
| --- | --- | --- | --- |
| ExtractFileID/VerifyCompoundCommandSignature exported, zero external callers | WAVE5 | compound.go:592,654,732,768 | Still exported; only outside caller is pkg/adapter/smb/connection_test.go — export-surface cleanup |
| Compound-header-walk loop duplicated 3x (MED bloat :69) | WAVE5 | compound.go:72-82,130-152,482-489 | Three hand-copied `rem := compoundData; for len(rem) >= header.HeaderSize` loops still present |
| VerifyCompoundCommandSignature collapses 3 failure classes | LIVE | compound.go:654-709 | Still returns plain `fmt.Errorf("SMB 3.1.1: unsigned unencrypted compound request requires disconnect")` at :695; processRemaining maps all to ACCESS_DENIED+break, no teardown for sub-commands 2..N |
| ReplaceCallback failure return ignored | LIVE | compound.go:196 | `connInfo.Handler.PendingCreateRegistry.ReplaceCallback(result.AsyncId, func(...))` — bool discarded; interim still sent + `MarkStarted` unchecked at :228 when entry already gone |
| ProcessCompoundRequest god function | WAVE5 | compound.go:49-311 | Still ~280-line body mixing validation/credit/dispatch/tracking/send — move-only split |
| Walk-compound loop 3x, double-parse hot path | WAVE5 | compound.go:130-152 | Trailing loop re-parses every subcommand, then processRemaining re-parses same bytes at :857 |
| compoundLoopState.processRemaining god function | WAVE5 | compound.go:845-1082 | Still ~240-line single loop with 9 concerns |
| Compound sub-commands 2..N skip CreditCharge validation | LIVE | compound.go:98,130-152 | `ValidateCreditCharge` called only for `firstBody` at :98; trailing loop does `SequenceWindow.Consume` only, no charge validation |
| CHANGE_NOTIFY as first compound command not gated | LIVE | compound.go:167,245 vs :953 | First command dispatched via `ProcessRequestWithFileIDAndCallback` unguarded; `hdr.Command == types.SMB2ChangeNotify && !isLastCommand` gate only in processRemaining |
| fileIDOffset treats lease-break-ack (SMB2OplockBreak) as carrying FileId | LIVE | compound.go:714-722 | `types.SMB2Flush, types.SMB2Lock, types.SMB2OplockBreak,` → return 8, no StructureSize (24 vs 36) check |
| tree_connect/tree_disconnect split target | WAVE5 | handlers/tree_connect.go:1 | Still 556-LOC self-contained family in 34k-LOC package — move-only |
| calculateMaximalAccess re-declares AccessMask constants (LOW) | WAVE5 | handlers/tree_connect.go:222-246 | Local `fileReadData = 0x00000001 …` const block still present |
| calculateMaximalAccess reinvents types.AccessMask (MED bloat) | WAVE5 | handlers/tree_connect.go:222 | Same const block; drift-risk-only, values currently agree |
| TreeConnect never checks dialect before honoring EncryptData | LIVE | handlers/tree_connect.go:316-321 | `if encryptionMode != "required" | | share == nil | | !share.EncryptData` — no `sess.Dialect < Dialect0300` check; preferred-mode 2.x client still gets dead tree |
| TREE_DISCONNECT no tree-ownership check (MED security) | FIXED | handlers/tree_disconnect.go:39 + response.go:525-535 | Shared prepareDispatch gate now does `tree.SessionID != reqHeader.SessionID → StatusNetworkNameDeleted` (#2424/#2413); handler still existence-only but unreachable cross-session |
| Share-permission resolution inlined in protocol handler | WAVE5 | handlers/tree_connect.go:362 | resolveSharePermission/calculateMaximalAccess/isRootUser still inlined — move-only refactor |
| TREE_DISCONNECT TreeID ownership (HIGH bugs) | FIXED | handlers/tree_disconnect.go:39 | Same root cause closed by prepareDispatch ownership gate (#2424/#2413) |
| TREE_DISCONNECT + every NeedsTree op (HIGH gaps) | FIXED | response.go:525-535 | `if !ok \|\| tree.SessionID != reqHeader.SessionID` in the one shared NeedsTree gate — the audit's own root-cause fix, landed |
| TREE_CONNECT response encoding duplicated | WAVE5 | handlers/tree_connect.go:208-217 vs :301-309 | Two verbatim smbenc.Writer 16-byte response builders — DRY only |
| Unowned goroutine per parked LOCK on TREE_DISCONNECT | LIVE | handlers/tree_disconnect.go:73 | `go func(p *PendingLock) { p.Callback(...) }(parked)` — fire-and-forget, no join; siblings (close.go/logoff.go) call synchronously |
| Duplicated unprivileged-AuthContext block | WAVE5 | handlers/auth_helper.go:73-94 vs :442-460 | Two near-identical 5-field AuthContext builds still present — DRY |
| resolveIdentityMapping hardcodes provider="kerberos" | WAVE5 | handlers/identity_resolver.go:33 | `ims.GetIdentityMapping(ctx, "kerberos", principal)` literal unchanged — structure/dead-flexibility |
| handleKerberosAuth/completeKerberosBind preamble dup (MED) | WAVE5 | handlers/kerberos_auth.go:26-105 vs :340-391 | Same 8-step AP-REQ authenticate+resolve+lookup preamble duplicated — DRY |
| completeKerberosBind re-implements AP-REP+MIC+SPNEGO construction (MED) | WAVE5 | handlers/kerberos_auth.go:248-328 vs :461-490 | Bind still re-inlines; drift persists (primary drops apRepToken :320, bind keeps :483) — DRY/observability |
| Kerberos session created before mechListMIC check, no rollback (security) | LIVE | handlers/kerberos_auth.go:144,189,290-296 | CreateSessionWithUserAndExpiry :144 and configureSessionSigningWithKey run before `auth.VerifyMechListMIC` :290; MIC failure → `StatusLogonFailure` with no DeleteSession |
| AP-REP building duplicated and drifted | WAVE5 | handlers/kerberos_auth.go:320 vs :483 | Same finding as above row — drift verbatim, structure-only |
| SPNEGO mechListMIC check runs after session state committed (bugs) | LIVE | handlers/kerberos_auth.go:290 | Same defect: MIC verify inside buildKerberosAcceptResponse, after StoreSession + key derivation; reauth path :237-243 also mutates first |
| AP-REQ auth + user resolution duplicated (LOW) | WAVE5 | handlers/kerberos_auth.go:47-105 vs :351-391 | Byte-identical modulo log strings — DRY |
| extractAPReqFromGSSToken hand-rolls GSS-unwrap | WAVE5 | handlers/kerberos_auth.go:598-634 | SMB-local copy incl. long-form rejection :621-623; move-only refactor to shared kerberos pkg |
| 45-line async-callback wiring block duplicated (MED) | WAVE5 | response.go:614-660 vs :276-320 | Five identical per-command async-callback setups in both entrypoints — DRY, copies in sync |
| checkEncryptionRequired TreeID=0 bypass | LIVE | response.go:514,728 | Both guards intact: `cmd.NeedsTree && reqHeader.TreeID != 0` and `if reqHeader.TreeID != 0` — per-share EncryptData never consulted at TreeID=0 |
| ProcessSingleRequest god function | WAVE5 | response.go:79-395 | Still ~300 lines mixing metrics/credit/gates/wiring/dispatch — move-only split |
| Sign-then-splice reimplemented 3x | WAVE5 | response.go:1009,1042,1168 | `copy(hdr.Signature[:], smbPayload[48:64])` at all three sites — DRY |
| HandleSMB1Negotiate hand-encodes SMB2 wire bytes | WAVE5 | response.go:1429-1457 | binary.LittleEndian.PutUint* at literal offsets unchanged — wrong-layer DRY |
| Anonymous/guest bypass in checkEncryptionRequired (HIGH) | LIVE | response.go:711 | `if sess.IsNull \|\| sess.IsGuest { return 0 }` still precedes per-share `tree.EncryptData` check :728 |
| Async-callback wiring duplicated (2nd listing, dup of L313) | WAVE5 | response.go:614-660 | Same finding as row above — overlap already accounted in lane count |
| ConnectionCryptoState exports every field | WAVE5 | crypto_state.go:30-69 | All 11 negotiation fields exported, accessor-only-by-convention — structure |
| Manager.RangeSessions leaks `any` | WAVE5 | session/manager.go:326 | `fn func(sessionID uint64, value any) bool` unchanged — structure |
| SessionCryptoState.Destroy dead code | LIVE | session/manager.go:88-94 + crypto_state.go:233 | DeleteSession body still only `m.sessions.Delete(sessionID)`; Destroy has zero callers in internal/adapter/smb |
| Session re-auth mutates identity fields unsynchronized | LIVE | handlers/kerberos_auth.go:237-243 + session_setup.go:2085-2089 | `sess.User = user; sess.Username = …; sess.IsGuest = …` unlocked; session.go:61 still documents "read-only after creation" |
| Session.Credits vs CommandSequenceWindow dual ledgers | FIXED | response.go:1237,1317 | Both async paths now call `connInfo.SessionManager.GrantCredits(sessionID, 1, 0)` before `SequenceWindow.Grant` — the exact fix requested (#2503 async credit grants) |
| IOCTL CreditCharge validation reads wrong field | LIVE | session/credit_validation.go:98-104 | Reads `body[28:32]` (InputCount); repo's own parseIoctlMaxOutputSize reads body[44:] (ioctl_fsctl.go:285) — decoders disagree |
| QUERY_DIRECTORY CreditCharge reads wrong field | LIVE | session/credit_validation.go:106-112 | Reads `body[4:8]` (FileIndex); query_directory.go:135-137 confirms FileIndex@4, OutputBufferLength@28 — inert check |
| LeaseManager god object | WAVE5 | lease/manager.go:194 | ~26 resolver-only methods still bundled — move-only split |
| Pre-registration write races same-key requests | LIVE | lease/manager.go:329-370,401,416 | Pre-register under mu, release, grant; success path still never re-asserts `bindings[ck]` — restorePreRegistration only on error/None |
| Lost-update race on lease bindings map | LIVE | lease/manager.go:362-370 | `restorePreRegistration` still unconditionally writes stale `prev` / deletes — no compare-and-swap guard |
| Missing ownership check on oplock break-ack | LIVE | handlers/stub_handlers.go:959-999 | handleOplockBreakAck has no `VerifyLeaseAckOwnership` (sibling :926 has it); `openFile.OplockLevel = ack.OplockLevel` unconditional at :999 |
| AllSharesResolver speculative generality | WAVE5 | lease/notifier.go:191-203 | Dead nil-fallback branch + assertion-that-can't-fail unchanged — dead flexibility |

**Notes for the orchestrator (T6):**
+ L313 ≡ L803 are the same async-callback-wiring finding listed twice (the lane's "minus 1 overlap"); counted once below.
+ The three TREE_DISCONNECT rows (L523/L698/L733) share one root cause, closed in one place: the prepareDispatch NeedsTree ownership gate (#2424/#2413). The per-handler existence-only lookups in tree_disconnect.go:39 remain but are unreachable cross-session via every current dispatch path.
+ L684 was closed by #2503 exactly as the audit prescribed (SessionManager.GrantCredits on both async completion paths).
+ Remaining LIVE security cluster worth fix-wave priority: encryption-gate bypasses (L740 anon/guest, L551 TreeID=0), Kerberos MIC-after-commit (L516/L761), oplock break-ack ownership (L558), and the two wrong-field credit decoders (L880/L887 — self-certified by their own test fixtures).

LANE=lane-T2 TOTAL=47 FIXED=4 LIVE=18 DRIFTED=0 INVALID=0 WAVE5=25

## Lane T3 — full re-derivation table

## Review — Wave 4 triage, lane T3 (re-derivation against current develop)

Classification scheme applied: concrete defects (bugs/gaps/security) + convention violations still present → LIVE; code-shape refactors (god objects, move-only, exported-unused, dedup/bloat) → WAVE5; verified-closed → FIXED. Every finding re-derived from source at the cited symbols.

| Finding (abbrev) | Verdict | Current location | Evidence |
| --- | --- | --- | --- |
| doc.go stale: dialect 0x0202-only, v2/handlers path (structure) | LIVE | internal/adapter/smb/doc.go:16 | Still "DittoFS implements SMB2 dialect 0x0202 (SMB 2.0.2)"; v2/handlers/ ref at doc.go:10 |
| checkShareModeConflict reimplements has*Access predicates | WAVE5 | handlers/handler.go:2409 | Local hex const block + hasRead/hasWrite closures still parallel hasReadAccess (handler.go:2772) |
| selectDialectFromList duplicates negotiate's selectDialect | WAVE5 | handlers/ioctl_validate_negotiate.go:222 | `func (h *Handler) selectDialectFromList` still its own priority loop, wildcard bolted on at :281 |
| closeFilesWithFilter/DeleteOpenFile never drain handleOps | LIVE | handlers/handler.go:1236 | DeleteOpenFile = forgetReplayState+Delete+handleOps.Delete, no DrainHandleOps; pipe CLOSE path close.go:174 |
| OpenFile second god object (~60 fields) | WAVE5 | handlers/handler.go:439 | Struct unchanged, multi-concern, per-handle mu |
| MS-FSA share-mode algorithm inlined in *Handler | WAVE5 | handlers/handler.go:2403 | checkShareModeConflict/checkShareDeleteConflict still *Handler methods reaching h.files |
| Share-access bits declared 3x under 3 names | WAVE5 | handlers/handler.go:2410 | fileShareRead :2410, fileShareDelete :2581, smbShareRead disconnected_state_machine.go:162 |
| Break-wait dispatch duplicated sync/async | WAVE5 | handlers/create_post_break.go:1517 | breakAndMaybeParkCreate (:223) and parkCreateOnLeaseBreak (:1517) both carry the shareConflictWait/other-key wait branch |
| Malformed ACE Size → slice-bounds panic (security) | LIVE | handlers/security.go:929 | Only `offset+int(aceSize) > len(data)` checked (:924); `data[offset+aceHeaderSize:offset+int(aceSize)]` low>high on aceSize<8 |
| pendingRegistry[V] leaks locking discipline | WAVE5 | handlers/pending_create_registry.go:255 | ReplaceCallback/MarkStarted still lock r.reg.mu and read byAsyncID raw |
| VALIDATE_NEGOTIATE hand-rolls IOCTL input parse | WAVE5 | handlers/ioctl_validate_negotiate.go:58 | Inline body[28:32] + hardcoded bufferStart=56 still present, parseIoctlInputData unused |
| ProcessLeaseCreateContext 11 positional params | WAVE5 | handlers/lease_context.go:353 | Signature verbatim: 3 adjacent bools + 2 adjacent strings |
| Exported lease/oplock API zero external consumers | WAVE5 | handlers/lease_context.go:353 | ProcessLeaseCreateContext/DecodeLeaseCreateContext/etc. still exported, same-package-only callers |
| ProcessLeaseCreateContext swallows non-sentinel errors at Debug | LIVE | handlers/lease_context.go:454 | `else if err != nil { logger.Debug(...); grantedState = lock.LeaseStateNone }`; returns nil error at :558 — config-class errors never Error-logged |
| Pervasive issue-number citations in security.go | LIVE | handlers/security.go:140 | "Directory SID Bridge (#1617)" + 15+ more (#1528/#1608/#1228) at :144,:172,:255,:298,:485,:723… |
| 19 one-line dispatch wrappers pass-through | WAVE5 | internal/adapter/smb/dispatch.go:150 | 9 pure `return h.X(ctx, body)` wrappers (:150-294) unchanged |
| Command.Name duplicates types.Command.String() | WAVE5 | internal/adapter/smb/dispatch.go:33 | 19 `Name: "X"` literals still hand-synced |
| FileID wire-offset table duplicated | WAVE5 | internal/adapter/smb/channel_sequence.go:32 | channelSeqFileID switch (off 16/8) still transcribes requestFileIDOffset's subset |
| completeCreateAfterBreak god function | WAVE5 | handlers/create_post_break.go:643 | Still ~870 lines, SD_BUFFER/EA/oplock-grant inline |
| CANCEL cancels another session's parked CREATE (no AsyncId ownership) | FIXED | handlers/pending_create_registry.go:223 | `UnregisterByAsyncId(connID, asyncId)` → `unregisterByAsyncIDOn(asyncID, connID)` (#2426); cancel_asyncid_scope_test.go covers all 4 registries |
| Handler is THE god object | WAVE5 | handlers/handler.go:36 | Struct unchanged, ~2900-LOC file, every dispatch entry's receiver |
| TOCTOU create-race recovery truncates winner without breaking lease/oplock | LIVE | handlers/create_post_break.go:797 | Race branch replays recheckExistingFileGates (share-mode+DACL only) then `h.overwriteFile(authCtx, winner, req)` — no breakAndMaybeParkCreate on winner |
| Unsynchronized OpenFile.MetadataHandle read in delete-on-close election | LIVE | handlers/doc_election.go:142 | `bytes.Equal(other.MetadataHandle, openFile.MetadataHandle)` unlocked vs mu-guarded write set_reparse_point.go:263 |
| parseACEs panics on undersized AceSize (bugs) | LIVE | handlers/security.go:929 | Same missing `aceSize >= aceHeaderSize` guard |
| CREATE parked mid-compound-chain unhandled (interior position) | LIVE | handlers/create_post_break.go:1639 | parkCreateOnLeaseBreak never checks ctx.NextCommand; compound.go:1040 defers MarkStarted only for isLastCommand → resume goroutine blocks forever on `<-pending.started` |
| VALIDATE_NEGOTIATE never checks MaxOutputResponse | LIVE | handlers/ioctl_validate_negotiate.go:43 | Handler+callees never read MaxOutputResponse; parseIoctlMaxOutputSize (ioctl_fsctl.go:281) not used here — undersized client still gets 24B STATUS_SUCCESS |
| parseACEs panics on AceSize < 8 (gaps) | LIVE | handlers/security.go:929 | Same defect, third framing |
| Issue-number ref in lease_context.go | LIVE | handlers/lease_context.go:406 | "Samba `is_lease_stat_open`, #751" still in comment |
| buildDACL/buildSACL/buildEmptySACL triplicate ACL wire logic | WAVE5 | handlers/security.go:585 | buildDACL (:585) and buildSACL (:633) byte-identical header + per-ACE loops |
| Package-level SIDMapper/DirectorySIDBridge globals | WAVE5 | handlers/security.go:118 | `_defaultSIDMapper` atomic global, `_directorySIDBridge` :173; SetSIDMapper still package-level |
| Issue numbers in conn-dispatch comments | LIVE | internal/adapter/smb/conn_types.go:43 | "issue #361" :43, "#378" :192, framing.go:122 "#717", hooks.go:123 "#362" |
| buildDACL and buildSACL duplicate (bloat) | WAVE5 | handlers/security.go:585 | Same duplication as triplicate finding, second framing |
| doc.go stale (bloat framing) | LIVE | internal/adapter/smb/doc.go:16 | Same stale text; duplicate finding |
| channelSeqFileID duplicates requestFileIDOffset (bloat) | WAVE5 | internal/adapter/smb/channel_sequence.go:29 | Same duplication, second framing |
| Ioctl() never validates request Flags (non-FSCTL dispatched) | LIVE | handlers/ioctl_dispatch.go:64 | Only CtlCode read (:65-67); no Flags/IS_FSCTL parse anywhere in file — MS-SMB2 3.3.5.15 MUST violated |
| getCachedShares/shareSecurityDescriptor fabricate context.Background() | WAVE5 | handlers/handler.go:2886 | `h.Registry.ShareRootGrantACL(context.Background(), shareName)` — ctx-threading refactor |
| fileID/openFile resolve-reject boilerplate 4x in ioctl_fsctl.go | WAVE5 | handlers/ioctl_fsctl.go:17 | Identical pair at :17, :52, :197, :221 |
| Fixed IOCTL header decoded ad hoc in 4+ places | WAVE5 | handlers/ioctl_dispatch.go:224 | parseIoctlFileID, parseIoctlMaxOutputSize/InputData (ioctl_fsctl.go:281/293), inline copy (ioctl_validate_negotiate.go:58) |
| Issue number leaked into ioctl comments (2x) | LIVE | handlers/ioctl_dispatch.go:52 | "See issue #436…" :52 and :122, no ponytail: prefix |
| EncodeCreateContexts hardcodes 64+88 inline | WAVE5 | handlers/lease_context.go:603 | `offset := uint32(64 + 88)` — smb2HeaderSize unused |

LANE=lane-T3 TOTAL=40 FIXED=1 LIVE=16 DRIFTED=0 INVALID=0 WAVE5=23

## Lane T4 — full re-derivation table

## Review — lane-T4, tranche `handlers-create` (8 findings re-derived against current develop worktree)

All 8 findings located via `area: handlers-create` grep; each diagnosis re-checked at current symbol locations (audit line numbers stale, code re-found by name).

| Finding (abbrev) | Verdict | Current file:line | Evidence |
| --- | --- | --- | --- |
| Dead 'no metadata service' fallback in handleOpenRootCreate | WAVE5 | internal/adapter/smb/handlers/create.go:1879 | Still present verbatim: `if metaSvc := h.Registry.GetMetadataService(); metaSvc != nil {` shadow re-check after 1861–1863 deref; unreachable else `grantedAccess = resolveAccessFlags(...)` at :1891 — dead-flexibility cleanup, no behavioral defect (cross-ref Wave 5 create.go decomposition) |
| QFid create-context block duplicated root-open vs normal-open | WAVE5 | create.go:1989 + create_post_break.go:1471 | Both sites still build `qfidResp := make([]byte, 32)` / `copy(qfidResp[0:16], qfidFileID[:16])` / `copy(qfidResp[16:32], h.ServerGUID[:])` verbatim; no `buildQFidContext` helper exists — dedup refactor only |
| Reconnect-context detection computed independently 3× | WAVE5 | create.go:575, 792, 811 | All three `FindCreateContext(...DurableHandleV1/V2ReconnectTag)` derivations still present (`isDurableReconnect` :575–578, `hasReconnectCtx` :792–794, `hasDHnC/hasDH2C` :811–812); IPC$ return at :651 still moots the guard — pure duplication, no bug |
| ADS base-file existence check swallows lookup error | LIVE | create.go:1290 | `if f, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, adsBaseFileName); f == nil {` still discards the error return while sibling :1230 checks `lookupErr` — defect verbatim (audit itself downgraded consequence to mis-mapped NTSTATUS) |
| ADS/stream-name parsing inline unnamed block | WAVE5 | create.go:689–724 | The colon-suffix switch mutating `filename`/`streamSuffix`/`explicitDataStream` is still an unnamed inline block in `Create()`; no `parseStreamSuffix` helper exists — extract-helper refactor, audit says "opportunity not defect" |
| AppInstanceId failover force-closes any user's handle, any share | LIVE | durable_context.go:970 | Filter still `func(f *OpenFile) bool { return f.AppInstanceId == appId }` with `0, // no specific sessionID — match across sessions` (:969); persisted lookup :997 `GetDurableHandlesByAppInstanceId(ctx, appId)` unscoped; call site create.go:1449–1452 still passes no share/path and no maximal-access gate — MS-SMB2 3.3.5.9.13 match conditions still absent (cross-ref: duplicate finding in area handlers-durable → lane T1; T5 holds the AppInstanceId cross-check. Surrounding displaced-lease release code was added, but the security defect itself is verbatim) |
| ~300-line durable-reconnect flow inlined in Create() | WAVE5 | create.go:801–1113 | Step 4b reconnect path (session-key hash, `ProcessDurableReconnectContext`, lease-regrant :875–1024, oplock-regrant :1026–1067, response build) still fully inline; no `resolveDurableReconnect` symbol exists in the package — move-only refactor, self-described subset of the Create() god-function finding |
| Create() ~1015-line god function | WAVE5 | create.go:498–1526 | `func (h *Handler) Create(` at :498, next func at :1528 — ~1030 lines, unchanged in shape; god-function decomposition belongs to Wave 5 |

Notes:
+ None of the 8 known already-fixed items (#2424/#2413, #2426/#2414, #2461, #2387, #2343, #2501, #2502, #2503) fall in this tranche.
+ The two LIVE rows are the only behavioral defects; both survive the ~12 SMB fixes + #2505 error consolidation untouched (`types.StatusForErr` appears at :1283 but not in either defect path).
+ Read-only respected: nothing written, no git commands run.

LANE=lane-T4 TOTAL=8 FIXED=0 LIVE=2 DRIFTED=0 INVALID=0 WAVE5=6

## Lane T5 — full re-derivation table

## Review

| Finding | VERDICT | Current file:line | Evidence |
| --- | --- | --- | --- |
| Scavenger goroutine: stop signal but no join in Stop() | LIVE | pkg/adapter/smb/adapter.go:548 | `go scavenger.Run(ctx)` fire-and-forget; no `wg.` anywhere in adapter.go |
| Machine SID mapper wired into package-level global vs per-instance | WAVE5 | pkg/adapter/smb/adapter.go:313 + internal/adapter/smb/handlers/security.go:118,173 | Verbatim present (`handlers.SetSIDMapper` → atomic globals `_defaultSIDMapper`/`_directorySIDBridge`), but audit itself grades it race-safe/move-only refactor — Wave 5 structure lane |
| Stop() races with wire*Resolver on identityUnsub/identityProviderUnsub/foreignSIDProviderUnsub | LIVE | pkg/adapter/smb/adapter.go:912-923 | Stop reads/nils the 3 fields with no `resolverMu`; wireIdentityResolver (adapter.go:767) writes them under `resolverMu.Lock()` — only netlogon fields (adapter.go:928) got the fix |
| Serve() unbounded pre-bind work → Stop-before-bind leak (structure) | FIXED | pkg/adapter/base.go:262-275 | ServeWithFactory now re-checks `b.Shutdown` after storing listener and "close[s] the fresh socket and unwind[s] rather than serving after Stop" — exactly the base.go fix the audit demanded |
| Serve() pre-bind DB/discovery work widens socket leak (bugs) | FIXED | pkg/adapter/base.go:262-275 | Same root cause, same landed fix; post-bind shutdown re-check closes the leaked-listener window |
| Package doc claims NTLMv2 validation unimplemented (false) | LIVE | internal/adapter/smb/auth/ntlm.go:10 | Stale comment still present: "of NTLMv2 response verification is required." while `ValidateNTLMv2Response` (ntlm.go:820) is called from prod |
| Test-only debug struct in exported production signatures | WAVE5 | internal/adapter/smb/auth/ntlm.go:977,1013 | `NTLMSSPMechListMICDebug` still exported, `dbg` param still threaded through both compute fns (prod passes nil) — exported-but-unused surface, Wave 5 bloat lane |
| NTLMv2 client-challenge AV_PAIRs structural-only; Type-3 MIC never verified | LIVE | internal/adapter/smb/auth/ntlm.go:872-892 | `validateNTLMv2ClientChallenge` doc lists only RespType/HiRespType/length/EOL checks; no MsvAvFlags/ChannelBindings parsing and no Type-3 MIC verification anywhere (audit: ChannelBindings/TargetName halves refuted, MIC half real and still open) |
| NTLM compute-only mechListMIC — no verify counterpart | LIVE | internal/adapter/smb/auth/ntlm.go:1013 + spnego.go:287 | No `VerifyNTLMSSPMechListMIC` exists (repo-wide grep: zero hits); `AuthenticateMessage` (ntlm.go:594-628) has no MIC field; spnego.VerifyMechListMIC wraps Kerberos gssapi.MICToken only, used solely from kerberos path |
| X-CHECK: fresh SESSION_SETUP keeps unclaimed nonzero client SessionId | LIVE | internal/adapter/smb/handlers/session_setup.go:976-984 | `sessionID := ctx.SessionID; if sessionID == 0 {GenerateSessionID()} else if GetSession ok {isReauth=true}` — a miss falls through keeping the client-supplied ID |
| X-CHECK: anonymous/guest bypass defeats per-share SMB2_SHAREFLAG_ENCRYPT_DATA | LIVE | internal/adapter/smb/response.go:709-713 | `if sess.IsNull | | sess.IsGuest { return 0 }` runs before the per-share `tree.EncryptData` check at response.go:731-738 — verbatim |
| X-CHECK: AppInstanceId failover force-close lacks share/path/access scoping | LIVE | internal/adapter/smb/handlers/durable_context.go:907,970 + create.go:1449-1451 | Filter is `f.AppInstanceId == appId` only across `handler.files.Range`; signature takes no shareName/path; no maximal-access check — MS-SMB2 §3.3.5.9.13 match+gate still absent |

**Notes on verdicts**
+ The two Serve() findings are one defect filed twice (structure + bugs lens); the base.go post-bind shutdown re-check closes both. Commit ID not verifiable without `git log` (supervisor may run `git log --oneline -S "close the fresh socket" -- pkg/adapter/base.go` to attribute).
+ The three HIGH cross-checks are all LIVE verbatim; the SessionId-squat and guest-encryption findings still need the design decisions the orchestrator flagged (MS-SMB2 anonymous-can't-encrypt tension) — not one-liners.
+ No DRIFTED and no INVALID verdicts in this tranche; the audit's SMB-auth/pkg-adapter diagnoses held up at pin time and the cited code is unchanged in the defect paths.

LANE=lane-T5 TOTAL=12 FIXED=2 LIVE=8 DRIFTED=0 INVALID=0 WAVE5=2
