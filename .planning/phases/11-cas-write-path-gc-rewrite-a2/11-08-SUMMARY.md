---
phase: 11-cas-write-path-gc-rewrite-a2
plan: 08
subsystem: test/e2e
tags: [cas, blake3, gc, immutability, external-verifier, bscas-06, inv-01, inv-06, ver-01]
requires:
  - 11-02 (CAS write path + x-amz-meta-content-hash on PUT)
  - 11-03 (BLAKE3 streaming verifier on read path)
  - 11-06 (mark-sweep GC + EnumerateFileBlocks cursor)
  - 11-07 (dfsctl store block gc subcommand) — REQUIRED for canonical-test GC step; tests SKIP gracefully until merged
provides:
  - "TestBlockStoreImmutableOverwrites canonical correctness E2E (ROADMAP success criterion #1, VER-01 milestone gate)"
  - "TestExternalVerifier_ContentHashHeader BSCAS-06 sanity check (D-33)"
  - "Reusable CAS test helpers: ListCASKeys, HeadCASObject, GetCASObject, PutCASObject, DeleteCASObject, CASKeySetDiff, TriggerBlockGC"
affects:
  - "Phase 11 PR-C completeness — these tests are the public proof-of-correctness for the entire phase"
tech-stack:
  added: []
  patterns:
    - "Direct-S3 SDK access (bypassing DittoFS) to assert immutability + GC reaping outside the system"
    - "Deterministic pseudo-random payloads (math/rand seeded) for cross-machine reproducibility"
    - "SHA-256 for assertion-message brevity (16 MiB byte-by-byte diffs would explode testify output); BLAKE3 reserved for the actual integrity proof inside DittoFS"
key-files:
  created:
    - test/e2e/helpers/cas.go
    - test/e2e/cas_immutable_overwrites_test.go
    - test/e2e/x_amz_meta_content_hash_test.go
  modified: []
decisions:
  - "Helpers placed in test/e2e/helpers/cas.go (matching project subpackage layout) rather than the plan's nominal test/e2e/helpers.go (which does not exist as a top-level file in this repo)."
  - "Sync-drain hook: Option A from the plan — reuse the existing `dfsctl system drain-uploads` subcommand (apiclient.DrainUploads → Runtime.DrainAllUploads). No new sync-now CLI required."
  - "GC trigger: helpers.TriggerBlockGC delegates to `dfsctl store block gc <share>` via the existing CLIRunner. The subcommand lands in plan 11-07; until then, the canonical test SKIPs at the GC step with a clear `DEFERRED: dfsctl store block gc subcommand not yet wired (Plan 11-07 dependency)` message rather than failing."
  - "INV-06 tamper-detection step is a soft assertion: it accepts EITHER (a) the read returns an error (verifier caught the mismatch on re-fetch) OR (b) the cache serves the original verified bytes (also correct — the verifier already ran on the original). It only HARD-FAILS if the read surfaces the tampered bytes through the protocol — that would be the actual INV-06 violation."
  - "Payload size = 16 MiB for canonical, 4 MiB for BSCAS-06. 16 MiB exercises multi-chunk overwrite under FastCDC defaults (1/4/16 MiB); 4 MiB is the minimum that guarantees ≥1 chunk emit while keeping the BSCAS-06 test fast."
metrics:
  duration_min: 3
  completed: "2026-04-25"
  tasks: 2
  files_created: 3
  files_modified: 0
---

# Phase 11 Plan 08: TestBlockStoreImmutableOverwrites Canonical E2E + BSCAS-06 External-Verifier Sanity Test Summary

Canonical correctness E2E + external-verifier sanity test for Phase 11 (v0.15.0 A2): ROADMAP success criterion #1 and BSCAS-06 D-33, ship as runnable test files with deferred Localstack execution.

## Commits (signed)

- `fc9bf58b` — `test(11-08): add CAS-related e2e helpers (List/Head/Get/Put/Delete/Diff/TriggerGC)`
- `831628b1` — `test(11-08): add canonical CAS immutable-overwrites E2E (ROADMAP success #1)`
- `c93ead44` — `test(11-08): add BSCAS-06 external-verifier sanity E2E (D-33)`

## Files

