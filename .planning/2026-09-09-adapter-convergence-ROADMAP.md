# Adapter convergence — roadmap as of 2026-09-09 (Wave 2 CLOSED)

Source of truth: `.planning/2026-09-08-adapter-convergence-MASTER-PLAN.md` (waves, corrections
ledger) and `.planning/2026-09-08-adapter-convergence-TODO.md` (execution tracking). This file is
the point-in-time status + proposed order. If the two disagree, the plan wins.

Verified against GitHub on 2026-09-09 ~23:40: Wave 2 fully landed — PRs #2494/#2495/#2496/#2497/#2498
all squash-merged, issues #2471/#2487/#2482/#2464/#2490 auto-closed, `origin/develop` at `7e5cd2e40`,
worktrees and branches deleted, `graphify update .` re-run from the merged checkout (31,103 nodes).
Note: develop carries 2 local docs commits (23de25e56 + eda765c47) not yet pushed to origin/develop.

---

## Where the program stands

| Wave | Scope | State |
| --- | --- | --- |
| 0 — Coordination | peer takeover, worktree cleanup | ✅ done (only #2340's 32-row re-derivation left) |
| 1 — Ownership class | #2393–#2396, #2413, #2414 | ✅ **LANDED 2026-09-08** — 7 PRs, soundness-audited; follow-ups #2449, #2451, #2436 closed; doc rule merged (#2468) |
| 2 — `sm.mu` + pynfs | #2398 + singles | ✅ **LANDED 2026-09-09** — residual #2340 rows + #2329 confirmation tracked below |
| 3 — Shared-layer reuse | errmap, identity, lifecycle | ✅ **LANDED 2026-09-10** — 6 PRs (#2499–#2504), all reviewed green |
| 3.5 — Error-universe consolidation | sentinel normalization + `StatusFor` extraction | ✅ IMPLEMENTED 2026-09-10 — consolidation @ f1d554b26 on origin (78 files, +933/-1423), pynfs v4.1 walkout @ f6e191892 on origin (38→14 rows; 4 #2340 rows remain: CSESS16a/26/27, RECC3); review fan-out running |
| 4 — SMB triage | ~155 untriaged findings | ❌ not started (plan moved it *before* the SMB fix waves) |
| 5 — God objects | `manager.go` 3,985, `Create()` 1,016… | ❌ blocked behind 1–4 |
| 6 — Dispatch-core finish line | acceptance test | lands inside 1/3/5 |
| 7 — Perf lens | bufpool, measurement plan | ❌ not started |

### Wave 2 — final state (all landed 2026-09-09)

| Issue | PR | Squash commit |
| --- | --- | --- |
| #2471 CREATE_SESSION channel sizes | #2494 | `c5dd09bc2` |
| #2482 v4.0/v4.1 client-index fold | #2496 | `a4f479f10` |
| #2487 reclaim-persist retry + off-sm.mu write | #2495 | `8c4895714` |
| #2464 pynfs verified-lease guard + sync watcher refresh | #2497 | `80331c4ea` |
| #2490 LOCK special-stateid BAD_STATEID | #2498 | `7e5cd2e40` |

Earlier Wave 2 singles: #2398 via #2459+#2466, #2362 via #2458, #2369 via #2460, #2382 via #2456,
# 2399 via #2457, #2341 via #2484+#2481, #2340 partial via #2491/#2489/#2486/#2485

**Wave 2 residual (carries into Wave 3+):** #2340's remaining pynfs v4.1 rows (walk out of
KNOWN_FAILURES only on demonstrated CI passes), #2329's pynfs delegation confirmation (server side
landed; residual is pynfs harness-side), and the 32-row re-derivation from Wave 0.

---

## Proposed order (next sessions)

### Step 1 — ~~Close Wave 2~~ ✅ DONE 2026-09-09

All five PRs merged and assigned marmos91; #2490/#2482/#2487/#2471/#2464 auto-closed. #2329's
pynfs delegation confirmation remains open as harness-side verification (no DittoFS change
warranted — verdict in `.planning/2026-09-09-diag-2329-2464.md`).

### Step 2 — Wave 3: shared-layer reuse + lifecycle (goals 2 + 4) — ✅ LANDED 2026-09-10

All six PRs squash-merged (serialized babysitter, order #2499→#2504), no Closes #N (none map to an
open issue). All six reviewed green before open (correctness + adversarial + simplification per
branch; errmap ErrStoreClosed P2 and v4content xdr-import P2 applied inline). Full history:
`.planning/2026-09-09-wave3-plan.md`.

| PR | Content | Squash commit |
| --- | --- | --- |
| #2499 guard: enum walk + ErrConflict row + delete MapLockToNFS3/4 | `6ffc4262b` |
| #2500 5 content-path sites → MapContentToNFS4 | `0a0e96c34` |
| #2501 getUserIdentity → uidGIDFromSessionUser rename | `cc4a5864d` |
| #2502 Stop-path listenerReady close + accept backoff | `7d7033b39` |
| #2503 async grant sites → GrantCredits + SequenceWindow.Grant | `c69818453` |
| #2504 stale CheckExportAccess citation fixes | `0390b3fb8` |

Deferred disclosures carried forward: awaitListener false-'Adapter started' narrow race (#2502 PR
body, service.go outside file set); ResolvedIdentity direct consumption (#2501 PR body).

### Step 2.5 — Wave 3.5: error-universe consolidation (sentinel normalization + `StatusFor` extraction) — LANDED 2026-09-10

Landed as PR #2505 (squash commit `4f418ff83` on develop, 84 files, +1036/−1521) plus the
# 2506 pynfs v4.1 walk-out (`75b45e987`, 38→15 rows: 24 walked out, DELEG26 added for a newly
listed failure) and its #2507 follow-up (`b2927215d`, DELEG26 reason + evidence-tier
disclosure). Reviews were three rounds (two combined correctness+simplification, one
dedicated adversarial per branch); all P1s fixed before PR open: the three enum-walk loops
bound at `merrs.ErrConflict` (red-without-fix proven by temporarily deleting the arm), the
stale mapper-reference sweep across 12 code files + 4 docs, and the Copilot comment fixes
(classify-at-bypass-site comment paths). Post-merge verified: build/vet/gofmt clean, full
go test green, `internal/adapter/common/` holds only `errclassify.go` + `normalize.go` + the
payload helpers; `statusfor.go` + `statusfor_test.go` live in all three types packages.

