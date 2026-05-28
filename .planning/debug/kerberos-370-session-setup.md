# #370 — Kerberos smbtorture SESSION_SETUP regression

## TL;DR

All 71 `smb2.session` smbtorture tests fail at initial `smb2_connect` with
`NT_STATUS_LOGON_FAILURE`. No test body executes. The 71-item taxonomy in the
issue body (reconnect/reauth/expire/bind) is misleading — it's *one* upstream
blocker.

Root cause: **PR #345** (centralized identity resolution, merged 2026-04-13)
refactored `internal/adapter/smb/v2/handlers/kerberos_auth.go` and accidentally
dropped four pieces of behaviour required for a working Kerberos SESSION_SETUP.
The Kerberos CI job was not re-run before merge (PR test plan: "Verify SMB
Kerberos auth resolves via centralized resolver" was left unchecked).

The fix keeps #345's centralized identity architecture intact (the `Resolver`
chain, `pkg/identity/`, per-provider scoping) and restores only the dropped
pieces.

## Regression analysis

Diff `git show 1d62855b -- internal/adapter/smb/v2/handlers/kerberos_auth.go`
shows four behaviours removed from `handleKerberosAuth`:

### (1) GSS-API wrapper no longer stripped from `mechToken` — **primary blocker**

Pre-#345:
```go
apReqBytes, err := extractAPReqFromGSSToken(mechToken)
if err != nil { ... }
authResult, err := h.KerberosService.Authenticate(apReqBytes, smbPrincipal)
```

Post-#345 (current):
```go
authResult, err := h.KerberosService.Authenticate(mechToken, smbPrincipal)
```

`KerberosService.Authenticate` at `internal/auth/kerberos/service.go:108` calls
`apReq.Unmarshal(apReqBytes)` directly on its input. It requires a raw AP-REQ,
not a GSS-API initial context token. A SPNEGO mechToken from the client has
shape `0x60 [len] 0x06 <oid-len> <oid-bytes> 0x02 0x00 <AP-REQ>` (RFC 2743
§3.1). Passing that unstripped to `apReq.Unmarshal` fails → `Kerberos
authentication failed` logged at `kerberos_auth.go:43`.

The stripping function `extractAPReqFromGSSToken` is still in the file
(lines 222-258) and still covered by `gss_token_test.go` — it's dead code. PR
#345 removed only the call site.

The NFS path at `internal/adapter/nfs/rpc/gss/framework.go:86` correctly calls
`extractAPReq(gssToken)` before `kerbService.Authenticate`, so NFS is
unaffected.

### (2) AP-REP no longer GSS-wrapped for the SPNEGO response

Pre-#345:
```go
rawAPRep, err := h.KerberosService.BuildMutualAuth(...)
apRepToken = kerbauth.WrapGSSToken(rawAPRep, kerbauth.KerberosV5OIDBytes,
                                   kerbauth.GSSTokenIDAPRep)
```

Post-#345:
```go
apRepToken, err := h.KerberosService.BuildMutualAuth(...)  // raw, unwrapped
```

Even if AP-REQ parsing succeeds, the response AP-REP inside the SPNEGO
accept-complete token is the raw `APPLICATION 15` bytes. **PR #337** (merged
before #345) specifically established that MIT/Heimdal clients reject raw
AP-REP with `GSS_S_DEFECTIVE_TOKEN` — they require the `0x60 [len] OID 0x02
0x00 <AP-REP>` wrapper. The docstring at `internal/auth/kerberos/service.go:177`
currently says "SMB passes raw AP-REP to SPNEGO" — that is outdated and
contradicts #337. Fix includes updating the docstring.

### (3) Per-session SMB 3.1.1 preauth hash no longer initialized

Pre-#345:
```go
if ctx.ConnCryptoState != nil {
    ctx.ConnCryptoState.InitSessionPreauthHash(sessionID)
}
```

(The old call used a one-arg form; signature evolved to
`InitSessionPreauthHash(sessionID, ssRequestBytes []byte)` — see
`internal/adapter/smb/v2/handlers/context.go:65`. The NTLM path already
uses the new form at `session_setup.go:320`.)

Without this call, `configureSessionSigningWithKey` derives signing/encryption
keys from the connection-level preauth hash instead of a fresh per-session
chain. For SMB 3.1.1 clients, the client-side derivation will diverge and the
client rejects the server's signed SUCCESS response.

### (4) `sess.ExpiresAt = <ticket endtime>` no longer set

Pre-#345:
```go
sess := session.NewSessionWithUser(sessionID, ctx.ClientAddr, user, authResult.Realm)
sess.ExpiresAt = authResult.APReq.Ticket.DecryptedEncPart.EndTime
h.SessionManager.StoreSession(sess)
```

Post-#345:
```go
sess := h.CreateSessionWithUser(sessionID, ctx.ClientAddr, user, authResult.Realm)
// no ExpiresAt assignment
```

