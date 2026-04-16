---
phase: 03-destination-drivers-encryption
plan: 04
subsystem: backup

tags: [s3, aws-sdk-go-v2, multipart-upload, aes-256-gcm, sha-256, destination-driver, backup, localstack]

# Dependency graph
requires:
  - phase: 03-destination-drivers-encryption
    provides: "Destination interface + BackupDescriptor + Factory registry + D-07 sentinels (plan 01); SHA-256 tee + AES-256-GCM envelope + key-ref resolver (plan 02)"
  - phase: 01-foundations-models-manifest-capability-interface
    provides: "manifest v1 codec, BackupRepo.GetConfig/SetConfig, BackupRepoKindS3 discriminator"
provides:
  - "S3 destination.Destination driver (backup/destination/s3.Store) with manager.Uploader-backed streaming multipart payload, manifest-last publish marker, SHA-256-over-ciphertext verification, optional AES-256-GCM envelope"
  - "D-13 bucket/prefix collision hard-reject via narrow blockStoreLister interface, reading cfg[\"prefix\"] to match the real block-store JSON shape (PITFALL #8 guard)"
  - "classifyS3Error mapping smithy.APIError codes (AccessDenied/SlowDown/NoSuchBucket/5xx) and HTTP status fallbacks to D-07 sentinels"
  - "sweepOrphans on New() — deletes payload-without-manifest older than grace_window + aborts stale multipart uploads; warn-only bucket lifecycle check nudges operator to add AbortIncompleteMultipartUpload rule"
  - "Shared-Localstack integration harness (TestMain + sharedHelper) following MEMORY.md no-per-test-containers rule"
affects: [04-scheduler-retention, 05-restore-orchestration, 06-cli-rest-apiclient]

# Tech tracking
tech-stack:
  added:
    - "github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.20.0 (multipart upload streaming)"
    - "github.com/aws/smithy-go v1.23.2 (promoted to direct — APIError + ResponseError types)"
  patterns:
    - "Producer goroutine builds encrypt+hash pipeline inline to avoid io.Pipe deadlock on eager envelope-header writes"
    - "Narrow sub-interface (blockStoreLister) local to the driver for D-13 collision check — same culture as pkg/controlplane/store/interface.go"
    - "Duplicate-over-premature-refactor: buildS3Client body mirrors pkg/blockstore/remote/s3.NewFromConfig verbatim; normalizeEndpoint/isValidScheme/hashTeeWriter/verifyReader all duplicated with comment links to source"

key-files:
  created:
    - "pkg/backup/destination/s3/store.go — Store, Config, New, PutBackup, GetBackup, List, Stat, Delete, ValidateConfig, sweepOrphans (825 lines)"
    - "pkg/backup/destination/s3/errors.go — classifyS3Error + isNotFound"
    - "pkg/backup/destination/s3/collision.go — blockStoreLister + checkPrefixCollision (D-13)"
    - "pkg/backup/destination/s3/hash.go — hashTeeWriter + verifyReader + verifyReadCloser"
    - "pkg/backup/destination/s3/store_unit_test.go — non-network unit tests"
    - "pkg/backup/destination/s3/store_integration_test.go — 10 integration tests"
    - "pkg/backup/destination/s3/localstack_helper_test.go — shared Localstack harness"
  modified:
    - "go.mod / go.sum — added feature/s3/manager@v1.20.0, promoted smithy-go to direct"

key-decisions:
  - "Duplicated pkg/blockstore/remote/s3 client bootstrap (buildS3Client) instead of factoring into internal/awsclient — two users is not yet three; matches 02-PATTERNS precedent."
  - "Build the encrypt+hash pipeline INSIDE the producer goroutine (not on the caller). NewEncryptWriter writes the 9-byte D-05 envelope header synchronously during construction; calling it before spawning the producer deadlocks the unbuffered io.Pipe because the uploader hasn't started consuming yet. Exposed by the encrypted-multipart integration test."
  - "Orphan sweep on New() runs asynchronously in a background goroutine with a 2-minute timeout so a slow or misbehaving bucket cannot block server startup."
  - "Integration tests live in package s3 (not s3_test) so they can share the unexported sharedHelper + WithClock test seam; public-API-only surface still covered through New/PutBackup/GetBackup/etc."
  - "Bucket-name sanitisation in uniqueBucket strips BOTH '/' (subtest separator) AND '_' (common in Go test names) because Localstack enforces S3 naming strictly (tests initially failed with InvalidBucketName)."
  - "multipart_part_size = 5 MiB, concurrency = 5 — SDK defaults, matches D-02 discretion. Revisit under benchmark in Phase 7."