What landed (differs from the plan below in one place): the enums walks bound at
`merrs.ErrConflict` (the 26th code), `common.ClassifyBlockStoreError` is applied at the
five raw-engine bypass sites + copychunk dest-WriteAt, and each types package exposes
`StatusFor` + `StatusForErr` (error-level wrapper), SMB also `StatusForLock` +
`StatusForLockErr`.

One PR, based on post-merge develop `7d7033b39` (its target files `errmap.go`, `lock_errmap.go`,
`content_errmap.go`, the payload helpers are now at their landed shape). Two parts, zero behaviour
change, ~15 files.

**Part 1 — sentinel normalization at the payload choke point.** `ReadFromBlockStore` /
`WriteToBlockStore` / `CommitBlockStore` (`internal/adapter/common/read_payload.go`,
`write_payload.go`) classify block sentinels as real `*merrs.StoreError` (multi-`%w`, sentinel
preserved in `Cause`): `engine.ErrStoreClosed` → `merrs.ErrStaleHandle`, everything else
(`ErrChunkContentMismatch`, `ErrChunkRefMissing`, `ErrRemoteUnavailable`, unknown) →
`merrs.ErrIOError`. Delete `content_errmap.go` + its test (8 pin tests move/convert); convert the
10 pre-existing `MapContentTo*` call sites plus #2500's five v4 sites to uniform
`MapToNFS3/4/SMB`.

**Part 2 — `StatusFor` extraction into the adapter types packages.** The per-protocol switches
move out of `errmap.go`/`lock_errmap.go` into the packages whose wire codes they produce:

