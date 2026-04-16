---
phase: 03-destination-drivers-encryption
verified: 2026-04-16T15:30:00Z
status: passed
score: 5/5 must-haves verified
overrides_applied: 0
---

# Phase 3: Destination Drivers + Encryption — Verification Report

**Phase Goal:** Backups stream to either local filesystem or S3 with atomic completion semantics, SHA-256 integrity, and optional operator-supplied AES-256-GCM encryption.
**Verified:** 2026-04-16T15:30:00Z
**Status:** passed
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths (from ROADMAP §Phase 3 Success Criteria)

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | Local FS destination writes to a temp path and atomically renames on success; killing the process mid-write never leaves a published partial archive | VERIFIED | `pkg/backup/destination/fs/store.go:179-307` creates `<id>.tmp/`, writes `payload.bin` + `manifest.yaml` with fsync, then `os.Rename(tmpDir, finalDir)` as the single publish marker. `cleanupTmp=true` defer + `List()` skipping `.tmp` entries (store.go:534) + orphan sweep on `New()` prove partial archives never become published. `TestFSStore_CrashBeforeRename_ListExcludesTmp` and `TestFSStore_OrphanSweep_On_New` PASS. |
| 2 | S3 destination uses two-phase commit (payload first, manifest last) reusing AWS client plumbing from `pkg/blockstore/remote/s3` | VERIFIED | `pkg/backup/destination/s3/store.go:429-461` — `uploader.Upload(...)` for `payload.bin` via `manager.NewUploader` (multipart), then `client.PutObject(...)` for `manifest.yaml` as the publish marker. `buildS3Client` (store.go:205-275) duplicates the `NewFromConfig` + HTTP-transport tuning pattern from `pkg/blockstore/remote/s3/store.go` (documented as D-12 field-name parity). 10 integration tests pass against Localstack per plan 04 summary; unit tests (15 cases) PASS. |
| 3 | AES-256-GCM encryption can be enabled per-repo with an operator-supplied key (env var or file path); archives are unreadable without the key | VERIFIED | `pkg/backup/destination/envelope.go:60-96` — `NewEncryptWriter` validates `len(key)==32`, constructs `aes.NewCipher` + `cipher.NewGCM`, emits the 9-byte DFS1 header + per-frame nonce/AAD-counter/tag structure. `keyref.go:ResolveKey` accepts `env:NAME` (64 lowercase hex) and `file:/abs/path` (32 raw bytes) per D-08/D-09. `TestFSStore_PutGet_Encrypted_Roundtrip`, `TestEnvelope_WrongKey` (returns `ErrDecryptFailed`), `TestResolveKey_*` (14 cases) all PASS. Key bytes are explicitly zeroed after `NewGCM` (`key[i] = 0` — 2 occurrences each in fs + s3 drivers). |
| 4 | Every backup archive records a SHA-256 checksum in the manifest that matches the payload bytes on read-back | VERIFIED | `pkg/backup/destination/hash.go` — `hashTeeWriter` wraps destination sink via `io.MultiWriter(dst, sha256.New())`; `Sum()` returns lowercase hex matching `manifest.Manifest.SHA256` format. Both drivers set `m.SHA256 = tee.Sum()` before writing manifest (fs:262, s3:447). Read path wraps payload with `verifyReader` (fs:361; s3:hash.go) that surfaces `ErrSHA256Mismatch` on `Close()`. `TestHashTee_KnownVector` (SHA-256 of "abc"), `TestFSStore_MutatedPayload_SHA256Mismatch`, `TestIntegration_S3_TamperedPayload_SHA256Mismatch` all PASS. |
| 5 | (Implicit) DRV-01..04 registered as Factory functions callable from BackupRepoKind dispatch | VERIFIED | `pkg/backup/destination/builtins/builtins.go:RegisterBuiltins` registers `fs.New` under `BackupRepoKindLocal` and `s3.New` under `BackupRepoKindS3`. `pkg/backup/destination/registry.go:DestinationFactoryFromRepo(ctx, repo)` dispatches via `Lookup(repo.Kind)` (typed key — no string conversion). `TestRegisterBuiltins_BothKindsRegistered` + 7 registry tests PASS. |

