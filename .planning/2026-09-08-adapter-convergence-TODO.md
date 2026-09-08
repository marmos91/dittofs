# Adapter convergence — program todo

Source of truth: `.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md` (PR #2420, open).
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
- [ ] Re-derive #2340's 32 pynfs rows — the peer's sizing context died with it.
      Count with `^\| *[A-Z]+[0-9]+[a-z]? *\|`; a trailing-digit regex silently drops
      `CSESS16a`, `EID5c/5d/5f/5g`, `EID6a`–`EID6g`

## Wave 1 — Ownership-check class (goal 1) — IN FLIGHT

One rule, both protocols, shipped together. 6 issues → 7 PRs, 4 agents.

- [~] **#2413** SMB tree-scoped commands don't check TreeID ownership — guard in `prepareDispatch`'s
      `NeedsTree` branch, `smb/response.go:514-520`. Covers TREE_DISCONNECT, CREATE and every
      tree-scoped command at once. *Both pre-checks already done — do not re-run.*
- [~] **#2414** CANCEL resolves a parked request by AsyncId with no ownership check.
      Scope the AsyncId lookups as the MessageID ones already are. **MS-SMB2 3.3.5.16 first** —
      SessionID, not ConnectionID, because SMB3 multichannel spans connections
- [~] **#2394** NFSv4 READ / READ_PLUS never check read permission → one shared helper, not two copies
- [ ] **#2395a** `ValidateStateid` owner-to-client binding (mirror `free*StateidLocked`, RFC 8881 §18.38.3).
      No dependency on #2398 — it is a field compare under an existing `RLock`
- [ ] **#2395b** stateid `other` → `crypto/rand`, separate PR (lock-neutral minting change)
- [ ] **#2396** DELEGRETURN stateid-ownership. **Check reachability first** — #2218 says recall never
      worked until #2219; a guard on a dead path is not worth shipping
- [ ] Follow-up issue for the NFSv4.0 DELEGRETURN gap (no clientid4 on the wire)
- [ ] Merge serially, smallest blast radius first; rebase each survivor before the next
- [ ] Doc PR: write the ownership rule into `docs/internals/architecture.md` —
      **grep for violations BEFORE writing it**; a prior rule went in with 3 counterexamples in-tree
- [ ] Regression tests per plan §Verification 3: a second session cannot TREE_DISCONNECT the first's
      tree and the first's opens survive; a CANCEL carrying another session's AsyncId cancels nothing

## Wave 2 — `sm.mu`, then the pynfs conformance wave (goal 1)

- [ ] **#2398 decided first** — #2341/#2340 add *write-side* work under the contended lock.
      Criterion (settled, on the issue): release-call-recommit is safe without re-validation only if the
      post-gap write is **idempotent, keyed by something stable, and records a fact the gap cannot
      falsify**. `ReclaimComplete` meets all three; a stateid/seqid commit meets **none** and owes a CAS
- [ ] #2341 (14 v4.0 rows) · #2340 (**32** v4.1) · #2329 (10 v4.1)
- [ ] Singles: #2371, #2382, #2389, #2399, #2359, #2362, #2369 (#2399 is now a plain consumer of
      `types/current_stateid.go`)
- [ ] Run against **memory AND postgres-s3** — memory-passes/SQL-fails is a persistence diagnosis,
      not a protocol one
- [ ] Read verdicts from the **CI artifact**; the local pynfs harness reports ~2× CI's failures on the same SHA

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
