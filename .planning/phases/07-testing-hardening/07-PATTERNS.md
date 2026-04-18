# Phase 7: Testing & Hardening - Pattern Map

**Mapped:** 2026-04-17
**Files analyzed:** 6
**Analogs found:** 6 / 6

## File Classification

| New/Modified File | Role | Data Flow | Closest Analog | Match Quality |
|---|---|---|---|---|
| `pkg/backup/destination/corruption_test.go` | test (integration) | CRUD | `pkg/backup/destination/fs/store_test.go` | exact |
| `pkg/backup/restore/version_gate_restore_test.go` | test (unit) | transform | `pkg/backup/manifest/manifest_test.go` | exact |
| `pkg/backup/concurrent_write_backup_restore_test.go` | test (integration) | request-response | `pkg/backup/backup_restore_test.go` | role-match |
| `test/e2e/backup_matrix_test.go` | test (e2e) | request-response | `test/e2e/store_matrix_test.go` | exact |
| `test/e2e/backup_chaos_test.go` | test (e2e) | event-driven | `test/e2e/store_matrix_test.go` + `test/e2e/helpers/server.go` | role-match |
| `test/e2e/backup_restore_mounted_test.go` | test (e2e) | request-response | `test/e2e/shares_test.go` + `test/e2e/backup_test.go` | role-match |

---

## Pattern Assignments

### `pkg/backup/destination/corruption_test.go` (test, integration, CRUD)

**Analog:** `pkg/backup/destination/fs/store_test.go` (corruption injection pattern) and `pkg/backup/destination/s3/store_integration_test.go` (Localstack + S3 injection pattern)

**Build tag + package declaration** (line 1, from `pkg/backup/destination/s3/store_integration_test.go`):
```go
//go:build integration

// Integration tests for destination corruption vectors. Run with:
//
//	go test -tags=integration ./pkg/backup/destination/... -count=1
//
// Uses the SHARED Localstack container pattern. Set LOCALSTACK_ENDPOINT to
// reuse an external Localstack instance.
package destination
```

Note: the file lives in `pkg/backup/destination/` (not the s3 sub-package), so it uses `package destination` (in-package) or `package destination_test` (external). Because it needs to inject bytes into the FS driver's temp dir and S3 directly, `package destination_test` with explicit driver construction via `fs.New` / `s3.New` is preferred — mirrors `pkg/backup/destination/destinationtest/roundtrip_integration_test.go` line 17: `package destinationtest_test`.

**TestMain + shared Localstack singleton** (from `pkg/backup/destination/destinationtest/roundtrip_integration_test.go` lines 55–109):
```go
var corruptionLocalstack struct {
    endpoint  string
    client    *awss3.Client
    container testcontainers.Container
}

func TestMain(m *testing.M) {
    cleanup := startLocalstackForCorruption()
    code := m.Run()
    cleanup()
    os.Exit(code)
}

func startLocalstackForCorruption() func() {
    ctx := context.Background()
    if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
        corruptionLocalstack.endpoint = endpoint
        corruptionLocalstack.client = initS3Client(endpoint)
        return func() {}
    }
    req := testcontainers.ContainerRequest{
        Image:        "localstack/localstack:3.0",
        ExposedPorts: []string{"4566/tcp"},
        Env: map[string]string{
            "SERVICES":              "s3",
            "DEFAULT_REGION":        "us-east-1",
            "EAGER_SERVICE_LOADING": "1",
        },
        WaitingFor: wait.ForAll(
            wait.ForListeningPort("4566/tcp"),
            wait.ForHTTP("/_localstack/health").
                WithPort("4566/tcp").
                WithStartupTimeout(90*time.Second),
        ),
    }
    // ... start container, fill corruptionLocalstack, return cleanup
}
```

