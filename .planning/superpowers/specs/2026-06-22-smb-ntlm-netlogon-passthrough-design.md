# SMB NTLM passthrough via NETLOGON (MS-NRPC) — Design

- **Issue:** #1314
- **Umbrella:** #1231 (AD/LDAP enterprise integration)
- **Date:** 2026-06-22
- **Deferred trackers:** #1323 (online-join), #1324 (DNS SRV DC discovery), #1325 (hot-reload)

## Problem

Mounting an SMB share as an **AD domain user over NTLM** fails:

```
net use S: \\SERVER\demo /user:DITTOFS\alice TestPassword01!
System error 86: The specified network password is not correct.
```

`completeNTLMAuth` (`internal/adapter/smb/handlers/session_setup.go:1093`) validates the
NTLMv2 response against a **local** NT hash in the control-plane user store
(`auth.ValidateNTLMv2Response`). An AD domain user has no local account and no local NT
hash, so the lookup fails → `STATUS_LOGON_FAILURE`.

NTLMv2 SESSION_SETUP gives the server only the challenge/response. Verifying it requires the
user's NT hash, which lives only on the DC. There is no LDAP/Kerberos shortcut (LDAP
simple-bind needs the cleartext password; Kerberos is a different protocol). A domain user's
NTLM logon **must** be delegated to a DC over the NETLOGON secure channel
(`NetrLogonSamLogonEx`). This is what Samba `winbindd` and every AD-member NAS does.

Today, AD domain auth works only via Kerberos/SPNEGO (the keytab path). This design adds the
NTLM path for legacy NTLM-only clients.

## Goals / Non-goals

