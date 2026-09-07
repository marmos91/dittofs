<!-- SMB adapter audit, scoped to internal/adapter/smb + pkg/adapter/smb, run against origin/develop @ 111192fd8. -->

# Status

The audit ran against `111192fd8`. **No file under `internal/adapter/smb/**` or
`pkg/adapter/smb/**` has changed since that pin** (verified against `deb2d7b96`), so the findings
stand exactly as written. The only adjacent file that moved is
`internal/adapter/common/write_payload.go`; re-check any finding citing it.

No status layer yet. Two HIGH findings have been filed as issues:

- **#2413** — tree-scoped commands never check the TreeID belongs to the requesting session.
  Reported independently by the `bugs` and `gaps` lenses; both entries describe one defect.
- **#2414** — CANCEL resolves a parked request by AsyncId with no ownership check.

Both were re-verified against `deb2d7b96` before filing, and both are the same shape: a missing
ownership check on an id that is guessable because it comes from a server-wide sequential counter.

# Lens coverage

Five passes: `bugs`, `security`, `structure`, `bloat`, `gaps` over 21 areas. **The `perf` lens is
~zero**, as on the NFS and core audits. `structure` dominates the result (83 of 157 findings),
which is the signal for the NFS/SMB convergence work rather than noise: `Handler` is a 36-field,
2925-LOC god object, `Create()` ~1015 lines, `setFileInfoFromStore` ~1540 lines.

---

# Audit Report — audit-smb (111192fd8)
Stack: Go (single module, repo root go.mod; no vendor dir). Userspace SMB2/SMB3 protocol server built on stdlib net/crypto — crypto/aes, crypto/cipher (AES-CCM/GCM), crypto/hmac, crypto/cmac-style CMAC, SP800-108 KDF, encoding/binary for wire codecs, sync.Map + atomics for lock-free registries. In-scope deps: internal/adapter/common (NTSTATUS error mapping), pkg/controlplane/runtime (single entrypoint to metadata + per-share block stores), pkg/identity, pkg/lock, pkg/models. Build: plain `go build`/`go test`, Cobra CLI at cmd/dfs, Nix flake for versioning. Testing: stdlib testing (~53k prod LOC vs ~53k test LOC in scope), plus external smbtorture/Samba conformance and pcap-diff interop harnesses in test/. Specs implemented: MS-SMB2 (dialects 0x0202-0x0311), MS-FSCC, MS-DTYP, MS-NLMP, MS-ERREF, MS-SRVS/MS-LSAT over DCERPC named pipes. · Areas: 21 · Findings: 7 HIGH / 62 MED / 88 LOW

## Summary by dimension

| Dimension | HIGH | MED | LOW |
|---|---|---|---|
| bugs | 1 | 12 | 6 |
| security | 1 | 9 | 2 |
| structure | 3 | 21 | 59 |
| bloat | 0 | 9 | 18 |
| gaps | 2 | 11 | 3 |

## Summary by area

| Area | Findings |
|---|---|
| handlers-auth | 9 |
| handlers-core-handler | 7 |
| handlers-create | 8 |
| handlers-create-post-break | 6 |
| handlers-durable | 12 |
| handlers-ioctl-core | 7 |
| handlers-lease-oplock | 6 |
| handlers-negotiate-session | 12 |
| handlers-read-write | 12 |
| handlers-security-sd | 7 |
| handlers-set-info | 12 |
| handlers-tree-connect | 10 |
| pkg-smb-adapter | 5 |
| pkg-smb-connection | 1 |
| smb-auth-ntlm-spnego | 4 |
| smb-compound | 10 |
| smb-conn-dispatch | 7 |
| smb-crypto | 3 |
| smb-lease-manager | 5 |
| smb-response-pipeline | 8 |
| smb-session | 6 |

## Findings
### [LOW] doc.go stale: claims dialect 0x0202-only, v2/handlers/ path that doesn't exist · `structure` · area: smb-conn-dispatch
- **Where:** `internal/adapter/smb/doc.go:16`
- **What:** doc.go:16 claims SMB2 dialect 0x0202 only; product ships 0x0202-0x0311 (Dialect0311 used throughout, e.g. channel_sequence.go:57). doc.go:11 cites 'v2/handlers/' path — actual is internal/adapter/smb/handlers/, no v2/ dir exists.
- **Why it matters:** package doc is first thing godoc/IDE shows; misleads on dialect support and layout.
- **Fix:** rewrite dialect list to 0x0202-0x0311, fix handler path, verify rest of arch overview (session/, types/, header/) against current names.
- **Verified:** CONFIRMED. Dialect0311 negotiated/branched in negotiate.go:96,104,309, signing/signer.go:67, kdf/kdf.go:120, framing.go:454, compound.go:683, hooks.go:97,149. `find . -type d -name v2` under smb returns nothing.

### [LOW] ExtractFileID/VerifyCompoundCommandSignature exported w/ zero external callers; ParseCompoundCommand/InjectFileID exported only for other package's tests · `structure` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:592`
- **What:** ExtractFileID(732)/VerifyCompoundCommandSignature(654) exported, called only within compound.go (252,1002,895) — zero external hits. ParseCompoundCommand(592)/InjectFileID(768) exported, only outside caller is pkg/adapter/smb/connection_test.go (10+ sites), no prod consumer outside package.
- **Why it matters:** min exported surface rule; export-for-test-in-another-package inflates public API with 4 symbols, 2 with no real caller anywhere.
- **Fix:** lowercase ExtractFileID/VerifyCompoundCommandSignature. Move connection_test.go cases into in-package compound_test.go, then lowercase ParseCompoundCommand/InjectFileID too.
- **Verified:** CONFIRMED by full-worktree grep. `smb.ExtractFileID` hits are a DIFFERENT symbol (nfs/xdr.ExtractFileID). All four run in prod via internal calls — not dead, just over-exported.

### [LOW] Manager.RangeSessions leaks `any` instead of `*Session`; only caller discards value · `structure` · area: smb-session
- **Where:** `internal/adapter/smb/session/manager.go:326`
- **What:** `RangeSessions(fn func(sessionID uint64, value any) bool)` type-erases concrete *Session stored in sync.Map. Sole prod caller handlers/state_debug.go:49 discards value with `_`.
- **Why it matters:** accept-interfaces/return-structs violation; exported iterator hands `any` when only type ever stored is *Session; forces future callers into unchecked type assertion.
- **Fix:** change sig to `fn func(sessionID uint64, s *Session) bool`, do `v.(*Session)` assertion once inside RangeSessions.
- **Verified:** CONFIRMED. manager.go:324 doc comment even says "receives (sessionID, *Session)" while sig says `any`. state_debug.go:49 discards with `_ any`.

### [LOW] LeaseManager god object: ~23 of 39 exported methods never touch bindings/mu · `structure` · area: smb-lease-manager
- **Where:** `internal/adapter/smb/lease/manager.go:150`
- **What:** 23 methods (~650 LOC: GetLeaseState:655 ... BreakReadLeasesOnWrite:1364) touch only `resolver`, none reference mu/bindings/versions/clientPrimarySession. Other ~18 (RequestLease family, AcknowledgeLeaseBreak, etc.) all touch shared state.
- **Why it matters:** split boundary already visible in field-usage data; bundling maximizes surface every caller/test-double must satisfy.
- **Fix:** extract `LeaseBreaker{resolver LockManagerResolver}` into break.go for the 23 resolver-only methods; embed in LeaseManager so call surface unchanged for ~40 callers in handlers/.
- **Verified:** CONFIRMED. 26 of 45 methods touch none of mu/bindings/versions/notifier. Reachable via NewLeaseManager at adapter.go:263. Zero behavioral impact — pure refactor opinion. Downgraded MED->LOW.

### [LOW] Pre-registration write in requestLeaseInternal races across concurrent same-key requests for different files · `structure` · area: smb-lease-manager
- **Where:** `internal/adapter/smb/lease/manager.go:327`
- **What:** writes lm.bindings[ck] under mu (327-355) before blocking grant call, releases mu, restorePreRegistration (362-370) restores `prev` captured at call's own start. Two concurrent calls same (ClientID,Share,Key) diff fileHandle: second overwrites first's binding before either grant completes; winner's success path (415-417) only checks HasLeaseOnHandle, never re-asserts bindings[ck].
- **Why it matters:** lock.Manager is real source of truth so self-heals, but in the window GetSessionForBreak/VerifyLeaseAckOwnership/AcknowledgeLeaseBreak can resolve wrong HandleKey, misrouting break/ack to wrong file's lease state.
- **Fix:** success path re-check identity: re-acquire mu, unconditionally re-set bindings[ck] to this call's binding on success (successful grant is authoritative).
- **Verified:** CONFIRMED in source. Narrow: needs one client racing two CREATEs reusing same lease key. LOW not MED.

### [LOW] Package doc claims NTLMv2 validation unimplemented — false · `structure` · area: smb-auth-ntlm-spnego
- **Where:** `internal/adapter/smb/auth/ntlm.go:9`
- **What:** doc says "additional implementation of NTLMv2 response verification is required." ValidateNTLMv2Response(820)/ComputeNTLMv2Hash(783)/validateNTLMv2ClientChallenge(886)/DeriveSigningKey(940) already implement it, called from session_setup.go:1166.
- **Why it matters:** comments must describe current behaviour; stale doc misleads readers into thinking validation is a TODO.
- **Fix:** delete/rewrite lines 9-10.
- **Verified:** CONFIRMED. ValidateNTLMv2Response called from prod session_setup.go:1166, DeriveSigningKey at 1215/1435/1532/1708. Cosmetic -> LOW.

### [LOW] Test-only debug struct baked into exported production function signatures · `structure` · area: smb-auth-ntlm-spnego
- **Where:** `internal/adapter/smb/auth/ntlm.go:977`
- **What:** NTLMSSPMechListMICDebug(977-985) exported; ComputeNTLMSSPMechListMIC(1013)/computeNTLMSSPMechListMICLegacy(1129) take `dbg *NTLMSSPMechListMICDebug`. Only non-nil caller: ntlm_mechlistmic_test.go:129 (same pkg). Prod callers pass nil (session_setup.go:950,2051).
- **Why it matters:** min-exported-surface violation for a debug hook a same-package test doesn't need exported.
- **Fix:** lowercase to ntlmsspMechListMICDebug, or split private computeWithDebug helper called by both public fn and test.
- **Verified:** CONFIRMED. Param/type itself is test-only surface — bloat/minimal-surface cleanup, not a live bug. LOW.

### [LOW] Durable-handle scavenger goroutine has stop signal but no join in Stop() · `structure` · area: pkg-smb-adapter
- **Where:** `pkg/adapter/smb/adapter.go:548`
- **What:** `go scavenger.Run(ctx)` fire-and-forget. Adapter.Stop(909-967) unsubscribes callbacks/closes sidecars/Kerberos/NETLOGON but never waits for scavenger exit. Only stop signal is ctx, cancelled by lifecycle's entry.cancel() which runs AFTER Stop() returns (adapters/service.go:429/443).
- **Why it matters:** every `go` needs owner+stop signal+join; caller treating Stop() return as "fully quiesced" can race still-running scavenger against store teardown.
- **Fix:** add sync.WaitGroup field, Add(1) before go call, Done() in wrapper, wg.Wait() (bounded) inside Stop before return.
- **Verified:** CONFIRMED. Run (durable_scavenger.go:52-67) exits only on ctx.Done(). No observed crash proven -> LOW.

### [LOW] Machine SID mapper wired into package-level global instead of instance field, inconsistent with PipeManager's own per-instance copy · `structure` · area: pkg-smb-adapter
- **Where:** `pkg/adapter/smb/adapter.go:313`
- **What:** handlers.SetSIDMapper(mapper)(313)/handlers.SetDirectorySIDBridge(...)(840) write process-global atomic.Pointer vars in handlers/security.go (_defaultSIDMapper,_directorySIDBridge). Two lines below, same mapper ALSO set per-instance via s.handler.PipeManager.SetSIDMapper(mapper)(315).
- **Why it matters:** global mutable state defeats per-instance isolation; multiple *smb.Adapter instances clobber each other's SID mapper process-wide.
- **Fix:** add SIDMapper field to *handlers.Handler (mirror PipeManager.SetSIDMapper), read from Handler instance instead of package atomic; drop the globals once callers converted.
- **Verified:** CONFIRMED. Read at security.go:257,262,314,444,707,894, no Handler association. atomic.Pointer is race-safe, single-adapter-per-process in practice -> LOW.

### [LOW] checkShareModeConflict reimplements module-level hasReadAccess/hasWriteAccess/hasDeleteAccess with parallel hex-literal const block · `bloat` · area: handlers-core-handler
- **Where:** `internal/adapter/smb/handlers/handler.go:2409`
- **What:** local const block (fileReadData,fileWriteData,fileAppendData,fileExecute,deleteAccess,genericRead/Write/All,maxAllowed raw hex) + closures hasRead/hasWrite/hasDelete(2438-2456) bit-for-bit identical to module-level hasReadAccess/hasWriteAccess/hasDeleteAccess(2766-2799) using types.AccessMask, already called by newOpenIsShareRestrictive(2812) same file. Own comment(2448-2449) admits duplication.
- **Why it matters:** two independently-maintained copies of same predicates = values-drift risk on future AccessMask changes.
- **Fix:** delete local closures+consts(2414-2423,2437-2456), call existing hasReadAccess/hasWriteAccess/hasDeleteAccess. Keep only fileShareRead/Write/Delete share-bit consts(2410-2412).
- **Verified:** CONFIRMED. Reachable: checkShareModeConflict called from create.go:1400, create_post_break.go:295/488/537/1614. No current divergence -> LOW.

### [LOW] selectCipher and selectSigningAlgorithm are same intersection algorithm duplicated per-type · `bloat` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/negotiate.go:482`
- **What:** selectSigningAlgorithm(482-497)/selectCipher(508-528) both: walk client []uint16 pref list, return first present in server allowed []uint16 set, else fallback. Structurally identical, differ only in allowed-set source and fallback value.
- **Why it matters:** repo rule explicitly flags per-dialect duplicated intersection logic (negotiate contexts, signing/KDF labels).
- **Fix:** extract `func selectPreferred(clientList, allowed []uint16, fallback uint16) uint16`, call from both.
- **Verified:** CONFIRMED. Both live: selectCipher called negotiate.go:397, selectSigningAlgorithm at :420. Downgraded MED->LOW: two doc comments carry genuinely different spec rationale that must survive collapse.

### [LOW] Duplicated unprivileged-AuthContext construction block · `bloat` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/auth_helper.go:73`
- **What:** BuildAuthContext(73-94) and buildOpenerAuthContext guest/null branch(404-420) build identical 5-field metadata.AuthContext{Context,ClientAddr,LockClientID:fmt.Sprintf("smb:%d",ctx.SessionID),Identity:&metadata.Identity{},BypassTraverseChecking:true}, call setUnprivilegedIdentity, set ShareReadOnly = ctx.Permission==models.PermissionRead — verbatim.
- **Why it matters:** byte-for-byte repeat in same file; future field addition must be remembered in both places.
- **Fix:** extract `func newUnprivilegedAuthContext(ctx *SMBHandlerContext) *metadata.AuthContext`, call from both.
- **Verified:** CONFIRMED verbatim. Both live: BuildAuthContext from close.go:248/402/485/834, create.go:661, ioctl_sparse.go:97/186/426; buildOpenerAuthContext from close.go/set_info.go. LOW not MED — 8 lines, second site's comment already points at first.

### [LOW] Dead 'no metadata service' fallback branch in handleOpenRootCreate — metaSvc already dereferenced unchecked 18 lines earlier · `bloat` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:1880`
- **What:** line 1862 `metaSvc := h.Registry.GetMetadataService()`, immediately calls metaSvc.GetFile(...) no nil check (panics on nil via Service.GetFile->storeForHandle). Line 1880 re-fetches with shadowed `if metaSvc := ...; metaSvc != nil {...} else { grantedAccess = resolveAccessFlags(...) }` — fallback unreachable, nil case already crashed at 1863.
- **Why it matters:** speculative generality — impossible dead branch + redundant duplicate call to same registry accessor already held locally.
- **Fix:** reuse metaSvc local from 1862, drop shadow re-declaration and impossible nil branch; or check nil once right after 1862 and return error consistently.
- **Verified:** CONFIRMED unreachable. GetMetadataService returns concrete *metadata.Service (runtime.go:1021), so nil check is real but Service.GetFile->storeForHandle->GetStoreForShare (service.go:316/246/252) dereferences receiver -> panics at :1863 first. handleOpenRootCreate live (create.go:797). LOW not MED.

### [LOW] ResolveForWrite is byte-identical clone of ResolveForRead, justified only by speculative future divergence · `bloat` · area: handlers-read-write
- **Where:** `internal/adapter/common/resolve.go:31`
- **What:** ResolveForWrite(35-40) verbatim copy of ResolveForRead(24-29) — same nil check, same body calling reg.GetBlockStoreForHandle, differs only in error string. Comment admits: kept as separate helper for future []ChunkRef divergence "that hasn't happened."
- **Why it matters:** YAGNI/speculative-generality — second function exists only to pre-stage an interface change that hasn't happened.
- **Fix:** collapse to one `ResolveBlockStore(ctx, reg, handle)` called from both read.go/write.go; split later if write path actually needs []ChunkRef.
- **Verified:** CONFIRMED, bodies identical. Both live widely (nfs v3/v4 read/write/commit/remove/clone/deallocate, smb close.go/write.go/handler.go, common rename/truncate_reclaim). Cost real but tiny (6 LOC + v4/helpers.go:262-264 switch that only exists to pick between twins). Severity MED->LOW: no drift risk, no behaviour.

### [LOW] selectDialectFromList duplicates negotiate.go's selectDialect · `bloat` · area: handlers-ioctl-core
- **Where:** `internal/adapter/smb/handlers/ioctl_validate_negotiate.go:222`
- **What:** selectDialectFromList(222-244) re-implements same DialectPriority/minP/maxP/best-priority loop as negotiate.go:259 selectDialect(259-296), missing wildcard branch which caller validateLegacy bolts back on ad hoc: `if hasWildcard && selectedDialect==0 { selectedDialect = types.Dialect0202 }`.
- **Why it matters:** two independently-maintained copies of dialect-priority algorithm — repo guidelines call this class out explicitly; future selection-rule change only applied in negotiate.go silently desyncs VNEG's downgrade check from what NEGOTIATE actually selected.
- **Fix:** drop selectDialectFromList; have validateFromCryptoState (and validateLegacy) call h.selectDialect(dialects), use its returned hasWildcard bool.
- **Verified:** CONFIRMED. Both call sites(178,279) hang off handleValidateNegotiateInfo, registered ioctl_dispatch.go:24 (FsctlValidateNegotiateInfo). ~20 LOC, no current behavioural divergence. LOW not MED.

### [LOW] QFid create-context response block duplicated verbatim between root-open and normal-open paths · `bloat` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:1988`
- **What:** handleOpenRootCreate builds QFid resp: 32-byte buf, copy fileUUID[0:16], ServerGUID[16:32], append CreateContext. Same seq again in create_post_break.go:1470-1481, only baseFileUUID args + debug log differ.
- **Why it matters:** two independent copies of QFid wire layout drift if format changes.
- **Fix:** extract `h.buildQFidContext(fileUUID [16]byte) CreateContext`, call from both sites.
- **Verified:** CONFIRMED. 8 dup lines, 2 sites, both live (not test-only). Downgraded HIGH->LOW.

### [LOW] WRITE never advances OpenFile.PositionInfo (CurrentByteOffset) · `gaps` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/write.go:449`
- **What:** read.go calls recordReadProgress on every success path (MS-FSA 2.1.5.3). write.go has zero PositionInfo/CurrentByteOffset hits — CommitWrite success updates atime/notify only, position left stale.
- **Why it matters:** FilePositionInformation (query_info.go:751, set_info.go:1641) round-trips PositionInfo directly; client querying position after WRITE gets pre-write offset. MS-FSA 2.1.5.4 step 21 requires the same update WRITE-side.
- **Fix:** call recordReadProgress(openFile, req.Offset, written) (rename generically) on write success paths ~449-455.
- **Verified:** CONFIRMED spec-wrong. LOW: only observable via FilePositionInformation/FILE_ALL offset 80 post-WRITE, no conformance row depends on it.

### [LOW] closeFilesWithFilter deletes OpenFile entries without draining handleOps, unlike WaitAndDeleteOpenFile · `bugs` · area: handlers-core-handler
- **Where:** `internal/adapter/smb/handlers/handler.go:1635`
- **What:** WaitAndDeleteOpenFile (1202-1205) calls DrainHandleOps before removing map entry. closeFilesWithFilter (1636-1638) calls deleteOpenFileEntry directly, deletes handleOps after (1648-1650), never DrainHandleOps. DeleteOpenFile (1236-1242, pipe CLOSE) same gap.
- **Why it matters:** connection.go:404 skips BeginHandleOp for CommandClose "because the deleter drains via WaitAndDeleteOpenFile" — pipe CLOSE via DeleteOpenFile breaks that invariant: in-flight pipe READ/IOCTL can get spurious STATUS_FILE_CLOSED (compound_find_close class). Teardown legs weaker (MS-SMB2 3.3.5.6 only requires closing, not draining).
- **Fix:** close.go:174 use WaitAndDeleteOpenFile (drain outside renameScanMu) for pipes.
- **Verified:** CONFIRMED. Sharpest instance is pipe CLOSE not teardown. Severity MED->LOW (pipes are srvsvc-only traffic).

### [LOW] Anonymous NTLM re-authentication bypasses guest policy and is mislabeled as Guest instead of Anonymous/Null · `bugs` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:1116`
- **What:** completeNTLMAuth: anon+IsReauth calls tryReauthUpdate(pending,"anonymous","",nil,true) — isGuest hardcoded true. IsNull = username=="" && !isGuest = false (username literal "anonymous"). checkGuestPolicy() not called, unlike createAnonymousSession/createGuestSessionWithID/createGuestSession.
- **Why it matters:** with GuestEnabled=false, authenticated session can be downgraded in place to unauthenticated nobody identity policy says must not exist; new TREE_CONNECTs resolve at share defaultPerm. (No on-disk-perm divergence or signing-key divergence — both refuted: auth_helper.go:81 maps guest and null/anonymous to same nobody UID/GID; reauth retains prior signing keys.)
- **Fix:** gate on checkGuestPolicy(), pass isGuest=false so IsNull set, matching createAnonymousSession semantics.
- **Verified:** CONFIRMED but 2 of 3 original "why" legs refuted (perm identity, signing). Narrowed to policy-bypass only.

### [LOW] SET_INFO FileAllocationInformation silently no-ops on a too-short buffer instead of STATUS_INFO_LENGTH_MISMATCH · `bugs` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:1691`
- **What:** 8-byte AllocationSize read only inside `if len(buffer) >= 8`; every sibling class (FileBasicInformation, FilePositionInformation, FileModeInformation, FileDispositionInformation, decodeEndOfFileInfo) checks length and errors. This one falls through to `return setInfoStatus(types.StatusSuccess)` at 1736 on short/nil buffer.
- **Why it matters:** MS-FSCC 2.4.4 fixes struct at 8 bytes; MS-SMB2 3.3.5.21 requires STATUS_INFO_LENGTH_MISMATCH on short input. Windows returns it; server silently reports success while doing nothing — debugging trap.
- **Fix:** `if len(buffer) < 8 { return setInfoStatus(types.StatusInfoLengthMismatch), nil }` before the access checks.
- **Verified:** CONFIRMED. Severity MED->LOW: malformed-input conformance only, real clients always send 8 bytes, no data-integrity impact.

### [LOW] SessionCryptoState.Destroy is dead code — session/channel key material never zeroed on teardown · `security` · area: smb-session
- **Where:** `internal/adapter/smb/session/manager.go:88`
- **What:** DeleteSession body is only `m.sessions.Delete(sessionID)` — never calls session.GetCryptoState().Destroy() (crypto_state.go:233, zero callers repo-wide). RemoveChannel (channel.go:121) never clears Channel.SigningKey. SetCryptoState (session_setup.go:1950) also swaps state, orphaning old keys unzeroed.
- **Why it matters:** SessionKey/SigningKey/EncryptionKey/DecryptionKey/ApplicationKey stay live in memory after LOGOFF until GC, defeating the defense-in-depth Destroy exists for (heap read / core dump / swap disclosure post-logoff).
- **Fix:** call sess.GetCryptoState().Destroy() in CleanupSession before DeleteSession; clear Channel.SigningKey in RemoveChannel.
- **Verified:** CONFIRMED, reachable via handler.go:1978 CleanupSession. Severity MED->LOW: defense-in-depth only, no in-tree exploit, Go zeroization best-effort anyway (derived key copies elsewhere unreachable by Destroy).

### [LOW] OpenFile struct is a second god object: ~60 fields spanning 8 unrelated concerns behind one mutex · `structure` · area: handlers-core-handler
- **Where:** `internal/adapter/smb/handlers/handler.go:439`
- **What:** type OpenFile (439-814, 375 LOC, ~70 fields) mixes identity, enumeration cursor, delete-on-close, timestamp-freeze, delayed-write, lease/oplock, notify-buffer, durable/channel-sequence state.
- **Why it matters:** mixes 8 concerns in one struct; readers of one concern reason about writers of every other.
- **Fix:** group fields into named sub-structs (EnumerationState, DeleteOnCloseState, TimestampFreezeState, DelayedWriteState, LeaseState); keep or split mutex per proven contention.
- **Verified:** CONFIRMED size/concerns. "One mutex over 8 concerns" partly misread — name/Notify* fields are atomic, not mu-guarded; mu is per-handle so only same-FileID ops contend. Structure/readability finding, not a defect.