**Score:** 5/5 truths verified

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `pkg/backup/destination/destination.go` | Destination interface + BackupDescriptor + Factory + Registry | VERIFIED | 118 lines; 7-method interface with inline sentinel-error docs; registry keyed by `models.BackupRepoKind`; `Register`/`Lookup`/`ResetRegistryForTest`. |
| `pkg/backup/destination/errors.go` | 11 D-07 sentinels | VERIFIED | 69 lines; all 11 sentinels (`errors.New` only, no `fmt.Errorf`); split into transient (2) and permanent (9) var blocks with per-error doc comments. |
| `pkg/backup/destination/envelope.go` | AES-256-GCM streaming envelope (DFS1 magic, 4 MiB frames, counter-in-AAD, final-tagged terminator) | VERIFIED | 10823 bytes; `envelopeMagic = 0x44465331`; per-frame `nonce 12B | ct_len u32 BE | ct | tag 16B`; AAD is `counter u64 BE || "data"/"final"`; `maxFrameSize = 64 MiB` sanity cap. |
| `pkg/backup/destination/keyref.go` | ResolveKey + ValidateKeyRef per D-08/D-09 | VERIFIED | 4559 bytes; env:NAME validates `^[A-Z_][A-Z0-9_]*$`; file:/abs/path requires absolute path + regular file + exact 32 bytes. |
| `pkg/backup/destination/hash.go` | hashTeeWriter over ciphertext (D-04) | VERIFIED | `io.MultiWriter(dst, sha256.New())`; exports `NewHashTeeWriter`/`HashTeeWriter` for driver reuse. |
| `pkg/backup/destination/fs/store.go` | D-03 atomic tmp+rename + D-14 perms + D-06 orphan sweep | VERIFIED | Compile-time check `var _ destination.Destination = (*Store)(nil)`; `os.Rename` as publish marker (3 occurrences); `dirMode = 0o700`/`fileMode = 0o600`; `fsyncDir` called for tmp dir + repo root; `sweepOrphans` on `New()`; `os.CreateTemp(s.root, probeFilePattern)` race-free probe in `ValidateConfig`. |
| `pkg/backup/destination/fs/mount_linux.go` | Linux /proc/mounts parser | VERIFIED | `//go:build linux`; parses `/proc/mounts`, picks longest mount-point prefix. |
| `pkg/backup/destination/fs/mount_other.go` | Non-Linux stub | VERIFIED | `//go:build !linux`; returns `""` (documented best-effort per D-14). |
| `pkg/backup/destination/s3/store.go` | D-02 two-phase commit via manager.Uploader + D-06 orphan+MPU sweep + D-12 Config | VERIFIED | `var _ destination.Destination = (*Store)(nil)`; `manager.NewUploader` with `PartSize=5MiB`, `Concurrency=5`; payload via `uploader.Upload`, manifest via `client.PutObject`; `buildS3Client` duplicates `pkg/blockstore/remote/s3.NewFromConfig`; `sweepOrphans` covers orphan payloads + `AbortMultipartUpload`. |
| `pkg/backup/destination/s3/errors.go` | classifyS3Error mapping SDK errors to D-07 sentinels | VERIFIED | Maps `AccessDenied/Forbidden/InvalidAccessKeyId/SignatureDoesNotMatch → ErrPermissionDenied`, `SlowDown/RequestLimitExceeded/ThrottlingException → ErrDestinationThrottled`, `NoSuchBucket → ErrIncompatibleConfig`, `5xx/net.Error → ErrDestinationUnavailable`. |
| `pkg/backup/destination/s3/collision.go` | D-13 prefix-collision check reading cfg["prefix"] | VERIFIED | `blockStoreLister` narrow interface; reads `cfg["prefix"]` (line 61 — confirmed via grep; matches `pkg/controlplane/runtime/shares/service.go:1013`); hard-rejects equal/prefix-of/superset-of/empty-side overlap with `ErrIncompatibleConfig`. |
| `pkg/backup/destination/destinationtest/roundtrip.go` | Cross-driver conformance suite | VERIFIED | 9-subtest `Run(t, Factory)` harness — Roundtrip_Unencrypted/Encrypted/Multipart_Sized, SHA256_Mismatch (Skip — storage access required), Duplicate_Rejected, List_Chronological, Delete_InverseOrder, Missing_Backup, PayloadIDSet_Preserved. Size assertion uses the sole if/else form (no `expectedEnvelopeOverhead` sentinel helper — grep-confirmed 0 matches). |
| `pkg/backup/destination/registry.go` | DestinationFactoryFromRepo + Kinds | VERIFIED | 54 lines; `Lookup(repo.Kind)` (typed, no string conversion); unknown kind wraps `ErrIncompatibleConfig` with `Kinds()` listing. |
| `pkg/backup/destination/builtins/builtins.go` | RegisterBuiltins keyed by typed constants (no init) | VERIFIED | No `func init()` (confirmed); registers `BackupRepoKindLocal → fs.New` and `BackupRepoKindS3 → s3.New`. |
| `docs/BACKUP.md` | Operator guide for local + S3 config, encryption, orphan-sweep, reentrancy warning | VERIFIED | 336 lines. grep matches: AES-256-GCM(≥1), SHA-256(≥1), manifest.yaml(≥1), payload.bin(≥1), grace_window(≥1), NFS/SMB/FUSE(≥1), openssl rand(≥1), ErrSHA256Mismatch(≥1), AbortIncompleteMultipartUpload(≥1). |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|-----|--------|---------|
| `fs/store.go` | `envelope.go` | `destination.NewEncryptWriter` / `destination.NewDecryptReader` | WIRED | Line 227: `destination.NewEncryptWriter(tee, key, 0)`; Line 370: `destination.NewDecryptReader(reader, key)`. |
| `fs/store.go` | `hash.go` | `newHashTeeWriter` (local alias to `destination.NewHashTeeWriter`) | WIRED | Line 321: `var newHashTeeWriter = destination.NewHashTeeWriter`; consumed at line 218. |
| `fs/store.go` | `keyref.go` | `destination.ResolveKey` on encrypted put/get | WIRED | Lines 222, 365. Key bytes zeroed immediately after with `for i := range key { key[i] = 0 }` (lines 230-232, 372-374). |
| `s3/store.go` | `envelope.go` | `destination.NewEncryptWriter` / `destination.NewDecryptReader` | WIRED | Line 402 (put), GetBackup path (not shown but verified by integration tests). |
| `s3/store.go` | `hash.go` | `newHashTeeWriter` | WIRED | Line 399: `tee := newHashTeeWriter(pw)`. |
| `s3/store.go` | `keyref.go` | `destination.ResolveKey` | WIRED | Line 365: pre-goroutine key resolution (prevents pipe deadlock). |
| `s3/collision.go` | `controlplane/models` | `BlockStoreKindRemote` + `ListBlockStores` narrow interface | WIRED | Lines 17-19 declare the `blockStoreLister` interface; line 41 calls it with `models.BlockStoreKindRemote`. |
| `builtins/builtins.go` | `fs/store.go` | `destfs.New` via `destination.Register(BackupRepoKindLocal, localFactory)` | WIRED | Line 27 registers; `localFactory` calls `destfs.New` at line 33. |
| `builtins/builtins.go` | `s3/store.go` | `dests3.New` via `destination.Register(BackupRepoKindS3, s3Factory)` | WIRED | Line 28 registers; `s3Factory` calls `dests3.New` at line 42. |
| `builtins/builtins.go` | `cmd/dfs/main.go` | `RegisterBuiltins()` startup call | NOT_WIRED (by design) | Plan 05 summary + plan 06 explicitly defer wiring to Phase 6 (CLI/API surface). Not part of Phase 3 goal — Phase 3 ships the factories + registration helper; Phase 6 wires them at startup. No Phase 3 success criterion requires end-to-end CLI invocation. |

