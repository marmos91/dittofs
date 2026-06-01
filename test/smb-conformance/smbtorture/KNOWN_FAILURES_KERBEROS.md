# smbtorture Known Failures — Kerberos (smb2.session)

Last updated: 2026-06-01

Tests listed here are expected to fail when running `smb2.session` with
`--use-kerberos=required`. Only NEW failures (not in this list) will cause
CI to fail. The `parse-results.sh` script reads test names from the first
column of the table below.

History: this list once carried 65 rows. After multi-channel session bind
shipped (#361) the bulk of them began passing under Kerberos; 36 stale rows
were harvested on 2026-06-01 (reconnect1/2, reauth1-4, the anonymous and
AES-128 signing/encryption tests, ntlmssp_bug14932, and 18 multi-channel
`bind_negative_*` rows). The remaining rows are the genuine Kerberos-path
bugs under #686 (v1.0 Kerberos sweep).

## Kerberos Session Bugs (Fix In Progress)

These are genuine Kerberos-specific bugs tracked in #340 / #686.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.reauth5 | Reauth | Signing keys wrong after Kerberos reauth | #340-A2 |
| smb2.bind1 | Bind | Kerberos session bind not wired — Phase 1 of #361 implements NTLM bind only | #361 |
| smb2.bind2 | Bind | Kerberos session bind not wired — Phase 1 of #361 implements NTLM bind only | #361 |
| smb2.bind_invalid_auth | Bind | Kerberos session bind not wired — Phase 1 of #361 implements NTLM bind only | #361 |
| smb2.expire1n | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire1s | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire1e | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire2s | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire2e | Expire | Ticket expiration not enforced correctly | #340-A1 |

## Session-Bind Crypto Negotiation (Fix In Progress)

Multi-channel session bind is implemented (#361). These `bind_negative_*`
tests assert that a *second* channel bind which changes the signing or
encryption algorithm relative to the first channel is rejected (MS-SMB2
§3.3.5.5.2). The remaining combinations are not yet enforced correctly on the
Kerberos path — DittoFS does not reject (or returns the wrong status for) the
specific sign/encrypt algorithm transitions below.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.bind_negative_smb3sneGtoGs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3sneGtoGd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3sneCtoCs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3sneCtoCd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signGtoGs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signGtoGd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signCtoCs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signCtoCd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signCtoHs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signCtoHd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signHtoCs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3signHtoCd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3encGtoGs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3encGtoGd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3encCtoCs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3encCtoCd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3encCtoGs | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |
| smb2.bind_negative_smb3encCtoGd | Session bind | Bind crypto-negotiation enforcement incomplete (Kerberos path) | #361 |

## AES-256 Session Encryption (Not Implemented)

DittoFS implements AES-128-CCM and AES-128-GCM but not the AES-256 variants.
The 128-bit variants pass. These fail identically on the NTLM path.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.encryption-aes-256-ccm | AES-256 | AES-256 encryption not implemented | #340 |
| smb2.encryption-aes-256-gcm | AES-256 | AES-256 encryption not implemented | #340 |