**Table-driven corruption vector skeleton** (adapted from `pkg/backup/destination/fs/store_test.go` lines 171–197):
```go
func TestCorruption(t *testing.T) {
    cases := []struct {
        name    string
        setup   func(t *testing.T, dest destination.Destination, id string)
        wantErr error
    }{
        {
            name: "TruncatedArchive",
            setup: func(t *testing.T, dest destination.Destination, id string) {
                // write partial bytes then close early via raw S3/FS injection
            },
            wantErr: io.ErrUnexpectedEOF, // or destination.ErrSHA256Mismatch
        },
        {
            name: "BitFlipInPayload",
            setup: func(t *testing.T, dest destination.Destination, id string) {
                // read payload.bin, flip one byte, write back via sharedHelper.client.PutObject
            },
            wantErr: destination.ErrSHA256Mismatch,
        },
        {
            name: "MissingManifest",
            setup: func(t *testing.T, dest destination.Destination, id string) {
                // upload payload without manifest file (or delete manifest.yaml after PutBackup)
            },
            wantErr: destination.ErrManifestMissing,
        },
        {
            name: "WrongStoreID",
            setup: func(t *testing.T, dest destination.Destination, id string) {
                // marshal manifest with StoreID="wrong-store", overwrite manifest.yaml
            },
            wantErr: restore.ErrStoreIDMismatch,
        },
        {
            name: "ManifestVersionUnsupported",
            setup: func(t *testing.T, dest destination.Destination, id string) {
                // set ManifestVersion: 2, marshal YAML, overwrite manifest.yaml
            },
            wantErr: restore.ErrManifestVersionUnsupported,
        },
    }

    for _, tc := range cases {
        t.Run(tc.name+"/FS", func(t *testing.T) {
            runCorruptionCase(t, newFSDestination(t), tc.setup, tc.wantErr)
        })
        t.Run(tc.name+"/S3", func(t *testing.T) {
            runCorruptionCase(t, newS3Destination(t), tc.setup, tc.wantErr)
        })
    }
}
```

**Bit-flip injection via raw S3 PutObject** (from `pkg/backup/destination/s3/store_integration_test.go` lines 162–173):
```go
_, err := sharedHelper.client.PutObject(context.Background(), &s3client.PutObjectInput{
    Bucket: aws.String(bucket),
    Key:    aws.String(id + "/payload.bin"),
    Body:   bytes.NewReader(randBytes(t, 4096)), // different random bytes = bit-flip effect
})
require.NoError(t, err)

_, rc, err := s.GetBackup(context.Background(), id)
require.NoError(t, err)
_, _ = io.ReadAll(rc)
err = rc.Close()
require.ErrorIs(t, err, destination.ErrSHA256Mismatch)
```

**FS driver construction** (from `pkg/backup/destination/fs/store_test.go` lines 29–42):
```go
func newFSDestination(t *testing.T) (destination.Destination, string) {
    t.Helper()
    dir := t.TempDir()
    repo := &models.BackupRepo{
        ID:   "repo-corruption-test",
        Kind: models.BackupRepoKindLocal,
    }
    require.NoError(t, repo.SetConfig(map[string]any{"path": dir, "grace_window": "24h"}))
    s, err := fs.New(context.Background(), repo)
    require.NoError(t, err)
    t.Cleanup(func() { _ = s.Close() })
    return s, dir
}
```

**S3 driver construction in integration test** (from `pkg/backup/destination/s3/store_integration_test.go` lines 56–82):
```go
func newS3Destination(t *testing.T) destination.Destination {
    t.Helper()
    bucket := uniqueBucket(t)
    corruptionLocalstack.createBucket(t, bucket)
    t.Cleanup(func() { corruptionLocalstack.deleteBucket(t, bucket) })
    repo := &models.BackupRepo{ID: ulid.Make().String(), Kind: models.BackupRepoKindS3}
    require.NoError(t, repo.SetConfig(map[string]any{
        "bucket":           bucket,
        "region":           "us-east-1",
        "endpoint":         corruptionLocalstack.endpoint,
        "access_key":       "test",
        "secret_key":       "test",
        "force_path_style": true,
        "max_retries":      3,
        "grace_window":     "24h",
    }))
    s, err := s3.New(context.Background(), repo)
    require.NoError(t, err)
    t.Cleanup(func() { _ = s.Close() })
    return s
}
```

