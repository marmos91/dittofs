//go:build integration

// Integration tests for the postgres backup driver (ENG-02). These require a
// live PostgreSQL instance; set DITTOFS_TEST_POSTGRES_DSN to a DSN that can
// CREATE DATABASE (superuser or a role owning the server's default template).
// Each test creates an isolated database so runs do not collide with the
// standard conformance suite or with each other.
package postgres_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// ----------------------------------------------------------------------------
// Test harness: per-test isolated database
// ----------------------------------------------------------------------------

// testPostgresEnv captures the connection parameters used for every isolated
// database spun up by the backup tests.
type testPostgresEnv struct {
	host     string
	port     int
	user     string
	password string
	sslMode  string
}

// loadPostgresEnv returns the connection parameters the backup tests use, or
// skips the test if the harness environment variable is missing. Defaults
// match the hardcoded values already present in postgres_conformance_test.go.
//
// DITTOFS_TEST_POSTGRES_DSN is a binary on/off signal. When callers need to
// override the individual connection parameters (for example when the local
// test Postgres runs on a non-default port), the following env vars take
// precedence: DITTOFS_TEST_POSTGRES_{HOST,PORT,USER,PASSWORD,SSLMODE}.
func loadPostgresEnv(t *testing.T) testPostgresEnv {
	t.Helper()
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL backup tests")
	}

	env := testPostgresEnv{
		host:     "localhost",
		port:     5432,
		user:     "postgres",
		password: "postgres",
		sslMode:  "disable",
	}
	if v := os.Getenv("DITTOFS_TEST_POSTGRES_HOST"); v != "" {
		env.host = v
	}
	if v := os.Getenv("DITTOFS_TEST_POSTGRES_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			env.port = p
		}
	}
	if v := os.Getenv("DITTOFS_TEST_POSTGRES_USER"); v != "" {
		env.user = v
	}
	if v := os.Getenv("DITTOFS_TEST_POSTGRES_PASSWORD"); v != "" {
		env.password = v
	}
	if v := os.Getenv("DITTOFS_TEST_POSTGRES_SSLMODE"); v != "" {
		env.sslMode = v
	}
	return env
}

// createIsolatedDatabase creates a fresh database named
// dittofs_backup_test_<uuid> and registers a cleanup that drops it after the
// test completes. Returns the database name.
func createIsolatedDatabase(t *testing.T, env testPostgresEnv) string {
	t.Helper()

	// Connect to the default `postgres` admin database to issue CREATE
	// DATABASE — you cannot CREATE DATABASE from inside the target DB.
	adminDSN := fmt.Sprintf(
		"host=%s port=%d dbname=postgres user=%s password=%s sslmode=%s",
		env.host, env.port, env.user, env.password, env.sslMode,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	adminConn, err := pgx.Connect(ctx, adminDSN)
	require.NoError(t, err, "connect admin db")
	defer adminConn.Close(context.Background())

	// UUID-derived DB names are always valid identifiers when underscored.
	dbName := "dittofs_backup_test_" + strings.ReplaceAll(uuid.New().String(), "-", "_")
	_, err = adminConn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName))
	require.NoError(t, err, "create isolated database")

	t.Cleanup(func() {
		// Fresh admin connection for teardown — the test ctx may have been
		// cancelled by the time we run.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		c, cerr := pgx.Connect(cleanupCtx, adminDSN)
		if cerr != nil {
			t.Logf("drop database %q: admin connect failed: %v", dbName, cerr)
			return
		}
		defer c.Close(context.Background())
		// Terminate any lingering connections so DROP DATABASE does not
		// error out under -count>1 / parallel runs.
		_, _ = c.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		if _, derr := c.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName)); derr != nil {
			t.Logf("drop database %q: %v", dbName, derr)
		}
	})

	return dbName
}