- `internal/adapter/nfs/types`: `StatusFor(merrs.ErrorCode) uint32`
- `internal/adapter/nfs/v4/types`: `StatusFor(merrs.ErrorCode) uint32`
- `internal/adapter/smb/types`: `StatusFor(merrs.ErrorCode) Status` + `StatusForLock(merrs.ErrorCode) Status`

The naming convention **is** the interface — same name, same signature, same contract in every
adapter package; no Go interface is declared because no runtime dispatch exists to consume one
(generics/interfaces were evaluated and rejected: packages aren't type parameters, and every
`common.To(...)` variant adds machinery without adding a property). A new adapter = new package +
one switch + one enum-walk test; `common/` untouched. `StatusForLock` is the one second entry
point, forced by three verified facts: MS-SMB2 3.3.5.14 mandates different lock-failure statuses
(LOCK denial → `STATUS_LOCK_NOT_GRANTED` vs I/O sharing violation → `STATUS_FILE_LOCK_CONFLICT`);
`merrs.ErrLocked` is produced in both contexts (lock manager conflict path and `CheckLockForIO`);
NFS needs no split (`NFS4ERR_DENIED` in both, NFSv3 has no lock procedure). v4's state-machine
mapper (`MapStateError`, `v41/handlers/deps.go`) stays out: it unwraps `NFS4StateError.Status`
carried on the error, not `merrs.ErrorCode`. Delete `errmap.go` + `lock_errmap.go`; convert ~15
call sites to the per-adapter names; one enum-walk test per package (the `walkAllErrorCodes`
pattern from #2499), CI-failing on any code a switch forgot.

**Accepted trade-off (recorded decision):** cross-protocol consistency review moves from one
table row (all three answers in one glance — the property that once caught `ErrLockLimitExceeded`
Jukebox-vs-IO drift) to three enum-walk tests. Completeness stays CI-enforced; consistency across
packages becomes human attention spanning code. Accepted because adapter addition is the actual
open/closed win the extraction buys.

**Supersedes:** the previously queued two-fold tidy-up (`fix/errmap-single-file` lock fold +
content fold) — discarded before landing; it was an intermediate state this extraction rewrites.

### Step 3 — Wave 4: SMB triage — **TRIAGED 2026-09-10** (`.planning/2026-09-10-wave4-triage.md`)

Five read-only lanes re-derived all 157 audit findings against develop (workflow `ec014755`):
**7 rows FIXED (6 defects), 61 rows LIVE (~55 defects after de-dup), 91 rows WAVE5**, 0
DRIFTED/INVALID. Committed as `ff6209016`.

Filed on GitHub 2026-09-10: umbrella **#2511** + seven tranches **#2512-#2518** (T1
set-info/read-write, T1 negotiate/durable, T2 compound/tree, T2 auth/session, T3
dispatch/security, T3/T4 create, T5 adapter+HIGHs), all assigned marmos91.

