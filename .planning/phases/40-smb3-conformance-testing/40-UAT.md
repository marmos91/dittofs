---
status: testing
phase: 40-smb3-conformance-testing
source: 33-03-SUMMARY.md, 34-02-SUMMARY.md, 35-03-SUMMARY.md, 36-03-SUMMARY.md, 37-03-SUMMARY.md, 38-03-SUMMARY.md, 39-03-SUMMARY.md, 40-05-SUMMARY.md, 40-06-SUMMARY.md
started: 2026-03-03T16:00:00Z
updated: 2026-03-03T18:20:00Z
---

## Current Test

number: 2
name: Message Signing Active
expected: |
  After connecting with SMB 3.x, session signing should be active.
  Server logs should indicate AES-CMAC or AES-GMAC signing for the session.
awaiting: blocked by NTLM auth failure (test 1 sub-issue)

## Tests

### 1. SMB 3.x Dialect Negotiation from Windows 11
expected: Connect from Windows 11 and negotiate SMB 3.x dialect. Server should select SMB 3.1.1 or 3.0.2.
result: pass
notes: Raw SMB2 NEGOTIATE confirmed dialect=0x0311 (SMB 3.1.1) with signing enabled (SecurityMode=0x0001). go-smb2 library also negotiates 3.1.1 successfully.

### 2. Message Signing Active
expected: After connecting with SMB 3.x, run `Get-SmbConnection | Format-List` — SigningEnabled should be True. Server logs should indicate AES-CMAC or AES-GMAC signing is active for the session.
result: issue
reported: "NTLM authentication fails with STATUS_LOGON_FAILURE across all dialects (2.1, 3.0, 3.1.1). NTLMv2 validation fails for all domain variants (WIN-IMBV3BLBFPA, empty, WORKGROUP). Cannot establish authenticated session to verify signing."
severity: blocker

### 3. Basic File Operations (Create, Read, Write, Delete)
expected: On the mounted share, create a file, read it back, overwrite it, verify, then delete it. All operations succeed without errors.
result: [pending]

### 4. Directory Operations (Create, List, Rename, Delete)
expected: Create a directory, create a file inside it, list contents, rename directory, verify, clean up. All succeed.
result: [pending]

### 5. Windows Explorer Navigation
expected: Open Windows Explorer and navigate to share. Directory listing loads. Create folder/file via right-click. Edit in Notepad. File persists.
result: [pending]

### 6. Large File Copy
expected: Copy a file of ~10MB or larger to the share. Copy completes without error. Copy back and verify identical.
result: [pending]

### 7. Concurrent Access (Two Explorer Windows)
expected: Two sessions can see each other's changes. Create from one, visible from other. Delete from one, reflected in other.
result: [pending]

### 8. Reconnect After Disconnect
expected: Files created before disconnect persist after reconnect. New operations work normally.
result: [pending]

### 9. Permission Enforcement
expected: File properties show ACL entries. Owner has full control.
result: [pending]

### 10. Share Disconnect and Cleanup
expected: Share disconnects cleanly. Re-mounting works immediately.
result: [pending]

## Summary

total: 10
passed: 1
issues: 1
pending: 8
skipped: 0

## Gaps

- truth: "Authenticated SMB session can be established with NTLM credentials"
  status: failed
  reason: "NTLMv2 validation fails for all domain variants. go-smb2 client sends AUTHENTICATE with domain=WIN-IMBV3BLBFPA, server tries WIN-IMBV3BLBFPA/empty/WORKGROUP — all fail. Affects ALL dialects (2.1, 3.0, 3.1.1). Server has correct NT hash stored (verified computation). The Handler's completeNTLMAuth uses pending.ServerChallenge from its own PendingAuth store (keyed by SMB session ID). Root cause likely in NTLMv2 response validation — server-computed NTProofStr doesn't match client's."
  severity: blocker
  test: 2
  root_cause: "NTLMv2 ValidateNTLMv2Response fails — HMAC-MD5(NTLMv2Hash, ServerChallenge+ClientBlob) doesn't match the NTProofStr sent by go-smb2. Both sides use HMAC-MD5(NT_hash, UPPER(username_utf16le) + domain_utf16le) for NTLMv2Hash. Possible issues: (1) ServerChallenge mismatch between what was sent in CHALLENGE and what's stored in PendingAuth, (2) SPNEGO wrapping corrupts the NTLM token bytes, (3) ClientBlob extraction incorrect, (4) TargetInfo AV_PAIR encoding issue affecting client's NTLMv2 computation."
  artifacts:
    - path: "internal/adapter/smb/v2/handlers/session_setup.go"
      issue: "completeNTLMAuth NTLMv2 validation fails for all domain variants"
    - path: "internal/adapter/smb/auth/ntlm.go"
      issue: "ValidateNTLMv2Response or BuildChallenge may have serialization issue"
    - path: "internal/adapter/smb/auth/authenticator.go"
      issue: "Separate pendingAuths store (unused in Handler path) — potential confusion"
  missing:
    - "Add hex dump debug logging of ServerChallenge at CHALLENGE send time vs validation time"
    - "Add hex dump of NTProofStr (expected vs received) in ValidateNTLMv2Response"
    - "Verify TargetInfo AV_PAIRs in BuildChallenge match what go-smb2 expects"
    - "Check if SPNEGO unwrapping preserves exact NTLM bytes"
  debug_session: ""
