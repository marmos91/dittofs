---
status: awaiting_human_verify
trigger: "SMB intermittent signing verification failure causing smb2.connect and smb2.scan.setinfo flaky test failures in CI"
created: 2026-03-23T00:00:00Z
updated: 2026-03-23T00:00:00Z
---

## Current Focus

hypothesis: LOGOFF's deferred session delete races with in-flight request goroutines. When a concurrent goroutine tries to sign its response (via SendMessage -> GetSession) after the session was deleted by the deferred delete, the response goes out unsigned. The client rejects the unsigned response with "Bad SMB2 signature" / STATUS_ACCESS_DENIED.
test: Remove the deferred session delete; keep session alive with LoggedOff=true until connection close
expecting: In-flight goroutines can still sign responses; verifier and prepareDispatch correctly handle LoggedOff=true
next_action: Apply fix and verify

## Symptoms

expected: After LOGOFF, the next request on the same connection should return NT_STATUS_USER_SESSION_DELETED
actual: Sometimes returns NT_STATUS_ACCESS_DENIED because the signing verifier sees a stale session key and rejects the signature before the session's LoggedOff flag is checked
errors:
  - smb2.connect: "status was NT_STATUS_ACCESS_DENIED, expected NT_STATUS_USER_SESSION_DELETED: logoff should have disabled session"
  - smb2.scan.setinfo: "Bad SMB2 (sign_algo_id=2) signature for message" -> "Failed to connect to SMB2 share - NT_STATUS_ACCESS_DENIED"
reproduction: Run smbtorture test suite (400+ tests serially on same server). These tests fail intermittently.
started: Flaky for a while. Phase 71 added client registry hooks that may widen the timing window.

## Eliminated

## Evidence

- timestamp: 2026-03-23T00:10:00Z
  checked: Signing verifier (framing.go VerifyRequest)
  found: Verifier correctly skips verification when LoggedOff=true (line 335) or session not found (line 328). Returns STATUS_ACCESS_DENIED only when signature mismatch on an active session.
  implication: The verifier itself is NOT the direct cause. The issue is upstream.

- timestamp: 2026-03-23T00:15:00Z
  checked: LOGOFF synchronous processing and deferred session delete flow
  found: connection.go processes LOGOFF synchronously (line 233-237). ProcessSingleRequest sends LOGOFF response, then deletes session via DeferredSessionDelete (response.go line 134-136). However, previously-dispatched goroutines (line 254-261) may still be in-flight.
  implication: Race window exists between deferred session delete and concurrent goroutines calling SendMessage.

- timestamp: 2026-03-23T00:20:00Z
  checked: SendMessage signing behavior when session is deleted (response.go line 380-421)
  found: SendMessage does `if sess, ok := connInfo.Handler.GetSession(hdr.SessionID); ok { ... sign ... }`. If session was deleted, ok=false, and the response goes out UNSIGNED. Client rejects unsigned response.
  implication: This is the root cause of "Bad SMB2 signature" errors.

- timestamp: 2026-03-23T00:22:00Z
  checked: Session lifecycle and LoggedOff handling across all relevant code paths
  found: verifier.VerifyRequest skips for LoggedOff=true. prepareDispatch returns StatusUserSessionDeleted for LoggedOff=true. SendMessage still signs for LoggedOff=true (only skips when session is NOT FOUND). Connection cleanupSessions deletes sessions on disconnect.
  implication: If we keep the session alive with LoggedOff=true (no deferred delete), all code paths work correctly AND in-flight goroutines can still sign responses.

## Resolution

root_cause: LOGOFF's deferred session delete (response.go line 134-136) races with in-flight request goroutines. When a concurrent goroutine finishes processing and calls SendMessage, GetSession returns false (session already deleted), so the response goes out unsigned. The client rejects the unsigned response with "Bad SMB2 signature" -> STATUS_ACCESS_DENIED.
fix: Remove the deferred session delete. Keep the session alive with LoggedOff=true. The verifier and prepareDispatch already handle LoggedOff=true correctly. The session is cleaned up on connection close via cleanupSessions.
verification: Code compiles, all unit tests pass (internal/adapter/smb/..., pkg/adapter/smb/...). Needs CI smbtorture verification.
files_changed:
  - internal/adapter/smb/response.go
  - internal/adapter/smb/v2/handlers/logoff.go
  - internal/adapter/smb/v2/handlers/session_setup.go
  - internal/adapter/smb/v2/handlers/context.go
  - test/smb-conformance/smbtorture/KNOWN_FAILURES.md