patterns-established:
  - "S3 destination driver layout: store.go + errors.go + collision.go + hash.go (duplicated tee+verify) — re-usable template if Azure/GCS drivers are ever added."
  - "D-07 error classification via smithy.APIError switch + smithyhttp.ResponseError status fallback + net.Error network-class catch."
  - "Integration test bucket naming: 'bkp-' + lowercased-sanitised-t.Name() + ULID suffix, trimmed to 63 chars."

requirements-completed: [DRV-02, DRV-03, DRV-04]

# Metrics
duration: 21min
completed: 2026-04-16
---

# Phase 03 Plan 04: S3 Destination Driver Summary

**S3 destination.Destination driver: streaming multipart payload via manager.Uploader, manifest-last publish marker, SHA-256-over-ciphertext verify, optional AES-256-GCM envelope, D-13 bucket/prefix collision hard-reject, D-06 orphan + MPU sweep on startup.**

## Performance

- **Duration:** ~21 min (first commit 14:27:38, last commit 14:41:04)
- **Started:** 2026-04-16T14:27:00Z
- **Completed:** 2026-04-16T14:41:04Z
- **Tasks:** 3 (plus 1 deadlock fix, 1 comment reword)
- **Files created:** 7 (store.go, errors.go, collision.go, hash.go, store_unit_test.go, store_integration_test.go, localstack_helper_test.go)
- **Files modified:** 2 (go.mod, go.sum)
- **Lines of Go added:** 1,885 (1,119 production, 766 test)

## Accomplishments

- Complete S3 destination driver satisfying destination.Destination (7 methods) with D-02 two-phase commit (payload-first-then-manifest via manager.Uploader), D-04 hash-over-ciphertext, D-05 streaming GCM envelope (via shared envelope.go), D-06 orphan+MPU sweep, D-12 field names verbatim, D-13 hard-reject prefix collisions.
- classifyS3Error maps SDK errors to D-07 sentinels at every call boundary — concrete regression tests assert AccessDenied → ErrPermissionDenied, SlowDown → ErrDestinationThrottled, NoSuchBucket → ErrIncompatibleConfig using real smithy.GenericAPIError instances.
- collision.go reads cfg["prefix"] (the real JSON key registered block stores use per shares/service.go:1013) — guarded by a regression test that builds the fake block-store config with exactly the same key, so the check is demonstrably reading what production persists.
- 10 integration tests pass against a fresh Localstack container in ~7 seconds end-to-end, including multipart-forcing 7 MiB encrypted roundtrip, tampered-payload SHA256 mismatch, missing-manifest, orphan sweep with fast-forward clock, duplicate-ID rejection, wrong-key decrypt failure, and ValidateConfig happy/missing-bucket.
- 15 unit tests (non-network) pass for Config parsing, prefix/endpoint normalisation, collision table (8 cases including the empty-prefix catastrophic regression), error classification, and isNotFound.

## Task Commits

1. **Task 1: Implement s3.Store, Config, errors.go, collision.go** — `5d911f24` (feat)
2. **Task 2: Unit tests (no network)** — `75b5fca9` (test)
3. **Deadlock fix: build encrypt/hash pipeline inside producer goroutine** — `d25ad5af` (fix — Rule 1)
4. **Task 3: Integration tests via shared Localstack** — `4e4b9eee` (test)
5. **Comment reword: avoid m.Validate() grep false positive** — `ebb44cfa` (docs)

## Files Created/Modified

- `pkg/backup/destination/s3/store.go` — Store struct, Config (D-12 field names), New factory, PutBackup (D-02 two-phase commit), GetBackup (verify-while-streaming), List/Stat/Delete, ValidateConfig (D-13 collision + D-06 lifecycle warning), sweepOrphans
- `pkg/backup/destination/s3/errors.go` — classifyS3Error, isNotFound
- `pkg/backup/destination/s3/collision.go` — blockStoreLister narrow interface, checkPrefixCollision (reads cfg["prefix"])
- `pkg/backup/destination/s3/hash.go` — hashTeeWriter (SHA-256-tee), verifyReader (hash-on-read + mismatch-on-close), verifyReadCloser (wraps S3 body + decrypt chain)
- `pkg/backup/destination/s3/store_unit_test.go` — parseConfig/parseGrace/normalizePrefix/normalizeEndpoint tables, checkPrefixCollision table with 8 cases, classifyS3Error concrete-code assertions, isNotFound code table
- `pkg/backup/destination/s3/localstack_helper_test.go` — sharedHelper, TestMain, startSharedLocalstack (LOCALSTACK_ENDPOINT opt-out), createBucket, deleteBucket (drains objects + aborts stale MPUs before DeleteBucket)
- `pkg/backup/destination/s3/store_integration_test.go` — 10 integration tests in package s3, uniqueBucket sanitiser (strips / and _), linear orphan-sweep setup
- `go.mod`, `go.sum` — added feature/s3/manager v1.20.0, promoted smithy-go to direct

## Decisions Made

