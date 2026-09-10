# Memory — Wave 2 closed, Wave 3 prepared (2026-09-09 ~23:58)

## Reflections to record

- Wave 2 CLOSED 2026-09-09: all five PRs (#2494/#2495/#2496/#2497/#2498) squash-merged to
  develop @ `7e5cd2e40`, issues #2471/#2482/#2487/#2464/#2490 auto-closed, all assigned
  marmos91, worktrees + remote branches deleted. Residual: #2340's remaining pynfs v4.1 rows,
  #2329's harness-side delegation confirmation, 32-row Wave 0 re-derivation.
- Wave 3 PREPARED: lane plan at `.planning/2026-09-09-wave3-plan.md`; premises re-verified on
  develop (closure doc `.planning/2026-09-09-wave2-closure.md`). Key corrections vs the
  roadmap text: (a) grantAdaptive is NOT dead — the real finding is response.go async paths
  bypassing Session.credits; (b) ErrStoreClosed → STALE already lives in lookupErrorRow —
  the bypass is NFSv4 handlers returning literal NFS4ERR_IO; (c) ErrorCode is a contiguous
  iota so the enum walk `for c := ErrNotFound; c <= ErrConflict` is valid for the
  TestErrorMapCoverage rewrite (types 25, enum now 26); (d) the roadmap's "not this quarter"
  backwalog typo is fixed.
- Local develop carries 3 unpushed docs commits (23de25e56, eda765c47, 8f8fd4332);
  chore/pi-migration @ 8f8fd4332 tracks them for the docs bookkeeping branch.
- Pre-existing stashes on refactor/nfs-dead-code (foreign smb pending_create_registry.go),
  refactor/smb-1055-v2 (pr12-wip), replay-rebase are UNRELATED — do not touch.