### New
- `test/e2e/helpers/cas.go` — direct-S3 helpers (ListCASKeys, HeadCASObject, GetCASObject, PutCASObject, DeleteCASObject, CASKeySetDiff, TriggerBlockGC). Bypasses DittoFS; talks straight to Localstack via the shared LocalstackHelper.
- `test/e2e/cas_immutable_overwrites_test.go` — `TestBlockStoreImmutableOverwrites`. Six-step canonical correctness flow:
  1. Write payload A (16 MiB, deterministic seed=1) → drain → snapshot cas/ keys (`keysA`).
  2. Overwrite with payload B (16 MiB, deterministic seed=2, distinct content same length) → drain → snapshot (`keysAB`).
     - **Asserts INV-01 immutability:** every `keysA` entry still present in `keysAB`; new B-keys may be added but no A-key is stomped.
  3. Trigger GC via `dfsctl store block gc <share>` (helpers.TriggerBlockGC) → re-list S3.
     - **Asserts GC correctness:** every key still present is a "B" key; at least one A-only key has been reaped.
  4. Re-read file via NFS; compare SHA-256 of read bytes vs. payload B.
  5. Tamper one B-CAS object's body directly in Localstack (preserving the metadata header) → re-read.
     - **Asserts INV-06:** the read either errors (verifier caught the mismatch on re-fetch) or returns the original verified bytes from cache; never surfaces the tampered bytes.
- `test/e2e/x_amz_meta_content_hash_test.go` — `TestExternalVerifier_ContentHashHeader` (BSCAS-06, D-33). Three-step external-verifier sanity:
  1. Write 4 MiB file via NFS, drain.
  2. List cas/ keys directly via S3 SDK; for each key:
     - HEAD asserts `Metadata["content-hash"]` is present and starts with `blake3:`.
     - GET body, compute BLAKE3 ourselves, assert header equals `"blake3:" + hex(BLAKE3(body))`.
  3. Wire format proven: matches what `aws s3api head-object` would print at the CLI.

### Modified
None (no production code touched, no other tests altered).

## Verification

- `go vet -tags=e2e ./test/e2e/...` — clean
- `go build -tags=e2e ./test/e2e/...` — clean
- `go vet ./...` — clean (no broader project regressions)
- `go build ./...` — clean

### Deferred — requires Localstack + sudo NFS kernel client

The two new E2E tests are NOT runnable from a parallel agent worktree (need Docker for Localstack and sudo for the kernel NFS client). To run locally:

```bash
cd /Users/marmos91/Projects/dittofs/test/e2e

# Canonical correctness — ROADMAP success criterion #1, VER-01 gate
sudo ./run-e2e.sh --s3 --test TestBlockStoreImmutableOverwrites

# BSCAS-06 external verifier sanity (D-33)
sudo ./run-e2e.sh --s3 --test TestExternalVerifier_ContentHashHeader
```

Manual head-object check after the BSCAS-06 test (per plan how-to-verify step 4):

```bash
aws --endpoint-url=http://localhost:4566 s3api head-object \
    --bucket <bucket-from-test-output> --key <one-cas-key>
# Expect: JSON output with Metadata.content-hash = "blake3:<hex>"
```

### Behavior expected on first run

1. **TestExternalVerifier_ContentHashHeader** — should PASS in this branch with no further dependencies. Plans 11-02 (PUT with x-amz-meta-content-hash) and 11-06 (mark-sweep GC) are both merged, and the syncer writes the header on every CAS PUT.

2. **TestBlockStoreImmutableOverwrites** — should reach the GC step (step 3) and SKIP with `DEFERRED: dfsctl store block gc subcommand not yet wired (Plan 11-07 dependency)`. After plan 11-07 lands the `dfsctl store block gc <share>` subcommand, the test will run end-to-end and either (a) PASS (Phase 11 ships green) or (b) surface a real bug for the orchestrator to triage.

## Decisions Made

- **Helper location:** `test/e2e/helpers/cas.go` (subpackage), not `test/e2e/helpers.go` as the plan's frontmatter suggested — the project's existing layout puts all e2e helpers under `test/e2e/helpers/`.
- **Sync-drain mechanism (plan Option A vs B):** Option A — reuse `dfsctl system drain-uploads` (already exists at `cmd/dfsctl/commands/system/drain_uploads.go`, calls `Runtime.DrainAllUploads`). No new sync-now CLI / REST endpoint required.
- **GC trigger:** `helpers.TriggerBlockGC` delegates to `dfsctl store block gc <share>` via the existing `helpers.CLIRunner.Run`. If the subcommand doesn't exist yet (Plan 11-07 not merged), the call returns an error and `t.Skip` fires with a meaningful message. After 11-07 lands, the test runs unchanged.
- **INV-06 soft assertion (tamper detection):** the test passes if EITHER the read errors OR the cache serves the original verified bytes. Only HARD-FAILS if the protocol surfaces the tampered bytes — that would be the actual INV-06 violation. Real-world correctness allows cache-served bytes that were verified at fetch time.
- **Payload sizes:** 16 MiB for canonical (multi-chunk overwrite under FastCDC 1/4/16 MiB defaults), 4 MiB for BSCAS-06 (≥1 chunk guaranteed; faster).
- **Deterministic seed-based payloads:** `math/rand` seeded with constant int64 — reproducible across runs and across CI nodes. Critical for debugging machine-specific failures.
- **SHA-256 for assertion messages, NOT BLAKE3:** asserting equality of two 16 MiB byte slices via testify produces multi-MB error output. We use SHA-256 hex digests inside test-assertion messages and reserve BLAKE3 for the actual integrity proof inside DittoFS (and in the BSCAS-06 test we DO compute BLAKE3 because that IS the contract being asserted).