---

### `pkg/backup/manifest/version_gate_test.go` (test, unit, transform)

**Analog:** `pkg/backup/manifest/manifest_test.go`

This file may not need to be separate — the manifest version-gate cases may live directly in `manifest_test.go` in a new function, mirroring `TestManifestVersionGuard_RejectsFuture` which already exists (line 64). The D-04 requirement adds: construct a Version=2 manifest, serialize it, write as a valid-looking archive to a temp destination, call `restore.Executor` or `Destination.GetManifestOnly`, assert `restore.ErrManifestVersionUnsupported`.

**Build tag + package** (from `pkg/backup/manifest/manifest_test.go` line 1):
```go
// No build tag — this is a plain unit test (no Docker, no server process)
package manifest
```

**Test helper pattern** (from `pkg/backup/manifest/manifest_test.go` lines 12–34):
```go
func fullyPopulated(t *testing.T) *Manifest {
    t.Helper()
    return &Manifest{
        ManifestVersion: CurrentVersion,
        BackupID:        "01HKQ2C5XY7N8P9Q0RSTUVWXYZ",
        CreatedAt:       time.Date(2026, 4, 15, 12, 34, 56, 0, time.UTC),
        StoreID:         "store-uuid-1",
        StoreKind:       "badger",
        SHA256:          "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
        SizeBytes:       12345,
        Encryption:      Encryption{Enabled: false},
        PayloadIDSet:    []string{},
    }
}
```

**Version-gate reject pattern** (from `pkg/backup/manifest/manifest_test.go` lines 64–70):
```go
func TestManifestVersionGuard_RejectsFuture(t *testing.T) {
    m := fullyPopulated(t)
    m.ManifestVersion = 999
    err := m.Validate()
    require.Error(t, err)
    require.Contains(t, err.Error(), "unsupported manifest_version")
}
```

**D-04 extension — full restore-path version gate:**
```go
// TestManifestVersionGate_RestoreReturnsErrUnsupported verifies SAFETY-03:
// a manifest with version=2 fed into the restore path returns
// restore.ErrManifestVersionUnsupported (not a panic or silent corruption).
func TestManifestVersionGate_RestoreReturnsErrUnsupported(t *testing.T) {
    m := fullyPopulated(t)
    m.ManifestVersion = 2
    // Marshal YAML, write to a temp FS destination, attempt GetManifestOnly.
    // Assert errors.Is(err, restore.ErrManifestVersionUnsupported).
}
```

Because `ErrManifestVersionUnsupported` lives in `pkg/backup/restore/errors.go`, this test is better placed in an integration-tagged file in `pkg/backup/destination/` (alongside `corruption_test.go`) or as a standalone file in `pkg/backup/` where both `restore` and `destination` are importable without cycles.

---

### `test/e2e/backup_matrix_test.go` (test, e2e, request-response)

**Analog:** `test/e2e/store_matrix_test.go`

**Build tag + package** (from `test/e2e/store_matrix_test.go` lines 1–2):
```go
//go:build e2e

package e2e
```

**Imports** (from `test/e2e/store_matrix_test.go` lines 4–18):
```go
import (
    "testing"
    "time"

    "github.com/marmos91/dittofs/test/e2e/framework"
    "github.com/marmos91/dittofs/test/e2e/helpers"
    "github.com/stretchr/testify/require"
)
```