// openStore returns a PostgresMetadataStore bound to the given database with
// migrations applied. Cleanup closes the store at test end.
func openStore(t *testing.T, env testPostgresEnv, dbName string) *postgres.PostgresMetadataStore {
	t.Helper()

	caps := metadata.FilesystemCapabilities{
		MaxReadSize:         1048576,
		PreferredReadSize:   1048576,
		MaxWriteSize:        1048576,
		PreferredWriteSize:  1048576,
		MaxFileSize:         9223372036854775807,
		MaxFilenameLen:      255,
		MaxPathLen:          4096,
		MaxHardLinkCount:    32767,
		SupportsHardLinks:   true,
		SupportsSymlinks:    true,
		CaseSensitive:       true,
		CasePreserving:      true,
		TimestampResolution: 1,
	}

	cfg := &postgres.PostgresMetadataStoreConfig{
		Host:        env.host,
		Port:        env.port,
		Database:    dbName,
		User:        env.user,
		Password:    env.password,
		SSLMode:     env.sslMode,
		AutoMigrate: true,
	}

	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	require.NoError(t, err, "NewPostgresMetadataStore")
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

// newIsolatedStore is a convenience wrapper: create a fresh DB, open a store
// on it, and return both. The store is closed and the DB is dropped at test
// end.
func newIsolatedStore(t *testing.T, env testPostgresEnv) *postgres.PostgresMetadataStore {
	t.Helper()
	dbName := createIsolatedDatabase(t, env)
	return openStore(t, env, dbName)
}

// ----------------------------------------------------------------------------
// Seeding helpers — populate a store with deterministic content so Backup /
// Restore have something meaningful to round-trip.
// ----------------------------------------------------------------------------

// seedShareWithFiles creates a share named `shareName`, its root directory,
// and `n` files under the root. Each file has a content_id derived from its
// name so tests can assert the returned PayloadIDSet.
//
// Uses the low-level MetadataStore API (PutFile / SetChild / SetLinkCount)
// rather than the service layer because the tests live in the store package
// and exercise the raw storage contract — which is exactly what Backup /
// Restore operate against.
func seedShareWithFiles(t *testing.T, store *postgres.PostgresMetadataStore, shareName string, n int) (rootHandle metadata.FileHandle, payloadIDs []string) {
	t.Helper()
	ctx := context.Background()

	// CreateRootDirectory performs a single transactional INSERT into both
	// `files` (as the share root) and `shares` (with root_file_id populated
	// via ON CONFLICT DO UPDATE). Calling the top-level CreateShare instead
	// would leave `shares.root_file_id` NULL — which the schema forbids —
	// until a subsequent CreateRootDirectory fills it in. Skipping the
	// CreateShare step keeps the seeding path aligned with the production
	// ShareService boot sequence.
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	require.NoError(t, err)
	rootHandle, err = metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("file_%03d.dat", i)
		fullPath := "/" + name
		payloadID := metadata.PayloadID(fmt.Sprintf("payload-%s-%d", shareName, i))

		handle, err := store.GenerateHandle(ctx, shareName, fullPath)
		require.NoError(t, err, "GenerateHandle %s", name)

		_, fileID, err := metadata.DecodeFileHandle(handle)
		require.NoError(t, err, "DecodeFileHandle %s", name)

		file := &metadata.File{
			ID:        fileID,
			ShareName: shareName,
			Path:      fullPath,
			FileAttr: metadata.FileAttr{
				Type:      metadata.FileTypeRegular,
				Mode:      0o644,
				UID:       1000,
				GID:       1000,
				Size:      uint64(10 * (i + 1)),
				PayloadID: payloadID,
			},
		}
		require.NoError(t, store.PutFile(ctx, file), "PutFile %s", name)
		require.NoError(t, store.SetChild(ctx, rootHandle, name, handle), "SetChild %s", name)
		require.NoError(t, store.SetLinkCount(ctx, handle, 1), "SetLinkCount %s", name)
		payloadIDs = append(payloadIDs, string(payloadID))
	}
	return rootHandle, payloadIDs
}

// countFiles returns the total row count of the `files` table, used as a
// round-trip sanity check after restore.
func countFiles(t *testing.T, store *postgres.PostgresMetadataStore) int64 {
	t.Helper()
	// Re-use the exported Backup path: it is an exact witness of the current
	// snapshot's file count. A cheaper but equivalent approach would be a
	// private QueryRow helper; we prefer going through the public API so the
	// test observes exactly what a consumer would see.
	var buf bytes.Buffer
	_, err := store.Backup(context.Background(), &buf)
	require.NoError(t, err)
	manifest := readManifestFromTar(t, buf.Bytes())
	for _, e := range manifest.TableList {
		if e.Name == "files" {
			return e.RowCount
		}
	}
	t.Fatalf("files table missing from manifest")
	return 0
}

// ----------------------------------------------------------------------------
// Archive-inspection helpers (tests only)
// ----------------------------------------------------------------------------