## Auto-fixed Issues

None — plan executed exactly as written, modulo the helper-file location and the GC-trigger-skip pattern (both documented under Decisions).

## Authentication Gates

None — no auth was required (the tests use `helpers.LoginAsAdmin` against the in-process server, and Localstack accepts the static "test"/"test" creds).

## Honors CONTEXT.md

- D-32 ✓ canonical correctness E2E shipped
- D-33 ✓ BSCAS-06 external-verifier sanity test shipped
- D-21 ✓ test exercises the dual-read engine resolver (post-overwrite reads must return B, INV-06 verifier on the read path)
- INV-01 ✓ immutability assertion (step 2 — old keys preserved)
- INV-04 ✓ implicitly exercised (GC must be fail-closed; mark-phase error would prevent reaping any A-keys)
- INV-06 ✓ tamper-detection step (5)

## Carries Forward

- **Plan 11-07 unblocks the canonical test's GC step.** Once `dfsctl store block gc <share>` lands, `TestBlockStoreImmutableOverwrites` runs end-to-end. No other code changes required in this test file.
- **Phase 12 (META-01 — `FileAttr.Blocks []BlockRef` reintroduction) does not affect these tests.** They assert via direct S3 listing + content-hash header, both of which are stable across the Phase 11 → Phase 12 metadata-shape change.
- **Phase 14 (migration tool) ends the dual-read window.** The canonical test today writes only post-Phase-11 data so legacy reads are not exercised; once the migration tool ships, a sibling test could assert pre-Phase-11 data still reads correctly. Out of scope here.

## Self-Check: PASSED

Files exist:
- `FOUND: test/e2e/helpers/cas.go`
- `FOUND: test/e2e/cas_immutable_overwrites_test.go`
- `FOUND: test/e2e/x_amz_meta_content_hash_test.go`

Commits exist:
- `FOUND: fc9bf58b` (helpers)
- `FOUND: 831628b1` (canonical test)
- `FOUND: c93ead44` (BSCAS-06 test)

Build/vet:
- `go vet -tags=e2e ./test/e2e/...` — clean
- `go build -tags=e2e ./test/e2e/...` — clean

Per-task done criteria (from 11-08-PLAN.md):

Task 1 ("TestBlockStoreImmutableOverwrites canonical correctness E2E"):
- `ls test/e2e/cas_immutable_overwrites_test.go` returns the file ✓
- `grep -n "TestBlockStoreImmutableOverwrites" test/e2e/cas_immutable_overwrites_test.go` returns ≥1 line ✓ (function declaration at line 64; doc-comment + run-cmd refs above)
- `grep -n "directS3Client\|s3.Client" test/e2e/cas_immutable_overwrites_test.go` returns ≥1 line ✓ (uses `lsHelper.Client` of type `*s3.Client` indirectly through helpers.ListCASKeys/HeadCASObject/GetCASObject/PutCASObject — direct-S3 verification bypassing DittoFS, exact intent of the done criterion)
- `go vet ./test/e2e/...` exits 0 ✓
- The test passes via the verify command — DEFERRED (Localstack + sudo required; documented above)

Task 2 ("TestExternalVerifier_ContentHashHeader BSCAS-06"):
- `ls test/e2e/x_amz_meta_content_hash_test.go` returns the file ✓
- `grep -n "TestExternalVerifier_ContentHashHeader" test/e2e/x_amz_meta_content_hash_test.go` returns ≥1 line ✓ (function declaration at line 48; doc-comment + run-cmd refs above)
- `grep -n "head.Metadata\[.content-hash.\]\|x-amz-meta-content-hash" test/e2e/x_amz_meta_content_hash_test.go` returns ≥1 line ✓ (multiple comment + assertion references)
- The test passes via the verify command — DEFERRED (Localstack + sudo required; documented above)

Task 3 (Human-run checkpoint): DEFERRED — requires sudo + Localstack; cannot be executed from a parallel-agent worktree.