**Matrix entry point with availability guards** (from `test/e2e/store_matrix_test.go` lines 26–60):
```go
func TestBackupMatrix(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping backup matrix tests in short mode")
    }

    postgresAvailable := framework.CheckPostgresAvailable(t)
    localstackAvailable := framework.CheckLocalstackAvailable(t)

    var postgresHelper *framework.PostgresHelper
    var localstackHelper *framework.LocalstackHelper

    if postgresAvailable {
        postgresHelper = framework.NewPostgresHelper(t)
    }
    if localstackAvailable {
        localstackHelper = framework.NewLocalstackHelper(t)
    }

    // 3 engines × 2 destinations = 6 sub-tests (D-07)
    for _, tc := range backupMatrixCases() {
        t.Run(tc.name, func(t *testing.T) {
            if tc.needsPostgres && !postgresAvailable {
                t.Skip("PostgreSQL not available")
            }
            if tc.needsS3 && !localstackAvailable {
                t.Skip("Localstack (S3) not available")
            }
            runBackupMatrixCase(t, tc, postgresHelper, localstackHelper)
        })
    }
}
```

**Per-case server lifecycle** (from `test/e2e/store_matrix_test.go` lines 63–78):
```go
func runBackupMatrixCase(t *testing.T, tc matrixCase, pgHelper *framework.PostgresHelper, lsHelper *framework.LocalstackHelper) {
    t.Helper()

    sp := helpers.StartServerProcess(t, "")
    t.Cleanup(sp.ForceKill)

    runner := helpers.LoginAsAdmin(t, sp.APIURL())
    // ... setup store, create backup repo, trigger backup via REST, poll for completion,
    //     restore to fresh store, byte-compare
}
```

**REST backup trigger + job poll pattern** (D-07 — exercise Phase 6 surface):
```go
// Trigger backup via REST (Phase 6 POST /api/v1/store/metadata/{name}/backups)
backupID, err := runner.TriggerBackup(metaStoreName, repoName)
require.NoError(t, err)

// Poll job until terminal state (Phase 6 GET /api/v1/backup-jobs/{id})
require.Eventually(t, func() bool {
    job, err := runner.GetBackupJob(backupID)
    return err == nil && (job.Status == "succeeded" || job.Status == "failed")
}, 30*time.Second, 500*time.Millisecond, "backup job did not complete")

job, err := runner.GetBackupJob(backupID)
require.NoError(t, err)
require.Equal(t, "succeeded", job.Status, "backup job must succeed")
```

---

### `test/e2e/backup_chaos_test.go` (test, e2e, event-driven)

**Analog:** `test/e2e/helpers/server.go` (ForceKill) + `test/e2e/store_matrix_test.go` (server lifecycle)

**Build tag + package** (same as all e2e files):
```go
//go:build e2e

package e2e
```

**ForceKill at mid-run** (from `test/e2e/helpers/server.go` lines 249–278):
```go
// ForceKill terminates the server process.
// It first attempts graceful shutdown (SIGTERM) to allow NFS/SMB connections
// to close cleanly, then falls back to SIGKILL if the process doesn't exit.
func (sp *ServerProcess) ForceKill() {
    if sp.process == nil {
        return
    }
    _ = sp.process.Signal(syscall.SIGTERM)
    done := make(chan struct{})
    go func() {
        _, _ = sp.process.Wait()
        close(done)
    }()
    select {
    case <-done:
    case <-time.After(2 * time.Second):
        _ = sp.process.Kill()
        <-done
    }
    if sp.logFileHandle != nil {
        _ = sp.logFileHandle.Close()
        sp.logFileHandle = nil
    }
}
```

