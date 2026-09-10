# Wave 4 fix-wave 2 lane prompts (2026-09-10)

Base: origin/develop at 83e532d78 (fix-wave 1 fully landed: #2523/#2524/#2525/#2526 merged, issues
#2518/#2512/#2514/#2516 closed). Two lanes, file-disjoint, parallel. Both lanes push branches and
write /tmp/pr-body-*.md files with `Closes #N` but do NOT open PRs or run gh.

## Shared boilerplate (both lanes)

- Smallest correct diff. No drive-by refactors.
- Source files are wave/lane/PR-number agnostic (`.planning/CONVENTIONS-WAVE-TEST-NAMES.md`):
  name new test files after the domain they cover, never `wave*`/lane names.
- Signed commits, subject <= 72 chars.
- No issue/PR citations in new code comments — behaviour comments only.
- Verify tier: `go build ./... && go vet ./... && gofmt -l internal/`; `go test -race
  -count=1 ./internal/adapter/smb/...`; red-without-fix proof for every bug fix (revert the fix,
  watch the new test fail, restore).
- Windows/CI gotchas: `renewed.Equal(before)` not `After(before)` when both stamps can fall in one
  clock tick; write polling loops directly in the `for` header (QF1006).
- Push via `git push origin +HEAD:refs/heads/<branch>`.
- Stop condition: `BRANCH= HEAD= PRBODY= STATUS=GREEN|BLOCKED`.

## Lane A — session-auth cluster (branch fix/wave4-session-auth, Closes #2513 AND Closes #2515)

Files owned: `internal/adapter/smb/handlers/session_setup.go`, `kerberos_auth.go`,
`replay_cache.go`, `stub_handlers.go` (oplock break-ack only), `internal/adapter/smb/auth/ntlm.go`,
`spnego.go`, `internal/adapter/smb/session/manager.go`, `credit_validation.go`,
`crypto_state.go`, `internal/adapter/smb/encryption/middleware.go`,
`internal/adapter/smb/lease/manager.go`.

Findings to fix (from #2513 + #2515, re-verify each at its symbol on the fresh base):

1. Anonymous NTLM re-auth bypasses guest policy and is mislabeled Guest —
   `session_setup.go` tryReauthUpdate(pending, "anonymous", "", nil, true) hardcodes isGuest
   true and skips checkGuestPolicy(). Fix: call the guest policy path and set isGuest honestly.
2. tryReauthUpdate / Kerberos re-auth write session identity fields unlocked —
   `session_setup.go` + `kerberos_auth.go`: `sess.User = ...`, `sess.Username = ...`,
   `sess.IsGuest = ...` direct writes; session.go documents these read-only after creation.
   Fix: guard the mutation with the session's lock (established lock order) or route through a
   session-manager mutator.
3. Kerberos bind checks identity by username only — `kerberos_auth.go` inline
   `user.Username != sess.User.Username`; the NTLM path has bindIdentityMatchesSession. Fix: use
   the same identity-comparison helper (UID/GID + username) on the Kerberos path.
4. Kerberos session created before the mechListMIC downgrade check, no rollback on MIC failure —
   `kerberos_auth.go`: CreateSessionWithUserAndExpiry + configureSessionSigningWithKey run before
   auth.VerifyMechListMIC; MIC failure returns StatusLogonFailure with no DeleteSession. Fix:
   delete the session (and unwind signing config) on MIC failure; keep the creation order (it is
   needed for the bind path) but make failure atomic.
5. NTLMv2 client-challenge AV_PAIRs structural-only; Type-3 MIC never verified —
   `auth/ntlm.go`: validateNTLMv2ClientChallenge checks only RespType/HiRespType/length/EOL.
   Fix: parse MsvAvFlags, verify the Type-3 MIC where the negotiate flag advertises it. NOTE:
   the MIC key derivation needs the NTLMv2 session keys — check what AuthenticateMessage
   computes and reuse it.
6. NTLM compute-only mechListMIC — no verify counterpart — `ntlm.go` + `spnego.go`: add
   VerifyNTLMSSPMechListMIC mirroring the compute side; wire it into the SPNEGO mechListMIC
   check for NTLM tokens (spnego.VerifyMechListMIC currently wraps Kerberos only).
7. SessionCryptoState.Destroy is dead code; key material never zeroed on teardown —
   `session/manager.go` DeleteSession body is only `m.sessions.Delete(sessionID)`. Fix: call
   Destroy (zero key material) on delete; keep the mutex contract.
8. IOCTL CreditCharge validation reads the wrong wire field — `session/credit_validation.go`
   reads body[28:32] (InputCount) while parseIoctlMaxOutputSize reads body[44:]. Fix: read the
   correct field for each FSCTL (MaxInputResponse/MaxOutputResponse region), or validate against
   the IOCTL payload length the way the compound loop does.
9. QUERY_DIRECTORY CreditCharge validation reads the wrong field — `credit_validation.go` reads
   body[4:8] (FileIndex); OutputBufferLength is at 28. Fix: use OutputBufferLength.
10. Missing ownership check on oplock break-ack — `handlers/stub_handlers.go`
    handleOplockBreakAck has no VerifyLeaseAckOwnership (sibling has it); openFile.OplockLevel =
    ack.OplockLevel unconditional. Fix: verify the lease-key ownership before applying.
11. Lease pre-registration lost-update races — `lease/manager.go`: success path never re-asserts
    bindings[ck]; restorePreRegistration unconditionally writes stale prev/deletes. Fix:
    compare-and-swap on the bindings map (re-assert under mu; only restore if the current value
    is still the one we granted away).
12. AEAD nonce fully random per message — `encryption/middleware.go`: rand.Read(nonce) per
    message violates MS-SMB2 3.1.4.3 MUST-NOT-repeat. Fix: per-session counter nonce (e.g. 4-byte
    random prefix + 8-byte counter, incremented under the connection crypto lock).
13. LockReplayCache.ForgetFile full scan + wrong bound comment — `handlers/replay_cache.go`:
    full-map scan per CLOSE; stale "16-slot cap" comment vs LockSequenceIndexMax = 64. Fix: keep
    the scan (ponytail if cheap) but correct the comment to the real bound.

Closes #2513 and Closes #2515 in the PR body. Disclose anything deliberately left.

## Lane B — create cluster (branch fix/wave4-create-post-break, Closes #2517)

Files owned: `internal/adapter/smb/handlers/create_post_break.go`, `create.go`,
`internal/adapter/smb/compound.go`, `internal/adapter/smb/handlers/durable_context.go`
(sessionKeyHash dead plumbing only).

Findings to fix (from #2517, re-verify each at its symbol on the fresh base):

1. TOCTOU create-race recovery truncates the winner without breaking its lease/oplock —
   `create_post_break.go` race branch replays recheckExistingFileGates (share-mode+DACL only)
   then h.overwriteFile — no breakAndMaybeParkCreate on the winner. Fix: run the same
   break/park path the non-race overwrite uses so the winner's lease/oplock is broken before
   truncation.
2. CREATE parked mid-compound-chain: trailing commands run against stale FileID, resume goroutine
   blocks forever — `create_post_break.go` parkCreateOnLeaseBreak never checks ctx.NextCommand;
   compound.go defers MarkStarted only for isLastCommand. Fix: when the parked CREATE is not the
   last compound command, either fail the whole compound with a retriable status or park the
   remaining chain (mirror the async-CREATE compound resume pattern at compound.go's
   ReplaceCallback path).
3. ADS base-file existence check swallows lookup error — `create.go`: check the actual code; if
   the lookup error is swallowed (err ignored), surface it as the create status instead of
   treating the base as missing/missing-permission.
4. sessionKeyHash threaded through 4 signatures, never read — `durable_context.go`: dead
   plumbing. Fix: remove the parameter from the 4 signatures (move-only, zero behaviour change).

Closes #2517 in the PR body. Disclose anything deliberately left.

## Orchestrator notes

- Lanes are file-disjoint EXCEPT both may touch `handlers/` test helpers — coordinate test-file
  names so they never collide (Lane A: `session_auth_*_test.go` / `ntlm_mic_test.go` /
  `lease_preregister_test.go`; Lane B: `create_post_break_test.go` additions or a domain-named
  new file).
- Serialized merge order after reviews: A -> B (A is larger; B rebases trivially).
- Three reviews per branch before PR open (correctness, simplification, adversarial); Copilot
  watch; assignee marmos91.
