---
status: inconclusive
trigger: "smb2.scan.find intermittently fails with 'Bad SMB2 (sign_algo_id=2) signature for message' (AES-128-GMAC) in CI full smbtorture suite — issue #362"
created: 2026-04-15T00:00:00Z
updated: 2026-04-15T00:00:00Z
---

## Current Focus

hypothesis: No confirmed root cause. Original "concurrent signing buffer aliasing" hypothesis is NOT supported by the code. The GMAC signer uses a local msgCopy, the outer wrapper writes into a goroutine-local payload slice, `go test -race ./internal/adapter/smb/...` reports clean. Strongest remaining candidate: **stale/incorrect session state lookup at sign time** — most plausibly, SendMessage at response.go:460 does a fresh `GetSession()` lookup and then calls `sess.SignMessage`. If the session's `ShouldSign/CryptoState.Signer` flips (e.g., due to a Destroy() on teardown) between the `ok := GetSession()` check and `SignMessage`, or if the wrong signer is selected when signing encrypted-but-not-encrypted session-setup responses (line 470-474 logic around `isNewSessionSetup`), the response is signed with a key the client does not expect. Needs live reproduction + per-message signing trace to confirm.
test: (not run in this session — Docker-based smbtorture reproducer scoped out due to session time bounds)
expecting: A single failing scan.find response where the server DEBUG log shows "Signed outgoing SMB2 message" with a MessageId whose signer key derivation differs from what the client expects (e.g., signed with the pre-rekey key or with the guest/anonymous signer); OR a response sent unsigned while the client expected it signed.
next_action: Run smbtorture scan.find in a 50× docker loop with DITTOFS_LOGGING_LEVEL=DEBUG capturing signing decisions and per-MessageId key fingerprint (first 4 bytes of cs.SigningKey), then correlate with the WPTS "Bad SMB2 signature" message printing the offending MessageId.

## Symptoms