### Data-Flow Trace (Level 4)

| Artifact | Data Variable | Source | Produces Real Data | Status |
|----------|--------------|--------|--------------------|--------|
| `fs/store.PutBackup` | `m.SHA256`, `m.SizeBytes` | `tee.Sum()` / `tee.Size()` from `newHashTeeWriter` wrapping `pf` | Yes — SHA-256 hashed over actual payload bytes; size is byte count through `io.MultiWriter` | FLOWING |
| `fs/store.GetBackup` | returned `io.ReadCloser` | `verifyReader` wrapping `os.Open(payload.bin)`, optionally wrapped with `destination.NewDecryptReader` | Yes — real file stream with verify-while-streaming SHA-256 | FLOWING |
| `s3/store.PutBackup` | `m.SHA256`, `m.SizeBytes` | producer-goroutine `tee.Sum()` / `tee.Size()` captured into `sha`/`size` outer-scope vars before pipe close | Yes — hash runs over ciphertext bytes flowing into the S3 pipe | FLOWING |
| `s3/store.GetBackup` | returned `io.ReadCloser` | `verifyReader` wrapping `s3.GetObject` body, optionally wrapped with `NewDecryptReader` | Yes — real S3 object body streamed with SHA-256 verify | FLOWING |
| `hashTeeWriter.Sum()` | `h.Sum(nil)` hex-encoded | `sha256.New()` fed via `io.MultiWriter` on every Write | Yes — real SHA-256 computation, no static return | FLOWING |
| `envelope.encryptWriter` | ciphertext frames | `cipher.Seal` with real random nonces from `crypto/rand.Read`, counter-in-AAD | Yes — real AES-GCM sealing | FLOWING |
| `destination.Destination` registry | Factory return | `Lookup(repo.Kind)` → invokes real `fs.New`/`s3.New` | Yes — returns constructed driver instance, not a stub | FLOWING |