**Mid-run kill timing pattern (D-03)**:
```go
func TestBackupChaos_KillMidBackup(t *testing.T) {
    sp := helpers.StartServerProcess(t, "")
    // DO NOT register t.Cleanup(sp.ForceKill) — we kill manually below

    runner := helpers.LoginAsAdmin(t, sp.APIURL())
    // ... setup large enough store, create repo pointing at Localstack ...

    backupID, err := runner.TriggerBackup(metaStoreName, repoName)
    require.NoError(t, err)

    // Sleep-then-kill: give backup 500ms to be in-flight (D-03)
    time.Sleep(500 * time.Millisecond)
    sp.ForceKill()

    // Restart server against the same state dir
    sp2 := helpers.StartServerProcess(t, sp.ConfigFile())
    t.Cleanup(sp2.ForceKill)
    runner2 := helpers.LoginAsAdmin(t, sp2.APIURL())

    // SAFETY-02: running → interrupted on restart
    job, err := runner2.GetBackupJob(backupID)
    require.NoError(t, err)
    require.Equal(t, "interrupted", job.Status)

    // DRV-02: no ghost multipart uploads left in Localstack
    mpuOut, err := lsHelper.Client.ListMultipartUploads(context.Background(),
        &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
    require.NoError(t, err)
    require.Empty(t, mpuOut.Uploads, "ghost MPU uploads must be cleaned up after kill+restart")
}
```

**Restart with same config** — the chaos test MUST restart with the same config file so DB state (job rows) persists. The `sp.ConfigFile()` method returns the path. This is different from `backup_test.go` which creates a new server each time:
```go
// ConfigFile returns the path to the server config file.
func (sp *ServerProcess) ConfigFile() string {
    return sp.configFile // from helpers/server.go line 309
}
```

---

### `test/e2e/backup_restore_mounted_test.go` (test, e2e, request-response)

**Analog:** `test/e2e/shares_test.go` (share enable/disable lifecycle) + `test/e2e/backup_test.go` (server start, REST trigger pattern)

**Build tag + package**:
```go
//go:build e2e

package e2e
```

**Server + share + enabled flag pattern** (from `test/e2e/shares_test.go` lines 29–69):
```go
sp := helpers.StartServerProcess(t, "")
t.Cleanup(sp.ForceKill)
runner := helpers.LoginAsAdmin(t, sp.APIURL())

metaStoreName := helpers.UniqueTestName("restore_meta")
localStoreName := helpers.UniqueTestName("restore_local")
_, err := runner.CreateMetadataStore(metaStoreName, "memory")
require.NoError(t, err)
_, err = runner.CreateLocalBlockStore(localStoreName, "memory")
require.NoError(t, err)

shareName := "/" + helpers.UniqueTestName("restore_share")
share, err := runner.CreateShare(shareName, metaStoreName, localStoreName)
require.NoError(t, err)
// Share is Enabled=true by default after creation
require.True(t, share.Enabled)
```

**409 Conflict assertion pattern** (D-09, Phase 5 D-26 / Phase 6 D-29):
```go
// Attempt restore while share is enabled — must get 409
err = runner.TriggerRestore(metaStoreName, repoName)
require.Error(t, err)
// Assert the error wraps a 409 Conflict with enabled_shares in the body
var apiErr *helpers.APIError
require.True(t, errors.As(err, &apiErr))
require.Equal(t, 409, apiErr.StatusCode)
require.Contains(t, apiErr.Body, "enabled_shares")
```

---

## Shared Patterns

### Build tags
**Source:** Every existing test file in both layers.
- `//go:build integration` — for `pkg/backup/destination/corruption_test.go` and any manifest version-gate test that needs a destination
- `//go:build e2e` — for all `test/e2e/backup_*.go` files
- No build tag — for pure unit tests in `pkg/backup/manifest/`

### TestMain + shared Localstack singleton (integration layer)
**Source:** `pkg/backup/destination/s3/localstack_helper_test.go` lines 40–104
**Apply to:** `pkg/backup/destination/corruption_test.go`

The canonical pattern: check `LOCALSTACK_ENDPOINT` env var first; fall back to `testcontainers.GenericContainer`; store result in package-level singleton; return cleanup callback; call `os.Exit(code)` from `TestMain`. Never create per-test containers — MEMORY.md forbids it.

### Server lifecycle in E2E tests
**Source:** `test/e2e/store_matrix_test.go` lines 63–68 and `test/e2e/backup_test.go` lines 25–28
**Apply to:** All `test/e2e/backup_*.go` files