expected: smb2.scan.find passes consistently across CI runs (it does on Samba's own CI)
actual: Intermittent fail in full smbtorture suite — single occurrence collapses scan.find sub-suite. Marked flaky in test/smb-conformance/smbtorture/KNOWN_FAILURES.md by commit eaf7d8ad.
errors:
  - "Bad SMB2 (sign_algo_id=2) signature for message"
  - "scan.find [Unknown error/failure. Missing torture_fail() or torture_assert_*() call?]"
reproduction: Run full smbtorture suite in CI multiple times; fails roughly 1 in N runs. scan.find rapidly iterates all QUERY_DIRECTORY information classes on a single signed session.
started: Surfaced as flake on develop after #356 (blockstore-only change, no SMB code touched). Re-failed on #357. Existed latently before; #356 likely changed timing.

## Eliminated

- hypothesis: LOGOFF deferred-delete race causes unsigned responses
  evidence: Fixed in .planning/debug/smb-logoff-signing-race.md — sessions kept alive with LoggedOff=true; SendMessage no longer hits the session-not-found path during LOGOFF. scan.find does not issue LOGOFF mid-test.
  timestamp: 2026-03-23

- hypothesis: Stale CHANGE_NOTIFY watcher fires async response with destroyed crypto state
  evidence: Fixed in .planning/debug/smb-signing-mismatch.md — closeFilesWithFilter now unregisters watchers. scan.find does not issue CHANGE_NOTIFY.
  timestamp: 2026-03-24

- hypothesis: Go stdlib AES-GCM cipher.AEAD.Seal is not safe for concurrent use
  evidence: Documented and verified safe — Seal/Open hold no mutable state on the AEAD; same instance can be called from multiple goroutines per Go crypto policy. The single shared `s.gcm` in GMACSigner (internal/adapter/smb/signing/gmac_signer.go:18) is fine to call concurrently.
  timestamp: 2026-04-15

- hypothesis: Caller-supplied buffer aliasing in signing.SignMessage causes cross-goroutine corruption of the signature field
  evidence: Code audit: SendMessage builds `smbPayload := append(hdr.Encode(), body...)` which is goroutine-local (response.go:457); compound.go:412 likewise builds a fresh per-sub-response `cmdBytes` slice. GMACSigner.Sign (gmac_signer.go:55) operates on its own `msgCopy`, never mutating caller input. `SignMessage` (signer.go:54-67) only mutates the caller's goroutine-local buffer. No shared-buffer path found.
  timestamp: 2026-04-15

- hypothesis: Data race in shared crypto state (SigningKey/Signer) under concurrent load
  evidence: `go test -race -count=1 -timeout 300s ./internal/adapter/smb/...` passes clean on signing, session, encryption, kdf, smbenc, v2/handlers. No detected races in the signing-relevant packages.
  timestamp: 2026-04-15

- hypothesis: scan.find is heavily concurrent and racy signing corrupts in-flight responses
  evidence: Samba smbtorture scan.find iterates QUERY_DIRECTORY info classes synchronously (issue one request, await response, next). With sequential request/response, at most one response is in signing at a time on the session, so concurrent-signing races cannot explain the failure.
  timestamp: 2026-04-15

## Evidence

- timestamp: 2026-04-15
  checked: pkg/adapter/smb/connection.go:249-262
  found: Each non-LOGOFF SMB request is dispatched in its own goroutine via `go func(...) { smb.ProcessSingleRequest(...) }`. Multiple in-flight requests on a single session can execute handlers and SendMessage concurrently, but smbtorture scan.find issues them sequentially.
  implication: Concurrent signing is *possible* on a busy connection but not exercised by scan.find.

- timestamp: 2026-04-15
  checked: internal/adapter/smb/response.go:456-518 (SendMessage)
  found: SendMessage builds `smbPayload := append(hdr.Encode(), body...)` (fresh slice), does `GetSession(hdr.SessionID)` lookup, then branches on `ShouldEncrypt`/`ShouldSign`. The `isNewSessionSetup` logic clears `sess.NewlyCreated` non-atomically. The writeMu is acquired AFTER signing, so signing occurs outside the write mutex.
  implication: Single-response path is goroutine-local at the buffer level. BUT session-state lookup and `NewlyCreated` mutation are not synchronized; under very specific interleavings this could mis-route a SESSION_SETUP response between signed-only and encrypted. (Not likely for scan.find, which runs post-setup.)

- timestamp: 2026-04-15
  checked: internal/adapter/smb/signing/gmac_signer.go:49-77 (GMACSigner.Sign)
  found: Sign creates a local `msgCopy`, zeros signature on the copy, derives 12-byte nonce from the copy's MessageId+flags, calls s.gcm.Seal. `s.gcm` is concurrent-safe per Go crypto. Returns a fresh [16]byte tag.
  implication: GMACSigner itself is concurrent-safe; no shared mutable state affecting signature output.

- timestamp: 2026-04-15
  checked: internal/adapter/smb/compound.go:395-428
  found: Compound responses sign each sub-response into its own freshly built `cmdBytes` buffer. Pad-then-sign ordering is correct.
  implication: Compound path is goroutine-local at the buffer level.

- timestamp: 2026-04-15
  checked: go test -race -count=1 -timeout 300s ./internal/adapter/smb/...
  found: All SMB internal packages pass the race detector: smb, auth, encryption, header, kdf, lease, rpc, session, signing, smbenc, types, v2/handlers.
  implication: No latent data race in the server's signing / session / encryption codepaths under the existing unit + integration test workload. Any race that explains #362 must be reachable only by the specific smbtorture scan.find call pattern.

- timestamp: 2026-04-15
  checked: KNOWN_FAILURES.md entry for scan.find
  found: scan.find iterates QUERY_DIRECTORY INFO CLASSES (0-255), not SMB2 commands. It opens a directory once, then blasts QUERY_DIRECTORY with each FileInfoClass on the same FileID. Unimplemented classes return StatusInvalidParameter via `handleRequest → handleQueryDirectory`; the response goes through standard SendMessage signing.
  implication: The test stresses a specific code path: QUERY_DIRECTORY error responses on the same open handle in tight succession. The signature failure likely involves either (a) the QueryDirectory handler mutating `openFile.EnumerationIndex`/`EnumerationPattern`/`EnumerationComplete` concurrently (not a signing issue, but could produce malformed bodies whose signature doesn't match what the server computed if body bytes are captured by reference before the mutation finalizes) or (b) a server→client flow control quirk (e.g., crediting) that causes the client to believe one MessageId is expected when another is delivered.

- timestamp: 2026-04-15
  checked: internal/adapter/smb/v2/handlers/query_directory.go:240-372
  found: QueryDirectory reads & mutates `openFile.EnumerationIndex/Complete/Pattern` fields and calls `h.StoreOpenFile(openFile)`. The handler returns a `*QueryDirectoryResponse` whose `Data` is a locally-built byte slice; no shared mutable body reference escapes the handler. StoreOpenFile is a map write; if it's not guarded it could race on concurrent handlers, but scan.find is sequential.
  implication: Not obviously a signing-relevant failure path for scan.find's sequential workload. Request-body corruption is already ruled out by buffer locality; response-body corruption by the handler would still produce a self-consistent signature.

## Resolution

root_cause: not determined in this session. Strongest remaining candidate is a session-state / signing-key selection issue at SendMessage time (wrong signer used for a small subset of responses). Confidence LOW — code inspection found no concrete bug, race detector is clean, and scan.find's sequential-per-session pattern rules out the obvious concurrent-signing races.

evidence_missing:
  - Live per-response signing trace from a failing CI run (MessageId, payload length, first 16 bytes of payload, signing key fingerprint, computed signature) — not collected because no local reproducer was produced in this session.
  - Correlated WPTS client log line showing the offending MessageId and its expected-vs-actual signature bytes.
  - A 50× iteration docker smbtorture scan.find loop against a race-built `dfs` binary to either (a) reproduce locally with DEBUG logs or (b) establish that the flake is CI-runner-specific (timing/load dependent).

recommended_next_steps:
  1. Add a temporary DEBUG trace in `signing.SignMessage` that emits `{sessionID, messageID, len(payload), payloadHash, signerKeyFingerprint, signatureBytes}` per call, and a symmetric trace on the verify side if needed.
  2. Build a race-enabled `dfs` binary and run `cd test/smb-conformance/smbtorture && ./run.sh --profile memory --filter smb2.scan.find` in a 50× loop. Capture full server + smbtorture logs.
  3. If reproduced, grep for the failing MessageId and compare actual vs expected signer key fingerprint — this will show whether the wrong key was used (signer swap) vs wrong bytes signed (buffer corruption) vs missing flag (signed flag not set).
  4. If NOT reproduced locally, the bug is timing-dependent / CI-specific. Rerun only in CI with the same DEBUG trace and wait for it to hit.

fix: not applied (goal was find_root_cause_only; investigation inconclusive).

## Update 2026-04-15 — Reproducer + Tracing

- **Local Docker reproducer dead end**: smbtorture container fails instantly with `NT_STATUS_NO_MEMORY` on Apple Silicon (amd64-on-Rosetta talloc issue documented in flaky-tests-investigation.md). Cannot reproduce #362 locally via Docker on this host.

- **Comprehensive trace committed (a951015d)**: Every signing decision now logs in detail.
  - `signing.SignMessage`: SIGN_TRACE on every sign + sign-then-verify self-check
  - `session.SignMessage`: signer type, key fingerprint, ShouldSign state
  - `response.SendMessage`: ERROR-level trace if GetSession misses for non-SESSION_SETUP

- **CI run dispatched**: https://github.com/marmos91/dittofs/actions/runs/24478651677
  - Will reproduce #362 statistically (1 in N runs) and capture pinpoint trace.

- **Local e2e baseline**: TestSMB3_GoSMB2_BasicFileOps passes with race detector. SESSION_SETUP `SendMessage session lookup MISS` fires on intermediate STATUS_MORE_PROCESSING_REQUIRED responses (expected — session not registered until SESSION_SETUP SUCCESS, and unsigned is correct because client has no key). Trace correctly distinguishes this from "non-SESSION_SETUP" misses.

next_action: Wait for first CI run; while waiting, write focused stress test mimicking scan.find QUERY_DIRECTORY blast pattern for additional local repro attempts.

## RESOLUTION 2026-04-16

**ROOT CAUSE CONFIRMED**: per-connection single-slot stash race in
`StashPendingSessionSetup` + `InitSessionPreauthHash` consumption pattern.

**Critical correction**: KNOWN_FAILURES.md misattributed #362 to
`smb2.scan.find`. The actual flaky tests are the multi-connection
`bench.*` family (`bench.path-contention-shared`, `bench.session-setup`,
`create.bench-path-contention-shared`). CI run 24478651677's
smbtorture-output.txt has zero scan.find entries; "Bad SMB2
(sign_algo_id=2) signature" appears 4 times, all in bench tests that
explicitly "Open 4 connections" and report "Failed opening 3/4".

**Mechanism (now confirmed by local server log iter1-server.log)**:
- `pkg/adapter/smb/connection.go:255` dispatches each request in its
  own goroutine.
- bench.session-setup pipelines LOGOFF + SESSION_SETUP messages on a
  single connection without waiting for responses.
- Multiple SESSION_SETUP `before-hook` invocations run concurrently on
  the same connection's `ConnectionCryptoState`. Each calls
  `StashPendingSessionSetup(rawMessage)` — overwriting the previous.
- Each handler then calls `InitSessionPreauthHash(sessionID)` which
  reads the (now-corrupted) latest stash, chains the WRONG bytes into
  the per-session preauth hash.
- Server derives signing key from corrupted hash; client derives from
  correct hash. SUCCESS response signature mismatch → client rejects
  with "Bad SMB2 signature" → STATUS_ACCESS_DENIED.

**Fix (commit f76290e5)**: rawMessage now flows through
`SMBHandlerContext.RawRequest` (set in `ProcessSingleRequest`). The
SESSION_SETUP handler passes its own bytes directly to
`InitSessionPreauthHash(sessionID, rawMessage)`. Stash mechanism
removed. No longer susceptible to cross-goroutine overwrite.

**Local verification**:
- 10/10 fail with "Bad SMB2 sign_algo_id=2 signature" pre-fix
- 0/10 with that error post-fix (verified iter1-client.log)
- All SMB unit tests pass
- All conformance-test crypto state tests pass

**Remaining work**: bench.session-setup still fails locally with a
different error (NT_STATUS_INVALID_NETWORK_RESPONSE) — separate bug
in the same scenario, masked by the signature failure. Tracked
separately. Issue #367 (WPTS BVT timestamp/DFS) untouched on this
branch.

**CI verification**: pushed branch and triggered run 24481000968.
Once the run completes, signature errors should be absent.

status: resolved (signature race) — pending CI verification
