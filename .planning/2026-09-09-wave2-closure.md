# Wave 2 closure — 2026-09-09 (~23:45)

## What landed (all squash-merged to develop @ 7e5cd2e40, all assigned marmos91)

- **#2494** / issue #2471 — CREATE_SESSION channel-size floors (request floor 256, response floor 1024→256 window), unknown-flag NFS4ERR_INVAL (suite conformance, not RFC MUST), session cap NFS4ERR_NOSPC. Squash `c5dd09bc2`.
- **#2496** / issue #2482 — v4.0/v4.1 client-index fold: one `clientsByID` map, `MinorVersion` field, `v40ClientLocked`/`v41ClientLocked` filters at all version-sensitive sites. Squash `a4f479f10`.
- **#2495** / issue #2487 — reclaim-persist repair: v4.0 CLAIM_PREVIOUS sets ReclaimComplete on the false→true transition only; initial persist + retry both OFF sm.mu (goroutine spawns after unlock); `pendingReclaimPersists` dedup with chain adoption (fresh timer armed on adoption); stale timers re-check chain liveness before the durable write. 5 Copilot review rounds, all findings fixed. Squash `8c4895714`.
- **#2497** / issue #2464 — two layers: (a) API root cause — PUT/PATCH/reset NFS settings handlers call `runtime.GetSettingsWatcher().RefreshNFSSettings(ctx)` after the store write so the reload lands synchronously before update returns (lease consumers read the watcher cache, previously refreshed only on the 10s DB-poll ticker); (b) harness guard — run-pynfs.sh polls `dfsctl adapter settings nfs show` 10×1s, fails fast on mismatch. Adversarial review BLOCK (guard-alone tautology) resolved by (a). Squash `80331c4ea`.
- **#2498** / issue #2490 — LockExisting (exist_lock_owner4) answers NFS4ERR_BAD_STATEID for both special stateid forms (all-zeros anonymous + all-ones READ-bypass) before the grace check; unit test pins both forms + negative STALE assertion. Adversarial review OK WITH NOTES (P2 grace-ordering inconsistency with LockNew disclosed in PR body, deliberate). Squash `7e5cd2e40`.

## Post-merge checklist (all done)

- develop pulled to 7e5cd2e40 (local branch synced)
- Remote branches fix/2487-reclaim-persist-retry, fix/2464-verified-lease-guard, fix/2490-lock-exist-bad-stateid deleted
- Worker worktrees (wt-2490, fix-2464-lease-guard) + stale pi-worktrees (2471/2482/2487) removed
- Issues #2471 #2482 #2487 #2464 #2490 all CLOSED (auto via Closes #N)
- `graphify update .` re-run from the merged checkout: 31,103 nodes, 69,131 edges
- Roadmap rewritten: Wave 2 ✅ LANDED, Step 2 (Wave 3) marked NEXT
- chore/pi-migration pushed (docs bookkeeping branch)

## Wave 3 prep (premises verified on develop 7e5cd2e40)

1. **TestErrorMapCoverage rewrite — LAND FIRST.** `errmap_test.go:54` hand-types `expectedCount = 25` but the enum now has 26 (`ErrConflict` at errors/errors.go:103; `for c := ErrNotFound; c <= ErrConflict` is valid — ErrorCode is a contiguous iota). 5-line PR; until it lands every other errmap fix is unguarded.
2. **ErrStoreClosed → STALE restore.** `lookupErrorRow` (errmap.go:266) already maps ErrStoreClosed → the ErrStaleHandle row; the bypassing paths are the NFSv4 handlers returning literal `types.NFS4ERR_IO` (read.go:143, write.go:215, read_plus.go:125) instead of calling the mapper. v3 write.go:261 already routes through MapContentToNFS3.
3. **MapLockToNFS3/NFS4 dead columns.** lock_errmap.go defines NFS3/NFS4 columns; the only production callers are SMB (lock.go:469/510/571, lock_async.go:252 → MapLockToSMB). The audit's "delete dead MapLockToNFS3/4" premise holds. ErrLockLimitExceeded: LOCK-context row says NFS3ErrJukebox (lock_errmap.go), general errorMap row says NFS3ErrIO (errmap.go:224) — the drift to resolve.
4. **ErrConflict row.** No errorMap row; producers are mderrors.NewConflictError at badger/transaction.go:463 and memory/transaction.go:315, but the runtime coordinator wraps ObjectID conflicts into engine.ErrObjectIDConflict via errors.Join (shares/coordinator.go:290-303) — must name the handler that actually observes a raw StoreError{ErrConflict} and pin it with a test.
5. **getUserIdentity deletion.** Premise holds: only caller is buildIdentity (auth_helper.go:188); SMB Kerberos/Netlogon already resolve through the ResolvedIdentity path (synthUserFromResolved lists primary GID first, kerberos_auth.go:554) then throw it away. Delete getUserIdentity + fix the kerberos_identity_test.go:43 assertion. Scope identity rules to *resolved* identities only — AUTH_SYS is exempt (RFC 5531 wire credentials).
6. **grantAdaptive.** NOT a dead function — it's the StrategyAdaptive arm (session/manager.go:231), reachable via config. The real finding is the async paths (response.go:1207-1210, 1278-1281) granting SequenceWindow directly and never Session.credits, under-counting GetOutstanding for grantAdaptive's throttle. Fix = route async grants through Manager.GrantCredits (or a Manager method updating both).
7. **Listener lifecycle.** base.go:203-218: net.Listen happens inside ServeWithFactory; Stop() before bind leaves listenerReady open forever (GetListenerAddr blocks). Accept loop on unexpected error continues without backoff (EMFILE/ENFILE busy-loop, base.go:246-263).

## Standing hazards carried into Wave 3 prompts

- Audits pinned at 40884ad4f predate #2356 — re-verify each premise on develop (done above for Wave 3 items)
- Postgres tests are //go:build integration — green go test ./... proves nothing about postgres
- Merge path: gh CLI (no github MCP server); PUT pulls/{n}/merge with re-read sha; sign everything; rebase never merge
- Assign every PR to marmos91; three reviews before open (correctness, simplification, adversarial); babysit Copilot + CI after open
- Known-failures ledger: 178 = SMB 86 + NFS 92; row-count regex ^\| *[A-Z]+[0-9]+[a-z]?*\|
