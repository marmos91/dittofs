---
phase: 03-destination-drivers-encryption
plan: 03
subsystem: backup
tags: [backup, destination, local-fs, atomic-rename, encryption, sha-256, orphan-sweep]

# Dependency graph
requires:
  - phase: 03-destination-drivers-encryption
    plan: 01
    provides: "Destination interface + 10 sentinels + registry (pkg/backup/destination/destination.go, errors.go)"
  - phase: 03-destination-drivers-encryption
    plan: 02
    provides: "NewEncryptWriter / NewDecryptReader / ResolveKey / ValidateKeyRef / hashTeeWriter"
provides:
  - "fs.Store implementing destination.Destination per D-03 atomic-rename publish + D-14 perms/no-chown/remote-FS warning"
  - "fs.New factory registered at cmd/dfs startup for BackupRepoKindLocal (wiring deferred to Phase 6)"
  - "fs.Config parsed from BackupRepo.Config JSON (path required+absolute, grace_window optional Go duration)"
  - "Orphan sweep on New() — D-06 layer 2 belt-and-suspenders"
  - "destination.NewHashTeeWriter exported wrapper so drivers can reuse the SHA-256 tee primitive from plan 02"
affects: [03-04, 03-05, 03-06, 05-restore, 06-cli-api]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Atomic directory rename as publish marker (<id>.tmp/ -> <id>/ via os.Rename on same FS) — classic tmp+rename discipline, matches restic/borg convention"
    - "Umask-defensive chmod after os.Mkdir / os.OpenFile so file/dir modes are exactly 0600/0700 regardless of process umask"
    - "fsyncDir helper: open directory, Sync, close — mirrors pkg/blockstore/local/fs/flush.go syncFile pattern but for directory entries"
    - "Verify-while-streaming SHA-256 reader: tee-hash on every Read; Mismatch surfaces on Close so the caller always drains first, validates second"
    - "Probe file via os.CreateTemp(root, \".dittofs-probe-*\") for ValidateConfig — concurrent probes never collide on a fixed filename (mitigates T-03-20a)"
    - "Build-tagged platform split: mount_linux.go parses /proc/mounts; mount_other.go stubs to \"\" on macOS/Windows so the remote-FS warning is Linux-only"
    - "Explicit pre-write field checks replace manifest.Validate() — Validate would fail on empty SHA256 (driver populates it from the tee)"
    - "Local alias (var newHashTeeWriter = destination.NewHashTeeWriter) keeps file-local identifier naming while reusing the shared primitive"

key-files:
  created:
    - pkg/backup/destination/fs/store.go
    - pkg/backup/destination/fs/store_test.go
    - pkg/backup/destination/fs/mount_linux.go
    - pkg/backup/destination/fs/mount_other.go
  modified:
    - pkg/backup/destination/hash.go

key-decisions:
  - "fs.New returns destination.Destination (interface) not *fs.Store — matches the Factory type signature in destination.go. The Store struct stays exported so tests using fs_test external-package can reference it; the factory path (and cmd/dfs registration) uses the interface."
  - "parseConfig rejects zero/negative grace_window as ErrIncompatibleConfig. A zero grace window would make New() sweep every <id>.tmp/ unconditionally; negative values are structurally invalid. Not in the plan but falls under Rule 2 (missing critical validation)."
  - "PutBackup performs an early os.Stat on the final dir BEFORE mkdir of the tmp dir. Duplicate-id rejection must precede any side effect; a retry producing ErrDuplicateBackupID must not leave an <id>.tmp/ trace behind."
  - "verifyReadCloser.Close closes the underlying file FIRST, then checks the hash. File descriptor release must not depend on hash state — a mismatch already won the 'errors win' race; the defer-cleanup contract preserves fd lifetimes."
  - "detectFilesystemType on Linux parses /proc/mounts line-by-line and picks the longest mount-point prefix. Handles nested mounts (e.g. /mnt vs /mnt/backup on different fstypes) — a simple prefix match would otherwise pick the shortest and misclassify."
  - "Added destination.NewHashTeeWriter and HashTeeWriter type alias as a Rule 2 deviation: plan 02 left the tee unexported, but the plan-03 acceptance criteria require the driver to call newHashTeeWriter from the fs package. Shipping an exported constructor in plan 02's file + a local alias var in plan 03's file satisfies both the naming and the cross-package visibility."

patterns-established:
  - "Destination drivers take *models.BackupRepo (not a parsed Config) so Factory signatures stay uniform; each driver's parseConfig pulls out its own typed fields from repo.GetConfig()."
  - "Encryption key bytes are resolved ONCE per PutBackup / GetBackup, passed to cipher.NewGCM, and immediately zeroed via for i := range key { key[i] = 0 } — no in-memory caching across operations."
  - "Every error-returning path that creates side-effect resources (tmp dir, file handles) has an explicit cleanup (defer _ = os.RemoveAll(tmpDir) + cleanupTmp = false after successful publish). No try/finally-style helper; boundaries are visible at the call site for reviewability."