`CreateSessionWithUser` (`handler.go:846`) calls `NewSessionWithUser` which
does not set `ExpiresAt`. Per-session expiry enforcement added in PR #341
(A1) is therefore dead on the Kerberos path. Zero `ExpiresAt` → `IsExpired()`
returns false forever → the `expire*` smbtorture tests cannot pass even once
the connect blocker is fixed.

Not strictly needed to unblock the connect, but required to restore #341's
functionality, and the PR description of #341 intended these tests to work.

## Emission site mapping

Given the four regressions, the observed failure is `smb2_connect` returning
`NT_STATUS_LOGON_FAILURE`. From the log-string table in the plan:

- **Site `:43-44`** — `Kerberos authentication failed` — fires on regression
  (1). This is the expected live emission today.
- Regressions (2), (3), (4) never get a chance to manifest because (1) fails
  the handler before reaching them.

Once (1) is fixed, (2) and (3) will manifest as client-side rejections of the
server's SESSION_SETUP response (not server-side emissions). (4) is invisible
to smbtorture `connect1/2` but blocks `expire*` tests.

## Fix approach — preserving #345 architecture

The fix is additive to the current `handleKerberosAuth`, not a revert:

1. **Restore `extractAPReqFromGSSToken(mechToken)` call** before `Authenticate`.
   Keep the function where it is. Add one error check.
2. **Restore `kerbauth.WrapGSSToken(apRep, KerberosV5OIDBytes, GSSTokenIDAPRep)`**
   after `BuildMutualAuth`. This matches #337's intent and fixes the MIT
   interop regression.
3. **Restore `ctx.ConnCryptoState.InitSessionPreauthHash(sessionID, ctx.RawRequest)`**
   after session creation (using the new two-arg signature that NTLM already
   uses).
4. **Restore `sess.ExpiresAt = authResult.APReq.Ticket.DecryptedEncPart.EndTime`**
   after `CreateSessionWithUser` returns. There's a data-race nuance from
   #341 (ExpiresAt before StoreSession). `CreateSessionWithUser` calls
   `StoreSession` inside it, so setting `ExpiresAt` after return has a small
   race window. Options:
   - (a) Set `ExpiresAt` on the returned `*Session` after `StoreSession` has
     published it — writer is this goroutine, readers are only future
     requests on this session, so in practice OK but theoretically racy.
   - (b) Add `CreateSessionWithUserKerberos` or extend
     `CreateSessionWithUser` with an optional expiry param so the field is
     set before `StoreSession`.
   - (c) Inline `NewSessionWithUser` + set ExpiresAt + StoreSession, matching
     the pre-#345 structure.
   Recommendation: **(b)** — add an `ExpiresAt` argument to
   `CreateSessionWithUser`, defaulting to zero for NTLM callers. Minimal
   diff, no race window, explicit intent.
5. **Update docstring** at `internal/auth/kerberos/service.go:172-177`:
   - Old: "SMB passes raw AP-REP to SPNEGO"
   - New: "SMB wraps in GSS-API token (0x60 + OID + 0x0200 header) per
     #337 for MIT/Heimdal client compatibility"
   Both NFS and SMB wrap; the docstring's implication that SMB doesn't is
   wrong and led directly to the #345 regression.

All four fixes are restorations; none adds a new code path. The centralized
`identity.Resolver` chain in `resolveKerberosPrincipal` (kerberos_auth.go:207-220)
is untouched.

## Verification plan

- `go test ./internal/adapter/smb/... ./internal/auth/kerberos/... ./pkg/auth/kerberos/...`
  — unit tests. `kerberos_auth_test.go:187` already tests the wrapped AP-REP
  shape; it's currently failing or missing the production call site.
- Full local Kerberos smbtorture run:
  ```
  cd test/smb-conformance/smbtorture
  ./run.sh --kerberos --filter smb2.session --verbose
  ```
  Expected: 3 pass (connect1, connect2, …), 53 known failures per the existing
  `KNOWN_FAILURES_KERBEROS.md`, 0 new — matching PR #341's reported baseline.
  If newly-visible real failures appear, triage per Phase 3.
- Regression check: `./run.sh --profile memory` (NTLM path) must still pass.
- Regression check: NFS Kerberos E2E (`.github/workflows/nfs-kerberos.yml`)
  should be unaffected since the NFS call path was not touched.

## Adjacent findings

- Dead code: `extractAPReqFromGSSToken` (kerberos_auth.go:222-258) kept by
  #345's refactor but no longer called. The fix restores the call so this
  comment resolves itself.
- The outdated docstring in `service.go:172-177` actively misled the #345
  author. Fixing the docstring is part of the fix.
- `gss_token_test.go` and `kerberos_auth_test.go` both exercise the expected
  wire shape but didn't catch the regression because they test helpers in
  isolation, not the composed handler flow. Consider a handler-level test
  that feeds a real wrapped mechToken through `handleKerberosAuth` — scope
  for a follow-up issue, not this PR.
- `BuildMutualAuth` docstring improvement is also a future-proofing against a
  similar regression if a third protocol adapter is added.