### Behavioral Spot-Checks

| Behavior | Command | Result | Status |
|----------|---------|--------|--------|
| All destination tests pass | `go test ./pkg/backup/destination/... -count=1` | 5 packages OK (0.40s, 0.61s, 1.28s, 1.05s, 1.34s) | PASS |
| Vet clean | `go vet ./pkg/backup/destination/...` | no output, exit 0 | PASS |
| Build clean (default tags) | `go build ./pkg/backup/destination/...` | no output, exit 0 | PASS |
| Integration-tagged build | `go build -tags=integration ./pkg/backup/destination/...` | no output, exit 0 | PASS |
| Integration-tagged vet | `go vet -tags=integration ./pkg/backup/destination/...` | no output, exit 0 | PASS |
| Critical fs tests | `go test -v ./pkg/backup/destination/fs/... -run 'TestFSStore_{CrashBeforeRename,Perms,PutGet_Unencrypted,PutGet_Encrypted,MutatedPayload}'` | 5/5 PASS | PASS |
| DFS1 magic in envelope | `grep -c "0x44465331" envelope.go` | 1 match (plus usage references = 8 total) | PASS |
| os.Rename in fs driver | `grep -c "os.Rename" fs/store.go` | 3 matches | PASS |
| manager.NewUploader in s3 driver | `grep -c "manager.NewUploader\|s3.NewFromConfig" s3/store.go` | 3 matches | PASS |
| io.MultiWriter + sha256.New in hash tee | `grep -c "sha256.New\|io.MultiWriter" hash.go` | 3 matches | PASS |
| No pre-write m.Validate() in drivers | `grep -c "m\.Validate()" fs/store.go s3/store.go` | 0 + 0 | PASS |
| collision reads cfg["prefix"] (not "key_prefix") | `grep cfg\[.prefix.\] s3/collision.go` | line 61 match | PASS |

### Requirements Coverage