// extractedManifest mirrors backupManifest but lives in the _test package so
// we do not depend on unexported symbols from pkg/metadata/store/postgres.
type extractedManifest struct {
	FormatVersion          int                    `yaml:"format_version"`
	EngineKind             string                 `yaml:"engine_kind"`
	PgServerVersion        string                 `yaml:"pg_server_version"`
	SchemaMigrationVersion int64                  `yaml:"schema_migration_version"`
	TableList              []extractedManifestRow `yaml:"table_list"`
	CreatedAt              time.Time              `yaml:"created_at"`
}

type extractedManifestRow struct {
	Name     string `yaml:"name"`
	RowCount int64  `yaml:"row_count"`
}

func readManifestFromTar(t *testing.T, archive []byte) extractedManifest {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "read tar header")
		if hdr.Name == "manifest.yaml" {
			data, err := io.ReadAll(tr)
			require.NoError(t, err)
			var m extractedManifest
			require.NoError(t, yaml.Unmarshal(data, &m))
			return m
		}
	}
	t.Fatalf("manifest.yaml not found in archive")
	return extractedManifest{}
}

// rewriteManifestSchemaVersion returns a new archive in which the manifest's
// schema_migration_version is replaced with `newVersion`. Used by the
// schema-version-mismatch test.
func rewriteManifestSchemaVersion(t *testing.T, archive []byte, newVersion int64) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	out := &bytes.Buffer{}
	tw := tar.NewWriter(out)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)

		if hdr.Name == "manifest.yaml" {
			var m extractedManifest
			require.NoError(t, yaml.Unmarshal(data, &m))
			m.SchemaMigrationVersion = newVersion
			newData, err := yaml.Marshal(m)
			require.NoError(t, err)
			newHdr := *hdr
			newHdr.Size = int64(len(newData))
			require.NoError(t, tw.WriteHeader(&newHdr))
			_, err = tw.Write(newData)
			require.NoError(t, err)
			continue
		}

		require.NoError(t, tw.WriteHeader(hdr))
		_, err = tw.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return out.Bytes()
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

// TestBackupRoundTrip_EmptyStore verifies Backup + Restore on a freshly
// migrated store with no user data: the archive must contain a manifest and
// every expected table entry, and Restore into a second fresh store must
// succeed.
func TestBackupRoundTrip_EmptyStore(t *testing.T) {
	env := loadPostgresEnv(t)
	source := newIsolatedStore(t, env)

	var archive bytes.Buffer
	payloadIDs, err := source.Backup(context.Background(), &archive)
	require.NoError(t, err)
	require.Equal(t, 0, payloadIDs.Len(), "empty store has no payload ids")

	manifest := readManifestFromTar(t, archive.Bytes())
	require.Equal(t, 1, manifest.FormatVersion)
	require.Equal(t, "postgres", manifest.EngineKind)
	require.NotEmpty(t, manifest.PgServerVersion, "server_version populated")
	require.Positive(t, manifest.SchemaMigrationVersion, "schema_migration_version populated")
	require.NotEmpty(t, manifest.TableList, "table_list populated")

	// Restore into a different fresh database.
	target := newIsolatedStore(t, env)
	require.NoError(t, target.Restore(context.Background(), bytes.NewReader(archive.Bytes())))

	// Round-trip sanity: backup the restored store and compare the file
	// count in the two manifests.
	require.Equal(t, int64(0), countFiles(t, target))
}

// TestBackupRoundTrip_WithFiles populates the source with a share and files,
// runs Backup, then Restore into a fresh store, and verifies the file count
// and payload-id set round-trip.
func TestBackupRoundTrip_WithFiles(t *testing.T) {
	env := loadPostgresEnv(t)
	source := newIsolatedStore(t, env)

	_, expectedPayloads := seedShareWithFiles(t, source, "round_trip_share", 5)

	var archive bytes.Buffer
	payloadIDs, err := source.Backup(context.Background(), &archive)
	require.NoError(t, err)
	require.Equal(t, len(expectedPayloads), payloadIDs.Len(),
		"payload-id set size matches seeded file count")
	for _, id := range expectedPayloads {
		require.True(t, payloadIDs.Contains(id), "payload id %q present", id)
	}

	sourceFileCount := countFiles(t, source)
	require.Greater(t, sourceFileCount, int64(0))

	// Restore into a second fresh DB, then verify counts match.
	target := newIsolatedStore(t, env)
	require.NoError(t, target.Restore(context.Background(), bytes.NewReader(archive.Bytes())))
	require.Equal(t, sourceFileCount, countFiles(t, target),
		"restored file count equals source file count")
}