Pattern:
```go
sp := helpers.StartServerProcess(t, "")
t.Cleanup(sp.ForceKill)
runner := helpers.LoginAsAdmin(t, sp.APIURL())
```

For chaos tests that kill and restart: do NOT register `t.Cleanup(sp.ForceKill)` before the manual kill — it will error on the already-dead process. Register only on the restarted process.

### Unique name isolation
**Source:** `test/e2e/shares_test.go` line 57, `test/e2e/store_matrix_test.go` line 72
**Apply to:** All E2E tests

```go
storeName := helpers.UniqueTestName("prefix")
shareName := "/" + helpers.UniqueTestName("prefix")
```

### Error sentinel assertions
**Source:** `pkg/backup/destination/fs/store_test.go` lines 183–184, `pkg/backup/destination/s3/store_integration_test.go` lines 172–173
**Apply to:** `pkg/backup/destination/corruption_test.go` and manifest version-gate test

```go
require.ErrorIs(t, err, destination.ErrSHA256Mismatch)
require.ErrorIs(t, err, destination.ErrManifestMissing)
require.ErrorIs(t, err, restore.ErrStoreIDMismatch)
require.ErrorIs(t, err, restore.ErrManifestVersionUnsupported)
```

All Phase 5 sentinels live in `pkg/backup/restore/errors.go`. All destination sentinels live in `pkg/backup/destination/errors.go`.

### manifest.Manifest construction for injection
**Source:** `pkg/backup/destination/s3/store_integration_test.go` lines 96–110
**Apply to:** `pkg/backup/destination/corruption_test.go` (WrongStoreID and ManifestVersionUnsupported vectors)

```go
func mkManifest(id string, encrypted bool, keyRef string) *manifest.Manifest {
    return &manifest.Manifest{
        ManifestVersion: manifest.CurrentVersion,
        BackupID:        id,
        CreatedAt:       time.Now().UTC().Truncate(time.Second),
        StoreID:         "store-test",
        StoreKind:       "memory",
        Encryption: manifest.Encryption{
            Enabled:   encrypted,
            Algorithm: "aes-256-gcm",
            KeyRef:    keyRef,
        },
        PayloadIDSet: []string{},
    }
}
```

For the WrongStoreID vector: set `StoreID: "wrong-store-id"`, marshal with `m.Marshal()`, overwrite `manifest.yaml` via raw FS write or S3 PutObject. For ManifestVersionUnsupported: set `ManifestVersion: 2`, then marshal. The YAML `sha256` field can be a valid-looking hex string (checksum validation is not the point of this test — version rejection must happen before payload read begins in `GetManifestOnly`).

---

## No Analog Found

None — all five files have close analogs in the codebase.

---

## Key Sentinel Reference

| Sentinel | Package | File |
|---|---|---|
| `ErrSHA256Mismatch` | `destination` | `pkg/backup/destination/errors.go:42` |
| `ErrManifestMissing` | `destination` | `pkg/backup/destination/errors.go:47` |
| `ErrIncompleteBackup` | `destination` | `pkg/backup/destination/errors.go:68` |
| `ErrRestorePreconditionFailed` | `restore` | `pkg/backup/restore/errors.go:27` |
| `ErrStoreIDMismatch` | `restore` | `pkg/backup/restore/errors.go:36` |
| `ErrManifestVersionUnsupported` | `restore` | `pkg/backup/restore/errors.go:55` |
| `ErrNoRestoreCandidate` | `restore` | `pkg/backup/restore/errors.go:32` |
| `manifest.CurrentVersion` | `manifest` | `pkg/backup/manifest/manifest.go:25` |

## Metadata

**Analog search scope:** `pkg/backup/destination/`, `pkg/backup/destination/s3/`, `pkg/backup/destination/destinationtest/`, `pkg/backup/manifest/`, `pkg/backup/restore/`, `test/e2e/`, `test/e2e/helpers/`, `test/e2e/framework/`
**Files scanned:** 18
**Pattern extraction date:** 2026-04-17
