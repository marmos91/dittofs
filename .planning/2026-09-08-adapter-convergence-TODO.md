# Adapter convergence — program todo

Source of truth: `.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md` (PR #2420, merged).
This file tracks execution only. **If the two disagree, the plan wins** — it carries its own
corrections ledger and has already overturned twelve claims.

Legend: `[ ]` open · `[~]` in flight · `[x]` done · `[!]` blocked/needs a decision

---

## Wave 0 — Coordination (blocking)

- [x] Peer `pynfs-nfsv4-conformance-ci` confirmed GONE — stand-down message undeliverable,
      full takeover stands (incl. #2398)
- [x] No live worktree, branch or PR claims any Wave-1 issue (checked by `--head`, not ancestry —
      squash-merge makes `--merged` lie)
- [x] Confirmed with the live `dittofs issue 1828` session: no overlap; it holds
      `pkg/metadata/store/{sql,sqlite,postgres}` only
- [x] Orphan worktrees verified to hold nothing: 0 ahead, 0 dirty, no PR, no remote branch.
      (Their "40 stashes" is repo-shared `refs/stash`, newest 2026-06-03 — not theirs.)
- [x] Deleted `~/dittofs-worktrees/{2340-session-validation,2341-stateid-classes}` and their branches.
      Nothing was unmerged: both branches pointed at the **same** commit `c5ba24c49`, a strict ancestor
      of develop — 0 unique commits, never pushed, no PR ever opened
- [ ] Re-derive #2340's 32 pynfs rows — the peer's sizing context died with it. (Still open —
      blocks the #2340 residual in Wave 2.)
      Count with `^\| *[A-Z]+[0-9]+[a-z]? *\|`; a trailing-digit regex silently drops
      `CSESS16a`, `EID5c/5d/5f/5g`, `EID6a`–`EID6g`

## Wave 1 — Ownership-check class (goal 1) — ✅ LANDED 2026-09-08

7 PRs, 4 agents, 16 review passes (simplifier + reviewer each). **No correctness defect survived
in any PR.** All findings relayed and applied.

| PR | Issue | Base | Lint can run? | State |
| --- | --- | --- | --- | --- |
| #2424 | 2413 tree ownership | develop | yes | reviewed, applied, rebased |
| #2426 | 2414 AsyncId scope | develop | yes | reviewed, must-fix applied + verified |
| #2432 | 2393 LINK cross-share | develop | yes | reviewed clean on all 7 points |
| #2427 | 2394 read permission | develop | yes | reviewed clean |
| ~~#2429~~ → #2448 | 2395a owner binding | develop | yes | CLOSED by base deletion; reopened as #2448 |
| #2434 | 2395b stateid entropy | ← #2429 | **NO — ran zero checks** | merged to feature branch by an unattributed actor; landed via #2448 |
| #2439 | 2396 DELEGRETURN | ← #2434 | **NO — ran zero checks** | same; landed via #2448 |

- [x] **#2443 merged FIRST** — `skip-cache: true`. A rebase before it just inherits the red.
- [x] #2424, #2426, #2432, #2427 merged — independent, rebase should go green
- [x] Stack collapsed onto #2448 and landed in one merge (see below); not: #2427 → wait for #2429's *first* lint → #2429 →
      wait for #2434's first lint → #2434 → wait → #2439. Four sequential runs.
- [x] Follow-up issue: NFSv4.0 gap → **#2437** shared by #2395a and #2396 (no trusted clientid on the wire)
- [x] Follow-up issue: `ErrCrossShare` → **#2441** code + errmap rows, so SMB stops reporting
      STATUS_INVALID_PARAMETER for the condition NFS now reports as XDEV
- [ ] Doc PR: ownership rule into `docs/internals/architecture.md` — **BLOCKED, needs a scope call.**
      The grep was done and found **14 live counterexamples → #2449**: `GetOpenFile` is a global
      lookup over one `h.files` map spanning every connection/session, and 14 of 16 handlers call
      `primeAuthContextFromOpenFile`, which adopts the located handle's SessionID/TreeID/user.
      Only `read.go` and `write.go` check ownership first. NOT remotely exploitable — FileId
      carries 64 bits of `crypto/rand` — so it is defence-in-depth, same posture as #2395.
      Either write the rule WITH an explicit "not enforced at these sites, see #2449", or land
      #2449 first (one guard inside `primeAuthContextFromOpenFile` + a refusal test).
- [x] `graphify update .` done — 30800 nodes / 66818 edges at 1d69f4d4a from the main checkout after the wave lands
- [x] Worktrees removed; all 5 remote wave-1 branches deleted (content-verified); VM torn down

### How Wave 1 actually landed

Final merge: **#2448 = `1d69f4d4a`**, carrying all three remaining commits after #2434 and #2439
were squash-merged into their *stacked bases* (not develop) at 11:54:38/39 by an actor none of the
three live sessions could identify. Nothing was lost; the effect was to collapse the stack onto one
PR targeting develop, which is what finally gave those two commits any CI at all.

All six issues closed COMPLETED: #2393, #2394, #2395, #2396, #2413, #2414.
Integrated verify on merged develop: 65 packages pass, 0 fail.

### Traps found this wave (carry forward)

- **A stacked PR runs ZERO checks, not a subset.** Every workflow gates on
  `pull_request: branches: [develop, main]`. `gh pr checks` prints "no checks reported" rather than
  failing, so a failure count reads clean at zero checks. Count the checks, not the failures.
- **You cannot retarget out of a stack to buy CI.** `gh pr edit --base develop` is refused with
  "Cannot change the base branch because the pull request is part of a stack". Merge it or recreate
  the PR off develop — there is no third option.
- **A green is evidence only for its head SHA, and someone else can move your head.** My 38-SUCCESS
  run was for `e9d754359`; the 11:54 merges moved the branch to `8171c5d41` without touching it.
  Merge via `PUT pulls/{n}/merge -f sha=<re-read head>` so GitHub refuses on a moved head.
- **A timed-out job concludes `cancelled`, not `failure`.** The Nix cache *save* post-step hung
  ~12 min against `timeout-minutes: 10` (`nfs-pynfs.yml:106`, `conformance.yml:134`); downstream
  matrices came back `skipped` and `ci-health.yml` treats `cancelled` as benign — so NFS conformance
  silently did not run. Classify anything outside {pass, skipping, pending} as suspect.
- **Do not `--delete-branch` a PR that is a base.** It closes the dependent PR irrecoverably
  (`gh pr reopen` fails). Retarget dependents first, delete after.

### Decisions taken during the wave (do not re-litigate)

- **#2414 is ConnectionID-scoped, and that is correct.** My SessionID steer was wrong. MS-SMB2
  3.3.5.16 confines the async search to `Connection.AsyncCommandList`; 3.3.1.13 scopes AsyncId
  uniqueness to one transport connection, so a ConnID-blind lookup can resolve the WRONG request;
  3.2.4.24/25 has the client send CANCEL on the connection holding the request.
- **Do NOT fold the three `free*StateidLocked` into `checkStateidOwner`.** I asked for this and
  retracted it. `checkStateidOwner` SKIPS on a zero clientID (v4.0 has no trusted identity);
  the free* helpers REJECT on any mismatch including zero. Folding flips "zero is refused" to
  "zero is waved through", and `stateid_test.go` passes zero at six sites expecting rejection.
- **`cp -l` does NOT fall back to copying on EXDEV** — it errors; `mv` is the one that falls back.
  My original rationale for XDEV was wrong. The decision stands on RFC 1813 + knfsd only.
- **#2434's discarded `rand.Read` error is correct** — since Go 1.24 the default reader calls
  `fatal()` rather than returning; `go.mod` pins 1.25.0. Depends on that; noted in the comment.
- **#2434's collision debt stays as a `ponytail:` marker**, not a retry loop. ~1 in 40M at 1M live
  stateids, math independently re-derived.

### Wave 1 soundness audit — 2026-09-08, post-landing

Each guard was neutralized through `go test -overlay=` (compile-time file substitution, repo
untouched) and the named test observed to **fail**. Verified, not read.

| Fix | Verdict | Guard | Test proven to fail without it |
| --- | --- | --- | --- |
| #2413 / #2424 | SOUND | `smb/response.go:534-539`, `prepareDispatch` is the only gate (2 call sites = whole dispatch surface) | `smb/prepare_dispatch_test.go:136` |
| #2414 / #2426 | SOUND | `pending_registry.go:165-172` `unregisterByAsyncIDOn`, all 3 generic registries; `change_notify.go:1006` for Notify | `smb/handlers/cancel_asyncid_scope_test.go`, 5 funcs |
| #2394 / #2427 | SOUND | one helper `helpers.go:258`, 3 call sites (`read.go:111`, `read_plus.go:101`, `seek.go:91`) | `v4/handlers/io_test.go:1765` |
| #2395 / #2448 | SOUND | `ValidateStateid` a real choke point — **all 9** non-test callers pass `ctx.SessionClientID`; `checkStateidOwner` in all 3 stateid families | `state/stateid_authz_test.go`, `handlers/stateid_client_binding_test.go:22` |
| #2396 / #2448 | **PARTIAL** | live at `delegation.go:331,344` and its test does fail without it — but see below | `handlers/delegreturn_test.go:180` |

**#2396 is a no-op for every file delegation the server can actually grant.** File delegations need
`client.CBPathUp` (`delegation.go:477`), set only in `ConfirmClientID` (`manager.go:655,779`) — the
v4.0 SETCLIENTID_CONFIRM path — and `ShouldGrantDelegation` reads only `clientsByID`, never
`v41ClientsByID`. So they are v4.0-only, where `SessionClientID == 0` and `checkStateidOwner` skips
by design (#2437). The commit's own table encodes it: `{"no client identity to check", 0, NFS4_OK}`.
The guard bites only on **directory** delegations (`v41/handlers/get_dir_delegation.go:79`), which
are v4.1-only and need no `CBPathUp`. Recorded on **#2436**, whose fix converts #2396 from
directory-only to real coverage. Not a reason to reopen #2396.

- [x] Residual gap filed → **#2451**: CLOSE, LOCK, LOCKU, OPEN_DOWNGRADE, OPEN_CONFIRM and
      TEST_STATEID resolve a client-supplied stateid by `Other` alone with no owner compare and no
      `ValidateStateid` upstream. Sharpest is `LockNew` (`manager.go:2419`) — never compares
      `openState.Owner.ClientID` against the `lockOwnerClientID` handed in, so on v4.1 a client can
      lock through another client's open. Bounded, not enumerable: after #2448 a stateid `other`
      carries 64 `crypto/rand` bits. **Assigned to the #2398 agent**, stacked on top of the hoist —
      three of the sites are the functions #2398 restructures, and the CAS re-validation it adds is
      where the owner compare belongs.
- [x] Doc PR unblocked by dispatching **#2449** (one guard in `primeAuthContextFromOpenFile`, which
      also retires the 3 hand-rolled copies). Write the rule as fact once it lands, not with an
      exception list. Two corrections to #2449's own text: there is a **third** hand-rolled compare
      (`stub_handlers.go:510`, ChangeNotify), and `ioctl_copychunk.go:203` compares the two handles
      to *each other* rather than to `ctx`, so two foreign FileIds from one other session pass it.
- Not gaps, checked: `ValidateDelegationStateid` (`delegation.go:739`) is existence-only but its only
  caller re-compares at `open.go:894`; `freeLockStateidLocked` (`stateid.go:513`) compares inline and
  rejects even for clientID 0; the 14 per-handler `GetTree` sites stay existence-only by design.

## Wave 2 — `sm.mu`, then the pynfs conformance wave (goal 1) — ~90% LANDED as of 2026-09-09

Status re-verified against GitHub 2026-09-09 (see `.planning/2026-09-09-adapter-convergence-ROADMAP.md`
for the landed-via table). Remaining work:

- [x] **#2398 decided and landed** — hoisted via #2459 + #2466; closed 2026-09-08
- [x] #2341 (14 v4.0 rows) — closed 2026-09-08 (#2484, #2481)
- [x] Singles: #2362 (#2458) · #2369 (#2460) · #2399 (#2457) · #2382 (#2456, documented per decision)
- [x] Blocked-behind-#2398 singles: #2359, #2389 — closed 2026-09-08/09
- [x] #2340 partially: #2491 (EXCHANGE_ID), #2489 (DESTROY_CLIENTID), #2486 (RECLAIM_COMPLETE),
      #2485 (boot epoch) merged; closed 2026-09-09
- [x] #2329's root cause landed: #2476 (client-record unify) + #2479 (grant delegations on a
      verified callback path, incl. the CREATE_SESSION auto-bind fore-only fix)
- [ ] #2340 remaining v4.1 rows — re-derive the 32-row sizing first (Wave 0 item below)
- [ ] #2329 — confirm with an actual pynfs run that delegations now flow, then close
- [ ] #2471 CREATE_SESSION negotiated reply sizes (NFS4ERR_RESOURCE where invalid) — last #2340-class gap
- [ ] #2371 NFS4ERR_RESOURCE on two v4.1-only paths — unblocked now that #2398 hoisted `sm.mu`
- [ ] Post-merge churn from the landed wave: #2482 (fold client indexes — do FIRST, unblocks
      #2490/#2483), #2483 (seqid=0 bypass leaks into v4.0), #2487 (reclaim persist never retried),
      #2490 (BAD vs STALE on `exist_lock_owner4`), #2467 (persisted require_kerberos at load time),
      #2464 (COUR6 flake — diagnose, needs postgres-s3)
- [ ] Run against **memory AND postgres-s3** — memory-passes/SQL-fails is a persistence diagnosis,
      not a protocol one
- [ ] Read verdicts from the **CI artifact**; the local pynfs harness reports ~2× CI's failures on the same SHA
- [ ] One writer: all remaining work touches `state/manager.go` / `v4/state` — sequential PRs, no stacking

## Wave 3 — Reuse the shared layer, delete bypasses (goals 2 + 4)

- [ ] Wire **all five** bypassing content paths into the shared mapper — v4 READ/WRITE/COMMIT/READ_PLUS
      **and v3 READ/COMMIT** — restoring `ErrStoreClosed → STALE`
- [ ] Delete dead `MapLockToNFS3` / `MapLockToNFS4`; fix `ErrLockLimitExceeded` Jukebox-vs-IO;
      `ErrConflict` (`shares/coordinator.go:290-303`) — `TestErrorMapCoverage` cannot see it, 5-line PR
- [ ] SMB `ResolvedIdentity` error mapping (`smb/adapter.go:774,823`)
- [ ] `BuildIdentityResolver` runtime-scoped + cache invalidation; SMB SID `Stop()`-before-bind
      (3 HIGHs, `pkg/adapter/base.go`); EMFILE/ENFILE busy-loop
- [ ] Fix the stale `CheckExportAccess` citation at `CLAUDE.md:105` → real symbols
      (`ResolveSharePermission`, `tree_connect.go:442`)
- [ ] Delete the `grantAdaptive` branch (small — **not** the headline defect three drafts called it;
      production selects `echo`, `StrategyAdaptive` has no production callers)
- [ ] **Before deleting anything: a dead function does not make its file dead.** An `init()` filling a
      table read by a live dispatcher has no callers by design — a prior pass called 777 LOC of live
      dispatch tables deletable

## Wave 4 — Triage SMB into tranches (MOVED BEFORE the SMB fix waves)

- [ ] Mirror #2407: an SMB umbrella issue plus area tranches, for ~155 untriaged audit findings.
      **Justification is the findings, not the conformance ledger** — that argument was wrong
- [ ] `Verified: CONFIRMED` covers the **diagnosis**, not the **Fix:** paragraph — quote the diagnosis,
      re-derive the fix
- [ ] Triage "duplicated/boilerplate" findings by **diffing**, not by grouping — the Kerberos AP-REP
      copies have already drifted

## Wave 5 — Decompose god objects (goal 2) — only after 1–4

Targets (prod-only): `manager.go` 3,985 · `setFileInfoFromStore` 1,538 · `Create()` 1,016 ·
`open.go` 1,103 · `completeCreateAfterBreak` 853 · `Handler` 56 fields.

- [ ] Real package boundaries — `smb/state/`, `handlers/create/`, `handlers/info/` (your call, taken
      against the ponytail recommendation to keep splits inside `package handlers`)
- [ ] **Test migration is the main event**: `smb/handlers` has 119 in-package test files / 42,810 LOC
      reaching unexported symbols. Every move PR: existing tests pass **unchanged or moved verbatim**;
      shared fixtures exported once (precedent: `nfs/v3/handlers/testing/fixtures.go`)
- [ ] **Move-only PRs** — `git diff -M --color-moved` must show zero body edits; one package per PR;
      never stack a behaviour PR on an unmerged move (rollback only works until something lands on top)
- [ ] Drop the "no function over 150 LOC" ceiling — it turns `Create()` into 7 helpers sharing a
      20-field context struct, i.e. the god object one level down. **Replace with: every extracted
      function has its own test**
- [ ] Acceptance test, defined now: one `dispatch_test.go` per protocol asserting the same four gate rejections

## Wave 6 — Shared dispatch core (the finish line, not a prerequisite)

- [ ] Deliverables land inside Waves 1/3/5 rather than as a separate build-out

## Wave 7 — Perf lens (goal 3)

- [ ] `perf` scored ~zero across all three audits, and the perf-attempts ledger names the adapter layer
      as the remaining un-walked axis ("the remaining rig gap is NFS/metadata per-read, NOT block store")
- [ ] Measurement plan — **none exists today**:
  - [ ] Connection ramp, both protocols, 50/sec to 10,000, holding idle: RSS + accept-to-first-response p50/p99
  - [ ] NFSv4.1 >64 concurrent ops on one session — confirm RFC-correct `SEQ4_STATUS`, not silent blocking
  - [ ] Large-I/O allocation rate with max read/write raised toward 16MB, before and after the tier fix
        (the cleanest before/after story in the audit)
- [ ] `bufpool`: **fix tier sizing before generalising** — the large tier is `1<<20`, which *equals* the
      advertised max I/O size, so every max-size WRITE/READ falls off the pool. `ReadFromBlockStore` has
      no ceiling, so NFSv4 READ heap-allocates up to file size. Generalising a mis-sized pool propagates it

---

## Cross-cutting — not owned by any wave

### auxsvc
- [ ] Fix the lock: `Group.Start` (`auxsvc.go:96-114`) calls `s.Start()` at `:107` **while holding `g.mu`**,
      stalling `Serve`'s accept loop and `StopAll` by up to 10s. Same shape as #2398
- [ ] Rename `auxsvc` → `sidecar` — pure rename, 47 refs / 9 files. The implementation already calls
      itself sidecar everywhere. **Sequence it when no peer branch touches `pkg/adapter/{nfs,smb}`**
- [ ] Do **not** split it — 234 lines, one file, 3-method interface (the "411 LOC" figure counted the test file)

### Conformance
- [ ] CI gate: **every known-failure row must have a non-empty Reason and Issue column** (`kf_load` already
      parses column 3). **Do not encode a hand-typed total** — that bakes in a wrong baseline
- [ ] Establish the knfsd/Samba comparison per SMB suite the way #2204 did for pynfs — a row we fail that
      Samba also fails is a different class
- [ ] **Never count the ledger files again.** Read each file's own tally or run `kf_load`. The count has been
      wrong three times (219 → 180 → 178) because three passes counted markdown rows.
      Current: **178** = SMB 86 + NFS 92
- [ ] SMB conformance is **already compliant** (#673 criterion met, zero UNJUSTIFIED). Outstanding work is
      entirely NFS-side: 60 pynfs rows + 32 POSIX rows

### Tests
- [ ] `pkg/adapter/identity_test.go` **does not exist** — the function every SMB and NFS Kerberos session
      calls has zero unit coverage. This is why the `Enabled` bypass survived
- [ ] Fix `errmap_test.go:20-47` — it hand-types 25 codes and asserts `len(...) == 25`, so `ErrConflict`
      being absent is invisible. A 1.0 test:prod ratio proves nothing about whether a test *can* fail
- [ ] Delete tests for the dead NFS mappers (`convention_test.go`, `table_test.go`, `cache_test.go`)
- [ ] Boundary rule: unit = pure logic · integration = anything touching a real backend (faking hides the
      bug class) · e2e = only genuinely cross-protocol assertions

### Instrumentation
- [ ] **NFSv4 records only `COMPOUND`** (`pkg/adapter/nfs/handlers.go:525`) — add per-op RED inside the
      COMPOUND loop. The plan flagged this for SMB and missed the identical NFSv4 defect
- [ ] Expose pprof on `pkg/metrics/server.go` too (already bearer-token + mTLS), not only the control-plane port
- [ ] Credit/sequence exhaustion is invisible
- [ ] Both adapters **are** already instrumented — the original search failed because metrics are reached
      through a narrow interface field (`connInfo.Metrics`), invisible to an import grep.
      *An absent import is not absent behaviour.*

### Code hygiene — three grep-able categories, target zero
- [ ] `// =====` banner separators (962)
- [ ] Doc comments restating the signature ("TreeDisconnect handles SMB2 TREE_DISCONNECT command")
- [ ] Comments citing symbols or line ranges that **do not exist** — `mapMetadataErrorToNFS`,
      `lockErrorToStatus`, `converters.go:364`, `xdr/errors.go:80-152`, `v4/types/errors.go:59-64`,
      `CheckExportAccess`. Worse than noise: they mislead
- [ ] **Keep** invariants the code cannot say (the `releaseReplay` comment is why a real bug is absent),
      RFC struct citations, and `ponytail:` markers
- [ ] Realistic LOC reduction is **15–25%, not half**. The bigger untouched target is test code:
      `smb/handlers` alone is 42,810 test LOC against 34,401 prod
- [x] ~~Fourth audit with `slop`/`bloat`/`comments` lenses scoped to LOC reduction~~ — **declined by user,
      2026-09-08.** Not needed at present. The 15–25% estimate stands unvalidated; revisit only if someone
      re-proposes halving the adapter layer

### Open questions carried into execution
- [ ] Whether an OTel-style span library exists anywhere in the tree — **unverified. Grep before proposing
      tracing machinery**

---

## Standing hazards (in every agent prompt)

- Audits pinned at `40884ad4f`, which **predates PR #2356** → re-verify every premise on develop first
- #2345: `DITTOFS_TEST_POSTGRES_DSN` is a **boolean gate only**; the db name is hardcoded `dittofs_test` in
  4 test files. A branch straddling a migration dies at open with "on-disk format is newer than this build".
  Check `pg_stat_activity` before any dropdb — another session is live on it
- Postgres tests are all `//go:build integration`; a green `go test ./...` proves **nothing** about postgres
- Scratchpad PR-body filenames must be issue-scoped — a generic name once published a sibling's body with
  the wrong `Closes #`
- Protocol/e2e suites are exclusive (fixed ports, mounts, sudo) — brokered, one agent at a time
- Never `gh pr update-branch --rebase` (rewrites commits unsigned) · sign every commit · rebase never merge

## Infrastructure

- **Wave-1 box (mine, teardown mine alone):** `dfs-wave1-adapters` · `a62b7b49-b176-46e7-bcb5-0361fdd75923`
  · `51.15.211.217` · fr-par-1 · POP2-4C-16G · Ubuntu 24.04.4 / kernel 6.8.0-138
  · Go 1.23.4, nfs-utils 2.6.4, cifs-utils 7.0 · `/mnt/dfs-nfs`, `/mnt/dfs-smb`
  · id in `scratchpad/wave1-vm.json`; `.bench-vm.json` deliberately not written
  · SSH: `SSH_AUTH_SOCK=$HOME/.dfsb.sock ssh root@51.15.211.217`
- **Never** touch `scw-coder-*` — guard by NAME PREFIX, never id (ids rotate)
- [x] Disposed 2026-09-08 (authorized): `dfs-e2e-adapters` (`c4cc8289`, untouched since 2026-08-27) plus
  its IP `163.172.184.170`, and the unattached IPs `51.15.217.98` / `51.15.137.63`. Verified gone by
  re-query, not by exit code. Wave-1 box confirmed still running afterwards.
  *Note: a server's deprecated `public_ip` field reads None even when one is attached — the real value is
  in `public_ips`. It misled me once here and once on our own box.*
- [ ] Possible further stale box, NOT disposed and not investigated: `dfsbench-smb3`. Flag only
- [!] 6 unstaged `.planning/perf/*` deletions sit in the shared main checkout (look like
  `chore/prune-spent-planning-docs` leakage). They follow any branch switch there and a careless
  `git add -A` sweeps them into an unrelated commit. Neither mine nor the #1828 session's
