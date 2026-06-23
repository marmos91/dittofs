package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// sqliteTestCapabilities returns the capabilities every SQLite test store is
// opened with. Mirrors the Postgres conformance capabilities so the shared
// suite asserts identical behavior across backends.
func sqliteTestCapabilities() metadata.FilesystemCapabilities {
	return metadata.FilesystemCapabilities{
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
}

// newSQLiteStoreFactory returns a factory that hands each conformance subtest a
// CLEAN store backed by its own on-disk database file in a per-subtest temp
// directory. A fresh file per subtest guarantees isolation without any shared
// state — sidestepping the shared-DB pollution traps the Postgres backend has
// to defend against. AutoMigrate creates the schema on open. The store is
// closed via t.Cleanup.
func newSQLiteStoreFactory() func(t *testing.T) metadata.Store {
	return func(t *testing.T) metadata.Store {
		t.Helper()
		dbPath := filepath.Join(t.TempDir(), "conformance.db")
		cfg := &sqlite.SQLiteMetadataStoreConfig{
			Path:        dbPath,
			AutoMigrate: true,
		}
		store, err := sqlite.NewSQLiteMetadataStore(context.Background(), cfg, sqliteTestCapabilities())
		if err != nil {
			t.Fatalf("NewSQLiteMetadataStore() failed: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
}

func TestConformance(t *testing.T) {
	storetest.RunConformanceSuite(t, newSQLiteStoreFactory())
}

func TestBackupConformance(t *testing.T) {
	storetest.RunBackupConformanceSuite(t, newSQLiteStoreFactory())
}

func TestResetThenRestoreConformance(t *testing.T) {
	storetest.ResetThenRestoreConformance(t, newSQLiteStoreFactory())
}

func TestLockPersistenceConformance(t *testing.T) {
	factory := newSQLiteStoreFactory()
	storetest.RunLockPersistenceSuite(t, func(t *testing.T) lock.LockStore {
		return factory(t).(lock.LockStore)
	})
}

// TestSQLite_TxServerConfigRoundTrip exercises the transaction-scoped
// SetServerConfig / GetServerConfig path, which the shared conformance suite
// does not cover (it only drives the pool path). The config column is JSON
// TEXT, so the tx path must marshal/unmarshal explicitly rather than binding a
// Go map directly to the driver.
func TestSQLite_TxServerConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newSQLiteStoreFactory()(t)

	want := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{"feature": "on", "n": float64(7)},
	}
	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetServerConfig(ctx, want)
	}); err != nil {
		t.Fatalf("tx SetServerConfig failed: %v", err)
	}

	var got metadata.MetadataServerConfig
	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		got, err = tx.GetServerConfig(ctx)
		return err
	}); err != nil {
		t.Fatalf("tx GetServerConfig failed: %v", err)
	}
	if got.CustomSettings["feature"] != "on" || got.CustomSettings["n"] != float64(7) {
		t.Fatalf("tx server config round-trip mismatch: got %#v", got.CustomSettings)
	}

	// The pool path must observe the tx-written config too.
	poolGot, err := store.GetServerConfig(ctx)
	if err != nil {
		t.Fatalf("pool GetServerConfig failed: %v", err)
	}
	if poolGot.CustomSettings["feature"] != "on" {
		t.Fatalf("pool path did not observe tx-written config: %#v", poolGot.CustomSettings)
	}
}

// TestSQLite_CleanShutdownMarkerDurable pins that a graceful Close records the
// clean-shutdown marker durably across a close+reopen against the same database
// file: a freshly-opened store defaults to unclean; a graceful Close records
// clean=true; reopening a NEW store against the same file reads it back.
func TestSQLite_CleanShutdownMarkerDurable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cleanshutdown.db")
	cfg := &sqlite.SQLiteMetadataStoreConfig{Path: dbPath, AutoMigrate: true}

	seed, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	// A fresh store has no server_epoch row yet, so it must read unclean.
	clean, err := seed.GetCleanShutdown(ctx)
	if err != nil {
		_ = seed.Close()
		t.Fatalf("GetCleanShutdown (fresh): %v", err)
	}
	if clean {
		_ = seed.Close()
		t.Fatalf("fresh store reported clean=true; absent marker must read unclean")
	}
	// Graceful Close records clean=true.
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	store, err := sqlite.NewSQLiteMetadataStore(ctx, cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clean, err = store.GetCleanShutdown(ctx)
	if err != nil {
		t.Fatalf("GetCleanShutdown (reopen): %v", err)
	}
	if !clean {
		t.Fatalf("graceful Close must durably record clean=true across reopen; got false")
	}
}