// TestBackupDeterministic verifies that two backups of the same store with
// no data changes produce byte-identical archives. This is what the tar's
// fixed ModTime is for.
//
// Note: the `created_at` timestamp in the manifest is wall-clock — so if
// both backups happen within 1s, rare but possible test flakiness could
// occur. We trim the manifest to exclude that field from the comparison.
func TestBackupDeterministic(t *testing.T) {
	env := loadPostgresEnv(t)
	source := newIsolatedStore(t, env)

	seedShareWithFiles(t, source, "det", 3)

	var a, b bytes.Buffer
	_, err := source.Backup(context.Background(), &a)
	require.NoError(t, err)
	_, err = source.Backup(context.Background(), &b)
	require.NoError(t, err)

	// Compare every tar entry OTHER than manifest.yaml byte-for-byte. The
	// manifest is expected to vary only in `created_at`.
	blobsA := extractTableBlobs(t, a.Bytes())
	blobsB := extractTableBlobs(t, b.Bytes())
	require.Equal(t, len(blobsA), len(blobsB), "same table set")
	for name, bytesA := range blobsA {
		bytesB, ok := blobsB[name]
		require.True(t, ok, "table %q present in second archive", name)
		require.Equal(t, bytesA, bytesB, "table %q bytes differ", name)
	}
}

// extractTableBlobs returns a map of table name -> raw binary COPY bytes
// from the archive (excludes manifest.yaml).
func extractTableBlobs(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		if strings.HasPrefix(hdr.Name, "tables/") {
			out[hdr.Name] = data
		}
	}
	return out
}

// TestRestore_RejectsSchemaMismatch forces the manifest's
// schema_migration_version to a value the binary does not know, then
// verifies Restore returns ErrSchemaVersionMismatch without touching the
// destination.
func TestRestore_RejectsSchemaMismatch(t *testing.T) {
	env := loadPostgresEnv(t)
	source := newIsolatedStore(t, env)

	var archive bytes.Buffer
	_, err := source.Backup(context.Background(), &archive)
	require.NoError(t, err)

	// Rewrite the manifest with a bogus schema version (+100).
	mutated := rewriteManifestSchemaVersion(t, archive.Bytes(),
		readManifestFromTar(t, archive.Bytes()).SchemaMigrationVersion+100)

	target := newIsolatedStore(t, env)
	err = target.Restore(context.Background(), bytes.NewReader(mutated))
	require.Error(t, err)
	require.True(t, errors.Is(err, metadata.ErrSchemaVersionMismatch),
		"error wraps metadata.ErrSchemaVersionMismatch; got: %v", err)

	// D-06 guarantee: the destination is still empty — the gate fired before
	// any write. If Restore mutated the destination despite the error, this
	// assertion catches the regression.
	require.Equal(t, int64(0), countFiles(t, target),
		"failed restore did not touch destination")
}

// TestRestore_RejectsNonEmptyDestination seeds the target with a share
// before attempting Restore, and asserts ErrRestoreDestinationNotEmpty.
func TestRestore_RejectsNonEmptyDestination(t *testing.T) {
	env := loadPostgresEnv(t)
	source := newIsolatedStore(t, env)
	seedShareWithFiles(t, source, "src", 2)

	var archive bytes.Buffer
	_, err := source.Backup(context.Background(), &archive)
	require.NoError(t, err)

	// Target is NOT fresh — seed some content first.
	target := newIsolatedStore(t, env)
	seedShareWithFiles(t, target, "occupant", 1)
	preRestoreCount := countFiles(t, target)
	require.Positive(t, preRestoreCount, "target has prior content")

	err = target.Restore(context.Background(), bytes.NewReader(archive.Bytes()))
	require.Error(t, err)
	require.True(t, errors.Is(err, metadata.ErrRestoreDestinationNotEmpty),
		"error wraps metadata.ErrRestoreDestinationNotEmpty; got: %v", err)

	// Target's prior content must be intact.
	require.Equal(t, preRestoreCount, countFiles(t, target),
		"failed restore did not touch destination")
}

// TestBackupable_CompileTimeAssertion is a meta-test that freezes the
// interface binding at the package boundary. If the driver drifts out of
// conformance with metadata.Backupable, this test fails to compile.
func TestBackupable_CompileTimeAssertion(t *testing.T) {
	// Done via the var in backup.go (`var _ metadata.Backupable = ...`);
	// this test exists so CI surfaces a coherent test name when the
	// assertion fires.
	var _ metadata.Backupable = (*postgres.PostgresMetadataStore)(nil)
}