**Goals**
- AD domain users authenticate over SMB NTLM via NETLOGON passthrough to a DC.
- Reuse the existing post-identity plumbing (#1317) so a user maps to the same UID/GID across
  NFS-krb5, SMB-krb5, and SMB-NTLM.
- Offline machine-credential acquisition (consistent with the current keytab posture).
- Internal seam (`MachineCredentialProvider`) so online-join (#1323) slots in later with no
  rework of the secure channel / authenticator.

**Non-goals (tracked separately)**
- Online `net ads join` + machine-password rotation → **#1323**.
- DNS SRV DC auto-discovery → **#1324**.
- Hot-reload of the machine credential without restart → **#1325**.

## Why the post-auth half is already built

NTLM hands `completeNTLMAuth` the exact bytes a DC needs:
- `authMsg.NtChallengeResponse` / `authMsg.LmChallengeResponse` — client proof
- `pending.ServerChallenge` (`[8]byte`) — the challenge we issued in the Type-2 message
- `authMsg.Username`, `authMsg.Domain`, `authMsg.NegotiateFlags`

After a DC validates, it returns a `SessionBaseKey` plus the user RID and group RIDs (relative
to the domain SID) — the same identity content the Kerberos PAC carries. From there the code
reuses the existing path:
- `synthUserFromResolved` / resolver → UID/GID (the #1317 directory-resolved-user path)
- `CreateSessionWithUser` + `SetPACIdentity(groupSIDs, userSID)`
- `configureSessionSigningWithKey` (SP800-108 KDF) seeded with the DC `SessionBaseKey`

Only the middle — DCE/RPC bind → schannel → `NetrLogonSamLogonEx` — is missing.

## Architecture

New package `internal/auth/netlogon` (sibling to `internal/auth/kerberos`). Three units:

### `MachineCredentialProvider` (interface)
```go
type MachineCredential struct {
    AccountName string // e.g. "DITTOFS$"
    NTHash      [16]byte
    // or KeytabPath for keytab-derived secret
}

type MachineCredentialProvider interface {
    Credential(ctx context.Context) (*MachineCredential, error)
}
```
One implementation now: `offlineCredential`, sourced from config/DB. Online-join (#1323) is a
second implementation; `SecureChannel` / `Authenticator` are unaffected.

### `SecureChannel`
Wraps the go-msrpc `dcerpc` runtime + schannel over `\PIPE\netlogon` (ncacn_np), or
ncacn_ip_tcp.
- `NetrServerReqChallenge` — exchange client/server challenges.
- `NetrServerAuthenticate3` — prove the machine NT hash; derive the netlogon session key;
  negotiate `NETLOGON_NEG_SUPPORTS_AES` + authenticated/sealed RPC (`AuthTypeNetLogon = 0x44`).
- Holds the netlogon credential + sequence state; **mutex-guarded**, **cached and reused**
  across logons; **re-established** on any RPC/seal error. Lazy-initialised on first
  passthrough.
- **Sign+seal AES is mandatory** — post-ZeroLogon DCs reject unprotected secure channels.

### `Authenticator`
```go
type LogonResult struct {
    SessionBaseKey [16]byte
    UserRID        uint32
    GroupRIDs      []uint32
    DomainSID      string
}

func (a *Authenticator) NetworkLogon(
    serverChallenge [8]byte, user, domain string, ntResp, lmResp []byte,
) (*LogonResult, error)
```
Calls `NetrLogonSamLogonEx` with `NETLOGON_NETWORK_INFO` carrying the original server
challenge + client responses. Maps `NETLOGON_VALIDATION_SAM_INFO4` → `LogonResult`.

## Config & data model

Extend the **existing kerberos** `IdentityProviderConfig` (no new provider type). New
`machine_account` sub-block:

```yaml
machine_account:
  enabled: bool
  account_name: string    # default "<netbios-host>$"
  secret: string          # machine password — WRITE-ONLY; redacted in API
  # or keytab_path: string
  dc_address: [string]     # static DC host(s) for the first cut
```

- **Precedence (mirrors #1311):** file/env **seeds** first boot; **DB row wins** thereafter.
- **Secret handling:** write-only, same redacting-MarshalJSON / `MarshalStored` bypass as the
  LDAP bind password.
- **Import-cycle crux (from #1311):** `pkg/config` imports `controlplane/api`, so SMB handlers
  cannot import `pkg/config`. The handler owns a plain DTO; `cmd/dfs` maps DTO →
  `config.KerberosConfig`. The new fields follow the same DTO pattern.
- **dfsctl:** `dfsctl identity-provider kerberos` gains the machine-account fields.
- **Apply on restart** for the first cut (same as kerberos today). Hot-reload → #1325.
- **DC discovery:** static `dc_address` now. DNS SRV → #1324.

## Auth integration & identity flow

Single injection point — `completeNTLMAuth` (`session_setup.go:1093`):

1. **Local user + NT-hash path runs first** (unchanged — preserves behaviour and perf).
2. On **local miss** and passthrough **enabled**: call `Authenticator.NetworkLogon` with the
   bytes already in hand.
3. On DC success: construct user SID + group SIDs from `DomainSID` + RIDs, then run the
   identity through the existing resolver/LDAP idmap to get UID/GID — reusing #1317's
   `synthUserFromResolved`.
4. Build the session with the DC `SessionBaseKey` (feeds `configureSessionSigningWithKey`) +
   `SetPACIdentity(groupSIDs, userSID)`.
5. **Fail closed:** DC unreachable / not configured / validation failure → current
   `STATUS_LOGON_FAILURE`, unchanged.

## Security

- Schannel sign+seal AES mandatory (post-ZeroLogon).
- Machine secret at-rest posture matches keytab/bind-password (plaintext-at-rest, redacted in
  API). Document the operational sensitivity.
- DittoFS exposes **no new inbound wire surface** — it is an outbound NETLOGON client to the DC.

## Testing

Gated on the existing `ad_dc` Samba fixture (`test/integration/ad-dc`, `.github/workflows/ad-dc.yml`):

- **Fixture extension:** provision a DittoFS machine account
  (`samba-tool computer create` + export the machine secret) in the entrypoint.
- **Integration (`ad_dc` tag):** real NTLMv2 logon for `alice` through `completeNTLMAuth`
  → asserts passthrough succeeds → `uid10001` + expected group SIDs.
- **E2E acceptance:** `smbclient -m SMB3` forcing NTLM as `DITTOFS\alice` against
  DittoFS-joined-to-DC.
- **pcap corpus (#1237):** capture the NETLOGON exchange (`tshark`) for the diff corpus.
- **Unit:** RID→SID construction, DTO↔config mapping, secret redaction, secure-channel
  re-establish on error.

## Build sequence (separate reviewable commits)

1. go-msrpc dependency + `netlogon` package (`SecureChannel` + `Authenticator`) with unit
   tests against recorded vectors.
2. Config / DTO / DB plumbing + dfsctl + redaction.
3. `completeNTLMAuth` integration + identity reuse (#1317).
4. ad_dc fixture machine-join + integration/e2e tests + docs (flip the "not implemented"
   NTLM caveats in `docs/SMB.md` and `docs/SECURITY.md` to document the passthrough; note the
   offline-credential requirement).

## Key references

- NTLM path: `internal/adapter/smb/handlers/session_setup.go:1093` (`completeNTLMAuth`)
- NTLM impl: `internal/adapter/smb/auth/ntlm.go` (`BuildChallenge`, `ValidateNTLMv2Response`,
  `DeriveSigningKey`)
- Kerberos/PAC path to mirror: `internal/adapter/smb/handlers/kerberos_auth.go`,
  `internal/auth/kerberos/service.go`
- Resolved-identity reuse (#1317): `synthUserFromResolved` / `pkg/identity`,
  `pkg/identity/ldap`
- Config: `pkg/config/config.go` (`KerberosConfig`),
  `pkg/controlplane/models/identity_provider.go`,
  `pkg/controlplane/store/identity_provider.go`
- Fixture / CI: `test/integration/ad-dc/`, `.github/workflows/ad-dc.yml`
- Library: `github.com/oiweiwei/go-msrpc` (`msrpc/nrpc/logon`, schannel)
