//go:build integration

package engine_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// parsePostgresDSN parses a libpq key=value DSN (the form documented in
// CLAUDE.md: "host=localhost port=15432 user=dfs password=dfs
// dbname=dittofs_grpa sslmode=disable") into the store config struct.
func parsePostgresDSN(t *testing.T, dsn string) *postgres.PostgresMetadataStoreConfig {
	t.Helper()
	cfg := &postgres.PostgresMetadataStoreConfig{
		SSLMode:     "disable",
		AutoMigrate: true,
	}
	for _, kv := range strings.Fields(dsn) {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "host":
			cfg.Host = val
		case "port":
			p, err := strconv.Atoi(val)
			if err != nil {
				t.Fatalf("parse port %q: %v", val, err)
			}
			cfg.Port = p
		case "user":
			cfg.User = val
		case "password":
			cfg.Password = val
		case "dbname", "database":
			cfg.Database = val
		case "sslmode", "ssl_mode":
			cfg.SSLMode = val
		}
	}
	if cfg.Host == "" || cfg.Port == 0 || cfg.Database == "" || cfg.User == "" {
		t.Fatalf("incomplete DSN %q (need host/port/dbname/user)", dsn)
	}
	return cfg
}

func newPostgresStore(t *testing.T) metadata.MetadataStore {
	t.Helper()
	dsn := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping Postgres rollup-drain tests")
	}
	cfg := parsePostgresDSN(t, dsn)
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
	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestPostgresDrainRollups_UniqueContent exercises the rollup-persist path
// with NO identical-content files: DrainRollups must persist every written
// file's block list so the Backup manifest covers all referenced hashes.
func TestPostgresDrainRollups_UniqueContent(t *testing.T) {
	ms := newPostgresStore(t)
	ctx := context.Background()

	shareName := "drainpg-uniq"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverStore(t, ms)

	uniqueA := distinctContent(0x10, 3*1024*1024)
	uniqueB := distinctContent(0x11, 3*1024*1024)

	pidA, hA := createRealFile(t, ms, shareName, "alpha.bin", rootHandle)
	pidB, hB := createRealFile(t, ms, shareName, "beta.bin", rootHandle)

	if _, err := bs.WriteAt(ctx, pidA, nil, uniqueA, 0); err != nil {
		t.Fatalf("WriteAt alpha: %v", err)
	}
	if _, err := bs.WriteAt(ctx, pidB, nil, uniqueB, 0); err != nil {
		t.Fatalf("WriteAt beta: %v", err)
	}

	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	want := block.NewHashSet(0)
	for name, h := range map[string]metadata.FileHandle{"alpha.bin": hA, "beta.bin": hB} {
		blocks := fileBlocks(t, ms, h)
		if len(blocks) == 0 {
			t.Fatalf("file %s has empty FileAttr.Blocks after DrainRollups", name)
		}
		for _, b := range blocks {
			want.Add(b.Hash)
		}
	}

	got, bufLen := manifestHashes(t, ms)
	missing := 0
	for _, h := range want.Sorted() {
		if !got.Contains(h) {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("Backup manifest missing %d/%d referenced hashes (manifest len=%d, buf=%d bytes)",
			missing, want.Len(), got.Len(), bufLen)
	}
}

// TestPostgresDrainRollups_MultiPassAppend reproduces #789 against a real
// Postgres metadata store: a file rolled up in two passes (append at a new
// offset) must end with file_block_refs covering the WHOLE byte range, not
// just the last pass. Without the persister-side complete-list fix, Postgres
// keeps only the last pass's chunk (DELETE-all + INSERT replace).
func TestPostgresDrainRollups_MultiPassAppend(t *testing.T) {
	ms := newPostgresStore(t)
	runMultiPassAppend(t, ms, "drainpg")
}

// TestPostgresDrainRollups_IdenticalContent reproduces BUG 2 over the REAL
// engine write path against a real Postgres metadata store: a share with two
// byte-identical files must snapshot successfully (DrainRollups tolerates the
// file-level-dedup object_id conflict), every file persists its block list,
// and the Backup manifest covers all referenced hashes.
func TestPostgresDrainRollups_IdenticalContent(t *testing.T) {
	ms := newPostgresStore(t)
	runIdenticalContentDrain(t, ms, "drainpg")
}

// TestPostgresDrainRollups_InPlaceModify exercises the snapshot-restore
// corruption scenario: a file written, rolled up, then modified in place and
// rolled up again must end with a manifest covering the LATEST content's
// blocks (the rollup-persist UPDATE path advances object_id O1 -> O2 on the
// same row).
func TestPostgresDrainRollups_InPlaceModify(t *testing.T) {
	ms := newPostgresStore(t)
	ctx := context.Background()

	shareName := "drainpg-mod"
	rootHandle := createShare(t, ms, shareName)
	bs := newEngineOverStore(t, ms)

	v1 := distinctContent(0x30, 3*1024*1024)
	v2 := distinctContent(0x31, 3*1024*1024)

	pid, h := createRealFile(t, ms, shareName, "file.bin", rootHandle)

	if _, err := bs.WriteAt(ctx, pid, nil, v1, 0); err != nil {
		t.Fatalf("WriteAt v1: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups v1: %v", err)
	}

	if _, err := bs.WriteAt(ctx, pid, nil, v2, 0); err != nil {
		t.Fatalf("WriteAt v2: %v", err)
	}
	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups v2: %v", err)
	}

	blocks := fileBlocks(t, ms, h)
	if len(blocks) == 0 {
		t.Fatalf("file has empty FileAttr.Blocks after in-place modify + drain")
	}
	want := block.NewHashSet(0)
	for _, b := range blocks {
		want.Add(b.Hash)
	}

	got, bufLen := manifestHashes(t, ms)
	missing := 0
	for _, hh := range want.Sorted() {
		if !got.Contains(hh) {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("Backup manifest missing %d/%d latest-content hashes (manifest len=%d, buf=%d bytes)",
			missing, want.Len(), got.Len(), bufLen)
	}
}
