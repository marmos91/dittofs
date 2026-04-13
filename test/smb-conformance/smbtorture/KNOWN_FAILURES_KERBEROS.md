# smbtorture Known Failures — Kerberos (smb2.session)

Last updated: 2026-04-13

Tests listed here are expected to fail when running `smb2.session` with
`--use-kerberos=required`. Only NEW failures (not in this list) will cause
CI to fail. The `parse-results.sh` script reads test names from the first
column of the table below.

## Kerberos Session Bugs (Fix In Progress)

These are genuine Kerberos-specific bugs tracked in #340.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.reconnect1 | Reconnect | STATUS_ACCESS_DENIED instead of STATUS_USER_SESSION_DELETED | #340-A3 |
| smb2.reconnect2 | Reconnect | STATUS_ACCESS_DENIED instead of STATUS_USER_SESSION_DELETED | #340-A3 |
| smb2.reauth4 | Reauth | Signing keys wrong after Kerberos reauth | #340-A2 |
| smb2.reauth5 | Reauth | Signing keys wrong after Kerberos reauth | #340-A2 |
| smb2.expire1n | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire1s | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire1e | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire2s | Expire | Ticket expiration not enforced correctly | #340-A1 |
| smb2.expire2e | Expire | Ticket expiration not enforced correctly | #340-A1 |

## Multi-Channel Session Bind (Not Implemented)

Multi-channel session binding requires establishing multiple TCP connections
to the same session (MS-SMB2 3.3.5.5.10). DittoFS does not implement
multi-channel. These fail identically on the NTLM path.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.bind_negative_smb3to3s | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3to3d | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneGtoCs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneGtoCd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneGtoGs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneGtoGd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneGtoHs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneGtoHd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneCtoCs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneCtoCd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneCtoGs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3sneCtoGd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signGtoCs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signGtoCd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signGtoGs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signGtoGd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signGtoHs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signGtoHd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signCtoCs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signCtoCd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signCtoGs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signCtoGd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signCtoHs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signCtoHd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signHtoCs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signHtoCd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signHtoGs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3signHtoGd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encGtoCs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encGtoCd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encGtoGs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encGtoGd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encCtoCs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encCtoCd | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encCtoGs | Multi-channel | Multi-channel not implemented | - |
| smb2.bind_negative_smb3encCtoGd | Multi-channel | Multi-channel not implemented | - |

## Anonymous Authentication (Not Supported)

DittoFS does not support anonymous (null) sessions with signing/encryption.
These fail identically on the NTLM path.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.anon-encryption1 | Anonymous | Anonymous sessions not supported | - |
| smb2.anon-encryption2 | Anonymous | Anonymous sessions not supported | - |
| smb2.anon-encryption3 | Anonymous | Anonymous sessions not supported | - |
| smb2.anon-signing1 | Anonymous | Anonymous sessions not supported | - |
| smb2.anon-signing2 | Anonymous | Anonymous sessions not supported | - |

## AES-256 Session Encryption (Not Implemented)

DittoFS implements AES-128-CCM and AES-128-GCM but not the AES-256 variants.
The 128-bit variants pass. These fail identically on the NTLM path.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.encryption-aes-256-ccm | AES-256 | AES-256 encryption not implemented | - |
| smb2.encryption-aes-256-gcm | AES-256 | AES-256 encryption not implemented | - |

## NTLMSSP Bug Compatibility (Not Kerberos)

This test exercises NTLM-specific bug compatibility behavior. Not applicable
to Kerberos auth path.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ntlmssp_bug14932 | NTLM | NTLM-specific test, not applicable to Kerberos | - |