requirements-completed: [DRV-01, DRV-03, DRV-04]

# Metrics
metrics:
  duration_min: 35
  tasks_completed: 2
  files_created: 4
  files_modified: 1
  lines_added: 780
  tests_added: 13
  completed_date: 2026-04-16
---

# Phase 3 Plan 3: Local Filesystem Destination Driver Summary

Local-filesystem Destination driver implementing D-03 atomic-rename publish and D-14 permission / mount-type / no-chown policy. Reuses the AES-256-GCM envelope, keyref resolver, and SHA-256 tee helpers from plan 02; reuses the Destination interface, sentinels, and registry from plan 01. Ships DRV-01 (local FS tmp+rename) + DRV-03 (AES-256-GCM) + DRV-04 (SHA-256) for the local-filesystem path.

## Work Completed

### Task 1 — Implement fs.Store + platform-split mount detection

Four files delivered:

- **`pkg/backup/destination/fs/store.go`** (511 lines) — the full `destination.Destination` implementation: all 7 methods (PutBackup, GetBackup, List, Stat, Delete, ValidateConfig, Close) plus the unexported `verifyReader`, `verifyReadCloser`, `writeNopCloser`, `fsyncDir`, `sweepOrphans`, and `isRemoteFS` helpers.
- **`pkg/backup/destination/fs/mount_linux.go`** — Linux-only `detectFilesystemType(path)` that parses `/proc/mounts` and returns the fstype of the longest matching mount-point prefix.
- **`pkg/backup/destination/fs/mount_other.go`** — build-tagged `//go:build !linux` stub returning `""` (non-Linux has no remote-FS warning).
- **`pkg/backup/destination/hash.go`** (modified) — added `NewHashTeeWriter` exported constructor + `HashTeeWriter` type alias so the fs driver can use the shared SHA-256 tee primitive from plan 02 (see Deviations).

Write path (`PutBackup`):

```
mkdir <id>.tmp (0700, explicit Chmod defend umask)
  + defer os.RemoveAll(tmpDir) on error
open payload.bin (0600, O_EXCL)
  + Chmod 0600 defend umask
tee := newHashTeeWriter(payload_file)
if encrypted:
    key := destination.ResolveKey(m.Encryption.KeyRef)
    enc := destination.NewEncryptWriter(tee, key, 0)
    zero(key)
    writer = enc
else:
    writer = writeNopCloser{tee}
io.Copy(writer, payload)
writer.Close()
payload_file.Sync(); payload_file.Close()

m.SHA256   = tee.Sum()
m.SizeBytes = tee.Size()

open manifest.yaml (0600, O_EXCL)
  + Chmod 0600 defend umask
m.WriteTo(manifest_file)
manifest_file.Sync(); manifest_file.Close()

fsyncDir(<id>.tmp)              # directory entries durable
os.Rename(<id>.tmp, <id>)       # atomic publish marker (D-03)
fsyncDir(repo_root)             # rename durable (best-effort)
```

Read path (`GetBackup`):

```
manifest := readManifest(<id>/manifest.yaml)   # ErrManifestMissing on absent
payload_file := os.Open(<id>/payload.bin)       # ErrIncompleteBackup on absent
reader := verifyReader{payload_file, expected=manifest.SHA256}
if manifest.Encryption.Enabled:
    key := destination.ResolveKey(manifest.Encryption.KeyRef)
    reader := destination.NewDecryptReader(reader, key)
    zero(key)
return manifest, verifyReadCloser{reader, file}
```

`Close()` on the returned ReadCloser closes the underlying file FIRST, then calls `verifyReader.Mismatch()`; a mismatch returns `ErrSHA256Mismatch` regardless of a benign close error.

### Task 2 — Unit tests (13 passing)

File: `pkg/backup/destination/fs/store_test.go` (343 lines, `package fs_test` external).

