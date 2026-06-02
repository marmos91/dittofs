# smbtorture Known Failures — Kerberos (smb2.session)

Last updated: 2026-06-02

Tests listed here are expected to fail when running `smb2.session` with
`--use-kerberos=required`. Only NEW failures (not in this list) will cause
CI to fail. The `parse-results.sh` script reads test names from the first
column of the table below.

History: this list once carried 65 rows. After multi-channel session bind
shipped (#361) the bulk of them began passing under Kerberos; 36 stale rows
were harvested on 2026-06-01 (reconnect1/2, reauth1-4, the anonymous and
AES-128 signing/encryption tests, ntlmssp_bug14932, and 18 multi-channel
`bind_negative_*` rows). On 2026-06-02 the four CMAC↔HMAC
`bind_negative_smb3sign{CtoH,HtoC}{s,d}` rows were fixed (#686): a re-bind on a
connection that already owns a bound channel is now rejected with ACCESS_DENIED
instead of silently replacing the channel. The remaining row is the genuine
Kerberos-path expectation under #686 (v1.0 Kerberos sweep).

## Kerberos Session Bugs (Fix In Progress)

These are genuine Kerberos-specific bugs tracked in #340 / #686.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.reauth5 | Reauth | Upstream Samba selftest known-fail (selftest/knownfail.d/ line 213): the test asserts `smb2_util_unlink` of a nonexistent file returns OK, but a correct server returns OBJECT_NAME_NOT_FOUND. Not a DittoFS bug — reauth1-4 (key retention across reauth) pass. | #340-A2 |