### Factoring decision: duplicated buildS3Client, not factored
The plan gave the planner discretion on factoring AWS client construction into internal/awsclient/ vs duplicating. Chose duplication — matches 02-PATTERNS precedent "duplicate over premature refactor", keeps the two users (block-store remote, backup destination) independent so changes to one don't risk the other, and the duplicated body is ~80 lines — small enough to audit in one pass.

### manager.Uploader tuning
PartSize = 5 MiB and Concurrency = 5 — both SDK defaults. D-02 gave latitude to tune to 8 MiB (matching block.Size) but without a benchmark it's premature optimisation; revisit in Phase 7 once end-to-end throughput is measurable.

### Pipeline built inside producer goroutine
Originally built the encrypt+hash pipeline on the calling goroutine before spawning the producer, matching the plan's skeleton verbatim. The encrypted-7-MiB integration test hung at NewEncryptWriter's header write because the uploader hadn't started consuming the pipe yet. Moved the pipeline construction inside the goroutine — cleaner separation anyway, and sha / size are captured via outer-scope variables just before successful pipe close.

### Integration tests in package s3 (not s3_test)
The plan example used `package s3_test`, but sharedHelper / WithClock are unexported and the test binary needs access. Moved to `package s3`. The tested surface is still strictly the public API (New / PutBackup / GetBackup / ValidateConfig / List / Stat / Delete) — in-package placement does not expand coverage.

### Smithy APIError for classifier tests
Used real smithy.GenericAPIError values in the classifyS3Error tests rather than ad-hoc fake types. The plan explicitly asks for concrete assertions; using the SDK's own type guarantees the tests will track any future smithy API changes rather than paper over them.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] PutBackup deadlocked on encrypted payloads**
- **Found during:** Task 3 integration tests — TestIntegration_S3_Roundtrip_Encrypted hung at the 7 MiB multipart boundary.
- **Issue:** NewEncryptWriter writes the 9-byte D-05 envelope header synchronously during construction. The plan's skeleton constructed the writer on the calling goroutine BEFORE spawning the producer, so the header write blocked on the unbuffered io.Pipe — the uploader hadn't started consuming the pipe reader yet.
- **Fix:** Build the encrypt+hash pipeline inside the producer goroutine. Key resolution stays up front (so config errors short-circuit cleanly) but cipher.NewGCM and the pipe writes all happen concurrently with the uploader. Outer-scope sha/size variables capture the hash-tee state just before successful pipe close.
- **Files modified:** pkg/backup/destination/s3/store.go
- **Verification:** All 10 integration tests pass (~7s end-to-end). Unit tests unchanged.
- **Committed in:** `d25ad5af` (separate commit after Task 1 — tightly coupled to Task 3 integration tests that exposed it)

**2. [Rule 1 - Bug] Localstack rejected bucket names with underscores**
- **Found during:** Task 3 first integration test run.
- **Issue:** `t.Name()` for `TestIntegration_S3_Roundtrip_Unencrypted` produces a string with underscores; S3 (and Localstack) enforces lowercase + no underscores in bucket names. All 10 tests failed with InvalidBucketName.
- **Fix:** uniqueBucket now replaces BOTH '/' (subtest separator) and '_' (common in Go test names) with '-', and truncates at 63 chars.
- **Files modified:** pkg/backup/destination/s3/store_integration_test.go
- **Verification:** All 10 integration tests pass; bucket names like `bkp-testintegration-s3-roundtrip-unencrypted-01kpb4ft` are valid S3 names.
- **Committed in:** `4e4b9eee` (landed with the Task 3 integration test commit — the sanitiser is part of the fixture code)

**3. [Rule 2 - Missing Critical] Post-upload manifest fields now read from outer-scope variables**
- **Found during:** Deadlock fix (related to auto-fix 1).
- **Issue:** After moving the hash-tee construction inside the goroutine, the post-upload code was left pointing at a now-out-of-scope `tee` variable.
- **Fix:** Added outer-scope `sha` / `size` variables set inside the goroutine just before it closes the pipe successfully; PutBackup reads them back into `m.SHA256` / `m.SizeBytes` after the producer channel drains.
- **Files modified:** pkg/backup/destination/s3/store.go
- **Verification:** Round-trip tests confirm manifest.SHA256 is populated correctly.
- **Committed in:** `d25ad5af` (same commit as fix 1)

**4. [Rule 3 - Blocking] Comment referencing m.Validate() blocked an acceptance grep**
- **Found during:** Acceptance verification run.
- **Issue:** The plan's verification step `grep 'm\.Validate()' pkg/backup/destination/s3/store.go returns 0 matches` was catching an explanatory comment that said "m.Validate() is NOT called here".
- **Fix:** Reworded the comment to say "the manifest's full validator" — keeps intent for readers, clears the mechanical check.
- **Files modified:** pkg/backup/destination/s3/store.go
- **Verification:** `grep -c 'm\.Validate()' pkg/backup/destination/s3/store.go` now returns 0.
- **Committed in:** `ebb44cfa` (docs commit)