### [LOW] MS-FSA share-mode/rename-conflict algorithm is protocol-handler business logic, not protocol concern · `structure` · area: handlers-core-handler
- **Where:** `internal/adapter/smb/handlers/handler.go:2403`
- **What:** checkShareModeConflict (2403-2545), checkShareDeleteConflict (2580-2615), checkParentDirRenameConflict (2656-2692), snapshotOpenChildren/anyOpenChild/hasOpenHandleOnFile (2697-2764), isFileDeletePending/isFileOrBaseDeletePending (2297-2391) implement full MS-FSA 2.1.5.1.2.2 sharing-access as *Handler methods purely to reach h.files.
- **Why it matters:** repo invariant sends business logic to pkg/metadata/stores; this is that class of logic, just keyed off in-memory open state instead of the store — can't move to pkg/metadata verbatim but can move out of the Handler god object.
- **Fix:** extract to `handlers/sharemode` taking `type OpenTable interface { Range(func(*OpenFile) bool) }` (accept-interface-at-consumer).
- **Verified:** CONFIRMED locations/reachability, but framing overstates CLAUDE.md invariant (share-mode state is in-memory protocol-session state, can't live in pkg/metadata — claim concedes this). Refactor suggestion, not invariant violation.

### [LOW] Share-access bit constants (0x01/0x02/0x04) declared three times under three names · `structure` · area: handlers-core-handler
- **Where:** `internal/adapter/smb/handlers/handler.go:2409`
- **What:** checkShareModeConflict redeclares local fileShareRead/Write/Delete=0x01/0x02/0x04 (2409-2412); checkShareDeleteConflict redeclares fileShareDelete=0x04 again (2581); package-level smbShareRead/Write/Delete (disconnected_state_machine.go:162-164, same values) used elsewhere in same file (2681, 2816-2822).
- **Why it matters:** three names for same three bits — future 4th-bit addition silently misses copies.
- **Fix:** delete both local const blocks (2409-2412, 2581), use existing smbShareRead/Write/Delete everywhere in file.
- **Verified:** CONFIRMED verbatim at all cited lines. Pure drift risk, no current defect.

### [LOW] session_setup.go is one file mixing 4 unrelated concerns at 2,133 LOC · `structure` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:1`
- **What:** one file = NTLM state machine (SessionSetup:132, handleNTLMNegotiate:974, completeNTLMAuth:1059), channel-bind subsystem (~430 LOC: 404/454/472/558/720/784/808), NETLOGON domain-fallback (~270 LOC: 1388/1490/1563/1602/1627), crypto/response setup (1795/2010/2044).
- **Why it matters:** 2,133-LOC file spans 4 independent state machines; navigation cost.
- **Fix:** split into session_bind.go, netlogon_fallback.go, keep auth+crypto in session_setup.go.
- **Verified:** CONFIRMED size/clusters/lines. Caveat: cited "one command family per file" rule doesn't exist in CLAUDE.md — finding rests on size alone. No behavioral consequence.

### [LOW] 8 issue-number references embedded in behavioral comments violate the repo's own comment rule · `structure` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:991`
- **What:** CLAUDE.md bans issue/PR numbers in comments (ponytail: sole exception). session_setup.go embeds them 8x: 991 "(issue #362)", 1309 "(#1317)", 1316 "(#1314)", 1318 "(#1632)", 1369 "(#1314)", 1371 "(#1317)", 1466 "(#1632)", 1562 "(#1357)" — all in behavioral-rationale prose, not deletable outright.
- **Why it matters:** project's own explicit rule violated 8x in one file; numbers go stale as trackers renumber/migrate.
- **Fix:** strip the parenthetical `(#NNNN)` from each, keep behavioral text.
- **Verified:** CONFIRMED all 8 sites via grep, none prefixed ponytail:. Convention-only, zero runtime effect.

### [LOW] resolveIdentityMapping hardcodes provider="kerberos" for the NTLM-only caller · `structure` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/identity_resolver.go:33`
- **What:** resolveIdentityMapping queries ims.GetIdentityMapping(ctx, "kerberos", principal) — literal, not parameter. Sole caller session_setup.go:1134 is NTLM AUTHENTICATE path passing "DOMAIN\\user". Contrast: kerberos_auth.go:534-539 builds real Provider:"kerberos" for krb5 principals; pkg/adapter/identity.go:88-91 uses genuine dynamic provider param.
- **Why it matters:** value that varies by protocol baked in as literal instead of threaded as param — the one pattern this same store already does right one layer up.
- **Fix:** add `provider string` param, pass "ntlm" from session_setup.go:1134, "kerberos" from kerberos callers; fix doc comment.
- **Verified:** CONFIRMED literal + reachability. Severity MED->LOW: "kerberos" is the system-wide DEFAULT bucket by construction (models/identity_mapping.go:14, store/identity.go:41 force empty->kerberos, apiclient defaults too), so normal creation paths land where SMB reads. Gap narrows to: operator who explicitly POSTs provider_name="ntlm" gets a row nothing reads. Collision claim (real krb5 "user@REALM" vs NTLM "DOMAIN\\user") not credible.

### [LOW] tree_connect.go/tree_disconnect.go are a ready-made split target out of the 34k-LOC handlers package · `structure` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_connect.go:1`
- **What:** 556 LOC combined (457+99), self-contained command family (TREE_CONNECT/TREE_DISCONNECT, MS-SMB2 2.2.9-2.2.12), clean dep surface: h.Registry, h.SessionManager, h.GenerateTreeID/StoreTree/GetTree/DeleteTree, h.CloseAllFilesForTree, h.PendingLockRegistry, h.LockWaitGraph, h.EncryptionConfig.
- **Why it matters:** handlers package is 34,430 LOC/177 files; this pair is smallest/lowest-risk boundary to peel off first, demonstrates the pattern.
- **Fix:** new subpackage, exported TreeConnect/TreeDisconnect + TreeConnection type; parseSharePath/calculateMaximalAccess/handleIPCShare/resolveSharePermission stay unexported (permission logic itself moves out separately).
- **Verified:** CONFIRMED all facts (LOC, surface, reachability). No defect — refactor opportunity only.

### [LOW] calculateMaximalAccess re-declares access-mask bit constants that already exist in types.AccessMask · `structure` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_connect.go:226`
- **What:** calculateMaximalAccess (222-246) locally redeclares fileReadData=0x1, fileWriteData=0x2, fileReadEA=0x8, readControl=0x20000, synchronize=0x100000, fullAccess=0x001F01FF; types/constants.go:612-631 already exports typed AccessMask equivalents used elsewhere in package.
- **Why it matters:** two independent bit-table definitions = drift risk if a bit changes in one but not the other.
- **Fix:** replace local const block with types.FileReadData|types.FileWriteData|... ; also collapse the ~15-test-file-duplicated 0x001F01FF into one exported constant.
- **Verified:** CONFIRMED at 222-246, reachable via calculateMaximalAccess called at 184. Values currently agree (no live bug). Nit: local genericRead is a computed union, not same as types.GenericRead — only individual bits duplicate.

### [LOW] Reconnect-context detection (same two FindCreateContext calls) computed independently 3 times · `structure` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:576`
- **What:** FindCreateContext(...DurableHandleV1/V2ReconnectTag) evaluated 3x: isDurableReconnect (576-579, plus ShareName!=IPC$ guard), hasReconnectCtx (793-795), hasDHnC/hasDH2C (812-813). IPC$ already returns at 649-653, before 793, making the extra guard on isDurableReconnect moot at both later sites.
- **Why it matters:** three local names answering the identical question — duplicated per-context lookup pattern.
- **Fix:** compute once after context parse, reuse isDurableReconnect as sole source of truth at 793 and 812, delete re-derivations.
- **Verified:** CONFIRMED all 3 sites + IPC$ return ordering. Pure duplication, no behavioral bug.

### [MED] handleKerberosAuth and completeKerberosBind duplicate the entire AP-REQ authenticate+resolve+lookup preamble · `bloat` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:39`
- **What:** Lines 39-105 (handleKerberosAuth) and 351-391 (completeKerberosBind) near-identical: KerberosService nil check, basePrincipal/smbPrincipal via deriveSMBPrincipal, extractAPReqFromGSSToken, Authenticate, resolveKerberosIdentity, userStore nil check, GetUser+resolveSessionUser — only log strings differ.
- **Why it matters:** ~50 lines auth logic in two places; fix to one path (new failure mode, new resolveSessionUser case) easy to miss on the other.
- **Fix:** Extract `authenticateKerberosTicket(ctx, mechToken) (*kerbauth.AuthResult, *models.User, *HandlerResult)`; both callers reduce to one call plus bind-specific identity-match/channel-registration.
- **Verified:** CONFIRMED. Same 8 steps same order both sites; only log prefixes + primary path's extra info log differ. handleKerberosAuth reachable from session_setup.go:323, completeKerberosBind from :689.

### [MED] completeKerberosBind re-implements buildKerberosAcceptResponse's AP-REP + mechListMIC + SPNEGO response construction · `bloat` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:461`
- **What:** buildKerberosAcceptResponse (248-328) and completeKerberosBind tail (461-491) both build AP-REP via BuildMutualAuth+WrapGSSToken, pick responseOID via clientKerberosOID, compute serverMIC via ComputeMechListMIC when client sent one, call BuildAcceptCompleteWithMIC with BuildAcceptComplete fallback.
- **Why it matters:** Second copy of response assembly; completeKerberosBind never calls buildKerberosAcceptResponse so the two drift — already do: bind fallback (:483) keeps apRepToken, primary fallback (:320) drops it.
- **Fix:** Factor AP-REP+MIC+SPNEGO assembly into one shared helper both call; settle fallback on apRepToken-preserving form.
- **Verified:** CONFIRMED, drift real. Give buildKerberosAcceptResponse a `micAlreadyVerified bool`, call from completeKerberosBind. On MIC-build failure primary path silently drops mutual auth while bind path keeps it — worth fixing beyond just dedup.

### [MED] calculateMaximalAccess reinvents types.AccessMask bit constants · `bloat` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_connect.go:222`
- **What:** Local const block (fileReadData=0x1 … synchronize=0x100000) re-declares types.FileReadData…types.Synchronize already at types/constants.go:611-632, package already imports types.
- **Why it matters:** Needless duplication of canonical AccessMask constants used everywhere else in handlers; future MS-DTYP bit fix has to land in two places.
- **Fix:** Delete local const block; build genericRead from types.FileReadData|types.FileReadEA|types.FileReadAttributes|types.ReadControl|types.Synchronize; keep fullAccess=0x001F01FF.
- **Verified:** CONFIRMED value-for-value, calculateMaximalAccess live (maximalAccess written :217). Worse than claimed: 9 of 14 locals never referenced in the function at all, only genericRead's 5 used.

### [MED] Break-wait dispatch logic duplicated verbatim between sync fallback and async resume goroutine · `bloat` · area: handlers-create-post-break
- **Where:** `internal/adapter/smb/handlers/create_post_break.go:470`
- **What:** breakAndMaybeParkCreate sync fallback (470-496) and parkCreateOnLeaseBreak resume goroutine (1604-1633) both implement identical shareConflictWait branch (WaitForShareConflictClear vs WaitForOtherKeyBreaks), logs differ only by sync/async prefix. Sync path caches initialConflict/conflictComputed, async does not.
- **Why it matters:** Two sites reimplement same wait; future change to wait semantics needs two edits; already drifted once (caching difference).
- **Fix:** Extract `h.waitForBreakOutcome(waitCtx, d, lockFileHandle, shareName, waitExceptKey, shareConflictWait, conflictComputed, initialConflict, logPrefix)`, call from both.
- **Verified:** CONFIRMED, both live — async park attempted first (:455), sync is no-slots fallback. Fix: shared helper with a seed param for the sync-only cache.

### [MED] DecodeWriteRequest carries a speculative, untested fallback for locating the data buffer · `bloat` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/write.go:118`
- **What:** Lines 118-134: primary dataStart from req.DataOffset, else-if fallback assumes data starts right after 48-byte fixed structure when primary range doesn't fit. No test exercises it.
- **Why it matters:** No protocol case where fallback is right but primary is wrong; per MS-SMB2 3.3.5.13, mismatch MUST fail STATUS_INVALID_PARAMETER, not substitute a guessed offset — malformed/padded WRITE gets wrong bytes committed instead of rejected.
- **Fix:** Drop fallback branch; validate dataStart+Length against len(body) once, return "write request body too short" error on mismatch, matching DecodeReadRequest's straight-line decode.
- **Verified:** CONFIRMED, zero test coverage (DecodeWriteRequest only in write.go/dispatch.go repo-wide). Spec-confirmed MUST-fail condition silently substituted with wrong bytes.

### [MED] 45-line async-callback wiring block duplicated verbatim across the two dispatch entrypoints · `bloat` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/response.go:216`
- **What:** response.go:216-263 (ProcessSingleRequest) and :582-624 (ProcessRequestWithFileIDAndCallback) wire identical 5 async-callback setups (CHANGE_NOTIFY, READ pipe-read, CREATE, LOCK, CANCEL) over `ci := connInfo` closures, differ only in comment wording. Peer-encryption sticky-track block (191-195/561-566) duplicated same way.
- **Why it matters:** Two copies of per-command async dispatch; third command or bugfix needs two edits — already happened once (comment-only divergence).
- **Fix:** Extract `wireAsyncCallbacks(handlerCtx, reqHeader, connInfo, asyncNotifyCallback)`, call from both entrypoints (or fold into prepareDispatch, already shared). Same for sticky-track snippet.
- **Verified:** CONFIRMED near-verbatim, second comment literally says "see ProcessSingleRequest". Both entrypoints prod: ProcessSingleRequest <- connection.go:376,449; other <- compound.go:167, response.go:665.

### [MED] Same compound-header-walk loop duplicated 3x instead of one iterator helper · `bloat` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:69`
- **What:** Identical skeleton `rem := compoundData; for len(rem) >= header.HeaderSize { ParseCompoundCommand(rem); if err break; rem = nextRem; ... }` at lines 69-82, 131-151, 480-489 — each re-parses the whole compound chain independently.
- **Why it matters:** Three copies of walk-and-parse skeleton; fix to bounds check/error handling must replicate 3x or drifts silently. File's own compoundLoopState split exists precisely to avoid this for the main loop.
- **Fix:** One `walkCompoundHeaders(data []byte, fn func(hdr *header.SMB2Header) bool)` helper, all three sites call it with a per-header closure.
- **Verified:** CONFIRMED 3 verbatim copies, prod-reachable (ProcessCompoundRequest/failEntireCompound). compoundData excludes first cmd so no dup-response bug, pure duplication.

### [MED] Sign() body is byte-for-byte triplicated across all three Signer impls · `bloat` · area: smb-crypto
- **Where:** `internal/adapter/smb/signing/hmac_signer.go:31`
- **What:** HMACSigner.Sign (hmac_signer.go:31-40), CMACSigner.Sign (cmac_signer.go:139-148), GMACSigner.Sign (gmac_signer.go:54-63) identical 8-line body: length check, alloc msgCopy, copy, zeroSignatureField, SignInPlace.
- **Why it matters:** Codebase already knows the fix — verifySig (signing.go:81) factors the same pattern for all three Verify(). Sign() wasn't given same treatment; future buffer-copy change needs 3 synced edits.
- **Fix:** Add `signWithCopy(s Signer, message []byte) [SignatureSize]byte` next to verifySig in signing.go; each Sign() becomes `return signWithCopy(s, message)`.
- **Verified:** CONFIRMED byte-identical bodies at all three sites. Precedent (verifySig) already exists in same file.

### [MED] TreeConnect never checks negotiated dialect before honoring Share.EncryptData — encrypted share silently unusable instead of TREE_CONNECT failing · `gaps` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_connect.go:316`
- **What:** shouldRejectUnencryptedTreeConnect (316-321) rejects only when global mode=="required" AND share.EncryptData; never checks sess.Dialect. Share.EncryptData=true + mode=="preferred" (default) + client on 2.0.2/2.1 (no cipher derived) → TREE_CONNECT succeeds, tree.EncryptData=true stored, then checkRequestEncryption (response.go:703-712) denies every subsequent request on that tree.
- **Why it matters:** MS-SMB2 3.3.5.7 requires failing TREE_CONNECT itself with ACCESS_DENIED when Share.EncryptData is TRUE but Connection.Dialect isn't SMB 3.x, so client gets one clear failure instead of a tree that's dead after appearing to connect.
- **Fix:** In shouldRejectUnencryptedTreeConnect, also reject when `share.EncryptData && sess.Dialect < types.Dialect0300`, independent of encryptionMode.
- **Verified:** CONFIRMED. Default mode "preferred" (config.go:192), selectDialect picks 2.x when offered. Real config field (models/share.go:28). Spec nuance: MS-SMB2's own ACCESS_DENIED is conditioned on RejectUnencryptedAccess TRUE — Windows in preferred mode lets 2.x use share in plaintext; DittoFS denies everything either way, deviation real regardless.

### [LOW] ADS base-file existence check swallows lookup error, may create file after real backend error · `bugs` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:1291`
- **What:** `if f, _, _ := h.lookupCaseInsensitive(...); f == nil {` discards error return; any real backend failure indistinguishable from "not found", falls into ADS auto-create path.
- **Why it matters:** Sibling call 4 lines earlier (create.go:1231) correctly checks lookupErr and returns MapToSMB(lookupErr); here error thrown away, genuine store error reinterpreted as "create it".
- **Fix:** `f, _, lookupErr := h.lookupCaseInsensitive(...); if lookupErr != nil { return CreateResponse{Status: common.MapToSMB(lookupErr)}, nil }; if f == nil { ... }`.
- **Verified:** Confirmed code as described. Downgraded HIGH->LOW: consequence benign — falls into metaSvc.CreateFile which re-runs same permission/store path and surfaces genuine error via MapToSMB; ErrAlreadyExists already handled as "another goroutine won". Worst case mis-mapped NTSTATUS, not access bypass or spurious file.

### [LOW] RDMA Channel / WriteChannelInfo fields decoded but never validated on READ and WRITE · `bugs` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/write.go:104`
- **What:** WriteRequest.Channel decoded (104), WriteChannelInfoOffset/Length read then discarded via r.Skip(4) (106). Neither Write() nor Read() (read.go:104 decodes req.Channel) ever checks these. Request with Channel!=0 processed as normal inline write.
- **Why it matters:** No RDMA capability ever negotiated (negotiate.go advertises no RDMA transform). Per MS-SMB2 3.3.5.12/3.3.5.13, non-zero Channel on non-RDMA connection MUST fail STATUS_INVALID_PARAMETER — channel info is a buffer descriptor, not file data. Silently treating it as file bytes accepts a request that should be rejected on the hottest data-plane path.
- **Fix:** In both Read() and Write(), next to MaxReadSize/MaxWriteSize clamp, add `if req.Channel != 0 { return ...StatusInvalidParameter }`. For WRITE also decode WriteChannelInfoOffset/Length into fields instead of Skip(4), reject when Length != 0.
- **Verified:** CONFIRMED, Channel never validated anywhere. No RDMA anywhere in adapter. Severity LOW: no client sets Channel without negotiated RDMA transport; fallout is mis-parsed request, not memory unsafety or auth bypass.

### [LOW] Lost-update race on lease bindings map: concurrent same-key RequestLease can clobber a winning grant's binding · `bugs` · area: smb-lease-manager
- **Where:** `internal/adapter/smb/lease/manager.go:327`
- **What:** requestLeaseInternal pre-registers binding for ck under lm.mu (327-355), releases lock, calls lockMgr without holding lm.mu. On failure/None grant, restorePreRegistration (362-370) unconditionally writes back stale prev/hadPrev snapshot — can clobber a concurrent same-ck call's real granted binding. Same unconditional-write shape at 469-471 in AcknowledgeLeaseBreak (delete on ErrLeaseAckNotFound, no re-check of concurrent write).
- **Why it matters:** leaseClientKey has no generation/version field, no re-validation before overwrite. LockManager holds valid granted lease record but SMB-side binding stale/gone; break routing resolves wrong session or "not found", OnOpLockBreak silently drops break notification until LockManager's own 5s/35s timeout force-completes.
- **Fix:** Compare-and-swap guard before restoring/deleting: only mutate lm.bindings[ck] if current value still equals what this call last wrote (compare SessionID/HandleKey, or generation counter). Apply same guard to AcknowledgeLeaseBreak delete.
- **Verified:** CONFIRMED TOCTOU. Downgraded HIGH->LOW: needs two concurrent requests on same client+share+lease key where one is denied/grants None; lock manager's break timeout bounds the damage.

### [LOW] Malformed ACE Size field in inbound Security Descriptor causes a slice-bounds panic (remote DoS) · `security` · area: handlers-security-sd
- **Where:** `internal/adapter/smb/handlers/security.go:924`
- **What:** parseACEs reads untrusted uint16 aceSize (918), only checks upper bound `offset+int(aceSize) > len(data)` (924). aceSize 0..7 (< aceHeaderSize=8) makes next slice `data[offset+aceHeaderSize : offset+int(aceSize)]` (929) low>high — runtime panic. Reachable from SET_INFO FileSecurityInformation (set_info.go:2145) and CREATE SMB2_CREATE_SD_BUFFER (create_post_break.go:827).
- **Why it matters:** Fully untrusted self-relative binary per MS-DTYP 2.4.4.2/2.4.5; single ACE with AceSize<8 in CREATE or SET_INFO Security request crashes handling goroutine.
- **Fix:** `if aceSize < aceHeaderSize { return nil, fmt.Errorf("ACE %d size %d smaller than ACE header", i, aceSize) }` before line 929's slice.
- **Verified:** CONFIRMED panic condition. Severity HIGH->LOW: claim of "no recover() guarding SMB request handling" is WRONG — connection.go:435/446 defer handleRequestPanic per-request goroutine (recover+log+release), :475 recovers at connection level. Panic contained, no server crash/leak, attacker only loses own request's response. Fix still one line.

### [LOW] Naked fire-and-forget goroutine completing pending pipe READ · `structure` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/write.go:624`
- **What:** handlePipeWrite spawns `go func(){ pending.Callback(...) }()` with no owner, cancellation, join, or panic recovery.
- **Why it matters:** Violates repo naked-goroutine rule; if Callback blocks (bad conn, backpressure) or panics, goroutine leaks silently forever, no way to observe/stop from session/pipe teardown.
- **Fix:** Route through same worker/registry-owned dispatch (PipeReadRegistry or bounded pool tied to h's shutdown ctx), or run synchronously with ctx-bounded timeout, log+drop on timeout.
- **Verified:** CONFIRMED, reachable via WRITE on IPC$ pipe handle. Demoted HIGH->LOW: bounded to at most one pending read per pipe FileID (PipeReadRegistry indexes by FileID, displaces); goroutine is single conn write not metadata round trip; identical shape is the established in-package idiom (pipe_read_registry.go:66 does the same). One instance of a package pattern, not a lone rule break — fix both sites together if fixed at all.

### [LOW] VerifyCompoundCommandSignature collapses 3 failure classes into unstructured errors — mandatory-disconnect case silently downgraded · `structure` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:654`
- **What:** VerifyCompoundCommandSignature (654-709) returns plain fmt.Errorf for 3 outcomes: missing signature (678), SMB 3.1.1 unsigned-unencrypted-to-authenticated-session downgrade requiring connection termination per MS-SMB2 3.3.5.2.4 (686), bad signature (702). No sentinel/typed error. Sole caller processRemaining (895-899) maps all three to StatusAccessDenied+break, cannot distinguish.
- **Why it matters:** Errors idiom rule: keep sentinel reachable via errors.Is/As so callers can branch on failure class. The one bit the caller needs (disconnect-required vs ordinary signature miss) is lost by construction.
- **Fix:** Sentinel errors (errSignatureMissing, errMustDisconnectDowngrade, errSignatureInvalid) or typed error with disconnect bool; processRemaining errors.As-switches, closes/marks connection for teardown on the disconnect sentinel instead of just appending ACCESS_DENIED and continuing.
- **Verified:** PARTLY CONFIRMED, impact overstated. Gap narrow: single-command path DOES enforce disconnect (framing.go:448-461 falls through connection loop's close-on-error); only compound sub-commands 2..N miss teardown. Request still REJECTED with ACCESS_DENIED — no auth bypass, only missing teardown. Downgrade HIGH->LOW. Fix: sentinel errRequiresDisconnect, errors.Is in processRemaining, propagate to close conn like framing.go does.

### [LOW] ADS/stream-name parsing (37-line switch mutating filename) is an unnamed block, not a function · `structure` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:689`
- **What:** Lines 689-724 parse NTFS colon-stream syntax out of `filename` via switch on uppercased colon suffix (::$DATA, ::$INDEX_ALLOCATION, :$I30:$INDEX_ALLOCATION, named ADS w/wo :$DATA), mutating `filename`, setting outer-scope streamSuffix + explicitDataStream; two early-return checks (727-729, 751-753) depend on outputs.
- **Why it matters:** Pure string transform, no Handler/req/ctx dep beyond filename + tree.StreamsDisabled — textbook standalone helper, matches siblings normalizeCreatePath (187) and decodeCreateContexts (327) already extracted. One inconsistency in otherwise well-factored parsing.
- **Fix:** Extract `func parseStreamSuffix(filename string) (base, suffix string, explicitDataStream bool)` for 689-724; keep StreamsDisabled/empty-name status checks in Create() (need `tree`, return SMB status).
- **Verified:** Confirmed. Reachable via Create(). Opportunity not defect — LOW.

### [LOW] pendingRegistry[V] generic core leaks its locking discipline to every consumer · `structure` · area: handlers-create-post-break
- **Where:** `internal/adapter/smb/handlers/pending_registry.go:31`
- **What:** pendingRegistry[V] (line 31) claims to centralize the generic core (byAsyncID map, indexes, buckets, single mutex) but exposes no validated insert. PendingCreateRegistry.Register (pending_create_registry.go:169-192) and PendingLockRegistry.Register (pending_lock_registry.go:143-166) both do `r.reg.mu.Lock()` / read `r.reg.byAsyncID` / call `r.reg.lookupLocked` / `r.reg.insertLocked` directly. ReplaceCallback (:254-256) and MarkStarted (:274-276) also poke fields raw.
- **Why it matters:** Lock+dup-check+insert copy-pasted 2x (pipe_read_registry.go:52-63 is a different shape — replace-on-collision, no cap/dup checks — so 2x copy + 1 variant, not 3x). Same-package unexported access = zero compile-time protection against a 4th registry getting lock order or a check wrong. No API boundary violated, no observed lock-order bug — same-package only.
- **Fix:** Add `func (r *pendingRegistry[V]) tryInsert(p *V, dupIdx []int, maxOps int) error` doing capacity+dup lookups+insertLocked under one `mu.Lock()`; callers map sentinel to ErrTooManyPendingCreates/ErrDuplicateMessageID/ErrDuplicateAsyncId. Add `mutateLocked(asyncID, fn)` for ReplaceCallback/MarkStarted.
- **Verified:** Confirmed, corrected count (2x+variant not 3x). Reachable via Handler registries, CREATE/LOCK dispatch. LOW.

### [LOW] Duplicated tree/session/handle-ownership validation block between Read and Write · `structure` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/read.go:252`
- **What:** read.go:252-269 and write.go:275-293 near-identical: GetTree→StatusInvalidHandle, GetSession→StatusUserSessionDeleted, TreeID/SessionID-vs-openFile mismatch→StatusFileClosed, then primeAuthContextFromOpenFile. ~18 lines dup, only log prefix/comment differ.
- **Why it matters:** Duplicated code collapsible behind one helper. Future fix to this gate needs hand-applying in both places.
- **Fix:** Extract `func (h *Handler) validateOpenOwnership(ctx *SMBHandlerContext, openFile *OpenFile, op string) (types.NTStatus, bool)`; Read and Write call it, return early on !ok.
- **Verified:** Confirmed near-verbatim, both reachable from READ/WRITE dispatch. Correct per MS-SMB2 3.3.5.2.5 today, drift risk only. LOW.

### [LOW] VALIDATE_NEGOTIATE_INFO hand-rolls IOCTL input-buffer parsing instead of reusing shared parser · `structure` · area: handlers-ioctl-core
- **Where:** `internal/adapter/smb/handlers/ioctl_validate_negotiate.go:58`
- **What:** handleValidateNegotiateInfo re-derives InputCount from raw `body[28:32]` and hardcodes `bufferStart := uint32(56)` (58-78) instead of calling shared `parseIoctlInputData(body)` (ioctl_fsctl.go:285-305, used by 8 other sites incl. handleSetCompression) which reads the actual wire InputOffset.
- **Why it matters:** Two independent parses of same fixed layout drift. parseIoctlInputData honors client's actual InputOffset; this copy assumes offset 56 always, never reads InputOffset field. Interop risk overstated though — Samba's smbd_smb2_request_process_ioctl rejects any in_input_offset != SMB2_HDR_BODY+fixed-body, so pinning 56 is not non-conformant; finding is duplication/drift, not correctness.
- **Fix:** Delete inline parse (58-78), call `parseIoctlInputData(body)` instead; drop manual bufferStart/inputCountR logic.
- **Verified:** Confirmed. Reachable via ioctlDispatch (ioctl_dispatch.go:24) → Handler.Ioctl. Severity cut MED→LOW (duplication only, not an interop bug).

### [LOW] Truncate-and-reclaim sequence duplicated between FileEndOfFileInformation and FileAllocationInformation · `structure` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:1531`
- **What:** FileEndOfFileInformation (1595-1637) and truncate-down sub-case of FileAllocationInformation (1703-1731) both run SetFileAttributes(Size)→common.ReclaimTruncatedBlocks→h.restoreFrozenTimestamps→flushSmbDelayedWrite→h.StoreOpenFile→h.breakParentDirLeasesForContentChange→notifyOpenFileModified(FileNotifyChangeSize), same order, twice.
- **Why it matters:** Named idiom violation (dup logic collapsible behind one fn). Already drifted: alloc copy omits metaSvc.CheckLockForIO conflict check (1581) and purgeConflictingDisconnectedHandlesForDataChange (1559) that EOF copy does — though EOF comment says lock check is a Windows-parity extra not MS-FSA-required, so may be deliberate. No confirmed wrong output.
- **Fix:** Extract shared `h.truncateOpenFile(ctx, authCtx, openFile, newSize, preFile)` for the 7-call sequence; each branch keeps own gate/lease-break, both call shared helper.
- **Verified:** Confirmed, both reachable (live SET_INFO arms). Maintainability only. LOW.

### [LOW] ProcessLeaseCreateContext: 11 positional params, 3 adjacent bools + 2 adjacent strings — swap hazard · `structure` · area: handlers-lease-oplock
- **Where:** `internal/adapter/smb/handlers/lease_context.go:353`
- **What:** ProcessLeaseCreateContext(ctx, leaseMgr, ctxData, fileHandle, sessionID, clientGUID, clientID, shareName, isDirectory, disallowWriteLease, statOpen) — 11 args. clientID/shareName adjacent strings; isDirectory/disallowWriteLease/statOpen three adjacent bools disambiguated only by call-site comments. Both call sites (create.go:1914, create_post_break.go:1018) pass three bare bools in a row.
- **Why it matters:** Long positional-bool lists are a known transposition hazard, nothing at compile time stops a swap. Same hazard already worse one layer down: lease/manager.go:294-311 has 11 params with isDirectory/isTraditionalOplock/statOpen adjacent too.
- **Fix:** Introduce `LeaseGrantRequest` struct carrying ctxData, fileHandle, sessionID, clientGUID, clientID, shareName, isDirectory, disallowWriteLease, statOpen; keep ctx/leaseMgr positional. Call sites become self-documenting struct literals.
- **Verified:** Confirmed verbatim. Reachable from create.go:1914, create_post_break.go:1018. No live defect. LOW.

### [LOW] Exported lease/oplock wire-format API has zero consumers outside package handlers · `structure` · area: handlers-lease-oplock
- **Where:** `internal/adapter/smb/handlers/lease_context.go:88`
- **What:** DecodeLeaseCreateContext, EncodeLeaseResponseContext, FindCreateContext, ProcessLeaseCreateContext, EncodeCreateContexts, LeaseCreateContext, LeaseResponseContext, LeaseBreakNotification, LeaseBreakAcknowledgment (lease_context.go) + OplockBreakRequest/Response/Notification + OplockLevel* constants (oplock_constants.go) all exported. Repo-wide grep `handlers\.<name>` returns 0 hits — every call site is same-package (create.go, create_post_break.go, _test.go).
- **Why it matters:** Minimise-exported-surface idiom, stated explicitly in repo's own review checklist. Not even test-only — prod callers same-package too. Inflates already-oversized public API of 34k-LOC package (named split candidate); every export is a constraint the future handlers/lease subpackage split has to account for despite zero external need.
- **Fix:** Unexport all zero-external-ref names: decodeLeaseCreateContext, encodeLeaseResponseContext, findCreateContext, processLeaseCreateContext, encodeCreateContexts, leaseCreateContext, leaseResponseContext, leaseBreakNotification, leaseBreakAcknowledgment, oplockBreakRequest/Response/Notification, oplockLevelNone/II/Exclusive/Batch/Lease. Re-export only what a future handlers/lease subpackage actually needs.
- **Verified:** Confirmed 0 external hits across all named symbols. Symbols live in-package prod code — excess export surface, not dead code. LOW.

### [LOW] ProcessLeaseCreateContext swallows every non-sentinel error (config/unexpected included) at Debug, always returns nil error · `structure` · area: handlers-lease-oplock
- **Where:** `internal/adapter/smb/handlers/lease_context.go:454`
- **What:** `} else if err != nil { logger.Debug(...); grantedState = lock.LeaseStateNone; epoch = 0 }` — every error other than ErrLeaseBreakInProgress/ErrLeaseKeyInUse logged Debug and discarded; final `return &LeaseResponseContext{...}, nil` (558) never surfaces err. Includes unexpected errors e.g. requestLeaseInternal's `fmt.Errorf("no lock manager for share %q", shareName)` (lease/manager.go:313-315), a config/wiring-error class.
- **Why it matters:** Violates repo's own convention (expected errors Debug, unexpected Error). Doesn't distinguish client-denial (fine, degrade) from server misconfig (should be Error-logged). Both collapse into same silent path. Severity capped LOW: degrading to LeaseState=None is itself spec-legal (MS-SMB2 3.3.5.9.8 — server may grant no lease for any reason), so client-visible behavior is correct; defect is observability only.
- **Fix:** Use errors.Is/As (or typed sentinel) to separate classes: keep Debug+continue for known denials, log unexpected at logger.Error.
- **Verified:** Confirmed. Reachable from create.go:1914, create_post_break.go:1018. LOW.

### [LOW] durable-handle command family (2,334 LOC) not split out of the 34k-LOC handlers package · `structure` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_context.go:1`
- **What:** durable_context.go(1117)+disconnected_state_machine.go(623)+durable_scavenger.go(227)+replay_cache.go(367)=2334 LOC, self-contained command family (DHnQ/DH2Q/DHnC/DH2C, disconnect/purge state machine, scavenger, replay cache) loose in package handlers. Split blockers: disconnected_state_machine.go touches h.disconnectedMu/disconnectedByFile/disconnectedTotal (73-140), h.durablePurgeMu (347), h.DurableStore (344,349), h.Registry (421); durable_scavenger.go touches s.handler.Registry (146,159), calls s.handler.forgetDisconnectedHandle (205); ProcessAppInstanceId takes *Handler wholesale (907-910). replay_cache.go has NO Handler dep — only *OpenFile/*CreateResponse.
- **Why it matters:** One-command-family-per-package is stated ceiling for this refactor pass; smallest, most self-contained family, cheapest first cut.
- **Fix:** New package internal/adapter/smb/handlers/durable. Move replay_cache.go verbatim first. For other 3, replace direct *Handler access with narrow interface (MarkDisconnected, ForgetDisconnected, HasDisconnected, PurgeMu, DurableStore, MetadataService, CloseFilesWithFilter, ReleaseLease) implemented by *Handler.
- **Verified:** LOC counts confirmed exact (1117+623+227+367=2334; handlers pkg 34,430 non-test matches "34k"). Blockers verified at cited lines. Pure package-layout proposal, no defect. Severity dropped MED→LOW.

### [LOW] ProcessAppInstanceId takes the whole *Handler god object, unlike its siblings · `structure` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_context.go:907`
- **What:** func ProcessAppInstanceId(ctx, durableStore, handler *Handler, contexts) at 907-1039 ranges handler.files (953), calls handler.closeFilesWithFilter (967), drives handler.LeaseManager.ReleaseLeaseForHandle/UnregisterOplockFileID/SignalParkedCreates (984-991) — entire multi-registry surface. Siblings ProcessDurableHandleContext/ProcessDurableReconnectContext take only specific collaborators.
- **Why it matters:** Accept-interfaces-at-consumer is stated convention; passing *Handler is same god-object coupling flagged for handler.go itself. Every future *Handler field is a hidden dep of this CREATE-context helper.
- **Fix:** Define small interface at call site naming exactly what's used: RangeOpenFiles, CloseFilesWithFilter, LeaseReleaser (ReleaseLeaseForHandle/UnregisterOplockFileID/SignalParkedCreates). *Handler already satisfies it.
- **Verified:** Confirmed at 907-912. Reachable from create.go:1450, create_post_break.go:1280, non-test. Coupling smell, no behavioral defect. LOW.

### [LOW] Same 6-field reconnect-identity tuple repeated as loose params in 3 signatures · `structure` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_context.go:464`
- **What:** processV1Reconnect (464-476) and processV2Reconnect (555-567) both take identical 11-param list (ctx, durableStore, metaSvc, contexts, dhnCCtx|dh2cCtx, sessionID, username, sessionKeyHash, shareName, filename, connClientGUID). ProcessDurableReconnectContext (349-360) carries same 6-tuple minus ctx-tag. validateAndRestore (701-712) narrower variant. lock.ReconnectIdentity{ShareName,Username,Filename,CheckPath} already exists, built inline at 719.
- **Why it matters:** Three near-identical long param lists for same "who is reconnecting" bundle. sessionKeyHash proven to be carried needlessly through all 4 — every future field add is a 3-4 site edit instead of 1-site.
- **Fix:** Extend lock.ReconnectIdentity (or handler-local struct) with SessionID/ClientGUID; pass once through ProcessDurableReconnectContext→processV1/V2Reconnect→validateAndRestore instead of 6 loose scalars per hop.
- **Verified:** Confirmed. Reachable via create.go:817. DRY nit, no defect. LOW.

### [LOW] Pervasive issue-number citations in comments violate repo's own comment convention · `structure` · area: handlers-security-sd
- **Where:** `internal/adapter/smb/handlers/security.go:140`
- **What:** 16+ bare issue refs embedded in behavioral comments: #1617 at 140,144,154,172,176,197,224,243,253,723,739; #1608 at 298,485,566; #1528 at 298,492; #1228 at 255,831. e.g. line 140: `// Directory SID Bridge (#1617)`.
- **Why it matters:** CLAUDE.md forbids issue/PR refs in source comments; `ponytail:` is sole sanctioned exception, none of these carry it. Worst offender file for this rule — dominant comment style for two whole sections (Directory SID Bridge, buildDACL grant-projection doc).
- **Fix:** Strip `(#NNNN)` parentheticals from all 16+ sites; surrounding prose already explains behavior without them.
- **Verified:** Confirmed by grep, all cited lines match. Convention violation not defect. LOW.

### [LOW] 19 one-line dispatch wrappers are pure pass-through boilerplate · `structure` · area: smb-conn-dispatch
- **Where:** `internal/adapter/smb/dispatch.go:150`
- **What:** dispatch.go:150-294 (~145 LOC): 9 funcs (handleNegotiate, handleSessionSetup, handleTreeConnect, handleTreeDisconnect, handleIoctl, handleCancel, handleChangeNotify, handleLock, handleOplockBreak) are literally `return h.X(ctx, body)`. Other 10 glue decode/handle/errorStatus into generic helpers.handleRequest pipeline.
- **Why it matters:** CommandHandler type is func(ctx, handler, body) — handler param prevents storing bound method value at init(), forcing wrapper. Pure indirection, 19 funcs to maintain in lockstep with Handler's method set.
- **Fix:** Flip CommandHandler param order to func(h *handlers.Handler, ctx, body). 9 pure ones become method expressions directly in table: `(*handlers.Handler).Negotiate` — no wrapper. 10 handleRequest-based move inline as closures. Net ~-100 to -145 LOC.
- **Verified:** Confirmed: 19 handle* funcs, exactly 9 pure one-line pass-throughs. Reachable via DispatchTable init(), invoked response.go:274,634. LOW — 145 LOC harmless glue.

### [LOW] Command.Name duplicates types.Command.String() verbatim, 19 strings kept in sync by hand · `structure` · area: smb-conn-dispatch
- **Where:** `internal/adapter/smb/dispatch.go:21`
- **What:** Command.Name field set to "NEGOTIATE".."OPLOCK_BREAK" (19 entries, init() 33-146) — identical to types.Command.String() (types/constants.go:99-142), same 19 labels. response.go reads cmd.Name for metrics/log at 6+ sites; reqHeader.Command already in scope at every one.
- **Why it matters:** Two hand-maintained tables mapping same 19 codes to same strings. Add 20th command, easy to update one and forget other — silent metrics-label drift, not compile error.
- **Fix:** Delete `Name string` field + 19 `Name: "X",` lines (~20 LOC). Change response.go from cmd.Name to reqHeader.Command.String().
- **Verified:** Confirmed string-for-string match. cmd.Name read at response.go:51,63(fallback already uses String()),266,269,276,288,304,627,630,636; reqHeader.Command in scope adjacent (261,622). Worst case metrics-label drift on 20th command, not functional bug. LOW.

### [LOW] FileID wire-offset table duplicated between request_fileid.go and channel_sequence.go · `structure` · area: smb-conn-dispatch
- **Where:** `internal/adapter/smb/channel_sequence.go:29`
- **What:** channelSeqFileID (29-44) hardcodes offset=16 for Read/Write/SetInfo, offset=8 for Ioctl. requestFileIDOffset (request_fileid.go:39-76) already has these exact 4 offsets (plus 6 more) computed from same MS-SMB2 struct layouts. Bounds check+copy (channel_sequence.go:38-43) also dup ExtractRequestFileID (request_fileid.go:23-34) verbatim.
- **Why it matters:** Same wire offsets independently transcribed twice. Future struct-layout fix has no forcing function to update both — silent drift risk for shared 4 commands.
- **Fix:** Replace channelSeqFileID body with switch filtering to 4 commands then calling ExtractRequestFileID(cmd, body): `switch cmd { case types.CommandRead, types.CommandWrite, types.CommandSetInfo, types.CommandIoctl: return ExtractRequestFileID(cmd, body); default: return fileID, false }`. ~10 LOC saved.
- **Verified:** Confirmed offsets match exactly. Reachable: channelSeqFileID called channel_sequence.go:61 from verifyChannelSequence, non-test. Silent drift risk only, no current mismatch. LOW.

### [MED] ReplaceCallback's failure return is ignored, letting a raced CANCEL/teardown produce a stale interim response and silently drop the rest of the compound · `bugs` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:196`
- **What:** async CREATE parks mid-compound; `ReplaceCallback` bool return discarded. Doc (pending_create_registry.go:250-262): false = entry already gone (raced CANCEL/teardown already sent final response). Code still sends interim STATUS_PENDING (210-217), MarkStarted unchecked (227-229), then `return` (230) — drops rest of compound with no response.
- **Why it matters:** double response for same MessageId; every chained command after CREATE (GETINFO/CLOSE/...) silently dropped, no response, client hangs to timeout.
- **Fix:** check ReplaceCallback bool. If false, skip interim + MarkStarted, log Debug, don't fall through.
- **Verified:** CONFIRMED. Real racer: stub_handlers.go:386-408 CANCEL does Unregister then invokes original callback w/ StatusCancelled in goroutine; handler.go:1919 UnregisterAllForSession is 2nd racer.

### [MED] Session re-auth mutates identity fields in place with zero synchronization while every in-flight request reads them unguarded · `bugs` · area: smb-session
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:221`
- **What:** reauthKerberosSession writes sess.User/Username/Domain/IsGuest/IsNull/ExpiresAt (221-228), tryReauthUpdate same (session_setup.go:2082-2086), no lock. Session doc (session/session.go:59-61) says read-only after creation except credit fields. Readers unlocked too: auth_helper.go:306-308, framing.go:455, response.go:469 IsExpired, handler.go:1432/2036-2041. request_order.go:26-29 confirms concurrent handler execution.
- **Why it matters:** torn/stale read of User/IsGuest during primeAuthContext can authorize wrong identity. Same bug class already fixed for CryptoState/NewlyCreated (audit #1132), missed here.
- **Fix:** lock these fields same as rest of Session (mu.Lock in writers + RLock/getters in readers), or fold into atomic.Pointer swap like cryptoState. Add race test mirroring TestSession_CryptoStateConcurrentReauthRace.
- **Verified:** CONFIRMED. Severity downgraded HIGH->MED: racing identities are same connection's own old/new identity, not cross-user escalation.

### [MED] Stop() races with wireIdentityResolver/wireForeignSIDResolver on identityUnsub/identityProviderUnsub/foreignSIDProviderUnsub · `bugs` · area: pkg-smb-adapter
- **Where:** `pkg/adapter/smb/adapter.go:910`
- **What:** Stop() reads/nils identityUnsub, identityProviderUnsub, foreignSIDProviderUnsub (910-923) with no s.resolverMu. wireIdentityResolver (762-796) and wireForeignSIDResolver (808-850) write same 3 fields under resolverMu.Lock(). netlogonProviderUnsub/netlogonAuth right below ARE correctly snapshotted under resolverMu with a comment about this exact hazard (924-935) — fix wasn't applied here.
- **Why it matters:** data race + possible double-close of unsub closure. Reachable via HTTP handler goroutine (identity_providers.go:221,382 -> NotifyIdentityProviderConfigChange -> runtime.go:1301 -> re-enters wire*Resolver) racing adapter Stop().
- **Fix:** snapshot+nil the 3 fields under resolverMu.Lock(), invoke closures outside lock — same pattern as netlogonProviderUnsub 12 lines below.
- **Verified:** CONFIRMED.

### [MED] Kerberos SESSION_SETUP bind checks identity by username only, not SID — bypasses the SID-authoritative match NTLM enforces · `security` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:396`
- **What:** completeKerberosBind: `if sess.User == nil || user.Username != sess.User.Username` — raw case-sensitive string compare. NTLM sibling completeSessionBind (session_setup.go:830) uses bindIdentityMatchesSession (784), SID-authoritative, with explicit anti-escalation comment. Kerberos users carry directory-resolved SID via synthUserFromResolved (554-574); completeKerberosBind never calls bindIdentityMatchesSession.
- **Why it matters:** SID-less/differently-provisioned account named identically to a domain principal can bind a channel onto the other's session, inherit its AuthContext/open-files/leases. Same class NTLM path was hardened against; Kerberos sibling missed.
- **Fix:** reuse bindIdentityMatchesSession(sess, user) in completeKerberosBind instead of inline Username compare.
- **Verified:** CONFIRMED at line 396. Severity dropped HIGH->MED: needs two same-named principals of different provenance (multi-realm/trust) — not trivially reachable.

### [MED] Kerberos SESSION_SETUP creates/mutates a live authenticated session before the SPNEGO mechListMIC downgrade check, never rolls back on MIC failure · `security` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:144`
- **What:** handleKerberosAuth: CreateSessionWithUserAndExpiry (144), SetPACIdentity, preauth hash init, configureSessionSigningWithKey (176) all run BEFORE buildKerberosAcceptResponse (188) does VerifyMechListMIC (290). MIC fail -> StatusLogonFailure but no DeleteSession; session stays live+keyed. reauthKerberosSession (221-231) mutates existing session's User/Domain/ExpiresAt/PAC before same MIC check, also unreverted on failure.
- **Why it matters:** rejected (LOGON_FAILURE) auth still leaves a fully signed live session, or silently reassigns existing session's identity. Sibling completeKerberosBind checks MIC (411) BEFORE AddChannel (448) with explicit comment; configureSessionSigningWithKey itself calls DeleteSession on setup failure (session_setup.go:1912/1921/1966). Fresh/reauth paths break that pattern.
- **Fix:** move MIC verification before session creation/mutation, mirroring completeKerberosBind. Or at minimum DeleteSession on fresh path / revert identity mutation on reauth path when MIC check fails.
- **Verified:** CONFIRMED. Severity MED not HIGH: surviving session carries ticket principal's OWN identity (no cross-user escalation); attacker holds no session key, matters only on non-signing dialect.

### [MED] TREE_DISCONNECT has no tree-ownership check — any session can teardown another session's tree connection · `security` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_disconnect.go:39`
- **What:** `_, ok := h.GetTree(ctx.TreeID)` — result discarded, no compare to ctx.SessionID before UnregisterAllForTree (68-82) + DeleteTree (88). Upstream gate response.go:514-520 also only checks existence. TreeID client-controlled, GenerateTreeID (handler.go:2070) bare atomic counter shared across sessions.
- **Why it matters:** any session can TREE_DISCONNECT a TreeID it never connected, deleting another session's tree mapping + cancelling victim's pending LOCKs, orphaning victim's open files. Sibling ownership checks exist in read.go:263, write.go:287, stub_handlers.go:510 — missing here.
- **Fix:** `tree, ok := h.GetTree(ctx.TreeID); if !ok || tree.SessionID != ctx.SessionID { return StatusNetworkNameDeleted }` before any close/cancel/delete.
- **Verified:** CONFIRMED. TreeConnection has SessionID field (handler.go:384), never compared.

### [MED] AppInstanceId failover force-closes ANY other user's open handle, on ANY share, with no identity or file-path check · `security` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:1450`
- **What:** Create() calls ProcessAppInstanceId whenever CREATE carries non-zero AppInstanceId + DurableStore set. Filter (durable_context.go:970): `f.AppInstanceId == appId` only, scanned across ALL sessions/shares via handler.files.Range (comment says "match across sessions"). GetDurableHandlesByAppInstanceId (997) not share-scoped either. Function never receives ShareName/path, no access check.
- **Why it matters:** MS-SMB2 3.3.5.9.13 requires match on AppInstanceId + PathName + Share + differing ClientGuid, THEN a maximal-access(GENERIC_READ) gate before close. All missing except AppInstanceId. Any authenticated user with create access anywhere can collide a 16-byte AppInstanceId and force-close a different user's file on a different share, releasing its locks/leases.
- **Fix:** thread TreeConnect share name + target path into ProcessAppInstanceId, filter on ShareName+Path+AppInstanceId, add maximal-access check via metadata service before closing.
- **Verified:** CONFIRMED per spec fetch. Severity HIGH->MED: 128-bit AppInstanceId must be observed on wire, not guessed.

### [MED] SET_INFO FileFullEaInformation never checks FILE_WRITE_EA on the open handle · `security` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:1774`
- **What:** FileInfoClass gate (221-234) exempts FileFullEaInformation from default hasAccessRight(FileWriteAttributes) check, comment claims "specific checks elsewhere" — but handler (1774-1816) only rejects reserved ACL xattr name then calls SetFileAttributes. Zero grep hits for WriteEa/FileWriteEa enforcement anywhere in package. Sibling classes (FileBasicInformation, FileEndOfFileInformation, FileDispositionInformation) all correctly gate on their access bit.
- **Why it matters:** MS-FSA 2.1.5.15.9 / MS-SMB2 3.3.5.21.1 require FILE_WRITE_EA on Open.GrantedAccess. metadata layer fallback (file_modify.go:609-615) only checks POSIX write for non-owner/non-root — owner bypasses even that. Owner opening with GENERIC_READ|FILE_WRITE_ATTRIBUTES only can still mutate EAs via SET_INFO.
- **Fix:** drop FileFullEaInformation from exemption list, add `if !hasAccessRight(openFile.GrantedAccess, types.FileWriteEa) { return StatusAccessDenied }`.
- **Verified:** CONFIRMED. Fix keeps set_info_ea_persist_test green (already grants FileWriteEA).

### [MED] AppInstanceId force-close has no share/path scoping and no access-rights gate — any authenticated user can hijack/kill another user's handle on any share · `security` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_context.go:907`
- **What:** ProcessAppInstanceId (907-1038): live-open filter `f.AppInstanceId == appId` (970), persisted-handle lookup GetDurableHandlesByAppInstanceId(997) — neither share/path scoped. Signature never receives ShareName/path. Call site create.go:1450-1452 passes authCtx only as ctx, never consulted.
- **Why it matters:** same defect as AppInstanceId finding above, verified independently at the callee. MS-SMB2 3.3.5.9.13's 4-condition match + maximal-access gate all absent.
- **Fix:** thread requesting TreeConnect + target path into ProcessAppInstanceId, add 4-condition match + maximal-access check.
- **Verified:** CONFIRMED — same defect as idx 3 (AppInstanceId create.go finding), duplicate root cause verified at callee. Severity MED: 128-bit value needs observation, not guessing.

### [MED] checkEncryptionRequired per-share encryption gate bypassable by sending TreeID=0 on NeedsTree commands · `security` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/response.go:514`
- **What:** prepareDispatch only resolves tree when `cmd.NeedsTree && reqHeader.TreeID != 0` (514) — TreeID=0 skips block silently, no rejection. checkEncryptionRequired (705) has identical `TreeID != 0` guard, so per-share EncryptData never consulted when TreeID=0.
- **Why it matters:** GetTree(0) would naturally return ok=false and reject on its own (tree IDs start above 0) — the extra `&& TreeID != 0` guard actively disables that. No test covers TreeID=0 for NeedsTree commands.
- **Fix:** drop `&& reqHeader.TreeID != 0` short-circuit; `if cmd.NeedsTree { tree, ok := GetTree(reqHeader.TreeID); if !ok { return StatusNetworkNameDeleted } }`. Add regression test for TreeID=0 on WRITE/READ + encrypted share.
- **Verified:** Gate bypass CONFIRMED, impact PARTLY REFUTED. read.go:263 / write.go:287 block via `openFile.TreeID != ctx.TreeID` check — bulk data paths safe. Still bypassable for CLOSE, SET_INFO, QUERY_INFO, FLUSH (resolve by FileID only, no tree/session cross-check) — cleartext metadata read/mutation on encryption-required share.

### [MED] Missing ownership check on traditional-oplock break-ack allows cross-session OpenFile.OplockLevel corruption · `security` · area: smb-lease-manager
- **Where:** `internal/adapter/smb/handlers/stub_handlers.go:986`
- **What:** handleOplockBreakAck resolves OpenFile via GetOpenFile(ack.FileID) (handler.go:1128, no session/tree filter), calls AcknowledgeLeaseBreak, on err==nil unconditionally writes openFile.OplockLevel = ack.OplockLevel (994). No VerifyLeaseAckOwnership call, unlike sibling handleLeaseBreakAck (921). AcknowledgeLeaseBreak returns nil even for a non-owned binding (lease/manager.go:449-454, treated as benign CLOSE-race).
- **Why it matters:** any session that observes/guesses another's FileID (monotonic counter, handler.go:2181) can fabricate an ack and corrupt victim's cached OplockLevel — same class handleLeaseBreakAck's own comment defends against.
- **Fix:** mirror line 921: `if !h.LeaseManager.VerifyLeaseAckOwnership(openFile.LeaseKey, ctx.SessionID, connClientGUID(ctx)) { return StatusInvalidOplockProtocol }` before AcknowledgeLeaseBreak.
- **Verified:** CONFIRMED. Severity cut HIGH->MED: only cached OplockLevel field corrupted (real lease record in lockMgr untouched); downstream impact is wrong durable-grant/lease-release gating, not data disclosure.

### [MED] NTLMv2 client-challenge AV_PAIRs validated structurally only — ChannelBindings, TargetName, MIC-present flag never extracted · `security` · area: smb-auth-ntlm-spnego
- **Where:** `internal/adapter/smb/auth/ntlm.go:886`
- **What:** validateNTLMv2ClientChallenge walks AV_PAIR list for wire well-formedness only (RespType, length, MsvAvEOL). Never reads MsvAvChannelBindings (0x000A), MsvAvTargetName (0x0009), or MIC-present bit in MsvAvFlags (0x0006). Zero grep hits for these across auth package.
- **Why it matters:** MS-NLMP 3.2.5.1.2: when MsvAvFlags bit 0x2 set, acceptor MUST verify AUTHENTICATE MIC. Samba does verify it — real deviation. No code verifies the Type-3 MIC at all.
- **Fix:** parse MsvAvFlags in validateNTLMv2ClientChallenge; when bit 0x2 set, verify 16-byte Type-3 MIC over NEGOTIATE||CHALLENGE||AUTHENTICATE (MIC zeroed), reject on mismatch.
- **Verified:** PARTLY CONFIRMED — MIC half real (Samba disagrees = genuine gap). ChannelBindings/TargetName halves REFUTED: SHOULD/optional per spec, Windows/Samba don't enforce for SMB either. Severity HIGH->MED: relay risk bounded by SigningConfig.Required/checkGuestPolicy.

### [MED] Share-permission resolution and access-mask business logic inlined in the protocol handler · `structure` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_connect.go:362`
- **What:** resolveSharePermission (362-434), calculateMaximalAccess (222-264), isRootUser (437-439), rootHasAdminAccess (447-457) — full share-ACL/squash/SID-grant precedence + MS-DTYP access-mask synthesis inlined in TreeConnect handler (calls at 61-217).
- **Why it matters:** violates CLAUDE.md invariant "protocol handlers handle only protocol concerns; permission checks belong in pkg/metadata". Root-squash bypass + local grant + additive AD/SID grant precedence is policy logic, not wire concern.
- **Fix:** move precedence logic (root-squash -> local -> SID, user-explicit wins) behind shared Runtime/identity ResolveShareAccess API; handler keeps only UNC parse + translate result to wire bytes.
- **Verified:** CONFIRMED as written. Two demotions HIGH->MED: (a) handler delegates grant lookup to userStore.ResolveSharePermission/ForSIDs — only precedence policy is inlined, not the whole decision engine; (b) claimed NFS-drift parallel `resolveNFSSharePermission` doesn't exist in tree (only appears as a word in a comment) — no live duplicate to drift against.

### [MED] 300-line durable-reconnect flow fully inlined in Create(), unlike its extracted siblings · `structure` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:811`
- **What:** DHnC/DH2C reconnect path (811-1112, ~300 LOC) anonymous nested block: session-key hash, ProcessDurableReconnectContext, OpenFile restore, lease-regrant (897-989) OR oplock-regrant (993-1050) sub-flows, response context build, two CreateResponse literals.
- **Why it matters:** same handled/not-handled shape already extracted for resolveCreateReplay (1567), handlePipeCreate (1779), handleOpenRootCreate (1849) — reconnect is largest/most stateful of the four yet only one left inline. Biggest lever for shrinking Create().
- **Fix:** extract to `func (h *Handler) resolveDurableReconnect(ctx, req, tree, sess, authCtx, fullFilename) (*CreateResponse, bool)`; split lease-regrant vs oplock-regrant into two further helpers (mutually exclusive branches).
- **Verified:** CONFIRMED, ~300 LOC as described. Severity MED not HIGH: subset of the create.go structure finding (same function, same fix direction), not independent.

### [MED] openFile.mu held across two metadata-store round trips in FileBasicInformation branch · `structure` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:404`
- **What:** openFile.mu.Lock() (404) held through metaSvc.GetFile (~508), lookupCaseInsensitive for ADS base (~502, 524), metaSvc.SetFileAttributes (485), second SetFileAttributes for ADS propagate (~549), Unlock (651). Comment (397-403) confirms deliberate serialization of SET_INFO/READ/WRITE/QUERY_INFO on same handle (#606).
- **Why it matters:** blocking backend round trips held under a lock gating other hot-path ops on same handle — stalls all concurrent ops on that handle for full round-trip latency, not just the in-memory mutation the lock should protect.
- **Fix:** snapshot freeze fields under lock, release before GetFile/lookupCaseInsensitive/both SetFileAttributes calls, re-acquire only to commit + re-check freeze generation before publish.
- **Verified:** CONFIRMED, one round trip worse than claimed (also covers the GetFile call). Demoted HIGH->MED: lock is per-handle not global/shared, so stall confined to that one handle; freeze-flag state machine genuinely needs pre-image read + write atomicity — narrowing is correctness-sensitive, not mechanical.

### [MED] completeNTLMAuth is a 307-line god function with 6+ responsibilities · `structure` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:1059`
- **What:** Lines 1059-1365: pending-auth lookup, TYPE_3 parse, anonymous branch, UserStore lookup, NTLMv2 domain-fallback validation loop (4-5 domain guesses), signing-key derivation, bind/reauth/fresh-session branching, NETLOGON fallback dispatch, guest fallback — all inline, 4+ nesting levels.
- **Why it matters:** SRP violation on security-critical TYPE_3 completion path; size/nesting hides which paths can/cannot fall back to guest, which must destroy session — missing return or misplaced if silently changes auth decision.
- **Fix:** Extract credentialed-user branch (1138-1277: NTLMv2 validate + derive key + bind/reauth/fresh dispatch) into h.completeNTLMAuthWithUser(ctx, pending, authMsg, user). Top-level becomes pure routing.
- **Verified:** CONFIRMED. Spans 1059-1388 (~307 LOC), all listed responsibilities inline (UserStore lookup 1128-1137, NETLOGON fallback ~1309-1318). Reachable: sole TYPE_3 path, called from SessionSetup:132. No concrete defect found; kept MED for auth-decision surface area.

### [MED] Kerberos AP-REP/mechListMIC response building duplicated between primary and bind paths, and the copies have silently drifted · `structure` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:461`
- **What:** buildKerberosAcceptResponse (248-328) is shared helper (AP-REP wrap + mechListMIC verify/compute + BuildAcceptCompleteWithMIC). completeKerberosBind (461-490) re-implements inline and diverged: primary Debug-logs BuildMutualAuth err (272-274) and ComputeMechListMIC err (306-311, falls back serverMIC=nil); on BuildAcceptCompleteWithMIC failure drops AP-REP (320, nil). Bind swallows both errs to `_` with no logging (465-467, 476), and on its own fallback *keeps* AP-REP (483) — opposite of primary.
- **Why it matters:** Comment at 461-462 claims bind "mirror[s] the primary Kerberos path" — it does not. Two copies of security-relevant response construction meant to be identical, silently diverged, less observable (no error logs).
- **Fix:** Extract `h.buildKerberosAPRepResponse(authResult, parsedToken) (spnegoResp []byte, err error)` and call from both handleKerberosAuth/reauthKerberosSession and completeKerberosBind; keep bind's earlier client-MIC verification ordering.
- **Verified:** Confirmed. buildKerberosAcceptResponse :248 (called :188, :242); completeKerberosBind :340 (called from session_setup.go:689) re-inlines at :462-484, drift as described, comment at :460-461 false. Not spec-violating (bind's behavior arguably better) — pure DRY/observability drift.

### [MED] completeCreateAfterBreak is a ~330-line function mixing 7+ unrelated concerns — SRP violation, and the file already knows how to split it · `structure` · area: handlers-create-post-break
- **Where:** `internal/adapter/smb/handlers/create_post_break.go:644`
- **What:** Inlines: share-mode/DACL/delete-on-close recheck dispatch, TOCTOU race-recovery resync, parent-dir-lease break, create/open/overwrite switch, SD_BUFFER apply (~822-857), EA_BUFFER apply (~865-893), GrantedAccess reconciliation, frozen-timestamp restore, ADS ctime bump, FileID generation, no-oplock conflict break, disconnected-handle purge, lease/oplock grant, durable-handle registration, CHANGE_NOTIFY emission, response-context assembly.
- **Why it matters:** Same file already extracted recheckExistingFileGates (516) for the race-recovery branch — proves author knows this should be decomposed by phase but only did the one piece that couldn't be duplicated inline. SD_BUFFER/EA_BUFFER/oplock-grant blocks are equally self-contained candidates.
- **Fix:** Extract applySDBufferContext(authCtx, tree, req, fileHandle, file) and applyEABufferContext(...) as named helpers, same shape as recheckExistingFileGates. Extract oplock/lease-grant block (Step 8b) into grantOplockOrLease(...). Prep work for the handlers/create subpackage split.
- **Verified:** Confirmed and UNDERSTATED: spans 644-1518 (next func parkCreateOnLeaseBreak) = ~870 lines, not ~330. recheckExistingFileGates at 516 confirms precedent. Reachable from Create()'s post-break path and resume goroutine. Real SRP violation vs CLAUDE.md "handlers handle only protocol concerns"; blocks the create/ subpackage split.

### [MED] Write() is a 417-line handler mixing protocol dispatch with inline business orchestration · `structure` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/write.go:169`
- **What:** One method (169-585) does: handle/tree/session validation, permission checks, lock-conflict check, lease-break, disconnected-handle purge, PrepareWrite/WriteAt/CommitWrite, write-through flush selection, frozen-timestamp restore, delayed-write arming, ADS name classification, atime bumps (file+parent) with frozen-restore, payload publish, change-notification dispatch.
- **Why it matters:** Violates repo invariant "protocol handlers handle only protocol concerns." Post-CommitWrite steps are pure bookkeeping already decomposed elsewhere for other handlers — inconsistent, leaves biggest chunk (ADS detection + notify dispatch, 508-565) inline.
- **Fix:** Extract postWriteBookkeeping(authCtx, openFile, name, preWriteMtime, writeOp) bundling steps 11b-12 (ADS classification, atime bumps, notify). Write's top-level body becomes validate → prepare → write → commit → respond.
- **Verified:** Confirmed: spans 169-585 (next func handlePipeWrite :588) = ~417 lines, all listed concerns inline (ADS :513-524, notify :545-565). Reachable: SMB2 WRITE dispatch. Correction: cited already-extracted helpers restoreFrozenTimestamps/updateBaseObjectTimestampsForADSWrite are NOT in this file (set_info.go:1922, create.go:2195) — strengthens the point.

### [MED] OpenFile.RequestedAllocSize / CreateOptions written with no lock, unlike every other field this file protects · `structure` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:1693`
- **What:** openFile.RequestedAllocSize = ... (1693, FileAllocationInformation) and openFile.CreateOptions = ... (1764, FileModeInformation) mutate with no openFile.mu anywhere in either branch. query_info.go:690/740/747/920 read RequestedAllocSize unlocked too.
- **Why it matters:** Every other OpenFile field this file mutates (BtimeFrozen/MtimeFrozen/CtimeFrozen/AtimeFrozen/DeletePending/SmbPendingAtime) is guarded by openFile.mu with #606 comments explaining this exact race. These two fields get none — data race under -race for concurrent SET_INFO + QUERY_INFO on same handle.
- **Fix:** Wrap both field writes in openFile.mu.Lock()/Unlock(); wrap query_info.go reads in RLock()/RUnlock(), same as freeze fields.
- **Verified:** CONFIRMED. :1693 and :1764 both unlocked while file locks elsewhere at 404/486, 809-811, 832-842, 1287-1290, 1337-1339, 1402-1404, 1488-1494. Unlocked readers: query_info.go:690/740/747/920, :1372, durable_context.go:1115. STRONGER than claimed: handler.go:417-432 documents CreateOptions as an "immutable field ... safe without the mutex" — set_info.go:1764 breaks that documented invariant outright; clients legitimately pipeline ops on one handle. Fix: lock both writes+reads, or drop CreateOptions from the immutable list.

### [MED] sessionKeyHash [32]byte threaded through 4 signatures, never read · `structure` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_context.go:356`
- **What:** ProcessDurableReconnectContext:356, processV1Reconnect:472, processV2Reconnect:563, validateAndRestore:708 all carry sessionKeyHash [32]byte param, passed down whole chain (365,374,549,676). validateAndRestore's own comment (744-749) says it's intentionally NOT compared. create.go:817 computes it via computeSessionKeyHash(sess) purely to feed this dead chain.
- **Why it matters:** Pure dead plumbing: crypto hash computed on every reconnect CREATE, threaded through 4 signatures for a value never consulted. Misleads readers into thinking session identity is re-verified on reconnect when it isn't.
- **Fix:** Drop sessionKeyHash from all 4 signatures and the create.go:817 call site. Move the NOTE comment up to ProcessDurableReconnectContext's doc.
- **Verified:** CONFIRMED. grep 'essionKeyHash' over non-test Go: zero reads inside validateAndRestore body. Reachable: create.go:811-823 feeds it from live CREATE path. buildPersistedDurableHandle (durable_context.go:1048) is a genuinely separate live use (writes handle.SessionKeyHash:1098, persisted) — correctly excluded from claim.

### [MED] Scavenger goroutine has a stop signal but no join · `structure` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_scavenger.go:52`
- **What:** DurableHandleScavenger.Run(ctx) (52-67) started via `go scavenger.Run(ctx)` at pkg/adapter/smb/adapter.go:548 inside Adapter.Serve(). ctx cancellation is stop signal (61-62) but Serve() returns immediately, no WaitGroup/completion channel.
- **Why it matters:** cleanupAndDelete (138-215) does metadata-store deletes, block-store flushes, delete-on-close RemoveFile/RemoveDirectory — shutdown has no way to know in-flight cleanup finished before tearing down Registry/stores.
- **Fix:** Adapter owns sync.WaitGroup, wg.Add(1) before `go scavenger.Run(ctx)`, wg.Done() on Run's return, wg.Wait() in Stop/shutdown before releasing Registry/stores.
- **Verified:** CONFIRMED. adapter.go:548 `go scavenger.Run(ctx)`; grep WaitGroup|wg\. in adapter.go: nothing. Run only selects ctx.Done() and returns, nobody observes it. Not in auxsvc group (only startEnabledDiscovery is). Real shutdown hazard.

### [MED] ProcessSingleRequest is a ~300-line god function mixing five unrelated concerns · `structure` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/response.go:79`
- **What:** ProcessSingleRequest (79-378) inlines: RED-metrics bookkeeping, credit-charge validation + sequence-window consumption (MS-SMB2 3.3.5.2.3/2.5), two security gates (encryption-required, channel-sequence), four per-command async-callback wiring blocks, handler dispatch, response ordering, response send, post-send/session housekeeping — flat scope, no named sub-steps.
- **Why it matters:** "Single funnel every request/response passes through" — size hides two security gates, and duplicates logic living in ProcessRequestWithFileIDAndCallback (symptom of not extracting named steps).
- **Fix:** Pull out validateCreditCharge(...), shared wireAsyncCallbacks(...), and the create/lock resume-signal block (351-355) into named functions.
- **Verified:** CONFIRMED. Spans 79-385 (next func isExpiryExemptCommand :386) = ~300 lines. All concerns verified: RED defer :101-111, credit/sequence gate :121ff, checkEncryptionRequired :197ff, channel-sequence gate :204ff, four async callback blocks :231/243/255+CHANGE_NOTIFY. Duplication confirmed: identical wiring re-appears in ProcessRequestWithFileIDAndCallback :597/607/616. Reachable: connection.go read loop.

### [MED] Sign-then-splice-signature sequence reimplemented three times instead of routed through sendMessage · `structure` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/response.go:1145`
- **What:** `sess.SignMessageOnChannel(connInfo.ConnID, smbPayload); copy(hdr.Signature[:], smbPayload[48:64])` appears 3x: sendMessage (1138-1149, documented as "internal implementation used by SendMessage and SendResponseWithHooks"), SendSignatureFailureResponse (978-1005), sendMessageWithSigner (1007-1026). Each independently builds `append(hdr.Encode(), body...)` and calls WriteNetBIOSFrame directly.
- **Why it matters:** sendMessage's own doc says it's the one place signing lives, yet two more paths reimplement sign-and-splice (incl. magic byte range 48:64). A future signing change (e.g. GMAC) must be found and updated in 3 places.
- **Fix:** Extract `signAndSplice(sess, connID, hdr, payload)` used by all 3 sites, or parameterize sendMessage with explicit signingSessionID so sendMessageWithSigner collapses into a call to sendMessage.
- **Verified:** CONFIRMED, 3 sites verbatim: response.go:1138-1145, :984-986, :1017-1019. sendMessage's doc :1048-1051 claims to be the internal implementation. Reachable: SendSignatureFailureResponse from connection.go:298; sendMessageWithSigner from SendErrorResponse :891,897 (USER_SESSION_DELETED path).

### [MED] HandleSMB1Negotiate hand-encodes SMB2 wire bytes instead of using header.SMB2Header.Encode() · `structure` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/response.go:1387`
- **What:** HandleSMB1Negotiate (1351-1420) builds SMB2 NEGOTIATE response via binary.LittleEndian.PutUint16/32/64 at literal offsets into two manually-sized slices (make([]byte, header.HeaderSize) and make([]byte, 65)), duplicating the layout header.SMB2Header.Encode() already owns and every other response path uses (buildResponseHeaderAndBody, sendMessage, SendSignatureFailureResponse).
- **Why it matters:** Wrong-layer duplication with magic offsets (header: 0,4,8,12,14,16; body: 0,2,4,8,28,32,36,40,48,56). If header layout changes, this drifts silently. Also pre-auth attack surface (SMB1 bootstrap) — riskiest place for hand-rolled encoding.
- **Fix:** Construct `header.SMB2Header{...}.Encode()` for the response header; factor NEGOTIATE body construction into a shared encoder reused by the real SMB2 NEGOTIATE handler (handlers/negotiate.go).
- **Verified:** CONFIRMED. response.go:1351-1420 pokes literal offsets as described; header.SMB2Header.Encode() owns that layout elsewhere. Reachable: connection.go:239 `return smb.HandleSMB1Negotiate(...)` — pre-auth SMB1 bootstrap (macOS Finder). Behavior itself is spec-correct (0x02FF/0x0202 per MS-SMB2 3.3.5.3.1/2); only the hand-encoding is wrong-layer.

### [MED] ProcessCompoundRequest is a ~280-line god function mixing 6 responsibilities · `structure` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:49`
- **What:** Spans 49-330: related-flag validation + error-chain construction (58-88), credit-charge validation (90-107), sequence-window consumption for first command (108-120) and all trailing commands (122-152), first-command dispatch, ~55-line inline closure special-casing STATUS_PENDING async CREATE (178-231), FileID/session/tree tracking (244-265), response accumulation (267-284), delegation to processRemaining (287), send + gate-release + post-send hooks (289-311).
- **Why it matters:** God-function per package's own stated idiom ceiling. Nine concerns in one body makes the async-CREATE interim path (trickiest ordering-sensitive code) hard to reason about apart from credit accounting.
- **Fix:** Split into validateCompoundEnvelope(...) (related-flag + credit-charge + both sequence-window loops), dispatchFirstCommandAsync(...) for the async-CREATE-interim closure as its own named function; leave ProcessCompoundRequest as ~30-line driver.
- **Verified:** CONFIRMED. Spans 49→331 (next func sendCompoundResponses) = ~280 lines. All concerns verified in body. Reachable: connection.go:437.

### [MED] Same 'walk compound chain via ParseCompoundCommand' loop hand-copied 3x, double-parses every subcommand on the hot path · `bloat` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:130`
- **What:** Three loops reimplement identical skeleton `rem := compoundData; for len(rem) >= header.HeaderSize { hdr,_,nextRem,err := ParseCompoundCommand(rem); if err != nil {break}; rem = nextRem; ... }`: related-flag error builder (69-82), trailing-sequence-window loop (130-152), processRemaining's own loop (851+, called from 287). Credit loop runs whenever connInfo.SequenceWindow set, then processRemaining immediately re-parses the same bytes — every subcommand header ParseCompoundCommand'd twice.
- **Why it matters:** Duplicated chain-walking logic could collapse behind one shared walker; independent copies risk disagreement on how NextCommand==0/misaligned/self-referential offsets are handled even though all delegate parsing to the same func.
- **Fix:** Extract `walkCompoundChain(data []byte, visit func(hdr *header.SMB2Header, body []byte) bool)`. Rewrite 3 call sites as one-liners. Optionally build the []*header.SMB2Header slice once in the credit pass and hand to processRemaining, eliminating the double-parse.
- **Verified:** CONFIRMED, count UNDERSTATED — 4 copies: :69-82, :130-152, :482 (failEntireCompound), :857 (processRemaining). Double-parse confirmed: :130-152 walks all compoundData when SequenceWindow != nil, then processRemaining re-walks same bytes via call at :287. Reachable: connection.go:437.

### [MED] compoundLoopState.processRemaining is a second ~240-line god function nested in the same file · `structure` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:844`
- **What:** processRemaining (844-1082) per iteration: parse next command, resolve related-command sentinel IDs (881-888), verify sub-signature (894-901), propagate session-level failure (918-928), propagate FileID-failure (930-945), gate non-last CHANGE_NOTIFY (947-968ish), dispatch via ProcessRequestWithInheritedFileID/ProcessRequestWithFileIDAndCallback (980-1006), track handler-assigned session/tree IDs (1008-1029), track async-pending state for tail commands (1039-1042), build+append response (1053-1067) — 9 concerns in one loop body.
- **Why it matters:** Same god-function idiom violation as ProcessCompoundRequest; file's own designated source of truth for sync AND async-resume compound paths, so size/branch count gates how safely completeCompoundAfterAsyncCreate can change without drift.
- **Fix:** Extract resolveRelatedIDs(hdr), propagatedFailureResponse(hdr) (*compoundResponse, bool) (replacing the two near-identical early-continue blocks), dispatchSubcommand(ctx, hdr, cmdBody, subRaw, connInfo, isEncrypted, asyncNotifyCallback). Loop becomes: parse → resolveRelatedIDs → verify sig → propagatedFailureResponse → CHANGE_NOTIFY gate → dispatchSubcommand → append.
- **Verified:** CONFIRMED. Spans 844→1083 (next func completeCompoundAfterAsyncCreate), ~240 lines, single loop body. All per-iteration concerns verified including ParseCompoundCommand :857, VerifyCompoundCommandSignature :895. Reachable from ProcessCompoundRequest:287 and completeCompoundAfterAsyncCreate (async-resume).

### [MED] Session.Credits and CommandSequenceWindow are two independent credit ledgers that silently diverge on async completions · `structure` · area: smb-session
- **Where:** `internal/adapter/smb/session/session.go:12`
- **What:** Package doc claims it "eliminates the dual ownership problem" and provides "a single source of truth", but two ledgers remain: Session.credits (Granted/Consumed/Outstanding/HighWaterMark, via Manager.GrantCredits→Session.GrantCredits) and CommandSequenceWindow.available (via Consume/Grant/Reclaim). Sync path composes both correctly (response.go:846 GrantCredits then :857 SequenceWindow.Grant). But response.go:1207-1210 (SendAsyncChangeNotifyResponse) and :1278-1281 (SendAsyncCompletionResponse) call `connInfo.SequenceWindow.Grant(1)` directly, never SessionManager.GrantCredits/Session.GrantCredits.
- **Why it matters:** Every async CHANGE_NOTIFY/compound completion advances the wire-advertised credit window but leaves Session.credits untouched. grantAdaptive (manager.go:264) uses GetOutstanding() to throttle; GetStats() reports these — both permanently under-counted for sessions using CHANGE_NOTIFY/async compounds, defeating the adaptive-throttle input and stats surface.
- **Fix:** Route async completions through the same Manager.GrantCredits/Session.GrantCredits call the sync path uses (or add a Manager-level method updating both atomically) instead of reaching into connInfo.SequenceWindow.Grant directly from response.go.
- **Verified:** CONFIRMED. Sync: grantConnectionCredits response.go:846 → session.ConsumeCredits manager.go:179 + session.GrantCredits :202, then :857 SequenceWindow.Grant. Async paths :1207-1210 and :1278-1281 skip the first half entirely. Session.GrantCredits session.go:381-393 is sole writer of credits fields. grantAdaptive manager.go:264 uses GetOutstanding(); GetStats session.go:416-427 reports them. Reachable: SendAsyncCompletionResponse called from response.go:231/243/255 and :597/607/616.

### [MED] tryNetlogonFallback and tryNetlogonBind duplicate the entire NETLOGON DC exchange + identity resolve + key derivation · `bloat` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:1388`
- **What:** tryNetlogonFallback (1388-1463) and tryNetlogonBind (1490-1547) both build identical netlogon.NetworkLogonRequest from authMsg/pending, handle same err/nil-result fail-closed cases with near-identical logs, call resolveNetlogonIdentity + synthUserFromResolved, derive signing key via identical auth.DeriveSigningKey call. Diverge only at terminal step: CreateSessionWithUser+configureSessionSigningWithKey vs completeSessionBind.
- **Why it matters:** ~40 lines of near-identical DC-exchange/resolve/derive logic in two places; a bugfix (e.g. domain-fallback list or DeriveSigningKey call) has to be made twice.
- **Fix:** Extract `h.netlogonAuthenticate(ctx, pending, authMsg) (user *models.User, signingKey []byte, ok bool)` containing DC call + resolve + key derivation; both callers keep only their distinct terminal step.
- **Verified:** CONFIRMED. :1388 and :1490 share ~35 lines verbatim-modulo-log-strings: same nil-check, same 5-field NetworkLogonRequest literal, same err/nil fail-closed pair, same resolveNetlogonIdentity + Found gate, same synthUserFromResolved, same 3-arg DeriveSigningKey call, same ctx.IsGuest=false. Diverge only at terminal step. Both reachable from handleSessionSetup's NTLM completion.

### [HIGH] TREE_DISCONNECT does not verify the TreeID belongs to the requesting session — cross-session tree hijack/DoS · `bugs` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_disconnect.go:39`
- **What:** `_, ok := h.GetTree(ctx.TreeID)` checks existence only, never `tree.SessionID == ctx.SessionID`. TreeIDs come from one global sequential atomic (`h.nextTreeID.Add(1)`), stored in one global `sync.Map`, guessable across sessions. `DeleteTree` + `UnregisterAllForTree` run unconditionally on the guessed TreeID. `CloseAllFilesForTree(ctx.Context, ctx.TreeID, ctx.SessionID)` passes attacker's SessionID, so victim's opens (filtered on SessionID too) never close — tree deleted, opens/locks/leases orphaned. Same gap in `prepareDispatch` NeedsTree gate (response.go:514-520) and create.go:513.
- **Why it matters:** Any authenticated session can tear down/leak resources of an unrelated session via a predictable int ID. Spec violation: MS-SMB2 3.3.5.2.11 requires TreeConnect located in Session.TreeConnectTable before acting.
- **Fix:** In prepareDispatch's NeedsTree gate, reject when `tree.SessionID != reqHeader.SessionID` (STATUS_NETWORK_NAME_DELETED) — covers TREE_DISCONNECT, CREATE, and every tree-scoped command in one place.
- **Verified:** CONFIRMED. tree.SessionID exists and is used elsewhere (handler.go:1758) but not here or in the shared gate; create.go:513 has the identical gap. READ/WRITE are safe only because they separately check openFile.SessionID.

### [HIGH] CANCEL cancels another session's parked CREATE — no ownership check on AsyncId · `security` · area: handlers-create-post-break
- **Where:** `internal/adapter/smb/handlers/pending_create_registry.go:222`
- **What:** `UnregisterByAsyncId(asyncId)` keyed on client-supplied AsyncId alone, no compare against stored `ConnID`/`SessionID` (present on `PendingCreate`, lines 24-27). `generateAsyncId` = `h.nextAsyncId.Add(1)`, one process-wide sequential counter — guessable. Same unchecked pattern in PendingLockRegistry, PipeReadRegistry, NotifyRegistry `UnregisterByAsyncId`. `UnregisterByMessageID` correctly scopes by `{ConnID, MessageID}` — only the AsyncId path skips the check.
- **Why it matters:** Any client can CANCEL another client's in-flight CREATE/LOCK/NOTIFY/pipe-read purely by counting up AsyncId — cross-session DoS. MS-SMB2 3.3.5.16 scopes async-cancel to the connection's own AsyncCommandList; response.go:455 even exempts CommandCancel from the session/expiry/channel gate, so no valid session is required.
- **Fix:** Require caller's `ConnID`/`SessionID` in `UnregisterByAsyncId` (and LOCK/NOTIFY/pipe-read equivalents), reject unless it matches the stored entry, under `r.reg.mu`. Thread `ctx.ConnID`/`ctx.SessionID` from `Handler.Cancel`.
- **Verified:** CONFIRMED. Same gap in all four registries; unauthenticated reachability confirmed via response.go:455 CommandCancel exemption. Availability-only impact but cheap and unauthenticated.

### [HIGH] Handler is THE god object: 36-field struct + 2925 LOC, six-plus unrelated responsibilities · `structure` · area: handlers-core-handler
- **Where:** `internal/adapter/smb/handlers/handler.go:36`
- **What:** `type Handler struct` (56 fields, file is 2925 LOC) carries: registries (pendingAuth/trees/files + nextTreeID/nextFileID/handleOps), handle refcounting, teardown (CloseAllFilesForSession/closeFilesWithFilter), delete-on-close + block-store purge, durable-handle bookkeeping, share-mode/rename conflict resolution, signing/encryption/dialect config, Kerberos/NTLM identity config, and the srvsvc share+SD cache. Every handler in the 34k-LOC package reaches through this one receiver.
- **Why it matters:** Violates single-responsibility at the package root type; the whole package can't be split without dragging config nothing touches. No test can exercise share-mode logic without wiring Kerberos + signing + pipe registries too.
- **Fix:** Split into cohesive collaborators Handler composes: `openreg` (registries + refcounting), `teardown` (session/tree cleanup, delete-on-close, purge), `sharemode` (conflict checks), `durable` (durable-handle state), `pipesrv` (srvsvc cache). Handler keeps wire/identity config + references to these five.
- **Verified:** CONFIRMED, understated: struct carries 56 fields not 36 (36 is the struct's start line). *Handler is receiver of every dispatch.go entry — fully reachable, not test-only.

### [HIGH] Create() is a ~1015-line god function doing 10+ unrelated jobs · `structure` · area: handlers-create
- **Where:** `internal/adapter/smb/handlers/create.go:499`
- **What:** `func (h *Handler) Create` runs 499-1514 (~1029 LOC, 45% of file). One body: tree/session lookup, durable-context validation, TWrp rejection, replay dispatch, impersonation check, CreateOptions/FileAttributes bit checks, IPC$ pipe delegation, AuthContext build, path normalization, ADS stream-suffix parse (~40 lines), full durable-handle reconnect flow (~300 lines), parent-DACL gate, disposition resolution, ADS auto-create, overwrite checks, write-perm check, share-mode conflict, delete-pending, AppInstanceId failover, draft-build + break dispatch.
- **Why it matters:** Each concern has its own MS-SMB2 subsection/failure mode; none independently unit-testable. A change to one create-context risks touching unrelated logic 800 lines away. File already has the right decomposition pattern (handlePipeCreate, handleOpenRootCreate, resolveCreateReplay already separate) — Create() itself wasn't split the same way.
- **Fix:** Extract `validateCreateRequestFields`, the reconnect block, `parseStreamSuffix`, `checkOverwriteConstraints` as private steps returning early. Target: Create() under ~150 LOC of pure dispatch.
- **Verified:** CONFIRMED. Body runs to next func at :1529; sibling extracted methods (handlePipeCreate:1779, handleOpenRootCreate:1849, resolveCreateReplay:1567, walkPath:2002, createNewFile:2058, overwriteFile:2120) prove the pattern is already established elsewhere in-file.

### [HIGH] setFileInfoFromStore is a ~1,540-line single function/switch mixing wire decode + full business logic · `structure` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:284`
- **What:** One func (284-1820), one switch, 8 of 9 FileInfoClass branches inlined: FileBasicInformation freeze/thaw (~250 lines), FileRenameInformation incl. stream-rename + share-mode conflict scan + lease-break orchestration + ADS propagation + notify (~670 lines), FileDispositionInformation, FileEndOfFileInformation, FileAllocationInformation, FileModeInformation, FileFullEaInformation all inline. Only FileLinkInformation delegated (`h.handleFileLinkInformation`, 2530).
- **Why it matters:** Violates "protocol handlers handle only protocol concerns" invariant. One function this size can't be unit-tested per-branch or safely edited without touching unrelated classes.
- **Fix:** Split into one method per FileInfoClass (setBasicInfo, setRenameInfo, setDispositionInfo, setEOFInfo, setAllocationInfo, setModeInfo, setEaInfo) matching handleFileLinkInformation's pattern; split file into set_info_timestamps.go, set_info_rename.go, set_info_disposition.go, set_info_size.go, set_info_security.go.
- **Verified:** CONFIRMED. Body runs to next func at :1832 (~1548 LOC). SetInfo (:195) is dispatch target. BasicInformation + RenameInformation alone are ~920 of the 1548 lines — extract those first.

### [HIGH] TREE_DISCONNECT (and every NeedsTree op) has no tree-ownership check — cross-session tree hijack/DoS · `gaps` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_disconnect.go:39`
- **What:** Same root cause as above, framed as a spec-conformance gap: `_, ok := h.GetTree(ctx.TreeID)` proceeds to cancel PendingLockRegistry waiters and unconditionally `DeleteTree(ctx.TreeID)` — never checks `tree.SessionID == ctx.SessionID`. `h.trees` is one global `sync.Map`; TreeIDs minted by one global sequential `atomic.Uint32` (`h.nextTreeID.Add(1)`, starting at 1) — small, sequential, shared across all clients. Same missing check in shared dispatch gate `prepareDispatch` (response.go:514-520) and compound.go's equivalent path. `CloseAllFilesForTree(ctx.Context, ctx.TreeID, ctx.SessionID)` passes attacker's own SessionID, so filter `f.TreeID==treeID && f.SessionID==sessionID` matches none of victim's opens — but `DeleteTree` still removes victim's TreeConnection. Victim's subsequent requests on that TreeID fail with STATUS_NETWORK_NAME_DELETED; files/locks orphaned with no owning tree. `PendingLockRegistry.UnregisterAllForTree(ctx.TreeID)` also scoped by TreeID alone — attacker force-completes victim's parked LOCK waiters cross-session.
- **Why it matters:** MS-SMB2 §3.3.5.2.11 scopes TreeConnect objects per-Session (Session.TreeConnectTable); server must verify TreeId belongs to the Session on the request. Any authenticated session (guest included) can enumerate small TreeIDs and disconnect/orphan another tenant's tree — no share permission of any kind needed on the victim's share.
- **Fix:** Add `tree.SessionID != ctx.SessionID` check in TreeDisconnect (return STATUS_NETWORK_NAME_DELETED to avoid leaking existence) — but root-cause fix is one check in the shared NeedsTree gate at response.go:514-520 so every NeedsTree handler (create.go:513, query_directory.go:1122, etc.) is protected at once.
- **Verified:** CONFIRMED + spec-violating. Only 4 session-ownership checks exist repo-wide, all on OpenFile (read.go:263, write.go:287, stub_handlers.go:510) — none on trees. MS-SMB2 3.3.5.2.11 fetched text: "If no tree connect found ... MUST be failed with STATUS_NETWORK_NAME_DELETED" via lookup in Session.TreeConnectTable by TreeId. Fix: check tree.SessionID == ctx.SessionID in prepareDispatch's NeedsTree gate (covers compound too) + TreeDisconnect.

### [HIGH] Anonymous/guest bypass in checkEncryptionRequired defeats per-share SMB2_SHAREFLAG_ENCRYPT_DATA · `gaps` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/response.go:686`
- **What:** `checkEncryptionRequired` returns 0 (allowed) unconditionally for `sess.IsNull || sess.IsGuest` (686-692), BEFORE the per-share `tree.EncryptData` check (705-712). Code comment cites "3.3.5.2.9: anonymous/null sessions bypass encryption requirements" — fabricated citation. Async completion paths (`SendAsyncChangeNotifyResponse`/`SendAsyncCompletionResponse`, 1211-1219/1282-1291) never set `hdr.TreeID`, so `sendMessage`'s tree.EncryptData fallback can't fire there either — relies entirely on sticky `PeerUsedEncryption`, false for a guest/null session that skipped encryption initially.
- **Why it matters:** MS-SMB2 3.3.5.2.11 requires STATUS_ACCESS_DENIED when `TreeConnect.Share.EncryptData==TRUE` and request unencrypted — no anonymous/guest carve-out in spec (anonymous exemptions exist only for SIGNING, 3.3.5.5.3). `tree_connect.go:316-321` gates rejection on `encryptionMode=='required'` only, so in default/'preferred' mode a guest/null session tree-connects to an EncryptData=true share and does plaintext I/O.
- **Fix:** Reorder so per-share/global-required checks run independent of IsNull/IsGuest — return StatusAccessDenied for null/guest against an encryption-required share rather than exempting them.
- **Verified:** CONFIRMED, spec-violating (citation fabricated). Reachability confirmed via tree_connect.go:316-321 default-mode gap.

### [HIGH] Fresh SESSION_SETUP with an unclaimed nonzero SessionId keeps the client-supplied ID instead of always minting a server-generated one · `bugs` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:976`
- **What:** `sessionID := ctx.SessionID; if sessionID == 0 { ... GenerateSessionID() } else if _, ok := h.GetSession(sessionID); ok { isReauth = true }`. Nonzero SessionID that misses GetSession falls through with the client-chosen value kept and isReauth=false. Top-level dispatcher only rejects on a hit. `GenerateSessionID` = `m.nextSessionID.Add(1)`, sequential, never advanced past a squatted ID. `m.sessions.Store(sessionID, session)` is a blind overwrite.
- **Why it matters:** A client can squat an unassigned nonzero SessionID; when the sequential counter later reaches the same value for an unrelated legit client, `Store` silently replaces the entry — session-fixation/ID-collision, violates MS-SMB2 3.3.5.5 (server owns SessionId allocation).
- **Fix:** Only honor ctx.SessionID as existing when GetSession hits; on miss always call `GenerateSessionID()` regardless of the client-supplied value. Consider STATUS_USER_SESSION_DELETED at dispatcher for nonzero non-binding SessionID that doesn't resolve.
- **Verified:** CONFIRMED but downgraded from HIGH claim rationale: dialect>=3.0 NeedsSession origin/bound-channel gate blunts identity takeover (needs 2.0.2/2.1 connection). Spec violation and ID-collision overwrite are real regardless.

### [MED] tryReauthUpdate mutates live Session identity fields with no lock while other channels/goroutines read them · `bugs` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:2082`
- **What:** `existingSess.Username = username; ...Domain...; ...User...; ...IsGuest...; ...IsNull...` direct unlocked writes on shared `*session.Session`. session.go:60-62 documents these fields read-only after creation except credit fields. Unsynchronized readers: `primeAuthContext` (auth_helper.go:306-308) on every CREATE/READ/WRITE/QUERY_DIRECTORY, `bindIdentityMatchesSession` (785-794) during concurrent bind.
- **Why it matters:** SMB3 multichannel means re-auth on one channel races normal request processing on another. Multi-field update not atomic — torn reads possible (new IsGuest with old User pointer). Real Go data race. auth_helper.go:236 already acknowledges the hazard but only works around it for the identity-cache path.
- **Fix:** Protect Username/Domain/User/IsGuest/IsNull with existing `session.mu` or add setter/getter methods analogous to SetPACIdentity/CachedAuthIdentity; update tryReauthUpdate and raw readers to go through them.
- **Verified:** CONFIRMED. Reachable from session_setup.go:1116/:1247. kerberos_auth.go:221-232 (reauthKerberosSession) has the identical unlocked multi-field write. PACIdentity already uses a lock — these five fields are the odd ones out.

### [MED] SPNEGO mechListMIC downgrade-protection check runs after session state is created/mutated, not before · `bugs` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:289`
- **What:** `buildKerberosAcceptResponse` verifies mechListMIC at 289-296, but fresh-session path already ran `CreateSessionWithUserAndExpiry` → `StoreSession` (live+lookupable) and `configureSessionSigningWithKey` (keys derived) before this call. Reauth path (`reauthKerberosSession`, 221-232) overwrites live session's User/Username/Domain/ExpiresAt/PAC SIDs before the MIC check, keeping the OLD signing key. `completeKerberosBind` (340-491) does it correctly — verifies MIC (410-415) before `AddChannel` (448), with a comment stating the ordering requirement.
- **Why it matters:** mechListMIC is RFC 4178 downgrade-protection; failing it must reject the negotiation, not leave state committed. Fresh path: fully-keyed zombie session survives (caller never calls DeleteSession) until connection close. Reauth path: client told LOGON_FAILURE but session now authorizes as the new principal under the OLD signing key.
- **Fix:** Move MIC verification before `CreateSessionWithUserAndExpiry` in handleKerberosAuth, and before the `sess.*` overwrites in `reauthKerberosSession`, mirroring completeKerberosBind's ordering.
- **Verified:** CONFIRMED. completeKerberosBind precedent is in-file. Severity MED not HIGH: reauth needs client's own origin/bound connection + valid AP-REQ; fresh-path zombie unusable without the derived keys.

### [MED] TOCTOU create-race recovery truncates winner's file without breaking its lease/oplock · `bugs` · area: handlers-create-post-break
- **Where:** `internal/adapter/smb/handlers/create_post_break.go:798`
- **What:** ErrAlreadyExists race-recovery branch (746-807) resyncs to the winner and replays `recheckExistingFileGates` (share-mode + DACL only, never lease/oplock), then calls `h.overwriteFile` (truncate via SetFileAttributes, no lease interaction). Normal path always runs `breakAndMaybeParkCreate` (BreakReasonDestructive) BEFORE truncating specifically to break existing holders — race-recovery re-enters mid-flow and skips that step for the newly-discovered winner.
- **Why it matters:** Winner can hold Batch/Handle lease independent of ShareAccess permissiveness (passes recheck). Loser silently truncates winner's content with no LEASE_BREAK — cache-coherency/data-integrity violation vs MS-SMB2 3.3.5.9 / Samba delay_for_oplock_fn. This is the #765 TOCTOU window, only half the gate (share-mode/DACL) was replayed.
- **Fix:** Before `overwriteFile` in the race-recovery branch, drive `d.existingHandle` through the same break dispatch as CREATE entry — call `h.breakAndMaybeParkCreate(ctx, d)` and only then proceed.
- **Verified:** CONFIRMED. Severity MED not HIGH: only reachable when winner file created in the same instant, only for OVERWRITE_IF/SUPERSEDE, winner has microseconds to acquire a lease.

### [MED] OpenFile.PositionInfo mutated without openFile.mu — data race on concurrent READ/QUERY_INFO/SET_INFO · `bugs` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/read.go:145`
- **What:** `recordReadProgress` does `open.PositionInfo = offset + bytesReturned` with no lock, called from Read() (368, 436) and handleSymlinkRead (535). Same field written unlocked at set_info.go:1652 (SET_INFO FilePositionInformation), read unlocked at query_info.go:754/944 (QUERY_INFO).
- **Why it matters:** SMB clients pipeline ops on the same handle (openfile_concurrency_test.go exists exactly to guard this — enumeration cursor, timestamp overlay, freeze/thaw flags all guarded by mu). PositionInfo is a plain uint64 with no mutex/atomic, unlike every other cross-goroutine-mutable OpenFile field. Real -race hit, torn/stale CurrentByteOffset that FilePositionInformation round-trip tests assert on.
- **Fix:** Guard PositionInfo like the other mutable fields: `openFile.mu.Lock()` in recordReadProgress and the SET_INFO write site, `RLock()` in the two QUERY_INFO read sites. Or switch to `atomic.Uint64`.
- **Verified:** CONFIRMED. openFile.mu exists and guards neighboring fields (query_info.go:646-659, set_info.go:404/1402) — PositionInfo is the odd field out.

### [MED] SET_INFO FileEndOfFileInformation has no FILE_WRITE_DATA gate — falls through to POSIX/ACL ownership check instead of the handle's frozen GrantedAccess · `bugs` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:1531`
- **What:** FileEndOfFileInformation case never calls `hasAccessRight(openFile.GrantedAccess, FileWriteData)`. Only sets `authCtx.WriteAuthorizedByHandle = hasWriteAccess(...)`; when false, `pkg/metadata/auth_permissions.go:300` falls through to `calculatePermissions` (ordinary POSIX/ACL check) instead of denying. A handle opened without FILE_WRITE_DATA can still truncate/extend if the identity's POSIX mode/ACL happens to allow write. Step 1b (221-235) explicitly exempts this class assuming it self-gates — false for EndOfFile. The very next case, FileAllocationInformation (1664), does the correct check.
- **Why it matters:** MS-FSA 2.1.5.15.5 requires STATUS_ACCESS_DENIED when Open.GrantedAccess lacks FILE_WRITE_DATA — same rule this file already implements one case later for FileAllocationInformation (2.1.5.15.1). set_info_truncate_reclaim_test.go only exercises a handle that already carries FileWriteData — path untested.
- **Fix:** Add `if !hasAccessRight(openFile.GrantedAccess, uint32(types.FileWriteData)) { return setInfoStatus(types.StatusAccessDenied), nil }` at top of FileEndOfFileInformation case, copying the FileAllocationInformation guard.
- **Verified:** CONFIRMED. Severity MED not HIGH: handle-scope violation not privilege escalation — a DACL that denies write also denies it in calculatePermissions; only bites when client asked for less access than identity actually holds (plus MaximumAllowed/GenericWrite widening).

### [MED] Unsynchronized read of the mutable OpenFile.MetadataHandle field during delete-on-close election · `bugs` · area: handlers-lease-oplock
- **Where:** `internal/adapter/smb/handlers/doc_election.go:142`
- **What:** `electDeleteOnClose` reads `openFile.MetadataHandle` (136) and `other.MetadataHandle` (142, `bytes.Equal`) with no lock. handler.go:431 lists MetadataHandle as safe-without-mutex/immutable — but set_reparse_point.go:254-257 does `openFile.mu.Lock(); openFile.MetadataHandle = newHandle; ...Unlock()` on a live shared pointer.
- **Why it matters:** Write-under-lock + read-without-lock on a slice header = real Go data race. A CLOSE racing SET_REPARSE_POINT (symlink conversion) on the same/sibling handle can read a torn/stale MetadataHandle, corrupting base-file/stream-sibling matching. handler.go:431 doc comment is stale for this field.
- **Fix:** Read `other.MetadataHandle`/`openFile.MetadataHandle` under `RLock()` in electDeleteOnClose, or revert set_reparse_point.go to allocate a fresh OpenFile instead of mutating in place; correct the doc comment either way.
- **Verified:** CONFIRMED. Both sides production-reachable (electDeleteOnClose from close.go:395/handler.go:1405 CLOSE; handleSetReparsePoint from ioctl_dispatch.go:26). Downgraded HIGH->MED: needs a symlink-creating IOCTL concurrent with a CLOSE election.

### [MED] parseACEs panics on undersized AceSize (slice bounds low>high) from untrusted wire bytes · `bugs` · area: handlers-security-sd
- **Where:** `internal/adapter/smb/handlers/security.go:929`
- **What:** Bounds-checks ACE only against `len(data)` (`offset+int(aceSize) > len(data)`), never checks `aceSize >= aceHeaderSize` (8). Wire AceSize of 0-7 makes `data[offset+aceHeaderSize : offset+int(aceSize)]` low>high — Go runtime panic, not an error. Reached from untrusted wire: SET_INFO FileSecurityInformation (set_info.go:2145) and CREATE SecD context (create_post_break.go:827), via parseDACL/parseACEs (used for both DACL and SACL).
- **Why it matters:** Attacker fully controls AceType/AceFlags/AceSize on wire; AceSize<8 is a trivial valid-looking uint16. MS-DTYP 2.4.4 minimum ACE is the 8-byte header — should be STATUS_INVALID_PARAMETER, not a crash.
- **Fix:** `if aceSize < aceHeaderSize { return nil, fmt.Errorf(...) }` right after reading aceSize, combined with the existing offset+aceSize bound check.
- **Verified:** CONFIRMED but downgraded HIGH->MED: `pkg/adapter/smb/connection.go:662` handleRequestPanic is deferred in every request goroutine (435, 446) and recovers — impact is one unanswered request + Error log, not connection/process kill.

### [MED] Async-callback wiring block duplicated verbatim across the two dispatch entry points · `structure` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/response.go:216`
- **What:** response.go:216-263 (ProcessSingleRequest) and response.go:582-624 (ProcessRequestWithFileIDAndCallback) near-byte-identical: same TryReserveAsync/ReleaseAsync wiring, same CHANGE_NOTIFY/READ/CREATE/LOCK/CANCEL if-blocks, same `ci := connInfo` closures. ~48 lines copy-pasted single-request vs compound path.
- **Why it matters:** Critical funnel (lease/oplock break parking for CREATE, lock-conflict parking for LOCK, pipe-read parking for READ). Fix to one copy doesn't propagate to the other unless author remembers both sites — classic DRY hazard on correctness-critical code.
- **Fix:** Extract `wireAsyncCallbacks(handlerCtx, reqHeader, connInfo, asyncNotifyCallback)`, call from both ProcessSingleRequest and ProcessRequestWithFileIDAndCallback right after TryReserveAsync/ReleaseAsync wiring.
- **Verified:** CONFIRMED. Both live: ProcessSingleRequest called connection.go:376,449; ProcessRequestWithFileIDAndCallback called compound.go:167,993 and response.go:665. No live bug today (copies in sync) — structure/DRY only.

### [MED] NTLM path has compute-only mechListMIC API — no verify counterpart in package · `structure` · area: smb-auth-ntlm-spnego
- **Where:** `internal/adapter/smb/auth/ntlm.go:1013`
- **What:** ComputeNTLMSSPMechListMIC(1013) + computeNTLMSSPMechListMICLegacy(1129) build SPNEGO downgrade-protection MIC over mechList for NTLM. No verify counterpart. session_setup.go:950,2051 call compute only (outgoing). session_setup.go:1083 `extractNTLMToken` discards MechListBytes/MIC on AUTHENTICATE step entirely.
- **Why it matters:** RFC 4178 §5 downgrade protection is bidirectional. spnego.go VerifyMechListMIC exists but wraps Kerberos gssapi.MICToken (RFC 4121) — incompatible wire format vs MS-NLMP §2.2.2.9.1 NTLMSSP MIC. Kerberos gets bidirectional protection; NTLM only outbound. On-path attacker can bid down Kerberos→NTLM undetected on NTLM path.
- **Fix:** Add `VerifyNTLMSSPMechListMIC(exportedSessionKey, mechListBytes, flags, micBytes)` mirroring the two compute fns (constant-time compare). Wire into completeNTLMAuth: capture parsed.MechListMIC from AUTHENTICATE-step token (currently discarded session_setup.go:1083), verify once signing key derived.
- **Verified:** CONFIRMED. ComputeNTLMSSPMechListMIC outbound-only (session_setup.go:950,2051). spnego.go:284 VerifyMechListMIC used only from kerberos_auth.go:290,411, structurally cannot verify NTLMSSP layout. Overlaps existing unverified AUTHENTICATE_MESSAGE MIC finding (same root/attacker) → MED not HIGH. Note: ntlm.go:1026/1052 show two incompatible MIC encodings already — verify-if-present must log-then-reject behind flag, not hard-reject day one.

### [MED] AEAD nonce fully random, not counter-guaranteed unique — violates MS-SMB2 MUST-NOT-repeat rule · `structure` · area: smb-crypto
- **Where:** `internal/adapter/smb/encryption/middleware.go:189`
- **What:** EncryptResponse generates nonce via crypto/rand.Read every call (189-190) into EncryptWithNonce. No per-session counter in crypto_state.go/middleware.go. Same pattern encryptor.go:57-59. GCM nonce 12B(96b), CCM 11B(88b). Key derived once at session establish (DeriveAllKeys), no rekey on reauth/rebind.
- **Why it matters:** MS-SMB2 3.1.4.3 nonce MUST NOT repeat under one key — package's own doc.go:28 states this then implements only probabilistic uniqueness. GCM reuse leaks GHASH auth subkey H → forge arbitrary messages. CCM reuse breaks confidentiality via keystream reuse. 88-bit CCM birthday bound ~2^44 reachable on busy long-lived session over months.
- **Fix:** Per-session atomic uint64 counter for Encryptor/Decryptor (separate, keys differ), pack into nonce (8B counter + zero-pad, Samba-style), increment per message, reset only on key re-derive. Fail closed on wrap.
- **Verified:** CONFIRMED against spec. MS-SMB2 2.2.41 + 3.1.4.3 step 4 require uniqueness a random draw cannot guarantee; both reference impls (Samba) use counters. Probability remote (2^44/2^48) so MED not HIGH, but violates a MUST and NIST SP 800-38D's 2^32 random-nonce bound. Fix: per-session atomic uint64 counter in low bytes, random per-session prefix.

### [MED] Serve() does unbounded pre-bind work, turning the confirmed base.go:217 Stop-before-bind leak from theoretical into reachable · `structure` · area: pkg-smb-adapter
- **Where:** `pkg/adapter/smb/adapter.go:523`
- **What:** Adapter.Serve(523-559) runs findDurableHandleStore + SeedFromDurableHandles (no timeout) + startEnabledDiscovery BEFORE ServeWithFactory (only place net.Listen runs, base.go:217-221). service.go registerAndRunAdapterLocked(611-619) launches `go adp.Serve(ctx)` then releases lock; concurrent stopAdapter(406-446) can call Stop() while Serve still scanning. BaseAdapter.Stop→initiateShutdown spends shutdownOnce with b.listener==nil, close skipped. Serve later binds; nothing closes it.
- **Why it matters:** Port bound forever, adapter stuck 'cancelled', unrestartable without killing process. SMB widens race window vs NFS (comparatively little pre-bind work) via DB scan + discovery sidecar start.
- **Fix:** Bind first: move listener bind synchronous at top of Serve(); gate SeedFromDurableHandles/discovery on `<-s.ListenerReady()` in a goroutine instead of inline before call.
- **Verified:** CONFIRMED reachable, worse than described. Full chain traced: service.go:611-619→stopAdapter:406-446→BaseAdapter.Stop:462→initiateShutdown:345 spends shutdownOnce with listener nil (:353 skipped). Serve proceeds into ServeWithFactory (base.go:203-217, no ctx/Shutdown pre-check), binds. ctx.Done goroutine(:225-229) then no-ops. Accept loop either serves on "stopped" adapter or gracefulShutdown(:391-419) waits on conns and NEVER closes listener — fd/port leaks for process life. Real fix belongs in base.go: after storing listener, if b.Shutdown already closed, close immediately.

### [MED] CREATE parked mid-compound-chain unhandled: trailing commands run against stale FileID, resume goroutine leaks · `gaps` · area: handlers-create-post-break
- **Where:** `internal/adapter/smb/handlers/create_post_break.go:1640`
- **What:** breakAndMaybeParkCreate/parkCreateOnLeaseBreak(224,1518) never consult ctx.NextCommand before parking, unlike lock.go:525-531. Compound processor special-cases only first(compound.go:178-242) and true-last(1039-1042). Interior-position parked CREATE: processRemaining(1053-1061) treats StatusPending as ordinary completed subcommand, embeds AsyncId in that frame, continues loop using s.lastFileID still zero (fid extraction only on StatusSuccess) — trailing CLOSE/QUERY_INFO runs against bogus FileID. Its AsyncId never added to markStartedAsyncIDs → MarkStarted never called → resume goroutine blocks forever on unconditional `<-pending.started`(1640), leaking goroutine+async credit slot until session teardown.
- **Why it matters:** compound.go:1036-1038's own comment documents the requirement ("Non-tail parked CREATEs MUST NOT be released here") but code beneath doesn't implement deferral. Client-visible: wrong status for trailing subcommands in same frame as premature Pending marker, real CREATE completion never reaches client until teardown.
- **Fix:** Mirror lock.go guard: refuse async-park unless ctx.NextCommand==0 or known first-command path. Add ctx-level IsFirstCompoundCommand flag, check in parkCreateOnLeaseBreak before returning nonzero AsyncId; interior positions fall through to existing synchronous wait (create_post_break.go:460-496).
- **Verified:** CONFIRMED. No NextCommand check anywhere in handlers/create*.go. response.go:605 wires AsyncCreateCompleteCallback on compound path so guard passes at any position. markStartedAsyncIDs only appended for first(228/241) and isLastCommand(1041). Severity MED not HIGH: needs ≥3-command chain with CREATE interior AND lease/share-mode break on it. Fix: `if ctx.NextCommand != 0 { return 0 }` in parkCreateOnLeaseBreak.

### [MED] VALIDATE_NEGOTIATE_INFO never checks MaxOutputResponse — must terminate connection when too small · `gaps` · area: handlers-ioctl-core
- **Where:** `internal/adapter/smb/handlers/ioctl_validate_negotiate.go:43`
- **What:** handleValidateNegotiateInfo + callees never read MaxOutputResponse (IOCTL body offset 44) before returning 24-byte response. Every other fixed-size FSCTL handler uses parseIoctlMaxOutputSize (ioctl_fsctl.go:277, used at :172, copychunk:161, sparse:217, network_interfaces:84) — this handler is the outlier.
- **Why it matters:** MS-SMB2 3.3.5.15.12: if MaxOutputResponse < size of VALIDATE_NEGOTIATE_INFO Response(24B), server MUST terminate transport connection. Same fail-closed contract as dialect/GUID/capabilities/security-mode checks already in validateFromCryptoState(151-166+). Today undersized-buffer client always gets STATUS_SUCCESS+24B instead.
- **Fix:** After fileID/size checks: `maxOut := parseIoctlMaxOutputSize(body); if maxOut < 24 { return &HandlerResult{DropConnection: true}, nil }`.
- **Verified:** CONFIRMED + explicit MUST (fetched MS-SMB2 3.3.5.15.12 item 2 verbatim: server MUST terminate transport connection and free Connection object). Reachable on 3.0/3.0.2 (3.1.1 already drops at line 112). Fix as stated.

### [MED] SET_INFO Rename with non-zero RootDirectory silently renames into wrong directory instead of rejecting · `gaps` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:866`
- **What:** MS-SMB2 §3.3.5.21.1 requires STATUS_INVALID_PARAMETER when FILE_RENAME_INFORMATION.RootDirectory non-zero over network transport. Code instead falls back: `toDir = openFile.Name().ParentHandle; toName = path.Base(newPath)` — renames into own current parent using basename, discarding RootDirectory intent, returns SUCCESS. Same bug hard links at set_info.go:2561-2569 (FILE_LINK_INFORMATION).
- **Why it matters:** Data-placement correctness bug not just unimplemented feature — request appears to succeed while moving/linking file somewhere client never asked, worse than returning error. Hits any client following classic NtSetInformationFile local-handle convention or conformance/fuzz client.
- **Fix:** Per §3.3.5.21.1: if RootDirectory non-zero, return setInfoStatus(StatusInvalidParameter) immediately. Delete same-dir fallback branches at 866-873 and 2562-2569, replace with explicit rejection.
- **Verified:** CONFIRMED + spec-violating. Fetched MS-SMB2 3.3.5.21.1: nonzero RootDirectory MUST fail STATUS_INVALID_PARAMETER; MS-FSCC 2.4.42.2/2.4.28.2 say field MUST be zero for network ops. Severity MED (no real client sets it; conformance/fuzz clients do). Fix: return StatusInvalidParameter both branches, delete fallback.

### [MED] AppInstanceId failover force-closes opens on ANY path/share, no access check — MS-SMB2 §3.3.5.9.13 requires matching PathName+Share and maximal-access (GENERIC_READ) check · `gaps` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_context.go:907`
- **What:** ProcessAppInstanceId(907-1038) force-closes every live OpenFile + persisted durable handle matching AppInstanceId only (`f.AppInstanceId == appId`, line 970; global durableStore.GetDurableHandlesByAppInstanceId, line 997 — store comment: "returns every handle for an app instance"). No check of: target PathName match, TreeConnect.Share match, peer ClientGuid difference, or requester's maximal access. handler.files is Handler-wide sync.Map shared across all shares/sessions — scan is global.
- **Why it matters:** §3.3.5.9.13 requires AppInstanceId+PathName+Share+ClientGuid-diff match, THEN maximal-access GENERIC_READ check before force-close. Consequence (a) correctness: Hyper-V reuses one AppInstanceId across VM's files (VHDX+config) — CREATE for file A wrongly closes unrelated file B's live open on possibly different share. (b) access control: AppInstanceId sent cleartext, not secret — any authenticated user supplying a matching 16-byte value can force-close another user's handle on any file/share, zero permission check — cross-tenant DoS.
- **Fix:** Pass shareName + target path into ProcessAppInstanceId, require match against Open's stored ShareName/Path before including in close set. Compare ClientGuid, skip same-client opens. Resolve requester's maximal access via metadata store, require GENERIC_READ, return STATUS_ACCESS_DENIED (not silent skip) when absent.
- **Verified:** CONFIRMED. No path/share/ClientGuid/maximal-access check anywhere in function. Fetched MS-SMB2 3.3.5.9.13: requires all four lookup conditions then "server MUST calculate maximal access... If maximal access includes GENERIC_READ, server MUST close the open." Severity MED not HIGH: cross-tenant force-close needs attacker knowing 16-byte AppInstanceId; correctness half (file A closing unrelated file B) is deterministic. Note: keep smb2.durable-v2-open.app-instance test in mind (reopens SAME file on tree2) — path/share/access checks safe, but don't add ClientGuid inequality without re-running it.

### [MED] parseACEs panics (slice bounds out of range) on ACE with AceSize < 8 · `gaps` · area: handlers-security-sd
- **Where:** `internal/adapter/smb/handlers/security.go:924`
- **What:** parseACEs only upper-bounds AceSize before slicing `data[offset+aceHeaderSize : offset+int(aceSize)]`(929); never checks AceSize >= aceHeaderSize(8). Client ACE with AceSize in [0,7] passes upper-bound check, slice has low>high → panic. Reached from SET_INFO FileSecurityInformation(parseDACL→parseACEs) and SACL path(805) and CREATE SecD buffer.
- **Why it matters:** Inbound-descriptor bounds-checking invariant: every offset in inbound SD must be bounds-checked before dereference, must not panic/over-read. AceSize attacker-controlled and under-checked.
- **Fix:** `if int(aceSize) < aceHeaderSize || offset+int(aceSize) > len(data) { return nil, fmt.Errorf("ACE %d size %d invalid", i, aceSize) }` — extend existing check at 924.
- **Verified:** CONFIRMED security.go:911-929, low>high slice panic reachable via set_info.go:2145 and create_post_break.go:827→ParseSecurityDescriptorWithOptions→parseDACL/parseACEs(758,805). "Crashes whole process" REFUTED: connection.go:661-673 handleRequestPanic deferred in per-request goroutines(435,446) + handleConnectionClose recover(224) — grep for recover() was scoped wrong (missed transport package). Real impact: one request never answered (client hangs on MessageID) + Error log, not server crash. Fix unchanged.

### [MED] Compound sub-commands 2..N skip CreditCharge-vs-payload-size validation (bypass for READ/WRITE/IOCTL/QUERY_DIRECTORY) · `gaps` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:141`
- **What:** session.ValidateCreditCharge called only for first command(compound.go:98). Pre-pass(130-152) walks trailing subcommands, calls EffectiveCreditCharge+SequenceWindow.Consume, but never ValidateCreditCharge. Dispatch paths ProcessRequestWithFileIDAndCallback(response.go:536) and ProcessRequestWithInheritedFileID(663) also never call it. Contrast ProcessSingleRequest(142) which does, for every standalone request.
- **Why it matters:** MS-SMB2 3.3.5.2.5 requires verifying CreditCharge sufficient for payload on every message, not just first of compound. Compounding smuggles full-size WRITE/READ/IOCTL/QUERY_DIRECTORY body past size-vs-charge gate every other path enforces.
- **Fix:** Call session.ValidateCreditCharge(hdr.Command, hdr.CreditCharge, cmdBody) for every non-exempt subcommand in the 130-152 loop, fail whole compound with STATUS_INVALID_PARAMETER on mismatch.
- **Verified:** CONFIRMED by exhaustive grep — exactly two non-test callers (response.go:142, compound.go:98 firstBody only). MS-SMB2 3.3.5.2.5 applies per received message, 3.3.5.2.7 processes each chained request in turn — real gap. Downgraded HIGH→MED: consequence is credit-accounting drift, not memory/auth compromise — payload still bounded by MaxRead/MaxWrite, sequence window still debits charge. Fix: call ValidateCreditCharge inside 130-152 loop (needs body, already returned by ParseCompoundCommand).

### [MED] CHANGE_NOTIFY as FIRST command of multi-command compound not gated for non-last position; going async silently drops every trailing subcommand's response · `gaps` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:178`
- **What:** processRemaining(953) rejects CHANGE_NOTIFY in non-last TRAILING position with STATUS_INTERNAL_ERROR. Gate never applied to FIRST command, dispatched unguarded at 167. If firstHeader.Command==CHANGE_NOTIFY goes async(StatusPending, common case), generic async-first branch(178-231) calls PendingCreateRegistry.ReplaceCallback/MarkStarted against wrong asyncId — CHANGE_NOTIFY completion tracked in separate NotifyRegistry(change_notify.go), so both calls are silent no-ops. Interim sent standalone, function `return`s — compoundData never parsed/dispatched, trailing commands orphaned.
- **Why it matters:** Client compounding CHANGE_NOTIFY+other commands (illegal, exactly what trailing-position gate exists to reject) gets no INTERNAL_ERROR and no responses at all when notify has nothing pending — orphaned rather than the single synchronous error Windows returns.
- **Fix:** Apply same isLastCommand-style guard to first command before dispatch: if firstHeader.Command==SMB2ChangeNotify && len(compoundData)>=HeaderSize, return STATUS_INTERNAL_ERROR for whole compound (same as 953-965) instead of dispatching.
- **Verified:** CONFIRMED. stub_handlers.go:804-808 confirms StatusPending path. Async-first branch touches only PendingCreateRegistry(196,228) — no-ops for CHANGE_NOTIFY (NotifyRegistry is separate, byAsyncId, change_notify.go:935/1726) — then `return`s at 230 unparsed. Downgraded HIGH→MED: only illegal/nonconformant compound reaches it (real clients put CHANGE_NOTIFY last), notify itself still completes via AsyncNotifyCallback, no memory/auth consequence. Fix: hoist :953 guard ahead of :167.

### [MED] IOCTL CreditCharge validation reads wrong field (InputCount, not MaxOutputResponse) · `gaps` · area: smb-session
- **Where:** `internal/adapter/smb/session/credit_validation.go:98`
- **What:** extractPayloadSize for CommandIoctl reads body[28:32], labels it MaxOutputResponse. MS-SMB2 §2.2.31: offset 28 is InputCount, MaxOutputResponse is offset 44. This repo's own ioctl_fsctl.go:273-283(parseIoctlMaxOutputSize) reads body[44:48], confirming layout. Test fixture credit_validation_test.go:119-125(makeIoctlBody) written to same wrong offset — self-certifies bug.
- **Why it matters:** §3.3.5.2.5 requires validating CreditCharge against MaxOutputResponse so client can't claim tiny charge with huge FSCTL output. Validating InputCount instead makes check near no-op for common case (small input, large output) — defeats credit accounting. Can also spuriously reject legit large-InputCount+small-MaxOutputResponse requests.
- **Fix:** Read MaxOutputResponse at body[44:48] (len(body)>=48 guard), matching ioctl_fsctl.go. Fix makeIoctlBody test fixture to write maxOutput at offset 44.
- **Verified:** CONFIRMED. Two decoders in repo disagree; credit one is wrong half. Reachable via ValidateCreditCharge(response.go:142/compound.go:98). MED not HIGH: inert-or-wrong accounting only, no memory/auth impact.

### [MED] QUERY_DIRECTORY CreditCharge validation reads wrong field (FileIndex, not OutputBufferLength) · `gaps` · area: smb-session
- **Where:** `internal/adapter/smb/session/credit_validation.go:106`
- **What:** extractPayloadSize for CommandQueryDirectory reads body[4:8], labels it OutputBufferLength. MS-SMB2 §2.2.33: offset 4 is FileIndex, OutputBufferLength is offset 28. handlers/query_directory.go:127-142(DecodeQueryDirectoryRequest) confirms layout (reads outputBufferLength only after FileIndex+FileID+two name fields). Test fixture makeQueryDirBody(127-133) written to same wrong offset.
- **Why it matters:** FileIndex is 0 for vast majority of requests (SMB2_INDEX_SPECIFIED rarely set) → extractPayloadSize returns 0 regardless of actual OutputBufferLength (commonly 64KB-1MB) → requiredCharge collapses to 1-credit floor → check effectively never rejects undercharged QUERY_DIRECTORY, silently inert.
- **Fix:** Read OutputBufferLength at body[28:32] (len(body)>=32 guard), matching query_directory.go field order. Fix makeQueryDirBody test fixture to offset 28.
- **Verified:** CONFIRMED. Reachable via ValidateCreditCharge(response.go:142, compound.go:98). MED: inert accounting check, no memory/auth impact.

### [MED] AES-CCM/GCM encryption nonce is pure crypto/rand per message, not session counter — violates MS-SMB2 "MUST NOT be reused" as guarantee · `gaps` · area: smb-crypto
- **Where:** `internal/adapter/smb/encryption/middleware.go:188`
- **What:** EncryptResponse: `nonce := make([]byte, nonceSize); rand.Read(nonce)` every outgoing message (12B GCM/11B CCM). Same shape gcm_encryptor.go:57-58. No counter anywhere in signing/kdf/encryption or session/crypto_state.go. doc.go:27-29 documents this as intentional. GMAC signing (signing/gmac_signer.go) already derives deterministic nonce from monotonic MessageId+direction bit — right pattern next door, not applied to encryption.
- **Why it matters:** MS-SMB2 §3.1.4.3/TRANSFORM_HEADER: nonce MUST NOT be reused within session — hard guarantee, random only gives probabilistic. CCM 11B(88-bit): reuse is catastrophic, full auth-key/plaintext recovery via forgery, not just confidentiality loss. Persistent handles/multi-channel sessions kept open weeks/months is exactly DittoFS's target scenario. Windows and Samba both use monotonic per-connection counters for this reason.
- **Fix:** Replace rand nonce with monotonic counter seeded once per session/key (atomic uint64 on SessionCryptoState, low-order nonce bytes), increment per message, re-seed only on key re-derive. Wrap forces session/key rotation, not silent reuse of counter 0.
- **Verified:** CONFIRMED middleware.go:188-190, no counter anywhere. Reachable non-test: response.go:942,1104, compound.go:359,443, conn_types.go:148. Spec confirms MUST NOT reuse guarantee. Downgraded HIGH→MED: exploitability math overstated in original claim — CCM 88-bit birthday bound ~2^44 messages under ONE session key, not reachable at stated throughput; GCM 96-bit random is NIST-approved construction. Real defect is conformance + CCM's catastrophic-on-reuse failure mode, not a live break. Fix: per-session monotonic counter, same shape as signing/gmac_signer.go.

### [MED] Serve() performs pre-bind DB/discovery work that widens the confirmed 'Stop before listener binds' socket leak · `bugs` · area: pkg-smb-adapter
- **Where:** `pkg/adapter/smb/adapter.go:523`
- **What:** Serve()(523-559) calls findDurableHandleStore(), SeedFromDurableHandles (metadata scan), startEnabledDiscovery (mDNS/WS-Discovery sockets) BEFORE ServeWithFactory, where net.Listen actually binds(base.go:203-221). service.go: claimAndRunAdapter(317-335) registers entry and starts Serve goroutine before releasing s.mu (deferred unlock); awaitListener(337-380) doesn't hold s.mu while waiting on ListenerReady(). stopAdapter/DisableAdapter(406-429,276-289) gate only on s.entries[type]/entry.stopping — nothing gates on listener ready.
- **Why it matters:** DisableAdapter right after start invokes Stop() while Serve still doing pre-bind scan. Stop()→initiateShutdown spends shutdownOnce with listener nil (no-op close). Serve later binds real listener; post-bind ctx-watcher can't re-trigger close since shutdownOnce already spent — listener leaks bound forever. SMB's DB scan + discovery sidecar start is what makes the race window practically hittable vs a few-instruction window in the base primitive.
- **Fix:** Move durable-handle/discovery setup into goroutine gated on `<-s.ListenerReady()` (or ctx.Done()), launched just before ServeWithFactory, so listener binds as first action of Serve().
- **Verified:** CONFIRMED on all three legs. adapter.go:523-556 order confirmed; base.go:202 net.Listen inside ServeWithFactory; base.go:344-366 initiateShutdown shutdownOnce-guarded no-op close when listener nil, once then spent so post-bind ctx watcher(224-229) is a no-op. REACHABLE non-test: service.go:334 registerAndRunAdapterLocked starts Serve under deferred unlock, awaitListener(337-378) waits WITHOUT s.mu, stopAdapter(406-427, reached from DisableAdapter:276-288=REST) gates only on entries/stopping — Disable concurrent with Start calls Stop mid pre-bind-scan. Accept then blocks on bound socket, stopAdapter times out, port held for process life. Fix: re-check b.Shutdown after storing listener in ServeWithFactory (close+return if already signalled), and/or bind before pre-accept work.

### [LOW] Issue-number reference in source comment · `structure` · area: handlers-lease-oplock
- **Where:** `internal/adapter/smb/handlers/lease_context.go:406`
- **What:** `// existing holder (MS-SMB2 §3.3.5.9.8 / Samba `is_lease_stat_open`, #751).` — cites bare issue/PR number (#751) in behaviour comment.
- **Why it matters:** CLAUDE.md 'Code comments': behaviour only, no issue/PR numbers. `ponytail:` sole exception, not this.
- **Fix:** drop `, #751` — MS-SMB2 section + Samba fn name already carry citation.
- **Verified:** CONFIRMED at lease_context.go:406. Incomplete though — same package carries ~15 more (lease/manager.go:249 #751, handler.go:391 #532, :407 #739, :2621 #1652, auth/ntlm.go:358 #1357, rpc/pipe.go:74/94/114 #1607, types/status.go:237/244 #1228). Fix as one sweep, not one line.

### [LOW] SMB2 share-access bit constants duplicated under a second name · `structure` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/disconnected_state_machine.go:162`
- **What:** disconnected_state_machine.go:161-165 declares pkg-level smbShareRead/Write/Delete = 0x01/0x02/0x04. handler.go:2409-2412 already has same MS-SMB2 FILE_SHARE_* bits as fileShareRead/Write/Delete, func-local const in checkShareModeConflict. File's own comment (264-267) says disconnectedShareDeniesNewOpen 'Mirrors the D-side half of checkShareModeConflict'.
- **Why it matters:** two names, one wire constant, one package — invites drift if bit value fixed in one const block and missed in other.
- **Fix:** hoist fileShareRead/Write/Delete to package scope (handler.go or shared consts.go), delete smbShareRead/Write/Delete, both sites reference single set.
- **Verified:** CONFIRMED. Both live (disconnectedShareDeniesNewOpen + checkShareModeConflict, CREATE path). Fix: drop local block, use smbShare*.

### [LOW] LockReplayCache.ForgetFile is a full-cache scan on every CLOSE, and its own bound comment is wrong · `structure` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/replay_cache.go:352`
- **What:** ForgetFile (352-360): `for k := range c.entries { if k.FileID == fileID { delete... } }` over ENTIRE server-wide entries map per CLOSE, not just this file's buckets. Type doc (289-291) claims 'Bounded implicitly by the per-open 16-slot cap' but LockSequenceIndexMax (272) = 64, not 16 — doc understates real bound 4x.
- **Why it matters:** entries keyed by lockReplayKey{FileID, Index}, no secondary FileID index, so releasing one file's <=64 buckets costs O(total buckets across every open file on server). Many concurrent locked opens closing turns per-open O(1) into O(N).
- **Fix:** key map as map[[16]byte]map[uint32]CachedLockResponse (outer key FileID) — ForgetFile becomes single map delete of outer key. Also fix '16-slot' comment to 64 (LockSequenceIndexMax).
- **Verified:** CONFIRMED both halves. ForgetFile called from forgetReplayState (handler.go:1265), invoked by DeleteOpenFile — production CLOSE path. Cache is one map per Handler (NewLockReplayCache, handler.go:1036), server-wide, cost O(total buckets) not O(this file's). Blast radius small: entries only written when lockSeqEnabled (durable handle or multichannel, lock.go:211-219). Fix: correct comment to 64; add per-FileID index only if profiling shows need.

### [LOW] buildDACL/buildSACL/buildEmptySACL triplicate the ACL-header and ACE-encode wire logic the doc comment says is identical · `structure` · area: handlers-security-sd
- **Where:** `internal/adapter/smb/handlers/security.go:591`
- **What:** buildDACL (591-608) and buildSACL (638-653) write byte-identical 8-byte ACL headers (revision=2, sbz1=0, size, count, sbz2=0) and byte-identical per-ACE loops (type/flags/size/mask/SID); buildEmptySACL (658-664) duplicates header write a third time. buildSACL's own comment (614): 'The wire layout is identical to a DACL... only the audit/alarm ACE types... differ.'
- **Why it matters:** identical wire format collapsed behind separate copies instead of one table/helper — three header-write copies bump odds one gets fixed on a future MS-DTYP revision bump and others don't.
- **Fix:** extract `writeACLHeader(buf *smbenc.Writer, size, count uint16)` and `writeACEs(buf *smbenc.Writer, aces []windowsACE) int /*totalSize*/` shared by buildDACL/buildSACL; buildEmptySACL becomes `writeACLHeader(buf, aclHeaderSize, 0)`.
- **Verified:** CONFIRMED byte-identical header writes and ACE loops in buildDACL/buildSACL; buildEmptySACL repeats header third time. All reachable from BuildSecurityDescriptorWithGrants (security.go:325/333/335), called by query_info.go:413 and handler.go:2896. buildSACL's own comment concedes layout identical. Fix: one writeACL(buf, aces) helper; buildEmptySACL becomes writeACL(buf, nil).

### [LOW] Package-level global mutable state (SIDMapper, DirectorySIDBridge) instead of a threaded dependency · `structure` · area: handlers-security-sd
- **Where:** `internal/adapter/smb/handlers/security.go:118`
- **What:** `_defaultSIDMapper` (118) and `_directorySIDBridge` (173) are package-level atomic.Pointer globals, set once via SetSIDMapper/SetDirectorySIDBridge from pkg/adapter/smb/adapter.go at startup, read from every SD-building/parsing fn (ownerSIDFor, groupSIDFor, principalToSID, parseACEs, securityDescriptorOwnerGroupSIDs, isCurrentOwnerSID/isCurrentGroupSID) via fresh atomic Load each. Every test needing non-default mapper/bridge must save-and-restore global (security_test.go:21,881,884,891; security_directory_sid_test.go withDirectoryBridge helper 52-56).
- **Why it matters:** hidden global dependency instead of value threaded via constructor injection — call sites depend on init/wiring order silently, forces every package test onto serialized global state instead of independent parallelizability.
- **Fix:** not urgent (single-process/single-instance server). Natural fix: small `securityCodec{mapper *sid.SIDMapper, bridge DirectorySIDBridge}` constructed once in adapter wiring, passed to/stored on *Handler, Build/ParseSecurityDescriptor take it as param instead of two package atomics. Lower priority than findings above; flag as `ponytail:`-worthy shortcut if not fixed now.
- **Verified:** CONFIRMED but weak/overstated. Globals are atomic by design (race-safe, comment at 116-117 says why), set-once before connections; only 2-3 tests that MUTATE it need save/restore — TestMain sets once for rest, package not broadly serialized. Threading a mapper through every SD fn signature is large diff for no bug. LOW at most; not actionable ahead of anything else.

### [LOW] Issue numbers embedded in behaviour comments (violates repo comment convention) · `structure` · area: smb-conn-dispatch
- **Where:** `internal/adapter/smb/conn_types.go:43`
- **What:** conn_types.go:43 '...issue #361)', conn_types.go:192 '...issue #378.', framing.go:122 '...(#717).', hooks.go:123 '(issue #362)' — all reference tracker issue numbers inline in behaviour comments.
- **Why it matters:** CLAUDE.md 'Code comments': behaviour only, no issue/PR numbers, `ponytail:` sole exception, none of these are it. Issue numbers rot (closed/renumbered) while comment stays; causal story belongs as behaviour, not tracker lookup key.
- **Fix:** reword each to state invariant/race guarded, without issue number — e.g. conn_types.go:43 drop '; issue #361', MS-SMB2 §3.3.5.5.2 citation already carries spec authority. Same for other three.
- **Verified:** CONFIRMED all four in production files. Same incompleteness as earlier finding — compound.go:95/455/465, crypto_state.go:155, response.go:128/184/728/754/1277 carry more. Fix: one sweep rewriting each as behaviour ('SESSION_SETUP hooks must chain in order or signing keys diverge'), drop numbers.

### [LOW] ConnectionCryptoState exports every negotiation field despite a doc comment mandating lock-guarded access · `structure` · area: smb-response-pipeline
- **Where:** `internal/adapter/smb/crypto_state.go:28`
- **What:** crypto_state.go:71 says 'mu protects all negotiation fields for concurrent access,' but every field it protects (Dialect, CipherId, SigningAlgorithmId, ServerGUID, ServerCapabilities, ServerSecurityMode, ClientGUID, ClientCapabilities, ClientSecurityMode, ClientDialects, PreauthIntegrityHashId) is exported. Accessor methods (SetDialect/GetDialect etc.) are convention-only, not compiler-enforced. crypto_state_test.go already reads `cs.PreauthIntegrityHashId` directly (unsynchronized); no `GetPreauthIntegrityHashId` at all — one field whose setter (274-278) has no matching getter, breaking otherwise-consistent Set/Get pairing.
- **Why it matters:** mutex guarding exported fields is a footgun — nothing stops a future same-package caller reading/writing field directly, racing with UpdatePreauthHash/Get* under -race; already happened once in test file. Missing getter is asymmetry inviting same direct-field-read shortcut in production code.
- **Fix:** add GetPreauthIntegrityHashId for symmetry. Longer term, unexport fields (dialect, cipherID, serverGUID, ...) now every field has (or should have) synchronized accessor, so mutex enforced by type system not convention.
- **Verified:** CONFIRMED all 11 fields exported, mu comment claims protection, accessors convention-only. PreauthIntegrityHashId has setter (274-278), no getter — grep shows nothing in production reads field, only crypto_state_test.go:145-146 unsynchronized. Asymmetry real but benign today (write-only field). No production unsynchronized field access found — footgun, not live race. Fix: unexport fields (accessors exist for rest), or drop write-only field + setter.

### [LOW] FileRenameInfo/DecodeFileRenameInfo duplicated verbatim as FileLinkInfo/DecodeFileLinkInfo · `bloat` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:148`
- **What:** FileRenameInfo(76-89)+DecodeFileRenameInfo(148-174) and FileLinkInfo(2470-2481)+DecodeFileLinkInfo(2486-2507) identical: same 3 fields, same buffer[0]/[8:16]/[16:20]/[20:] parse. Comment 2469 admits layout mirrors rename.
- **Why it matters:** ~60 LOC dup for one wire structure; fix to one decoder's bounds-check won't propagate to other.
- **Fix:** collapse to one type/decoder: FileLinkInfo=FileRenameInfo alias, DecodeFileLinkInfo=DecodeFileRenameInfo (or shared decodeRenameOrLinkInfo, call sites 685/2536).
- **Verified:** CONFIRMED structurally identical (not literally byte-identical, names differ). Both reachable (rename branch + handleFileLinkInformation). MS-FSCC defines as 2 distinct info classes sharing layout — distinct types defensible, only ~22-line decoder redundant. LOW.

### [LOW] Destination-directory resolution duplicated between rename and hardlink handlers · `bloat` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:861`
- **What:** 861-901 (rename branch) and 2557-2594 (handleFileLinkInformation) both: RootDirectory zero/non-zero check, same-dir fallback + path.Base, else GetTree->GetRootHandle->path.Base/path.Dir->walkPath. Only var names/logs differ. Comment 2565 says "parity with rename".
- **Why it matters:** ~35 LOC dup; bugfix (e.g. RootDirectory->handle resolution) needs applying twice.
- **Fix:** extract resolveSetInfoDestPath(authCtx, openFile, rootDir, newPath) (dir metadata.FileHandle, name string, status types.Status), used by both.
- **Verified:** CONFIRMED verbatim except identifiers/logs. Same status mapping (StatusInvalidHandle/StatusObjectPathNotFound). Both reachable via SET_INFO dispatch. LOW — drift risk only when RootDirectory->handle resolution lands, exactly when both copies need same edit.

### [LOW] Truncate+reclaim+restore+notify tail duplicated between EOF-set and allocation-driven truncate · `bloat` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:1595`
- **What:** FileEndOfFileInformation(1595-1637) and truncate-down branch of FileAllocationInformation(1703-1733) both run SetFileAttributes(Size)->ReclaimTruncatedBlocks->restoreFrozenTimestamps->flushSmbDelayedWrite->StoreOpenFile->breakParentDirLeasesForContentChange->notifyOpenFileModified(FileNotifyChangeSize). Comment 1701 claims "reuse" but is hand copy.
- **Why it matters:** ~8-line seq duplicated, only size/snapshot var differs; stated reuse not realized, copies can drift silently.
- **Fix:** extract truncateFileAndReclaim(ctx, authCtx, openFile, preFile, newSize) error covering shared tail; keep distinct pre-checks (lock-conflict for EOF; FILE_WRITE_DATA gate for Allocation) at call site.
- **Verified:** CONFIRMED, and drift ALREADY started: EOF path takes byte-range lock-conflict check (1580-1593->StatusFileLockConflict), alloc path doesn't; alloc path sets authCtx.WriteAuthorizedByHandle(1710), EOF doesn't — two guards already disagree. Fix: one truncateTo() helper. LOW as bloat; guard asymmetry worth separate look.

### [LOW] Scavenger constructor threads a `timeoutMs` default that is never read · `bloat` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_scavenger.go:25`
- **What:** DurableHandleScavenger.timeoutMs set from ctor param(43), never read by any method. expireHandles/expireHandlesFromPreviousInstance use per-handle h.TimeoutMs, never s.timeoutMs. Prod caller (adapter.go:536-546) still resolves+passes a default that does nothing.
- **Why it matters:** dead struct field carried through public ctor sig — leftover from refactor where per-handle timeout replaced server-wide default.
- **Fix:** drop timeoutMs field+param; update adapter.go and *_test.go NewDurableHandleScavenger call sites.
- **Verified:** CONFIRMED. Only timeout reads are h.TimeoutMs at :89,:124. Zero reads of s.timeoutMs. Ctor is prod-reachable (adapter.go:541) so dead param threaded through live plumbing. LOW.

### [LOW] `purgeOneDisconnectedHandle` re-derives `LockOpenID` instead of calling it · `bloat` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/disconnected_state_machine.go:431`
- **What:** 431-435 manually reimplements PersistedDurableHandle.LockOpenID() (fallback OriginalFileID->FileID, fmt.Sprintf("%x")) instead of calling d.LockOpenID(). Doc comment on that method (durable_store.go:194-199) says cleanup paths must use it by this ID; scavenger's cleanupAndDelete(151) and ProcessAppInstanceId correctly call it — this is the one path that didn't.
- **Why it matters:** same fact hand-maintained twice; future fallback-rule change silently misses this call site.
- **Fix:** replace 431-435 with `openID := d.LockOpenID()`.
- **Verified:** CONFIRMED byte-for-byte reimplementation of durable_store.go:195-201. REACHABLE non-test: purgeOneDisconnectedHandle <- purgeDisconnectedConflicts:362 <- write.go:392, set_info.go:1147/1159; ForOpen <- create_post_break.go:971. Behaviour identical today, pure drift risk. LOW.

### [LOW] `processV1Reconnect`'s conflicting-V2-context check is unreachable · `bloat` · area: handlers-durable
- **Where:** `internal/adapter/smb/handlers/durable_context.go:502`
- **What:** processV1Reconnect re-checks FindCreateContext(DurableHandleV2RequestTag/ReconnectTag)!=nil -> StatusInvalidParameter. Create() (create.go:530) already calls ValidateDurableContexts unconditionally before any reconnect branch, which already rejects dhnc+dh2q and dhnc+dh2c. processV1Reconnect only runs when dhnc!=nil, so by line 503 neither DH2Q nor DH2C can be present.
- **Why it matters:** duplicate validation left behind after shared gate introduced — dead branch, single call site, unconditional prior gate.
- **Fix:** delete 502-507 (+ its log line), or replace with comment referencing ValidateDurableContexts.
- **Verified:** CONFIRMED dead, deader than claimed: ProcessDurableReconnectContext:363 takes DH2C branch first, so dh2c!=nil never even reaches V1. Sole chain: create.go:820->ProcessDurableReconnectContext->processV1Reconnect. Delete it. LOW, no behaviour change.

### [LOW] buildDACL and buildSACL duplicate the ACL-header + ACE-serialization write loop · `bloat` · area: handlers-security-sd
- **Where:** `internal/adapter/smb/handlers/security.go:586`
- **What:** buildDACL(585-609) and buildSACL(633-653) both do byte-for-byte: totalACLSize loop, 8-byte ACL header write, per-ACE type/flags/size/mask/SID write over []windowsACE. Only how `aces` gets populated differs (DACL adds grant-merge/dedup).
- **Why it matters:** per-format duplicated wire-serialization, ~18 lines copy-pasted verbatim.
- **Fix:** extract writeACL(buf *smbenc.Writer, aces []windowsACE) covering header-compute+write+per-ACE-write; call from both.
- **Verified:** CONFIRMED verbatim (rev 2, Sbz1, size, count, Sbz2 header + per-ACE encodeSID), only comments differ. Reachable: buildDACL(security.go:325)+buildSACL(:333) inside BuildSecurityDescriptorWithGrants <- query_info.go:413, handler.go:2896. ~20 lines. LOW — cosmetic, wire format fixed by MS-DTYP 2.4.5.

### [LOW] doc.go is stale: wrong dialect ceiling and a path that no longer exists · `bloat` · area: smb-conn-dispatch
- **Where:** `internal/adapter/smb/doc.go:16`
- **What:** doc.go:16 "DittoFS implements SMB2 dialect 0x0202 (SMB 2.0.2)" — ships 0x0202-0x0311. doc.go:10 cites "Handler Layer (v2/handlers/)" — no v2/handlers/ dir exists; real package is internal/adapter/smb/handlers.
- **Why it matters:** godoc surface, wrong protocol-support claim + dead dir reference actively mislead.
- **Fix:** update line 16 to list 0x0202-0x0311; fix line 10 to say handlers/ (drop v2/ prefix).
- **Verified:** CONFIRMED. Shipped default MaxDialect SMB3.1.1 (adapter_settings.go:157,296; negotiate.go handles Dialect0311 at :96,104,309). `find internal/adapter/smb -type d` shows no v2 dir (doc.go:9 pkg/adapter/smb/ correct, does exist). Also omits SMB3 encryption/leases/durable-handles. Docs-only. LOW.

### [LOW] channelSeqFileID duplicates requestFileIDOffset's per-command byte-offset table · `bloat` · area: smb-conn-dispatch
- **Where:** `internal/adapter/smb/channel_sequence.go:29`
- **What:** channelSeqFileID(29-44) reimplements per-command FileID-offset switch (Read/Write/SetInfo->16, Ioctl->8), strict subset of requestFileIDOffset (request_fileid.go:39), same package smb, identical offsets for identical commands.
- **Why it matters:** same fact hand-maintained twice in same package; future MS-SMB2 change or new handle-scoped command desyncs silently.
- **Fix:** delete switch in channelSeqFileID, call requestFileIDOffset(cmd) directly, keep only copy+bound-check local.
- **Verified:** CONFIRMED both tables agree by hand (same constants per types/constants.go:154). Reachable: channelSeqFileID <- verifyChannelSequence:61 <- response.go:207,:572, both prod dispatch. Caveat: shorter list is SEMANTIC (only CSN-participating commands) — fix should delegate via a participation switch, not a flat table merge. LOW.

### [LOW] AllSharesResolver split from LockManagerResolver is speculative generality with an untested dead branch · `bloat` · area: smb-lease-manager
- **Where:** `internal/adapter/smb/lease/notifier.go:191`
- **What:** SMBOplockBreaker.resolver typed narrow LockManagerResolver, resolveLockManagerForHandle(191-203) type-asserts to wider AllSharesResolver, nil on fail per comment "for now, use AllSharesLockManager if resolver supports it." Only prod implementer (adapter.go's metadataServiceResolver) always implements both — assertion unconditionally true. SMBOplockBreaker never calls GetLockManagerForShare, only GetLockManagerForHandle via this assert. No test for SMBOplockBreaker/resolveLockManagerForHandle.
- **Why it matters:** struct typed for capability it never uses, defensively probes for capability it does use via assertion that can't fail — guarding a variation that doesn't exist.
- **Fix:** type resolver field (and NewSMBOplockBreaker param) as AllSharesResolver directly, call GetLockManagerForHandle without assert, delete nil-fallback branch.
- **Verified:** CONFIRMED. SMBOplockBreaker calls only GetLockManagerForHandle(:161,:171,:181). Sole prod construction adapter.go:269 passes metadataServiceResolver implementing both(:1003,:1012) — assert never fails, nil branch dead. Struct prod-reachable, only fallback dead. LOW, ~12 lines, no behavior risk.

### [LOW] asyncCallback closure rebuilt twice per request instead of once per connection · `bloat` · area: pkg-smb-connection
- **Where:** `pkg/adapter/smb/connection.go:436`
- **What:** c.makeAsyncNotifyCallback(ci) called at 436 (compound-request goroutine) and again identically at 448 (single-request goroutine). ci built once in connInfo()(236), never changes for connection lifetime — same value every time, re-derived per request in two branches.
- **Why it matters:** pure duplication of equivalent closure across both goroutine-launch branches and every loop iteration.
- **Fix:** compute asyncCallback := c.makeAsyncNotifyCallback(ci) once right after ci := c.connInfo()(236), capture single value in both goroutine closures.
- **Verified:** CONFIRMED. ci built once at :235, never reassigned in loop; closure(:465-469) captures nothing else, invariant for connection. Fix: hoist one call beside ci at :235. LOW — one closure alloc/request, no correctness impact.

### [LOW] Ioctl() never validates the request Flags field — non-FSCTL IOCTLs are silently dispatched as FSCTL · `gaps` · area: handlers-ioctl-core
- **Where:** `internal/adapter/smb/handlers/ioctl_dispatch.go:64`
- **What:** Ioctl() reads CtlCode, dispatches straight to ioctlDispatch[ctlCode], never inspects request Flags (offset 48, SMB2_0_IOCTL_IS_FSCTL). No upstream caller (dispatch.go:277) checks it either.
- **Why it matters:** MS-SMB2 3.3.5.15 MUST: if Flags != SMB2_0_IOCTL_IS_FSCTL, fail STATUS_NOT_SUPPORTED, checked before CtlCode dispatch. Samba does this in smb2_ioctl.c before switch. DittoFS treats every IOCTL as FSCTL regardless of Flags.
- **Fix:** in Ioctl(), read Flags (body[48:52]), return NewErrorResult(types.StatusNotSupported) before dispatch-table lookup if not SMB2_0_IOCTL_IS_FSCTL(0x1).
- **Verified:** CONFIRMED. Flags never parsed in file; spec text confirmed (MS-SMB2 3.3.5.15). Reachable dispatch.go:116-118 handleIoctl -> h.Ioctl, non-test. LOW: only non-conformant client sends non-FSCTL-flagged IOCTL; fix is 4-line read+reject.

### [LOW] fileIDOffset/ExtractFileID/InjectFileID treat OPLOCK_BREAK's lease-break-acknowledgment body as if it carried a FileId, misreading/corrupting the LeaseKey field · `gaps` · area: smb-compound
- **Where:** `internal/adapter/smb/compound.go:714`
- **What:** fileIDOffset(SMB2OplockBreak) unconditionally returns 8 — correct for Oplock Break Ack (StructureSize=24, FileId at offset 8) but SMB2_OPLOCK_BREAK is reused for Lease Break Ack, whose offset 8 holds 16-byte LeaseKey (StructureSize=36 distinguishes on wire). Never inspects StructureSize. ExtractFileID on a lease-ack body copies LeaseKey into lastFileID; InjectFileID on a related lease-ack sub-command overwrites bytes 8-24 (real LeaseKey) with inherited FileID.
- **Why it matters:** MS-SMB2 2.2.24.1 vs 2.2.24.2 share opcode, not body layout at this offset; conflating means FileID inheritance across compounded lease-break-ack uses/produces garbage, can misroute a related command or break the lease-break-ack itself (server can't match outstanding break).
- **Fix:** fileIDOffset (or ExtractFileID/InjectFileID) check sub-command StructureSize (24 vs 36) for SMB2OplockBreak, return -1 for 36-byte lease-break-ack variant (never carries FileId).
- **Verified:** CONFIRMED structurally. ExtractFileID(:732)->state.lastFileID(:252,1002); InjectFileID(:768) overwrites 8-24 when hdr.IsRelated()&&lastFileID!=0(:982). OPLOCK_BREAK live dispatch entry (dispatch.go:140), both helpers called from prod compound loop. LOW: only triggers when client compounds a lease-break ack, blast radius is that client's own ack. Fix: -1 (or branch on StructureSize) for SMB2OplockBreak.

### [LOW] getCachedShares/shareSecurityDescriptor take no ctx, fabricate context.Background() · `structure` · area: handlers-core-handler
- **Where:** `internal/adapter/smb/handlers/handler.go:2886`
- **What:** shareSecurityDescriptor(2880) calls h.Registry.ShareRootGrantACL(context.Background(), shareName) — method has no ctx param, only caller getCachedShares(2830) also takes none.
- **Why it matters:** repo convention: ctx first param, not fabricated ad hoc in a leaf func — fabricated background context drops request cancellation/deadline for the grant-ACL lookup.
- **Fix:** thread ctx: getCachedShares(ctx), shareSecurityDescriptor(ctx, shareName), srvsvc RPC handler (already holds request ctx) as caller instead of minting background ctx inside handlers.
- **Verified:** CONFIRMED, style/structure only. Reachable from create.go:1797 (IPC$/srvsvc share enumeration), non-test. No behavioral bug — lookup best-effort, failures logged+swallowed — but drops cancellation/deadline, breaks ctx-first convention. LOW.

### [LOW] configureSessionSigningWithKey is a 220-line function combining crypto derivation, signing config, and encryption negotiation · `structure` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:1795`
- **What:** 1795-1997 (203 lines: ~140 code + ~60 comment) handles dialect detection, reauth-vs-fresh preauth-hash branching, SP800-108 key derivation, signing-required gating, cipher selection + anon-encryption eligibility, encryptor creation, separate SMB2.x-only path — one function, 5 nesting levels deep (dialect>=0300 -> encryptionEnabled&&(!IsNull||allowAnon) -> encCipherId!=0 -> CreateEncryptors err -> Mode=="required").
- **Why it matters:** mixes 3 orthogonal decisions (which preauth hash, whether to sign, whether/how to encrypt) in one deeply nested body — hard to reason about any one axis without tracing the whole function.
- **Fix:** extract 3.x branch(~1854-1960) into deriveAndActivateCrypto3x(sess, ctx, sessionKey, dialect, isReauth) *HandlerResult; keep short 2.x branch inline; extract anon-encryption eligibility(~1894-1897) into anonymousEncryptionAllowed(sess, ctx) bool.
- **Verified:** CONFIRMED size/nesting/axes. Reauth-vs-fresh preauth hash(1818-1840), signing gate(1860-1870), cipher select+anon-eligibility+encryptor create(1872-1948), separate 2.x branch(1960-1975). REACHABLE: 3 non-test callers — kerberos_auth.go:176, session_setup.go:1264,:1454,:1723. LOW.

### [LOW] SPNEGO signature-byte sniff duplicated 3x verbatim · `structure` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:316`
- **What:** Identical condition `len(buf) >= 2 && (buf[0]==0x60 || buf[0]==0xa0 || buf[0]==0xa1)` at 316-317 (SessionSetup), 496 (extractNTLMToken), 678-679 (handleSessionBind Kerberos-bind detect). 3 independent copies of same wire-format check.
- **Why it matters:** dup per-message-type detection logic — repo convention wants this collapsed behind one func. 3 copies = 3 places to update if new SPNEGO framing byte needs recognizing.
- **Fix:** Extract `func isSPNEGOToken(buf []byte) bool` once, call from all 3 sites.
- **Verified:** CONFIRMED 3 verbatim copies. All three call auth.Parse on same buffer after. All 3 live handler paths, none test-only. Fix: one `isSPNEGO(b []byte) bool` in session_setup.go, 3 call sites.

### [LOW] SMB2_SESSION_FLAG_BINDING breaks Go MixedCaps naming convention used by its own sibling constants · `structure` · area: handlers-negotiate-session
- **Where:** `internal/adapter/smb/handlers/session_setup.go:53`
- **What:** `const SMB2_SESSION_FLAG_BINDING = 0x01` uses C-style SCREAMING_SNAKE_CASE. Sibling flags types.SMB2SessionFlagEncryptData, types.SMB2SessionFlagIsGuest use Go MixedCaps.
- **Why it matters:** Effective Go: exported ids use MixedCaps, never underscores; the one constant on this surface that breaks it, right next to two that don't.
- **Fix:** Rename to SMB2SessionFlagBinding; update ~9 non-test + ~6 test call sites in handlers pkg (package-local, no cross-pkg break).
- **Verified:** CONFIRMED. Only SCREAMING_SNAKE id in internal/adapter/smb (regex swept whole tree). Siblings MixedCaps at types/constants.go:423-425. Usage: 1 non-test read (session_setup.go:174) + 6 _test.go sites, package-local — rename mechanical. Exported id w/ underscores violates Effective Go.

### [LOW] AP-REQ authentication + user resolution duplicated between handleKerberosAuth and completeKerberosBind · `structure` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:356`
- **What:** Principal derivation 47-51 vs 356-360 identical. AP-REQ extract+Authenticate 57-69 vs 362-372 identical modulo log text. userStore lookup+resolveSessionUser+failure log 92-105 vs 379-391 identical modulo log text.
- **Why it matters:** same authenticate-AP-REQ-then-resolve-user sequence copy-pasted across primary session-setup path and channel-bind path; any future change applied twice, can silently skip one site.
- **Fix:** Extract `h.authenticateKerberosAPReq(ctx, mechToken) (*kerbauth.AuthResult, *models.User, *HandlerResult)` covering principal derivation through resolveSessionUser; both callers branch on result.
- **Verified:** CONFIRMED. Principal derivation 47-51 vs 356-360 byte-identical. AP-REQ extract+Authenticate identical modulo log strings. userStore lookup identical modulo log strings. Both paths live: handleKerberosAuth from session_setup.go:323, completeKerberosBind from handleSessionBind (session_setup.go:680 branch). Fix: one `authenticateAPReq(ctx, mechToken)` helper, 2 call sites.

### [LOW] extractAPReqFromGSSToken hand-rolls the same GSS-unwrap already implemented on the NFS side, in the opposite direction from the shared WrapGSSToken · `structure` · area: handlers-auth
- **Where:** `internal/adapter/smb/handlers/kerberos_auth.go:598`
- **What:** extractAPReqFromGSSToken (598-634) + parseGSSASN1Length (636-656) reimplement nfs/rpc/gss/framework.go's extractAPReq/parseASN1Length near byte-for-byte: same 0x60-tag check, same RFC 2743/1964 refs, same OID-tag/length/token-ID walk, short-form-only length parsing. Comment at :56 acknowledges twin exists but code never shared. Pack direction (WrapGSSToken) for same wire format already shared in internal/auth/kerberos/gsswrap.go, called by both SMB and NFS.
- **Why it matters:** same GSS-API InitialContextToken framing decoded twice in two adapter trees instead of once in the pkg that already owns encode side — a wire-format fix has two call sites, only one in scope here.
- **Fix:** Move extractAPReqFromGSSToken/parseGSSASN1Length into internal/auth/kerberos (gsswrap.go, as UnwrapGSSToken/unwrapASN1Length next to WrapGSSToken/encodeASN1Length), delete SMB-local copy, kerberos_auth.go calls shared func (NFS's own copy out of scope but should switch too).
- **Verified:** CONFIRMED, and DRIFT ALREADY PRESENT. Same 0x60 tag check, same short-form-only OID length walk, same GSSTokenIDAPReq compare, same error wrap string. Already disagree: SMB rejects long-form OID length (kerberos_auth.go:621-623), NFS silently misreads it as short (framework.go:199). Encode side already shared at gsswrap.go (WrapGSSToken), called both trees. REACHABLE both sides: framework.go:87 calls extractAPReq; kerberos_auth.go:57 and :362 call extractAPReqFromGSSToken. Correction: SMB fn has NO doc comment (only NFS carries RFC 2743/1964 citations).

### [LOW] TREE_CONNECT response encoding duplicated between TreeConnect and handleIPCShare · `structure` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_connect.go:208`
- **What:** 16-byte TREE_CONNECT response (StructureSize/ShareType/Reserved/ShareFlags/Capabilities/MaximalAccess) built w/ identical smbenc.Writer calls at 208-217 (disk share) and 300-309 (IPC$), differing only in 4 param values.
- **Why it matters:** same wire structure, same field order, 2 copies to keep in sync if response layout ever changes.
- **Fix:** Extract writeTreeConnectResponse(shareType uint8, shareFlags, capabilities, maximalAccess uint32) []byte, call from both TreeConnect and handleIPCShare.
- **Verified:** CONFIRMED. Same 6 smbenc.Writer calls in same order, differing only in 4 values. Both live (dispatch handlers). Fix: `func treeConnectResponse(shareType uint8, flags, caps, maxAccess uint32) []byte`, 2 call sites.

### [LOW] Unowned goroutine per parked LOCK cancelled on TREE_DISCONNECT · `structure` · area: handlers-tree-connect
- **Where:** `internal/adapter/smb/handlers/tree_disconnect.go:71`
- **What:** `go func(p *PendingLock){ p.Callback(...) }(parked)` spawned per pending lock unregistered from tree (68-81), no join, no bound on count, no cancellation tied to goroutine's own lifetime. TreeDisconnect returns success response immediately after loop w/o waiting.
- **Why it matters:** violates "no naked goroutines" rule. Client disconnecting many trees w/ many parked LOCKs (or blocked Callback on slow conn) fans out unbounded, un-joined goroutines w/ no backpressure; TREE_DISCONNECT completing doesn't mean cancellations landed.
- **Fix:** Bound w/ small worker pool or errgroup scoped to ctx.Context (already on request), or at minimum sync.WaitGroup caller can wait on for tests; cap concurrent cancellations rather than one-goroutine-per-lock.
- **Verified:** CONFIRMED, and STRENGTHENED: every sibling parked-LOCK drain calls Callback SYNCHRONOUSLY — close.go:370, logoff.go:215, handler.go:1517 (shared closeFilesWithFilter, own comment says mirrors close.go for "LOGOFF / tree-disconnect / transport drop"). tree_disconnect.go is sole `go func`, no comment justifying it, no lock held across loop. TreeDisconnect returns success at :98 w/o joining — cancels can land after it, smbtorture smb2.lock.cancel-tdis ordering left to scheduler. REACHABLE: live dispatch handler. Fix is subtraction: drop the `go func` wrapper, match the 3 siblings.

### [LOW] Boolean-trap positional args needing an inline comment to be readable · `structure` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/write.go:362`
- **What:** `metaSvc.CheckLockForIO(..., true, // isWrite = true for write operations)` mirrored `false, // isWrite = false...` in read.go:344; likewise `h.purgeConflictingDisconnectedHandlesForDataChange(..., true, // WRITE breaks Read leases to NONE...)` at write.go:396. Every call site needs trailing comment to say what bare bool means.
- **Why it matters:** classic boolean-trap anti-pattern — comment at every call site is itself the symptom param isn't self-documenting.
- **Fix:** Replace bool w/ named type/consts at call sites, e.g. CheckReadLock/CheckWriteLock wrapper methods, or `type IOKind bool; const (ForRead IOKind = false; ForWrite IOKind = true)`.
- **Verified:** Confirmed. write.go:362 and read.go:344 into metadata.Service.CheckLockForIO (pkg/metadata/service.go:752, last param isWrite bool). write.go:396 purgeConflictingDisconnectedHandlesForDataChange same shape. Reachable: live SMB WRITE/READ handlers. Style-only, no spec issue. Fix: split into CheckReadLockForIO/CheckWriteLockForIO or named enum type.

### [LOW] Magic byte-offset constants un-named in WRITE request decode · `structure` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/write.go:120`
- **What:** DecodeWriteRequest uses bare literals 64 (SMB2 header size) and 48 (fixed WRITE body size) 3x each (120,123,126,128,130,132) computing data-buffer start, plus two-tier fallback w/ no named constant tying to spec size.
- **Why it matters:** same magic-number smell as repeated 0x50 DataOffset literal in read.go (5 occurrences). Un-named literals reused across arithmetic easy to get subtly wrong on future edit.
- **Fix:** `const smb2HeaderSize = 64` and `const writeRequestFixedSize = 48` in write.go; `const readResponseDataOffset = 0x50` in read.go. Pure rename, no behavior change.
- **Verified:** Confirmed in DecodeWriteRequest. Bare 64 at `dataStart := int(req.DataOffset) - 64`; bare 48 4x. Pkg already defines smb2HeaderSize = 64 (session_setup.go:65, used at 106/2019) which this file ignores; no const exists for 48-byte WRITE fixed body. read.go 0x50 confirmed at 371,470,539,633,657 (claim's lines ~1-3 off but 5 occurrences exist). Reachable: DecodeWriteRequest is live WRITE decoder. Fix: use smb2HeaderSize, add writeReqFixedSize=48 / readRespDataOffset=0x50.

### [LOW] atime SetFileAttributes errors discarded without logging, inconsistent with sibling flush call in the same function · `structure` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/read.go:453`
- **What:** `_, _ = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &metadata.SetAttrs{Atime: &now})` drops error entirely. Same shape write.go:535/538. Contrast write.go:494-496 same function: analogous best-effort FlushPendingWriteForFile error caught+logged at Debug.
- **Why it matters:** repo's own error-handling convention treats silently-dropped error as bug-adjacent; expected errors should log at Debug — these 3 sites don't even log, just `_, _ =`.
- **Fix:** `if _, err := metaSvc.SetFileAttributes(...); err != nil { logger.Debug("READ: atime update failed (non-fatal)", "path", path, "error", err) }` — matches pattern 2 lines away for FlushPendingWriteForFile.
- **Verified:** Confirmed. read.go:453; same shape twice in write.go (~535 file atime, ~538 parent atime). Contrast write.go:494-496 same function: flushErr caught+logged Debug. Reachable: live READ/WRITE paths. Behaviour intentionally best-effort so no spec issue; finding is the inconsistent silence. Fix: log at Debug like sibling.

### [LOW] Repeated `&ReadResponse{SMBResponseBase{Status: X}}` / `&WriteResponse{...}` error-response boilerplate · `structure` · area: handlers-read-write
- **Where:** `internal/adapter/smb/handlers/read.go:169`
- **What:** 16 call sites read.go + 10 write.go construct `&ReadResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusX}}` / Write equivalent inline, each preceded by own logger.Debug/Warn call.
- **Why it matters:** not wrong, just boilerplate — every early-return is 3+ lines of struct-literal ceremony a helper would collapse to one line, same shape at ~26 sites across 2 files.
- **Fix:** `func readErr(status types.NTStatus) (*ReadResponse, error) { return &ReadResponse{SMBResponseBase: SMBResponseBase{Status: status}}, nil }` (+ Write equivalent) — call sites become `return readErr(types.StatusFileClosed)`. Optional cleanup, low value alone; worth doing only if touching file for other reason.
- **Verified:** Confirmed and undercounted: read.go = 21 sites, write.go = 22 sites (claim said 16/10). Each early return 3-line struct-literal preceded by own logger.Debug/Warn. Reachable: live handlers. Pure boilerplate, no correctness issue. Fix: `func readErr(s types.Status) *ReadResponse` / `writeErr(...)` one-liners.

### [LOW] fileID/openFile resolve-and-reject boilerplate repeated 4x in ioctl_fsctl.go · `structure` · area: handlers-ioctl-core
- **Where:** `internal/adapter/smb/handlers/ioctl_fsctl.go:17`
- **What:** handleGetCompression (17-26), handleSetCompression (52-61), handleMarkHandle (195-204), handleQueryFileRegions (219-228) all open w/ identical 8-line pair: parseIoctlFileID -> StatusInvalidParameter, h.GetOpenFile -> StatusFileClosed. buildObjectIDResponse already factors this out for its 2 callers.
- **Why it matters:** ~32 dup lines pure boilerplate across file; every new handle-scoped FSCTL added copies it again.
- **Fix:** Add `func (h *Handler) resolveIoctlOpenFile(body []byte) (*OpenFile, [16]byte, *HandlerResult)` returning non-nil HandlerResult on failure; 4 sites do `openFile, fileID, errResult := h.resolveIoctlOpenFile(body); if errResult != nil { return errResult, nil }`.
- **Verified:** Confirmed verbatim 4x. buildObjectIDResponse (ioctl_fsctl.go:160-169) contains identical pair, already shared by its 2 callers (148,154), proving factoring exists. Reachable: all 4 wired in live dispatch table (ioctl_dispatch.go:32,33,38,39). Fix: `func (h *Handler) resolveIoctlHandle(body []byte) ([16]byte, *OpenFile, *HandlerResult)`.

### [LOW] Fixed IOCTL request header decoded ad hoc in 4 different places with unexplained magic offsets · `structure` · area: handlers-ioctl-core
- **Where:** `internal/adapter/smb/handlers/ioctl_dispatch.go:224`
- **What:** Same fixed IOCTL-request header (StructureSize/Reserved/CtlCode/FileId/InputOffset/InputCount/MaxInputResponse/OutputOffset/OutputCount/MaxOutputResponse) walked by 4 separate hand-written funcs w/ bare int offsets, no shared struct: parseIoctlFileID (ioctl_dispatch.go:224-231, offset 8), parseIoctlMaxOutputSize (ioctl_fsctl.go:273-283, offset 44), parseIoctlInputData (ioctl_fsctl.go:285-305, offsets 24/28), inline re-parse in ioctl_validate_negotiate.go:58-78 (offset 28, hardcoded 56). Ioctl() itself reads CtlCode w/ own `r.Skip(4); r.ReadUint32()`.
- **Why it matters:** same class as VALIDATE_NEGOTIATE parser drift finding: 5 independent readers of one wire layout, each trusting own arithmetic. Future header-field addition means auditing 5 call sites instead of 1.
- **Fix:** One `type ioctlHeader struct {...}` + one `parseIoctlHeader(body []byte) (ioctlHeader, bool)` built w/ smbenc.Reader; every other helper becomes field access on parsed struct.
- **Verified:** All 5 readers confirmed. Guards already disagree (48 vs 32 vs 24 minimums) — that inconsistency is the smell. Reachable: all on live IOCTL path. Fix: one decodeIoctlRequest returning a struct.

### [LOW] Issue number leaked into source comments (twice, same file) · `structure` · area: handlers-ioctl-core
- **Where:** `internal/adapter/smb/handlers/ioctl_dispatch.go:52`
- **What:** Line 52: "See issue #436 and the constant comment in stub_handlers.go." Line 122 repeats: "See the constant doc in stub_handlers.go for the rationale (issue #436)". Neither is a `ponytail:` marker (sole sanctioned exception).
- **Why it matters:** CLAUDE.md "Code comments" convention: comments describe behaviour only; issue/PR numbers belong in commit messages or .planning/, not source — tracker refs rot when tracker does.
- **Fix:** Rewrite both to state behavioural rule directly (smbtorture's multichannel.leases.test2 keys off this FSCTL's NTSTATUS to unblock cleanup, described once at handler in stub_handlers.go), drop "#436" token from both.
- **Verified:** Confirmed literally at both lines. Neither carries ponytail: prefix, so neither falls under sanctioned exception. Reachable: comments sit on live dispatch table and handleSmbtortureForceUnackedTimeout. Caveat: convention violated pkg-wide not just here — ~104 issue-ref-shaped comments in non-test files in this pkg, so fixing only these 2 is cosmetic. Fix: drop issue numbers, keep behavioural rationale.

### [LOW] DecodeFileRenameInfo and DecodeFileLinkInfo are the same 20-byte decoder duplicated verbatim · `structure` · area: handlers-set-info
- **Where:** `internal/adapter/smb/handlers/set_info.go:148`
- **What:** DecodeFileRenameInfo (148-174) and DecodeFileLinkInfo (2486-2507) both decode: byte 0 -> ReplaceIfExists, bytes 8-16 -> RootDirectory, bytes 16-20 -> FileNameLength (LE u32), bytes 20+ -> UTF-16LE FileName, identical length checks + error messages modulo struct type. FILE_RENAME_INFORMATION and FILE_LINK_INFORMATION share this layout per MS-FSCC 2.4.42.2 / 2.4.28.2.
- **Why it matters:** per-class wire decoders that could collapse behind one shared func are a named dup finding for this file.
- **Fix:** One decodeRenameLikeInfo(buffer) (bool, [8]byte, string, error) helper; DecodeFileRenameInfo and DecodeFileLinkInfo become 3-line wrappers assigning into respective struct types.
- **Verified:** Confirmed identical decoders. Only leading message string and struct type differ. Reachable outside tests: set_info.go:685 (rename) and set_info.go:2536 (link). Layouts genuinely coincide per MS-FSCC, collapsing spec-safe. Fix: one decodeRenameLikeInfo returning (replaceIfExists, rootDir, name).

### [LOW] EncodeCreateContexts hardcodes SMB2 header size inline instead of the existing smb2HeaderSize constant · `structure` · area: handlers-lease-oplock
- **Where:** `internal/adapter/smb/handlers/lease_context.go:603`
- **What:** `offset := uint32(64 + 88) // SMB2 header + CREATE response fixed fields` — 64 duplicates smb2HeaderSize already defined session_setup.go:65 and tested (session_setup_test.go:1174); 88 (CREATE response fixed size) has no named constant anywhere.
- **Why it matters:** pkg already establishes named-size-constant convention (smb2HeaderSize, sdHeaderSize, aclHeaderSize, aceHeaderSize in security.go) specifically to avoid this; this line is the one place in scope not following it.
- **Fix:** Reuse smb2HeaderSize, add `const createResponseFixedSize = 88` next to it (or beside this func) so offset reads `uint32(smb2HeaderSize + createResponseFixedSize)`.
- **Verified:** Confirmed. smb2HeaderSize=64 defined session_setup.go:65 in same pkg (used session_setup.go:106,2019, pinned by session_setup_test.go:1174). No const for 88-byte CREATE response fixed size. Reachable: EncodeCreateContexts called from create.go:439 (non-test). Caveat: "one place in scope not following it" overstated — write.go's DecodeWriteRequest hardcodes 64 same way. Fix: smb2HeaderSize + new createRespFixedSize=88.

## Implementation gaps vs reference

Comparator: Samba `smbd` (source3/smbd/smb2_server.c, smb2_ioctl.c, share_access.c, scavenger.c) + MS-SMB2/MS-FSCC/MS-DTYP specs. Rows = confirmed `gaps` findings mapped to checklist items where one exists.

| Expected behavior (ref) | Our behavior | Gap | Source |
|---|---|---|---|
| TreeConnect scoped per-Session (MS-SMB2 §3.3.5.2.11); TreeId lookup MUST verify Session ownership | `tree_disconnect.go:39` `GetTree(ctx.TreeID)` no SessionID check; same hole in shared NeedsTree gate `response.go:514-520`. Global sync.Map, sequential TreeID counter (`handler.go:53-54`) | any session can enumerate small TreeIDs, DELETE another session's tree, force-complete its parked LOCKs (cross-tenant DoS/hijack) | github.com/samba-team/samba source3/smbd/share_access.c |
| Per-share `SMB2_SHAREFLAG_ENCRYPT_DATA` enforceable indep. of global/session encryption | `response.go:686-692` `checkEncryptionRequired`: IsNull/IsGuest returns 0 (allowed) BEFORE per-share check at 705-712 | guest/null client does plaintext I/O on `EncryptData=true` share in default "preferred" mode | learn.microsoft.com/.../ms-smb2/5606ad47-5ee0-437a-817e-70c366052962 |
| Compound related-chain FileId/SessionId/TreeId inheritance correct at every position | CREATE never checks `ctx.NextCommand` before parking on lease break (`create_post_break.go`, unlike `lock.go:532`); interior-parked CREATE leaves trailing related cmds running on stale `lastFileID`; resume goroutine blocks forever on `<-pending.started` (`create_post_break.go:1640`, no timeout) | wrong status in-frame for trailing subcommands + goroutine/async-credit leak | lists.samba.org/archive/cifs-protocol/2017-July/003064.html |
| `FSCTL_VALIDATE_NEGOTIATE_INFO`: MaxOutputResponse < 24 → MUST terminate transport connection | `ioctl_validate_negotiate.go:43` never reads MaxOutputResponse (req offset 44); always returns 24-byte STATUS_SUCCESS | undersized-buffer MUST violated on 3.0/3.0.2 (3.1.1 already drops) | learn.microsoft.com/.../ms-smb2/0b7803eb-d561-48a4-8654-327803f59ec6 |
| FILE_RENAME/LINK_INFORMATION: nonzero RootDirectory MUST fail STATUS_INVALID_PARAMETER (network op) | `set_info.go:866-873` (rename) + `:2561-2569` (link): same-dir fallback using `path.Base(newPath)`, returns STATUS_SUCCESS | silent rename/link into wrong (own-parent) dir instead of required error | - (MS-SMB2 §3.3.5.21.1 / MS-FSCC 2.4.42.2, not in ref checklist) |
| AppInstanceId failover close: MUST match target PathName + Share + differing ClientGuid, then require GENERIC_READ maximal access before closing | `durable_context.go:907-1038` `ProcessAppInstanceId`: filters on `AppInstanceId` alone, global `handler.files` scan, no path/share/ClientGuid/access check | CREATE for file A can force-close unrelated file B's open (same VM AppInstanceId, any share); no perm check = cross-tenant force-close DoS | - (MS-SMB2 §3.3.5.9.13, not in ref checklist) |
| Inbound SD/ACE offsets bounds-checked before dereference, no panic on malformed input | `security.go:911-929` `parseACEs`: checks `offset+aceSize > len(data)` but not `aceSize >= 8`; `data[offset+8:offset+aceSize]` panics (low>high) on aceSize 0-7 | malformed SET_INFO security ACE crashes the request goroutine (recovered by `connection.go:661` — request just hangs, not full-server crash as first suspected) | learn.microsoft.com/.../ms-dtyp/cca27429-5689-4a16-b2b4-9325d93e4ba2 |
| CreditCharge validated against actual payload size for every message (READ/WRITE/IOCTL/QUERY_DIRECTORY), incl. compounded ones | `ValidateCreditCharge` called only for standalone (`response.go:142`) and compound command #1 (`compound.go:98`); trailing subcommand loop (`compound.go:130-152`) does `SequenceWindow.Consume` but never validates charge | compound position 2+ can declare undersized CreditCharge with oversized payload, bypassing the check every other path enforces | learn.microsoft.com/.../ms-smb2/0edce9a0-766e-41aa-a3b7-2044defbbb8f |
| CHANGE_NOTIFY going async only legal as LAST command of a compound (else STATUS_INTERNAL_ERROR) | `compound.go:953` gates this only for trailing subcommands; first-command dispatch (`:167-231`) unguarded — async-first branch wires PendingCreateRegistry (wrong registry for notify) as no-ops, then `return`s | CHANGE_NOTIFY as command #1 of an illegal multi-cmd compound silently orphans every trailing subcommand's response instead of one sync INTERNAL_ERROR | lists.samba.org/archive/cifs-protocol/2017-July/003064.html |
| IOCTL CreditCharge validated against MaxOutputResponse (req offset 44) | `credit_validation.go:98` `extractPayloadSize` reads offset 28 (InputCount), mislabeled MaxOutputResponse; own test fixture written to same wrong offset | check near-inert for common small-input/large-output IOCTLs (COPYCHUNK, snapshot enum); can also spuriously reject large-input/small-output requests | learn.microsoft.com/.../ms-smb2/0edce9a0-766e-41aa-a3b7-2044defbbb8f |
| QUERY_DIRECTORY CreditCharge validated against OutputBufferLength (req offset 28) | `credit_validation.go:106` reads offset 4 (FileIndex), mislabeled OutputBufferLength; FileIndex is 0 unless SMB2_INDEX_SPECIFIED | check collapses to 1-credit floor in the common case — inert | learn.microsoft.com/.../ms-smb2/0edce9a0-766e-41aa-a3b7-2044defbbb8f |
| AES-CCM/GCM TRANSFORM_HEADER nonce MUST NOT be reused under same key (hard guarantee) | `encryption/middleware.go:188` `EncryptResponse`: fresh `crypto/rand` nonce per message, no per-session counter (sibling `signing/gmac_signer.go` already does deterministic counter-based nonce correctly) | only probabilistic uniqueness; CCM (88-bit nonce) fails catastrophically on any reuse — non-conformant even though practically low-risk at realistic session lifetimes | learn.microsoft.com/.../ms-smb2/da4e579e-02ce-4e27-bbce-3fc816a3ff92 |
| TREE_CONNECT MUST fail STATUS_ACCESS_DENIED when Share.EncryptData=TRUE but Connection.Dialect not 3.x | `tree_connect.go:316-321` `shouldRejectUnencryptedTreeConnect` checks only global `encryptionMode=="required"`, never `sess.Dialect`; `tree.EncryptData` still set (~:171) | 2.x client TREE_CONNECTs successfully to encrypted share, then every subsequent request dies at `checkEncryptionRequired` — dead tree instead of TREE_CONNECT itself failing | learn.microsoft.com/.../ms-smb2/5606ad47-5ee0-437a-817e-70c366052962 |
| WRITE updates Open.CurrentByteOffset (MS-FSA §2.1.5.4 step 21), symmetric to READ | `write.go`: zero PositionInfo/CurrentByteOffset writes anywhere; `read.go` does it via `recordReadProgress` on every success path | `FilePositionInformation`/FILE_ALL offset 80 after a WRITE returns stale pre-write offset | - (MS-FSA §2.1.5.4, not in ref checklist) |
| IOCTL request Flags != SMB2_0_IOCTL_IS_FSCTL MUST fail STATUS_NOT_SUPPORTED before CtlCode dispatch | `ioctl_dispatch.go:64` `Ioctl()` never reads Flags (req offset 48), dispatches straight on CtlCode | non-FSCTL-flagged IOCTL with a known FSCTL code is processed instead of rejected | github.com/samba-team/samba source3/smbd/smb2_ioctl.c |
| Lease Break Acknowledgment (StructureSize 36) body has LeaseKey at offset 8, not FileId — distinct from Oplock Break Ack (StructureSize 24) | `compound.go:714` `fileIDOffset(SMB2OplockBreak)` unconditionally returns 8, no StructureSize check; `ExtractFileID`/`InjectFileID` treat LeaseKey as FileId during FileID inheritance | compounded lease-break-ack: LeaseKey read/overwritten as a fake FileID, corrupting the ack or misrouting a related subcommand | - (MS-SMB2 §2.2.24.1/.2, not in ref checklist) |