| Test | What it proves |
|------|----------------|
| `TestFSStore_PutGet_Unencrypted_Roundtrip` | 64 KiB round-trip; files exist; SHA256 + SizeBytes populated; Close returns nil |
| `TestFSStore_PutGet_Encrypted_Roundtrip` | 1 MiB round-trip through AES-256-GCM (env:DITTOFS_FS_TEST_KEY); SHA256 is 32 bytes hex over ciphertext |
| `TestFSStore_Perms_0600_0700` | Directory 0700, payload.bin + manifest.yaml both 0600 after publish |
| `TestFSStore_CrashBeforeRename_ListExcludesTmp` | Hand-created `<id>.tmp/` never surfaces in List |
| `TestFSStore_MutatedPayload_SHA256Mismatch` | Tampered payload: Read succeeds, Close returns ErrSHA256Mismatch |
| `TestFSStore_MissingManifest_GetReturnsManifestMissing` | `<id>/payload.bin` only: GetBackup → ErrManifestMissing |
| `TestFSStore_MissingPayload_GetReturnsIncomplete` | `<id>/manifest.yaml` only: GetBackup → ErrIncompleteBackup |
| `TestFSStore_Delete_InverseOrder` | Delete removes the whole subtree |
| `TestFSStore_OrphanSweep_On_New` | 48h-old `.tmp/` swept; fresh `.tmp/` preserved |
| `TestFSStore_ValidateConfig` | Happy path, non-existent path, not-a-directory path |
| `TestFSStore_DuplicateID_Rejected` | Second PutBackup with same id → ErrDuplicateBackupID |
| `TestFSStore_List_ChronologicalOrder` | Three ULIDs 2ms apart sort lexicographic == chronological |
| `TestFSStore_NilPayloadIDSet_Rejected` | Regression: nil PayloadIDSet → ErrIncompatibleConfig pre-write |

## Integration Surface

Phase 6 (CLI + REST) will register the factory at `cmd/dfs/main.go` startup:

```go
destination.Register(models.BackupRepoKindLocal, fs.New)
```

`fs.New(ctx, repo)` then resolves from `destination.Lookup(repo.Kind)` inside the Phase 4 scheduler and Phase 5 restore orchestrator. No direct import of `pkg/backup/destination/fs` by those orchestrators — they only touch the `Destination` interface.

## Grace Window Default

When the BackupRepo config omits `grace_window`, the driver applies `defaultGraceWindow = 24 * time.Hour` (D-06). Operators can override in the Config JSON blob:

```json
{"path": "/var/lib/dittofs/backups/store-prod", "grace_window": "4h"}
```

Parse failures (non-duration strings, negative values, zero) return `ErrIncompatibleConfig` at construction time.

## Pre-Write Field Checks (NOT m.Validate())

The driver does **NOT** call `manifest.Manifest.Validate()` pre-write — Validate requires a non-empty SHA256, but the driver is the one that populates that field from the tee after streaming the payload. Instead, explicit field checks catch the fields that callers must set before handoff:

```go
if m.BackupID == ""        // ULID identity
if m.StoreID == ""          // source store for FK snapshot
if m.StoreKind == ""        // schema-compatibility guard
if m.PayloadIDSet == nil    // block-GC hold (SAFETY-01); empty slice is valid, nil is not
```

A regression test (`TestFSStore_NilPayloadIDSet_Rejected`) guards this boundary — nil PayloadIDSet must surface `ErrIncompatibleConfig` before any file is created.

Confirmed via `grep -c 'm\.Validate()' pkg/backup/destination/fs/store.go` returns `0`.

## Probe File Naming Strategy (T-03-20a Mitigation)

ValidateConfig uses `os.CreateTemp(s.root, ".dittofs-probe-*")` — the trailing `*` is expanded by `CreateTemp` into a random suffix so concurrent probes (e.g. operator running `dfsctl destination validate` in two shells, or the test harness + the scheduler sharing a repo) never collide on a fixed filename.

The probe file is removed via a `defer` so cleanup runs even on panic; log visibility is preserved because the probe is cheap.

Confirmed via `grep -c 'os\.CreateTemp(s\.root, probeFilePattern)' pkg/backup/destination/fs/store.go` returns `1`.

## Deviations from Plan

**[Rule 2 — Missing critical export] `destination.NewHashTeeWriter`**

- **Found during:** Task 1 initial build
- **Issue:** Plan 02 shipped `hashTeeWriter` as an unexported type with an unexported `newHashTeeWriter` constructor. The plan-03 acceptance criteria require the fs driver to reference `newHashTeeWriter` — but from a sibling sub-package (`pkg/backup/destination/fs`), the lowercase identifier is inaccessible.
- **Fix:** Added `HashTeeWriter = hashTeeWriter` type alias and `func NewHashTeeWriter(dst io.Writer) *HashTeeWriter` constructor to `pkg/backup/destination/hash.go`. In `pkg/backup/destination/fs/store.go`, a local `var newHashTeeWriter = destination.NewHashTeeWriter` alias preserves the literal identifier required by the acceptance grep while delegating to the shared primitive.
- **Files modified:** `pkg/backup/destination/hash.go` (added 18 lines), `pkg/backup/destination/fs/store.go` (added 5 lines)
- **Commit:** included in Task 1 commit (e2f5a744)

