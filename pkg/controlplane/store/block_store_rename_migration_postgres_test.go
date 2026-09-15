//go:build integration

package store

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// This file adds Postgres to the migration dialect list. The two backends do
// not travel the same route through the upgrade — the drivers answer "does
// this column exist" by different means, and the guards' raw SQL meets
// Postgres's stricter NULL and quoting rules — so the SQLite run says nothing
// about what an operator on Postgres gets.
func init() {
	postgresMigrationDialect = &migrationDialect{
		name:     "postgres",
		newEmpty: newPostgresTestDatabase,
	}
}

// newPostgresTestDatabase hands back a database of its own for one test.
//
// These tests build an old schema and upgrade it, so truncating a shared
// database is not enough: each needs a server that has never seen the current
// model. The database is dropped when the test ends.
func newPostgresTestDatabase(t *testing.T) *Config {
	t.Helper()
	admin := postgresAdminConfig(t)

	name := "dittofs_mig_" + randomSuffix(t)
	adminDB, err := gorm.Open(postgres.Open(admin.DSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect to %s: %v", admin.Database, err)
	}
	if err := adminDB.Exec("CREATE DATABASE " + name).Error; err != nil {
		closeRaw(t, adminDB)
		t.Fatalf("create database %s: %v", name, err)
	}
	closeRaw(t, adminDB)

	t.Cleanup(func() {
		db, err := gorm.Open(postgres.Open(admin.DSN()), &gorm.Config{})
		if err != nil {
			t.Logf("drop %s: connect: %v", name, err)
			return
		}
		// A connection the test left open would refuse the drop, and a leaked
		// database would outlive the run.
		db.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = ?", name)
		if err := db.Exec("DROP DATABASE IF EXISTS " + name).Error; err != nil {
			t.Logf("drop %s: %v", name, err)
		}
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	cfg := admin
	cfg.Database = name
	return &Config{Type: DatabaseTypePostgres, Postgres: cfg}
}

// postgresAdminConfig reads the connection the suite was pointed at, which is
// also where the per-test databases are created.
func postgresAdminConfig(t *testing.T) PostgresConfig {
	t.Helper()
	dsn := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping postgres migration case")
	}

	cfg := PostgresConfig{Host: "localhost", Port: 5432, Database: "postgres", User: "postgres", SSLMode: "disable"}
	for _, field := range strings.Fields(dsn) {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "host":
			cfg.Host = val
		case "port":
			p, err := strconv.Atoi(val)
			if err != nil {
				t.Fatalf("parse DSN port %q: %v", val, err)
			}
			cfg.Port = p
		case "dbname", "database":
			cfg.Database = val
		case "user":
			cfg.User = val
		case "password":
			cfg.Password = val
		case "sslmode", "ssl_mode":
			cfg.SSLMode = val
		}
	}
	return cfg
}

// randomSuffix keeps concurrently running packages from colliding on a
// database name.
func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}
