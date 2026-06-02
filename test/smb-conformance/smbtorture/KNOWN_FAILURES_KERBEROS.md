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
| smb2.reauth5 | Reauth | Upstream Samba selftest known-fail (selftest/knownfail.d/ line 213): the test asserts `smb2_util_unlink` of a nonexistent file returns OK, but a correct server returns OBJECT_NAME_NOT_FOUND. Not a DittoFS bug — reauth1-4 (key retention across reauth) pass. | #340-A2 |

## Session-Bind Crypto Negotiation (Fix In Progress)

Multi-channel session bind is implemented (#361). These `bind_negative_*`
tests assert that a *second* channel bind which changes the signing or
encryption algorithm relative to the first channel is rejected (MS-SMB2
§3.3.5.5.2). The cross-algorithm transition matrix (GMAC↔CMAC↔HMAC, GCM↔CCM)
is already enforced correctly on the Kerberos path — all of those variants
pass. The four rows below are the CMAC↔HMAC case, where the bind itself is
accepted (correct) and the follow-up *unsigned re-auth* on the bound channel
must be rejected. The server now returns ACCESS_DENIED for that re-auth, but the
signed error reply is mis-decoded as STATUS_OK by the smbtorture client under an
encrypt-required/keyless fresh-init — a wire-level response-encoding follow-up
that needs a byte-level pcap diff.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.bind_negative_smb3signCtoHs | Session bind | CMAC→HMAC re-auth: server rejects (ACCESS_DENIED) but signed error reply mis-decoded as OK by client (wire-encoding follow-up) | #686 |
| smb2.bind_negative_smb3signCtoHd | Session bind | CMAC→HMAC re-auth: server rejects (ACCESS_DENIED) but signed error reply mis-decoded as OK by client (wire-encoding follow-up) | #686 |
| smb2.bind_negative_smb3signHtoCs | Session bind | HMAC→CMAC re-auth: server rejects (ACCESS_DENIED) but signed error reply mis-decoded as OK by client (wire-encoding follow-up) | #686 |
| smb2.bind_negative_smb3signHtoCd | Session bind | HMAC→CMAC re-auth: server rejects (ACCESS_DENIED) but signed error reply mis-decoded as OK by client (wire-encoding follow-up) | #686 |