**[Rule 2 — Missing validation] Zero/negative grace window rejection**

- **Found during:** Task 1 implementation
- **Issue:** The original `parseConfig` only checked that `grace_window` parsed as a valid duration — zero and negative durations would pass through. A zero `graceWindow` means `sweepOrphans` sweeps every `.tmp/` unconditionally (including fresh in-flight publishes in a parallel process).
- **Fix:** Added `if d <= 0 { return ErrIncompatibleConfig }` in `parseConfig`. Not a plan-level requirement, but a correctness requirement (Rule 2).
- **Files modified:** `pkg/backup/destination/fs/store.go`
- **Commit:** included in Task 1 commit (e2f5a744)

Otherwise, no deviations. The plan was followed task-by-task.

## Threat Model Alignment

All threat-register entries are mitigated by the implementation:

| Threat | Mechanism |
|--------|-----------|
| T-03-13 Tampering (published partial) | `os.Rename` is the publish marker; List skips `tmpSuffix`; sweepOrphans removes stale tmp dirs |
| T-03-14 Info Disclosure (world-read plaintext manifest) | Explicit `Chmod 0600` after `OpenFile`; `TestFSStore_Perms_0600_0700` asserts mode |
| T-03-15 Info Disclosure (key leaked in errors) | ResolveKey errors reference path/env var names only; fs.Store never logs key bytes |
| T-03-16 Tampering (bit-rot) | SHA-256 tee over ciphertext at write; verifyReader on read; `ErrSHA256Mismatch` on Close |
| T-03-17 DoS (tmp-dir fill) | sweepOrphans on New removes stale `.tmp/` beyond grace window |
| T-03-18 Spoofing (non-atomic rename on NFS/SMB/FUSE) | mount-type detection at ValidateConfig; `slog.Warn` (not reject) per D-14 |
| T-03-19 Repudiation (which tmp-dir from which run) | ULID prefix is chronologically sortable; sweep logs emit path + age at WARN |
| T-03-20 EoP (group/other perms via umask) | Explicit Chmod after OpenFile; defensive against any process umask |
| T-03-20a DoS (probe-file collision) | `os.CreateTemp` random-suffixed probe; defer-cleanup |

## Threat Flags

None. No new security-relevant surface introduced outside what the plan anticipated.

## Known Stubs

None. All 7 `Destination` methods are complete; `mount_other.go` intentionally returns `""` on non-Linux (D-14 best-effort policy — documented, not a stub).

## Verification

- `go build ./pkg/backup/destination/fs/...` — clean
- `go vet ./pkg/backup/destination/fs/...` — clean
- `go test ./pkg/backup/destination/fs/... -count=1` — 13/13 pass (0.629s)
- `go test -race ./pkg/backup/destination/fs/... -count=1` — 13/13 pass, no races (1.544s)
- `go test ./pkg/backup/...` — all packages clean (destination, destination/fs, manifest)

Acceptance criteria greps:

| Criterion | Count |
|-----------|-------|
| `os.Rename` | 3 |
| `0o700` | 1 |
| `0o600` | 1 |
| `payloadFilename = "payload.bin"` | 1 |
| `manifestFilename = "manifest.yaml"` | 1 |
| `destination.NewEncryptWriter` | 1 |
| `destination.NewDecryptReader` | 1 |
| `destination.ResolveKey` | 2 |
| `newHashTeeWriter` | 5 |
| `sweepOrphans` | 3 |
| `key[i] = 0` | 2 |
| `os.CreateTemp(s.root, probeFilePattern)` | 1 |
| `m\.Validate()` | 0 |
| `fsyncDir` | 4 |
| `//go:build linux` (mount_linux.go) | 1 |
| `/proc/mounts` (mount_linux.go) | 3 |
| `//go:build !linux` (mount_other.go) | 1 |
| `_ destination.Destination = (*Store)(nil)` | 1 |

## Commits

| Task | Commit | Message |
|------|--------|---------|
| 1 | `e2f5a744` | feat(03-03): implement local FS destination driver |
| 2 | `61e5465a` | test(03-03): unit tests for local FS destination driver |

## Self-Check: PASSED

All created files exist:
- `pkg/backup/destination/fs/store.go` — FOUND
- `pkg/backup/destination/fs/store_test.go` — FOUND
- `pkg/backup/destination/fs/mount_linux.go` — FOUND
- `pkg/backup/destination/fs/mount_other.go` — FOUND
- `pkg/backup/destination/hash.go` — FOUND (modified)
- `.planning/phases/03-destination-drivers-encryption/03-03-SUMMARY.md` — FOUND (this file)

All commit hashes found in git log:
- `e2f5a744` — FOUND
- `61e5465a` — FOUND