**Fix-wave 1 fanned out 2026-09-10** (first two workflows `3ba95578`/`5f6757f4` failed at
allocation; relaunch `9c327e4f` recovered B/C mid-work while A cold-start-died and D died on a
malformed sed). Recovery 2026-09-10: C LANDED GREEN (`fix/wave4-compound-tree-2514` @ `12ed93178`,
PR body ready), A LANDED GREEN (`fix/wave4-highs-2518` @ `0f2ec7a4`, PR body ready; first relaunch
implemented all three HIGHs before its worktree was auto-removed, second relaunch re-applied the
saved 368-line patch and pushed). B and D timed out twice before committing; their worktree states
were saved as patches (`/tmp/wave4-setinfo-rw-2512-partial.patch` 739 lines incl. an out-of-scope
pkg/metadata/file_modify.go POSIX EA carve-out to disclose, `/tmp/wave4-sec-hygiene-2516-partial.patch`
579 lines) and relaunched as finish runs `f1a5e821` (B) and `039c0a57` (D). ALL FOUR LANES LANDED
GREEN 2026-09-10: A `fix/wave4-highs-2518` @ `0f2ec7a4`, B `fix/wave4-setinfo-rw-2512` @ `a43158adf`
(land-only run `1904e607` after three timeout/worktree-loss cycles; carries the pkg/metadata
EA carve-out, disclosed), C `fix/wave4-compound-tree-2514` @ `12ed93178`, D
`fix/wave4-security-hygiene-2516` @ `2999df102`. Base drift for the resumed lanes is only
flake.nix + adapter_settings.go (#2510 + v0.31.1) — zero overlap with their SMB files, PRs
rebase trivially at merge. Stale triage worktrees removed, orphaned pi-subagents branches
deleted. Lane
prompts at `.planning/2026-09-10-wave4-fix-prompts.md`. Lanes push branches + /tmp PR bodies;
PRs opened after the review fan-out.

**PRs opened 2026-09-10 after the three-review fan-out**: #2523 (A, `fix/wave4-highs-2518`, Closes
# 2518), #2524 (C, `fix/wave4-compound-tree-2514`, Closes #2514), #2525 (D,
`fix/wave4-security-hygiene-2516`, Closes #2516), #2526 (B, `fix/wave4-setinfo-rw-2512`, Closes
# 2512). Post-review fixes applied before open: A empty-path doc + TREE_DISCONNECT disclosure
(0 P1, triply-reviewed OK); C FLUSH/OplockBreak FileId restored to offset 8 (the verified P1),
per-sub-command credit exemption, dead tests strengthened; D vacuous VNEG MaxOutputResponse pin
made red-without-fix, IS_FSCTL gate-wiring pin, dangling `(refs)` removed; B FileFullEaInformation
exempted from the step-1b attrs gate (own FILE_WRITE_EA check governs), `onlyEAMutations` excludes
explicit timestamps, WRITE offset clamp removed, EA-only pass arm strengthened.

**Squash-merged 2026-09-10**: #2525 as `d4535ad56` (D), #2524 as `a751af2bf` (C, after the round-2
Copilot re-review verified the ReplaceCallback-false early return correct — the CANCEL path drives
the standalone callback, no interim was ever sent on that path so no ordering violation).
Round-2 Copilot fixes pushed on the remaining two: A case-insensitive AppInstanceId share/path
match + pin test (`d0272b01b` — SMB namespaces are case-insensitive, exact match would miss
different-case spellings of the same file); B AppendUint16 return reassigned in the rename test
encoder (`c226454c9` — the original dropped the extended slice, encoding an empty FileName); B's
CreateOptions read-modify-write was already mu-guarded with the struct doc updated.
# 2523/#2526 CI + Copilot re-review pending at chunk end.

**Source-file naming rule (2026-09-10)**: source files are wave/lane/PR-number agnostic
(`.planning/CONVENTIONS-WAVE-TEST-NAMES.md`). Wave-named test files renamed to domain names in
the branches before merge: `wave4_highs_test.go` → `session_durable_test.go` (A),
`wave4_ct_tree_test.go` → `tree_connect_acl_test.go` + `wave4_ct_compound_test.go` →
`compound_integrity_test.go` (C), `wave4_rw_gates_test.go` → `set_info_gates_test.go` (B).
Every future fan-out prompt carries the naming line; reviewers block `wave\d` matches under
`internal/`, `pkg/`, `cmd/`.

