---
status: resolved (fix applied, pending CI verification)
trigger: "5 residual 'Bad SMB2 (sign_algo_id=2) signature' failures on fix/issue-367-362-smb-flaky-tests after the primary stash race was fixed in f76290e5 (#362)"
created: 2026-04-16
updated: 2026-04-16
---

## Summary

Primary fix f76290e5 eliminated the per-connection SESSION_SETUP stash race.
CI run 24482922640 still showed 5 bad-signature failures on unrelated tests:
`acls.OWNER-RIGHTS-DENY1`, `bench.session-setup`,
`delete-on-close-perms.FIND_and_set_DOC`,
`ioctl.dup_extents_src_is_dest_overlap`, `replay.channel-sequence`. All 5
fire on the very first signed response of a fresh connection (SESSION_SETUP
SUCCESS) so the client closes with NT_STATUS_ACCESS_DENIED without doing a
TREE_CONNECT.

## Root cause

Per-session preauth hash chain diverged between server and client due to
ordering of after-hook vs next-request dispatch on a single connection.

**Sequence (session 30, connection 127.0.0.1:34038, run 24482922640):**

```
G1 (msgID=1 SESSION_SETUP / CHALLENGE):
  line 10824  InitSessionPreauthHash(30, ssReq1)     chain: H0 → ssReq1
  line 10827  SendMessage (writes ssResp1 to wire)
  line 10840  AFTER-hook runs: chain += ssResp1

G2 (msgID=2 SESSION_SETUP / AUTH) — dispatched by ProcessSingleRequest
goroutine as soon as the client's AUTH request arrives:
  line 10831  BEFORE-hook runs: chain += ssReq2       ← RACE: runs BEFORE
                                                        G1's after-hook at
                                                        line 10840
  line 10841  NTLM key derivation reads chain         ← uses wrong hash:
                                                        (H0, ssReq1, ssReq2)
                                                        client expects
                                                        (H0, ssReq1, ssResp1, ssReq2)
```

Server-derived signing key ≠ client-derived signing key → client verifies
and rejects the signed SESSION_SETUP SUCCESS (72cfeccc on server, client
expected bfbed68e per smbtorture-output.txt:1183-1184).

Proof from the same log: session 300 (later in the run) shows the correct
ordering (line 107050 "Per-session preauth hash updated with response"
BEFORE line 107052 "SESSION_SETUP preauth before-hook" for ssReq2) and does
not fail. The race window is small (microseconds between wire write and
after-hook) and timing-dependent.

The after-hook ran in `SendResponseWithHooks` AFTER `SendMessage` returned,
which is AFTER the wire write. Since `pkg/adapter/smb/connection.go:255`
dispatches every request in its own goroutine, the client could already have
received ssResp1 and sent its AUTH request, and the server could have
already dispatched G2, all before G1's after-hook ran.

## Fix (response.go)

Introduce a `preWrite` callback on a new internal `sendMessage` helper. The
preauth hash update (via `RunAfterHooks`) now runs from this hook, so the
chain is advanced in the window where the client cannot possibly have
observed the wire bytes (write hasn't happened yet).

```go
func SendResponseWithHooks(...) error {
    respHeader, body := buildResponseHeaderAndBody(...)
    preWrite := func(wirePlaintext []byte) {
        RunAfterHooks(connInfo, reqHeader.Command, wirePlaintext)
    }
    return sendMessage(respHeader, body, connInfo, preWrite)
}
```

`sendMessage` calls `preWrite(smbPayload)` after signing (if any) but before
`WriteNetBIOSFrame`. For encrypted sessions the hook runs on the plaintext
payload before encryption — the preauth chain hashes plaintext on both
sides.

Bonus fix: the old `rawResponse := append(respHeader.Encode(), body...)`
call in `SendResponseWithHooks` reconstructed the header from struct
fields, so `hdr.Flags` lacked the `SMB2_FLAGS_SIGNED` bit (signer sets the
bit on `smbPayload` but doesn't write through to the struct). The hash
chain could chain bytes that differed from the wire form in the flags
field. Using `smbPayload` directly in the hook removes that inconsistency.

## Verification

- `go build ./...` clean.
- `go test -race -count=1 -timeout 120s ./internal/adapter/smb/...` passes
  (14 packages, no regressions).
- Conformance suite `crypto_state_conformance_test.go` still covers the
  hash chain invariants.

CI verification pending (push + run 24482922640-style full smbtorture).

## Temp instrumentation still active (revert before merge)

See `.planning/debug/smb-scan-find-signing-flake.md` continuation notes:
- signing.SignMessage SIGN_TRACE + self-verify
- session.SignMessage entry trace
- SendMessage session lookup MISS ERROR trace
- encryption middleware ENCRYPT_TRACE
- test/e2e/smb3_signing_stress_test.go
- `.github/workflows/smb-conformance.yml` "Ensure dittofs.log captured"