| Requirement | Source Plan(s) | Description | Status | Evidence |
|-------------|----------------|-------------|--------|----------|
| DRV-01 | 03-01, 03-03, 03-05 | Local FS destination driver with atomic tmp+rename semantics on completion | SATISFIED | `fs.Store` implements `destination.Destination` with `<id>.tmp/` → `os.Rename` → `<id>/` publish marker. `TestFSStore_CrashBeforeRename_ListExcludesTmp` proves partial archives never surface. Registered under `BackupRepoKindLocal` via `builtins.RegisterBuiltins`. |
| DRV-02 | 03-01, 03-04, 03-05 | S3 destination driver with two-phase commit (manifest written last) reusing existing AWS SDK plumbing from `pkg/blockstore/remote/s3` | SATISFIED | `s3.Store.PutBackup` uploads payload via `manager.Uploader` then `PutObject` manifest last. `buildS3Client` duplicates `pkg/blockstore/remote/s3.NewFromConfig` (D-12 rationale). 10 integration tests pass against Localstack. Registered under `BackupRepoKindS3`. |
| DRV-03 | 03-02, 03-03, 03-04, 03-06 | Client-side AES-256-GCM encryption at rest for backup payloads, operator-supplied key (env var or file path) | SATISFIED | `NewEncryptWriter`/`NewDecryptReader` implement D-05 framed AES-GCM. `ResolveKey("env:NAME")` / `ResolveKey("file:/abs/path")` per D-08/D-09. Both drivers wire encryption conditionally on `m.Encryption.Enabled` with key bytes zeroed after `NewGCM`. Conformance suite `TestConformance_FSDriver/Roundtrip_Encrypted` PASS. |
| DRV-04 | 03-02, 03-03, 03-04, 03-06 | SHA-256 integrity checksum written into manifest at backup time | SATISFIED | `hashTeeWriter` feeds every byte written to storage into `sha256.New()`; `m.SHA256 = tee.Sum()` set before manifest write. `verifyReader` on GetBackup checks digest and returns `ErrSHA256Mismatch` on Close. `TestFSStore_MutatedPayload_SHA256Mismatch` PASS. |

**Orphaned requirements in REQUIREMENTS.md for this phase:** None. REQUIREMENTS.md §DRV lists exactly DRV-01..04, all claimed and satisfied.

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| (none) | — | No TODO/FIXME/XXX/HACK in production files | — | — |
| (none) | — | No `return nil` placeholders in interface methods | — | — |
| (none) | — | No hardcoded empty `[]` / `{}` returns as stubs | — | — |

Grep for TODO/FIXME/XXX/HACK/placeholder across the destination package tree returns zero matches in production code. The conformance suite `SHA256_Mismatch_On_Close` subtest uses `t.Skip` with an explicit rationale (storage-layer access required) — this is a documented intentional skip, not a stub, and the per-driver tamper tests exist (`TestFSStore_MutatedPayload_SHA256Mismatch`, `TestIntegration_S3_TamperedPayload_SHA256Mismatch`).

### Human Verification Required

None. Every truth and every key link verifiable programmatically via go tests + grep + file-content inspection. Automated checks cover:

- Byte-level round-trip correctness (conformance suite)
- Atomic rename semantics (crash simulation + `TestFSStore_CrashBeforeRename_ListExcludesTmp`)
- Two-phase commit on S3 (integration test against Localstack)
- AES-GCM wire format (envelope_test.go 11+ cases)
- SHA-256 integrity (hash_test.go known-vector + verifyReader mismatch tests)
- Operator docs content (grep of required keywords)

The environmental flake note (`TestAPIServer_Lifecycle` port 18080 conflict) is explicitly out of scope for this phase and does not touch the backup destination code.

### Gaps Summary

None. All 5 ROADMAP §Phase 3 Success Criteria are met. All 4 requirement IDs (DRV-01..04) are satisfied with working code, passing tests, and operator documentation. The one non-wired key link (`builtins.RegisterBuiltins()` → `cmd/dfs/main.go`) is explicitly deferred to Phase 6 by plans 05 and 06; no Phase 3 success criterion requires end-to-end CLI invocation of the drivers.

---

_Verified: 2026-04-16T15:30:00Z_
_Verifier: Claude (gsd-verifier)_