---

**Total deviations:** 4 auto-fixed (3× Rule 1 bug, 1× Rule 3 blocking-acceptance)
**Impact on plan:** Deviations 1 and 3 are the same root cause (unbuffered io.Pipe + eager header writes) surfacing as two symptoms. Deviation 2 is an environmental mismatch with Localstack strict naming. Deviation 4 is cosmetic (comment wording). None expanded scope — the core design is unchanged.

## Issues Encountered

### buildS3Client response-type mismatch
Initial draft had a nil-check on `awsconfig.LoadDefaultConfig` return that the compiler flagged as nonsensical — removed to match the upstream pkg/blockstore/remote/s3 pattern.

### feature/s3/manager version compatibility
The default `go get` upgraded the entire aws-sdk-go-v2 to v1.41.5, which was a larger bump than desired. Reverted and pinned feature/s3/manager@v1.20.0 which requires aws-sdk-go-v2 v1.39.4 (compatible with our v1.39.6).

## Known Stubs
None — all seven Destination methods are fully implemented and exercised by integration tests.

## Threat Flags
None — the driver only introduces surface already threat-modelled in the plan's threat_register (T-03-21 through T-03-29). No new endpoints, auth paths, file-access patterns, or schema changes.

## Self-Check

- [x] store.go contains `var _ destination.Destination = (*Store)(nil)` — FOUND
- [x] store.go contains `manager.NewUploader(` — FOUND (5 matches)
- [x] store.go contains `s3.NewFromConfig(` — FOUND
- [x] store.go contains `"bucket"`, `"prefix"`, `"access_key"` JSON field names — FOUND (7 matches)
- [x] store.go contains `payload.bin` and `manifestName       = "manifest.yaml"` — FOUND
- [x] store.go contains `ListMultipartUploads` + `AbortMultipartUpload` — FOUND
- [x] store.go contains `GetBucketLifecycleConfiguration` + `AbortIncompleteMultipartUpload` — FOUND
- [x] store.go contains `destination.NewEncryptWriter` + `destination.NewDecryptReader` — FOUND
- [x] store.go contains `key[i] = 0` (two occurrences — Put + Get paths) — FOUND
- [x] store.go contains `multipartPartSize = 5 * 1024 * 1024` — FOUND
- [x] `grep m\.Validate() pkg/backup/destination/s3/store.go` returns 0 — CONFIRMED
- [x] errors.go contains ErrPermissionDenied / ErrDestinationThrottled / ErrDestinationUnavailable — FOUND
- [x] collision.go contains `blockStoreLister interface`, `BlockStoreKindRemote`, `strings.HasPrefix` — FOUND
- [x] `grep cfg\["prefix"\] pkg/backup/destination/s3/collision.go` returns 1 — CONFIRMED
- [x] `grep cfg\["key_prefix"\] pkg/backup/destination/s3/collision.go` returns 0 — CONFIRMED
- [x] `grep fakeAPIErr pkg/backup/destination/s3/store_unit_test.go` returns 0 — CONFIRMED
- [x] `go build ./pkg/backup/destination/s3/...` exits 0 — CONFIRMED
- [x] `go vet ./pkg/backup/destination/s3/...` exits 0 — CONFIRMED
- [x] `go build -tags=integration ./pkg/backup/destination/s3/...` exits 0 — CONFIRMED
- [x] `go vet -tags=integration ./pkg/backup/destination/s3/...` exits 0 — CONFIRMED
- [x] Unit tests pass: `go test ./pkg/backup/destination/s3/` — ok (15 tests)
- [x] Integration tests pass against fresh Localstack: 10/10 in ~7s — CONFIRMED
- [x] Commits exist: 5d911f24, 75b5fca9, d25ad5af, 4e4b9eee, ebb44cfa — FOUND (git log --oneline)

**Self-Check: PASSED**

## Next Phase Readiness

Wave 3 of Phase 3 is now complete (plans 03 and 04 land in parallel — fs + s3 drivers). Wave 4 (plan 05 — driver registration + CLI wiring) and Wave 5 (plan 06 — phase-complete wrap-up) can proceed against:

- `destination.Destination` concretely implemented for BackupRepoKindS3 — factory is `s3.New(ctx, repo, opts...)`
- ValidateConfig ready for Phase 6 repo-create wiring; needs a composite Store passed via `WithBlockStoreLister` for D-13 enforcement
- Orphan sweep runs automatically on every New(), covers both crashed PutBackup (orphan payload) and crashed uploader (stale MPU)

---
*Phase: 03-destination-drivers-encryption*
*Completed: 2026-04-16*
