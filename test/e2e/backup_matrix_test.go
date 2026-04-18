//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type backupMatrixCase struct {
	name            string
	engineKind      string // memory | badger | postgres
	destinationKind string // local | s3
	needsPostgres   bool
	needsS3         bool
}

func backupMatrixCases() []backupMatrixCase {
	return []backupMatrixCase{
		{name: "Memory_Local", engineKind: "memory", destinationKind: "local"},
		{name: "Memory_S3", engineKind: "memory", destinationKind: "s3", needsS3: true},
		{name: "Badger_Local", engineKind: "badger", destinationKind: "local"},
		{name: "Badger_S3", engineKind: "badger", destinationKind: "s3", needsS3: true},
		{name: "Postgres_Local", engineKind: "postgres", destinationKind: "local", needsPostgres: true},
		{name: "Postgres_S3", engineKind: "postgres", destinationKind: "s3", needsPostgres: true, needsS3: true},
	}
}

func TestBackupMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping backup matrix tests in short mode")
	}

	postgresAvailable := framework.CheckPostgresAvailable(t)
	localstackAvailable := framework.CheckLocalstackAvailable(t)

	var pgHelper *framework.PostgresHelper
	var lsHelper *framework.LocalstackHelper
	if postgresAvailable {
		pgHelper = framework.NewPostgresHelper(t)
	}
	if localstackAvailable {
		lsHelper = framework.NewLocalstackHelper(t)
	}

	for _, tc := range backupMatrixCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsPostgres && !postgresAvailable {
				t.Skip("Skipping: PostgreSQL container not available")
			}
			if tc.needsS3 && !localstackAvailable {
				t.Skip("Skipping: Localstack (S3) container not available")
			}
			runBackupMatrixCase(t, tc, pgHelper, lsHelper)
		})
	}
}

func runBackupMatrixCase(t *testing.T, tc backupMatrixCase, pgHelper *framework.PostgresHelper, lsHelper *framework.LocalstackHelper) {
	t.Helper()
	ctx := context.Background()

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())
	apiClient := helpers.GetAPIClient(t, sp.APIURL())

	storeName := helpers.UniqueTestName(fmt.Sprintf("bkmtx_%s", tc.engineKind))
	switch tc.engineKind {
	case "memory":
		_, err := runner.CreateMetadataStore(storeName, "memory")
		require.NoError(t, err, "create memory metadata store")
	case "badger":
		badgerPath := filepath.Join(t.TempDir(), "badger-"+storeName)
		_, err := runner.CreateMetadataStore(storeName, "badger", helpers.WithMetaDBPath(badgerPath))
		require.NoError(t, err, "create badger metadata store")
	case "postgres":
		require.NotNil(t, pgHelper, "postgres helper required for postgres case")
		schema := "bkmtx_" + strings.ReplaceAll(strings.ReplaceAll(storeName, "-", "_"), ".", "_")
		pgConfig := fmt.Sprintf(
			`{"host":%q,"port":%d,"user":%q,"password":%q,"database":%q,"schema":%q,"sslmode":"disable"}`,
			pgHelper.Host, pgHelper.Port, pgHelper.User, pgHelper.Password, pgHelper.Database, schema,
		)
		_, err := runner.CreateMetadataStore(storeName, "postgres", helpers.WithMetaRawConfig(pgConfig))
		require.NoError(t, err, "create postgres metadata store")
	default:
		t.Fatalf("unknown engine kind %q", tc.engineKind)
	}

	for i := 0; i < 5; i++ {
		username := helpers.UniqueTestName(fmt.Sprintf("bkuser_%d", i))
		_, err := runner.CreateUser(username, "testpass123",
			helpers.WithEmail(fmt.Sprintf("%s@test.com", username)))
		require.NoError(t, err, "seed user %d", i)
	}

	mbr := helpers.NewMetadataBackupRunner(t, apiClient, storeName)
	repoName := helpers.UniqueTestName("bkrepo")

	switch tc.destinationKind {
	case "local":
		repoPath := filepath.Join(t.TempDir(), "backups-"+repoName)
		_ = mbr.CreateLocalRepo(repoName, repoPath)
	case "s3":
		require.NotNil(t, lsHelper, "localstack helper required for s3 case")
		bucket := s3SafeBucketName("mx-" + repoName)
		require.NoError(t, lsHelper.CreateBucket(ctx, bucket), "create bucket")
		t.Cleanup(func() { lsHelper.CleanupBucket(ctx, bucket) })
		_ = mbr.CreateS3Repo(repoName, bucket, lsHelper.Endpoint)
	default:
		t.Fatalf("unknown destination kind %q", tc.destinationKind)
	}

	resp := mbr.TriggerBackup(repoName)
	require.NotNil(t, resp.Job, "TriggerBackup must return a Job")
	job := mbr.PollJobUntilTerminal(resp.Job.ID, 2*time.Minute)
	assert.Equal(t, "succeeded", job.Status, "backup job must succeed; error=%q", job.Error)
	assert.Empty(t, job.Error, "backup job must not surface error")

	rec := mbr.WaitForBackupRecordSucceeded(repoName, 30*time.Second)
	require.NotNil(t, rec)
	assert.Greater(t, rec.SizeBytes, int64(0), "backup record must have non-zero size")

	// No enabled shares on a freshly-created store, so StartRestore succeeds without a precondition error.
	restoreJob := mbr.StartRestoreMustSucceed(rec.ID)
	require.NotNil(t, restoreJob, "restore job must be created")
	finalRestore := mbr.PollJobUntilTerminal(restoreJob.ID, 2*time.Minute)
	assert.Equal(t, "succeeded", finalRestore.Status, "restore job must succeed; error=%q", finalRestore.Error)
	assert.Empty(t, finalRestore.Error, "restore job must not surface error")
}

// s3SafeBucketName enforces S3 bucket naming: lowercase alphanumeric + hyphens, 3–63 chars.
func s3SafeBucketName(seed string) string {
	s := strings.ToLower(seed)
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	if len(s) > 63 {
		s = s[:63]
	}
	if len(s) == 0 || s[0] == '-' {
		s = "b-" + s
		if len(s) > 63 {
			s = s[:63]
		}
	}
	s = strings.TrimRight(s, "-")
	if len(s) < 3 {
		s = s + "-xx"
	}
	return s
}