Remaining fix-wave 2 candidates (not yet fanned): #2513 negotiate/session+durable (Kerberos
MIC-after-commit ordering, AEAD nonce, unlocked session writes) and #2515 auth/session (NTLMv2
MIC verification, wrong-field credit decoders, oplock break-ack ownership) — both touch
session_setup.go/kerberos_auth.go so they serialize after Lane A lands; #2517 create/post-break
last (touches create.go, also in Lane A's file set).

1. **Priority HIGHs (all three LIVE verbatim, each needs a design decision):** unclaimed
   nonzero SessionId kept at `session_setup.go:976-984`; anonymous/guest encryption bypass at
   `response.go:709-713` (MS-SMB2 3.3.5.2.9 tension); AppInstanceId force-close without
   share/path/access scoping at `durable_context.go:907,970` (MS-SMB2 3.3.5.9.13).
2. **Security cluster:** Kerberos MIC-after-commit (`kerberos_auth.go:290`), malformed-ACE
   slice-bounds panic (`security.go:929`), oplock break-ack ownership
   (`stub_handlers.go:959`), wrong-field credit decoders (`credit_validation.go:98-112`),
   TOCTOU create-race overwrite (`create_post_break.go:797`), parked-CREATE mid-chain resume
   hang (`create_post_break.go:1639`), AEAD nonce reuse (`encryption/middleware.go:189`).
3. **Small LIVE rows** (stale doc.go, issue-number comment citations, Debug-swallowed lease
   errors) batch into one hygiene PR.
4. **The 91 WAVE5 rows are Wave 5 input** — do not open fix PRs for them here; they feed the
   god-object/move-only wave with updated line numbers.
- Credit/sequencing: the plan's `smb/state/` + `handlers/create/` + `handlers/info/` split is Wave 5;
  triage decides what joins it

### Step 4 — Wave 5: god objects, one package per move-only PR

`manager.go` 3,985 → `setFileInfoFromStore` 1,538 → `Create()` 1,016 → `open.go` 1,103 →
`completeCreateAfterBreak` 853 → `Handler` 56 fields. Apparatus is load-bearing (119 test files /
42,810 LOC in `smb/handlers`): move-only, `git diff -M --color-moved` zero body edits, one package
per PR merged same day, `graphify update .` after each, CI guard requiring a `_test.go` per new
package. Every extracted function gets its own test (no 150-LOC ceiling).

### Step 5 — Wave 7: perf lens (goal 3)

No measurement plan exists. Build it before touching anything: connection ramp both protocols,
v4.1 >64 concurrent ops per session, large-I/O allocation rate toward 16MB. Then `bufpool` tier
sizing (large tier `1<<20` == advertised max I/O; every max-size READ/WRITE falls off the pool).
Issue 2423 (adaptive controller samples the wrong window) belongs here with #2398's axis.

### Continuously (cross-cutting, slot into any lull)

- `auxsvc` lock fix (`Group.Start` holds `g.mu` across `s.Start()`); rename → `sidecar`
- Conformance CI gate: non-empty Reason+Issue per known-failure row (`kf_load`); knfsd/Samba
  comparison per SMB suite; #2322 suite unification
- Instrumentation: per-op RED inside the NFSv4 COMPOUND loop; pprof on `pkg/metrics/server.go`
- Hygiene sweeps: 962 banner separators, signature-restating doc comments, phantom-symbol citations
- #2437 (v4.0 has no trusted client identity) — the known ceiling on Wave 1's guards; needs a
  design decision, not a patch. Wave 1's stateid semantics have settled, so it can be scheduled.
- #2441 (`ErrCrossShare` code + errmap rows so SMB matches NFS's XDEV) — small, pairs with Step 2
- #2397 (GDD4_OK but no CB_NOTIFY) and #2442 (SEEK ISDIR for non-regular files) — protocol gaps
- #2483 (seqid=0 v4.1 bypass applied to v4.0) — the last Wave-2 stateid item; touches all seqid
  ops in `v4/state`, one writer after Step 2 settles

Not this quarter: #2423 beyond the measurement, #2353 (RAG substrate), #2324, #2320, #2322 (beyond
the gate), #2345, #2258, #2215, #2263, #117, and the pre-audit backlog (issues 1417, 1418,
1454, 1489, 1290, 1199, 1603).

---

## Standing hazards (unchanged, carry into every prompt)

- Audits pinned at `40884ad4f`, predating #2356 — re-verify premises on develop
- Postgres tests are `//go:build integration`; a green `go test ./...` proves nothing about postgres
- #2345: DSN is a boolean gate; check `pg_stat_activity` before any dropdb — sessions may be live
- Count `gh pr checks`, not failures; a stacked PR runs zero checks
- Merge via `PUT pulls/{n}/merge -f sha=<re-read head>`; sign every commit; rebase, never merge
- Protocol/e2e suites are exclusive — brokered, one agent at a time
- Never re-count the known-failures ledger files: read each file's own tally (178 = SMB 86 + NFS 92)
