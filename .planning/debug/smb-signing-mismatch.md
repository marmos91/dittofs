---
status: awaiting_human_verify
trigger: "Fix flaky signing mismatch in smb2.replay.dhv2-pending*-vs-*-windows smbtorture tests"
created: 2026-03-24T10:00:00Z
updated: 2026-03-24T11:30:00Z
---

## Current Focus

hypothesis: closeFilesWithFilter (session cleanup on connection drop) does not unregister pending CHANGE_NOTIFY watchers from NotifyRegistry. Combined with new NotifyChange triggers in SET_INFO/WRITE on this branch, stale watchers fire during subsequent tests, sending async responses via dead ConnInfos. The real signing corruption occurs because the stale async callback references a session being concurrently deleted, creating a window where SendMessage either signs with a partially-destroyed crypto state or skips signing entirely.
test: Add NotifyRegistry cleanup to closeFilesWithFilter for directory handles
expecting: Stale watchers no longer fire during subsequent tests, eliminating the race window
next_action: Implement the fix in handler.go closeFilesWithFilter

## Symptoms

expected: smb2.replay.dhv2-pending*-vs-*-windows tests pass (as on develop)
actual: Bad SMB2 signature error, NT_STATUS_ACCESS_DENIED, intermittent across different -windows variants
errors: "Bad SMB2 (sign_algo_id=2) signature for message", "NT_STATUS_ACCESS_DENIED"
reproduction: Run WPTS smbtorture tests; -sane variant skips (multi-channel), then -windows variant fails
started: After switching cipher/signing selection from server-preference to client-preference order

## Eliminated

- hypothesis: negotiate cipher/signing change directly causes mismatch
  evidence: Error shows sign_algo_id=2 (GMAC) which is the same algorithm both develop and this branch would select (GMAC is first in smbtorture's offer list, and our server supports it). The algorithm selection change doesn't change the actual selected algorithm.
  timestamp: 2026-03-24T10:30:00Z

- hypothesis: Header encoder AsyncId change causes signing mismatch for replay tests
  evidence: The AsyncId encoding change only affects CHANGE_NOTIFY responses (the only handler that sets result.AsyncId != 0). Replay tests don't issue CHANGE_NOTIFY.
  timestamp: 2026-03-24T10:45:00Z

- hypothesis: Stale CHANGE_NOTIFY async callback directly corrupts new test connection
  evidence: The async callback captures the OLD ConnInfo with the OLD TCP connection. Writing to a closed TCP connection fails silently. The new test's connection is a different TCP stream and can't receive data from the old one.
  timestamp: 2026-03-24T11:00:00Z

## Evidence

- timestamp: 2026-03-24T10:10:00Z
  checked: negotiate.go diff between develop and branch
  found: selectCipher and selectSigningAlgorithm changed from server-preference to client-preference order. For smbtorture's typical offering (GMAC first in signing list), both orderings select GMAC.
  implication: The negotiate change alone doesn't change the selected algorithm.

- timestamp: 2026-03-24T10:20:00Z
  checked: Header encoder change (encoder.go)
  found: New code writes AsyncId to bytes 32-39 when FlagAsync is set. Only CHANGE_NOTIFY sets AsyncId on HandlerResult.
  implication: Header encoding change is isolated to CHANGE_NOTIFY responses.

- timestamp: 2026-03-24T10:30:00Z
  checked: closeFilesWithFilter in handler.go
  found: When cleaning up session files on connection drop, DeleteOpenFile only removes from files map. It does NOT unregister pending CHANGE_NOTIFY watchers from NotifyRegistry. The CLOSE handler (close.go line 348-366) does unregister, but closeFilesWithFilter bypasses the CLOSE handler.
  implication: Stale NotifyRegistry entries persist after connection cleanup.

- timestamp: 2026-03-24T10:35:00Z
  checked: New NotifyChange triggers on this branch
  found: set_info.go and write.go now call h.NotifyRegistry.NotifyChange() for attribute, size, security, and atime changes. These are new on this branch (not on develop).
  implication: On develop, stale NotifyRegistry entries never fired because no code triggered NotifyChange during normal operations. On this branch, file operations trigger NotifyChange, which can fire stale watchers.

- timestamp: 2026-03-24T10:40:00Z
  checked: Session cleanup ordering in cleanupSessions (connection.go)
  found: cleanupSessions first removes from sessionConns map, then calls CleanupSession which closes files and deletes session. There's a window where the session exists but ConnInfo is gone.
  implication: When a stale NotifyChange fires, the session might still exist temporarily, allowing SendMessage to find it and attempt signing. If the session's CryptoState.Signer is being destroyed concurrently (via Destroy()), the signing could produce garbage.

- timestamp: 2026-03-24T10:50:00Z
  checked: SessionCryptoState.Destroy() in crypto_state.go
  found: Destroy() sets cs.Signer = nil and clears all key material. If SignMessage is called concurrently with Destroy(), the Signer could be nil mid-operation.
  implication: Race between async notification signing and session crypto state destruction.

## Resolution

root_cause: closeFilesWithFilter (used during session cleanup on connection drop) does not unregister pending CHANGE_NOTIFY watchers from the NotifyRegistry. The CLOSE handler does this (close.go line 348), but closeFilesWithFilter bypasses the CLOSE handler and only calls DeleteOpenFile (raw map delete). On this branch, new NotifyChange triggers were added to SET_INFO (FileBasicInformation, FileEndOfFileInformation, security descriptor) and WRITE (parent directory atime). When a prior test's connection drops without proper cleanup (e.g., skipped -sane test), its pending CHANGE_NOTIFY watchers persist in the global NotifyRegistry. When the next test (-windows) performs file operations, these stale watchers fire via NotifyChange. The async callback references the dead ConnInfo and attempts SendAsyncChangeNotifyResponse, which races with session destruction (CryptoState.Destroy setting Signer to nil). On develop this was never triggered because there were no NotifyChange calls in SET_INFO/WRITE.
fix: Added NotifyRegistry.Unregister(fileID) call in closeFilesWithFilter's second pass, before DeleteOpenFile. This ensures pending CHANGE_NOTIFY watchers are cleaned up when file handles are closed during session cleanup, matching the behavior of the explicit CLOSE handler. Also removed the known failure entry for smb2.replay.dhv2-pending2n-vs-lease-windows.
verification: All unit tests pass (internal/adapter/smb/..., pkg/adapter/smb/...). Needs smbtorture WPTS verification.
files_changed:
  - internal/adapter/smb/v2/handlers/handler.go
  - test/smb-conformance/smbtorture/KNOWN_FAILURES.md
